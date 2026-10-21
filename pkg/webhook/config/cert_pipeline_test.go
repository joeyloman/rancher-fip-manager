package config

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testWebhookName      = "rancher-fip-manager-webhook"
	testWebhookNamespace = "rancher-fip-manager"
	testSecretName       = testWebhookName + "-tls"
	testCSRName          = testWebhookName + "." + testWebhookNamespace + ".svc"
)

func newTestHandler(client kubernetes.Interface) *Handler {
	return &Handler{
		ctx:               context.Background(),
		clientset:         client,
		webhookName:       testWebhookName,
		webhookNamespace:  testWebhookNamespace,
		webhookSecretName: testSecretName,
		csrName:           testCSRName,
	}
}

// newSelfSignedPEM produces PEM certificate material and its private key,
// with second precision on NotAfter (x509 encoding truncates sub-second).
func newSelfSignedPEM(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: testCSRName},
		DNSNames:  []string{testCSRName},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  notAfter.Truncate(time.Second),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM
}

func tlsSecret(certPEM, keyPEM []byte) *corev1.Secret {
	s := &corev1.Secret{}
	s.Type = corev1.SecretTypeTLS
	s.Name = testSecretName
	s.Namespace = testWebhookNamespace
	s.Data = map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}

	return s
}

// signCSRReactor answers CertificateSigningRequest Gets with the tracked CSR
// plus a populated Status.Certificate, simulating a functioning signer.
func signCSRReactor(client *fake.Clientset, cert []byte) k8stesting.ReactionFunc {
	gvr := certsv1.SchemeGroupVersion.WithResource("certificatesigningrequests")

	return k8stesting.ReactionFunc(func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(gvr, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return false, nil, nil
		}
		signed := obj.DeepCopyObject().(*certsv1.CertificateSigningRequest)
		signed.Status.Certificate = cert

		return true, signed, nil
	})
}

func shortenCSRWait(t *testing.T) {
	t.Helper()

	oldTimeout, oldInterval := csrSigningTimeout, csrPollInterval
	csrSigningTimeout = 300 * time.Millisecond
	csrPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		csrSigningTimeout, csrPollInterval = oldTimeout, oldInterval
	})
}

// TestRunBrickedSecretForcesRenewalAndPreservesSecret pins the must-renew
// contract: an empty tls.crt (slow CSR signer leftover) must be treated as
// must-renew, the renewal must go through a CSR (not hand back the empty
// cert), and a failing signer must never destroy the existing secret.
func TestRunBrickedSecretForcesRenewalAndPreservesSecret(t *testing.T) {
	shortenCSRWait(t)
	t.Setenv("HOME", t.TempDir())

	client := fake.NewSimpleClientset(tlsSecret(nil, []byte("key-material")))
	h := newTestHandler(client)

	renewed, err := h.Run(60 * 24)
	assert.Error(t, err, "unsigned CSR must fail the renewal, not return empty material")
	assert.False(t, renewed)

	got, getErr := client.CoreV1().Secrets(testWebhookNamespace).Get(context.TODO(), testSecretName, metav1.GetOptions{})
	require.NoError(t, getErr, "failed renewal must not delete/replace the existing secret")
	assert.Empty(t, got.Data["tls.crt"], "secret must be untouched, not overwritten with garbage")

	csrs, listErr := client.CertificatesV1().CertificateSigningRequests().List(context.TODO(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Len(t, csrs.Items, 1, "empty certificate material must trigger a renewal attempt via CSR")
}

// TestRunRegeneratesSecretViaSigner pins the first-boot path:
// the secret is only created once the signer actually returns a parseable
// certificate; the on-disk material then serves that certificate.
func TestRunRegeneratesSecretViaSigner(t *testing.T) {
	shortenCSRWait(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	notAfter := time.Now().Add(time.Hour)
	certPEM, _ := newSelfSignedPEM(t, notAfter)

	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "certificatesigningrequests", signCSRReactor(client, certPEM))
	h := newTestHandler(client)

	renewed, err := h.Run(60 * 24)
	require.NoError(t, err)
	assert.True(t, renewed)

	got, getErr := client.CoreV1().Secrets(testWebhookNamespace).Get(context.TODO(), testSecretName, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, certPEM, got.Data["tls.crt"])
	assert.NotEmpty(t, got.Data["tls.key"])

	onDisk, readErr := os.ReadFile(filepath.Join(dir, "tls.crt"))
	require.NoError(t, readErr)
	assert.Equal(t, certPEM, onDisk)

	expireDate, err := h.GetCertExpireDate()
	require.NoError(t, err)
	assert.WithinDuration(t, notAfter, expireDate, time.Minute)
}

// TestRunTransientSecretErrorDoesNotRegenerate pins the transient-error
// contract: a transient Secrets Get error must surface as an error to be
// retried, and
// must never be conflated with "absent" (which would regenerate material
// and, pre-fix, crash-loop the pod via the scheduler panic).
func TestRunTransientSecretErrorDoesNotRegenerate(t *testing.T) {
	shortenCSRWait(t)
	t.Setenv("HOME", t.TempDir())

	client := fake.NewSimpleClientset(tlsSecret([]byte("garbage"), []byte("key")))
	client.PrependReactor("get", "secrets", k8stesting.ReactionFunc(
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("boom: transient API failure")
		}))
	h := newTestHandler(client)

	renewed, err := h.Run(60 * 24)
	assert.Error(t, err)
	assert.False(t, renewed)

	csrs, listErr := client.CertificatesV1().CertificateSigningRequests().List(context.TODO(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, csrs.Items, "transient API error must not start a regeneration")
}

// TestRunGenerateErrorDoesNotCreateSecret pins the generate-error path: when
// generateTLSKeyAndCert fails the old code fell through to createSecret
// with a zero-value tls.Certificate and panicked on Certificate[0]. Run
// must return the error and write no secret at all.
func TestRunGenerateErrorDoesNotCreateSecret(t *testing.T) {
	shortenCSRWait(t)
	t.Setenv("HOME", t.TempDir())

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "certificatesigningrequests", k8stesting.ReactionFunc(
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("boom: cannot create CSR")
		}))
	h := newTestHandler(client)

	renewed, err := h.Run(60 * 24)
	assert.Error(t, err)
	assert.False(t, renewed)

	secrets, listErr := client.CoreV1().Secrets(testWebhookNamespace).List(context.TODO(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, secrets.Items, "no secret may be created from ungenerated material")
}

// TestGetCertExpireDateMissingFileIsUnusable pins the scheduler contract:
// absent/broken on-disk material is classified errCertUnusable (retry),
// never a hard "cannot determine" that used to log.Panicf the process.
func TestGetCertExpireDateMissingFileIsUnusable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	h := newTestHandler(fake.NewSimpleClientset())
	_, err := h.GetCertExpireDate()
	require.Error(t, err)
	assert.ErrorIs(t, err, errCertUnusable)
}

