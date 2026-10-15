package floatingippool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/controller/controllerutil"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions/rancher.k8s.binbash.org/v1beta2"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"k8s.io/client-go/kubernetes"
)

const controllerAgentName = "floatingippool-controller"

// Controller is the controller implementation for FloatingIPPool resources.
// It is responsible for managing the lifecycle of FloatingIPPool objects,
// primarily by populating the IPAM service with subnets and rebuilding
// the status of pools on startup.
type Controller struct {
	clientset                    clientset.Interface
	kubeClient                   kubernetes.Interface
	fipLister                    listers.FloatingIPLister
	fipSynced                    cache.InformerSynced
	fipPoolLister                listers.FloatingIPPoolLister
	fipPoolSynced                cache.InformerSynced
	floatingIPProjectQuotaLister listers.FloatingIPProjectQuotaLister
	floatingIPProjectQuotaSynced cache.InformerSynced
	workqueue                    workqueue.RateLimitingInterface
	ipam                         *ipam.IPAllocator
	initSyncDone                 chan struct{}
	// initSyncOnce guards initSyncDone so every Run exit path releases the
	// gate exactly once (see Run).
	initSyncOnce sync.Once

	// syncHandler is the reconcile entry point, invoked per workqueue key by
	// the workers. It is a field (bound to syncHandlerImpl in New) so tests
	// can inject failure/panic behaviour.
	syncHandler func(ctx context.Context, key string) error
}

// New creates a new Controller for managing FloatingIPPool resources.
func New(
	clientset clientset.Interface,
	kubeClient kubernetes.Interface,
	fipInformer informers.FloatingIPInformer,
	fipPoolInformer informers.FloatingIPPoolInformer,
	floatingIPProjectQuotaInformer informers.FloatingIPProjectQuotaInformer,
	ipam *ipam.IPAllocator) *Controller {

	controller := &Controller{
		clientset:                    clientset,
		kubeClient:                   kubeClient,
		fipLister:                    fipInformer.Lister(),
		fipSynced:                    fipInformer.Informer().HasSynced,
		fipPoolLister:                fipPoolInformer.Lister(),
		fipPoolSynced:                fipPoolInformer.Informer().HasSynced,
		floatingIPProjectQuotaLister: floatingIPProjectQuotaInformer.Lister(),
		floatingIPProjectQuotaSynced: floatingIPProjectQuotaInformer.Informer().HasSynced,
		workqueue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "FloatingIPPools"),
		ipam:                         ipam,
		initSyncDone:                 make(chan struct{}),
	}
	controller.syncHandler = controller.syncHandlerImpl

	logrus.Info("Setting up event handlers for FloatingIPPool controller")
	fipPoolInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleFipPoolCreate,
		UpdateFunc: func(old, new interface{}) {
			// Always enqueue on update. The syncHandler will handle diffs and IPAM updates.
			controller.enqueueFipPool(new)
		},
		DeleteFunc: controller.handleFipPoolDelete,
	})

	return controller
}

// IsInitialSyncDone returns a channel that is closed when the initial sync is complete.
// This can be used to wait for the controller to be ready before starting other components.
func (c *Controller) IsInitialSyncDone() <-chan struct{} {
	return c.initSyncDone
}

