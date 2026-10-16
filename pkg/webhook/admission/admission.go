package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/util"
	log "github.com/sirupsen/logrus"
	admregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Explicit admission control knobs (REPORT.md 2.5/5.1): failurePolicy and
// timeoutSeconds were never set, so the API defaults (Fail / 10s) applied
// invisibly. Pin the intent in the object: Fail is deliberate — an
// unreachable webhook must not wave through FloatingIP/Pool writes that
// would double-allocate — and the 10s timeout is the bound the webhook's
// own request path (pool/quota Gets plus the bounded live FloatingIP
// list) must fit inside.
const (
	admissionTimeoutSeconds int32 = 10

	// REPORT.md M9: a kube-root-ca.crt Get failure during startup used to
	// panic immediately, crashlooping the webhook on a pure API blip even
	// though it could serve fine. Retry the admission setup with backoff
	// and panic only on sustained failure (kubeconfig/clientset errors are
	// deterministic local configuration problems and stay fatal).
	addWebhookConfigMaxRetries     = 5
	addWebhookConfigRetryBaseDelay = 1 * time.Second
	addWebhookConfigRetryMaxDelay  = 16 * time.Second
)

func setAdmissionPolicy(webhook *admregv1.ValidatingWebhook) {
	webhook.FailurePolicy = new(admregv1.Fail)
	webhook.TimeoutSeconds = new(admissionTimeoutSeconds)
}

type Handler struct {
	ctx                         context.Context
	kubeConfig                  string
	kubeContext                 string
	clientset                   kubernetes.Interface
	webhookNamespace            string
	webhookName                 string
	validatingWebhookConfigName string
}

func Register(ctx context.Context, kubeConfig string, kubeContext string, webhookName string, webhookNamespace string, validatingWebhookConfigName string) *Handler {
	return &Handler{
		ctx:                         ctx,
		kubeConfig:                  kubeConfig,
		kubeContext:                 kubeContext,
		webhookName:                 webhookName,
		webhookNamespace:            webhookNamespace,
		validatingWebhookConfigName: validatingWebhookConfigName,
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

	if err := retryWithBackoff(h.ctx, addWebhookConfigMaxRetries, addWebhookConfigRetryBaseDelay, addWebhookConfigRetryMaxDelay, h.AddValidatingWebhookConfiguration); err != nil {
		log.Panicf("%s", err.Error())
	}
}

// retryWithBackoff runs fn with exponential backoff (baseDelay doubling up
// to maxDelay) until it succeeds or maxRetries attempts are exhausted. A
// cancelled context aborts immediately instead of burning the budget, so a
// SIGTERM during startup is not stalled by the retry loop.
func retryWithBackoff(ctx context.Context, maxRetries int, baseDelay time.Duration, maxDelay time.Duration, fn func() error) error {
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else if attempt >= maxRetries {
			return fmt.Errorf("giving up after %d attempts: %s", attempt, err.Error())
		} else {
			log.Errorf("attempt %d/%d failed, retrying in %s: %s", attempt, maxRetries, delay, err.Error())
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
		}
	}
}

func (h *Handler) getRancherFloatingIPWebhook() (webhook admregv1.ValidatingWebhook, err error) {
	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		return
	}

	webhook.Name = fmt.Sprintf("floatingip-%s.%s.svc", h.webhookName, h.webhookNamespace)

	matchLabels := make(map[string]string)
	matchLabels["admission-webhook"] = "enabled"
	nameSpaceSelector := metav1.LabelSelector{}
	webhook.NamespaceSelector = &nameSpaceSelector

	var rules []admregv1.RuleWithOperations

	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"rancher.k8s.binbash.org"}
	rule.APIVersions = []string{"v1beta2", "v1beta1"}
	rule.Operations = []admregv1.OperationType{"CREATE", "UPDATE"}
	rule.Resources = []string{"floatingips"}
	scope := admregv1.NamespacedScope
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	clientconfig := admregv1.WebhookClientConfig{}
	serviceref := admregv1.ServiceReference{}
	serviceref.Namespace = h.webhookNamespace
	serviceref.Name = h.webhookName
	path := "/validate-floatingip"
	serviceref.Path = &path
	port := int32(8443)
	serviceref.Port = &port
	clientconfig.Service = &serviceref
	clientconfig.CABundle = []byte(cert)
	webhook.ClientConfig = clientconfig

	webhook.AdmissionReviewVersions = []string{"v1"}
	setAdmissionPolicy(&webhook)

	return
}

