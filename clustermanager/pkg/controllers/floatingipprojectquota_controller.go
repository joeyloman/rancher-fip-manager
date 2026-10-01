package controllers

import (
	"context"
	"fmt"
	"strings"

	"time"

	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/client-go/tools/clientcmd"
)

const (
	// floatingIPProjectQuotaCleanupFinalizer is owned by this controller. It
	// guarantees the config secrets created for a FloatingIPProjectQuota are
	// removed before the object disappears. It is deliberately distinct from
	// the rancher-fip-manager owned "floatingipprojectquota-cleanup"
	// finalizer, which cascade-deletes the attached FloatingIP objects.
	floatingIPProjectQuotaCleanupFinalizer = "rancher.k8s.binbash.org/clustermanager-secret-cleanup"
	// configSecretPrefix is the name prefix of every per-project config
	// secret created by HandleConfigSecrets.
	configSecretPrefix = "rancher-fip-config-"
	// orphanedSecretSweepInterval is how often the orphaned config secret
	// sweep runs.
	orphanedSecretSweepInterval = 1 * time.Hour
	// floatingIPProjectQuotaDeletionGraceWindow bounds how long the deletion
	// cleanup keeps retrying against a failing downstream cluster. After the
	// window expires the cleanup finalizer is removed anyway so the
	// FloatingIPProjectQuota (and the Rancher project deletion that cascaded
	// to it) can never be wedged by an unreachable cluster.
	floatingIPProjectQuotaDeletionGraceWindow = 10 * time.Minute
	// downstreamOperationTimeout bounds every single downstream API call made
	// by the deletion cleanup and the orphan sweep.
	downstreamOperationTimeout = 30 * time.Second
)

