package floatingippool

import (
	"context"
	"fmt"
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

	v1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions/rancher.k8s.binbash.org/v1beta1"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta1"
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
func (c *Controller) Run(ctx context.Context, workers int) {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	logrus.Info("Starting FloatingIPPool controller")

	logrus.Info("Waiting for informer caches to sync for FloatingIPPool controller")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.fipPoolSynced, c.fipSynced, c.floatingIPProjectQuotaSynced); !ok {
		logrus.Fatal("failed to wait for caches to sync")
	}

	logrus.Info("Starting initial sync for FloatingIPPool controller")
	queueLen := c.workqueue.Len()
	logrus.Infof("FloatingIPPool controller initial sync: processing %d items", queueLen)
	for i := 0; i < queueLen; i++ {
		if !c.processNextWorkItem(ctx) {
			logrus.Fatal("worker shutdown during initial sync")
		}
	}
	logrus.Info("Finished initial sync for FloatingIPPool controller")
	close(c.initSyncDone)

	logrus.Info("Starting workers for FloatingIPPool controller")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logrus.Info("Started workers for FloatingIPPool controller")
	<-ctx.Done()
	logrus.Info("Shutting down workers for FloatingIPPool controller")
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

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
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

func (c *Controller) syncHandler(ctx context.Context, key string) error {
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

	for _, fip := range fips {
		if fip.Spec.FloatingIPPool == pool.Name {
			if fip.Spec.IPAddr != nil && *fip.Spec.IPAddr != "" {
				allocatedIPs = append(allocatedIPs, *fip.Spec.IPAddr)

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
				allocatedMap[*fip.Spec.IPAddr] = allocatedDisplayName
			}
		}
	}

	// Update IPAM atomically
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

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		poolToUpdate, err := c.clientset.RancherV1beta1().FloatingIPPools().Get(ctx, pool.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get latest version of fip pool %s: %w", pool.Name, err)
		}

		poolToUpdate.Status = v1beta1.FloatingIPPoolStatus{
			Allocated: allocatedMap,
			Used:      c.ipam.Used(pool.Name),
			Available: c.ipam.Available(pool.Name),
		}
		_, err = c.clientset.RancherV1beta1().FloatingIPPools().UpdateStatus(ctx, poolToUpdate, metav1.UpdateOptions{})
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
	_, ok := obj.(*v1beta1.FloatingIPPool)
	if !ok {
		runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
		return
	}

	c.enqueueFipPool(obj)
}

func (c *Controller) handleFipPoolDelete(obj interface{}) {
	pool, ok := obj.(*v1beta1.FloatingIPPool)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		pool, ok = tombstone.Obj.(*v1beta1.FloatingIPPool)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}
	logrus.Infof("FloatingIPPool deleted: %s", pool.Name)
	c.ipam.DeleteSubnet(pool.Name)
}