// TestGetCertExpireDateBrokenFileIsUnusable covers the empty-cert file the
// 2.1 chain used to leave on disk.
func TestGetCertExpireDateBrokenFileIsUnusable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), nil, 0644))

	h := newTestHandler(fake.NewSimpleClientset())
	_, err := h.GetCertExpireDate()
	require.Error(t, err)
	assert.ErrorIs(t, err, errCertUnusable)
}

// newServingPEM produces a real servable pair (PKCS8 key, matching public
// key) — unlike newSelfSignedPEM whose key is PKCS1 bytes under a "PRIVATE
// KEY" label, which tls.X509KeyPair correctly refuses.
func newServingPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: testCSRName},
		DNSNames:  []string{testCSRName},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour).Truncate(time.Second),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

// TestServingCertificateIsServable pins the in-place rotation contract: the
// pair handed to the HTTPS server must be parseable by crypto/tls (a real
// private key, not raw PEM bytes), so a handshake can actually succeed.
func TestServingCertificateIsServable(t *testing.T) {
	certPEM, keyPEM := newServingPEM(t)
	h := newTestHandler(fake.NewSimpleClientset(tlsSecret(certPEM, keyPEM)))

	pair, err := h.ServingCertificate()
	require.NoError(t, err)
	require.NotNil(t, pair)
	require.Len(t, pair.Certificate, 1)
	_, err = x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err, "serving material must be a parsed certificate chain")
	require.NotNil(t, pair.PrivateKey)
	_, ok := pair.PrivateKey.(*rsa.PrivateKey)
	assert.True(t, ok, "crypto/tls requires a parsed private key for handshakes")
}

// TestServingCertificateRejectsMismatchedPair: a secret whose key does not
// belong to its certificate must be reported errCertUnusable, keeping the
// scheduler on the old (still-valid) material instead of pushing a pair
// that would fail every handshake.
func TestServingCertificateRejectsMismatchedPair(t *testing.T) {
	certPEM, _ := newServingPEM(t)
	_, otherKeyPEM := newServingPEM(t)

	h := newTestHandler(fake.NewSimpleClientset(tlsSecret(certPEM, otherKeyPEM)))

	_, err := h.ServingCertificate()
	require.Error(t, err)
	assert.ErrorIs(t, err, errCertUnusable)
}

// TestServingCertificateOnDiskParity: whatever Run writes to disk for the
// file-fallback path must be loadable by tls.LoadX509KeyPair (the service's
// boot fallback parses, unlike the old raw-bytes writes).
func TestServingCertificateOnDiskParity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	certPEM, keyPEM := newServingPEM(t)
	h := newTestHandler(fake.NewSimpleClientset(tlsSecret(certPEM, keyPEM)))
	require.NoError(t, h.writeTLSDataFromSecret())

	_, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	require.NoError(t, err, "atomic-replaced disk pair must be servable")
}