// FloatingIPProjectQuotaReconciler reconciles a FloatingIPProjectQuota object.
type FloatingIPProjectQuotaReconciler struct {
	client.Client
	// Scheme is the scheme for the controller.
	Scheme *runtime.Scheme
	// Config is the configuration for the controller.
	Config       *config.Config
	AppNamespace string
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
func (r *FloatingIPProjectQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logrus.WithFields(logrus.Fields{
		"controller": "floatingipprojectquota",
		"name":       req.NamespacedName,
	})

	// Fetch the FloatingIPProjectQuota object
	var floatingIPProjectQuota rbbv1beta2.FloatingIPProjectQuota
	if err := r.Get(ctx, req.NamespacedName, &floatingIPProjectQuota); err != nil {
		if apierrors.IsNotFound(err) {
			// The FloatingIPProjectQuota is gone and had no cleanup finalizer
			// left, so there is nothing to reconcile.
			return ctrl.Result{}, nil
		}
		log.WithError(err).Error("unable to fetch FloatingIPProjectQuota")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The FloatingIPProjectQuota is being deleted: clean up the config secrets
	// it caused to be created before allowing the object to disappear.
	if !floatingIPProjectQuota.DeletionTimestamp.IsZero() {
		return r.handleFloatingIPProjectQuotaDelete(ctx, &floatingIPProjectQuota, log)
	}

	// Ensure the cleanup finalizer is present so the config secrets are
	// cleaned up when the FloatingIPProjectQuota is deleted.
	if !controllerutil.ContainsFinalizer(&floatingIPProjectQuota, floatingIPProjectQuotaCleanupFinalizer) {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh rbbv1beta2.FloatingIPProjectQuota
			if err := r.Get(ctx, req.NamespacedName, &fresh); err != nil {
				return err
			}
			if controllerutil.ContainsFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer) {
				return nil
			}
			base := fresh.DeepCopy()
			controllerutil.AddFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer)
			return r.Patch(ctx, &fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}); err != nil {
			log.WithError(err).Error("unable to add cleanup finalizer to FloatingIPProjectQuota")
			return ctrl.Result{}, err
		}
		// Fall through: nothing downstream depends on the finalizer being
		// persisted first, and the next mutation below re-fetches anyway.
	}

	// Find the project namespace
	var projects managementv3.ProjectList
	if err := r.List(ctx, &projects); err != nil {
		log.WithError(err).Error("unable to list projects")
		return ctrl.Result{}, err
	}

	var projectNamespace string
	var project managementv3.Project
	for _, p := range projects.Items {
		if p.Name == floatingIPProjectQuota.Name {
			project = p
			projectNamespace = p.Namespace
			break
		}
	}

	if projectNamespace == "" {
		err := fmt.Errorf("project not found")
		log.WithError(err).Error("unable to find project")
		return ctrl.Result{}, nil // Do not requeue
	}

	log.Infof("Project %s found in namespace %s, processing...", project.Name, projectNamespace)

	// Get the downstream cluster
	var cluster managementv3.Cluster
	if err := r.Get(ctx, types.NamespacedName{Name: project.Spec.ClusterName}, &cluster); err != nil {
		log.WithError(err).Error("unable to fetch downstream cluster")
		return ctrl.Result{}, err
	}

	log.Infof("Downstream cluster found: %s [%s], processing...", cluster.Name, cluster.Spec.DisplayName)

	// Determine the load balancer type from the cluster label
	loadBalancerType := "purelb"
	if cluster.Labels["rancher-fip-lbtype"] == "metallb" {
		loadBalancerType = "metallb"
	}

	// If the downstream cluster is a Harvester cluster only create the secret in the local cluster
	if cluster.Labels["provider.cattle.io"] == "harvester" {
		return HandleConfigSecrets(
			ctx,
			r.Client,
			nil,
			r.Config.RancherFipLBControllerNamespace,
			&project,
			projectNamespace,
			"harvester",
			r.Config.RancherFipApiServerURL,
			cluster.Name,
			loadBalancerType,
			r.Config.CaCrt,
		)
	}

	// Check if Rancher FIP is enabled for the downstream cluster
	if cluster.Labels["rancher-fip"] != "enabled" {
		log.Infof("Rancher FIP is not enabled for cluster %s [%s], skipping...", cluster.Name, cluster.Spec.DisplayName)
		return ctrl.Result{}, nil
	}

	// Retrieve kubeconfig for the downstream cluster and build a client for it
	downstreamClient, err := r.getDownstreamClient(ctx, &cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("kubeconfig secret not found")
			return ctrl.Result{}, err
		}
		log.WithError(err).Error("unable to create downstream client")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Loop all floatingippool.rancher.k8s.binbash.org objects in the local cluster and check if the target cluster is the same as the downstream cluster
	var floatingIPPools rbbv1beta2.FloatingIPPoolList
	if err := r.List(ctx, &floatingIPPools); err != nil {
		log.WithError(err).Error("unable to list floatingippool objects")
		return ctrl.Result{}, err
	}

	var targetNetwork string
	for _, floatingIPPool := range floatingIPPools.Items {
		if floatingIPPool.Spec.TargetCluster == cluster.Name {
			log.Infof("FloatingIPPool %s found in the local cluster, processing...", floatingIPPool.Name)

			// Loop annotations and check if the rancher-fip-default-network exists and is set to "true"
			for key, value := range floatingIPPool.Annotations {
				if key == "rancher-fip-default-network" && value == "true" {
					targetNetwork = fmt.Sprintf("%s--%s", cluster.Spec.DisplayName, floatingIPPool.Spec.TargetNetwork)
					break
				}
			}
		}
		if floatingIPPool.Spec.TargetCluster == cluster.Spec.DisplayName {
			log.Infof("FloatingIPPool %s found in the local cluster, processing...", floatingIPPool.Name)

			// Loop annotations and check if the rancher-fip-default-network exists and is set to "true"
			for key, value := range floatingIPPool.Annotations {
				if key == "rancher-fip-default-network" && value == "true" {
					targetNetwork = fmt.Sprintf("%s--%s", cluster.Spec.DisplayName, floatingIPPool.Spec.TargetNetwork)
					break
				}
			}
		}

		// If the target network is found, break the loop
		if targetNetwork != "" {
			break
		}
	}

	if targetNetwork == "" {
		err := fmt.Errorf("rancher-fip-default-network annotation not found")
		log.WithError(err).Error("cannot determine target network")
		return ctrl.Result{}, err
	}

	if err := HandleNetworkConfigMap(ctx, r.Client, downstreamClient, cluster.Spec.DisplayName, r.Config.RancherFipLBControllerNamespace); err != nil {
		log.WithError(err).Error("failed to handle network configmap")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	return HandleConfigSecrets(
		ctx,
		r.Client,
		downstreamClient,
		r.Config.RancherFipLBControllerNamespace,
		&project,
		projectNamespace,
		targetNetwork,
		r.Config.RancherFipApiServerURL,
		cluster.Name,
		loadBalancerType,
		r.Config.CaCrt,
	)
}

