package admission

import (
	"context"
	"fmt"
	"testing"

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
			{Name: "floatingip", FailurePolicy: new(admregv1.Fail), TimeoutSeconds: new(admissionTimeoutSeconds)},
		},
	}
	h, client := newTestHandler(t, pinned)

	require.NoError(t, h.AddValidatingWebhookConfiguration())

	for _, action := range client.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "already-pinned configuration must not be rewritten every boot")
	}
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
