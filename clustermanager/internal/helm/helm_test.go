package helm

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/client-go/rest"
)

const (
	testRelease   = "purelb"
	testNamespace = "rancher-fip-lb-controller"
	testVersion   = "1.2.3"
	testManifest  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm\ndata:\n  key: value\n"
)

func testChart(version string) *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion: "v2",
			Name:       testRelease,
			Version:    version,
		},
		Templates: []*chart.File{{Name: "templates/configmap.yaml", Data: []byte(testManifest)}},
	}
}

func testSpec() ChartSpec {
	return ChartSpec{
		ChartName:   testRelease,
		ReleaseName: testRelease,
		Namespace:   testNamespace,
		Version:     testVersion,
		Values:      "{}",
	}
}

// seededRelease builds the release record Helm would have left behind in the
// given status, on the given chart version.
func seededRelease(status release.Status, version string) *release.Release {
	return &release.Release{
		Name:      testRelease,
		Namespace: testNamespace,
		Version:   1,
		Info:      &release.Info{Status: status, Description: "seeded by test"},
		Chart:     testChart(version),
		Config:    map[string]interface{}{},
		Manifest:  testManifest,
	}
}

type harness struct {
	client *Client
	store  *storage.Storage
}

// blockingKubeClient parks the wait phase of an install until the test lets it
// go, so the cancellation test controls the exact moment the operation becomes
// interruptible instead of racing a timer.
type blockingKubeClient struct {
	*kubefake.FailingKubeClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingKubeClient) Wait(_ kube.ResourceList, _ time.Duration) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// newHarness runs the real InstallOrUpgrade against an in-memory release store
// and a fake kube client, so the status handling — not the network — is what
// is under test.
func newHarness(t *testing.T, seeded ...*release.Release) *harness {
	t.Helper()
	return newHarnessWithKube(t, nil, seeded...)
}

func newHarnessWithKube(t *testing.T, kubeClient kube.Interface, seeded ...*release.Release) *harness {
	t.Helper()

	if kubeClient == nil {
		kubeClient = &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	}

	mem := driver.NewMemory()
	mem.SetNamespace(testNamespace)
	store := storage.Init(mem)
	for _, rel := range seeded {
		require.NoError(t, store.Create(rel))
	}

	h := &harness{store: store}
	h.client = NewClient()
	h.client.newActionConfig = func(*rest.Config, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:     store,
			KubeClient:   kubeClient,
			Capabilities: chartutil.DefaultCapabilities.Copy(),
			Log:          func(string, ...interface{}) {},
		}, nil
	}
	h.client.loadChart = func(*action.Configuration, ChartSpec) (*chart.Chart, error) {
		return testChart(testVersion), nil
	}
	return h
}

func (h *harness) latest(t *testing.T) *release.Release {
	t.Helper()
	rel, err := h.store.Last(testRelease)
	require.NoError(t, err, "expected a release record to exist")
	return rel
}

func (h *harness) historyLen(t *testing.T) int {
	t.Helper()
	rels, err := h.store.History(testRelease)
	require.NoError(t, err)
	return len(rels)
}

func TestPlanReleaseAction(t *testing.T) {
	tests := []struct {
		name      string
		status    release.Status
		installed string
		desired   string
		want      releaseAction
	}{
		{"deployed on the desired chart is a no-op", release.StatusDeployed, "1.0.0", "1.0.0", actionSkip},
		{"deployed on another chart is upgraded", release.StatusDeployed, "0.9.0", "1.0.0", actionUpgrade},
		{"failed is upgraded in place", release.StatusFailed, "1.0.0", "1.0.0", actionUpgrade},
		{"pending-install is purged and reinstalled", release.StatusPendingInstall, "1.0.0", "1.0.0", actionRepair},
		{"pending-upgrade is purged and reinstalled", release.StatusPendingUpgrade, "1.0.0", "1.0.0", actionRepair},
		{"pending-rollback is purged and reinstalled", release.StatusPendingRollback, "1.0.0", "1.0.0", actionRepair},
		{"uninstalling is purged and reinstalled", release.StatusUninstalling, "1.0.0", "1.0.0", actionRepair},
		{"uninstalled is purged and reinstalled", release.StatusUninstalled, "1.0.0", "1.0.0", actionRepair},
		{"unknown is purged and reinstalled", release.StatusUnknown, "1.0.0", "1.0.0", actionRepair},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, planReleaseAction(tc.status, tc.installed, tc.desired))
		})
	}
}