// getDownstreamClient builds a controller-runtime client for the downstream
// cluster of the given management cluster, using the kubeconfig secret
// Rancher stores in the fleet-default namespace.
func (r *FloatingIPProjectQuotaReconciler) getDownstreamClient(ctx context.Context, cluster *managementv3.Cluster) (client.Client, error) {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace("fleet-default")); err != nil {
		return nil, fmt.Errorf("unable to list secrets in fleet-default namespace: %w", err)
	}

	var kubeconfigSecret corev1.Secret
	found := false
	kubeconfigSecretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	kubeconfigSecretNameWithDisplayName := fmt.Sprintf("%s-kubeconfig", cluster.Spec.DisplayName)

	for _, secret := range secrets.Items {
		if secret.Name == kubeconfigSecretName || secret.Name == kubeconfigSecretNameWithDisplayName {
			kubeconfigSecret = secret
			found = true
			break
		}
	}

	if !found {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), fmt.Sprintf("%s or %s", kubeconfigSecretName, kubeconfigSecretNameWithDisplayName))
	}

	downstreamKubeconfig, ok := kubeconfigSecret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s does not contain 'value' key", kubeconfigSecret.Name)
	}

	downstreamConfig, err := clientcmd.RESTConfigFromKubeConfig(downstreamKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create REST config from kubeconfig: %w", err)
	}

	return client.New(downstreamConfig, client.Options{Scheme: r.Scheme})
}

