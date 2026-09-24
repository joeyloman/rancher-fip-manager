package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetConfig(t *testing.T) {
	configYAML := `
rancher-fip-lb-controller:
  chartVersion: 0.1.0
metallb:
  chartVersion: 0.13.7
`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rancher-fip-config",
			Namespace: "rancher-fip-manager",
		},
		Data: map[string]string{
			"config.yaml": configYAML,
		},
	}

	objs := []client.Object{cm}
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	config, err := GetConfig(context.Background(), fakeClient)

	assert.NoError(t, err)
	assert.NotNil(t, config)
	assert.Equal(t, "0.1.0", config.RancherFIPLBController.ChartVersion)
	assert.Equal(t, "0.13.7", config.MetalLB.ChartVersion)
}
