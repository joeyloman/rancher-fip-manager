package controllers

import (
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// downstreamOperationTimeout bounds every single downstream API call made by
// these controllers, and is the per-request timeout of every rest.Config
// built by newDownstreamRESTConfig.
const downstreamOperationTimeout = 30 * time.Second

// newDownstreamRESTConfig builds a rest.Config from a Rancher kubeconfig
// secret value and bounds every request it makes.
//
// RESTConfigFromKubeConfig leaves Timeout unset, so a black-holed downstream
// API server (powered-off cluster, dead load balancer in front of it) blocks
// the calling goroutine indefinitely: client-go only aborts on context
// cancellation, and the reconcile context is cancelled only when the manager
// stops. The timeout is what makes a hung cluster cost one stuck worker
// instead of the whole controller.
func newDownstreamRESTConfig(kubeconfig []byte) (*rest.Config, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	config.Timeout = downstreamOperationTimeout
	return config, nil
}