// handleFloatingIPProjectQuotaDelete removes the config secrets that were
// created for the FloatingIPProjectQuota, then removes the cleanup finalizer.
//
// A FloatingIPProjectQuota (i.e. a Rancher project) can be bound to a single
// baremetal cluster or shared by multiple downstream clusters running in
// Harvester Virtualization, so the cleanup does not rely on any stored
// cluster reference: it sweeps every cluster for the per-project config
// secret and deletes it wherever it exists.
//
// Failures against a downstream cluster only retry within
// floatingIPProjectQuotaDeletionGraceWindow. After that the finalizer is
// removed anyway: a powered-off downstream cluster must never wedge the
// FloatingIPProjectQuota deletion (and the Rancher project deletion that
// cascaded to it).
func (r *FloatingIPProjectQuotaReconciler) handleFloatingIPProjectQuotaDelete(ctx context.Context, floatingIPProjectQuota *rbbv1beta2.FloatingIPProjectQuota, log *logrus.Entry) (ctrl.Result, error) {
	log = log.WithField("deletion", true)
	log.Infof("FloatingIPProjectQuota %s is being deleted: cleaning up its config secrets in every cluster", floatingIPProjectQuota.Name)

	graceExpired := !floatingIPProjectQuota.DeletionTimestamp.IsZero() &&
		time.Since(floatingIPProjectQuota.DeletionTimestamp.Time) > floatingIPProjectQuotaDeletionGraceWindow

	// 1. Local cluster: the config secret lives in the namespace of the
	// cluster the project belongs to. Match it by name everywhere so the
	// cleanup works even when the project (and with it the cluster
	// association) is already gone.
	localCtx, cancel := context.WithTimeout(ctx, downstreamOperationTimeout)
	if err := r.deleteLocalConfigSecretsByName(localCtx, floatingIPProjectQuota.Name, log); err != nil {
		cancel()
		if !graceExpired {
			return ctrl.Result{}, err
		}
		log.WithError(err).Warn("grace window expired, continuing although the local config secret cleanup failed")
	} else {
		cancel()
	}

	// 2. Downstream clusters: delete the per-project config secret in every
	// cluster that has a kubeconfig secret. Clusters without one never got a
	// config secret and are skipped.
	var clusters managementv3.ClusterList
	if err := r.List(ctx, &clusters); err != nil {
		log.WithError(err).Error("unable to list clusters")
		return ctrl.Result{}, err
	}

	for _, cluster := range clusters.Items {
		if cluster.Name == "local" {
			continue
		}

		// An empty target namespace would turn this into a cluster-wide
		// secret scan on the downstream cluster; refuse instead.
		if r.Config.RancherFipLBControllerNamespace == "" {
			log.Warn("RancherFipLBControllerNamespace is not configured, skipping downstream config secret cleanup")
			continue
		}

		downstreamClient, err := r.getDownstreamClient(ctx, &cluster)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// No kubeconfig secret: this cluster never received a config
				// secret.
				continue
			}
			if !graceExpired {
				log.WithError(err).Errorf("unable to create downstream client for cluster %s, requeuing", cluster.Name)
				return ctrl.Result{}, err
			}
			log.WithError(err).Warnf("grace window expired, skipping downstream config secret cleanup for cluster %s", cluster.Name)
			continue
		}

		downstreamCtx, cancel := context.WithTimeout(ctx, downstreamOperationTimeout)
		downstreamSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configSecretPrefix + floatingIPProjectQuota.Name,
				Namespace: r.Config.RancherFipLBControllerNamespace,
			},
		}
		err = downstreamClient.Delete(downstreamCtx, downstreamSecret)
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			if !graceExpired {
				log.WithError(err).Errorf("unable to delete config secret in downstream cluster %s, requeuing", cluster.Name)
				return ctrl.Result{}, err
			}
			log.WithError(err).Warnf("grace window expired, config secret in downstream cluster %s may be left behind", cluster.Name)
			continue
		}
		if err == nil {
			log.Infof("Deleted secret %s/%s in downstream cluster %s", r.Config.RancherFipLBControllerNamespace, downstreamSecret.Name, cluster.Name)
		}
	}

	released, err := r.releaseCleanupFinalizer(ctx, floatingIPProjectQuota.Name, log)
	if err != nil {
		log.WithError(err).Error("unable to remove cleanup finalizer from FloatingIPProjectQuota")
		return ctrl.Result{}, err
	}
	if released {
		log.Infof("Config secrets for FloatingIPProjectQuota %s are cleaned up, removing finalizer", floatingIPProjectQuota.Name)
	}

	return ctrl.Result{}, nil
}

// releaseCleanupFinalizer removes the cleanup finalizer from the
// FloatingIPProjectQuota. It returns false when there was nothing to release:
// the object is already gone (a concurrent reconcile finished the cleanup) or
// the finalizer was already removed. Conflicts with concurrent status writes
// on the same object are retried transparently.
func (r *FloatingIPProjectQuotaReconciler) releaseCleanupFinalizer(ctx context.Context, name string, log *logrus.Entry) (bool, error) {
	released := false
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// The Get must be inside the closure so every attempt reads a fresh
		// resourceVersion.
		var fresh rbbv1beta2.FloatingIPProjectQuota
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &fresh); err != nil {
			if apierrors.IsNotFound(err) {
				log.Debugf("FloatingIPProjectQuota %s is already gone, nothing to release", name)
				return nil
			}
			return err
		}
		if !controllerutil.ContainsFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer) {
			return nil
		}
		base := fresh.DeepCopy()
		controllerutil.RemoveFinalizer(&fresh, floatingIPProjectQuotaCleanupFinalizer)
		if err := r.Patch(ctx, &fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsNotFound(err) {
				log.Debugf("FloatingIPProjectQuota %s was deleted while releasing the finalizer", name)
				return nil
			}
			return err
		}
		released = true
		return nil
	}); err != nil {
		return false, err
	}
	return released, nil
}

