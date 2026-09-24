package controllers

import (
	"context"
	"fmt"
	"time"

	secret "github.com/joeyloman/rancher-fip-cluster-manager/internal/secret"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func HandleConfigSecrets(
	ctx context.Context,
	localClient client.Client,
	downstreamClient client.Client,
	targetNamespace string,
	project *v3.Project,
	projectNamespace string,
	targetNetwork string,
	rancherFipApiServerURL string,
	cluster string,
	loadBalancerType string,
	caCrt []byte,
) (ctrl.Result, error) {
	projectID := project.Name
	secretName := fmt.Sprintf("rancher-fip-config-%s", projectID)
	var secretExists bool = false

	// Check if the secret already exists in the local cluster
	var existingSecret corev1.Secret
	if err := localClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: projectNamespace}, &existingSecret); err == nil {
		log.Infof("Secret %s already exists in local cluster, using existing clientSecret", secretName)
		secretExists = true
	}

	var clientSecret string
	if secretExists {
		clientSecret = string(existingSecret.Data["clientSecret"])
	} else {
		// Generate clientSecret and create secrets
		var err error
		clientSecret, err = secret.GenerateClientSecret()
		if err != nil {
			log.WithError(err).Error("unable to generate clientSecret")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	}

	// Create secret in the local cluster
	if !secretExists {
		localSecret := secret.NewLocalSecret(secretName, projectNamespace, clientSecret, projectID)
		if err := localClient.Create(ctx, localSecret); err != nil {
			log.WithError(err).Error("unable to create secret in local cluster")
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		log.Info("Successfully created secret in local cluster")
	}

	// This happens for Harvester clusters
	if downstreamClient == nil {
		log.Infof("Downstream client is nil, skipping downstream cluster processing..")
		return ctrl.Result{}, nil
	}

	// check if the rancher-fip-manager namespace exists in the downstream cluster, if not create it
	var ns corev1.Namespace
	if err := downstreamClient.Get(ctx, types.NamespacedName{Name: targetNamespace}, &ns); err != nil {
		if errors.IsNotFound(err) {
			log.Infof("Namespace %s does not exist in the downstream cluster, creating it", targetNamespace)
			newNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: targetNamespace,
				},
			}
			if err := downstreamClient.Create(ctx, newNs); err != nil {
				log.WithError(err).Errorf("unable to create namespace %s in the downstream cluster", targetNamespace)
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			log.Infof("Successfully created namespace %s in the downstream cluster", targetNamespace)
		} else {
			log.WithError(err).Errorf("unable to get namespace %s from the downstream cluster", targetNamespace)
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	}

	// create the cacerts secret in the downstream cluster
	if caCrt != nil {
		caCertsSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cacerts",
				Namespace: targetNamespace,
			},
			Data: map[string][]byte{
				"ca.crt": caCrt,
			},
		}

		// check if the secret already exists
		var existingCaCertsSecret corev1.Secret
		if err := downstreamClient.Get(ctx, types.NamespacedName{Name: "cacerts", Namespace: targetNamespace}, &existingCaCertsSecret); err != nil {
			if errors.IsNotFound(err) {
				log.Infof("Secret cacerts does not exist in the downstream cluster, creating it")
				if err := downstreamClient.Create(ctx, caCertsSecret); err != nil {
					log.WithError(err).Error("unable to create cacerts secret in downstream cluster")
					return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
				}
				log.Info("Successfully created cacerts secret in downstream cluster")
			} else {
				log.WithError(err).Errorf("unable to get secret cacerts from the downstream cluster")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
		} else {
			log.Debugf("Secret %s already exists in the downstream cluster, skipping creation", "cacerts")
		}
	}

	// check if the secret exists in the downstream cluster, if not create it
	var existingDownstreamSecret corev1.Secret
	if err := downstreamClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: targetNamespace}, &existingDownstreamSecret); err != nil {
		if errors.IsNotFound(err) {
			log.Infof("Secret %s does not exist in the downstream cluster, creating it", secretName)
			downstreamSecret := secret.NewDownstreamSecret(secretName, targetNamespace, rancherFipApiServerURL, clientSecret, cluster, projectID, targetNetwork, loadBalancerType)
			if err := downstreamClient.Create(ctx, downstreamSecret); err != nil {
				log.WithError(err).Error("unable to create secret in downstream cluster")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			log.Info("Successfully created secret in downstream cluster")
		} else {
			log.WithError(err).Errorf("unable to get secret %s from the downstream cluster", secretName)
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
	} else {
		log.Debugf("Secret %s already exists in the downstream cluster, skipping creation", secretName)
	}

	return ctrl.Result{}, nil
}
