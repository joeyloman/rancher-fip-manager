package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/joeyloman/rancher-fip-lb-controller/pkg/ipam"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/metallb"
	"github.com/joeyloman/rancher-fip-lb-controller/pkg/purelb"
	"github.com/sirupsen/logrus"
	"go.universe.tf/metallb/api/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

const (
	finalizerName = "rancher.k8s.binbash.org/floatingip-cleanup"

	// floatingIPAnnotation records the address allocated to a Service.
	floatingIPAnnotation = "rancher.k8s.binbash.org/floatingip"

	// floatingIPRequestAnnotation records that an allocation was requested but
	// its address was not persisted yet. A crash between RequestFIP and the
	// annotation write leaves only this marker; the next reconcile uses it to
	// adopt the existing FloatingIP instead of requesting a second one.
	// Its value is the project the request was made under.
	floatingIPRequestAnnotation = "rancher.k8s.binbash.org/floatingip-request"

	// floatingIPIndex indexes cached Services by their floatingip annotation,
	// so checking whether an address is still referenced is a local lookup
	// instead of a cluster-wide API list.
	floatingIPIndex = "floatingip"

	// projectAnnotation records the project a FloatingIP was allocated under.
	// The deletion path uses it instead of re-deriving the project from
	// namespace labels, which can change after allocation or disappear during
	// a project decommission.
	projectAnnotation = "rancher.k8s.binbash.org/floatingip-project"

	// cleanupProgressAnnotation records how far a deletion got before the
	// process stopped. A restart resumes at the recorded stage instead of
	// repeating the whole pool-delete/release/finalizer sequence.
	// Its value is one of the cleanupStage* constants.
	cleanupProgressAnnotation = "rancher.k8s.binbash.org/floatingip-cleanup"

	// cleanupStageResourcesDeleted means the MetalLB/PureLB pool and
	// advertisement (or PureLB ServiceGroup) have been withdrawn.
	cleanupStageResourcesDeleted = "resources-deleted"

	// cleanupStageReleased means the IPAM allocation has been released.
	cleanupStageReleased = "released"

	// resyncPeriod re-reconciles every cached Service periodically. Without
	// it, a Service whose allocation was skipped or whose pool, finalizer or
	// annotation was edited by hand never heals until a controller restart.
	resyncPeriod = 15 * time.Minute

	// deferredRequeueDelay retries a reconcile that deferred its decision
	// (quota, missing project or Secret, address conflict) instead of
	// dropping it until an unrelated event.
	deferredRequeueDelay = 5 * time.Minute

	// shutdownGracePeriod is how long Run lets in-flight reconciles finish
	// after cancellation. It stays below the shipped
	// terminationGracePeriodSeconds (30s) so the process can drain and exit
	// instead of being SIGKILLed between RequestFIP and the annotation write.
	shutdownGracePeriod = 20 * time.Second

	// shutdownAbortTimeout bounds the wait after in-flight reconciles are
	// canceled, so a call that ignores cancellation cannot keep the process
	// alive past the pod's grace period.
	shutdownAbortTimeout = 5 * time.Second
)

// serviceUpdateBackoff bounds the in-process conflict retries of a Service
// update. Conflicts are short-lived, and a reconcile must not hold a worker for
// the fixed minute the old polls waited. When the retries are
// exhausted the error returns to the work queue, which backs off without
// occupying a worker.
var serviceUpdateBackoff = wait.Backoff{
	Steps:    5,
	Duration: 50 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// Controller watches for service events and reconciles the state of FIPs.
type Controller struct {
	clientset       kubernetes.Interface
	metallbClient   *metallb.Client
	purelbClient    *purelb.Client
	serviceInformer cache.SharedIndexInformer
	queue           workqueue.RateLimitingInterface
	reconciler      *reconciler
	recorder        record.EventRecorder

	// tombstones holds the last known state of Services whose delete event
	// the informer replayed after a disconnect. The object is gone from the
	// cache by the time its queued key is processed, so the snapshot is the
	// only record of the allocation to release.
	tombstoneMu sync.Mutex
	tombstones  map[string]*v1.Service
}

// New creates a new controller.
func New(clientset kubernetes.Interface, metallbClient *metallb.Client, purelbClient *purelb.Client, appNamespace string, caStore *ipam.CAStore) *Controller {
	informerFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	serviceInformer := informerFactory.Core().V1().Services().Informer()

	// Index by allocation so shared-address checks read the informer cache
	// instead of listing every Service in the cluster. The
	// indexer is only added before the informer starts, and the index
	// function cannot fail, so the returned error has no reachable cause.
	_ = serviceInformer.AddIndexers(cache.Indexers{
		floatingIPIndex: indexServiceByFloatingIP,
	})

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "rancher-fip-lb-controller"})

	c := &Controller{
		clientset:       clientset,
		metallbClient:   metallbClient,
		purelbClient:    purelbClient,
		serviceInformer: serviceInformer,
		queue:           queue,
		recorder:        recorder,
		tombstones:      make(map[string]*v1.Service),
	}
	c.reconciler = &reconciler{
		clientset:      clientset,
		metallbClient:  metallbClient,
		purelbClient:   purelbClient,
		queue:          queue,
		recorder:       recorder,
		appNamespace:   appNamespace,
		caStore:        caStore,
		serviceIndexer: serviceInformer.GetIndexer(),
		newIPAMClient: func(apiURL, clientID string, caStore *ipam.CAStore) (ipamClient, error) {
			return ipam.NewClientWithCA(apiURL, clientID, caStore)
		},
	}

	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addFunc,
		UpdateFunc: c.updateFunc,
		DeleteFunc: c.deleteFunc,
	})

	return c
}

// Run starts the controller and drains in-flight reconciles before returning.
func (c *Controller) Run(ctx context.Context, workers int) {
	// Reconcile API calls use workCtx, derived from ctx but deliberately not
	// canceled by it. Cancellation must stop the queue without abandoning a
	// reconcile between RequestFIP and the annotation write; the bounded
	// drain below lets in-flight work finish, then cancels what is left.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	go c.serviceInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.serviceInformer.HasSynced) {
		c.queue.ShutDown()
		return
	}

	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			wait.Until(func() { c.runWorker(workCtx) }, time.Second, ctx.Done())
		}()
	}

	<-ctx.Done()

	// Stop handing out keys. A worker already inside a reconcile finishes it
	// against workCtx; its next Get observes the shutdown and exits.
	c.queue.ShutDown()

	drained := make(chan struct{})
	go func() {
		workersWG.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		logrus.Info("Controller workers drained")
		return
	case <-time.After(shutdownGracePeriod):
		logrus.Warnf("Controller workers still reconciling after %s, aborting in-flight work", shutdownGracePeriod)
	}

	cancelWork()
	select {
	case <-drained:
	case <-time.After(shutdownAbortTimeout):
		logrus.Warnf("Controller workers did not stop within %s after cancellation", shutdownAbortTimeout)
	}
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.reconcile(ctx, key.(string))
	if err != nil {
		logrus.Errorf("Error reconciling service: %v", err)
		c.queue.AddRateLimited(key)
	} else {
		c.queue.Forget(key)
	}
	return true
}

type ipamClient interface {
	RequestFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) (string, string, string, error)
	ReleaseFIP(ctx context.Context, clientSecret, cluster, project, floatingIPPool, serviceNamespace, serviceName, ipaddr, floatingIPGroup string) error
	ListFIPs(ctx context.Context, clientSecret, cluster, project, floatingIPPool string) (*ipam.FIPListResponse, error)
}

type metallbClient interface {
	CreateIPAddressPool(ctx context.Context, pool *v1beta1.IPAddressPool) error
	CreateL2Advertisement(ctx context.Context, ad *v1beta1.L2Advertisement) error
	DeleteIPAddressPool(ctx context.Context, name, namespace string) error
	DeleteL2Advertisement(ctx context.Context, name, namespace string) error
	GetIPAddressPools(ctx context.Context, namespace string) ([]v1beta1.IPAddressPool, error)
	GetIPAddressPool(ctx context.Context, name, namespace string) (*v1beta1.IPAddressPool, error)
	UpdateIPAddressPool(ctx context.Context, pool *v1beta1.IPAddressPool) error
}

type purelbClient interface {
	CreateServiceGroup(ctx context.Context, group *purelb.ServiceGroup) error
	CreateLBNodeAgent(ctx context.Context, agent *purelb.LBNodeAgent) error
	DeleteServiceGroup(ctx context.Context, name, namespace string) error
	DeleteLBNodeAgent(ctx context.Context, name, namespace string) error
	GetServiceGroups(ctx context.Context, namespace string) ([]purelb.ServiceGroup, error)
}

// requeuer schedules a later reconcile. The work queue satisfies it; the
// reconciler only needs AddAfter, and tests can substitute a recorder.
type requeuer interface {
	AddAfter(item interface{}, duration time.Duration)
}

// ipamClientEntry is one cached IPAM client together with the Secret values
// it was built from. A changed apiUrl or cluster invalidates the entry, so a
// rotated or corrected Secret takes effect without a restart.
type ipamClientEntry struct {
	apiURL   string
	clientID string
	client   ipamClient
}

// reconciler reconciles a Service object
type reconciler struct {
	clientset     kubernetes.Interface
	metallbClient metallbClient
	purelbClient  purelbClient
	queue         requeuer
	logger        *logrus.Logger
	recorder      record.EventRecorder
	appNamespace  string
	caStore       *ipam.CAStore

	// serviceIndexer is the informer's Service cache, indexed by the
	// floatingip annotation. It is nil in unit tests that construct the
	// reconciler directly; addressInUse then falls back to an API list.
	serviceIndexer cache.Indexer

	// addressLocks serializes the shared-address decision and cleanup of one
	// FloatingIP. With several workers, two group members deleted at once
	// would each see the other and both keep the pool, leaking it once both
	// are gone. Entries are never removed: one small
	// mutex per address ever seen, bounded by the address pool.
	addressLocksMu sync.Mutex
	addressLocks   map[string]*sync.Mutex

	// ipamClientOverride is a test seam: when set, every project uses this
	// client instead of the per-project cache below.
	ipamClientOverride ipamClient

	// newIPAMClient builds a real client. Tests replace it to observe cache
	// keys and rebuilds.
	newIPAMClient func(apiURL, clientID string, caStore *ipam.CAStore) (ipamClient, error)

	// ipamClients holds one client per project. Projects may point at
	// different IPAM endpoints, so a single shared client would send later
	// projects' requests to the first project's endpoint. Guarded by ipamMu
	// because every worker reconciles through the same reconciler.
	ipamMu      sync.Mutex
	ipamClients map[string]ipamClientEntry
}