// deleteLocalConfigSecretsByName deletes every secret named
// rancher-fip-config-<projectID> in the local cluster, regardless of
// namespace, and returns the number of deleted secrets.
func (r *FloatingIPProjectQuotaReconciler) deleteLocalConfigSecretsByName(ctx context.Context, projectID string, log *logrus.Entry) error {
	secretName := configSecretPrefix + projectID

	var localSecrets corev1.SecretList
	if err := r.List(ctx, &localSecrets); err != nil {
		return fmt.Errorf("unable to list secrets in the local cluster: %w", err)
	}

	for i := range localSecrets.Items {
		s := &localSecrets.Items[i]
		if s.Name != secretName {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			log.WithError(err).Warnf("unable to delete secret %s/%s in the local cluster", s.Namespace, s.Name)
			continue
		}
		log.Infof("Deleted secret %s/%s in the local cluster", s.Namespace, s.Name)
	}

	return nil
}

// floatingIPProjectQuotaSweeper runs the orphaned config secret sweep on a
// fixed interval, independent of reconcile traffic, so orphans left behind by
// a crashed or replaced controller are cleaned up even when no
// FloatingIPProjectQuota events fire. The manager leader-gates plain
// runnables, so this only runs on the elected replica.
type floatingIPProjectQuotaSweeper struct {
	reconciler *FloatingIPProjectQuotaReconciler
}

// Start implements runnable.Runnable. It never returns an error for
// sweep-related failures — a returning Runnable makes the manager shut down.
func (s *floatingIPProjectQuotaSweeper) Start(ctx context.Context) error {
	log := logrus.WithField("controller", "floatingipprojectquota")
	// Run one sweep immediately, then every interval.
	if err := s.reconciler.sweepOrphanedConfigSecrets(ctx, log); err != nil {
		log.WithError(err).Warn("orphan sweep: pass failed")
	}

	ticker := time.NewTicker(orphanedSecretSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.reconciler.sweepOrphanedConfigSecrets(ctx, log); err != nil {
				log.WithError(err).Warn("orphan sweep: pass failed")
			}
		}
	}
}

// sweepOrphanedConfigSecrets deletes config secrets that reference a project
// without a FloatingIPProjectQuota. This catches secrets left behind when a
// FloatingIPProjectQuota was deleted while this controller was not running,
// and secrets created before the deletion cleanup existed. It fails closed:
// listing problems and empty lists abort the pass, never delete.
func (r *FloatingIPProjectQuotaReconciler) sweepOrphanedConfigSecrets(ctx context.Context, log *logrus.Entry) error {
	// Collect the projects that still have a FloatingIPProjectQuota.
	var floatingIPProjectQuotas rbbv1beta2.FloatingIPProjectQuotaList
	if err := r.List(ctx, &floatingIPProjectQuotas); err != nil {
		return fmt.Errorf("unable to list FloatingIPProjectQuota objects: %w", err)
	}
	activeProjects := make(map[string]bool, len(floatingIPProjectQuotas.Items))
	for _, fipq := range floatingIPProjectQuotas.Items {
		activeProjects[fipq.Name] = true
	}

	// Fail closed: an empty list is far more likely a cold cache, missing
	// RBAC on the CRD, or a scheme problem than a cluster where every
	// FloatingIPProjectQuota was deleted. Deleting every config secret in
	// that case would break FIP allocation for live projects.
	if len(activeProjects) == 0 {
		log.Warn("orphan sweep: no FloatingIPProjectQuota objects found, skipping sweep to avoid deleting config secrets of live projects")
		return nil
	}

	deleted := 0
	// Local cluster: a single cluster-wide pass, run before and independent of
	// the cluster list — a config secret can outlive its cluster object, so
	// the local sweep must also run when the cluster list is (transiently)
	// empty.
	n, err := r.deleteOrphanedLocalConfigSecrets(ctx, activeProjects, log)
	if err != nil {
		log.WithError(err).Warn("orphan sweep: local pass failed")
	} else {
		deleted += n
	}

	var clusters managementv3.ClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return fmt.Errorf("unable to list clusters: %w", err)
	}

	// Fail closed for the downstream pass only: without a cluster list there
	// is nothing to scan remotely. The local pass above already ran.
	if len(clusters.Items) == 0 {
		log.Warn("orphan sweep: no clusters found, skipping the downstream pass")
	}

	downstreamClusters := 0
	for _, cluster := range clusters.Items {
		// Skip the local management cluster itself and Harvester clusters:
		// Harvester clusters only get a config secret in the local cluster.
		if cluster.Name == "local" || cluster.Labels["provider.cattle.io"] == "harvester" {
			continue
		}
		downstreamClusters++

		// An empty target namespace would turn the downstream pass into a
		// cluster-wide secret scan; refuse instead.
		if r.Config.RancherFipLBControllerNamespace == "" {
			log.Warn("orphan sweep: RancherFipLBControllerNamespace is not configured, skipping downstream pass")
			continue
		}

		downstreamClient, err := r.getDownstreamClient(ctx, &cluster)
		if err != nil {
			log.WithError(err).Warnf("orphan sweep: unable to build downstream client for cluster %s, skipping", cluster.Name)
			continue
		}

		downstreamCtx, cancel := context.WithTimeout(ctx, downstreamOperationTimeout)
		var downstreamSecrets corev1.SecretList
		if err := downstreamClient.List(downstreamCtx, &downstreamSecrets, client.InNamespace(r.Config.RancherFipLBControllerNamespace)); err != nil {
			log.WithError(err).Warnf("orphan sweep: unable to list secrets in downstream cluster %s", cluster.Name)
			cancel()
			continue
		}
		for _, s := range downstreamSecrets.Items {
			projectID := strings.TrimPrefix(s.Name, configSecretPrefix)
			if projectID == s.Name || projectID == "" || activeProjects[projectID] {
				continue
			}
			downstreamSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace}}
			if err := downstreamClient.Delete(downstreamCtx, downstreamSecret); err != nil && !apierrors.IsNotFound(err) {
				log.WithError(err).Warnf("orphan sweep: unable to delete orphaned secret %s/%s in downstream cluster %s", s.Namespace, s.Name, cluster.Name)
				continue
			}
			log.Infof("orphan sweep: deleted orphaned secret %s/%s in downstream cluster %s (no FloatingIPProjectQuota for project %s)", s.Namespace, s.Name, cluster.Name, projectID)
			deleted++
		}
		cancel()
	}

	log.Infof("orphan sweep: swept the local cluster and %d downstream cluster(s) against %d FloatingIPProjectQuota(s), deleted %d orphaned config secret(s)", downstreamClusters, len(activeProjects), deleted)

	return nil
}

