package floatingippool

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
)

// newStartedController builds a controller whose informers are started and
// synced, with an optional injected syncHandler.
func newStartedController(t *testing.T, sync func(ctx context.Context, key string) error) *Controller {
	t.Helper()

	clientset := fake.NewSimpleClientset()
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()
	quotaInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	// New() must run first: the generated accessors return lightweight
	// structs and only register the informers with the factory on the first
	// .Informer() call inside New(). Starting before that would leave the
	// informers unstarted and HasSynced false forever.
	controller := New(clientset, kubeClient, fipInformer, fipPoolInformer, quotaInformer, ipam.New())

	stopCh := make(chan struct{})
	informerFactory.Start(stopCh)
	require.True(t, allSynced(informerFactory.WaitForCacheSync(stopCh)), "informer caches failed to sync")
	t.Cleanup(func() { close(stopCh) })

	if sync != nil {
		controller.syncHandler = sync
	}
	return controller
}

func allSynced(res map[reflect.Type]bool) bool {
	for _, ok := range res {
		if !ok {
			return false
		}
	}
	return len(res) > 0
}

// Regression for report 1.5: the initial-sync drain used to run a fixed count
// of blocking processNextWorkItem calls, so keys waiting out a rate-limiter
// backoff in workqueue.Get() could park close(initSyncDone) — and main with
// it — forever while the leader lease was held. Run must release the gate
// promptly even when every early sync fails and retries are delayed.
func TestRunReleasesInitialSyncGateDespiteDelayedRequeues(t *testing.T) {
	controller := newStartedController(t, func(ctx context.Context, key string) error {
		return assert.AnError
	})

	// One immediately-available key (whose sync fails) plus a key already
	// parked in the rate limiter's delayed queue with a large backoff
	// (~20 s after 12 requeues with the default limiter).
	controller.workqueue.Add("pool-failing")
	for range 12 {
		controller.workqueue.AddRateLimited("pool-wedged")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx, 1) }()

	select {
	case <-controller.IsInitialSyncDone():
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not release the initial-sync gate")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on context cancellation")
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// Regression for report 1.5: cache-sync failure used to exit via
// logrus.Fatal (process death bypassing graceful shutdown). A cancelled
// context must instead release Run and the initial-sync gate.
func TestRunReturnsGracefullyWhenContextCancelledBeforeCacheSync(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	// Deliberately never started: HasSynced stays false.

	controller := New(
		clientset,
		kubeClient,
		informerFactory.Rancher().V1beta2().FloatingIPs(),
		informerFactory.Rancher().V1beta2().FloatingIPPools(),
		informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas(),
		ipam.New(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx, 1) }()
	select {
	case err := <-done:
		require.NoError(t, err, "cancelled context must end Run without Fatal")
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	select {
	case <-controller.IsInitialSyncDone():
	default:
		t.Fatal("initial-sync gate must be released on every Run exit path")
	}
}

// Regression for report 1.4: a panicking syncHandler is recovered and turned
// into a rate-limited requeue instead of crashing the process.
func TestProcessNextWorkItemRecoversSyncHandlerPanic(t *testing.T) {
	controller := newStartedController(t, func(ctx context.Context, key string) error {
		panic("boom")
	})

	controller.workqueue.Add("pool-a")
	require.True(t, controller.processNextWorkItem(context.Background()),
		"worker must keep running after a panicking reconcile")
	assert.Equal(t, 1, controller.workqueue.NumRequeues("pool-a"),
		"panicked key must be requeued with rate limiting")
}
