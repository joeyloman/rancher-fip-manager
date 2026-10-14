package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (h *Handler) tlsSecretData(tlsPair tls.Certificate) (map[string][]byte, error) {
	if len(tlsPair.Certificate) == 0 || len(tlsPair.Certificate[0]) == 0 {
		return nil, fmt.Errorf("refusing to write empty certificate to webhook secret")
	}

	bKey, err := x509.MarshalPKCS8PrivateKey(tlsPair.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal private key: %s", err.Error())
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: bKey})
	if pemKey == nil {
		return nil, fmt.Errorf("failed to encode key to PEM")
	}

	return map[string][]byte{
		"tls.key": pemKey,
		"tls.crt": tlsPair.Certificate[0],
	}, nil
}

func (h *Handler) newSecret(tlsPair tls.Certificate) (*corev1.Secret, error) {
	data, err := h.tlsSecretData(tlsPair)
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{}
	secret.Type = corev1.SecretTypeTLS
	secret.ObjectMeta.Name = h.webhookSecretName
	secret.ObjectMeta.Namespace = h.webhookNamespace
	secret.Data = data

	return secret, nil
}

func (h *Handler) createSecret(tlsPair tls.Certificate) error {
	newSecret, err := h.newSecret(tlsPair)
	if err != nil {
		return err
	}

	_, err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Create(context.TODO(), newSecret, metav1.CreateOptions{})
	return err
}

// updateSecret replaces the TLS material of the webhook secret in place. The
// existing secret (which may still hold a valid certificate) survives until a
// fully signed certificate is in hand, so a failed renewal can never brick the
// webhook. If the secret disappeared concurrently it is created instead.
func (h *Handler) updateSecret(tlsPair tls.Certificate) error {
	data, err := h.tlsSecretData(tlsPair)
	if err != nil {
		return err
	}

	existing, err := h.getSecret()
	if apierrors.IsNotFound(err) {
		return h.createSecret(tlsPair)
	}
	if err != nil {
		return fmt.Errorf("cannot fetch webhook secret: %s", err.Error())
	}

	secret := existing.DeepCopy()
	secret.Type = corev1.SecretTypeTLS
	secret.Data = data

	_, err = h.clientset.CoreV1().Secrets(h.webhookNamespace).Update(context.TODO(), secret, metav1.UpdateOptions{})
	return err
}

// getSecret returns the webhook TLS secret. Callers MUST distinguish
// apierrors.IsNotFound(err) (absent) from other errors (transient API
// failure); treating every error as "absent" makes the webhook regenerate
// certificates during an API blip.
func (h *Handler) getSecret() (corev1.Secret, error) {
	secret, err := h.clientset.CoreV1().Secrets(h.webhookNamespace).Get(context.TODO(), h.webhookSecretName, metav1.GetOptions{})
	if err != nil {
		return corev1.Secret{}, err
	}

	return *secret, nil
}

func (h *Handler) checkSecret() (bool, error) {
	_, err := h.getSecret()
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error checking webhook TLS secret: %s", err.Error())
	}

	return true, nil
}
