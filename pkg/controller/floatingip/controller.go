package floatingip

import (
	"context"
	"fmt"
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

	v1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
	applyv1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/generated/applyconfiguration/rancher.k8s.binbash.org/v1beta1"
	clientset "github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions/rancher.k8s.binbash.org/v1beta1"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta1"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
)

const controllerAgentName = "floatingip-controller"
const finalizerName = "rancher.k8s.binbash.org/floatingip-cleanup"
const projectNameLabel = "rancher.k8s.binbash.org/project-name"
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
func (c *Controller) Run(ctx context.Context, workers int) {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	logrus.Info("Starting FloatingIP controller")

	// Wait for the caches to be synced before starting workers
	logrus.Info("Waiting for informer caches to sync for FloatingIP controller")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.fipSynced, c.fipPoolSynced, c.floatingIPProjectQuotaSynced); !ok {
		logrus.Fatal("failed to wait for caches to sync")
	}

	logrus.Info("Starting initial sync for FloatingIP controller")
	queueLen := c.workqueue.Len()
	logrus.Infof("FloatingIP controller initial sync: processing %d items", queueLen)
	for i := 0; i < queueLen; i++ {
		if !c.processNextWorkItem(ctx) {
			logrus.Fatal("worker shutdown during initial sync")
		}
	}
	logrus.Info("Finished initial sync for FloatingIP controller")
	close(c.initSyncDone)

	logrus.Info("Starting workers for FloatingIP controller")
	// Launch workers to process FloatingIP resources
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logrus.Info("Started workers for FloatingIP controller")
	<-ctx.Done()
	logrus.Info("Shutting down workers for FloatingIP controller")
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

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
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

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the FloatingIP resource
// with the current status of the resource.
func (c *Controller) syncHandler(ctx context.Context, key string) error {
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
		fipApplyConfig := applyv1beta1.FloatingIP(fip.Name, fip.Namespace).
			WithFinalizers(append(fip.GetFinalizers(), finalizerName)...)

		_, err := c.clientset.RancherV1beta1().FloatingIPs(namespace).Apply(ctx, fipApplyConfig, metav1.ApplyOptions{FieldManager: controllerAgentName})
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

func (c *Controller) reconcile(ctx context.Context, fip *v1beta1.FloatingIP) error {
	logrus.Infof("Reconciling FloatingIP %s/%s", fip.Namespace, fip.Name)

	// Check if the FloatingIP is in an Error state
	if fip.Status.State == "Error" {
		logrus.Errorf("FloatingIP %s/%s is in Error state. Skipping reconciliation.", fip.Namespace, fip.Name)
		return nil // Skip further processing if in Error state
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
				currentProjectConfig, errGet := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
				if errGet != nil {
					return errGet
				}
				projectConfigCopy := currentProjectConfig.DeepCopy()
				if projectConfigCopy.Status.FloatingIPs != nil && projectConfigCopy.Status.FloatingIPs[poolName] != nil && projectConfigCopy.Status.FloatingIPs[poolName].Allocated != nil {
					projectConfigCopy.Status.FloatingIPs[poolName].Allocated[ipAddr] = newClusterName

					_, errUpdate := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
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
				currentFip, errGet := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
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

				_, updateErr := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
				return updateErr
			})
			if err != nil {
				return fmt.Errorf("failed to update fip status for fip %s/%s: %w", fip.Namespace, fip.Name, err)
			}
		} else {
			// else if the assigned status is nil, set it
			if fip.Status.Assigned == nil {
				logrus.Infof("Setting assigned status for FIP %s/%s because cluster label is present", fip.Namespace, fip.Name)

				err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
					currentFip, errGet := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
					if errGet != nil {
						return errGet
					}
					fipCopy := currentFip.DeepCopy()
					fipCopy.Status.Assigned = &v1beta1.AssignedInfo{
						ClusterName:      currentFip.Labels["rancher.k8s.binbash.org/cluster-name"],
						ProjectName:      currentFip.Labels["rancher.k8s.binbash.org/project-name"],
						ServiceNamespace: currentFip.Labels["rancher.k8s.binbash.org/service-namespace"],
						ServiceName:      currentFip.Labels["rancher.k8s.binbash.org/service-name"],
					}
					condition := metav1.Condition{
						Type:    "Ready",
						Status:  metav1.ConditionTrue,
						Reason:  "IPAssigned",
						Message: fmt.Sprintf("IP is assigned to service %s/%s in cluster %s.", currentFip.Labels["rancher.k8s.binbash.org/service-namespace"], currentFip.Labels["rancher.k8s.binbash.org/service-name"], currentFip.Labels["rancher.k8s.binbash.org/cluster-name"]),
					}
					fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

					_, updateErr := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
					return updateErr
				})
				if err != nil {
					return fmt.Errorf("failed to update fip status for fip %s/%s: %w", fip.Namespace, fip.Name, err)
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

	// Allocate IP
	var allocatedIP string
	var fipForSpecUpdate *v1beta1.FloatingIP = fip
	if fip.Spec.IPAddr != nil && *fip.Spec.IPAddr != "" {
		allocatedIP, err = c.ipam.GetIP(poolName, *fip.Spec.IPAddr)
		if err != nil {
			errMsg := fmt.Sprintf("failed to allocate requested IP %s from pool %s: %v", *fip.Spec.IPAddr, poolName, err)
			logrus.Error(errMsg)
			if updateErr := c.updateFipStatusWithError(ctx, fip, "AllocationFailed", errMsg); updateErr != nil {
				logrus.Errorf("failed to update fip status with error: %v", updateErr)
			}
			return fmt.Errorf("%s", errMsg)
		}
	} else {
		allocatedIP, err = c.ipam.GetIP(poolName, "")
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

	updatedFip, err := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).Update(ctx, fipCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update fip spec: %w", err)
	}
	fipForSpecUpdate = updatedFip

	// Update FloatingIP status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFip, errGet := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).Get(ctx, fip.Name, metav1.GetOptions{})
		if errGet != nil {
			return errGet
		}
		fipCopyForStatus := currentFip.DeepCopy()

		fipCopyForStatus.Status.IPAddr = allocatedIP
		fipCopyForStatus.Status.State = "Allocated"
		if currentFip.Labels != nil {
			fipCopyForStatus.Status.Assigned = &v1beta1.AssignedInfo{
				ClusterName:      currentFip.Labels["rancher.k8s.binbash.org/cluster-name"],
				ProjectName:      currentFip.Labels["rancher.k8s.binbash.org/project-name"],
				ServiceNamespace: currentFip.Labels["rancher.k8s.binbash.org/service-namespace"],
				ServiceName:      currentFip.Labels["rancher.k8s.binbash.org/service-name"],
			}
		}
		condition := metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "IPAssigned",
			Message: fmt.Sprintf("IP is assigned to service %s/%s in cluster %s.", currentFip.Labels["rancher.k8s.binbash.org/service-namespace"], currentFip.Labels["rancher.k8s.binbash.org/service-name"], currentFip.Labels["rancher.k8s.binbash.org/cluster-name"]),
		}
		fipCopyForStatus.Status.Conditions = c.setCondition(fipCopyForStatus.Status.Conditions, condition)

		_, updateErr := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopyForStatus, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to update fip status: %w", err)
	}

	// Update FloatingIPPool status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFipPool, errGet := c.clientset.RancherV1beta1().FloatingIPPools().Get(ctx, fipPool.Name, metav1.GetOptions{})
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

		_, errUpdate := c.clientset.RancherV1beta1().FloatingIPPools().UpdateStatus(ctx, currentFipPoolCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update fip pool status: %w", err)
	}

	// Update FloatingIPProjectQuota status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentProjectConfig, errGet := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
		if errGet != nil {
			return errGet
		}

		projectConfigCopy := currentProjectConfig.DeepCopy()
		if projectConfigCopy.Status.FloatingIPs == nil {
			projectConfigCopy.Status.FloatingIPs = make(map[string]*v1beta1.FipInfo)
		}
		if projectConfigCopy.Status.FloatingIPs[poolName] == nil {
			projectConfigCopy.Status.FloatingIPs[poolName] = &v1beta1.FipInfo{
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

		_, errUpdate := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update project config status: %w", err)
	}

	logrus.Infof("Successfully allocated IP %s to FloatingIP %s/%s", allocatedIP, fip.Namespace, fip.Name)
	return nil
}

