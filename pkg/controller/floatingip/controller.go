package floatingip

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/controller/controllerutil"
	applyv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/generated/applyconfiguration/rancher.k8s.binbash.org/v1beta2"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions/rancher.k8s.binbash.org/v1beta2"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"github.com/joeyloman/rancher-fip-manager/pkg/util"
)

const controllerAgentName = "floatingip-controller"
const finalizerName = "rancher.k8s.binbash.org/floatingip-cleanup"
const projectNameLabel = "rancher.k8s.binbash.org/project-name"
const floatingipGroupLabel = "rancher.k8s.binbash.org/floatingip-group"
const maxConditions = 30

// Controller is the controller implementation for FloatingIP resources. It is responsible
// for reconciling FloatingIP objects, which includes allocating an IP address from a
// specified pool, updating project quotas, and managing the lifecycle of the IP
// assignment through a finalizer.
type Controller struct {
	// clientset is a clientset for our own API group
	clientset  clientset.Interface
	kubeClient kubernetes.Interface

	fipLister                    listers.FloatingIPLister
	fipSynced                    cache.InformerSynced
	fipPoolLister                listers.FloatingIPPoolLister
	fipPoolSynced                cache.InformerSynced
	floatingIPProjectQuotaLister listers.FloatingIPProjectQuotaLister
	floatingIPProjectQuotaSynced cache.InformerSynced

	ipam *ipam.IPAllocator

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue    workqueue.RateLimitingInterface
	initSyncDone chan struct{}
	// initSyncOnce guards initSyncDone so every Run exit path releases the
	// gate exactly once (see Run).
	initSyncOnce sync.Once

	// syncHandler is the reconcile entry point, invoked per workqueue key by
	// the workers. It is a field (bound to syncHandlerImpl in New) so tests
	// can inject failure/panic behaviour.
	syncHandler func(ctx context.Context, key string) error
}

