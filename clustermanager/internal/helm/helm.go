package helm

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// ChartSpec defines the parameters for a Helm chart installation.
type ChartSpec struct {
	ChartName   string
	ReleaseName string
	Namespace   string
	Version     string
	Values      string
	RepoURL     string
}

// Interface defines the methods for a Helm client.
type Interface interface {
	// InstallOrUpgrade installs a Helm chart if it's not already present, or upgrades it if it is.
	InstallOrUpgrade(ctx context.Context, config *rest.Config, spec ChartSpec) error
	AddOrUpdateChartRepo(repo repo.Entry) error
	// CheckIfChartIsInstalled checks if a Helm chart is installed in the specified namespace. Returns true if installed, false if not found, and an error if something goes wrong.
	CheckIfChartIsInstalled(ctx context.Context, config *rest.Config, releaseName, namespace string) (bool, error)
}

// Client is a Helm client that implements the Interface.
type Client struct{}

// NewClient creates a new Helm client.
func NewClient() *Client {
	return &Client{}
}

func (c *Client) locateChart(spec ChartSpec, settings *cli.EnvSettings) (*chart.Chart, error) {
	dl := downloader.ChartDownloader{
		Out:              os.Stdout,
		Getters:          getter.All(settings),
		RepositoryConfig: settings.RepositoryConfig,
		RepositoryCache:  settings.RepositoryCache,
	}

	chartPath, _, err := dl.DownloadTo(spec.ChartName, spec.Version, settings.RepositoryCache)
	if err != nil {
		return nil, err
	}

	return loader.Load(chartPath)
}

// InstallOrUpgrade installs a Helm chart if it's not already present, or upgrades it if it is.
func (c *Client) InstallOrUpgrade(ctx context.Context, config *rest.Config, spec ChartSpec) error {
	settings := cli.New()
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(newRESTClientGetter(config, spec.Namespace), spec.Namespace, "secret", log.Printf); err != nil {
		return err
	}

	var chart *chart.Chart
	if registry.IsOCI(spec.ChartName) {
		registryClient, err := registry.NewClient()
		if err != nil {
			return err
		}
		actionConfig.RegistryClient = registryClient

		installClientForChartPath := action.NewInstall(actionConfig)
		installClientForChartPath.ChartPathOptions.Version = spec.Version
		installClientForChartPath.ChartPathOptions.RepoURL = spec.RepoURL

		chartPath, err := installClientForChartPath.ChartPathOptions.LocateChart(spec.ChartName, settings)
		if err != nil {
			return errors.Wrapf(err, "failed to locate chart %q", spec.ChartName)
		}

		chart, err = loader.Load(chartPath)
		if err != nil {
			return errors.Wrapf(err, "failed to load chart from path %q", chartPath)
		}
	} else {
		var err error
		chart, err = c.locateChart(spec, settings)
		if err != nil {
			return err
		}
	}

	var vals map[string]interface{}
	if err := yaml.Unmarshal([]byte(spec.Values), &vals); err != nil {
		return err
	}

	historyClient := action.NewHistory(actionConfig)
	historyClient.Max = 1
	if _, err := historyClient.Run(spec.ReleaseName); err == driver.ErrReleaseNotFound {
		log.Printf("Release %q not found. Installing...", spec.ReleaseName)
		installClient := action.NewInstall(actionConfig)
		installClient.Namespace = spec.Namespace
		installClient.Version = spec.Version
		installClient.Wait = true
		installClient.Timeout = 10 * time.Minute
		installClient.ReleaseName = spec.ReleaseName

		if _, err := installClient.Run(chart, vals); err != nil {
			return errors.Wrapf(err, "failed to install release %q", spec.ReleaseName)
		}
		log.Printf("Release %q installed successfully.", spec.ReleaseName)
		return nil
	} else if err != nil {
		return errors.Wrapf(err, "failed to check history of release %q", spec.ReleaseName)
	}

	getClient := action.NewGet(actionConfig)
	release, err := getClient.Run(spec.ReleaseName)
	if err != nil {
		return errors.Wrapf(err, "failed to get release %q", spec.ReleaseName)
	}

	if release.Chart.Metadata.Version == chart.Metadata.Version {
		log.Printf("Release %q is already installed with chart version %q. Skipping upgrade.", spec.ReleaseName, chart.Metadata.Version)
		return nil
	}

	log.Printf("Release %q found. Upgrading...", spec.ReleaseName)
	upgradeClient := action.NewUpgrade(actionConfig)
	upgradeClient.Namespace = spec.Namespace
	upgradeClient.Version = spec.Version
	upgradeClient.Wait = true
	upgradeClient.Timeout = 10 * time.Minute
	upgradeClient.Install = true // Keep this, it can handle some edge cases.

	if _, err := upgradeClient.Run(spec.ReleaseName, chart, vals); err != nil {
		return errors.Wrapf(err, "failed to upgrade release %q", spec.ReleaseName)
	}
	log.Printf("Release %q upgraded successfully.", spec.ReleaseName)

	return nil
}

