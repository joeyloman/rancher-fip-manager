package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/config"
	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/service"
)

// keepRetriesAway prevents the deferred AfterFunc recovery callback from
// firing after the test returns (it would touch a fake-less handler).
func keepRetriesAway(t *testing.T) {
	t.Helper()

	old := renewalRetryInterval
	renewalRetryInterval = time.Hour
	t.Cleanup(func() { renewalRetryInterval = old })
}

func newSchedulerTestConfigHandler() *config.Handler {
	return config.Register(context.Background(), "", "", "rancher-fip-manager-webhook", "rancher-fip-manager")
}

// TestStartCertRenewalSchedulerDoesNotPanicWithoutMaterial pins the
// REPORT.md 2.1/2.2 steady state: previously an empty or absent certificate
// made the scheduler log.Panicf at startup, permanently CrashLoopBackOff-ing
// the webhook with only a manual Secret deletion as recovery. Now it must
// return and keep retrying.
func TestStartCertRenewalSchedulerDoesNotPanicWithoutMaterial(t *testing.T) {
	keepRetriesAway(t)
	t.Setenv("HOME", t.TempDir())

	sHandler := &service.Handler{}
	cHandler := newSchedulerTestConfigHandler()

	require.NotPanics(t, func() {
		StartCertRenewalScheduler(cHandler, sHandler, 60*24)
	})
}

// TestStartCertRenewalSchedulerSchedulesValidCert pins the healthy path: a
// readable serving certificate schedules the ticker normally.
func TestStartCertRenewalSchedulerSchedulesValidCert(t *testing.T) {
	keepRetriesAway(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "rancher-fip-manager-webhook.rancher-fip-manager.svc"},
		DNSNames:  []string{"rancher-fip-manager-webhook.rancher-fip-manager.svc"},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644))

	sHandler := &service.Handler{}
	cHandler := newSchedulerTestConfigHandler()

	require.NotPanics(t, func() {
		StartCertRenewalScheduler(cHandler, sHandler, 60*24)
	})
	assert.NotNil(t, ticker, "healthy certificate must arm the renewal ticker")
	ticker.Stop()
}
