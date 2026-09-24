package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joeyloman/rancher-fip-cluster-manager/internal/helm"
	"github.com/joeyloman/rancher-fip-cluster-manager/pkg/config"
	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	provisioningv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"helm.sh/helm/v3/pkg/repo"
)

// ClusterProvisioningReconciler reconciles a managementv3.Cluster object.
type ClusterProvisioningReconciler struct {
	client.Client
	// Scheme is the scheme for the controller.
	Scheme *runtime.Scheme
	// HelmClient is the Helm client for installing and upgrading charts.
	HelmClient helm.Interface
	// Config is the configuration for the controller.
	Config *config.Config
	// AppNamespace is the namespace where the application is deployed.
	AppNamespace string
	// NewDownstreamClientFunc is a function that returns a new downstream client.
	NewDownstreamClientFunc func(config *rest.Config) (client.Client, error)
	// previousLabels stores the previous "rancher-fip" label values for UPDATE event comparison
	previousLabels sync.Map
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
func (r *ClusterProvisioningReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	clusterKey := req.NamespacedName.String()

	// Remove the clusterKey from the previousLabels map if the reconciliation fails or is requeued
	defer func() {
		if result.RequeueAfter > 0 {
			// Delete the clusterKey from the previousLabels map so rancher-fip relabeling will trigger a new reconciliation if the cluster is requeued
			r.previousLabels.Delete(clusterKey)
		}
	}()

	log := logrus.WithFields(logrus.Fields{
		"controller": "clusterprovisioning",
		"name":       req.NamespacedName,
	})

	// Fetch the Cluster object
	var cluster managementv3.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Object not found, treat as deleted
			log.Infof("Cluster %s not found, checking for FloatingIP cleanup", req.Name)

			// Delete the clusterKey from the previousLabels map
			r.previousLabels.Delete(clusterKey)

			// Run cleanup
			if err := r.releaseFloatingIPs(ctx, log, req.Name); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		log.WithError(err).Error("unable to fetch Cluster")
		return ctrl.Result{}, err
	}

	// Filter out clusters that should not be reconciled
	if cluster.Name == "local" ||
		cluster.Labels["provider.cattle.io"] == "harvester" ||
		cluster.Labels["rancher-fip"] != "enabled" {
		log.Debugf("Skipping reconciliation for cluster: %s [%s]", cluster.Name, cluster.Spec.DisplayName)

		// Delete the clusterKey from the previousLabels map so rancher-fip relabeling will trigger a new reconciliation
		r.previousLabels.Delete(clusterKey)

		return ctrl.Result{}, nil
	}

	// Check if this is an UPDATE event by checking if we have a previous label value stored
	previousLabelValue, isUpdate := r.previousLabels.Load(clusterKey)

	if isUpdate {
		// This is an UPDATE event - check the "rancher-fip" label change
		oldLabelValue, _ := previousLabelValue.(string)
		newLabelValue := cluster.Labels["rancher-fip"]

		// Check if old object had the label
		oldHadLabel := oldLabelValue != ""

		// Check if new object has the label
		newHasLabel := newLabelValue != ""

		// Continue only if:
		// 1. Old object had the label and new object has a different value, OR
		// 2. Old object didn't have the label but new object does
		shouldContinue := false
		if oldHadLabel && newHasLabel && oldLabelValue != newLabelValue {
			// Label value changed
			shouldContinue = true
			log.Debugf("rancher-fip label value changed from '%s' to '%s' for cluster %s [%s]", oldLabelValue, newLabelValue, cluster.Name, cluster.Spec.DisplayName)
		} else if !oldHadLabel && newHasLabel {
			// Label was added
			shouldContinue = true
			log.Debugf("rancher-fip label was added with value '%s' for cluster %s [%s]", newLabelValue, cluster.Name, cluster.Spec.DisplayName)
		}

		// Update the stored label value for future comparisons
		r.previousLabels.Store(clusterKey, newLabelValue)

		if !shouldContinue {
			log.Debugf("Skipping reconciliation for UPDATE event - rancher-fip label condition not met for cluster: %s [%s]", cluster.Name, cluster.Spec.DisplayName)
			return ctrl.Result{}, nil
		}
	} else {
		// This is a CREATE event - store the current label value for future UPDATE comparisons
		r.previousLabels.Store(clusterKey, cluster.Labels["rancher-fip"])
	}

	log.Infof("Reconciling cluster: %s [%s]", cluster.Name, cluster.Spec.DisplayName)

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
		log.Debugf("kubeconfig secret not found for cluster %s (tried %s and %s), retrying in 2 minutes", cluster.Name, kubeconfigSecretName, kubeconfigSecretNameWithDisplayName)
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
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

	downstreamClient, err := r.NewDownstreamClientFunc(downstreamConfig)
	if err != nil {
		log.WithError(err).Error("unable to create new downstream client")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Health check the downstream cluster
	podList := &corev1.PodList{}
	if err := downstreamClient.List(ctx, podList, client.InNamespace("kube-system")); err != nil {
		if strings.Contains(err.Error(), "Forbidden") {
			log.Debugf("downstream cluster [%s] is still in deploy state: %s, retrying in 2 minutes", cluster.Name, err.Error())
			return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
		}
		log.WithError(err).Error("unable to list pods in downstream cluster for health check")
		return ctrl.Result{}, err
	}

	log.Debugf("Successfully connected to downstream cluster and performed health check")

	// Check if the r.Config.RancherFipLBControllerNamespace exsists in the downstream cluster, if not create it
	if err := EnsureNamespace(ctx, downstreamClient, r.Config.RancherFipLBControllerNamespace); err != nil {
		log.WithError(err).Error("failed to ensure rancher-fip-lb-controller namespace")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Install rancher-fip-lb-controller
	if strings.HasPrefix(r.Config.RancherFipLBControllerChartRef, "https://") || strings.HasPrefix(r.Config.RancherFipLBControllerChartRef, "http://") {
		chartRepo := repo.Entry{
			Name: r.Config.RancherFipLBControllerChartName,
			URL:  r.Config.RancherFipLBControllerChartRef,
		}
		if err := r.HelmClient.AddOrUpdateChartRepo(chartRepo); err != nil {
			log.WithError(err).Error("unable to add or update rancher-fip-lb-controller chart repo")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
	}
	rancherFipLBControllerChartSpec := r.createChartSpec(
		r.Config.RancherFipLBControllerChartName,
		r.Config.RancherFipLBControllerChartRef,
		r.Config.RancherFipLBControllerChartVersion,
		r.Config.RancherFipLBControllerNamespace,
		r.Config.RancherFipLBControllerValues,
	)
	if err := r.HelmClient.InstallOrUpgrade(ctx, downstreamConfig, rancherFipLBControllerChartSpec); err != nil {
		log.WithError(err).Error("unable to install rancher-fip-lb-controller chart")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Determine the load balancer type from the cluster label
	loadBalancerType := "purelb"
	if cluster.Labels["rancher-fip-lbtype"] == "metallb" {
		loadBalancerType = "metallb"
	}

	log.Debugf("loadBalancerType %s detected for cluster %s [%s]", loadBalancerType, cluster.Name, cluster.Spec.DisplayName)

	if loadBalancerType == "purelb" {
		chartInstalled, err := r.HelmClient.CheckIfChartIsInstalled(ctx, downstreamConfig, r.Config.MetalLBControllerChartName, r.Config.MetalLBControllerNamespace)
		if err != nil {
			log.WithError(err).Error("unable to check if metallb chart is installed")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
		if chartInstalled {
			log.Errorf("load balancer type is set to purelb, but metallb is already installed, skipping reconciliation")
			return ctrl.Result{}, nil
		}
	} else if loadBalancerType == "metallb" {
		chartInstalled, err := r.HelmClient.CheckIfChartIsInstalled(ctx, downstreamConfig, r.Config.PureLBControllerChartName, r.Config.PureLBControllerNamespace)
		if err != nil {
			log.WithError(err).Error("unable to check if purelb chart is installed")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
		if chartInstalled {
			log.Errorf("load balancer type is set to metallb, but purelb is already installed, skipping reconciliation")
			return ctrl.Result{}, nil
		}
	}

	if loadBalancerType == "metallb" {
		// Check if the r.Config.MetalLBControllerNamespace exists in the downstream cluster, if not create it
		if err := EnsureNamespace(ctx, downstreamClient, r.Config.MetalLBControllerNamespace); err != nil {
			log.WithError(err).Error("failed to ensure metallb-controller namespace")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}

		// Install MetalLB
		if strings.HasPrefix(r.Config.MetalLBControllerChartRef, "https://") || strings.HasPrefix(r.Config.MetalLBControllerChartRef, "http://") {
			chartRepo := repo.Entry{
				Name: r.Config.MetalLBControllerChartName,
				URL:  r.Config.MetalLBControllerChartRef,
			}
			if err := r.HelmClient.AddOrUpdateChartRepo(chartRepo); err != nil {
				log.WithError(err).Error("unable to add or update metallb chart repo")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
			}
		}
		metalLBChartSpec := r.createChartSpec(
			r.Config.MetalLBControllerChartName,
			r.Config.MetalLBControllerChartRef,
			r.Config.MetalLBControllerChartVersion,
			r.Config.MetalLBControllerNamespace,
			r.Config.MetalLBControllerValues,
		)

		if err := r.HelmClient.InstallOrUpgrade(ctx, downstreamConfig, metalLBChartSpec); err != nil {
			log.WithError(err).Error("unable to install metallb chart")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
	} else {
		// If no rancher0fip-lbtype is set, PureLB is the default LB controller, so we need to install it
		// Check if the r.Config.PureLBControllerNamespace exists in the downstream cluster, if not create it
		if err := EnsureNamespace(ctx, downstreamClient, r.Config.PureLBControllerNamespace); err != nil {
			log.WithError(err).Error("failed to ensure pure-controller namespace")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}

		// Install PureLB
		if strings.HasPrefix(r.Config.PureLBControllerChartRef, "https://") || strings.HasPrefix(r.Config.PureLBControllerChartRef, "http://") {
			chartRepo := repo.Entry{
				Name: r.Config.PureLBControllerChartName,
				URL:  r.Config.PureLBControllerChartRef,
			}
			if err := r.HelmClient.AddOrUpdateChartRepo(chartRepo); err != nil {
				log.WithError(err).Error("unable to add or update purelb chart repo")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
			}
		}
		pureLBChartSpec := r.createChartSpec(
			r.Config.PureLBControllerChartName,
			r.Config.PureLBControllerChartRef,
			r.Config.PureLBControllerChartVersion,
			r.Config.PureLBControllerNamespace,
			r.Config.PureLBControllerValues,
		)

		if err := r.HelmClient.InstallOrUpgrade(ctx, downstreamConfig, pureLBChartSpec); err != nil {
			log.WithError(err).Error("unable to install purelb chart")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
		}
	}

	// Get the provisioningv1 cluster object by matching them with the cluster.name or cluster.spec.displayname
	var provisioningClusters provisioningv1.ClusterList
	if err := r.List(ctx, &provisioningClusters); err != nil {
		log.WithError(err).Error("unable to list provisioningv1 clusters")
		return ctrl.Result{}, err
	}

	var provisioningCluster *provisioningv1.Cluster
	for i := range provisioningClusters.Items {
		pCluster := &provisioningClusters.Items[i]
		if pCluster.Name == cluster.Name || pCluster.Name == cluster.Spec.DisplayName {
			provisioningCluster = pCluster
			log.Infof("Found matching provisioningv1 cluster: %s in namespace %s", provisioningCluster.Name, provisioningCluster.Namespace)
			break
		}
	}

	if provisioningCluster == nil {
		err := fmt.Errorf("unable to find matching provisioningv1 cluster for %s", cluster.Name)
		log.Error(err)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// check if the cloudCredentialSecretName exists
	if provisioningCluster.Spec.CloudCredentialSecretName == "" {
		log.Infof("No Harvester cloudCredentialSecretName found for cluster %s [%s], skipping client/server secret creation", cluster.Name, cluster.Spec.DisplayName)

		return ctrl.Result{}, nil
	}

	var harvesterConfigName string
	if provisioningCluster.Spec.RKEConfig != nil && provisioningCluster.Spec.RKEConfig.MachinePools != nil {
		for _, pool := range provisioningCluster.Spec.RKEConfig.MachinePools {
			if pool.ControlPlaneRole && pool.NodeConfig != nil && pool.NodeConfig.Kind == "HarvesterConfig" {
				harvesterConfigName = pool.NodeConfig.Name
				log.Infof("Found HarvesterConfig: %s", harvesterConfigName)
				break
			}
		}
	}

	if harvesterConfigName == "" {
		log.Info("No HarvesterConfig found for control plane nodes, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Get the HarvesterConfig.rke-machine-config.cattle.io object from the "fleet-default" namespace named harvesterConfigName
	harvesterConfig := &unstructured.Unstructured{}
	harvesterConfig.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "rke-machine-config.cattle.io",
		Version: "v1",
		Kind:    "HarvesterConfig",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: harvesterConfigName, Namespace: "fleet-default"}, harvesterConfig); err != nil {
		log.WithError(err).Error("unable to fetch HarvesterConfig")
		return ctrl.Result{}, err
	}
	log.Infof("Successfully fetched HarvesterConfig: %s", harvesterConfig.GetName())

	// Get the Namespace in Harvester where the VMs are deployed for the downstream cluster from the HarvesterConfig object
	vmNamespace, _, _ := unstructured.NestedString(harvesterConfig.Object, "vmNamespace")
	log.Infof("the harvester vmNamespace is: %s", vmNamespace)

	// Get the configured networks for the downstream cluster from the HarvesterConfig object
	networkInfoRaw, _, _ := unstructured.NestedString(harvesterConfig.Object, "networkInfo")
	log.Infof("the harvester networkInfo is: %s", networkInfoRaw)

	type networkInfoData struct {
		Interfaces []struct {
			NetworkName string `json:"networkName"`
		} `json:"interfaces"`
	}

	var networkInfo networkInfoData
	if err := json.Unmarshal([]byte(networkInfoRaw), &networkInfo); err != nil {
		log.WithError(err).Error("unable to unmarshal networkInfo")
		return ctrl.Result{}, err
	}

	if len(networkInfo.Interfaces) == 0 || networkInfo.Interfaces[0].NetworkName == "" {
		err := fmt.Errorf("networkName not found in networkInfo")
		log.Error(err)
		return ctrl.Result{}, err
	}

	networkName := networkInfo.Interfaces[0].NetworkName
	networkNameParts := strings.Split(networkName, "/")
	if len(networkNameParts) != 2 {
		err := fmt.Errorf("invalid networkName format: %s", networkName)
		log.Error(err)
		return ctrl.Result{}, err
	}

	harvesterNamespace := networkNameParts[0]
	harvesterNetworkname := networkNameParts[1]
	log.Infof("harvester namespace: %s and networkname: %s", harvesterNamespace, harvesterNetworkname)

	// get the cloud credential secret name by splitting the secret object <namespace>:<secret>
	cloudCredentialSecretNameSplitted := strings.Split(provisioningCluster.Spec.CloudCredentialSecretName, ":")
	if len(cloudCredentialSecretNameSplitted) < 2 {
		err := fmt.Errorf("error cloudCredentialSecretName format is not correct, expected format <namespace>:<secret>")
		log.Error(err)
		return ctrl.Result{}, err
	}
	cloudCredentialNamespace := cloudCredentialSecretNameSplitted[0]
	cloudCredentialName := cloudCredentialSecretNameSplitted[1]
	log.Infof("cloudCredentialSecretName [%s/%s]", cloudCredentialNamespace, cloudCredentialName)

	cloudCredentialSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: cloudCredentialName, Namespace: cloudCredentialNamespace}, cloudCredentialSecret); err != nil {
		log.WithError(err).Errorf("error while fetching the cloud credential secret contents")
		return ctrl.Result{}, err
	}

	clusterId, ok := cloudCredentialSecret.Data["harvestercredentialConfig-clusterId"]
	if !ok {
		err := fmt.Errorf("harvestercredentialConfig-clusterId not found in secret %s/%s", cloudCredentialNamespace, cloudCredentialName)
		log.Error(err)
		return ctrl.Result{}, err
	}
	log.Infof("Found clusterId: %s", string(clusterId))

	// Get the Harvester cluster name from the cluster.management.cattle.io object
	harvesterCluster := &managementv3.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: string(clusterId)}, harvesterCluster); err != nil {
		log.WithError(err).Errorf("error while fetching the cluster.management.cattle.io object")
		return ctrl.Result{}, err
	}
	log.Infof("Found cluster: %s", harvesterCluster.Spec.DisplayName)

	// Create the target network name
	targetNetwork := fmt.Sprintf("%s--%s", harvesterCluster.Spec.DisplayName, harvesterNetworkname)
	log.Infof("targetNetwork: %s", targetNetwork)

	harvesterKubeConfigSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-kubeconfig", clusterId), Namespace: "fleet-default"}, harvesterKubeConfigSecret); err != nil {
		log.WithError(err).Errorf("error while fetching the Harvester kubeconfig secret contents")
		return ctrl.Result{}, err
	}

	harvesterKubeConfig := harvesterKubeConfigSecret.Data["value"]

	harvesterClientConfig, err := clientcmd.RESTConfigFromKubeConfig(harvesterKubeConfig)
	if err != nil {
		log.WithError(err).Errorf("error while creating REST config from Harvester kubeconfig")
		return ctrl.Result{}, err
	}

	harvesterClient, err := r.NewDownstreamClientFunc(harvesterClientConfig)
	if err != nil {
		log.WithError(err).Errorf("error while creating new downstream client")
		return ctrl.Result{}, err
	}

	// Get the namespace object with the vmNamespace variable name from the Harvester cluster using the harvesterClient.
	namespace := &corev1.Namespace{}
	if err := harvesterClient.Get(ctx, types.NamespacedName{Name: vmNamespace}, namespace); err != nil {
		log.WithError(err).Errorf("unable to fetch namespace %s from Harvester cluster", vmNamespace)
		return ctrl.Result{}, err
	}
	log.Infof("Successfully fetched namespace: %s", namespace.Name)

	// Generate and create the secret for the downstream cluster
	projectID, ok := namespace.Annotations["field.cattle.io/projectId"]
	if !ok {
		err := fmt.Errorf("projectId label not found on cluster")
		log.WithError(err).Error("unable to get project ID")
		return ctrl.Result{}, nil // Do not requeue, as this is a configuration issue
	}
	log.Infof("found projectID in namespace annotations: %s", projectID)

	projectIDParts := strings.Split(projectID, ":")
	if len(projectIDParts) == 2 {
		projectID = projectIDParts[1]
	}

	// Find the project namespace
	var projects managementv3.ProjectList
	if err := r.List(ctx, &projects); err != nil {
		log.WithError(err).Error("unable to list projects")
		return ctrl.Result{}, err
	}

	var project managementv3.Project
	var projectNamespace string
	projectFound := false
	for _, p := range projects.Items {
		if p.Name == projectID {
			project = p
			projectNamespace = p.Namespace
			projectFound = true
			break
		}
	}

	if !projectFound {
		err := fmt.Errorf("project not found")
		log.WithError(err).Errorf("unable to find project with ID %s", projectID)
		return ctrl.Result{}, nil // Do not requeue
	}

	// Label the RancherFipLBControllerNamespace with the projectID in the downstream cluster so we can determine the project
	if err := LabelNamespaceWithProjectID(ctx, downstreamClient, r.Config.RancherFipLBControllerNamespace, projectID); err != nil {
		log.WithError(err).Error("failed to label namespace with project id")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	if err := HandleNetworkConfigMap(ctx, r.Client, downstreamClient, harvesterCluster.Spec.DisplayName, r.Config.RancherFipLBControllerNamespace); err != nil {
		log.WithError(err).Error("failed to handle network configmap")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	return HandleConfigSecrets(
		ctx, r.Client,
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

func (r *ClusterProvisioningReconciler) releaseFloatingIPs(ctx context.Context, log *logrus.Entry, clusterName string) error {
	logrus.Infof("Checking for floatingips to release for cluster %s", clusterName)

	// Get all FloatingIPs which are assigned to the cluster
	var fipList rbbv1beta2.FloatingIPList
	if err := r.List(ctx, &fipList); err != nil {
		log.WithError(err).Error("unable to list floatingips")
		return err
	}

	for _, fip := range fipList.Items {
		// compare the clustername with the floatingip object rancher.k8s.binbash.org/cluster-name label
		if fip.Labels["rancher.k8s.binbash.org/cluster-name"] == clusterName {
			fipToUpdate := fip.DeepCopy()

			delete(fipToUpdate.Labels, "rancher.k8s.binbash.org/cluster-name")
			delete(fipToUpdate.Labels, "rancher.k8s.binbash.org/service-name")
			delete(fipToUpdate.Labels, "rancher.k8s.binbash.org/service-namespace")

			// the status assigned and conditions will be updated by the rancher-fip-manager controller

			if err := r.Update(ctx, fipToUpdate); err != nil {
				log.Errorf("failed to update floatingip: %s", err.Error())
				return err
			}
			log.Infof("Released FloatingIP %s for deleted cluster %s", fip.Name, clusterName)
		}
	}

	return nil
}

func (r *ClusterProvisioningReconciler) createChartSpec(name, ref, version, namespace, values string) helm.ChartSpec {
	spec := helm.ChartSpec{
		ChartName:   name,
		Version:     version,
		Namespace:   namespace,
		Values:      values,
		ReleaseName: name,
	}

	if strings.HasPrefix(ref, "oci://") {
		spec.ChartName = ref
	} else if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		spec.RepoURL = ref
		spec.ChartName = fmt.Sprintf("%s/%s", name, name)
	}

	return spec
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterProvisioningReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&managementv3.Cluster{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return true
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}