func (c *Client) AddOrUpdateChartRepo(entry repo.Entry) error {
	settings := cli.New()

	repoFile := settings.RepositoryConfig

	// Ensure the directory for the repository file exists.
	if err := os.MkdirAll(filepath.Dir(repoFile), 0755); err != nil {
		return err
	}

	// Load the repository file
	r, err := repo.LoadFile(repoFile)
	if err != nil {
		if os.IsNotExist(errors.Cause(err)) {
			r = repo.NewFile()
		} else {
			return err
		}
	}

	// If the repo doesn't exist, add it.
	if !r.Has(entry.Name) {
		r.Add(&entry)
		if err := r.WriteFile(repoFile, 0644); err != nil {
			return err
		}
		log.Printf("Repository %s has been added.", entry.Name)
	} else {
		log.Printf("Repository %s already exists.", entry.Name)
	}

	// Update the repo
	cr, err := repo.NewChartRepository(&entry, getter.All(settings))
	if err != nil {
		return err
	}
	if _, err := cr.DownloadIndexFile(); err != nil {
		return errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", entry.URL)
	}
	log.Printf("Successfully got an update from the %q chart repository", entry.Name)

	return nil
}

// CheckIfChartIsInstalled checks if a Helm chart release is installed in the specified namespace.
// Returns true if the chart is installed, false if the chart is not found, and an error if something goes wrong.
func (c *Client) CheckIfChartIsInstalled(ctx context.Context, config *rest.Config, releaseName, namespace string) (bool, error) {
	// Validate that release name is not empty
	if releaseName == "" {
		return false, errors.New("release name cannot be empty")
	}

	// Validate that namespace is not empty
	if namespace == "" {
		return false, errors.New("namespace cannot be empty")
	}

	actionConfig := new(action.Configuration)

	// Initialize action config with the namespace where the release should be installed
	if err := actionConfig.Init(newRESTClientGetter(config, namespace), namespace, "secret", log.Printf); err != nil {
		return false, errors.Wrapf(err, "failed to initialize action config for checking release %q in namespace %q", releaseName, namespace)
	}

	historyClient := action.NewHistory(actionConfig)
	historyClient.Max = 1
	_, err := historyClient.Run(releaseName)
	if err == driver.ErrReleaseNotFound {
		return false, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "failed to check if release %q is installed in namespace %q", releaseName, namespace)
	}

	log.Printf("Release %q is installed in namespace %q.", releaseName, namespace)
	return true, nil
}

// restClientGetter is a simple implementation of genericclioptions.RESTClientGetter.
type restClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

// newRESTClientGetter creates a new restClientGetter.
func newRESTClientGetter(config *rest.Config, namespace string) *restClientGetter {
	return &restClientGetter{
		restConfig: config,
		namespace:  namespace,
	}
}

// ToRESTConfig returns the REST config.
func (c *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return c.restConfig, nil
}

// ToDiscoveryClient returns a discovery client.
func (c *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	d, err := discovery.NewDiscoveryClientForConfig(c.restConfig)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(d), nil
}

// ToRESTMapper returns a REST mapper.
func (c *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	d, err := c.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(d)
	expander := restmapper.NewShortcutExpander(mapper, d, nil)
	return expander, nil
}

// ToRawKubeConfigLoader returns a raw kubeconfig loader.
func (c *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// use the KUBECONFIG environment variable
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	overrides := &clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: c.restConfig.Host}}
	if c.restConfig.BearerToken != "" {
		overrides.AuthInfo = clientcmdapi.AuthInfo{Token: c.restConfig.BearerToken}
	}
	if c.restConfig.CAData != nil {
		overrides.ClusterInfo.CertificateAuthorityData = c.restConfig.CAData
	}
	if c.restConfig.CertData != nil {
		overrides.AuthInfo.ClientCertificateData = c.restConfig.CertData
	}
	if c.restConfig.KeyData != nil {
		overrides.AuthInfo.ClientKeyData = c.restConfig.KeyData
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
}
