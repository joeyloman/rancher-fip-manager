package config

import (
	"context"
	"fmt"

	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config holds the configuration for the rancher-fip-cluster-manager.
type Config struct {
	// RancherFIPLBController holds the configuration for the rancher-fip-lb-controller Helm chart.
	RancherFIPLBController struct {
		// ChartVersion is the version of the rancher-fip-lb-controller Helm chart to install.
		ChartVersion string `yaml:"chartVersion"`
	} `yaml:"rancher-fip-lb-controller"`
	// MetalLB holds the configuration for the MetalLB Helm chart.
	MetalLB struct {
		// ChartVersion is the version of the MetalLB Helm chart to install.
		ChartVersion string `yaml:"chartVersion"`
	} `yaml:"metallb"`
}

// GetKubeConfig returns a Kubernetes REST config, loading it from the KUBECONFIG environment variable,
// or from the default kubeconfig path if KUBECONFIG is not set.
func GetKubeConfig() (*rest.Config, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	kubeconfig = filepath.Join(home, ".kube", "config")
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// GetConfig retrieves the configuration from the rancher-fip-config ConfigMap in the rancher-fip namespace
// and unmarshals it into a Config struct.
func GetConfig(ctx context.Context, c client.Client) (*Config, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: "rancher-fip-config", Namespace: "rancher-fip-manager"}, cm)
	if err != nil {
		return nil, err
	}

	configData, ok := cm.Data["config.yaml"]
	if !ok {
		return nil, fmt.Errorf("config.yaml not found in rancher-fip-config ConfigMap")
	}

	var config Config
	err = yaml.Unmarshal([]byte(configData), &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
