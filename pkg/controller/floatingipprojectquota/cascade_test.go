package floatingipprojectquota

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
)

func TestCascadeRetryDelay(t *testing.T) {
	// Regression for the cascade retry backoff: the cascade used to requeue at a fixed 2 s
	// forever. The retry rate must grow exponentially and converge on the cap.
	require.Equal(t, 2*time.Second, cascadeRetryDelay(1))
	require.Equal(t, 4*time.Second, cascadeRetryDelay(2))
	require.Equal(t, 8*time.Second, cascadeRetryDelay(3))
	require.Equal(t, 16*time.Second, cascadeRetryDelay(4))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(5))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(6))
	require.Equal(t, cascadeRetryMaxDelay, cascadeRetryDelay(200))
}

// Regression for the cascade retry backoff: while attached FloatingIPs are still deleting,
// the cascade must back off exponentially (no fixed 2 s hammering).
func TestCascadeDeletionBacksOff(t *testing.T) {
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

	// Once the cache observes the FloatingIP gone: no further requeue, the
	// retry bookkeeping is reset, and the finalizer is released.
	fipInformer.Informer().GetIndexer().Delete(attachedFip)
	require.NoError(t, controller.syncHandler(context.Background(), key))
	assert.Len(t, delays, 3, "completed cascade must not requeue again")
	_, pending := controller.cascadeRetries.Load(key)
	assert.False(t, pending, "cascade retry count must be cleared once the cascade completes")

	updated, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updated.Finalizers, quotaFinalizerName)
}

// Regression for the cascade retry bookkeeping: if the quota disappears from the
// cache while FloatingIPs are still attached, the cascade never reaches its
// own reset path. The stale per-key attempt count must be dropped, or a
// recreated quota with the same name would start its cascade at the capped
// delay instead of the base delay.
func TestCascadeRetriesClearedWhenQuotaVanishes(t *testing.T) {
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

	key, err := cache.MetaNamespaceKeyFunc(quota)
	require.NoError(t, err)

	// Two cascade passes build up backoff state for the key.
	for range 2 {
		require.NoError(t, controller.syncHandler(context.Background(), key))
	}
	_, pending := controller.cascadeRetries.Load(key)
	assert.True(t, pending, "cascade state must exist while FloatingIPs are attached")

	// The quota vanishes from the cache (deleted without a finalizer, or the
	// informer observing the API removal): the NotFound branch must clear it.
	projectInformer.Informer().GetIndexer().Delete(quota)
	require.NoError(t, controller.syncHandler(context.Background(), key))
	_, pending = controller.cascadeRetries.Load(key)
	assert.False(t, pending, "cascade retry count must be cleared once the quota is gone from the cache")
}
