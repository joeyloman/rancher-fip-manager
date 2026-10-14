package config

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// CSR signing wait tunables; overridable in tests.
var (
	// csrSigningTimeout bounds how long createAndSignCSR waits for the CSR
	// signer to populate Status.Certificate after approval.
	csrSigningTimeout = 30 * time.Second
	// csrPollInterval is the poll period while waiting for the CSR to sign.
	csrPollInterval = 500 * time.Millisecond
)

// errCertUnusable marks certificate material that is present but cannot be
// served (empty, undecodable, unparseable, or missing from disk). Renewal
// treats it as must-renew; the scheduler treats it as "retry soon".
// Transient API errors NEVER carry this sentinel.
var errCertUnusable = errors.New("unusable webhook certificate")

func certFilePath() string {
	return fmt.Sprintf("%s/tls.crt", os.Getenv("HOME"))
}

func keyFilePath() string {
	return fmt.Sprintf("%s/tls.key", os.Getenv("HOME"))
}

func (h *Handler) generateTLSKeyAndCert() (tlsPair tls.Certificate, err error) {
	var DNSnames []string

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tlsPair, fmt.Errorf("error while generating key: %s", err.Error())
	}

	cn := fmt.Sprintf("system:node:%s.%s.svc", h.webhookName, h.webhookNamespace)
	DNSnames = append(DNSnames, h.webhookName)
	DNSnames = append(DNSnames, fmt.Sprintf("%s.%s", h.webhookName, h.webhookNamespace))
	DNSnames = append(DNSnames, h.csrName)
	DNSnames = append(DNSnames, fmt.Sprintf("%s.%s.cluster.local", h.webhookName, h.webhookNamespace))

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"system:nodes"},
		},
		SignatureAlgorithm: x509.SHA256WithRSA,
		DNSNames:           DNSnames,
	}

	bCsr, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return tlsPair, fmt.Errorf("error while creating certificate request: %s", err.Error())
	}
	pCsr := pem.EncodeToMemory(
		&pem.Block{
			Type:  "CERTIFICATE REQUEST",
			Bytes: bCsr,
		},
	)

	cert, err := h.createAndSignCSR(pCsr)
	if err != nil {
		return
	}
	// Status.Certificate is PEM material (same form the serving files carry);
	// reject material we could never serve instead of writing it to the secret.
	if _, err := certNotAfter(cert); err != nil {
		return tlsPair, fmt.Errorf("certificate returned by the signer for %s is unusable: %s", h.csrName, err.Error())
	}

	tlsPair.Certificate = append(tlsPair.Certificate, cert)
	tlsPair.PrivateKey = key

	return
}

func (h *Handler) checkCSR() (bool, error) {
	_, err := h.getCSR()
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error checking certificate signing request: %s", err.Error())
	}

	return true, nil
}

func (h *Handler) getCSR() (*certsv1.CertificateSigningRequest, error) {
	return h.clientset.CertificatesV1().CertificateSigningRequests().Get(context.TODO(), h.csrName, metav1.GetOptions{})
}

func (h *Handler) deleteCSR() error {
	return h.clientset.CertificatesV1().CertificateSigningRequests().Delete(context.TODO(), h.csrName, metav1.DeleteOptions{})
}

func (h *Handler) createAndSignCSR(pCsr []byte) ([]byte, error) {
	newCsrObj := certsv1.CertificateSigningRequest{}
	newCsrObj.ObjectMeta.Name = h.csrName
	newCsrObj.Spec.Groups = []string{"system:authenticated"}
	newCsrObj.Spec.Request = pCsr
	newCsrObj.Spec.SignerName = "kubernetes.io/kubelet-serving"
	newCsrObj.Spec.Usages = []certsv1.KeyUsage{
		certsv1.UsageDigitalSignature,
		certsv1.UsageKeyEncipherment,
		certsv1.UsageServerAuth,
	}
	csrObj, err := h.clientset.CertificatesV1().CertificateSigningRequests().Create(context.TODO(), &newCsrObj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error while creating signing request: %s", err.Error())
	}

	approval := certsv1.CertificateSigningRequest{
		Status: certsv1.CertificateSigningRequestStatus{
			Conditions: []certsv1.CertificateSigningRequestCondition{{
				Type:           certsv1.CertificateApproved,
				Status:         corev1.ConditionTrue,
				Reason:         "Approved by TLS Service",
				Message:        "KubeTLS Approved",
				LastUpdateTime: metav1.Now(),
			}},
		},
	}
	approval.ObjectMeta = csrObj.ObjectMeta
	_, err = h.clientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(context.TODO(), h.csrName, &approval, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error while approving signing request: %s", err.Error())
	}

	// The signer populates Status.Certificate asynchronously; a fixed sleep
	// raced it and handed back an empty certificate, which previously bricked
	// the webhook. Poll until the certificate shows up and fail instead.
	var cert []byte
	err = wait.PollUntilContextTimeout(context.TODO(), csrPollInterval, csrSigningTimeout, true,
		func(ctx context.Context) (bool, error) {
			updatedCsr, err := h.clientset.CertificatesV1().CertificateSigningRequests().Get(ctx, h.csrName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, err
				}

				return false, nil
			}

			for _, condition := range updatedCsr.Status.Conditions {
				if condition.Type == certsv1.CertificateDenied {
					return false, fmt.Errorf("signing request was denied: %s", condition.Message)
				}
			}

			if len(updatedCsr.Status.Certificate) > 0 {
				cert = updatedCsr.Status.Certificate

				return true, nil
			}

			return false, nil
		})
	if err != nil {
		return nil, fmt.Errorf("error waiting for signing request %s to be signed: %s", h.csrName, err.Error())
	}

	return cert, nil
}