// ipamClientFor returns the IPAM client for a project, creating and caching it
// on first use and rebuilding it when the project's Secret changes its apiUrl
// or cluster. The cache key is the project, not the Service, so all Services
// of a project share a client and its connection pool.
func (r *reconciler) ipamClientFor(projectID string, secret *v1.Secret) (ipamClient, error) {
	if r.ipamClientOverride != nil {
		return r.ipamClientOverride, nil
	}

	apiURL := string(secret.Data["apiUrl"])
	clientID := fmt.Sprintf("client-%s", string(secret.Data["cluster"]))

	r.ipamMu.Lock()
	defer r.ipamMu.Unlock()

	if entry, ok := r.ipamClients[projectID]; ok && entry.apiURL == apiURL && entry.clientID == clientID {
		return entry.client, nil
	}

	newClient := r.newIPAMClient
	if newClient == nil {
		newClient = func(apiURL, clientID string, caStore *ipam.CAStore) (ipamClient, error) {
			return ipam.NewClientWithCA(apiURL, clientID, caStore)
		}
	}
	client, err := newClient(apiURL, clientID, r.caStore)
	if err != nil {
		return nil, err
	}
	if r.ipamClients == nil {
		r.ipamClients = make(map[string]ipamClientEntry)
	}
	r.ipamClients[projectID] = ipamClientEntry{apiURL: apiURL, clientID: clientID, client: client}
	return client, nil
}

func (c *Controller) reconcile(ctx context.Context, key string) error {
	_, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// The key can never become valid, so retrying is pointless: returning
		// nil makes the worker Forget it instead of backing off forever.
		logrus.Errorf("Dropping invalid resource key %q: %v", key, err)
		return nil
	}

	obj, exists, err := c.serviceInformer.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		svc := c.peekTombstone(key)
		if svc == nil {
			logrus.Debugf("Service '%s' in work queue no longer exists", key)
			return nil
		}
		// The API object is gone, so only the deletion path can do anything
		// with the snapshot. A force-deleted Service never got a
		// DeletionTimestamp; mark it deleted so the allocation is released
		// instead of being treated as a new request.
		if svc.DeletionTimestamp == nil {
			now := metav1.Now()
			svc.DeletionTimestamp = &now
		}
		logrus.Infof("Cleaning up the FloatingIP of deleted Service '%s' from its last known state", key)
		if err := c.reconciler.reconcile(ctx, svc); err != nil {
			// The queue retries the key, but the object is not in the cache
			// to be looked up again: keep the snapshot so the retry still
			// knows which allocation to release.
			return err
		}
		c.takeTombstone(key)
		return nil
	}

	// The Service exists again: a snapshot of the deleted object must not
	// release the allocation of the Service that now owns the name.
	c.takeTombstone(key)

	svc := obj.(*v1.Service).DeepCopy()
	if err := c.reconciler.reconcile(ctx, svc); err != nil {
		// The reconcile may have recorded an allocation on the cached object
		// just before failing (for example, a Service deleted between
		// RequestFIP and the annotation write). Keep that snapshot so the
		// queue's retry can release the address after the informer drops the
		// deleted object.
		c.rememberTombstone(svc)
		return err
	}
	return nil
}

// getProjectIDFromAppNamespace retrieves the project ID from the controller's application namespace.
func (r *reconciler) getProjectIDFromAppNamespace(ctx context.Context) (string, error) {
	// Get the the projectId from the "rancher.k8s.binbash.org/project-name" label of namespace r.appNamespace.
	appNs, err := r.clientset.CoreV1().Namespaces().Get(ctx, r.appNamespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Debugf("Application namespace %s does not exist, this is not a virtual cluster", r.appNamespace)
			return "", nil
		}
		return "", fmt.Errorf("failed to get namespace %s: %w", r.appNamespace, err)
	}
	projectId, ok := appNs.Labels["rancher.k8s.binbash.org/project-name"]
	if !ok {
		logrus.Debugf("Application namespace %s does not have a project ID label, this is not a virtual cluster", r.appNamespace)
		return "", nil
	}
	return projectId, nil
}

