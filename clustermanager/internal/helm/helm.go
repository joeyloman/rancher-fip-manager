package helm

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
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
//
// The action-config and chart-loading seams are fields rather than direct
// calls so the convergence logic in InstallOrUpgrade can be exercised against
// an in-memory release store and a fake kube client.
type Client struct {
	// newActionConfig builds the Helm action configuration for one
	// downstream cluster.
	newActionConfig func(config *rest.Config, namespace string) (*action.Configuration, error)
	// loadChart resolves and loads the chart a spec points at; it takes the
	// action configuration because OCI chart locations need its registry
	// client.
	loadChart func(actionConfig *action.Configuration, spec ChartSpec) (*chart.Chart, error)
	// purgeReleaseRecords deletes every Helm storage record for a release
	// by label selector WITHOUT decoding them. Needed when the record is
	// too corrupt for Helm's own Get/Uninstall to read: an unreadable
	// release must still be purgeable so provisioning can converge.
	purgeReleaseRecords func(ctx context.Context, config *rest.Config, namespace, releaseName string) error
	// repoLock serialises access to the process-wide on-disk repository
	// file and index cache: repo.File.WriteFile is not atomic and the
	// reconcile workers install the same charts for many clusters at once.
	repoLock sync.Mutex
}

// NewClient creates a new Helm client.
func NewClient() *Client {
	c := &Client{}
	c.newActionConfig = c.defaultActionConfig
	c.loadChart = c.defaultLoadChart
	c.purgeReleaseRecords = defaultPurgeReleaseRecords
	return c
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

// defaultActionConfig builds the Helm action configuration that talks to the
// downstream cluster reached through config.
func (c *Client) defaultActionConfig(config *rest.Config, namespace string) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(newRESTClientGetter(config, namespace), namespace, "secret", log.Printf); err != nil {
		return nil, err
	}
	return actionConfig, nil
}

// defaultLoadChart resolves and loads the chart a spec points at, from either
// an OCI registry or an HTTP chart repository. Both paths are bounded by the
// Helm getter timeout (five minutes unless HELM_HTTP_TIMEOUT_SECONDS says
// otherwise).
func (c *Client) defaultLoadChart(actionConfig *action.Configuration, spec ChartSpec) (*chart.Chart, error) {
	settings := cli.New()

	if !registry.IsOCI(spec.ChartName) {
		return c.locateChart(spec, settings)
	}

	registryClient, err := registry.NewClient()
	if err != nil {
		return nil, err
	}
	// NewInstall wires the registry client into the chart path options,
	// which is what makes the OCI download below work.
	actionConfig.RegistryClient = registryClient

	installClientForChartPath := action.NewInstall(actionConfig)
	installClientForChartPath.ChartPathOptions.Version = spec.Version
	installClientForChartPath.ChartPathOptions.RepoURL = spec.RepoURL

	chartPath, err := installClientForChartPath.ChartPathOptions.LocateChart(spec.ChartName, settings)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to locate chart %q", spec.ChartName)
	}

	chrt, err := loader.Load(chartPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load chart from path %q", chartPath)
	}
	return chrt, nil
}

// helmOperationTimeout bounds a single install/upgrade operation, including
// its wait phase. helmUninstallTimeout bounds the release-repair uninstall
// (which includes its delete hooks).
const (
	helmOperationTimeout = 10 * time.Minute
	helmUninstallTimeout = 2 * time.Minute
)

// releaseAction is the operation InstallOrUpgrade derives from the *stored*
// release. Chart version equality alone is not evidence of a healthy release,
// so the status has to be part of the plan for the wrapper to converge.
type releaseAction int

const (
	actionSkip releaseAction = iota
	actionUpgrade
	actionRepair
)

// planReleaseAction maps the status Helm left behind to the operation needed
// to reach desiredVersion.
//
// Only a `deployed` release on the desired chart counts as converged. Helm
// parks a release in a `pending-*` status when an operation dies half-way (a
// killed pod, an expired Wait) and then refuses to upgrade a pending release
// ("another operation is in progress") as well as to install over a name that
// is still in use; those — plus `uninstalling`, `uninstalled` and `unknown` —
// can only move via purge-and-install. A `failed` release can be upgraded in
// place, which is the non-destructive repair; InstallOrUpgrade escalates a
// failed in-place repair to the purge path.
func planReleaseAction(status release.Status, installedVersion, desiredVersion string) releaseAction {
	switch status {
	case release.StatusDeployed:
		if installedVersion == desiredVersion {
			return actionSkip
		}
		return actionUpgrade
	case release.StatusFailed:
		return actionUpgrade
	default:
		return actionRepair
	}
}

