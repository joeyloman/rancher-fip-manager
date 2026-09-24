package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"k8s.io/client-go/tools/clientcmd"
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
		log.WithError(err).Error("unable to fetch FloatingIPProjectQuota")
		return ctrl.Result{}, client.IgnoreNotFound(err)
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

	// Retrieve kubeconfig for the downstream cluster by listing all secrets in the fleet-default namespace
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace("fleet-default")); err != nil {
		log.WithError(err).Error("unable to list secrets in fleet-default namespace")
		return ctrl.Result{}, err
	}

	var kubeconfigSecret corev1.Secret
	found := false
	kubeconfigSecretName := fmt.Sprintf("%s-kubeconfig", cluster.Name)
	kubeconfigSecretNameWithDisplayName := fmt.Sprintf("%s-kubeconfig", cluster.Spec.DisplayName)

	for _, secret := range secrets.Items {
		if secret.Name == kubeconfigSecretName || secret.Name == kubeconfigSecretNameWithDisplayName {
			kubeconfigSecret = secret
			found = true
			log.Debugf("Found kubeconfig secret %s for cluster %s", secret.Name, cluster.Name)
			break
		}
	}

	if !found {
		err := fmt.Errorf("unable to fetch kubeconfig secret for cluster %s (tried %s and %s)", cluster.Name, kubeconfigSecretName, kubeconfigSecretNameWithDisplayName)
		log.WithError(err).Error("kubeconfig secret not found")
		return ctrl.Result{}, err
	}

	// Create a new client for the downstream cluster
	downstreamKubeconfig, ok := kubeconfigSecret.Data["value"]
	if !ok {
		err := fmt.Errorf("kubeconfig secret does not contain 'value' key")
		log.WithError(err).Error("invalid kubeconfig secret")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	downstreamConfig, err := clientcmd.RESTConfigFromKubeConfig(downstreamKubeconfig)
	if err != nil {
		log.WithError(err).Error("unable to create REST config from kubeconfig")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	downstreamClient, err := client.New(downstreamConfig, client.Options{Scheme: r.Scheme})
	if err != nil {
		log.WithError(err).Error("unable to create new downstream client")
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

// SetupWithManager sets up the controller with the Manager.
func (r *FloatingIPProjectQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbbv1beta2.FloatingIPProjectQuota{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return false
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