func (r *reconciler) reconcile(ctx context.Context, svc *v1.Service) error {
	var ipAddress string
	var poolName string
	var floatingIPGroup string
	var projectId string
	var ok bool

	logger := logrus.WithFields(logrus.Fields{
		"service":    fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
		"serviceUID": svc.UID,
	})

	logger.Info("Reconciling Service")

	if svc.Annotations["rancher.k8s.binbash.org/floatingip-group"] != "" {
		floatingIPGroup = svc.Annotations["rancher.k8s.binbash.org/floatingip-group"]
	}

	// Handle service deletion and a Service converted away from LoadBalancer
	// while it still holds an allocation. Both must release the address and
	// remove the pool and finalizer; without the conversion case the address
	// stays allocated and the pool advertised forever.
	if svc.ObjectMeta.DeletionTimestamp != nil || (svc.Spec.Type != v1.ServiceTypeLoadBalancer && hasFloatingIPRecord(svc)) {
		// The finalizer normally drives this branch, but it can be removed by
		// hand while another finalizer keeps the object alive. The allocation
		// is still live then, so finish the cleanup instead of leaking the
		// address and its pool once the object disappears.
		// removeFinalizer below is a no-op when the finalizer is already gone.
		if hasFloatingIPRecord(svc) {
			if svc.Annotations != nil {
				ipAddress = svc.Annotations["rancher.k8s.binbash.org/floatingip"]
			}

			// Resolve the project the address was allocated under. The
			// annotation written at allocation time wins over the namespace
			// labels: those can change after allocation, and a different
			// project selects a different Secret and IPAM identity.
			var err error
			projectId, err = r.deletionProjectID(ctx, svc)
			if err != nil {
				return err
			}
			if projectId == "" {
				// No project means no Secret, no IPAM identity and therefore
				// no way to release. Returning here would keep the finalizer
				// and wedge the Service and its namespace in Terminating
				// forever, so finish the deletion instead.
				return r.abandonDeletion(ctx, logger, svc, ipAddress, projectId,
					fmt.Sprintf("namespace %s does not belong to a project", svc.Namespace))
			}

			// Create a new IPAM client
			secretName := fmt.Sprintf("rancher-fip-config-%s", projectId)
			secret, err := r.clientset.CoreV1().Secrets(r.appNamespace).Get(ctx, secretName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					// A decommissioned project (its Secret deleted) can never
					// release its addresses. Remove the finalizer instead of
					// retrying forever.
					return r.abandonDeletion(ctx, logger, svc, ipAddress, projectId,
						fmt.Sprintf("Secret %s/%s not found", r.appNamespace, secretName))
				}
				return fmt.Errorf("failed to get secret %s: %w", secretName, err)
			}

			// Determine the floating IP pool name once: the shared-use
			// decision below and the release both need it.
			fipPoolName := svc.Annotations["rancher.k8s.binbash.org/static-network"]
			if fipPoolName == "" {
				fipPoolName = string(secret.Data["floatingIPPool"])
			} else {
				logger.Infof("Found request for static network %s for service %s/%s", fipPoolName, svc.Namespace, svc.Name)
			}

			// A pending marker without the floatingip annotation means a
			// reconcile crashed between RequestFIP and the annotation write.
			// Ask the IPAM for this Service's allocation so deletion releases
			// it instead of leaking it. The lookup ignores
			// the floatingip-group annotation: the group is not part of the
			// Service's identity and may have been edited or removed after
			// allocation.
			if ipAddress == "" && svc.Annotations[floatingIPRequestAnnotation] != "" {
				ipamClient, err := r.ipamClientFor(projectId, secret)
				if err != nil {
					return fmt.Errorf("failed to create IPAM client: %w", err)
				}
				fips, err := ipamClient.ListFIPs(ctx,
					string(secret.Data["clientSecret"]),
					string(secret.Data["cluster"]),
					string(secret.Data["project"]),
					fipPoolName,
				)
				if err != nil {
					return fmt.Errorf("failed to list floating IPs for %s/%s: %w", svc.Namespace, svc.Name, err)
				}
				if recoveredIP, found := lookupServiceFloatingIP(fips, string(secret.Data["cluster"]), svc.Namespace, svc.Name); found {
					ipAddress = recoveredIP
					logger.Infof("Recovered pending FloatingIP %s for Service %s/%s from the IPAM", recoveredIP, svc.Namespace, svc.Name)
				}
			}

			if ipAddress != "" {
				// Check if the allocatedIPAddress is an IPv4 or IPv6 address so we can set the poolName
				if net.ParseIP(ipAddress) == nil {
					return fmt.Errorf("failed to parse allocated IP address: %s", ipAddress)
				}
				poolName = poolNameForAddress(ipAddress)
			}

			// Pools and advertisements are shared by every Service using the
			// same address, so only the last Service may delete them. The
			// decision must not depend on this Service's annotations: an
			// operator can remove the floatingip-group annotation (or a peer
			// can lose its floatingip annotation) while the pool still serves
			// other Services. Serialize the decision per
			// address so two group members deleted at once cannot each defer
			// to the other and leak the pool.
			unlockAddress := r.lockAddress(ipAddress)
			defer unlockAddress()

			sharedAddress := false
			if ipAddress != "" {
				sharedAddress, err = r.addressInUse(ctx, svc, ipAddress)
				if err != nil {
					return err
				}
				if !sharedAddress {
					// The pool's own allocation rules and the IPAM's claimant
					// labels still know about peers whose annotations are
					// gone.
					sharedAddress, err = r.sharedPoolInUse(ctx, svc, projectId, secret, ipAddress, poolName, fipPoolName)
					if err != nil {
						return err
					}
				}
			}

			if ipAddress != "" {
				// Resume a previously interrupted deletion at the recorded
				// stage instead of repeating steps that already completed.
				// Every step is also idempotent, so a crash between the work
				// and the progress write is safe.
				progress := svc.Annotations[cleanupProgressAnnotation]
				released := cleanupStageReached(progress, cleanupStageReleased)
				resourcesDeleted := cleanupStageReached(progress, cleanupStageResourcesDeleted)

				if !released && !sharedAddress && !resourcesDeleted {
					// Last claimant: withdraw the advertisement and delete
					// the pool before releasing the address in the IPAM.
					// Releasing first would make the address available to
					// another Service or cluster while this one still
					// advertises it, creating a duplicate-IP window on the L2
					// segment.
					if string(secret.Data["loadBalancerType"]) == "metallb" {
						// Delete MetalLB resources, advertisement first so the
						// address stops being announced before its pool is gone.
						if err := r.metallbClient.DeleteL2Advertisement(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
							return fmt.Errorf("failed to delete L2Advertisement %s: %w", poolName, err)
						}
						if err := r.metallbClient.DeleteIPAddressPool(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
							return fmt.Errorf("failed to delete IPAddressPool %s: %w", poolName, err)
						}
					} else if string(secret.Data["loadBalancerType"]) == "purelb" {
						// Delete PureLB resources
						if err := r.purelbClient.DeleteServiceGroup(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
							return fmt.Errorf("failed to delete ServiceGroup %s: %w", poolName, err)
						}
						// Not needed for now, but keeping the code here for future reference
						// if err := r.purelbClient.DeleteLBNodeAgent(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
						// 	return fmt.Errorf("failed to delete LBNodeAgent %s: %w", poolName, err)
						// }
					}
					if err := r.recordCleanupProgress(ctx, svc, cleanupStageResourcesDeleted); err != nil {
						return err
					}
				}

				if !released {
					// Release this Service's claim even when the address stays
					// allocated to peers. Release is per Service: the manager
					// removes only this Service's labels and keeps the FloatingIP
					// while another Service still claims it. Skipping it would
					// leave a stale claim in the IPAM that then looks like a live
					// peer and keeps the address allocated forever.
					// The group is deliberately not sent: the group
					// annotation is not part of the Service's identity and may
					// have been edited, while the address and service labels
					// already identify the claim exactly.
					ipamClient, err := r.ipamClientFor(projectId, secret)
					if err != nil {
						return fmt.Errorf("failed to create IPAM client: %w", err)
					}

					err = ipamClient.ReleaseFIP(ctx,
						string(secret.Data["clientSecret"]),
						string(secret.Data["cluster"]),
						string(secret.Data["project"]),
						fipPoolName,
						svc.Namespace,
						svc.Name,
						ipAddress,
						"",
					)
					if err != nil {
						return fmt.Errorf("failed to release FIP: %w", err)
					}
					if err := r.recordCleanupProgress(ctx, svc, cleanupStageReleased); err != nil {
						return err
					}

					if sharedAddress {
						logger.Infof("Released Service %s/%s claim on FloatingIP %s, keeping the allocation and its pool for the other Services", svc.Namespace, svc.Name, ipAddress)
					} else {
						logger.Infof("Successfully released FIP %s", ipAddress)
					}
				} else {
					logger.Infof("FloatingIP %s was already released, resuming at annotation and finalizer removal", ipAddress)
				}
			}

			// The annotation records this Service's own allocation; drop it
			// even when the address stays allocated for the other members of
			// a floatingIPGroup. The pending marker and the cleanup progress
			// go with it.
			if ipAddress != "" || svc.Annotations[floatingIPRequestAnnotation] != "" || svc.Annotations[cleanupProgressAnnotation] != "" {
				// Delete the allocation annotations. The update is retried
				// through conflicts only; waiting for a fixed minute would
				// block this worker and every reconcile behind it.
				_, err = r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
					changed := false
					if _, ok := currentSvc.Annotations[floatingIPAnnotation]; ok {
						delete(currentSvc.Annotations, floatingIPAnnotation)
						changed = true
					}
					if _, ok := currentSvc.Annotations[floatingIPRequestAnnotation]; ok {
						delete(currentSvc.Annotations, floatingIPRequestAnnotation)
						changed = true
					}
					if _, ok := currentSvc.Annotations[cleanupProgressAnnotation]; ok {
						delete(currentSvc.Annotations, cleanupProgressAnnotation)
						changed = true
					}
					return changed
				})
				if err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("failed to remove floatingip annotations from service %s/%s: %w", svc.Namespace, svc.Name, err)
				}
			}

			// Remove the finalizer
			if err := r.removeFinalizer(ctx, logger, svc); err != nil {
				return err
			}
		}
		return nil
	}

	// Only LoadBalancer Services get a FloatingIP. A ClusterIP Service can
	// still reach this point through a stale queue entry (it was a
	// LoadBalancer when enqueued); allocating for it would leak an address
	// and pin a finalizer on a Service that never uses it.
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return nil
	}

	ns, err := r.clientset.CoreV1().Namespaces().Get(ctx, svc.Namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", svc.Namespace, err)
	}

	// Check if the namespace if part of a project, if not check if the cluster is part of a project
	projectId, err = r.getProjectIDFromAppNamespace(ctx)
	if err != nil {
		return err
	}
	if projectId == "" {
		projectId, ok = ns.Labels["field.cattle.io/projectId"]
		if !ok {
			logger.Infof("Service namespace %s does not have a project ID label, skipping FloatingIP request", svc.Namespace)
			r.requeueAfter(svc, deferredRequeueDelay)
			return nil
		}
	}

	logger = logger.WithField("projectID", projectId)
	logger.Infof("Service is in project")

	// Construct the secret name and get the secret
	secretName := fmt.Sprintf("rancher-fip-config-%s", projectId)
	secret, err := r.clientset.CoreV1().Secrets(r.appNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Warnf("Secret %s not found in namespace %s, skipping FloatingIP request", secretName, r.appNamespace)
			r.requeueAfter(svc, deferredRequeueDelay)
			return nil
		}
		return fmt.Errorf("failed to get secret %s: %w", secretName, err)
	}

	logger.Infof("Successfully fetched secret %s", secret.Name)

	// Get the network-interface-mappings configmap
	configMap, err := r.clientset.CoreV1().ConfigMaps(r.appNamespace).Get(ctx, "network-interface-mappings", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get configmap network-interface-mappings: %w", err)
	}

	// Look up the network interface
	staticNetworkName := svc.Annotations["rancher.k8s.binbash.org/static-network"]
	fipPoolName := staticNetworkName
	if fipPoolName == "" {
		fipPoolName = string(secret.Data["floatingIPPool"])
	} else {
		logger.Infof("Found request for static network %s for service %s/%s", fipPoolName, svc.Namespace, svc.Name)
	}

	// Validate the project configuration before the IPAM is asked for an
	// address. With an unknown loadBalancerType or an incomplete Secret the
	// request used to succeed, the Service was annotated and finalized, and
	// no pool was ever created: the address was consumed and the
	// LoadBalancer stayed Pending forever without any signal.
	if err := validateProjectConfig(secret, fipPoolName); err != nil {
		logger.Errorf("Invalid configuration for project %s: %v", projectId, err)
		r.recorder.Eventf(svc, v1.EventTypeWarning, "InvalidConfiguration",
			"Cannot allocate a FloatingIP for this Service: %v", err)
		r.requeueAfter(svc, deferredRequeueDelay)
		return nil
	}

	networkInterface, ok := configMap.Data[fipPoolName]
	if !ok {
		return fmt.Errorf("no network interface found for floating ip pool %s", fipPoolName)
	}
	logger.Infof("Found network interface %s for floating ip pool %s", networkInterface, fipPoolName)

	// Check if a static IP is given in the service annotations
	ipAddress = svc.Annotations["rancher.k8s.binbash.org/static-ip"]
	if ipAddress != "" {
		logger.Infof("Found request for static IP %s for service %s/%s", ipAddress, svc.Namespace, svc.Name)
	}

	// Create (or reuse) the project's IPAM client. It is needed even when
	// adopting an existing allocation: the cleanup paths release the address
	// on a later error. Clients are cached per project and rebuilt when the
	// project's Secret changes its endpoint or cluster.
	ipamClient, err := r.ipamClientFor(projectId, secret)
	if err != nil {
		return fmt.Errorf("failed to create IPAM client: %w", err)
	}

	// The floatingip annotation is the controller's record that this Service
	// owns an address. Reconcile from it instead of asking the IPAM for
	// another one: the manager allocates a *new* FloatingIP for a repeated
	// request, so a retry or a controller restart after a partial reconcile
	// would otherwise leak an address.
	allocatedIPAddress := svc.Annotations["rancher.k8s.binbash.org/floatingip"]
	var subnet, sharedKey string
	adoptingExisting := allocatedIPAddress != ""

	// A Service that already has a load balancer address but no annotation
	// either lost its ownership record or was given an address by someone
	// else. A pending marker means an earlier reconcile requested an address
	// but did not persist it before exiting. In both cases ask the IPAM which
	// one it is: re-requesting would leak the existing allocation.
	pendingRequestProject := svc.Annotations[floatingIPRequestAnnotation]
	pendingRequest := pendingRequestProject != ""
	// projectForAnnotation is the allocation-time project persisted on the
	// Service. For an allocation adopted from the marker it is known exactly;
	// for an allocation recovered from ingress alone it is not (the labels
	// may have changed since), so the label-based fallback stays.
	projectForAnnotation := ""
	if !adoptingExisting && (len(svc.Status.LoadBalancer.Ingress) > 0 || pendingRequest) {
		fips, err := ipamClient.ListFIPs(ctx,
			string(secret.Data["clientSecret"]),
			string(secret.Data["cluster"]),
			string(secret.Data["project"]),
			fipPoolName,
		)
		if err != nil {
			return fmt.Errorf("failed to list floating IPs for %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		// The group annotation is not part of the Service's identity: it can
		// be edited or removed after the allocation. Matching on the service
		// labels alone avoids requesting a second address when it changed.
		recoveredIP, found := lookupServiceFloatingIP(fips, string(secret.Data["cluster"]), svc.Namespace, svc.Name)
		if found {
			allocatedIPAddress = recoveredIP
			adoptingExisting = true
			projectForAnnotation = pendingRequestProject
			logger.Infof("Recovered FloatingIP %s for Service %s/%s from the IPAM", recoveredIP, svc.Namespace, svc.Name)
		} else if len(svc.Status.LoadBalancer.Ingress) > 0 {
			logger.Infof("Service %s/%s already has a load balancer IP and no FloatingIP allocation, skipping FloatingIP request", svc.Namespace, svc.Name)
			return nil
		}
		// A pending marker without a listed allocation means the request
		// never produced one (or its cleanup completed): request again below.
	}

	// createdPool records whether this reconcile created the MetalLB/PureLB
	// pool object. Only such a pool may be deleted by the failure cleanup:
	// pools are shared by floatingIPGroup members.
	createdPool := false
	if adoptingExisting {
		// The IPAM response is the only source of the subnet; persist it at
		// allocation time so an adopted address can still rebuild its
		// PureLB ServiceGroup without asking for a new allocation.
		subnet = svc.Annotations["rancher.k8s.binbash.org/floatingip-subnet"]
		logger.Infof("Service already has FloatingIP %s, reconciling the existing allocation", allocatedIPAddress)
		if ipAddress != "" && ipAddress != allocatedIPAddress {
			logger.Warnf("Service requests static IP %s but already holds FloatingIP %s, keeping the existing allocation", ipAddress, allocatedIPAddress)
		}
	} else {
		projectForAnnotation = projectId
		// Persist the intent before asking the IPAM. A crash after the
		// request but before the annotation write then leaves a marker that
		// the next reconcile uses to adopt the allocation instead of
		// requesting a second one.
		if err := r.markAllocationPending(ctx, svc, projectId); err != nil {
			if errors.IsNotFound(err) {
				// The Service was deleted between the informer read and the
				// marker write. Requesting now would allocate an address that
				// no Service can release.
				logger.Infof("Service %s/%s disappeared before the FloatingIP request, not allocating", svc.Namespace, svc.Name)
				return nil
			}
			return fmt.Errorf("failed to record pending FloatingIP allocation for %s/%s: %w", svc.Namespace, svc.Name, err)
		}

		// Request a floating IP
		allocatedIPAddress, subnet, sharedKey, err = ipamClient.RequestFIP(ctx,
			string(secret.Data["clientSecret"]),
			string(secret.Data["cluster"]),
			string(secret.Data["project"]),
			fipPoolName,
			svc.Namespace,
			svc.Name,
			ipAddress,
			floatingIPGroup,
		)
		if err != nil {
			if strings.Contains(err.Error(), "quota exceeded") {
				logger.Infof("failed to request FIP: quota exceeded for project %s service %s/%s, skipping", string(secret.Data["project"]), svc.Namespace, svc.Name)
				r.recorder.Eventf(svc, v1.EventTypeWarning, "QuotaExceeded", "Failed to request FloatingIP for Service Load Balancer: quota exceeded for project %s", string(secret.Data["project"]))
				r.clearAllocationPending(ctx, svc)
				r.requeueAfter(svc, deferredRequeueDelay)
				return nil
			}
			if strings.Contains(err.Error(), "denied the request") {
				logger.Errorf("%s", err)
				r.recorder.Eventf(svc, v1.EventTypeWarning, "RequestDenied", "Failed to request FloatingIP for Service Load Balancer: request denied for project %s", string(secret.Data["project"]))
				r.clearAllocationPending(ctx, svc)
				r.requeueAfter(svc, deferredRequeueDelay)
				return nil
			}
			if ipAddress != "" && strings.Contains(err.Error(), "IP address is already assigned") {
				// A pinned address is only rejected as "already assigned" when
				// another Service owns it. The manager accepts a re-claim by
				// the Service that already holds the address, so this is either
				// a genuine conflict or a manager without that idempotent path.
				// Ask the IPAM who owns it: adopting is safe when the
				// allocation is this Service's own (a retry after the
				// annotation write failed); a conflict with another Service
				// cannot succeed by retrying, so stop instead of looping
				// forever.
				fips, listErr := ipamClient.ListFIPs(ctx,
					string(secret.Data["clientSecret"]),
					string(secret.Data["cluster"]),
					string(secret.Data["project"]),
					fipPoolName,
				)
				if listErr != nil {
					return fmt.Errorf("failed to request FIP: %w (listing floating IPs: %v)", err, listErr)
				}
				if ownsFloatingIP(fips, string(secret.Data["cluster"]), svc.Namespace, svc.Name, ipAddress) {
					logger.Infof("Static IP %s is already allocated to this Service, adopting the existing allocation", ipAddress)
					allocatedIPAddress = ipAddress
					adoptingExisting = true
				} else {
					logger.Errorf("Static IP %s is already assigned to another Service: %v", ipAddress, err)
					r.recorder.Eventf(svc, v1.EventTypeWarning, "StaticIPConflict", "Static IP %s is already assigned to another Service", ipAddress)
					r.clearAllocationPending(ctx, svc)
					r.requeueAfter(svc, deferredRequeueDelay)
					return nil
				}
			} else {
				return fmt.Errorf("failed to request FIP: %w", err)
			}
		} else {
			logger.Infof("Successfully requested FIP %s", allocatedIPAddress)
		}
	}

	// Check if the allocatedIPAddress is an IPv4 or IPv6 address so we can set the poolName
	ip := net.ParseIP(allocatedIPAddress)
	if ip == nil {
		return fmt.Errorf("failed to parse allocated IP address: %s", allocatedIPAddress)
	}
	poolName = poolNameForAddress(allocatedIPAddress)

	if string(secret.Data["loadBalancerType"]) == "metallb" {
		// Check if floatingIPGroup is not empty and if the IPAddressPool already exists
		poolAlreadyExists := false
		if floatingIPGroup != "" {
			existingPool, err := r.metallbClient.GetIPAddressPool(ctx, poolName, r.appNamespace)
			if err == nil && existingPool != nil {
				// Pool exists, update it by adding the namespace if not already present
				logger.Infof("IPAddressPool %s already exists, updating with namespace %s", poolName, svc.Namespace)
				namespaceExists := false
				for _, ns := range existingPool.Spec.AllocateTo.Namespaces {
					if ns == svc.Namespace {
						namespaceExists = true
						break
					}
				}
				if !namespaceExists {
					existingPool.Spec.AllocateTo.Namespaces = append(existingPool.Spec.AllocateTo.Namespaces, svc.Namespace)
					if err := r.metallbClient.UpdateIPAddressPool(ctx, existingPool); err != nil {
						return fmt.Errorf("failed to update IPAddressPool: %w", err)
					}
					logger.Infof("Successfully updated IPAddressPool %s with namespace %s", poolName, svc.Namespace)
				}
				// Skip the rest of the MetalLB resource creation since we're just updating the pool
				poolAlreadyExists = true
			} else if err != nil && !errors.IsNotFound(err) {
				// If error is not "not found", log it but continue to create the pool
				logger.Warnf("Failed to get IPAddressPool %s: %v, proceeding to create it", poolName, err)
			}
		}

		// Create MetalLB resources only if pool doesn't already exist
		var ipAddressPool *v1beta1.IPAddressPool
		if !poolAlreadyExists {
			// Determine the match labels
			matchLabels := make(map[string]string)
			if floatingIPGroup != "" {
				matchLabels["rancher.k8s.binbash.org/servicegroup"] = floatingIPGroup
			} else {
				matchLabels["rancher.k8s.binbash.org/service"] = svc.Name
				matchLabels["rancher.k8s.binbash.org/servicenamespace"] = svc.Namespace
			}
			ipAddressPool = &v1beta1.IPAddressPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: r.appNamespace,
				},
				Spec: v1beta1.IPAddressPoolSpec{
					Addresses: []string{
						addressCIDR(allocatedIPAddress),
					},
					AllocateTo: &v1beta1.ServiceAllocation{
						Namespaces: []string{svc.Namespace},
						ServiceSelectors: []metav1.LabelSelector{
							{
								MatchLabels: matchLabels,
							},
						},
					},
				},
			}

			if err := r.metallbClient.CreateIPAddressPool(ctx, ipAddressPool); err != nil && !errors.IsAlreadyExists(err) {
				// if an IP is already a part of an existing IPAddressPool, it was not properly released, so we need to clean things up and try again
				if strings.Contains(err.Error(), "overlaps with already defined CIDR") {
					logger.Warnf("IP %s is already a part of an existing MetalLBIPAddressPool, trying to clean things up and try again", allocatedIPAddress)
					// get all IPAddressPools, loop through them and identify the CIDR that overlaps
					ipAddressPools, err := r.metallbClient.GetIPAddressPools(ctx, r.appNamespace)
					if err != nil {
						return fmt.Errorf("failed to get IPAddressPools: %w", err)
					}
					for _, p := range ipAddressPools {
						if poolContainsAddress(p.Spec.Addresses, ip) {
							// The overlapping pool may belong to another live
							// Service of the same floatingIPGroup. Deleting it
							// would break that Service's load balancer.
							inUse, listErr := r.addressInUse(ctx, svc, allocatedIPAddress)
							if listErr != nil {
								return fmt.Errorf("failed to create IPAddressPool: %w (listing services to check for shared use: %v)", err, listErr)
							}
							if inUse {
								logger.Warnf("IP %s is still used by another Service, keeping MetalLB IPAddressPool %s/%s", allocatedIPAddress, p.Namespace, p.Name)
								r.recorder.Eventf(svc, v1.EventTypeWarning, "AddressInUse", "FloatingIP %s is still used by another Service, keeping its MetalLB IPAddressPool", allocatedIPAddress)
								return fmt.Errorf("failed to create IPAddressPool: %w", err)
							}

							logger.Infof("CIDR %s found in MetalLB IPAddressPool %s/%s, removing the old L2Advertisement and IPAddressPool", addressCIDR(allocatedIPAddress), p.Namespace, p.Name)
							var releaseFIP bool = false
							if err := r.metallbClient.DeleteL2Advertisement(ctx, p.Name, r.appNamespace); err != nil {
								logger.Errorf("failed to delete L2Advertisement: %s", err)
								releaseFIP = true
							}
							if err := r.metallbClient.DeleteIPAddressPool(ctx, p.Name, r.appNamespace); err != nil {
								logger.Errorf("failed to delete IPAddressPool: %s", err)
								releaseFIP = true
							}
							if releaseFIP {
								// An adopted allocation belongs to the Service
								// already; only an address requested by this
								// reconcile may be released.
								if !adoptingExisting {
									errRelease := ipamClient.ReleaseFIP(ctx,
										string(secret.Data["clientSecret"]),
										string(secret.Data["cluster"]),
										string(secret.Data["project"]),
										fipPoolName,
										svc.Namespace,
										svc.Name,
										allocatedIPAddress,
										floatingIPGroup,
									)
									if errRelease != nil {
										return fmt.Errorf("failed to release FIP during CreateIPAddressPool cleanup: %v, original error: %w", errRelease, err)
									}
								}
								return fmt.Errorf("failed to create IPAddressPool: %w", err)
							}
							logger.Infof("Successfully deleted the old MetalLB L2Advertisement and IPAddressPool %s/%s, trying to create a new one", r.appNamespace, ipAddressPool.Name)

							// Retry the creation of the IPAddressPool
							if err := r.metallbClient.CreateIPAddressPool(ctx, ipAddressPool); err != nil && !errors.IsAlreadyExists(err) {
								return fmt.Errorf("failed to create IPAddressPool: %w", err)
							} else if err == nil {
								createdPool = true
							}
							logger.Infof("Successfully created IPAddressPool %s/%s", r.appNamespace, poolName)
							break
						}
					}
				} else {
					logger.Errorf("Failed to create MetalLB IPAddressPool: %s, releasing the FloatingIP", err)
					// An adopted allocation belongs to the Service already; only
					// an address requested by this reconcile may be released.
					if !adoptingExisting {
						errRelease := ipamClient.ReleaseFIP(ctx,
							string(secret.Data["clientSecret"]),
							string(secret.Data["cluster"]),
							string(secret.Data["project"]),
							fipPoolName,
							svc.Namespace,
							svc.Name,
							allocatedIPAddress,
							floatingIPGroup,
						)
						if errRelease != nil {
							return fmt.Errorf("failed to release FIP during CreateIPAddressPool cleanup: %v, original error: %w", errRelease, err)
						}
					}
					return fmt.Errorf("failed to create IPAddressPool: %w", err)
				}
			} else if err == nil {
				createdPool = true
			}

			l2Advertisement := &v1beta1.L2Advertisement{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: r.appNamespace,
				},
				Spec: v1beta1.L2AdvertisementSpec{
					IPAddressPools: []string{poolName},
					Interfaces:     []string{networkInterface},
				},
			}
			if err := r.metallbClient.CreateL2Advertisement(ctx, l2Advertisement); err != nil && !errors.IsAlreadyExists(err) {
				logger.Errorf("Failed to create MetalLB L2Advertisement: %s, cleaning up and releasing the FloatingIP", err)
				// Never delete the pool while another Service of the group
				// still uses the address.
				inUse, listErr := r.addressInUse(ctx, svc, allocatedIPAddress)
				if listErr != nil {
					return fmt.Errorf("failed to create L2Advertisement: %w (listing services to check for shared use: %v)", err, listErr)
				}
				if inUse {
					logger.Warnf("IP %s is still used by another Service, keeping MetalLB IPAddressPool %s/%s", allocatedIPAddress, r.appNamespace, poolName)
					r.recorder.Eventf(svc, v1.EventTypeWarning, "AddressInUse", "FloatingIP %s is still used by another Service, keeping its MetalLB IPAddressPool", allocatedIPAddress)
					return fmt.Errorf("failed to create L2Advertisement: %w", err)
				}
				errDelete := r.metallbClient.DeleteIPAddressPool(ctx, poolName, r.appNamespace)
				if errDelete != nil {
					return fmt.Errorf("failed to delete IPAddressPool during CreateL2Advertisement cleanup: %v, original error: %w", errDelete, err)
				}
				// An adopted allocation belongs to the Service already; only an
				// address requested by this reconcile may be released.
				if !adoptingExisting {
					errRelease := ipamClient.ReleaseFIP(ctx,
						string(secret.Data["clientSecret"]),
						string(secret.Data["cluster"]),
						string(secret.Data["project"]),
						fipPoolName,
						svc.Namespace,
						svc.Name,
						allocatedIPAddress,
						floatingIPGroup,
					)
					if errRelease != nil {
						return fmt.Errorf("failed to release FIP during CreateL2Advertisement cleanup: %v, original error: %w", errRelease, err)
					}
				}
				return fmt.Errorf("failed to create L2Advertisement: %w", err)
			}
		}
	} else if string(secret.Data["loadBalancerType"]) == "purelb" {
		// Create PureLB resources
		// IPv6 addresses belong in V6Pools; putting them in V4Pools made
		// PureLB ignore or reject the pool, and a /32 on an IPv6 address
		// covers 2^96 addresses instead of the single allocation.
		addressPool := purelb.AddressPool{
			Aggregation: "default",
			Pool:        addressCIDR(allocatedIPAddress),
			Subnet:      subnet,
		}
		serviceGroupLocal := &purelb.ServiceGroupLocal{}
		if ip.To4() != nil {
			serviceGroupLocal.V4Pools = []purelb.AddressPool{addressPool}
		} else {
			serviceGroupLocal.V6Pools = []purelb.AddressPool{addressPool}
		}
		serviceGroup := &purelb.ServiceGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      poolName,
				Namespace: r.appNamespace,
			},
			Spec: purelb.ServiceGroupSpec{
				Local: serviceGroupLocal,
			},
		}

		// Check if the floatingIPGroup is set and if the PureLB service group already exists
		// If so, skip creating a new ServiceGroup
		skipServiceGroupCreation := false
		if floatingIPGroup != "" || (adoptingExisting && subnet == "") {
			serviceGroups, err := r.purelbClient.GetServiceGroups(ctx, r.appNamespace)
			if err != nil {
				return fmt.Errorf("failed to get ServiceGroups: %w", err)
			}
			for _, sg := range serviceGroups {
				if sg.Name == poolName {
					if floatingIPGroup != "" {
						logger.Infof("Found existing PureLB ServiceGroup %s/%s for floatingIPGroup %s, skipping creation", r.appNamespace, poolName, floatingIPGroup)
					} else {
						logger.Infof("Found existing PureLB ServiceGroup %s/%s, skipping creation", r.appNamespace, poolName)
					}
					skipServiceGroupCreation = true
					break
				}
			}
		}

		// An adopted allocation from an older controller version has no
		// persisted subnet. If its ServiceGroup is gone there is no valid
		// configuration to rebuild: creating one with an empty subnet would
		// break the Service in a new way. Report it instead; the address stays
		// allocated and the operator can restore the ServiceGroup.
		if !skipServiceGroupCreation && adoptingExisting && subnet == "" {
			logger.Warnf("PureLB ServiceGroup %s/%s is missing and the subnet of adopted FloatingIP %s is unknown, not recreating it", r.appNamespace, poolName, allocatedIPAddress)
			r.recorder.Eventf(svc, v1.EventTypeWarning, "MissingServiceGroupSubnet", "Cannot recreate PureLB ServiceGroup %s for FloatingIP %s: allocation subnet is unknown", poolName, allocatedIPAddress)
			skipServiceGroupCreation = true
		}

		if !skipServiceGroupCreation {
			// AlreadyExists means a previous reconcile created the ServiceGroup
			// for this address but did not finish annotating the Service (or
			// the annotation was removed). Adoption must not release the
			// address just because the resource is already there.
			if err := r.purelbClient.CreateServiceGroup(ctx, serviceGroup); err != nil && !errors.IsAlreadyExists(err) {
				logger.Errorf("Failed to create PureLB ServiceGroup: %s, releasing the FloatingIP", err)
				// A ServiceGroup of a floatingIPGroup is shared: releasing the
				// address while another member uses it breaks that member.
				inUse, listErr := r.addressInUse(ctx, svc, allocatedIPAddress)
				if listErr != nil {
					return fmt.Errorf("failed to create ServiceGroup: %w (listing services to check for shared use: %v)", err, listErr)
				}
				if inUse {
					logger.Warnf("IP %s is still used by another Service, keeping PureLB ServiceGroup %s/%s", allocatedIPAddress, r.appNamespace, poolName)
					r.recorder.Eventf(svc, v1.EventTypeWarning, "AddressInUse", "FloatingIP %s is still used by another Service, keeping its PureLB ServiceGroup", allocatedIPAddress)
					return fmt.Errorf("failed to create ServiceGroup: %w", err)
				}
				// An adopted allocation belongs to the Service already; only an
				// address requested by this reconcile may be released.
				if !adoptingExisting {
					errRelease := ipamClient.ReleaseFIP(ctx,
						string(secret.Data["clientSecret"]),
						string(secret.Data["cluster"]),
						string(secret.Data["project"]),
						fipPoolName,
						svc.Namespace,
						svc.Name,
						allocatedIPAddress,
						floatingIPGroup,
					)
					if errRelease != nil {
						return fmt.Errorf("failed to release FIP during CreateServiceGroup cleanup: %v, original error: %w", errRelease, err)
					}
				}
				return fmt.Errorf("failed to create ServiceGroup: %w", err)

			} else if err == nil {
				createdPool = true
				logger.Infof("Successfully created PureLB ServiceGroup %s/%s", r.appNamespace, poolName)
			}

			// Not needed for now, but keeping the code here for future reference
			// sendGARP := true
			// lbNodeAgent := &purelb.LBNodeAgent{
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name:      poolName,
			// 		Namespace: r.appNamespace,
			// 	},
			// 	Spec: purelb.LBNodeAgentSpec{
			// 		Local: &purelb.LBNodeAgentLocal{
			// 			LocalInt: networkInterface,
			// 			SendGARP: &sendGARP,
			// 		},
			// 	},
			// }
			// if err := r.purelbClient.CreateLBNodeAgent(ctx, lbNodeAgent); err != nil {
			// 	// Clean up the resources if the creation fails
			// 	return fmt.Errorf("failed to create PureLB LBNodeAgent: %w", err)
			// }

			// logger.Infof("Successfully created PureLB LBNodeAgent %s/%s", r.appNamespace, poolName)
		}
	}

	// Add finalizer, label and annotation to the service. Conflicts are
	// retried a bounded number of times in-process; the fixed 60s poll this
	// replaces could hold a worker (and every reconcile behind it) for a
	// minute.
	_, err = r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
		needsUpdate := false

		if currentSvc.Labels == nil {
			currentSvc.Labels = make(map[string]string)
		}
		if currentSvc.Labels["rancher.k8s.binbash.org/service"] != currentSvc.Name {
			currentSvc.Labels["rancher.k8s.binbash.org/service"] = currentSvc.Name
			needsUpdate = true
		}
		if currentSvc.Labels["rancher.k8s.binbash.org/servicenamespace"] != currentSvc.Namespace {
			currentSvc.Labels["rancher.k8s.binbash.org/servicenamespace"] = currentSvc.Namespace
			needsUpdate = true
		}
		if floatingIPGroup != "" && currentSvc.Labels["rancher.k8s.binbash.org/servicegroup"] != floatingIPGroup {
			currentSvc.Labels["rancher.k8s.binbash.org/servicegroup"] = floatingIPGroup
			needsUpdate = true
		}

		if currentSvc.Annotations == nil {
			currentSvc.Annotations = make(map[string]string)
		}
		if currentSvc.Annotations[floatingIPAnnotation] != allocatedIPAddress {
			currentSvc.Annotations[floatingIPAnnotation] = allocatedIPAddress
			needsUpdate = true
		}
		// The allocation is now persisted, so the pending marker has served
		// its purpose.
		if _, ok := currentSvc.Annotations[floatingIPRequestAnnotation]; ok {
			delete(currentSvc.Annotations, floatingIPRequestAnnotation)
			needsUpdate = true
		}
		// Persist the subnet returned by the IPAM so a restart can reconcile
		// (and, for PureLB, rebuild) the allocation without requesting a new
		// address.
		if subnet != "" && currentSvc.Annotations["rancher.k8s.binbash.org/floatingip-subnet"] != subnet {
			currentSvc.Annotations["rancher.k8s.binbash.org/floatingip-subnet"] = subnet
			needsUpdate = true
		}
		// Persist the project the address was allocated under so deletion
		// never re-derives it from namespace labels that may change or
		// disappear. The project is empty for an allocation
		// recovered from ingress alone: it may not be the project that
		// allocated the address, so the label-based fallback stays instead of
		// freezing a guess.
		if projectForAnnotation != "" && currentSvc.Annotations[projectAnnotation] != projectForAnnotation {
			currentSvc.Annotations[projectAnnotation] = projectForAnnotation
			needsUpdate = true
		}

		// The current object's group annotation decides whether the shared-IP
		// annotation still applies; it may have been edited since the
		// reconcile started.
		currentGroup := currentSvc.Annotations["rancher.k8s.binbash.org/floatingip-group"]
		if string(secret.Data["loadBalancerType"]) == "metallb" {
			// Add sharedKey annotation when the Service is part of a
			// floatingIPGroup and the IPAM returned a shared key.
			if currentGroup != "" && sharedKey != "" && currentSvc.Annotations["metallb.io/allow-shared-ip"] != sharedKey {
				currentSvc.Annotations["metallb.io/allow-shared-ip"] = sharedKey
				needsUpdate = true
			}
		} else if string(secret.Data["loadBalancerType"]) == "purelb" {
			if currentSvc.Annotations["purelb.io/service-group"] != poolName {
				currentSvc.Annotations["purelb.io/service-group"] = poolName
				needsUpdate = true
			}
			if currentGroup != "" && sharedKey != "" && currentSvc.Annotations["purelb.io/allow-shared-ip"] != sharedKey {
				currentSvc.Annotations["purelb.io/allow-shared-ip"] = sharedKey
				needsUpdate = true
			}
		}

		if !containsString(currentSvc.ObjectMeta.Finalizers, finalizerName) {
			currentSvc.ObjectMeta.Finalizers = append(currentSvc.ObjectMeta.Finalizers, finalizerName)
			needsUpdate = true
		}

		return needsUpdate
	})

	if errors.IsNotFound(err) {
		logger.Debugf("Service '%s/%s' in work queue no longer exists", svc.Namespace, svc.Name)
		if allocatedIPAddress != "" {
			// The Service disappeared after the address was requested or
			// adopted but before the annotation/finalizer write. The cached
			// object is the only record of the allocation, so release it here
			// instead of leaking the address and its pool.
			if cleanupErr := r.releaseVanishedAllocation(ctx, svc, projectId, secret, allocatedIPAddress, poolName, fipPoolName); cleanupErr != nil {
				// The Service is already gone from the API. Record the
				// allocation on the cached object so the queue's retry can
				// still release it after the informer drops the object.
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[floatingIPAnnotation] = allocatedIPAddress
				return cleanupErr
			}
		}
		return nil
	}
	if err != nil {
		// Undo only what this reconcile created, and only when no other
		// Service still uses the address. A pool of a floatingIPGroup belongs
		// to the whole group: deleting it (or releasing the shared address)
		// from one member's failure path breaks every other member.
		inUse, listErr := r.addressInUse(ctx, svc, allocatedIPAddress)
		if listErr != nil {
			return fmt.Errorf("failed to add finalizer, label and annotation to service %s/%s: %w (listing services to check for shared use: %v)", svc.Namespace, svc.Name, err, listErr)
		}
		if inUse {
			logger.Warnf("FloatingIP %s is still used by another Service, keeping its pool and allocation after failing to annotate %s/%s: %v", allocatedIPAddress, svc.Namespace, svc.Name, err)
			return fmt.Errorf("failed to add finalizer, label and annotation to service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		if createdPool {
			if string(secret.Data["loadBalancerType"]) == "metallb" {
				errDeleteAdv := r.metallbClient.DeleteL2Advertisement(ctx, poolName, r.appNamespace)
				if errDeleteAdv != nil {
					return fmt.Errorf("failed to delete L2Advertisement during finalizer update cleanup: %v, original error: %w", errDeleteAdv, err)
				}
				errDeletePool := r.metallbClient.DeleteIPAddressPool(ctx, poolName, r.appNamespace)
				if errDeletePool != nil {
					return fmt.Errorf("failed to delete IPAddressPool during finalizer update cleanup: %v, original error: %w", errDeletePool, err)
				}
			} else if string(secret.Data["loadBalancerType"]) == "purelb" {
				errDeleteServiceGroup := r.purelbClient.DeleteServiceGroup(ctx, poolName, r.appNamespace)
				if errDeleteServiceGroup != nil {
					return fmt.Errorf("failed to delete ServiceGroup during finalizer update cleanup: %v, original error: %w", errDeleteServiceGroup, err)
				}
				// Not needed for now, but keeping the code here for future reference
				// errDeleteLBNodeAgent := r.purelbClient.DeleteLBNodeAgent(ctx, poolName, r.appNamespace)
				// if errDeleteLBNodeAgent != nil {
				// 	return fmt.Errorf("failed to delete LBNodeAgent during finalizer update cleanup: %v, original error: %w", errDeleteLBNodeAgent, err)
				// }
			}
		}
		// An adopted allocation stays allocated even when the annotation poll
		// fails: the Service still owns the address, and releasing it would
		// orphan a pool that is (or will be) reconciled from the annotation.
		if adoptingExisting {
			return fmt.Errorf("failed to add finalizer, label and annotation to service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
		errRelease := ipamClient.ReleaseFIP(ctx,
			string(secret.Data["clientSecret"]),
			string(secret.Data["cluster"]),
			string(secret.Data["project"]),
			fipPoolName,
			svc.Namespace,
			svc.Name,
			allocatedIPAddress,
			floatingIPGroup,
		)
		if errRelease != nil {
			return fmt.Errorf("failed to release FIP during finalizer update cleanup: %v, original error: %w", errRelease, err)
		}
		return fmt.Errorf("failed to add finalizer, label and annotation to service %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	return nil
}

// requeueAfter schedules another reconcile without reporting an error: the
// decision was deferred, not failed. A nil queue is allowed for tests that
// call reconcile directly.
func (r *reconciler) requeueAfter(svc *v1.Service, delay time.Duration) {
	if r.queue == nil {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return
	}
	r.queue.AddAfter(key, delay)
}

// markAllocationPending records that an allocation is about to be requested.
// It runs before RequestFIP: a crash between the request and the annotation
// write leaves the marker, and the next reconcile (including on restart) lists
// the IPAM for this Service's allocation instead of requesting a second one.
// The value is the project, so deletion can still find the
// allocation when the namespace labels changed in between.
func (r *reconciler) markAllocationPending(ctx context.Context, svc *v1.Service, projectId string) error {
	_, err := r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
		if currentSvc.Annotations[floatingIPRequestAnnotation] == projectId {
			return false
		}
		if currentSvc.Annotations == nil {
			currentSvc.Annotations = make(map[string]string)
		}
		currentSvc.Annotations[floatingIPRequestAnnotation] = projectId
		return true
	})
	// A NotFound error means the Service disappeared before the marker could
	// be persisted. The caller must abort instead of requesting an address for
	// an object that no longer exists, because no Service would ever carry the
	// annotation that releases it.
	return err
}

// recordCleanupProgress persists the stage a deletion reached. A missing
// Service means its finalizer was removed externally: the work still continues
// (release is idempotent), there is just nothing left to resume.
func (r *reconciler) recordCleanupProgress(ctx context.Context, svc *v1.Service, stage string) error {
	_, err := r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
		if currentSvc.Annotations[cleanupProgressAnnotation] == stage {
			return false
		}
		if currentSvc.Annotations == nil {
			currentSvc.Annotations = make(map[string]string)
		}
		currentSvc.Annotations[cleanupProgressAnnotation] = stage
		return true
	})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to record cleanup progress %q on service %s/%s: %w", stage, svc.Namespace, svc.Name, err)
	}
	return nil
}