// releaseStatusOf reports the stored status; records without one (partially
// written or hand-edited release secrets) are treated as unknown so they get
// repaired instead of trusted.
func releaseStatusOf(rel *release.Release) release.Status {
	if rel == nil || rel.Info == nil {
		return release.StatusUnknown
	}
	return rel.Info.Status
}

// releaseChartVersion reports the chart version recorded on a stored release.
// An empty string for records that never got a chart makes the caller treat
// them as "not the desired version".
func releaseChartVersion(rel *release.Release) string {
	if rel == nil || rel.Chart == nil || rel.Chart.Metadata == nil {
		return ""
	}
	return rel.Chart.Metadata.Version
}

// InstallOrUpgrade installs a Helm chart if it's not already present, or
// upgrades it if it is. A release that exists but is not `deployed` (wedged
// `pending-*`, `failed`, `uninstalled`) is repaired instead of being reported
// as successfully synced.
//
// ctx bounds every Helm operation below, so a cancelled reconcile context
// aborts an in-flight install instead of letting it run to its own timeout.
// That is realised whenever the manager is allowed to drain — e.g. under
// `--leader-elect=false`; under leader election the process exits as soon as
// RunOrDie returns, and the `pending-*` record a killed install leaves behind
// is picked up by the status plan above. Chart downloads are bounded by the
// Helm getter timeout.
func (c *Client) InstallOrUpgrade(ctx context.Context, config *rest.Config, spec ChartSpec) error {
	actionConfig, err := c.newActionConfig(config, spec.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to initialize action config for release %q", spec.ReleaseName)
	}

	chrt, err := c.loadChart(actionConfig, spec)
	if err != nil {
		return errors.Wrapf(err, "failed to load chart %q version %q", spec.ChartName, spec.Version)
	}

	var vals map[string]interface{}
	if err := yaml.Unmarshal([]byte(spec.Values), &vals); err != nil {
		return errors.Wrapf(err, "failed to parse values for release %q", spec.ReleaseName)
	}

	historyClient := action.NewHistory(actionConfig)
	historyClient.Max = 1
	if _, err := historyClient.Run(spec.ReleaseName); stderrors.Is(err, driver.ErrReleaseNotFound) {
		log.Printf("Release %q not found. Installing...", spec.ReleaseName)
		return c.installRelease(ctx, actionConfig, spec, chrt, vals)
	} else if err != nil {
		return errors.Wrapf(err, "failed to check history of release %q", spec.ReleaseName)
	}

	getClient := action.NewGet(actionConfig)
	existingRelease, err := getClient.Run(spec.ReleaseName)
	if err != nil {
		// History saw the storage rows but the release itself cannot be
		// read — the usual cause is a corrupt (undecodable) release
		// secret. There is no status to plan from, and Helm's own purge
		// cannot run either because Uninstall reads the same broken
		// record. Drop the storage rows by label and reinstall: an
		// unreadable release is indistinguishable from an absent one.
		log.Printf("Release %q is stored but unreadable (%s); purging its storage records and reinstalling.", spec.ReleaseName, err)
		if perr := c.purgeReleaseRecords(ctx, config, spec.Namespace, spec.ReleaseName); perr != nil {
			return errors.Wrapf(perr, "failed to purge unreadable release %q", spec.ReleaseName)
		}
		return c.installRelease(ctx, actionConfig, spec, chrt, vals)
	}

	status := releaseStatusOf(existingRelease)
	installedVersion := releaseChartVersion(existingRelease)

	switch planReleaseAction(status, installedVersion, chrt.Metadata.Version) {
	case actionSkip:
		log.Printf("Release %q is already installed with chart version %q. Skipping upgrade.", spec.ReleaseName, chrt.Metadata.Version)
		return nil
	case actionUpgrade:
		err := c.upgradeRelease(ctx, actionConfig, spec, chrt, vals)
		if err == nil {
			return nil
		}
		if status != release.StatusFailed {
			return err
		}
		// An in-place repair of a failed release did not work: fall back to
		// purging it, which is the only operation that can clear the record.
		log.Printf("In-place repair of failed release %q failed (%s); purging and reinstalling.", spec.ReleaseName, err)
		return c.repairRelease(ctx, actionConfig, spec, chrt, vals)
	default:
		log.Printf("Release %q is not deployed (status %q, chart version %q); purging and reinstalling chart version %q.",
			spec.ReleaseName, status, installedVersion, chrt.Metadata.Version)
		return c.repairRelease(ctx, actionConfig, spec, chrt, vals)
	}
}

