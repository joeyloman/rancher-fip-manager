package mocks

import (
	"context"

	"github.com/stretchr/testify/mock"
	"k8s.io/client-go/rest"
)

type HelmClient struct {
	mock.Mock
}

func (m *HelmClient) InstallOrUpgrade(ctx context.Context, config *rest.Config, chartName, releaseName, namespace string, values map[string]interface{}) error {
	args := m.Called(ctx, config, chartName, releaseName, namespace, values)
	return args.Error(0)
}
