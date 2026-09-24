package secret

import (
	"crypto/rand"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GenerateClientSecret generates a new, random clientSecret token.
func GenerateClientSecret() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// NewSecret creates a new Kubernetes Secret object in the Downstream cluster with the given name, namespace, clientsecret, and project ID.
func NewDownstreamSecret(name, namespace, apiUrl, clientSecret, cluster, project, floatingIPPool, loadBalancerType string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"apiUrl":           []byte(apiUrl),
			"cluster":          []byte(cluster),
			"clientSecret":     []byte(clientSecret),
			"project":          []byte(project),
			"floatingIPPool":   []byte(floatingIPPool),
			"loadBalancerType": []byte(loadBalancerType),
		},
	}
}

// NewLocalSecret creates a new Kubernetes Secret object in the Local cluster with the given name, namespace, clientsecret, and project ID.
func NewLocalSecret(name, namespace, clientSecret, project string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"clientSecret": []byte(clientSecret),
			"project":      []byte(project),
		},
	}
}