// installRelease runs `helm install` and waits for the resources to become
// ready, bounded by ctx and helmOperationTimeout.
func (c *Client) installRelease(ctx context.Context, actionConfig *action.Configuration, spec ChartSpec, chrt *chart.Chart, vals map[string]interface{}) error {
	installClient := action.NewInstall(actionConfig)
	installClient.Namespace = spec.Namespace
	installClient.Version = spec.Version
	installClient.Wait = true
	installClient.Timeout = helmOperationTimeout
	installClient.ReleaseName = spec.ReleaseName
	// Adopt resources a previous, half-way-dead operation left behind instead
	// of failing the install with "already exists".
	installClient.TakeOwnership = true

	if _, err := installClient.RunWithContext(ctx, chrt, vals); err != nil {
		return errors.Wrapf(err, "failed to install release %q", spec.ReleaseName)
	}
	log.Printf("Release %q installed successfully.", spec.ReleaseName)
	return nil
}

// upgradeRelease runs `helm upgrade` and waits for the resources to become
// ready, bounded by ctx and helmOperationTimeout.
func (c *Client) upgradeRelease(ctx context.Context, actionConfig *action.Configuration, spec ChartSpec, chrt *chart.Chart, vals map[string]interface{}) error {
	log.Printf("Release %q found. Upgrading to chart version %q...", spec.ReleaseName, chrt.Metadata.Version)
	upgradeClient := action.NewUpgrade(actionConfig)
	upgradeClient.Namespace = spec.Namespace
	upgradeClient.Version = spec.Version
	upgradeClient.Wait = true
	upgradeClient.Timeout = helmOperationTimeout
	upgradeClient.Install = true // Keep this, it can handle some edge cases.
	// Remove resources created by a failed attempt so the next reconcile can
	// retry without tripping over them.
	upgradeClient.CleanupOnFail = true

	if _, err := upgradeClient.RunWithContext(ctx, spec.ReleaseName, chrt, vals); err != nil {
		return errors.Wrapf(err, "failed to upgrade release %q", spec.ReleaseName)
	}
	log.Printf("Release %q upgraded successfully.", spec.ReleaseName)
	return nil
}

// repairRelease purges the release record (and whatever resources Helm still
// claims for it) and installs the desired chart again. It is the only path
// that can move a release out of a `pending-*` status, because Helm refuses
// to upgrade or install over such a release.
func (c *Client) repairRelease(ctx context.Context, actionConfig *action.Configuration, spec ChartSpec, chrt *chart.Chart, vals map[string]interface{}) error {
	uninstallClient := action.NewUninstall(actionConfig)
	uninstallClient.KeepHistory = false
	uninstallClient.IgnoreNotFound = true
	uninstallClient.Timeout = helmUninstallTimeout
	if _, err := uninstallClient.Run(spec.ReleaseName); err != nil {
		return errors.Wrapf(err, "failed to purge release %q during repair", spec.ReleaseName)
	}
	return c.installRelease(ctx, actionConfig, spec, chrt, vals)
}

func (c *Client) AddOrUpdateChartRepo(entry repo.Entry) error {
	// The repository file and index cache are process-wide state shared by
	// every reconcile worker.
	c.repoLock.Lock()
	defer c.repoLock.Unlock()

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

// defaultPurgeReleaseRecords deletes all Helm release-storage secrets for
// releaseName via label selector, bypassing Helm's storage layer entirely:
// the records may be undecodable, which is exactly when this is needed.
func defaultPurgeReleaseRecords(ctx context.Context, config *rest.Config, namespace, releaseName string) error {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrapf(err, "failed to build kube client to purge release %q", releaseName)
	}
	records, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("name=%s,owner=helm", releaseName),
	})
	if err != nil {
		return errors.Wrapf(err, "failed to list storage records of release %q", releaseName)
	}
	for i := range records.Items {
		if err := client.CoreV1().Secrets(namespace).Delete(ctx, records.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "failed to delete storage record %s of release %q", records.Items[i].Name, releaseName)
		}
	}
	return nil
}