func TestReleaseStatusOfToleratesPartialRecords(t *testing.T) {
	assert.Equal(t, release.StatusUnknown, releaseStatusOf(nil))
	assert.Equal(t, release.StatusUnknown, releaseStatusOf(&release.Release{Name: testRelease}))
	assert.Equal(t, "", releaseChartVersion(&release.Release{Name: testRelease}))
	assert.Equal(t, "", releaseChartVersion(&release.Release{Chart: &chart.Chart{}}))
	assert.Equal(t, "1.0.0", releaseChartVersion(&release.Release{Chart: testChart("1.0.0")}))
}

// TestInstallOrUpgradeConverges is the regression for the wrapper reporting
// success on a release it never made healthy: a release Helm parked in a
// pending status on the *desired* chart version used to match the version
// comparison and be logged as "already installed, skipping upgrade".
func TestInstallOrUpgradeConverges(t *testing.T) {
	tests := []struct {
		name           string
		seed           *release.Release
		wantHistoryLen int
	}{
		{"no release yet", nil, 1},
		{"wedged pending-install on the desired chart", seededRelease(release.StatusPendingInstall, testVersion), 1},
		{"wedged pending-upgrade on the desired chart", seededRelease(release.StatusPendingUpgrade, testVersion), 1},
		{"wedged pending-rollback on the desired chart", seededRelease(release.StatusPendingRollback, testVersion), 1},
		{"half-written uninstalling record", seededRelease(release.StatusUninstalling, testVersion), 1},
		{"uninstalled record still occupies the name", seededRelease(release.StatusUninstalled, testVersion), 1},
		{"unknown record", seededRelease(release.StatusUnknown, testVersion), 1},
		{"failed release is repaired in place", seededRelease(release.StatusFailed, testVersion), 2},
		{"deployed release on an older chart is upgraded", seededRelease(release.StatusDeployed, "0.9.0"), 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			if tc.seed == nil {
				h = newHarness(t)
			} else {
				h = newHarness(t, tc.seed)
			}

			require.NoError(t, h.client.InstallOrUpgrade(context.Background(), &rest.Config{}, testSpec()))

			latest := h.latest(t)
			assert.Equal(t, release.StatusDeployed, latest.Info.Status, "the release must end up deployed, not left in the status it was found in")
			assert.Equal(t, testVersion, latest.Chart.Metadata.Version)
			assert.Equal(t, tc.wantHistoryLen, h.historyLen(t))
		})
	}
}

// TestInstallOrUpgradeSkipsHealthyRelease guards the other side of the status
// check: a deployed release on the desired chart must still be left alone,
// because re-installing it would restart the data plane of every healthy
// cluster on every reconcile.
func TestInstallOrUpgradeSkipsHealthyRelease(t *testing.T) {
	h := newHarness(t, seededRelease(release.StatusDeployed, testVersion))

	require.NoError(t, h.client.InstallOrUpgrade(context.Background(), &rest.Config{}, testSpec()))

	latest := h.latest(t)
	assert.Equal(t, 1, latest.Version, "the stored revision must be untouched")
	assert.Equal(t, "seeded by test", latest.Info.Description, "the stored record must not be rewritten")
	assert.Equal(t, 1, h.historyLen(t))
}

