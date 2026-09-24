package controllers

import (
	"context"

	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	configMapName = "network-interface-mappings"
)

func HandleNetworkConfigMap(ctx context.Context, localClient client.Client, downstreamClient client.Client, targetCluster string, appNamespace string) error {
	log := logrus.WithFields(logrus.Fields{
		"controller": "HandleNetworkConfigMap",
		"cluster":    targetCluster,
	})

	log.Info("handling network configmap")

	var floatingIPPools rbbv1beta2.FloatingIPPoolList
	if err := localClient.List(ctx, &floatingIPPools); err != nil {
		log.WithError(err).Error("unable to list floatingippool objects")
		return err
	}

	configMapData := make(map[string]string)
	for _, floatingIPPool := range floatingIPPools.Items {
		if floatingIPPool.Spec.TargetCluster == targetCluster {
			log.Infof("found matching floatingippool %s for cluster %s", floatingIPPool.Name, targetCluster)
			configMapData[floatingIPPool.Name] = floatingIPPool.Spec.TargetNetworkInterface
		}
	}

	if len(configMapData) == 0 {
		log.Infof("no matching floatingippools found for cluster %s, skipping configmap creation", targetCluster)
		return nil
	}

	// first check if the namespace exists, if not create it
	namespace := &corev1.Namespace{}
	err := downstreamClient.Get(ctx, types.NamespacedName{Name: appNamespace}, namespace)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Infof("namespace %s does not exist, creating it", appNamespace)
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: appNamespace,
				},
			}
			if err := downstreamClient.Create(ctx, ns); err != nil {
				log.WithError(err).Errorf("failed to create namespace %s", appNamespace)
				return err
			}
		} else {
			log.WithError(err).Errorf("failed to get namespace %s", appNamespace)
			return err
		}
	}

	cm := &corev1.ConfigMap{}
	err = downstreamClient.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: appNamespace}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create the ConfigMap
			newCm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: appNamespace,
				},
				Data: configMapData,
			}
			if err := downstreamClient.Create(ctx, newCm); err != nil {
				log.WithError(err).Error("failed to create configmap")
				return err
			}
			log.Infof("successfully created configmap %s/%s", appNamespace, configMapName)
			return nil
		}
		log.WithError(err).Error("failed to get configmap")
		return err
	}

	// Update the ConfigMap
	cm.Data = configMapData
	if err := downstreamClient.Update(ctx, cm); err != nil {
		log.WithError(err).Error("failed to update configmap")
		return err
	}
	log.Infof("successfully updated configmap %s/%s", appNamespace, configMapName)

	return nil
}
