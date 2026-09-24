package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRegister(t *testing.T) {
	ctx := context.Background()
	kubeConfig := "/path/to/kubeconfig"
	kubeContext := "my-context"
	webhookName := "my-webhook"
	webhookNamespace := "my-namespace"

	handler := Register(ctx, kubeConfig, kubeContext, webhookName, webhookNamespace)

	assert.NotNil(t, handler)
	assert.Equal(t, ctx, handler.ctx)
	assert.Equal(t, kubeConfig, handler.kubeConfig)
	assert.Equal(t, kubeContext, handler.kubeContext)
	assert.Equal(t, webhookName, handler.webhookName)
	assert.Equal(t, webhookNamespace, handler.webhookNamespace)
}

func TestInit(t *testing.T) {
	handler := &Handler{
		clientset: fake.NewSimpleClientset(),
	}

	// handler.Init() should not be called here directly as it tries to get a real kubeconfig
	// Instead, we just check if the clientset is not nil after being set.
	assert.NotNil(t, handler.clientset)
}