// cleanupStageReached reports whether progress has reached stage. Stages are
// ordered: released implies resources-deleted.
func cleanupStageReached(progress, stage string) bool {
	if progress == cleanupStageReleased {
		return true
	}
	return stage == cleanupStageResourcesDeleted && progress == cleanupStageResourcesDeleted
}

// hasFloatingIPRecord reports whether the Service carries any state that a
// cleanup must process. It is used to recognize a Service converted away from
// LoadBalancer and a stale queue entry for a non-LoadBalancer Service.
func hasFloatingIPRecord(svc *v1.Service) bool {
	return containsString(svc.ObjectMeta.Finalizers, finalizerName) ||
		svc.Annotations[floatingIPAnnotation] != "" ||
		svc.Annotations[floatingIPRequestAnnotation] != "" ||
		svc.Annotations[cleanupProgressAnnotation] != ""
}

// releaseVanishedAllocation releases the address of a Service that disappeared
// after RequestFIP (or after adopting an allocation) but before the
// annotation/finalizer write. The cached object is the only record of the
// allocation, so without this cleanup the address and its pool stay allocated
// forever. A peer sharing the address keeps the pool, but
// this Service's own claim is still released.
func (r *reconciler) releaseVanishedAllocation(ctx context.Context, svc *v1.Service, projectId string, secret *v1.Secret, ipAddress, poolName, fipPoolName string) error {
	// Serialize with a peer's deletion: both decide whether the pool is still
	// in use, and a concurrent decision could delete it under the survivor.
	unlockAddress := r.lockAddress(ipAddress)
	defer unlockAddress()

	shared, err := r.addressInUse(ctx, svc, ipAddress)
	if err != nil {
		return err
	}
	if !shared {
		shared, err = r.sharedPoolInUse(ctx, svc, projectId, secret, ipAddress, poolName, fipPoolName)
		if err != nil {
			return err
		}
	}

	if !shared {
		switch string(secret.Data["loadBalancerType"]) {
		case "metallb":
			if err := r.metallbClient.DeleteL2Advertisement(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("failed to delete L2Advertisement %s of vanished service %s/%s: %w", poolName, svc.Namespace, svc.Name, err)
			}
			if err := r.metallbClient.DeleteIPAddressPool(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("failed to delete IPAddressPool %s of vanished service %s/%s: %w", poolName, svc.Namespace, svc.Name, err)
			}
		case "purelb":
			if err := r.purelbClient.DeleteServiceGroup(ctx, poolName, r.appNamespace); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("failed to delete ServiceGroup %s of vanished service %s/%s: %w", poolName, svc.Namespace, svc.Name, err)
			}
		}
	}

	ipamClient, err := r.ipamClientFor(projectId, secret)
	if err != nil {
		return fmt.Errorf("failed to create IPAM client while cleaning up vanished service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	if err := ipamClient.ReleaseFIP(ctx,
		string(secret.Data["clientSecret"]),
		string(secret.Data["cluster"]),
		string(secret.Data["project"]),
		fipPoolName,
		svc.Namespace,
		svc.Name,
		ipAddress,
		"",
	); err != nil {
		return fmt.Errorf("failed to release FloatingIP %s of vanished service %s/%s: %w", ipAddress, svc.Namespace, svc.Name, err)
	}
	return nil
}