// TestInstallOrUpgradeAbortsCancelledInstall proves the reconcile context is
// threaded into the Helm actions: the wait phase is cut short instead of
// running to the ten minute operation timeout. Without RunWithContext the
// install would ignore the cancelled context and sit in Wait until the test
// let it go.
func TestInstallOrUpgradeAbortsCancelledInstall(t *testing.T) {
	wait := &blockingKubeClient{
		FailingKubeClient: &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}},
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	h := newHarnessWithKube(t, wait)
	t.Cleanup(func() { close(wait.release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-wait.entered
		cancel()
	}()

	err := h.client.InstallOrUpgrade(ctx, &rest.Config{}, testSpec())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "the install must abort with the context error, got %v", err)
}

// corruptDriver stores records like the memory driver but reproduces the
// undecodable-release-secret state observed live: the storage rows exist,
// yet Query fails to decode every one of them (empty list, nil error — the
// wedge at the history check) while reading the release itself can only
// fail ("no revision").
type corruptDriver struct {
	mu      sync.Mutex
	records map[string]*release.Release
}

func (d *corruptDriver) Create(key string, rls *release.Release) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.records[key]; exists {
		return driver.ErrReleaseExists
	}
	d.records[key] = rls
	return nil
}

func (d *corruptDriver) Update(key string, rls *release.Release) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.records[key]; !exists {
		return driver.ErrReleaseNotFound
	}
	d.records[key] = rls
	return nil
}

func (d *corruptDriver) Delete(key string) (*release.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rel, ok := d.records[key]
	if !ok {
		return nil, driver.ErrReleaseNotFound
	}
	delete(d.records, key)
	return rel, nil
}

func (d *corruptDriver) Get(key string) (*release.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rel, ok := d.records[key]
	if !ok {
		return nil, driver.ErrReleaseNotFound
	}
	return rel, nil
}

func (d *corruptDriver) List(filter func(*release.Release) bool) ([]*release.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*release.Release
	for _, rel := range d.records {
		if filter == nil || filter(rel) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// Query models the corrupt read: every row fails to decode, so Helm sees
// neither a record nor an error.
func (d *corruptDriver) Query(_ map[string]string) ([]*release.Release, error) {
	return nil, nil
}

func (d *corruptDriver) Name() string { return "corrupt" }

func (d *corruptDriver) purgeByName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.records {
		if strings.Contains(key, "."+name+".v") {
			delete(d.records, key)
		}
	}
}

// TestInstallOrUpgradePurgesUnreadableRelease pins bug C: a stored but
// undecodable release record must be purged by label and reinstalled, not
// fail the reconcile with "no revision for release" forever — the loop that
// previously needed a human to delete the corrupt secret by hand.
func TestInstallOrUpgradePurgesUnreadableRelease(t *testing.T) {
	d := &corruptDriver{records: map[string]*release.Release{}}
	require.NoError(t, storage.Init(d).Create(seededRelease(release.StatusUnknown, testVersion)))

	kubeClient := &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard}}
	var purged []string

	client := NewClient()
	client.purgeReleaseRecords = func(_ context.Context, _ *rest.Config, namespace, name string) error {
		purged = append(purged, namespace+"/"+name)
		d.purgeByName(name)
		return nil
	}
	store := storage.Init(d)
	client.newActionConfig = func(*rest.Config, string) (*action.Configuration, error) {
		return &action.Configuration{
			Releases:     store,
			KubeClient:   kubeClient,
			Capabilities: chartutil.DefaultCapabilities.Copy(),
			Log:          func(string, ...interface{}) {},
		}, nil
	}
	client.loadChart = func(*action.Configuration, ChartSpec) (*chart.Chart, error) {
		return testChart(testVersion), nil
	}

	require.NoError(t, client.InstallOrUpgrade(context.Background(), &rest.Config{}, testSpec()),
		"an unreadable release must be purged and reinstalled, not fail the reconcile")
	assert.Equal(t, []string{testNamespace + "/" + testRelease}, purged,
		"the purge seam must run exactly once for the corrupt release")

	rels, err := d.List(func(*release.Release) bool { return true })
	require.NoError(t, err)
	require.Len(t, rels, 1, "the corrupt record must be replaced by exactly the fresh install")
	assert.Equal(t, release.StatusDeployed, rels[0].Info.Status)
}
