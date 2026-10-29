package kube

import (
	"log"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClientset creates and returns a new Kubernetes clientset.
// It uses the KUBECONFIG and KUBECONTEXT environment variables if they are set.
// If KUBECONFIG is not set, it defaults to ~/.kube/config.
// If no kubeconfig file is found, it attempts to use the in-cluster configuration.
func NewClientset() (*kubernetes.Clientset, error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	kubecontext := os.Getenv("KUBECONTEXT")

	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Printf("Error getting user home directory: %s", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: kubecontext},
	).ClientConfig()

	if err != nil {
		logrus.Errorf("Could not load kubeconfig, trying in-cluster config: %s", err)
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}