// clearAllocationPending removes the pending marker after the IPAM refused the
// request outright. A failed request that might still have allocated (timeout,
// connection reset) keeps the marker so the retry can adopt the allocation.
func (r *reconciler) clearAllocationPending(ctx context.Context, svc *v1.Service) {
	_, err := r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
		if _, ok := currentSvc.Annotations[floatingIPRequestAnnotation]; !ok {
			return false
		}
		delete(currentSvc.Annotations, floatingIPRequestAnnotation)
		return true
	})
	if err != nil && !errors.IsNotFound(err) {
		logrus.Warnf("Failed to clear the pending FloatingIP marker on service %s/%s: %v", svc.Namespace, svc.Name, err)
	}
}

// validateProjectConfig checks the Secret values an allocation needs before
// the IPAM is asked for an address. RequestFIP used to run with an unknown
// loadBalancerType or an incomplete Secret: the address was allocated and the
// Service annotated and finalized, but no pool was created, so the
// LoadBalancer stayed Pending while the address was silently consumed.
func validateProjectConfig(secret *v1.Secret, fipPoolName string) error {
	lbType := string(secret.Data["loadBalancerType"])
	if lbType != "metallb" && lbType != "purelb" {
		return fmt.Errorf("Secret %s has unsupported loadBalancerType %q (must be \"metallb\" or \"purelb\")", secret.Name, lbType)
	}
	for _, key := range []string{"apiUrl", "clientSecret", "cluster", "project"} {
		if len(secret.Data[key]) == 0 {
			return fmt.Errorf("Secret %s is missing required key %q", secret.Name, key)
		}
	}
	// fipPoolName is the resolved pool: the Service's static-network
	// annotation when set, otherwise the Secret's floatingIPPool.
	if fipPoolName == "" {
		return fmt.Errorf("Secret %s has no floatingIPPool and the Service requests no static-network", secret.Name)
	}
	return nil
}