// Run starts the controller, which will process items from the workqueue until the
// context is cancelled. It will start a number of worker goroutines to process
// items concurrently.
//
// Run returns when the context is cancelled or the controller could not
// start. Every exit path releases IsInitialSyncDone (see completeInitialSync):
// the previous implementation could park there forever, holding the leader
// lease with zero workers running.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()
	// Belt and braces: if Run bails out before (or during) the initial sync,
	// main's <-IsInitialSyncDone() must still be released. The sync.Once
	// makes this a no-op after the normal completion below.
	defer c.completeInitialSync()

	logrus.Info("Starting FloatingIPPool controller")

	logrus.Info("Waiting for informer caches to sync for FloatingIPPool controller")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.fipPoolSynced, c.fipSynced, c.floatingIPProjectQuotaSynced); !ok {
		if ctx.Err() != nil {
			// Shutting down before the caches synced; not a failure to report.
			return nil
		}
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logrus.Info("Starting initial sync for FloatingIPPool controller")
	c.processInitialSyncItems(ctx)
	logrus.Info("Finished initial sync for FloatingIPPool controller")
	c.completeInitialSync()

	logrus.Info("Starting workers for FloatingIPPool controller")
	for range workers {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logrus.Info("Started workers for FloatingIPPool controller")
	<-ctx.Done()
	logrus.Info("Shutting down workers for FloatingIPPool controller")
	return nil
}

// processInitialSyncItems drains the items the informer's initial list put on
// the workqueue, before dependent controllers are started by cmd/manager.
//
// It never blocks on the workqueue: the old fixed-count loop of
// processNextWorkItem calls went through workqueue.Get(), which blocks for as
// long as a failing key sits in the rate limiter's delayed queue (up to
// ~1000 s with the default limiter) — parking the initial-sync gate, and main
// with it, while the lease was held. The drain stops as soon as nothing is
// immediately available; keys still waiting out a backoff are picked up by
// the workers.
func (c *Controller) processInitialSyncItems(ctx context.Context) {
	queueLen := c.workqueue.Len()
	logrus.Infof("FloatingIPPool controller initial sync: up to %d items queued", queueLen)
	for range queueLen {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if c.workqueue.Len() == 0 {
			// Everything left is delayed for backoff; workers will retry it.
			return
		}
		if !c.processNextWorkItem(ctx) {
			// Queue shut down; the workers are going away with it.
			return
		}
	}
}

// completeInitialSync releases the initial-sync gate exactly once.
func (c *Controller) completeInitialSync() {
	c.initSyncOnce.Do(func() {
		close(c.initSyncDone)
	})
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done. A panic
	// from the syncHandler is recovered and turned into a rate-limited
	// requeue: without it, a panicking reconcile propagates out of the
	// worker goroutine and kills the whole manager process.
	err := func(obj interface{}) (err error) {
		var key string
		defer c.workqueue.Done(obj)
		defer func() {
			if r := recover(); r != nil {
				err = controllerutil.HandleSyncPanic(key, r)
				if key != "" {
					c.workqueue.AddRateLimited(key)
				}
			}
		}()
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.syncHandler(ctx, key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		logrus.Infof("FloatingIPPool %s successfully synced", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncHandlerImpl(ctx context.Context, key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	pool, err := c.fipPoolLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("floatingippool '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	logrus.Infof("Syncing FloatingIPPool %s", name)

	if pool.Spec.IPConfig == nil {
		return nil
	}

	// Gather all existing allocations (FIPs) for this pool
	allocatedIPs := []string{}
	allocatedMap := make(map[string]string)

	// Add excluded IPs to allocated map for status
	for _, excludedIP := range pool.Spec.IPConfig.Pool.Exclude {
		allocatedMap[excludedIP] = "excluded"
	}

	fips, err := c.fipLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list floatingips: %w", err)
	}

	logrus.Debugf("FloatingIPPool %s sync: found %d total FloatingIPs", name, len(fips))

	for _, fip := range fips {
		if fip.Spec.FloatingIPPool == pool.Name {
			// Skip FloatingIPs that are being deleted - they should not be included
			// in the allocation list as their IP is being released
			if fip.GetDeletionTimestamp() != nil {
				logrus.Debugf("FloatingIPPool %s sync: skipping FIP %s (being deleted)", name, fip.Name)
				continue
			}

			// Use Status.IPAddr instead of Spec.IPAddr to correctly track both
			// manually assigned and auto-allocated IPs. Spec.IPAddr is only set
			// for manually assigned IPs, while Status.IPAddr contains the actual
			// allocated IP (whether auto-allocated or manually assigned).
			if fip.Status.IPAddr != "" {
				// DEBUG: Log each IP being added to allocation list
				logrus.Debugf("FloatingIPPool %s sync: adding IP %s from FIP %s",
					name, fip.Status.IPAddr, fip.Name)
				allocatedIPs = append(allocatedIPs, fip.Status.IPAddr)

				// Determine display name for status
				var allocatedDisplayName string
				projectName, ok := fip.Labels["rancher.k8s.binbash.org/project-name"]
				if ok {
					allocatedTo, err := c.floatingIPProjectQuotaLister.Get(projectName)
					if err != nil {
						runtime.HandleError(fmt.Errorf("failed to get floatingipprojectquota %s: %w", projectName, err))
						allocatedDisplayName = fmt.Sprintf("%s [Unassigned]", projectName)
					} else {
						allocatedDisplayName = fmt.Sprintf("%s [%s]", projectName, allocatedTo.Spec.DisplayName)
					}
				} else {
					allocatedDisplayName = "Unassigned"
				}
				allocatedMap[fip.Status.IPAddr] = allocatedDisplayName
			}
		}

		// Log summary of allocations being used for IPAM refresh
		if len(allocatedIPs) > 0 {
			logrus.Debugf("FloatingIPPool %s: total IPs to allocate in IPAM: %d", name, len(allocatedIPs))
		}
	}

	// Update IPAM atomically
	logrus.Debugf("FloatingIPPool %s: refreshing subnet with %d allocated IPs: %v", pool.Name, len(allocatedIPs), allocatedIPs)
	err = c.ipam.RefreshSubnet(
		pool.Name,
		pool.Spec.IPConfig.Subnet,
		pool.Spec.IPConfig.Pool.Start,
		pool.Spec.IPConfig.Pool.End,
		pool.Spec.IPConfig.Pool.Exclude,
		allocatedIPs,
	)
	if err != nil {
		return fmt.Errorf("failed to refresh subnet for pool %s: %w", pool.Name, err)
	}
	logrus.Debugf("FloatingIPPool %s: IPAM refreshed, Used=%d, Available=%d", pool.Name, c.ipam.Used(pool.Name), c.ipam.Available(pool.Name))

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		poolToUpdate, err := c.clientset.RancherV1beta2().FloatingIPPools().Get(ctx, pool.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get latest version of fip pool %s: %w", pool.Name, err)
		}

		poolToUpdate.Status = v1beta2.FloatingIPPoolStatus{
			Allocated: allocatedMap,
			Used:      c.ipam.Used(pool.Name),
			Available: c.ipam.Available(pool.Name),
		}
		logrus.Debugf("FloatingIPPool %s: updating status with %d allocated IPs: %v", pool.Name, len(allocatedMap), allocatedMap)
		_, err = c.clientset.RancherV1beta2().FloatingIPPools().UpdateStatus(ctx, poolToUpdate, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update fip pool status for %s: %w", pool.Name, err)
	}

	logrus.Infof("Successfully synced FloatingIPPool %s", name)
	return nil
}

func (c *Controller) enqueueFipPool(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) handleFipPoolCreate(obj interface{}) {
	_, ok := obj.(*v1beta2.FloatingIPPool)
	if !ok {
		runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
		return
	}

	c.enqueueFipPool(obj)
}

func (c *Controller) handleFipPoolDelete(obj interface{}) {
	pool, ok := obj.(*v1beta2.FloatingIPPool)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		pool, ok = tombstone.Obj.(*v1beta2.FloatingIPPool)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}
	logrus.Infof("FloatingIPPool deleted: %s", pool.Name)
	c.ipam.DeleteSubnet(pool.Name)
}
