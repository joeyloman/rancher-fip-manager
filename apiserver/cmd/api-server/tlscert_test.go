package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// writeTLSPair writes a self-signed serving key pair with the given serial
// into dir, replacing any previous files, and gives both files a distinct
// mtime so the reloader's change detection cannot be fooled by a
// same-timestamp rewrite.
func writeTLSPair(t *testing.T, dir string, serial int64, stamp time.Time) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %s", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "rancher-fip-api-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"rancher-fip-api-server"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %s", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %s", err)
	}

	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write certificate: %s", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %s", err)
	}
	for _, path := range []string{certPath, keyPath} {
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %s", path, err)
		}
	}
	return certPath, keyPath
}

func serialOf(t *testing.T, cert *tls.Certificate) int64 {
	t.Helper()
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("no certificate returned")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %s", err)
	}
	return leaf.SerialNumber.Int64()
}

func testLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// A renewal rewrites the mounted files; the next handshake must serve the
// new leaf without a restart.
func TestCertReloaderPicksUpRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	certPath, keyPath := writeTLSPair(t, dir, 1, base)

	r, err := newCertReloader(testLogger(), certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %s", err)
	}
	r.checkInterval = 0

	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %s", err)
	}
	if got := serialOf(t, cert); got != 1 {
		t.Fatalf("initial serial = %d, want 1", got)
	}

	writeTLSPair(t, dir, 2, base.Add(time.Minute))

	cert, err = r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %s", err)
	}
	if got := serialOf(t, cert); got != 2 {
		t.Fatalf("serial after rotation = %d, want 2", got)
	}
}

// A broken update (partial write, cert/key mismatch) must not break the
// running server, and the reload must be retried once the files are valid.
func TestCertReloaderKeepsCachedCertificateOnBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	certPath, keyPath := writeTLSPair(t, dir, 1, base)

	r, err := newCertReloader(testLogger(), certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %s", err)
	}
	r.checkInterval = 0

	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupt certificate: %s", err)
	}
	if err := os.Chtimes(certPath, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatalf("chtimes: %s", err)
	}

	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate with broken files: %s", err)
	}
	if got := serialOf(t, cert); got != 1 {
		t.Fatalf("serial with broken files = %d, want the cached 1", got)
	}

	writeTLSPair(t, dir, 3, base.Add(2*time.Minute))
	cert, err = r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after repair: %s", err)
	}
	if got := serialOf(t, cert); got != 3 {
		t.Fatalf("serial after repair = %d, want 3", got)
	}
}

// The wiring main.go uses — server.TLSConfig.GetCertificate plus
// ServeTLS(ln, "", "") — must serve the rotated certificate to a real TLS
// client, including after a renewal while the server keeps running.
func TestServeTLSReloadsCertificateWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	certPath, keyPath := writeTLSPair(t, dir, 10, base)

	r, err := newCertReloader(testLogger(), certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %s", err)
	}
	r.checkInterval = 0

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	addr := ln.Addr().String()

	server := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }),
		TLSConfig: &tls.Config{GetCertificate: r.GetCertificate},
	}
	go func() { _ = server.ServeTLS(ln, "", "") }()
	defer server.Close()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		},
	}
	fetch := func() int64 {
		t.Helper()
		resp, err := client.Get("https://" + addr + "/")
		if err != nil {
			t.Fatalf("GET: %s", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("GET: status %d body %q", resp.StatusCode, body)
		}
		return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
	}

	if got := fetch(); got != 10 {
		t.Fatalf("initial served serial = %d, want 10", got)
	}

	writeTLSPair(t, dir, 11, base.Add(time.Minute))
	if got := fetch(); got != 11 {
		t.Fatalf("served serial after rotation = %d, want 11", got)
	}
}

func TestNewCertReloaderFailsOnMissingFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := newCertReloader(testLogger(), filepath.Join(dir, "absent.crt"), filepath.Join(dir, "absent.key")); err == nil {
		t.Fatal("newCertReloader accepted missing files")
	}
}