// poolNameForAddress returns the MetalLB/PureLB resource name for an address.
// IPv4 uses the dotted form. IPv6 keeps the historical dot form when it is a
// valid DNS name (full-form addresses), and otherwise sanitizes the canonical
// address: a compressed address such as 2001:db8::1 used to produce
// "ip6-2001.db8..1", which Kubernetes rejects as an object name.
func poolNameForAddress(ipAddress string) string {
	ip := net.ParseIP(ipAddress)
	if ip != nil && ip.To4() == nil {
		legacy := "ip6-" + strings.ReplaceAll(ipAddress, ":", ".")
		if err := validation.IsDNS1123Subdomain(legacy); err == nil {
			return legacy
		}
		sanitized := strings.Trim(strings.ReplaceAll(ip.String(), ":", "-"), "-")
		if sanitized == "" {
			sanitized = "0"
		}
		return "ip6-" + sanitized
	}
	return "ip4-" + ipAddress
}

// addressCIDR returns the CIDR that covers exactly one address: /32 for IPv4
// and /128 for IPv6. A /32 on an IPv6 address covers 2^96 addresses, so
// MetalLB may allocate an address other than the one the IPAM assigned, and a
// second IPv6 allocation in the same pool fails as an overlap.
func addressCIDR(ipAddress string) string {
	if ip := net.ParseIP(ipAddress); ip != nil && ip.To4() == nil {
		return ipAddress + "/128"
	}
	return ipAddress + "/32"
}

