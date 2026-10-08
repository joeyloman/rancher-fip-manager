package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, rbbv1beta2.AddToScheme(scheme))
	require.NoError(t, managementv3.AddToScheme(scheme))
	return scheme
}

func newTestLogger() *logrus.Entry {
	logger := logrus.New()
	logger.SetLevel(logrus.PanicLevel)
	return logrus.NewEntry(logger)
}

func newTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.RancherFipLBControllerNamespace = "rancher-fip-manager"
	cfg.RancherFipApiServerURL = "https://rancher-fip-api.test/v1"
	return cfg
}

// testKubeconfig is a syntactically valid kubeconfig pointing at a closed
// port, so building the downstream client succeeds and the first API call
// fails with a connection error — the powered-off-cluster scenario.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: c-fail
contexts:
- context:
    cluster: c-fail
    user: u
  name: ctx
current-context: ctx
users:
- name: u
  user:
    token: test
`

func TestSweepOrphanedConfigSecrets_failsClosedOnEmptyQuotaList(t *testing.T) {
	scheme := newTestScheme(t)

	// Secrets that look orphaned, a cluster to sweep, but ZERO
	// FloatingIPProjectQuota objects — the exact cold-cache/RBAC scenario the
	// guard protects against.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-x", Namespace: "c-test1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-y", Namespace: "c-test1"}},
		&managementv3.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c-test1"}},
	).Build()

	r := &FloatingIPProjectQuotaReconciler{Client: c, Scheme: scheme, Config: newTestConfig()}

	require.NoError(t, r.sweepOrphanedConfigSecrets(context.Background(), newTestLogger()))

	// Nothing may be deleted.
	for _, name := range []string{"rancher-fip-config-p-x", "rancher-fip-config-p-y"} {
		err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "c-test1"}, &corev1.Secret{})
		assert.NoError(t, err, "secret %s must survive a fail-closed sweep", name)
	}
}

func TestDeleteOrphanedLocalConfigSecrets_onlyDeletesOrphans(t *testing.T) {
	scheme := newTestScheme(t)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Active project: must survive.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-keep", Namespace: "c-test1"}},
		// Orphan: must be deleted.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-gone", Namespace: "c-test1"}},
		// Prefix without a project suffix: must survive.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config", Namespace: "c-test1"}},
		// Unrelated name: must survive.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cacerts", Namespace: "c-test1"}},
	).Build()

	r := &FloatingIPProjectQuotaReconciler{Client: c, Scheme: scheme}
	log := newTestLogger()

	deleted, err := r.deleteOrphanedLocalConfigSecrets(context.Background(), map[string]bool{"p-keep": true}, log)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "only the orphaned secret must be deleted")

	err = c.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-gone", Namespace: "c-test1"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "orphaned secret should be deleted")

	for _, name := range []string{"rancher-fip-config-p-keep", "rancher-fip-config", "cacerts"} {
		err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "c-test1"}, &corev1.Secret{})
		assert.NoError(t, err, "secret %s must survive", name)
	}
}

func TestSweepOrphanedConfigSecrets_sweepsDownstreamOrphanOfOrphanOnlyNamespace(t *testing.T) {
	scheme := newTestScheme(t)

	// Cluster "c-only" exists and its only local config secret is orphaned.
	// The candidate namespaces are collected before the local pass deletes
	// that orphan, so the downstream orphan in cluster "c-only" must still be
	// swept.
	cluster := &managementv3.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "c-only",
			Labels: map[string]string{"rancher-fip": "enabled"},
		},
	}
	localOrphan := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-gone", Namespace: "c-only"}}
	active := &rbbv1beta2.FloatingIPProjectQuota{ObjectMeta: metav1.ObjectMeta{Name: "p-keep"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, localOrphan, active).Build()

	// The downstream cluster holds an orphaned config secret plus the secret
	// of the still-active project, which must survive.
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-gone", Namespace: "rancher-fip-manager"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-keep", Namespace: "rancher-fip-manager"}},
	).Build()

	r := &FloatingIPProjectQuotaReconciler{
		Client: c,
		Scheme: scheme,
		Config: newTestConfig(),
		newDownstreamClient: func(context.Context, *managementv3.Cluster) (client.Client, error) {
			return downstream, nil
		},
	}

	require.NoError(t, r.sweepOrphanedConfigSecrets(context.Background(), newTestLogger()))

	// The orphaned local secret is deleted.
	err := c.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-gone", Namespace: "c-only"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "orphaned local secret should be deleted")

	// The downstream orphan of the same project is deleted too, even though
	// its namespace had no surviving local config secret.
	err = downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-gone", Namespace: "rancher-fip-manager"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "downstream orphan should be deleted")

	err = downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-keep", Namespace: "rancher-fip-manager"}, &corev1.Secret{})
	assert.NoError(t, err, "downstream secret of the active project must survive")
}

func TestHandleFloatingIPProjectQuotaDelete_releasesFinalizerWhenClusterIsGone(t *testing.T) {
	scheme := newTestScheme(t)

	deletionTime := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	fipq := &rbbv1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p-gone2",
			Finalizers:        []string{floatingIPProjectQuotaCleanupFinalizer},
			DeletionTimestamp: &deletionTime,
		},
	}
	localSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-gone2", Namespace: "c-missing"}}
	// Secrets that must survive: a differently-named config secret and the
	// shared cacerts secret.
	otherSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-other", Namespace: "c-missing"}}
	cacertsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cacerts", Namespace: "c-missing"}}

	// No cluster objects at all: the local cleanup matches the config secret
	// by name across namespaces and the downstream pass has nothing to do.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fipq, localSecret, otherSecret, cacertsSecret).Build()

	r := &FloatingIPProjectQuotaReconciler{
		Client: c,
		Scheme: scheme,
		Config: newTestConfig(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p-gone2"}})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	// The local config secret must be removed even though the downstream
	// cluster does not exist.
	err = c.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-gone2", Namespace: "c-missing"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "local project secret should be deleted")

	// Same-name secrets of other projects and the shared cacerts secret must
	// survive.
	for _, name := range []string{"rancher-fip-config-p-other", "cacerts"} {
		err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "c-missing"}, &corev1.Secret{})
		assert.NoError(t, err, "secret %s must survive", name)
	}

	// The cleanup finalizer must be released so the object can disappear.
	var fresh rbbv1beta2.FloatingIPProjectQuota
	err = c.Get(context.Background(), types.NamespacedName{Name: "p-gone2"}, &fresh)
	if err == nil {
		assert.False(t, controllerutil.ContainsFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer), "cleanup finalizer should be removed")
	} else {
		// The fake client garbage-collects the object once its last
		// finalizer is gone, which is equally fine.
		assert.True(t, apierrors.IsNotFound(err), "unexpected error: %v", err)
	}
}

func TestHandleFloatingIPProjectQuotaDelete_requeuesWithinGraceOnDownstreamFailure(t *testing.T) {
	scheme := newTestScheme(t)

	deletionTime := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	fipq := &rbbv1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p-grace",
			Finalizers:        []string{floatingIPProjectQuotaCleanupFinalizer},
			DeletionTimestamp: &deletionTime,
		},
	}
	// A bare metal cluster whose kubeconfig secret exists but points at a
	// closed port: deleting the config secret in it fails with a connection
	// error. The local config secret names the cluster ("c-fail") so the
	// cleanup can find it.
	cluster := &managementv3.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "c-fail",
			Labels: map[string]string{"rancher-fip": "enabled"},
		},
	}
	localSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-grace", Namespace: "c-fail"}}
	kubeconfig := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-fail-kubeconfig", Namespace: "fleet-default"},
		Data:       map[string][]byte{"value": []byte(testKubeconfig)},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fipq, localSecret, cluster, kubeconfig).Build()

	r := &FloatingIPProjectQuotaReconciler{
		Client: c,
		Scheme: scheme,
		Config: newTestConfig(),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p-grace"}})
	require.Error(t, err, "within the grace window a downstream failure must requeue")

	// The cleanup finalizer must still be present so the reconcile retries.
	var fresh rbbv1beta2.FloatingIPProjectQuota
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "p-grace"}, &fresh))
	assert.True(t, controllerutil.ContainsFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer), "finalizer must be kept while retrying within the grace window")
}

func TestHandleFloatingIPProjectQuotaDelete_graceExpiredReleasesFinalizerOnDownstreamFailure(t *testing.T) {
	scheme := newTestScheme(t)

	deletionTime := metav1.NewTime(time.Now().Add(-11 * time.Minute))
	fipq := &rbbv1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p-grace2",
			Finalizers:        []string{floatingIPProjectQuotaCleanupFinalizer},
			DeletionTimestamp: &deletionTime,
		},
	}
	localSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-grace2", Namespace: "c-fail"}}
	cluster := &managementv3.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c-fail"}}
	kubeconfig := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c-fail-kubeconfig", Namespace: "fleet-default"},
		Data:       map[string][]byte{"value": []byte(testKubeconfig)},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fipq, localSecret, cluster, kubeconfig).Build()

	r := &FloatingIPProjectQuotaReconciler{
		Client: c,
		Scheme: scheme,
		Config: newTestConfig(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p-grace2"}})
	require.NoError(t, err, "after the grace window the deletion must not wedge on the broken downstream cluster")
	assert.Equal(t, ctrl.Result{}, res)

	// The local config secret is still cleaned up.
	err = c.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-grace2", Namespace: "c-fail"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "local project secret should be deleted")

	// The cleanup finalizer must be released despite the downstream failure.
	var fresh rbbv1beta2.FloatingIPProjectQuota
	err = c.Get(context.Background(), types.NamespacedName{Name: "p-grace2"}, &fresh)
	if err == nil {
		assert.False(t, controllerutil.ContainsFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer), "cleanup finalizer should be removed after the grace window")
	} else {
		assert.True(t, apierrors.IsNotFound(err), "unexpected error: %v", err)
	}
}
