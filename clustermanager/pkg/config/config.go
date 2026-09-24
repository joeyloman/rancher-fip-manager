package config

// Config holds the configuration for the rancher-fip-cluster-manager
type Config struct {
	RancherFipApiServerURL             string
	RancherFipLBControllerChartName    string
	RancherFipLBControllerChartRef     string
	RancherFipLBControllerChartVersion string
	RancherFipLBControllerNamespace    string
	RancherFipLBControllerValues       string
	PureLBControllerChartName          string
	PureLBControllerChartRef           string
	PureLBControllerChartVersion       string
	PureLBControllerNamespace          string
	PureLBControllerValues             string
	MetalLBControllerChartName         string
	MetalLBControllerChartRef          string
	MetalLBControllerChartVersion      string
	MetalLBControllerNamespace         string
	MetalLBControllerValues            string
	CaCrt                              []byte
}
