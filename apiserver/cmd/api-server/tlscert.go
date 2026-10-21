package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// tlsCertReloadInterval bounds how often a handshake stats the cert/key
// files. The certificate is cert-manager-managed and renewed ~30 days
// before expiry, so a 30s detection lag is irrelevant; the throttle just
// keeps the two stat syscalls off the hot path of every handshake.
const tlsCertReloadInterval = 30 * time.Second

// certReloader serves the TLS key pair from disk and re-reads it when the
// files change, so a cert-manager renewal reaches the running process
// without a pod restart. kubelet updates the secret volume
// with an atomic symlink swap, so os.Stat sees either the old or the new
// file, never a partial write.
type certReloader struct {
	log           *logrus.Logger
	certFile      string
	keyFile       string
	checkInterval time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	certStamp fileStamp
	keyStamp  fileStamp
	lastCheck time.Time
}

// fileStamp identifies one version of a file. mtime alone is not enough:
// a same-second rewrite on a coarse filesystem would be missed, and size
// changes catch that.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func statStamp(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modTime: fi.ModTime(), size: fi.Size()}, nil
}

// newCertReloader loads the key pair once at startup so a missing or
// malformed certificate fails the process instead of every handshake.
func newCertReloader(log *logrus.Logger, certFile, keyFile string) (*certReloader, error) {
	cert, certStamp, keyStamp, err := loadTLSPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	r := &certReloader{
		log:           log,
		certFile:      certFile,
		keyFile:       keyFile,
		checkInterval: tlsCertReloadInterval,
		cert:          cert,
		certStamp:     certStamp,
		keyStamp:      keyStamp,
		lastCheck:     time.Now(),
	}
	r.logCertificate("serving TLS certificate", cert)
	return r, nil
}

// GetCertificate is the tls.Config hook: it returns the cached key pair,
// first re-reading the files when they changed since the last check.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if now := time.Now(); now.Sub(r.lastCheck) >= r.checkInterval {
		r.lastCheck = now
		if err := r.reloadIfChangedLocked(); err != nil {
			// Keep serving the cached certificate: a transient read error
			// (or a straddled cert/key update) must not fail handshakes
			// while a still-valid certificate is in hand.
			r.log.Warnf("TLS certificate reload failed, keeping the cached certificate: %v", err)
		}
	}
	if r.cert == nil {
		return nil, errors.New("no TLS certificate loaded")
	}
	return r.cert, nil
}

// reloadIfChangedLocked reloads only when the file stamps differ from the
// ones the cached certificate was loaded from. The stamps are recorded
// only after a successful load, so a failed attempt is retried on the next
// check instead of being remembered as done.
func (r *certReloader) reloadIfChangedLocked() error {
	certStamp, err := statStamp(r.certFile)
	if err != nil {
		return fmt.Errorf("stat TLS certificate: %w", err)
	}
	keyStamp, err := statStamp(r.keyFile)
	if err != nil {
		return fmt.Errorf("stat TLS key: %w", err)
	}
	if certStamp == r.certStamp && keyStamp == r.keyStamp {
		return nil
	}

	cert, certStamp, keyStamp, err := loadTLSPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert = cert
	r.certStamp = certStamp
	r.keyStamp = keyStamp
	r.logCertificate("reloaded TLS certificate", cert)
	return nil
}

// loadTLSPair reads the key pair together with the stamps of exactly the
// version that was read. The pre/post stat comparison retries when the
// files are swapped mid-read, which could otherwise pair a new certificate
// with an old key (LoadX509KeyPair rejects a mismatch, but a cert+key pair
// from two different renewals would be a hard handshake failure until the
// next check).
func loadTLSPair(certFile, keyFile string) (*tls.Certificate, fileStamp, fileStamp, error) {
	for attempt := 0; attempt < 3; attempt++ {
		certStamp, err := statStamp(certFile)
		if err != nil {
			return nil, fileStamp{}, fileStamp{}, fmt.Errorf("stat TLS certificate: %w", err)
		}
		keyStamp, err := statStamp(keyFile)
		if err != nil {
			return nil, fileStamp{}, fileStamp{}, fmt.Errorf("stat TLS key: %w", err)
		}

		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fileStamp{}, fileStamp{}, fmt.Errorf("load TLS key pair: %w", err)
		}

		certStampAfter, err := statStamp(certFile)
		if err != nil {
			return nil, fileStamp{}, fileStamp{}, fmt.Errorf("stat TLS certificate: %w", err)
		}
		keyStampAfter, err := statStamp(keyFile)
		if err != nil {
			return nil, fileStamp{}, fileStamp{}, fmt.Errorf("stat TLS key: %w", err)
		}
		if certStamp == certStampAfter && keyStamp == keyStampAfter {
			return &cert, certStamp, keyStamp, nil
		}
	}
	return nil, fileStamp{}, fileStamp{}, errors.New("TLS certificate files changed while being read")
}

// logCertificate records the leaf identity and expiry, so a stale leaf is
// visible in the log without inspecting the volume.
func (r *certReloader) logCertificate(msg string, cert *tls.Certificate) {
	if len(cert.Certificate) == 0 {
		r.log.Warnf("%s: empty certificate chain", msg)
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		r.log.Warnf("%s: cannot parse leaf certificate: %v", msg, err)
		return
	}
	r.log.Infof("%s: subject=%q serial=%s notBefore=%s notAfter=%s",
		msg, leaf.Subject, leaf.SerialNumber,
		leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	if now := time.Now(); now.After(leaf.NotAfter) {
		r.log.Warnf("serving TLS certificate expired at %s", leaf.NotAfter.Format(time.RFC3339))
	}
}
