package admission

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testWebhookName = "rancher-fip-manager-webhook"
	testWebhookNS   = "rancher-fip-manager"
	testVWCName     = "rancher-fip-manager-validator"
)

func newTestHandler(t *testing.T, objects ...runtime.Object) (*Handler, *fake.Clientset) {
	t.Helper()

	caCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-system"},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\ndmFsdWUK-----END CERTIFICATE-----\n"},
	}
	client := fake.NewSimpleClientset(append([]runtime.Object{caCM}, objects...)...)

	h := Register(context.Background(), "", "", testWebhookName, testWebhookNS, testVWCName)
	h.clientset = client

	return h, client
}

// TestBuildersPinFailurePolicyAndTimeout pins REPORT.md 2.5/5.1: the two
// knobs were never set, so the API applied Fail/10s invisibly and any
// operator reading the code could not tell the policy from an accident.
func TestBuildersPinFailurePolicyAndTimeout(t *testing.T) {
	h, _ := newTestHandler(t)

	fipWebhook, err := h.getRancherFloatingIPWebhook()
	require.NoError(t, err)
	require.NotNil(t, fipWebhook.FailurePolicy)
	assert.Equal(t, admregv1.Fail, *fipWebhook.FailurePolicy)
	require.NotNil(t, fipWebhook.TimeoutSeconds)
	assert.Equal(t, admissionTimeoutSeconds, *fipWebhook.TimeoutSeconds)

	poolWebhook, err := h.getRancherFloatingIPPoolWebhook()
	require.NoError(t, err)
	require.NotNil(t, poolWebhook.FailurePolicy)
	assert.Equal(t, admregv1.Fail, *poolWebhook.FailurePolicy)
	require.NotNil(t, poolWebhook.TimeoutSeconds)
	assert.Equal(t, admissionTimeoutSeconds, *poolWebhook.TimeoutSeconds)
}

func TestAddCreatesVWCWithPinnedPolicy(t *testing.T) {
	h, client := newTestHandler(t)

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	vwc, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), testVWCName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, vwc.Webhooks, 2)

	for _, webhook := range vwc.Webhooks {
		require.NotNil(t, webhook.FailurePolicy, "webhook %s created without explicit failurePolicy", webhook.Name)
		assert.Equal(t, admregv1.Fail, *webhook.FailurePolicy)
		require.NotNil(t, webhook.TimeoutSeconds, "webhook %s created without explicit timeoutSeconds", webhook.Name)
		assert.Equal(t, admissionTimeoutSeconds, *webhook.TimeoutSeconds)
	}
}

// TestAddSyncsPolicyOnPreExistingVWC: an already-installed cluster carries
// a VWC built by the old code (no policy fields). Add must pin them, or
// "explicit failurePolicy/TimeoutSeconds" would only ever apply to fresh
// installs.
func TestAddSyncsPolicyOnPreExistingVWC(t *testing.T) {
	legacy := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testVWCName},
		Webhooks: []admregv1.ValidatingWebhook{
			{Name: "legacy-floatingip"},
			{Name: "legacy-floatingippool", FailurePolicy: new(admregv1.Ignore)},
		},
	}
	h, client := newTestHandler(t, legacy)

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	got, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), testVWCName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Webhooks, 2)

	for _, webhook := range got.Webhooks {
		require.NotNil(t, webhook.FailurePolicy)
		assert.Equal(t, admregv1.Fail, *webhook.FailurePolicy, "webhook %s must be pinned to Fail", webhook.Name)
		require.NotNil(t, webhook.TimeoutSeconds)
		assert.Equal(t, admissionTimeoutSeconds, *webhook.TimeoutSeconds, "webhook %s must be pinned to the timeout", webhook.Name)
	}
}

func TestAddNoUpdateWhenAlreadyPinned(t *testing.T) {
	pinned := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testVWCName},
		Webhooks: []admregv1.ValidatingWebhook{
			{
				Name:          "floatingip",
				FailurePolicy: new(admregv1.Fail), TimeoutSeconds: new(admissionTimeoutSeconds),
				// Non-empty: REPORT.md M9 fills empty caBundles once, so
				// the no-update contract now requires a populated bundle.
				ClientConfig: admregv1.WebhookClientConfig{CABundle: []byte("existing-bundle")},
			},
		},
	}
	h, client := newTestHandler(t, pinned)

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	for _, action := range client.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "already-pinned configuration with a populated caBundle must not be rewritten every boot")
	}
}

// TestAddFillsEmptyCABundleExactlyOnce: a policy-pinned VWC with an empty
// caBundle gets the bundle filled on the first Add (REPORT.md M9) and must
// not be rewritten again afterwards.
func TestAddFillsEmptyCABundleExactlyOnce(t *testing.T) {
	pinned := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testVWCName},
		Webhooks: []admregv1.ValidatingWebhook{
			{Name: "floatingip", FailurePolicy: new(admregv1.Fail), TimeoutSeconds: new(admissionTimeoutSeconds)},
		},
	}
	h, client := newTestHandler(t, pinned)

	require.NoError(t, h.AddValidatingWebhookConfiguration())
	updates := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			updates++
		}
	}
	assert.Equal(t, 1, updates, "empty caBundle must be filled exactly once")

	require.NoError(t, h.AddValidatingWebhookConfiguration())
	updates = 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			updates++
		}
	}
	assert.Equal(t, 1, updates, "second Add must be a no-op once the caBundle is filled")
}