// New creates a new Controller for managing FloatingIP resources.
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
		ipam:                         ipam,
		workqueue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "FloatingIPs"),
		initSyncDone:                 make(chan struct{}),
	}
	controller.syncHandler = controller.syncHandlerImpl

	logrus.Info("Setting up event handlers for FloatingIP controller")
	// Set up an event handler for when FloatingIP resources change
	fipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueFip,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueFip(new)
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

	// Start the informer factories to begin populating the informer caches
	logrus.Info("Starting FloatingIP controller")

	// Wait for the caches to be synced before starting workers
	logrus.Info("Waiting for informer caches to sync for FloatingIP controller")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.fipSynced, c.fipPoolSynced, c.floatingIPProjectQuotaSynced); !ok {
		if ctx.Err() != nil {
			// Shutting down before the caches synced; not a failure to report.
			return nil
		}
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logrus.Info("Starting initial sync for FloatingIP controller")
	c.processInitialSyncItems(ctx)
	logrus.Info("Finished initial sync for FloatingIP controller")
	c.completeInitialSync()

	logrus.Info("Starting workers for FloatingIP controller")
	// Launch workers to process FloatingIP resources
	for range workers {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logrus.Info("Started workers for FloatingIP controller")
	<-ctx.Done()
	logrus.Info("Shutting down workers for FloatingIP controller")
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
	logrus.Infof("FloatingIP controller initial sync: up to %d items queued", queueLen)
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

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
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
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
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
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put on the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// FloatingIP resource to be synced.
		if err := c.syncHandler(ctx, key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		logrus.Infof("FloatingIP %s successfully synced", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandlerImpl compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the FloatingIP resource
// with the current status of the resource. It is reached through the
// c.syncHandler field so tests can inject behaviour.
func (c *Controller) syncHandlerImpl(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	fip, err := c.fipLister.FloatingIPs(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("floatingip '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	// Deletion logic
	if fip.GetDeletionTimestamp() != nil {
		if containsString(fip.GetFinalizers(), finalizerName) {
			return c.reconcileDelete(ctx, fip)
		}
		return nil // Object is being deleted, but no finalizer, so we do nothing.
	}

	// Add finalizer if it doesn't exist
	if !containsString(fip.GetFinalizers(), finalizerName) {
		logrus.Infof("Adding finalizer to FloatingIP %s/%s", fip.Namespace, fip.Name)
		fipApplyConfig := applyv1beta2.FloatingIP(fip.Name, fip.Namespace).
			WithFinalizers(append(fip.GetFinalizers(), finalizerName)...)

		_, err := c.clientset.RancherV1beta2().FloatingIPs(namespace).Apply(ctx, fipApplyConfig, metav1.ApplyOptions{FieldManager: controllerAgentName})
		if err != nil {
			return fmt.Errorf("failed to add finalizer to floatingip %s/%s: %w", fip.Namespace, fip.Name, err)
		}
		return nil // Requeue will happen automatically due to the update event.
	}

	// Main reconciliation logic
	return c.reconcile(ctx, fip)
}

// enqueueFip takes a FloatingIP resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than FloatingIP.
func (c *Controller) enqueueFip(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) setCondition(conditions []metav1.Condition, newCondition metav1.Condition) []metav1.Condition {
	// Check if the last condition is the same as the new one, if so, don't add it (prevents reconcile loops)
	if len(conditions) > 0 {
		if conditions[len(conditions)-1].Type == newCondition.Type && conditions[len(conditions)-1].Status == newCondition.Status && conditions[len(conditions)-1].Reason == newCondition.Reason && conditions[len(conditions)-1].Message == newCondition.Message {
			return conditions
		}
	}
	newCondition.LastTransitionTime = metav1.Now()

	// If the number of conditions is reaching the limit, remove the oldest one.
	if len(conditions) >= maxConditions {
		conditions = conditions[1:]
	}

	return append(conditions, newCondition)
}

func (c *Controller) reconcile(ctx context.Context, fip *v1beta2.FloatingIP) error {
	logrus.Infof("Reconciling FloatingIP %s/%s", fip.Namespace, fip.Name)

	// A previous allocation attempt failed (Status.State == "Error"). This is
	// not a dead end: fall through and re-attempt the work. Failed
	// allocations return an error, so the workqueue requeues this key with
	// rate-limited backoff; a successful attempt sets State back to
	// "Allocated" below.
	if fip.Status.State == "Error" {
		logrus.Infof("FloatingIP %s/%s is in Error state from a previous failed allocation. Re-attempting.", fip.Namespace, fip.Name)
	}

	// If IP is already assigned in status, our main work is done.
	// We just need to handle potential updates to service assignments.
	if fip.Status.IPAddr != "" {
		projectName, ok := fip.Labels[projectNameLabel]
		if !ok {
			return fmt.Errorf("fip %s/%s does not have project label '%s'", fip.Namespace, fip.Name, projectNameLabel)
		}
		projectConfig, err := c.floatingIPProjectQuotaLister.Get(projectName)
		if err != nil {
			if errors.IsNotFound(err) {
				runtime.HandleError(fmt.Errorf("floatingipprojectquota '%s' not found for namespace %s", projectName, fip.Namespace))
				return nil
			}
			return err
		}

		poolName := fip.Spec.FloatingIPPool
		ipAddr := fip.Status.IPAddr
		var currentClusterNameInProject string
		if projectConfig.Status.FloatingIPs != nil &&
			projectConfig.Status.FloatingIPs[poolName] != nil &&
			projectConfig.Status.FloatingIPs[poolName].Allocated != nil {
			currentClusterNameInProject = projectConfig.Status.FloatingIPs[poolName].Allocated[ipAddr]
		}

		var newClusterName string
		if fip.Status.Assigned != nil && fip.Status.Assigned.ClusterName != "" {
			newClusterName = fip.Status.Assigned.ClusterName
		} else {
			newClusterName = "Unassigned"
		}

		if currentClusterNameInProject != newClusterName {
			logrus.Infof("Updating floatingipprojectquota for FIP %s/%s. Cluster changed from '%s' to '%s'", fip.Namespace, fip.Name, currentClusterNameInProject, newClusterName)
			err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				currentProjectConfig, errGet := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
				if errGet != nil {
					return errGet
				}
				projectConfigCopy := currentProjectConfig.DeepCopy()
				if projectConfigCopy.Status.FloatingIPs != nil && projectConfigCopy.Status.FloatingIPs[poolName] != nil && projectConfigCopy.Status.FloatingIPs[poolName].Allocated != nil {
					projectConfigCopy.Status.FloatingIPs[poolName].Allocated[ipAddr] = newClusterName

					_, errUpdate := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
					return errUpdate
				} else {
					runtime.HandleError(fmt.Errorf("floatingipprojectquota status for pool %s is not fully initialized for FIP %s/%s", poolName, fip.Namespace, fip.Name))
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("failed to update project config status for fip %s/%s: %w", fip.Namespace, fip.Name, err)
			}
		}

		// Check if the "rancher.k8s.binbash.org/cluster-name" label is present, if not clear the assigned status
		if _, ok := fip.Labels["rancher.k8s.binbash.org/cluster-name"]; !ok {
			logrus.Infof("Clearing assigned status for FIP %s/%s because cluster label is not present", fip.Namespace, fip.Name)

			err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				currentFip, errGet := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
				if errGet != nil {
					return errGet
				}

				fipCopy := currentFip.DeepCopy()
				fipCopy.Status.Assigned = nil
				condition := metav1.Condition{
					Type:    "Ready",
					Status:  metav1.ConditionTrue,
					Reason:  "IPUnassigned",
					Message: "IP is unassigned from any service.",
				}
				fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

				_, updateErr := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
				return updateErr
			})
			if err != nil {
				return fmt.Errorf("failed to update fip status for fip %s/%s: %w", fip.Namespace, fip.Name, err)
			}
		} else {
			// Cluster label is present, check if we need to set or update the assigned status
			if fip.Status.Assigned == nil {
				logrus.Infof("Setting assigned status for FIP %s/%s because cluster label is present", fip.Namespace, fip.Name)

				// store the floatingip-group label
				floatingIPGroup := fip.Labels[floatingipGroupLabel]

				// if the floatingip-group label is set we need to generate a shared key
				var sharedKey string
				if floatingIPGroup != "" {
					suffix, errRand := util.GenerateRandomString(12)
					if errRand != nil {
						return fmt.Errorf("failed to generate floatingip-group shared key for %s/%s: %w", fip.Namespace, fip.Name, errRand)
					}
					sharedKey = fmt.Sprintf("%s-%s", fip.Labels["rancher.k8s.binbash.org/cluster-name"], suffix)
				}

				err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
					currentFip, errGet := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
					if errGet != nil {
						return errGet
					}
					fipCopy := currentFip.DeepCopy()
					services := parseServicesFromLabels(currentFip.Labels)
					fipCopy.Status.Assigned = &v1beta2.AssignedInfo{
						ClusterName:     currentFip.Labels["rancher.k8s.binbash.org/cluster-name"],
						ProjectName:     currentFip.Labels["rancher.k8s.binbash.org/project-name"],
						FloatingIPGroup: floatingIPGroup,
						SharedKey:       sharedKey,
						Services:        services,
					}
					serviceDesc := describeServices(services)
					condition := metav1.Condition{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "IPAssigned",
						Message: fmt.Sprintf("IP is assigned to %s in cluster %s.", serviceDesc, currentFip.Labels["rancher.k8s.binbash.org/cluster-name"]),
					}
					fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

					_, updateErr := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
					return updateErr
				})
				if err != nil {
					return fmt.Errorf("failed to update fip status for fip %s/%s: %w", fip.Namespace, fip.Name, err)
				}
			} else {
				// Assigned status already exists, but we need to update services if they've changed
				logrus.Infof("Updating services for FIP %s/%s because service labels may have changed", fip.Namespace, fip.Name)

				err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
					currentFip, errGet := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
					if errGet != nil {
						return errGet
					}
					fipCopy := currentFip.DeepCopy()
					services := parseServicesFromLabels(currentFip.Labels)
					fipCopy.Status.Assigned.Services = services
					serviceDesc := describeServices(services)
					condition := metav1.Condition{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "IPAssigned",
						Message: fmt.Sprintf("IP is assigned to %s in cluster %s.", serviceDesc, currentFip.Labels["rancher.k8s.binbash.org/cluster-name"]),
					}
					fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

					_, updateErr := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
					return updateErr
				})
				if err != nil {
					return fmt.Errorf("failed to update fip services for fip %s/%s: %w", fip.Namespace, fip.Name, err)
				}
			}
		}

		return nil
	}

	// If we are here, Status.IPAddr is empty. We need to allocate an IP.
	logrus.Infof("IP not yet allocated for %s/%s. Starting allocation process.", fip.Namespace, fip.Name)

	// Get the FloatingIPPool
	poolName := fip.Spec.FloatingIPPool
	if poolName == "" {
		return fmt.Errorf("floatingippool not specified for floatingip %s/%s", fip.Namespace, fip.Name)
	}
	fipPool, err := c.fipPoolLister.Get(poolName)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("floatingippool '%s' not found", poolName))
			return nil
		}
		return err
	}

	// Get the FloatingIPProjectQuota
	projectName, ok := fip.Labels[projectNameLabel]
	if !ok {
		return fmt.Errorf("fip %s/%s does not have project label '%s'", fip.Namespace, fip.Name, projectNameLabel)
	}
	projectConfig, err := c.floatingIPProjectQuotaLister.Get(projectName)
	if err != nil {
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("floatingipprojectquota '%s' not found for namespace %s", projectName, fip.Namespace))
			return nil
		}
		return err
	}

	// Allocate IP. Claims are owner-aware (namespace/name) so that a retry
	// after a partially persisted allocation (spec update landed, status
	// update failed) idempotently re-claims this FIP's own IP instead of
	// failing with "already allocated" forever.
	fipOwner := fip.Namespace + "/" + fip.Name
	var allocatedIP string
	var fipForSpecUpdate *v1beta2.FloatingIP = fip
	if fip.Spec.IPAddr != nil && *fip.Spec.IPAddr != "" {
		allocatedIP, err = c.ipam.GetIPOwned(poolName, *fip.Spec.IPAddr, fipOwner)
		if err != nil {
			errMsg := fmt.Sprintf("failed to allocate requested IP %s from pool %s: %v", *fip.Spec.IPAddr, poolName, err)
			logrus.Error(errMsg)
			if updateErr := c.updateFipStatusWithError(ctx, fip, "AllocationFailed", errMsg); updateErr != nil {
				logrus.Errorf("failed to update fip status with error: %v", updateErr)
			}
			return fmt.Errorf("%s", errMsg)
		}
	} else {
		allocatedIP, err = c.ipam.GetIPOwned(poolName, "", fipOwner)
		if err != nil {
			errMsg := fmt.Sprintf("failed to allocate an IP from pool %s: %v", poolName, err)
			logrus.Error(errMsg)
			if updateErr := c.updateFipStatusWithError(ctx, fip, "AllocationFailed", errMsg); updateErr != nil {
				logrus.Errorf("failed to update fip status with error: %v", updateErr)
			}
			return fmt.Errorf("%s", errMsg)
		}
	}

	if allocatedIP == "" {
		return fmt.Errorf("no IP could be allocated from pool %s", poolName)
	}

	// Update FloatingIP spec with allocated IP and add project label
	fipCopy := fip.DeepCopy()
	fipCopy.Spec.FloatingIPPool = fip.Spec.FloatingIPPool
	fipCopy.Spec.IPAddr = &allocatedIP

	updatedFip, err := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Update(ctx, fipCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update fip spec: %w", err)
	}
	fipForSpecUpdate = updatedFip

	// store the floatingip-group label
	floatingIPGroup := fip.Labels[floatingipGroupLabel]

	// if the floatingip-group label is set we need to generate a shared key
	var sharedKey string
	if floatingIPGroup != "" {
		suffix, errRand := util.GenerateRandomString(12)
		if errRand != nil {
			return fmt.Errorf("failed to generate floatingip-group shared key for %s/%s: %w", fip.Namespace, fip.Name, errRand)
		}
		sharedKey = fmt.Sprintf("%s-%s", fip.Labels["rancher.k8s.binbash.org/cluster-name"], suffix)
	}

	// Update FloatingIP status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFip, errGet := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
		if errGet != nil {
			return errGet
		}
		fipCopyForStatus := currentFip.DeepCopy()

		fipCopyForStatus.Status.IPAddr = allocatedIP
		fipCopyForStatus.Status.State = "Allocated"
		var services []v1beta2.AssignedService
		if currentFip.Labels != nil {
			services = parseServicesFromLabels(currentFip.Labels)
			fipCopyForStatus.Status.Assigned = &v1beta2.AssignedInfo{
				ClusterName:     currentFip.Labels["rancher.k8s.binbash.org/cluster-name"],
				ProjectName:     currentFip.Labels["rancher.k8s.binbash.org/project-name"],
				Services:        services,
				FloatingIPGroup: floatingIPGroup,
				SharedKey:       sharedKey,
			}
		}
		serviceDesc := describeServices(services)
		condition := metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "IPAssigned",
			Message: fmt.Sprintf("IP is assigned to %s in cluster %s.", serviceDesc, currentFip.Labels["rancher.k8s.binbash.org/cluster-name"]),
		}
		fipCopyForStatus.Status.Conditions = c.setCondition(fipCopyForStatus.Status.Conditions, condition)

		_, updateErr := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopyForStatus, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to update fip status: %w", err)
	}

	// Update FloatingIPPool status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFipPool, errGet := c.clientset.RancherV1beta2().FloatingIPPools().Get(ctx, fipPool.Name, metav1.GetOptions{})
		if errGet != nil {
			if errors.IsNotFound(errGet) {
				logrus.Warnf("FloatingIPPool %s not found during status update on FIP create. Assuming it's already gone.", fipPool.Name)
				return nil
			}
			return errGet
		}

		currentFipPoolCopy := currentFipPool.DeepCopy()
		if currentFipPoolCopy.Status.Allocated == nil {
			currentFipPoolCopy.Status.Allocated = make(map[string]string)
		}
		allocatedDisplayName := fmt.Sprintf("%s [%s]", projectName, projectConfig.Spec.DisplayName)

		currentFipPoolCopy.Status.Allocated[allocatedIP] = allocatedDisplayName
		currentFipPoolCopy.Status.Used = c.ipam.Used(poolName)
		currentFipPoolCopy.Status.Available = c.ipam.Available(poolName)

		_, errUpdate := c.clientset.RancherV1beta2().FloatingIPPools().UpdateStatus(ctx, currentFipPoolCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update fip pool status: %w", err)
	}

	// Update FloatingIPProjectQuota status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentProjectConfig, errGet := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
		if errGet != nil {
			return errGet
		}

		projectConfigCopy := currentProjectConfig.DeepCopy()
		if projectConfigCopy.Status.FloatingIPs == nil {
			projectConfigCopy.Status.FloatingIPs = make(map[string]*v1beta2.FipInfo)
		}
		if projectConfigCopy.Status.FloatingIPs[poolName] == nil {
			projectConfigCopy.Status.FloatingIPs[poolName] = &v1beta2.FipInfo{
				Allocated: make(map[string]string),
			}
		}
		if projectConfigCopy.Status.FloatingIPs[poolName].Allocated == nil {
			projectConfigCopy.Status.FloatingIPs[poolName].Allocated = make(map[string]string)
		}

		if fipPool.Spec.IPConfig != nil && fipPool.Spec.IPConfig.Family != "" {
			projectConfigCopy.Status.FloatingIPs[poolName].Family = fipPool.Spec.IPConfig.Family
		}
		var clusterName string
		if fipForSpecUpdate.Status.Assigned != nil && fipForSpecUpdate.Status.Assigned.ClusterName != "" {
			clusterName = fipForSpecUpdate.Status.Assigned.ClusterName
		} else {
			clusterName = "Unassigned"
		}

		// Only increment used count if the IP is new to this project's status
		if _, ok := projectConfigCopy.Status.FloatingIPs[poolName].Allocated[allocatedIP]; !ok {
			projectConfigCopy.Status.FloatingIPs[poolName].Used++
		}
		projectConfigCopy.Status.FloatingIPs[poolName].Allocated[allocatedIP] = clusterName

		_, errUpdate := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update project config status: %w", err)
	}

	logrus.Infof("Successfully allocated IP %s to FloatingIP %s/%s", allocatedIP, fip.Namespace, fip.Name)
	return nil
}