func (c *Controller) reconcileDelete(ctx context.Context, fip *v1beta1.FloatingIP) error {
	logrus.Infof("Reconciling deletion of FloatingIP %s/%s", fip.Namespace, fip.Name)

	// The source of truth for an allocated IP is the status field.
	if fip.Status.IPAddr == "" {
		logrus.Infof("No IP assigned in status for FloatingIP %s/%s. Removing finalizer.", fip.Namespace, fip.Name)
		return c.removeFinalizer(ctx, fip)
	}

	ipToRelease := fip.Status.IPAddr
	poolName := fip.Spec.FloatingIPPool

	// Update FloatingIPProjectQuota status
	if projectName, ok := fip.Labels[projectNameLabel]; ok {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			projectConfig, errGet := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(ctx, projectName, metav1.GetOptions{})
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
					_, errUpdate := c.clientset.RancherV1beta1().FloatingIPProjectQuotas().UpdateStatus(ctx, projectConfigCopy, metav1.UpdateOptions{})
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
	if err := c.ipam.ReleaseIP(poolName, ipToRelease); err != nil {
		// Log the error but continue, as we still want to remove the finalizer
		logrus.Warnf("failed to release ip %s from ipam pool %s: %v", ipToRelease, poolName, err)
	}

	// enable for debugging purposes
	// c.ipam.Usage(poolName)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		currentFipPool, errGet := c.clientset.RancherV1beta1().FloatingIPPools().Get(ctx, poolName, metav1.GetOptions{})
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
			return nil // Already released
		}

		delete(currentFipPoolCopy.Status.Allocated, ipToRelease)
		currentFipPoolCopy.Status.Used = c.ipam.Used(poolName)
		currentFipPoolCopy.Status.Available = c.ipam.Available(poolName)

		_, errUpdate := c.clientset.RancherV1beta1().FloatingIPPools().UpdateStatus(ctx, currentFipPoolCopy, metav1.UpdateOptions{})
		return errUpdate
	})
	if err != nil {
		return fmt.Errorf("failed to update fip pool status on delete: %w", err)
	}

	return c.removeFinalizer(ctx, fip)
}

func (c *Controller) updateFipStatusWithError(ctx context.Context, fip *v1beta1.FloatingIP, reason, message string) error {
	fipCopy := fip.DeepCopy()
	fipCopy.Status.State = "Error"
	condition := metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
	fipCopy.Status.Conditions = c.setCondition(fipCopy.Status.Conditions, condition)

	_, err := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).UpdateStatus(ctx, fipCopy, metav1.UpdateOptions{})
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

func (c *Controller) removeFinalizer(ctx context.Context, fip *v1beta1.FloatingIP) error {
	fipCopy := fip.DeepCopy()
	fipCopy.SetFinalizers(removeString(fipCopy.GetFinalizers(), finalizerName))

	_, err := c.clientset.RancherV1beta1().FloatingIPs(fip.Namespace).Update(ctx, fipCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizer from floatingip %s/%s: %w", fip.Namespace, fip.Name, err)
	}

	logrus.Infof("Successfully removed finalizer from FloatingIP %s/%s", fip.Namespace, fip.Name)
	return nil
}
