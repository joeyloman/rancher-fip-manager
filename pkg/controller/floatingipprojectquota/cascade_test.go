package floatingipprojectquota

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
)

func TestCascadeRetryDelay(t *testing.T) {
	// Regression for report 1.6: the cascade used to requeue at a fixed 2 s
	// forever. The retry rate must grow exponentially and converge on the cap.
	require.Equal(t, 2*time.Second, cascadeRetryDelay(1))
	require.Equal(t, 4*time.Second, cascadeRetryDelay(2))
	require.Equal(t, 8*time.Second, cascadeRetryDelay(3))
	require.Equal(t, 16*time.Second, cascadeRetryDelay(4))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(5))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(6))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(200))
}

// Regression for report 1.6: while attached FloatingIPs are still deleting,
// the cascade must back off exponentially (no fixed 2 s hammering) and must
// surface why the quota sits in Terminating on the status, not only in logs.
func TestCascadeDeletionBacksOffAndSurfacesCondition(t *testing.T) {
	quota := &v1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-project",
			Finalizers:        []string{quotaFinalizerName},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
	}
	attachedFip := &v1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "attached-fip",
			Namespace:  "test-ns",
			Labels:     map[string]string{projectNameLabel: "test-project"},
			Finalizers: []string{"rancher.k8s.binbash.org/floatingip-cleanup"},
		},
		Spec: v1beta2.FloatingIPSpec{FloatingIPPool: "test-pool"},
	}

	objs := []runtime.Object{quota, attachedFip}
	clientset := fake.NewSimpleClientset(objs...)
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()
	projectInformer.Informer().GetIndexer().Add(quota)
	fipInformer.Informer().GetIndexer().Add(attachedFip)

	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)
	var delays []time.Duration
	controller.cascadeEnqueueAfter = func(key string, delay time.Duration) {
		delays = append(delays, delay)
	}

	key, err := cache.MetaNamespaceKeyFunc(quota)
	require.NoError(t, err)

	// Three cascade passes while the informer cache still lists the (already
	// deleted) FloatingIP: requeue delay must double each pass.
	for range 3 {
		require.NoError(t, controller.syncHandler(context.Background(), key))
	}
	assert.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}, delays,
		"cascade requeues must back off exponentially instead of a fixed 2 s loop")

	updated, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	cond := meta.FindStatusCondition(updated.Status.Conditions, cascadeConditionType)
	require.NotNil(t, cond, "cascade progress must be surfaced as a status condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "FloatingIPsStillAttached", cond.Reason)
	assert.Contains(t, cond.Message, "1 attached FloatingIP")

	// Once the cache observes the FloatingIP gone: no further requeue, the
	// retry bookkeeping is reset, and the finalizer is released.
	fipInformer.Informer().GetIndexer().Delete(attachedFip)
	require.NoError(t, controller.syncHandler(context.Background(), key))
	assert.Len(t, delays, 3, "completed cascade must not requeue again")
	_, pending := controller.cascadeRetries.Load(key)
	assert.False(t, pending, "cascade retry count must be cleared once the cascade completes")

	updated, err = clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Finalizers, quotaFinalizerName)
}

// Regression for report 1.4: panics from the quota reconcile are recovered in
// the worker and requeued instead of killing the manager process.
func TestProcessNextWorkItemRecoversSyncHandlerPanic(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()

	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)
	controller.syncHandler = func(ctx context.Context, key string) error {
		panic("boom")
	}

	controller.workqueue.Add("test-project")
	require.True(t, controller.processNextWorkItem(context.Background()),
		"worker must keep running after a panicking reconcile")
	assert.Equal(t, 1, controller.workqueue.NumRequeues("test-project"),
		"panicked key must be requeued with rate limiting")
}
