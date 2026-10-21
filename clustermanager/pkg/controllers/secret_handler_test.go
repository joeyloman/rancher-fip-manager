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

func downstreamConfigSecretData(apiURL, clientSecret, cluster, project, pool, lbType string) map[string][]byte {
	return map[string][]byte{
		"apiUrl":           []byte(apiURL),
		"cluster":          []byte(cluster),
		"clientSecret":     []byte(clientSecret),
		"project":          []byte(project),
		"floatingIPPool":   []byte(pool),
		"loadBalancerType": []byte(lbType),
	}
}

// TestHandleConfigSecrets_reconcilesDriftedDownstreamSecret pins the
// downstream-secret reconcile fix: the downstream config secret must follow
// the local clientSecret (lost and regenerated local secret) and the current
// apiUrl/pool/LB-type instead of being skipped forever with its stale values.
func TestHandleConfigSecrets_reconcilesDriftedDownstreamSecret(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-drift", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-drift", Namespace: "c-test1"},
			Data: map[string][]byte{
				"clientSecret": []byte("new-secret"),
				"project":      []byte("p-drift"),
			},
		},
	).Build()
	// Downstream copy still carries the pre-regeneration clientSecret and an
	// old api-server URL.
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-drift", Namespace: "rancher-fip-manager"},
			Data:       downstreamConfigSecretData("https://old.example/v1", "old-secret", "c-test1", "p-drift", "net", "purelb"),
		},
	).Build()

	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "metallb", nil, "",
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var updated corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-drift", Namespace: cfg.RancherFipLBControllerNamespace}, &updated))
	assert.Equal(t, []byte("new-secret"), updated.Data["clientSecret"], "stale downstream clientSecret must be reconciled to the local value")
	assert.Equal(t, []byte(cfg.RancherFipApiServerURL), updated.Data["apiUrl"], "stale apiUrl must be reconciled")
	assert.Equal(t, []byte("metallb"), updated.Data["loadBalancerType"], "stale loadBalancerType must be reconciled")
	assert.Equal(t, []byte("net"), updated.Data["floatingIPPool"])

	// The local secret keeps its clientSecret.
	var localSecret corev1.Secret
	require.NoError(t, local.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-drift", Namespace: "c-test1"}, &localSecret))
	assert.Equal(t, []byte("new-secret"), localSecret.Data["clientSecret"])
}

// TestHandleConfigSecrets_leavesUpToDateDownstreamSecretUntouched ensures the
// reconciliation is read-compare-write: an in-sync downstream secret is not
// rewritten on every reconcile pass.
func TestHandleConfigSecrets_leavesUpToDateDownstreamSecretUntouched(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-sync", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-sync", Namespace: "c-test1"},
			Data: map[string][]byte{
				"clientSecret": []byte("same-secret"),
				"project":      []byte("p-sync"),
			},
		},
	).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-sync", Namespace: cfg.RancherFipLBControllerNamespace},
			Data:       downstreamConfigSecretData(cfg.RancherFipApiServerURL, "same-secret", "c-test1", "p-sync", "net", "purelb"),
		},
	).Build()

	var before corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-sync", Namespace: cfg.RancherFipLBControllerNamespace}, &before))

	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", nil, "",
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var after corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-sync", Namespace: cfg.RancherFipLBControllerNamespace}, &after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "an in-sync downstream secret must not be rewritten")
}

// TestHandleConfigSecrets_reconcilesRotatedCACerts pins the CA half of the
// reconcile fix: a rotated CA must propagate into an existing downstream
// cacerts secret.
func TestHandleConfigSecrets_reconcilesRotatedCACerts(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-ca", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-ca", Namespace: "c-test1"},
			Data: map[string][]byte{
				"clientSecret": []byte("secret"),
				"project":      []byte("p-ca"),
			},
		},
	).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-ca", Namespace: cfg.RancherFipLBControllerNamespace},
			Data:       downstreamConfigSecretData(cfg.RancherFipApiServerURL, "secret", "c-test1", "p-ca", "net", "purelb"),
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace},
			Data:       map[string][]byte{"ca.crt": []byte("old-ca")},
		},
	).Build()

	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", []byte("new-ca"), "",
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var cacerts corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &cacerts))
	assert.Equal(t, []byte("new-ca"), cacerts.Data["ca.crt"], "rotated CA must propagate downstream")

	// Same CA again: no rewrite.
	var before corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &before))
	_, err = HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", []byte("new-ca"), "",
	)
	require.NoError(t, err)
	var after corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "an in-sync cacerts secret must not be rewritten")
}

