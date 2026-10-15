package floatingipprojectquota

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/controller/controllerutil"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions/rancher.k8s.binbash.org/v1beta2"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const controllerAgentName = "floatingipprojectquota-controller"

const quotaFinalizerName = "rancher.k8s.binbash.org/floatingipprojectquota-cleanup"

const projectNameLabel = "rancher.k8s.binbash.org/project-name"

// Cascade requeue pacing: exponential from baseDelay, capped, so a stalled
// FloatingIP deletion converges on a bounded poll instead of hammering at a
// fixed 2 s forever.
const (
	cascadeRetryBaseDelay = 2 * time.Second
	cascadeRetryMaxDelay  = 30 * time.Second
)

// Controller is the controller implementation for FloatingIPProjectQuota resources.
// It keeps the FloatingIPProjectQuota status in sync with its attached FloatingIPs and cascades
// FloatingIPProjectQuota deletion: a FloatingIPProjectQuota being deleted first deletes all FloatingIPs labeled
// with its name, and only then releases itself via its finalizer.
type Controller struct {
	clientset                    clientset.Interface
	kubeClient                   kubernetes.Interface
	floatingIPProjectQuotaLister listers.FloatingIPProjectQuotaLister
	floatingIPProjectQuotaSynced cache.InformerSynced
	fipLister                    listers.FloatingIPLister
	fipSynced                    cache.InformerSynced
	fipPoolLister                listers.FloatingIPPoolLister
	fipPoolSynced                cache.InformerSynced
	workqueue                    workqueue.RateLimitingInterface
	initSyncDone                 chan struct{}
	// initSyncOnce guards initSyncDone so every Run exit path releases the
	// gate exactly once (see Run).
	initSyncOnce sync.Once

	// syncHandler is the reconcile entry point, invoked per workqueue key by
	// the workers. It is a field (bound to syncHandlerImpl in New) so tests
	// can inject failure/panic behaviour.
	syncHandler func(ctx context.Context, key string) error

	// cascadeRetries tracks, per quota key, how many cascade passes have
	// already run while FloatingIPs are still attached. Deleted once the
	// cascade completes so a future deletion starts at the base delay again.
	cascadeRetries sync.Map

	// cascadeEnqueueAfter requeues a cascade pass after a backoff delay.
	// Bound to workqueue.AddAfter in New; a seam for tests.
	cascadeEnqueueAfter func(key string, delay time.Duration)
}