// poolContainsAddress reports whether a MetalLB IPAddressPool entry covers ip.
// Entries are usually CIDRs and are compared by family, so an IPv4 entry can
// never match an IPv6 allocation; a bare address is accepted as well. Parsing
// the entry also recognizes pools created by older controller versions, whose
// IPv6 entries were written as /32.
func poolContainsAddress(addresses []string, ip net.IP) bool {
	for _, address := range addresses {
		if _, cidr, err := net.ParseCIDR(address); err == nil {
			if cidr.Contains(ip) {
				return true
			}
			continue
		}
		if single := net.ParseIP(address); single != nil && single.Equal(ip) {
			return true
		}
	}
	return false
}

// deletionProjectID returns the project a Service's FloatingIP was allocated
// under. The annotation written at allocation time takes precedence over the
// namespace labels: labels are mutable, and changing them would select a
// different Secret and IPAM identity for the release. It returns
// "" when no project can be determined.
func (r *reconciler) deletionProjectID(ctx context.Context, svc *v1.Service) (string, error) {
	if projectId := svc.Annotations[projectAnnotation]; projectId != "" {
		return projectId, nil
	}
	// A pending request records the project before the address is known.
	if projectId := svc.Annotations[floatingIPRequestAnnotation]; projectId != "" {
		return projectId, nil
	}

	projectId, err := r.getProjectIDFromAppNamespace(ctx)
	if err != nil {
		return "", err
	}
	if projectId != "" {
		return projectId, nil
	}

	ns, err := r.clientset.CoreV1().Namespaces().Get(ctx, svc.Namespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get namespace %s: %w", svc.Namespace, err)
	}
	return ns.Labels["field.cattle.io/projectId"], nil
}

// abandonDeletion removes the finalizer without releasing the FloatingIP. It is
// the terminal decision for a deletion that can never succeed: without the
// project or its Secret there is no IPAM identity to release with, and retrying
// would keep the Service and its namespace in Terminating forever.
// The orphaned address is reported so it can be released manually.
func (r *reconciler) abandonDeletion(ctx context.Context, logger *logrus.Entry, svc *v1.Service, ipAddress, projectId, reason string) error {
	if ipAddress != "" {
		logger.Warnf("Removing finalizer without releasing FloatingIP %s of project %q: %s; release the address manually", ipAddress, projectId, reason)
		r.recorder.Eventf(svc, v1.EventTypeWarning, "OrphanedFloatingIP",
			"Removing finalizer without releasing FloatingIP %s of project %q: %s; the address must be released manually", ipAddress, projectId, reason)
	} else {
		logger.Infof("Removing finalizer without releasing a FloatingIP: %s", reason)
	}
	return r.removeFinalizer(ctx, logger, svc)
}

