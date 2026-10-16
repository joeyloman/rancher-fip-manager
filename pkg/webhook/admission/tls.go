package admission

import "fmt"

func (h *Handler) getCaBundleFromCABundleConfigMap() (cert string, err error) {
	c, err := h.getCABundleConfigMap()
	if err != nil {
		return "", err
	}

	cert, exists := c.Data["ca.crt"]
	if !exists {
		return "", fmt.Errorf("ca.crt not found in kube-root-ca.crt configmap")
	}

	return cert, nil
}
