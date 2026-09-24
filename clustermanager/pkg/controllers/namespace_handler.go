package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func EnsureNamespace(ctx context.Context, c client.Client, namespaceName string) error {
	log := logrus.WithFields(logrus.Fields{
		"handler":   "namespace",
		"namespace": namespaceName,
	})

	if namespaceName == "" {
		err := fmt.Errorf("namespace name is not set in the configuration")
		log.Error(err)
		return err
	}

	namespace := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Infof("Namespace %s does not exist, creating it", namespaceName)
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespaceName,
				},
			}
			if err := c.Create(ctx, namespace); err != nil {
				log.WithError(err).Error("unable to create namespace")
				return err
			}
			log.Infof("Successfully created namespace %s", namespaceName)
		} else {
			log.WithError(err).Error("unable to fetch namespace")
			return err
		}
	} else {
		log.Debugf("Namespace %s already exists", namespaceName)
	}

	return nil
}

func LabelNamespaceWithProjectID(ctx context.Context, c client.Client, namespaceName, projectID string) error {
	log := logrus.WithFields(logrus.Fields{
		"handler":   "namespace",
		"namespace": namespaceName,
		"projectID": projectID,
	})

	namespace := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace); err != nil {
		log.WithError(err).Error("unable to fetch namespace for labeling")
		return err
	}
	log.Infof("Successfully fetched namespace: %s", namespace.Name)

	if namespace.Labels == nil {
		namespace.Labels = make(map[string]string)
	}

	if val, ok := namespace.Labels["rancher.k8s.binbash.org/project-name"]; ok && val == projectID {
		log.Infof("Namespace %s already has the correct project-name label", namespace.Name)
		return nil
	}

	namespace.Labels["rancher.k8s.binbash.org/project-name"] = projectID
	if err := c.Update(ctx, namespace); err != nil {
		log.WithError(err).Error("unable to update namespace with project-name label")
		return err
	}
	log.Infof("Successfully updated namespace %s with project-name label", namespace.Name)

	// Restart the rancher-fip-lb-controller deployment to pick up the new label
	deploymentName := "rancher-fip-lb-controller"
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespaceName}, deployment); err != nil {
		if errors.IsNotFound(err) {
			log.Warnf("Deployment %s/%s not found, skipping restart (may not be deployed yet)", namespaceName, deploymentName)
			return nil
		}
		log.WithError(err).Errorf("unable to fetch deployment %s/%s for restart", namespaceName, deploymentName)
		return err
	}

	// Add restart annotation to trigger rollout restart
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["rancher.k8s.binbash.org/lb-controller-restarted-at"] = time.Now().Format(time.RFC3339)

	if err := c.Update(ctx, deployment); err != nil {
		log.WithError(err).Errorf("unable to restart deployment %s/%s", namespaceName, deploymentName)
		return err
	}
	log.Infof("Successfully restarted deployment %s/%s to pick up new namespace label", namespaceName, deploymentName)

	return nil
}