func (h *Handler) getTLSDataFromSecret() (tlsPair tls.Certificate, err error) {
	s, err := h.getSecret()
	if err != nil {
		return tlsPair, fmt.Errorf("cannot fetch webhook TLS secret: %s", err.Error())
	}

	cert, exists := s.Data["tls.crt"]
	if !exists || len(cert) == 0 {
		return tlsPair, fmt.Errorf("%w: tls.crt not found in secret or empty", errCertUnusable)
	}
	tlsPair.Certificate = append(tlsPair.Certificate, cert)

	key, exists := s.Data["tls.key"]
	if !exists || len(key) == 0 {
		return tlsPair, fmt.Errorf("%w: tls.key not found in secret or empty", errCertUnusable)
	}
	tlsPair.PrivateKey = key

	return
}

func (h *Handler) writeTLSDataFromSecret() (err error) {
	tlsPair, err := h.getTLSDataFromSecret()
	if err != nil {
		return fmt.Errorf("error while fetching TLS data: %s", err.Error())
	}

	if err = os.WriteFile(keyFilePath(), []byte(fmt.Sprintf("%s", tlsPair.PrivateKey)), 0600); err != nil {
		return fmt.Errorf("error while writing private key file: %s", err.Error())
	}

	if err = os.WriteFile(certFilePath(), tlsPair.Certificate[0], 0644); err != nil {
		return fmt.Errorf("error while writing certificate file: %s", err.Error())
	}

	return
}

func (h *Handler) renewTLSPair() (err error) {
	csrExists, err := h.checkCSR()
	if err != nil {
		return
	}
	if csrExists {
		if err = h.deleteCSR(); err != nil {
			return
		}
	}

	// Generate the replacement material first: the existing secret is never
	// touched until a fully signed certificate is in hand, so a slow or
	// failing signer can no longer destroy the only valid TLS material.
	tlsPair, err := h.generateTLSKeyAndCert()
	if err != nil {
		return
	}

	return h.updateSecret(tlsPair)
}

// certNotAfter parses PEM certificate material and returns its expiry.
// Material that is present but cannot be understood is reported with the
// errCertUnusable sentinel so callers can distinguish it from API errors.
func certNotAfter(pemCert []byte) (time.Time, error) {
	if len(pemCert) == 0 {
		return time.Time{}, fmt.Errorf("%w: certificate is empty", errCertUnusable)
	}
	b, _ := pem.Decode(pemCert)
	if b == nil {
		return time.Time{}, fmt.Errorf("%w: cannot decode TLS PEM data", errCertUnusable)
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: cannot parse TLS PEM data: %s", errCertUnusable, err.Error())
	}

	return cert.NotAfter, nil
}

// GetCertExpireDate reports the expiry of the certificate the HTTPS server
// actually serves, i.e. the on-disk material. A missing or broken file is
// reported with the errCertUnusable sentinel; the scheduler retries the
// renewal pipeline instead of panicking.
func (h *Handler) GetCertExpireDate() (expireDate time.Time, err error) {
	certPath := certFilePath()

	pemCert, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: cannot read certificate file %s: %s", errCertUnusable, certPath, err.Error())
	}

	return certNotAfter(pemCert)
}

// checkCertExpireDate reports whether the certificate stored in the webhook
// secret must be renewed. Unusable material (empty, undecodable or
// unparseable — e.g. left behind by a slow CSR signer) counts as must-renew,
// so it is repaired automatically instead of wedging the renewal scheduler.
// Transient API errors count as "not now" and are retried on a later tick.
func (h *Handler) checkCertExpireDate(certRenewalPeriod int64) bool {
	tlsPair, err := h.getTLSDataFromSecret()
	if err != nil {
		if errors.Is(err, errCertUnusable) {
			log.Errorf("webhook TLS material is unusable, forcing renewal: %s", err.Error())

			return true
		}
		log.Errorf("cannot read webhook TLS secret, skipping renewal this cycle: %s", err.Error())

		return false
	}

	expireDate, err := certNotAfter(tlsPair.Certificate[0])
	if err != nil {
		log.Errorf("webhook TLS material is unusable, forcing renewal: %s", err.Error())

		return true
	}

	currentDate := time.Now().UTC()
	difference := expireDate.Sub(currentDate)

	return int64(difference.Minutes()) < certRenewalPeriod
}
