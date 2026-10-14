package config

import (
	"context"
	"fmt"

	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/util"
	log "github.com/sirupsen/logrus"

	"k8s.io/client-go/kubernetes"
)

type Handler struct {
	ctx               context.Context
	kubeConfig        string
	kubeContext       string
	clientset         kubernetes.Interface
	webhookNamespace  string
	webhookName       string
	webhookSecretName string
	csrName           string
}

func Register(ctx context.Context, kubeConfig string, kubeContext string, webhookName string, webhookNamespace string) *Handler {
	return &Handler{
		ctx:              ctx,
		kubeConfig:       kubeConfig,
		kubeContext:      kubeContext,
		webhookName:      webhookName,
		webhookNamespace: webhookNamespace,
	}
}

func (h *Handler) Init() {
	config, err := util.GetKubeConfig(h.kubeConfig, h.kubeContext)
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panicf("%s", err.Error())
	}
	h.clientset = clientset

	h.webhookSecretName = fmt.Sprintf("%s-tls", h.webhookName)
	h.csrName = fmt.Sprintf("%s.%s.svc", h.webhookName, h.webhookNamespace)
}

// Run ensures the webhook TLS secret and the on-disk serving material exist,
// renewing the certificate when it is missing, expired, near expiry or
// unusable. It returns renewed=true when fresh TLS material was written.
//
// Errors are returned for the caller to log/retry: Run never panics, never
// regenerates material while the secret merely failed to read (transient API
// error), and never deletes the existing secret before a fully signed
// replacement is in hand. A slow CSR signer or an API blip therefore costs a
// retry on the next scheduler tick instead of bricking the webhook.
func (h *Handler) Run(certRenewalPeriod int64) (renewed bool, err error) {
	secretExists, err := h.checkSecret()
	if err != nil {
		return false, err
	}

	if !secretExists {
		csrExists, err := h.checkCSR()
		if err != nil {
			return false, err
		}
		if csrExists {
			if err := h.deleteCSR(); err != nil {
				return false, fmt.Errorf("error deleting stale certificate signing request: %s", err.Error())
			}
		}

		tlsPair, err := h.generateTLSKeyAndCert()
		if err != nil {
			return false, fmt.Errorf("error generating TLS key and certificate: %s", err.Error())
		}

		if err := h.createSecret(tlsPair); err != nil {
			return false, fmt.Errorf("error creating webhook TLS secret: %s", err.Error())
		}

		renewed = true
	} else if h.checkCertExpireDate(certRenewalPeriod) {
		if err := h.renewTLSPair(); err != nil {
			return false, fmt.Errorf("error renewing webhook TLS pair: %s", err.Error())
		}

		renewed = true
	}

	if err := h.writeTLSDataFromSecret(); err != nil {
		return renewed, err
	}

	return renewed, nil
}
