package controllers

import (
	"context"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"

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
	apiReader client.Reader,
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
	appNamespace string,
) (ctrl.Result, error) {
	projectID := project.Name
	secretName := configSecretPrefix + projectID
	var secretExists bool = false

	// Credential material is read through the uncached API reader so a
	// just-regenerated local secret is never reconciled from a stale
	// informer snapshot; the cached client remains the fallback for callers
	// (tests) that inject no API reader.
	localReader := client.Reader(localClient)
	if apiReader != nil {
		localReader = apiReader
	}

	// Check if the secret already exists in the local cluster
	var existingSecret corev1.Secret
	if err := localReader.Get(ctx, types.NamespacedName{Name: secretName, Namespace: projectNamespace}, &existingSecret); err == nil {
		log.Infof("Secret %s already exists in local cluster, using existing clientSecret", secretName)
		secretExists = true
	}

	var clientSecret string
	if secretExists {
		clientSecret = string(existingSecret.Data["clientSecret"])
		if clientSecret == "" {
			// A local config secret without a clientSecret can never
			// authenticate (the api-server requires the key and a non-empty
			// match). Regenerate it in place instead of propagating an empty
			// secret downstream during the reconcile below.
			log.Infof("Local secret %s has no clientSecret, regenerating it", secretName)
			var err error
			clientSecret, err = secret.GenerateClientSecret()
			if err != nil {
				log.WithError(err).Error("unable to generate clientSecret")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			existingSecret.Data = map[string][]byte{
				"clientSecret": []byte(clientSecret),
				"project":      []byte(projectID),
			}
			if err := localClient.Update(ctx, &existingSecret); err != nil {
				log.WithError(err).Error("unable to update secret in local cluster")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			log.Info("Successfully regenerated secret in local cluster")
		}
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
	// Resolve the CA bundle freshly on every pass: the local cacerts secret
	// is read through the uncached API reader so a CA rotated after the pod
	// started is observed on the next reconcile instead of never. The
	// startup snapshot (caCrt) is only the fallback for a
	// transiently failing read; a locally deleted cacerts secret stops the
	// propagation (deleting stale downstream copies is not attempted here).
	if apiReader != nil && appNamespace != "" {
		var localCaCertsSecret corev1.Secret
		if err := apiReader.Get(ctx, types.NamespacedName{Name: "cacerts", Namespace: appNamespace}, &localCaCertsSecret); err == nil {
			caCrt = localCaCertsSecret.Data["ca.crt"]
		} else if errors.IsNotFound(err) {
			caCrt = nil
		} else {
			log.WithError(err).Warn("unable to read the local cacerts secret, using the startup CA snapshot")
		}
	}
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
			// The secret is reconciled, not just created once: a rotated
			// cluster CA must propagate downstream or the lb-controller can
			// no longer verify the api-server.
			caCertsData := map[string][]byte{"ca.crt": caCrt}
			if !apiequality.Semantic.DeepEqual(existingCaCertsSecret.Data, caCertsData) {
				log.Info("Secret cacerts in the downstream cluster is out of sync, updating it")
				existingCaCertsSecret.Data = caCertsData
				if err := downstreamClient.Update(ctx, &existingCaCertsSecret); err != nil {
					log.WithError(err).Error("unable to update cacerts secret in downstream cluster")
					return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
				}
				log.Info("Successfully updated cacerts secret in downstream cluster")
			} else {
				log.Debugf("Secret %s in the downstream cluster is up to date", "cacerts")
			}
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
		// The secret is reconciled, not just created once: the local secret
		// can be lost and regenerated with a new clientSecret, and apiUrl /
		// pool / load-balancer type can change with the configuration — a
		// stale downstream copy breaks downstream->api-server auth on every
		// allocation until manual deletion.
		downstreamSecret := secret.NewDownstreamSecret(secretName, targetNamespace, rancherFipApiServerURL, clientSecret, cluster, projectID, targetNetwork, loadBalancerType)
		if !apiequality.Semantic.DeepEqual(existingDownstreamSecret.Data, downstreamSecret.Data) {
			log.Infof("Secret %s in the downstream cluster is out of sync, updating it", secretName)
			existingDownstreamSecret.Data = downstreamSecret.Data
			if err := downstreamClient.Update(ctx, &existingDownstreamSecret); err != nil {
				log.WithError(err).Error("unable to update secret in downstream cluster")
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			log.Info("Successfully updated secret in downstream cluster")
		} else {
			log.Debugf("Secret %s in the downstream cluster is up to date", secretName)
		}
	}

	return ctrl.Result{}, nil
}