func (c *Controller) reconcileDelete(ctx context.Context, fip *v1beta2.FloatingIP) error {
	logrus.Infof("Reconciling deletion of FloatingIP %s/%s", fip.Namespace, fip.Name)

	// The source of truth for an allocated IP is the status field.
	if fip.Status.IPAddr == "" {
		// A partially persisted allocation (spec update landed, status update
		// failed) leaves an owned IPAM claim behind. Release it on the way out
		// so the address is not burned until a pool refresh or restart.
		// Owner-aware: an IP held by another FIP is never released here.
		if fip.Spec.IPAddr != nil && *fip.Spec.IPAddr != "" {
			switch err := c.ipam.ReleaseIPOwned(fip.Spec.FloatingIPPool, *fip.Spec.IPAddr, fip.Namespace+"/"+fip.Name); {
			case err == nil:
				logrus.Infof("FloatingIP %s/%s: released spec IP %s from IPAM", fip.Namespace, fip.Name, *fip.Spec.IPAddr)
			case ipam.IsForeignClaim(err):
				logrus.Warnf("FloatingIP %s/%s: spec IP %s is held by another claim in pool %s, not releasing it: %v",
					fip.Namespace, fip.Name, *fip.Spec.IPAddr, fip.Spec.FloatingIPPool, err)
			default:
				// Requested but never granted, or the claim was already wiped
				// by a pool refresh — a normal no-op, not an anomaly.
				logrus.Debugf("FloatingIP %s/%s: spec IP %s has no IPAM claim to release in pool %s: %v",
					fip.Namespace, fip.Name, *fip.Spec.IPAddr, fip.Spec.FloatingIPPool, err)
			}
		}
		logrus.Infof("No IP assigned in status for FloatingIP %s/%s. Removing finalizer.", fip.Namespace, fip.Name)
		return c.removeFinalizer(ctx, fip)
	}

	ipToRelease := fip.Status.IPAddr
	poolName := fip.Spec.FloatingIPPool

	// Update FloatingIPProjectQuota status
	if projectName, ok := fip.Labels[projectNameLabel]; ok {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			projectConfig, errGet := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
			if errors.IsNotFound(errGet) {
				logrus.Warnf("FloatingIPProjectQuota %s not found during FIP deletion. Assuming it's already gone.", projectName)
				return nil
			}
			if errGet != nil {
				return errGet
			}

			projectConfigCopy := projectConfig.DeepCopy()
			if pcStatus, pcOK := projectConfigCopy.Status.FloatingIPs[poolName]; pcOK && pcStatus != nil {
				if _, ipOK := pcStatus.Allocated[ipToRelease]; ipOK {
					if pcStatus.Used > 0 {
						pcStatus.Used--
					}
					delete(pcStatus.Allocated, ipToRelease)
					_, errUpdate := c.clientset.RancherV1beta2().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
					return errUpdate
				}
			}
			return nil // Nothing to update
		})
		if err != nil {
			return fmt.Errorf("failed to update project config status on delete: %w", err)
		}
	}

	// Release from IPAM and FloatingIPPool
	logrus.Infof("FloatingIP %s/%s: releasing IP %s from pool %s", fip.Namespace, fip.Name, ipToRelease, poolName)
	if err := c.ipam.ReleaseIP(poolName, ipToRelease); err != nil {
		// Log the error but continue, as we still want to remove the finalizer
		logrus.Warnf("failed to release ip %s from ipam pool %s: %v", ipToRelease, poolName, err)
	} else {
		logrus.Infof("FloatingIP %s/%s: successfully released IP %s from IPAM", fip.Namespace, fip.Name, ipToRelease)
	}

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFipPool, errGet := c.clientset.RancherV1beta2().FloatingIPPools().Get(ctx, poolName, metav1.GetOptions{})
		if errGet != nil {
			if errors.IsNotFound(errGet) {
				logrus.Warnf("FloatingIPPool %s not found during status update on FIP delete. Assuming it's already gone.", poolName)
				return nil
			}
			return errGet
		}

		currentFipPoolCopy := currentFipPool.DeepCopy()
		if currentFipPoolCopy.Status.Allocated == nil {
			return nil // Nothing to release
		}

		if _, ok := currentFipPoolCopy.Status.Allocated[ipToRelease]; !ok {
			// Even if the IP is not in the allocated map, we should still update the Used/Available counts
			// because ReleaseIP was just called successfully on the IPAM allocator.
			// This handles cases where the status map might be out of sync but IPAM was tracking it.
			// However, if IPAM also didn't have it, ReleaseIP would have returned an error (which we logged but ignored).
			// So it's safer to proceed with updating counts.
			logrus.Warnf("IP %s is not in the allocated map but ReleaseIP was successful. Updating counts anyway.", ipToRelease)
		}

		delete(currentFipPoolCopy.Status.Allocated, ipToRelease)
		currentFipPoolCopy.Status.Used = c.ipam.Used(poolName)
		currentFipPoolCopy.Status.Available = c.ipam.Available(poolName)

		logrus.Infof("FloatingIP %s/%s: updating pool %s status after release, removing IP %s, new Used=%d, Available=%d",
			fip.Namespace, fip.Name, poolName, ipToRelease, currentFipPoolCopy.Status.Used, currentFipPoolCopy.Status.Available)
		_, errUpdate := c.clientset.RancherV1beta2().FloatingIPPools().UpdateStatus(ctx, currentFipPoolCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update fip pool status on delete: %w", err)
	}

	return c.removeFinalizer(ctx, fip)
}

