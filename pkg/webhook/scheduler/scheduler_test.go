package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"sync"
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
// steady state: previously an empty or absent certificate
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

// --- fakes for the tick paths (no API server needed) ---

type fakeCertSource struct {
	mu        sync.Mutex
	runCalls  int
	runOK     bool
	runErr    error
	certPair  *tls.Certificate
	certErr   error
	expire    time.Time
	expireErr error
}

func (f *fakeCertSource) Run(certRenewalPeriod int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls++

	return f.runOK, f.runErr
}

func (f *fakeCertSource) ServingCertificate() (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.certPair, f.certErr
}

func (f *fakeCertSource) GetCertExpireDate() (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.expire, f.expireErr
}

type fakeServer struct {
	mu        sync.Mutex
	runCalls  int
	stopCalls int
	setCalls  int
	serving   bool
	lastPair  *tls.Certificate
}

func (f *fakeServer) Run() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls++
	f.serving = true
}

func (f *fakeServer) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.serving = false

	return nil
}

func (f *fakeServer) IsServing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.serving
}

func (f *fakeServer) SetCertificate(pair *tls.Certificate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.lastPair = pair
}

func TestRenewalCheckIntervalMinutes(t *testing.T) {
	const (
		day   int64 = 24 * 60
		month int64 = 30 * 24 * 60
	)

	cases := []struct {
		name        string
		minutesLeft int64
		period      int64
		want        int64
	}{
		// The raw schedule (lifetime − period + 1) must be
		// bounded by min(lifetime/3, period).
		{"year cert with 30d period checks monthly", 365 * day, month, month},
		{"period bound beats third of lifetime", 365 * day, 1, 1},
		{"third of lifetime bounds late raw value", 45, 10, 10},
		{"near expiry with oversized period floors at one", 120, month, 1},
		{"expired cert ticks at one minute", -5, month, 1},
		{"exactly at renewal window", 60, 60, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, renewalCheckIntervalMinutes(c.minutesLeft, c.period))
		})
	}
}

// TestRenewCertificateRotatesInPlace pins the in-place rotation contract: a successful
// renewal must swap the certificate into the live listener — the admission
// endpoint is never stopped, so no write-blackout window is created.
func TestRenewCertificateRotatesInPlace(t *testing.T) {
	pair := &tls.Certificate{}
	c := &fakeCertSource{runOK: true, certPair: pair}
	s := &fakeServer{serving: true}

	renewCertificate(c, s, 60)

	assert.Equal(t, 1, c.runCalls)
	assert.Equal(t, 1, s.setCalls)
	assert.Same(t, pair, s.lastPair)
	assert.Zero(t, s.stopCalls, "renewal must not tear down the admission listener")
	assert.Zero(t, s.runCalls, "renewal must not restart the HTTPS server")
}

// TestRenewCertificateRebindsWhenNotServing pins the recovery path: a dead
// listener (never bound or died) is re-armed, and a Run failure does not
// stop the push attempt; the Stop/Run pair is the ONLY way the server is
// bounced.
func TestRenewCertificateRebindsWhenNotServing(t *testing.T) {
	old := serviceRestartDelay
	serviceRestartDelay = time.Millisecond
	t.Cleanup(func() { serviceRestartDelay = old })

	c := &fakeCertSource{runErr: errors.New("signer down"), certErr: errors.New("no material")}
	s := &fakeServer{serving: false}

	renewCertificate(c, s, 60)

	assert.Equal(t, 1, s.stopCalls)
	assert.Zero(t, s.setCalls, "unusable material must not be pushed")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.runCalls == 1
	}, 2*time.Second, 5*time.Millisecond, "dead listener must be rebound")
}