// TestAddTransientGetErrorIsNotTreatedAsAbsent: the old code equated any
// Get error with "VWC missing" and fell through to Create. Keep the new
// branch honest: a transient error surfaces and no Create is attempted.
func TestAddTransientGetErrorIsNotTreatedAsAbsent(t *testing.T) {
	h, client := newTestHandler(t)
	client.PrependReactor("get", "validatingwebhookconfigurations", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("transient blip")
	})

	err := h.AddValidatingWebhookConfiguration()
	require.Error(t, err)
	for _, action := range client.Actions() {
		assert.NotEqual(t, "create", action.GetVerb(), "transient Get error must not fall through to Create")
	}
}

// REPORT.md M9: the CA-bundle read must propagate the configmap Get error
// instead of masquerading as an empty configmap ("ca.crt not found").
func TestGetCABundleConfigMapPropagatesGetError(t *testing.T) {
	h := &Handler{clientset: fake.NewSimpleClientset()}

	cm, err := h.getCABundleConfigMap()
	require.Error(t, err, "expected error when kube-root-ca.crt configmap is missing")
	require.Nil(t, cm, "expected nil configmap on error")
	assert.Contains(t, err.Error(), "failed to get kube-root-ca.crt configmap", "error does not identify the failed Get")
}

func TestGetCaBundleFromCABundleConfigMap(t *testing.T) {
	h, _ := newTestHandler(t)
	cert, err := h.getCaBundleFromCABundleConfigMap()
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----\ndmFsdWUK-----END CERTIFICATE-----\n", cert)

	h = &Handler{clientset: fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-root-ca.crt"},
		Data:       map[string]string{},
	})}
	_, err = h.getCaBundleFromCABundleConfigMap()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca.crt not found", "missing key must stay a distinct error")

	h = &Handler{clientset: fake.NewSimpleClientset()}
	_, err = h.getCaBundleFromCABundleConfigMap()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get kube-root-ca.crt configmap", "Get error must propagate, not masquerade as a missing key")
}

// TestSyncFillsOnlyEmptyCABundle: on the pre-existing-VWC path the sync
// fills an EMPTY caBundle (REPORT.md M9, rotation gap) but never replaces
// a non-empty, operator-owned one — kube-root-ca.crt is the apiserver CA
// while the serving cert may be signed by a different cluster CA, and
// swapping a working bundle under failurePolicy=Fail denies all writes.
func TestSyncFillsOnlyEmptyCABundle(t *testing.T) {
	operatorBundle := []byte("operator-ca-bundle")
	existing := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testVWCName},
		Webhooks: []admregv1.ValidatingWebhook{
			{
				Name:         "floatingip",
				ClientConfig: admregv1.WebhookClientConfig{CABundle: operatorBundle},
			},
			{
				Name: "floatingippool",
			},
		},
	}
	h, client := newTestHandler(t, existing)

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	got, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), testVWCName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, operatorBundle, got.Webhooks[0].ClientConfig.CABundle, "non-empty operator bundle must not be overwritten")
	assert.Equal(t, "-----BEGIN CERTIFICATE-----\ndmFsdWUK-----END CERTIFICATE-----\n", string(got.Webhooks[1].ClientConfig.CABundle), "empty caBundle must be filled from the configmap")
}

// TestSyncToleratesConfigMapGetError: the caBundle is refinement on the
// pre-existing-VWC path, not a startup gate — a transient CA-configmap
// failure must leave the existing bundle intact and not fail (or update)
// anything, or the M9 crashloop just moves one call deeper.
func TestSyncToleratesConfigMapGetError(t *testing.T) {
	existing := &admregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: testVWCName},
		Webhooks: []admregv1.ValidatingWebhook{
			{
				Name:          "floatingip",
				FailurePolicy: new(admregv1.Fail), TimeoutSeconds: new(admissionTimeoutSeconds),
				ClientConfig: admregv1.WebhookClientConfig{CABundle: []byte("existing-bundle")},
			},
		},
	}
	h, client := newTestHandler(t, existing)
	// Strip the seeded kube-root-ca.crt so the CA Get fails.
	require.NoError(t, client.CoreV1().ConfigMaps("kube-system").Delete(context.TODO(), "kube-root-ca.crt", metav1.DeleteOptions{}))

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	got, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), testVWCName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("existing-bundle"), got.Webhooks[0].ClientConfig.CABundle, "existing bundle must survive a failed CA read")
	for _, action := range client.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "failed CA read must not trigger an Update")
	}
}

func TestRetryWithBackoff(t *testing.T) {
	t.Cleanup(func() { log.SetLevel(log.InfoLevel) })
	log.SetLevel(log.PanicLevel)

	ctx := context.Background()

	attempts := 0
	err := retryWithBackoff(ctx, 5, time.Millisecond, 2*time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	})
	require.NoError(t, err, "expected success after retries")
	assert.Equal(t, 3, attempts)

	attempts = 0
	err = retryWithBackoff(ctx, 3, time.Millisecond, 2*time.Millisecond, func() error {
		attempts++
		return errors.New("persistent failure")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "giving up after 3 attempts")
	assert.Contains(t, err.Error(), "persistent failure")
	assert.Equal(t, 3, attempts, "expected exactly maxRetries attempts")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = retryWithBackoff(cancelled, 5, time.Hour, time.Hour, func() error {
		return errors.New("transient")
	})
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "cancelled context must not burn the backoff budget")
}
