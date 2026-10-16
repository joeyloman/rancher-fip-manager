package admission

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// REPORT.md M9: transient API-server errors (throttle, connection refused,
// RBAC blip) must surface to the caller instead of masquerading as an empty
// configmap ("ca.crt not found in configmap"), which crashed the webhook at
// startup for a condition it could have retried through.
func (h *Handler) getCABundleConfigMap() (*corev1.ConfigMap, error) {
	configmap, err := h.clientset.CoreV1().ConfigMaps("kube-system").Get(h.ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-root-ca.crt configmap in kube-system: %s", err.Error())
	}

	return configmap, nil
}