func (c *Controller) updateFipStatusWithError(ctx context.Context, fip *v1beta2.FloatingIP, reason, message string) error {
	condition := metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}

	// Failed allocations are retried with rate-limited backoff. Skip the
	// status write when the object already records this exact failure,
	// otherwise every retry rewrites an identical status.
	if fip.Status.State == "Error" {
		if last := len(fip.Status.Conditions) - 1; last >= 0 {
			lastCond := fip.Status.Conditions[last]
			if lastCond.Type == condition.Type && lastCond.Status == condition.Status &&
				lastCond.Reason == condition.Reason && lastCond.Message == condition.Message {
				return nil
			}
		}
	}

	fipCopy := fip.DeepCopy()
	fipCopy.Status.State = "Error"
	fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

	_, err := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update fip status with error: %w", err)
	}
	return nil
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

func (c *Controller) removeFinalizer(ctx context.Context, fip *v1beta2.FloatingIP) error {
	fipCopy := fip.DeepCopy()
	fipCopy.SetFinalizers(removeString(fipCopy.GetFinalizers(), finalizerName))

	_, err := c.clientset.RancherV1beta2().FloatingIPs(fip.Namespace).Update(ctx, fipCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from floatingip %s/%s: %w", fip.Namespace, fip.Name, err)
	}

	logrus.Infof("Successfully removed finalizer from FloatingIP %s/%s", fip.Namespace, fip.Name)
	return nil
}