// TestHandleConfigSecrets_regeneratesEmptyLocalClientSecret covers the corrupt
// local secret: without the guard the reconcile would overwrite a working
// downstream clientSecret with an empty one.
func TestHandleConfigSecrets_regeneratesEmptyLocalClientSecret(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-empty", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-empty", Namespace: "c-test1"},
			Data:       map[string][]byte{"project": []byte("p-empty")},
		},
	).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-empty", Namespace: cfg.RancherFipLBControllerNamespace},
			Data:       downstreamConfigSecretData(cfg.RancherFipApiServerURL, "old-secret", "c-test1", "p-empty", "net", "purelb"),
		},
	).Build()

	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", nil, "",
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var localSecret corev1.Secret
	require.NoError(t, local.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-empty", Namespace: "c-test1"}, &localSecret))
	regenerated := string(localSecret.Data["clientSecret"])
	assert.NotEmpty(t, regenerated, "corrupt local secret must be regenerated, not propagated as empty")

	var downstreamSecret corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "rancher-fip-config-p-empty", Namespace: cfg.RancherFipLBControllerNamespace}, &downstreamSecret))
	assert.Equal(t, regenerated, string(downstreamSecret.Data["clientSecret"]), "downstream secret must follow the regenerated clientSecret")
}

// TestHandleConfigSecrets_rotatedLocalCAIsObservedOnNextPass pins the CA
// source fix: the local cacerts secret is re-read (uncached) on every pass,
// so a CA rotated after the clustermanager started propagates downstream on
// the next reconcile instead of requiring a pod restart.
func TestHandleConfigSecrets_rotatedLocalCAIsObservedOnNextPass(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	appNamespace := "rancher-fip-manager"
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-rot", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-rot", Namespace: "c-test1"},
			Data: map[string][]byte{
				"clientSecret": []byte("secret"),
				"project":      []byte("p-rot"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cacerts", Namespace: appNamespace},
			Data:       map[string][]byte{"ca.crt": []byte("ca-v1")},
		},
	).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects().Build()

	// Pass 1: no startup snapshot needed, the local secret is the source.
	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", nil, appNamespace,
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var cacerts corev1.Secret
	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &cacerts))
	assert.Equal(t, []byte("ca-v1"), cacerts.Data["ca.crt"], "first pass must propagate the local CA")

	// Rotate the local CA between passes.
	var localCacerts corev1.Secret
	require.NoError(t, local.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: appNamespace}, &localCacerts))
	localCacerts.Data = map[string][]byte{"ca.crt": []byte("ca-v2")}
	require.NoError(t, local.Update(context.Background(), &localCacerts))

	res, err = HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", []byte("stale-startup-snapshot"), appNamespace,
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	require.NoError(t, downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &cacerts))
	assert.Equal(t, []byte("ca-v2"), cacerts.Data["ca.crt"], "a rotated local CA must propagate; the stale startup snapshot must be ignored")
}

// TestHandleConfigSecrets_deletedLocalCAStopsPropagation pins the fallback
// semantics: a locally deleted cacerts secret stops the CA propagation
// instead of resurrecting the startup snapshot downstream.
func TestHandleConfigSecrets_deletedLocalCAStopsPropagation(t *testing.T) {
	scheme := newTestScheme(t)
	cfg := newTestConfig()
	appNamespace := "rancher-fip-manager"
	project := &managementv3.Project{ObjectMeta: metav1.ObjectMeta{Name: "p-del", Namespace: "c-test1"}}

	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rancher-fip-config-p-del", Namespace: "c-test1"},
			Data: map[string][]byte{
				"clientSecret": []byte("secret"),
				"project":      []byte("p-del"),
			},
		},
	).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects().Build()

	res, err := HandleConfigSecrets(
		context.Background(), local, local, downstream,
		cfg.RancherFipLBControllerNamespace, project, "c-test1", "net",
		cfg.RancherFipApiServerURL, "c-test1", "purelb", []byte("stale-startup-snapshot"), appNamespace,
	)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	err = downstream.Get(context.Background(), types.NamespacedName{Name: "cacerts", Namespace: cfg.RancherFipLBControllerNamespace}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "a deleted local cacerts secret must not propagate the startup snapshot")
}