// deleteOrphanedLocalConfigSecrets deletes every config secret in the local
// cluster whose project has no FloatingIPProjectQuota, regardless of
// namespace — the project's cluster object may already be gone — and returns
// the number of deleted secrets.
func (r *FloatingIPProjectQuotaReconciler) deleteOrphanedLocalConfigSecrets(ctx context.Context, activeProjects map[string]bool, log *logrus.Entry) (int, error) {
	var localSecrets corev1.SecretList
	if err := r.List(ctx, &localSecrets); err != nil {
		return 0, fmt.Errorf("unable to list secrets in the local cluster: %w", err)
	}
	deleted := 0
	for i := range localSecrets.Items {
		s := &localSecrets.Items[i]
		projectID := strings.TrimPrefix(s.Name, configSecretPrefix)
		if projectID == s.Name || projectID == "" || activeProjects[projectID] {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			log.WithError(err).Warnf("orphan sweep: unable to delete orphaned secret %s/%s in the local cluster", s.Namespace, s.Name)
			continue
		}
		log.Infof("orphan sweep: deleted orphaned secret %s/%s in the local cluster (no FloatingIPProjectQuota for project %s)", s.Namespace, s.Name, projectID)
		deleted++
	}
	return deleted, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FloatingIPProjectQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// The orphaned config secret sweep runs on a fixed interval as its own
	// runnable, independent of reconcile traffic. The manager leader-gates
	// it, so only the elected replica sweeps.
	if err := mgr.Add(&floatingIPProjectQuotaSweeper{reconciler: r}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&rbbv1beta2.FloatingIPProjectQuota{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Regular updates are ignored. An update that carries a
				// deletionTimestamp is the beginning of a delete of an object
				// protected by our cleanup finalizer and must be reconciled
				// so the config secrets get cleaned up.
				return !e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}