func (h *Handler) getRancherFloatingIPPoolWebhook() (webhook admregv1.ValidatingWebhook, err error) {
	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		return
	}

	webhook.Name = fmt.Sprintf("floatingippool-%s.%s.svc", h.webhookName, h.webhookNamespace)

	matchLabels := make(map[string]string)
	matchLabels["admission-webhook"] = "enabled"
	nameSpaceSelector := metav1.LabelSelector{}
	webhook.NamespaceSelector = &nameSpaceSelector

	var rules []admregv1.RuleWithOperations

	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"rancher.k8s.binbash.org"}
	rule.APIVersions = []string{"v1beta2", "v1beta1"}
	rule.Operations = []admregv1.OperationType{"CREATE", "UPDATE"}
	rule.Resources = []string{"floatingippools"}
	scope := admregv1.ClusterScope
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	clientconfig := admregv1.WebhookClientConfig{}
	serviceref := admregv1.ServiceReference{}
	serviceref.Namespace = h.webhookNamespace
	serviceref.Name = h.webhookName
	path := "/validate-floatingippool"
	serviceref.Path = &path
	port := int32(8443)
	serviceref.Port = &port
	clientconfig.Service = &serviceref
	clientconfig.CABundle = []byte(cert)
	webhook.ClientConfig = clientconfig

	webhook.AdmissionReviewVersions = []string{"v1"}
	setAdmissionPolicy(&webhook)

	return
}

func (h *Handler) AddValidatingWebhookConfiguration() (err error) {
	existing, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(h.ctx, h.validatingWebhookConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return h.createValidatingWebhookConfiguration()
	}
	if err != nil {
		return fmt.Errorf("error checking validating webhook configuration: %s", err.Error())
	}

	// A configuration created by an older release predates the explicit
	// policy fields; reconcile them so upgrades are not silent no-ops.
	return h.syncAdmissionPolicy(existing)
}

func (h *Handler) createValidatingWebhookConfiguration() (err error) {
	vwc := admregv1.ValidatingWebhookConfiguration{}
	vwc.ObjectMeta.Name = h.validatingWebhookConfigName

	rancherFloatingIPWebhook, err := h.getRancherFloatingIPWebhook()
	if err != nil {
		return
	}
	vwc.Webhooks = append(vwc.Webhooks, rancherFloatingIPWebhook)

	rancherFloatingIPPoolWebhook, err := h.getRancherFloatingIPPoolWebhook()
	if err != nil {
		return
	}
	vwc.Webhooks = append(vwc.Webhooks, rancherFloatingIPPoolWebhook)

	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(h.ctx, &vwc, metav1.CreateOptions{})

	return
}

// syncAdmissionPolicy pins failurePolicy/TimeoutSeconds on a pre-existing
// ValidatingWebhookConfiguration and fills an EMPTY caBundle from the
// kube-root-ca.crt configmap. A non-empty bundle is never replaced: it is
// operator-owned, and kube-root-ca.crt is the apiserver CA while the
// serving cert may be signed by a different cluster CA — swapping a
// working bundle under failurePolicy=Fail would deny all FloatingIP/Pool
// writes. A failed CA read is likewise non-fatal here (the existing bundle
// already validates the serving cert; failing startup would relocate the
// M9 crashloop to this path). The create path keeps the read mandatory: a
// fresh VWC without a caBundle can never validate. True rotation support
// remains REPORT.md 2.8, out of scope.
func (h *Handler) syncAdmissionPolicy(existing *admregv1.ValidatingWebhookConfiguration) error {
	changed := false

	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		log.Warnf("skipping caBundle sync on validating webhook configuration %s, keeping existing bundle: %s", h.validatingWebhookConfigName, err.Error())
	} else {
		for i := range existing.Webhooks {
			webhook := &existing.Webhooks[i]
			if len(webhook.ClientConfig.CABundle) == 0 {
				webhook.ClientConfig.CABundle = []byte(cert)
				changed = true
			}
		}
	}

	for i := range existing.Webhooks {
		webhook := &existing.Webhooks[i]
		if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admregv1.Fail {
			webhook.FailurePolicy = new(admregv1.Fail)
			changed = true
		}
		if webhook.TimeoutSeconds == nil || *webhook.TimeoutSeconds != admissionTimeoutSeconds {
			webhook.TimeoutSeconds = new(admissionTimeoutSeconds)
			changed = true
		}
	}

	if !changed {
		return nil
	}

	if _, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(h.ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error syncing validating webhook configuration: %s", err.Error())
	}
	log.Infof("synced failurePolicy=Fail timeoutSeconds=%d caBundle on validating webhook configuration %s", admissionTimeoutSeconds, h.validatingWebhookConfigName)

	return nil
}