// removeFinalizer removes the controller's finalizer from the Service, retrying
// update conflicts a bounded number of times. The Service disappearing counts
// as success.
func (r *reconciler) removeFinalizer(ctx context.Context, logger *logrus.Entry, svc *v1.Service) error {
	_, err := r.updateService(ctx, svc.Namespace, svc.Name, func(currentSvc *v1.Service) bool {
		if !containsString(currentSvc.ObjectMeta.Finalizers, finalizerName) {
			return false
		}
		currentSvc.ObjectMeta.Finalizers = removeString(currentSvc.ObjectMeta.Finalizers, finalizerName)
		return true
	})
	if err != nil && !errors.IsNotFound(err) {
		logger.Warnf("Failed to remove finalizer from service %s/%s: %v", svc.Namespace, svc.Name, err)
		return fmt.Errorf("failed to remove finalizer from service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}

// updateService applies mutate to the current Service and writes it back. It
// retries update conflicts only, with a bounded backoff: waiting for a fixed
// minute for an unrelated writer would hold a worker and delay every queued
// Service behind it. When the retries are exhausted the error
// returns to the work queue, which backs off without occupying a worker.
func (r *reconciler) updateService(ctx context.Context, namespace, name string, mutate func(*v1.Service) bool) (*v1.Service, error) {
	var result *v1.Service
	err := retry.RetryOnConflict(serviceUpdateBackoff, func() error {
		currentSvc, err := r.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		svcToUpdate := currentSvc.DeepCopy()
		if !mutate(svcToUpdate) {
			result = currentSvc
			return nil
		}

		updatedSvc, err := r.clientset.CoreV1().Services(namespace).Update(ctx, svcToUpdate, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		result = updatedSvc
		return nil
	})
	return result, err
}

// indexServiceByFloatingIP indexes a Service by the address recorded in its
// floatingip annotation.
func indexServiceByFloatingIP(obj interface{}) ([]string, error) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return nil, nil
	}
	if ipAddress := svc.Annotations[floatingIPAnnotation]; ipAddress != "" {
		return []string{ipAddress}, nil
	}
	return nil, nil
}

// reconcileAnnotations are the Service annotations whose changes require a
// reconcile: they select the address, the pool and the sharing group.
var reconcileAnnotations = []string{
	"rancher.k8s.binbash.org/floatingip",
	"rancher.k8s.binbash.org/floatingip-group",
	"rancher.k8s.binbash.org/static-ip",
	"rancher.k8s.binbash.org/static-network",
	projectAnnotation,
}

func (c *Controller) enqueue(svc *v1.Service) {
	key, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return
	}
	c.queue.Add(key)
}

func (c *Controller) addFunc(obj interface{}) {
	service, ok := obj.(*v1.Service)
	if !ok {
		return
	}

	if service.Spec.Type == v1.ServiceTypeLoadBalancer || hasFloatingIPRecord(service) {
		// A ClusterIP Service that still carries an allocation record (for
		// example, it was converted while the controller was down) must be
		// reconciled so its address is released.
		c.enqueue(service)
	}
}

func (c *Controller) updateFunc(oldObj, newObj interface{}) {
	oldSvc, ok := oldObj.(*v1.Service)
	if !ok {
		return
	}
	service, ok := newObj.(*v1.Service)
	if !ok {
		return
	}

	if serviceNeedsReconcile(oldSvc, service) {
		c.enqueue(service)
	}
}

// serviceNeedsReconcile reports whether a Service update must be reconciled.
// The previous handler only looked at DeletionTimestamp, so a ClusterIP ->
// LoadBalancer switch, an annotation edit, or a manually removed finalizer
// was ignored until a restart.
func serviceNeedsReconcile(oldSvc, newSvc *v1.Service) bool {
	if newSvc.DeletionTimestamp != nil {
		return true
	}
	if newSvc.Spec.Type != v1.ServiceTypeLoadBalancer {
		// A Service converted away from LoadBalancer must release its
		// allocation, and a resync must clean up a ClusterIP Service that
		// still carries a record from such a conversion.
		return oldSvc.Spec.Type == v1.ServiceTypeLoadBalancer || hasFloatingIPRecord(newSvc)
	}
	if oldSvc.Spec.Type != v1.ServiceTypeLoadBalancer {
		return true
	}
	if oldSvc.ResourceVersion == newSvc.ResourceVersion {
		// Periodic resync: the informer replays the cached object unchanged.
		return true
	}
	if containsString(oldSvc.Finalizers, finalizerName) != containsString(newSvc.Finalizers, finalizerName) {
		return true
	}
	for _, annotation := range reconcileAnnotations {
		if oldSvc.Annotations[annotation] != newSvc.Annotations[annotation] {
			return true
		}
	}
	return false
}

func (c *Controller) deleteFunc(obj interface{}) {
	service, ok := obj.(*v1.Service)
	if !ok {
		// The informer replaces an object it can no longer fetch with a
		// tombstone (a delete that happened while it was disconnected).
		// Dropping it left the allocation of a Service whose finalizer was
		// removed externally unreleased.
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		service, ok = tombstone.Obj.(*v1.Service)
		if !ok {
			return
		}
		c.rememberTombstone(service)
	}

	c.enqueue(service)
}

// rememberTombstone stores the last known state of a deleted Service when it
// still carries the finalizer or an allocation record. The key is processed
// after the object left the cache, so reconcile reads this snapshot instead.
func (c *Controller) rememberTombstone(svc *v1.Service) {
	if !containsString(svc.Finalizers, finalizerName) &&
		svc.Annotations[floatingIPAnnotation] == "" &&
		svc.Annotations[floatingIPRequestAnnotation] == "" {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return
	}
	c.tombstoneMu.Lock()
	c.tombstones[key] = svc.DeepCopy()
	c.tombstoneMu.Unlock()
}

// peekTombstone returns the snapshot stored for key without removing it.
func (c *Controller) peekTombstone(key string) *v1.Service {
	c.tombstoneMu.Lock()
	defer c.tombstoneMu.Unlock()
	return c.tombstones[key]
}

// takeTombstone returns and removes the snapshot stored for key.
func (c *Controller) takeTombstone(key string) *v1.Service {
	c.tombstoneMu.Lock()
	defer c.tombstoneMu.Unlock()
	svc := c.tombstones[key]
	delete(c.tombstones, key)
	return svc
}

// ownsFloatingIP reports whether the IPAM's list shows ipAddress allocated to
// the given Service. It distinguishes a retry of this Service's own allocation
// from a static-IP conflict with another Service. The group annotation is
// deliberately not matched: it is editable and not part of the Service's
// identity.
func ownsFloatingIP(fips *ipam.FIPListResponse, cluster, namespace, name, ipAddress string) bool {
	allocated, found := lookupServiceFloatingIP(fips, cluster, namespace, name)
	return found && allocated == ipAddress
}

// lookupServiceFloatingIP returns the address the IPAM lists for a Service
// without matching the floatingip-group annotation. The group is not part of
// the Service's identity and can be edited or removed after allocation, while
// the service labels in the IPAM remain the authoritative claim.
func lookupServiceFloatingIP(fips *ipam.FIPListResponse, cluster, namespace, name string) (string, bool) {
	if fips == nil {
		return "", false
	}
	for _, fip := range fips.FloatingIPs {
		if fip.Cluster == cluster &&
			fip.ServiceNamespace == namespace &&
			fip.ServiceName == name {
			return fip.IPAddress, true
		}
	}
	return "", false
}

// sharedPoolInUse reports whether the pool for ipAddress, or the IPAM's
// allocation record, still serves another Service. addressInUse only sees
// Services that carry the floatingip annotation; an operator can remove that
// annotation (or the floatingip-group annotation) while the Service is still
// configured with the shared address. The pool's allocation rules and the
// IPAM's service labels are the authoritative record then.
func (r *reconciler) sharedPoolInUse(ctx context.Context, svc *v1.Service, projectId string, secret *v1.Secret, ipAddress, poolName, fipPoolName string) (bool, error) {
	if string(secret.Data["loadBalancerType"]) == "metallb" {
		inUse, err := r.metalLBAddressInUse(ctx, svc, poolName)
		if err != nil || inUse {
			return inUse, err
		}
	}
	// PureLB ServiceGroups carry no per-Service allocation rules, so for them
	// (and as a final check for MetalLB) the IPAM labels decide.
	return r.ipamAddressInUse(ctx, svc, projectId, secret, ipAddress, fipPoolName)
}

// serviceNeedsFloatingIP reports whether a Service can use a load balancer
// address. A Service converted to ClusterIP keeps the servicegroup label (it is
// never removed) and may keep its floatingip annotation until something
// reconciles it; counting it would pin the shared pool forever, the inverse of
// the annotation-dependence this check removes.
func serviceNeedsFloatingIP(svc *v1.Service) bool {
	return svc.Spec.Type == v1.ServiceTypeLoadBalancer
}

// metalLBAddressInUse reports whether an existing MetalLB IPAddressPool still
// admits a Service other than the one being deleted. This catches a group
// member whose floatingip annotation is gone: the pool still selects it by
// label and namespace.
func (r *reconciler) metalLBAddressInUse(ctx context.Context, svc *v1.Service, poolName string) (bool, error) {
	if r.metallbClient == nil {
		return false, nil
	}

	pool, err := r.metallbClient.GetIPAddressPool(ctx, poolName, r.appNamespace)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get IPAddressPool %s/%s: %w", r.appNamespace, poolName, err)
	}
	if pool == nil {
		return false, nil
	}
	if pool.Spec.AllocateTo == nil {
		// A pool without allocation rules is usable by every Service. The
		// controller always writes AllocateTo, so this pool was not created
		// here; never delete something this controller cannot prove unused.
		return true, nil
	}
	if len(pool.Spec.AllocateTo.NamespaceSelectors) > 0 {
		// Same reasoning: the controller never writes namespace selectors.
		return true, nil
	}

	selectors := make([]labels.Selector, 0, len(pool.Spec.AllocateTo.ServiceSelectors))
	for i := range pool.Spec.AllocateTo.ServiceSelectors {
		selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.AllocateTo.ServiceSelectors[i])
		if err != nil {
			return false, fmt.Errorf("invalid ServiceSelector on IPAddressPool %s/%s: %w", r.appNamespace, poolName, err)
		}
		selectors = append(selectors, selector)
	}

	// The controller writes one ServiceSelector per pool, so the API server
	// can filter the list instead of fetching every Service in each namespace.
	// Multiple selectors are ORed below, which one
	// label-selector string cannot express, so those fall back to a full list.
	listOptions := metav1.ListOptions{}
	if len(selectors) == 1 {
		listOptions.LabelSelector = selectors[0].String()
	}

	for _, namespace := range pool.Spec.AllocateTo.Namespaces {
		services, err := r.clientset.CoreV1().Services(namespace).List(ctx, listOptions)
		if err != nil {
			return false, fmt.Errorf("failed to list services in namespace %s: %w", namespace, err)
		}
		for i := range services.Items {
			other := &services.Items[i]
			if other.Namespace == svc.Namespace && other.Name == svc.Name {
				continue
			}
			if !serviceNeedsFloatingIP(other) {
				continue
			}
			// A terminating peer has already made its own deletion decision
			// under the same address lock. Counting it here would make two
			// members deleted together each defer to the other, leaking the
			// pool once both are gone.
			if other.DeletionTimestamp != nil {
				continue
			}
			if len(selectors) == 0 {
				// No service selector: every Service in the namespace may
				// use the pool.
				return true, nil
			}
			for _, selector := range selectors {
				if selector.Matches(labels.Set(other.Labels)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// ipamAddressInUse reports whether the IPAM still lists ipAddress for a live
// Service other than the one being deleted. IPAM labels can outlive their
// Service (an older controller did not release per member), so a claim only
// counts while the Service it names still exists.
func (r *reconciler) ipamAddressInUse(ctx context.Context, svc *v1.Service, projectId string, secret *v1.Secret, ipAddress, fipPoolName string) (bool, error) {
	ipamClient, err := r.ipamClientFor(projectId, secret)
	if err != nil {
		return false, fmt.Errorf("failed to create IPAM client: %w", err)
	}
	fips, err := ipamClient.ListFIPs(ctx,
		string(secret.Data["clientSecret"]),
		string(secret.Data["cluster"]),
		string(secret.Data["project"]),
		fipPoolName,
	)
	if err != nil {
		return false, fmt.Errorf("failed to list floating IPs for %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	cluster := string(secret.Data["cluster"])
	for _, fip := range fips.FloatingIPs {
		if fip.Cluster != cluster || fip.IPAddress != ipAddress {
			continue
		}
		if fip.ServiceNamespace == svc.Namespace && fip.ServiceName == svc.Name {
			continue
		}
		other, err := r.clientset.CoreV1().Services(fip.ServiceNamespace).Get(ctx, fip.ServiceName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("failed to get service %s/%s listed for address %s: %w", fip.ServiceNamespace, fip.ServiceName, ipAddress, err)
		}
		if other.DeletionTimestamp != nil || !serviceNeedsFloatingIP(other) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// addressInUse reports whether another Service still references ipAddress
// through its floatingip annotation. A MetalLB IPAddressPool/L2Advertisement
// or PureLB ServiceGroup is shared by every Service of a floatingIPGroup, so
// deleting or releasing one is only safe when no other Service uses the
// address. The lookup uses the informer's floatingip index, so it does not
// list every Service in the cluster on each retry; only a reconciler built
// without an informer (unit tests) falls back to the API list.
func (r *reconciler) addressInUse(ctx context.Context, svc *v1.Service, ipAddress string) (bool, error) {
	if ipAddress == "" {
		return false, nil
	}

	if r.serviceIndexer != nil {
		objs, err := r.serviceIndexer.ByIndex(floatingIPIndex, ipAddress)
		if err != nil {
			return false, fmt.Errorf("failed to look up services using address %s: %w", ipAddress, err)
		}
		for _, obj := range objs {
			other, ok := obj.(*v1.Service)
			if !ok || (other.Namespace == svc.Namespace && other.Name == svc.Name) {
				continue
			}
			// Re-read through the API: the informer can still show an
			// annotation that a peer's reconcile just removed, which would
			// make the last claimant skip the release and leak the address.
			live, err := r.clientset.CoreV1().Services(other.Namespace).Get(ctx, other.Name, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return false, fmt.Errorf("failed to get service %s/%s while checking address %s: %w", other.Namespace, other.Name, ipAddress, err)
			}
			if live.Annotations[floatingIPAnnotation] == ipAddress && serviceNeedsFloatingIP(live) {
				return true, nil
			}
		}
		return false, nil
	}

	serviceList, err := r.clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list services: %w", err)
	}
	for i := range serviceList.Items {
		other := &serviceList.Items[i]
		if other.Namespace == svc.Namespace && other.Name == svc.Name {
			continue
		}
		if other.Annotations[floatingIPAnnotation] == ipAddress && serviceNeedsFloatingIP(other) {
			return true, nil
		}
	}
	return false, nil
}

// lockAddress serializes reconciles that decide the fate of one address. The
// returned function releases the lock.
func (r *reconciler) lockAddress(ipAddress string) func() {
	if ipAddress == "" {
		return func() {}
	}

	r.addressLocksMu.Lock()
	if r.addressLocks == nil {
		r.addressLocks = make(map[string]*sync.Mutex)
	}
	lock, ok := r.addressLocks[ipAddress]
	if !ok {
		lock = &sync.Mutex{}
		r.addressLocks[ipAddress] = lock
	}
	r.addressLocksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	for i, item := range slice {
		if item == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}