// New creates a new Controller for managing FloatingIPProjectQuota resources.
func New(
	clientset clientset.Interface,
	kubeClient kubernetes.Interface,
	floatingIPProjectQuotaInformer informers.FloatingIPProjectQuotaInformer,
	fipInformer informers.FloatingIPInformer,
	fipPoolInformer informers.FloatingIPPoolInformer) *Controller {

	controller := &Controller{
		clientset:                    clientset,
		kubeClient:                   kubeClient,
		floatingIPProjectQuotaLister: floatingIPProjectQuotaInformer.Lister(),
		floatingIPProjectQuotaSynced: floatingIPProjectQuotaInformer.Informer().HasSynced,
		fipLister:                    fipInformer.Lister(),
		fipSynced:                    fipInformer.Informer().HasSynced,
		fipPoolLister:                fipPoolInformer.Lister(),
		fipPoolSynced:                fipPoolInformer.Informer().HasSynced,
		workqueue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "FloatingIPProjectQuotas"),
		initSyncDone:                 make(chan struct{}),
	}
	controller.syncHandler = controller.syncHandlerImpl
	controller.cascadeEnqueueAfter = func(key string, delay time.Duration) {
		controller.workqueue.AddAfter(key, delay)
	}

	logrus.Info("Setting up event handlers for FloatingIPProjectQuota controller")
	floatingIPProjectQuotaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleFloatingIPProjectQuotaCreate,
		UpdateFunc: func(_, newObj interface{}) {
			controller.enqueueFloatingIPProjectQuota(newObj)
		},
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

	logrus.Info("Starting FloatingIPProjectQuota controller")

	logrus.Info("Waiting for informer caches to sync for FloatingIPProjectQuota controller")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.floatingIPProjectQuotaSynced, c.fipSynced, c.fipPoolSynced); !ok {
		if ctx.Err() != nil {
			// Shutting down before the caches synced; not a failure to report.
			return nil
		}
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logrus.Info("Starting initial sync for FloatingIPProjectQuota controller")
	c.processInitialSyncItems(ctx)
	logrus.Info("Finished initial sync for FloatingIPProjectQuota controller")
	c.completeInitialSync()

	logrus.Info("Starting workers for FloatingIPProjectQuota controller")
	for range workers {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logrus.Info("Started workers for FloatingIPProjectQuota controller")
	<-ctx.Done()
	logrus.Info("Shutting down workers for FloatingIPProjectQuota controller")
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
	logrus.Infof("FloatingIPProjectQuota controller initial sync: up to %d items queued", queueLen)
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
		logrus.Infof("FloatingIPProjectQuota %s successfully synced", key)
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

	floatingIPProjectQuota, err := c.floatingIPProjectQuotaLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("floatingipprojectquota '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	logrus.Infof("Syncing FloatingIPProjectQuota %s", name)

	// FloatingIPProjectQuota is being deleted: cascade-delete all attached FloatingIPs before
	// letting the quota go away.
	if !floatingIPProjectQuota.GetDeletionTimestamp().IsZero() {
		return c.handleFloatingIPProjectQuotaDelete(ctx, key, name)
	}

	// Ensure the cleanup finalizer is present so deletion can be cascaded.
	if !containsString(floatingIPProjectQuota.GetFinalizers(), quotaFinalizerName) {
		logrus.Infof("Adding finalizer to FloatingIPProjectQuota %s", name)
		quotaWithFinalizer := floatingIPProjectQuota.DeepCopy()
		quotaWithFinalizer.Finalizers = append(quotaWithFinalizer.GetFinalizers(), quotaFinalizerName)
		_, err = c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Update(ctx, quotaWithFinalizer, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to add finalizer to FloatingIPProjectQuota %s: %w", name, err)
		}
		// Continue syncing from the updated object so the status update below
		// does not overwrite the object with a pre-finalizer copy.
		floatingIPProjectQuota = quotaWithFinalizer
	}

	fips, err := c.fipLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list floatingips: %w", err)
	}

	newFipsStatus := make(map[string]*v1beta2.FipInfo)

	for _, fip := range fips {
		projectName, ok := fip.Labels[projectNameLabel]
		if !ok || projectName != name {
			continue
		}

		poolName := fip.Spec.FloatingIPPool
		if poolName == "" {
			continue
		}

		if _, ok := newFipsStatus[poolName]; !ok {
			newFipsStatus[poolName] = &v1beta2.FipInfo{
				Allocated: make(map[string]string),
			}
			pool, err := c.fipPoolLister.Get(poolName)
			if err != nil {
				runtime.HandleError(fmt.Errorf("failed to get fip pool %s: %w", poolName, err))
			} else {
				if pool.Spec.IPConfig != nil {
					newFipsStatus[poolName].Family = pool.Spec.IPConfig.Family
				}
			}
		}

		if fip.Status.IPAddr != "" {
			var clusterName string
			if fip.Status.Assigned != nil && fip.Status.Assigned.ClusterName != "" {
				clusterName = fip.Status.Assigned.ClusterName
			} else {
				clusterName = "Unassigned"
			}
			newFipsStatus[poolName].Allocated[fip.Status.IPAddr] = clusterName
			newFipsStatus[poolName].Used++
		}
	}

	newStatus := v1beta2.FloatingIPProjectQuotaStatus{
		FloatingIPs: newFipsStatus,
	}

	if reflect.DeepEqual(floatingIPProjectQuota.Status, newStatus) {
		logrus.Infof("FloatingIPProjectQuota %s status is already up to date.", name)
		return nil
	}

	floatingIPProjectQuotaToUpdate := floatingIPProjectQuota.DeepCopy()
	floatingIPProjectQuotaToUpdate.Status = newStatus

	_, err = c.clientset.RancherV1beta2().FloatingIPProjectQuotas().UpdateStatus(ctx, floatingIPProjectQuotaToUpdate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("could not update status for FloatingIPProjectQuota %s: %w", name, err)
	}
	logrus.Infof("Successfully synced FloatingIPProjectQuota %s", name)
	return nil
}

// handleFloatingIPProjectQuotaDelete cascades the deletion of a FloatingIPProjectQuota: all
// FloatingIPs labeled with the FloatingIPProjectQuota's name are deleted first. Each FloatingIP
// releases its IPAM allocation and pool slot via its own floatingip-cleanup
// finalizer (which tolerates the FloatingIPProjectQuota already being gone). The FloatingIPProjectQuota finalizer
// is only removed once no attached FloatingIPs remain.
func (c *Controller) handleFloatingIPProjectQuotaDelete(ctx context.Context, key, name string) error {
	fips, err := c.fipLister.List(labels.SelectorFromSet(labels.Set{projectNameLabel: name}))
	if err != nil {
		return fmt.Errorf("failed to list floatingips for FloatingIPProjectQuota %s: %w", name, err)
	}

	if len(fips) > 0 {
		logrus.Infof("FloatingIPProjectQuota %s is being deleted: deleting %d attached FloatingIP(s) first", name, len(fips))
		for _, fip := range fips {
			err = c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Delete(ctx, fip.Name, metav1.DeleteOptions{})
			if errors.IsNotFound(err) {
				// Informer cache is stale; the requeue below waits for it to catch up.
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to delete floatingip %s/%s during FloatingIPProjectQuota %s deletion: %w", fip.Namespace, fip.Name, name, err)
			}
			logrus.Infof("Deleted FloatingIP %s/%s as part of FloatingIPProjectQuota %s deletion", fip.Namespace, fip.Name, name)
		}

		// FloatingIPs are gone from the API but the informer cache may still
		// list them; requeue until the list is empty and their finalizers ran.
		// Passes back off exponentially (2 s doubling, capped at 30 s) so a
		// stalled FloatingIP deletion converges on a bounded retry rate
		// instead of an unbounded fixed 2 s poll.
		attempt := 1
		if prev, ok := c.cascadeRetries.Load(key); ok {
			attempt = prev.(int) + 1
		}
		c.cascadeRetries.Store(key, attempt)
		c.cascadeEnqueueAfter(key, cascadeRetryDelay(attempt))
		return nil
	}
	c.cascadeRetries.Delete(key)
	quota, err := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get FloatingIPProjectQuota %s: %w", name, err)
	}

	if !containsString(quota.GetFinalizers(), quotaFinalizerName) {
		return nil
	}

	logrus.Infof("All FloatingIPs for FloatingIPProjectQuota %s are deleted, removing finalizer", name)
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentQuota, errGet := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(ctx, name, metav1.GetOptions{})
		if errGet != nil {
			return errGet
		}
		quotaCopy := currentQuota.DeepCopy()
		quotaCopy.Finalizers = removeString(quotaCopy.GetFinalizers(), quotaFinalizerName)
		_, errUpdate := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Update(ctx, quotaCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from FloatingIPProjectQuota %s: %w", name, err)
	}
	return nil
}

// cascadeRetryDelay returns the requeue delay for the attempt-th (1-based)
// cascade pass of a Terminating FloatingIPProjectQuota: 2 s, 4 s, 8 s, …
// capped at cascadeRetryMaxDelay.
func cascadeRetryDelay(attempt int) time.Duration {
	delay := cascadeRetryBaseDelay
	for i := 1; i < attempt && delay < cascadeRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > cascadeRetryMaxDelay {
		delay = cascadeRetryMaxDelay
	}
	return delay
}

func (c *Controller) handleFloatingIPProjectQuotaCreate(obj interface{}) {
	_, ok := obj.(*v1beta2.FloatingIPProjectQuota)
	if !ok {
		runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
		return
	}

	c.enqueueFloatingIPProjectQuota(obj)
}

func (c *Controller) enqueueFloatingIPProjectQuota(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

// containsString checks if a string is in a slice of strings.
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// removeString removes a string from a slice of strings.
func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
