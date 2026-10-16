package helm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgofake "k8s.io/client-go/kubernetes/fake"
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
	// The sanitize pass hits the kube API for storage secrets; these tests
	// drive Helm's actions through the in-memory store, so stub it out. Its
	// decode-and-delete behavior is pinned by TestDefaultPurgeUndecodableRecords.
	h.client.purgeUndecodableRecords = func(context.Context, *rest.Config, string, string) error {
		return nil
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
	client.purgeUndecodableRecords = func(context.Context, *rest.Config, string, string) error {
		return nil
	}
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

// encodedRelease builds a storage payload exactly the way Helm's secret
// driver stores one: base64(gzip(json(release))).
func encodedRelease(r *release.Release) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// TestDefaultPurgeUndecodableRecords pins the live failure from the aio2
// cluster: a corrupt release-storage secret (raw binary where base64 text
// belongs) is silently skipped by every Helm read and can never be removed
// by Uninstall, so the sanitizer must delete exactly the undecodable records
// and keep the readable ones Helm plans from.
func TestDefaultPurgeUndecodableRecords(t *testing.T) {
	valid, err := encodedRelease(seededRelease(release.StatusDeployed, testVersion))
	require.NoError(t, err)

	// Legacy record from before the storage driver compressed releases:
	// plain base64(json(release)) with no gzip magic header.
	legacyJSON, err := json.Marshal(seededRelease(release.StatusDeployed, testVersion))
	require.NoError(t, err)
	legacy := base64.StdEncoding.EncodeToString(legacyJSON)

	releaseLabels := func(version string) map[string]string {
		return map[string]string{"name": testRelease, "owner": "helm", "status": "deployed", "version": version}
	}
	client := clientgofake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + testRelease + ".v1", Namespace: testNamespace, Labels: releaseLabels("1")},
			Data:       map[string][]byte{"release": []byte(valid)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + testRelease + ".v2", Namespace: testNamespace, Labels: releaseLabels("2")},
			Data:       map[string][]byte{"release": []byte(legacy)},
		},
		// The observed corruption: the payload is not base64 at all.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + testRelease + ".v3", Namespace: testNamespace, Labels: releaseLabels("3")},
			Data:       map[string][]byte{"release": {0x8f, 0x00, 0x1f, 0x8b, 0x00}},
		},
		// Decodable base64, but neither gzip nor JSON once decoded.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + testRelease + ".v4", Namespace: testNamespace, Labels: releaseLabels("4")},
			Data:       map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString([]byte("not a release"))), "extra": []byte("ignored")},
		},
	)

	require.NoError(t, purgeUndecodableRecordsWithClient(context.Background(), client, testNamespace, testRelease),
		"the sanitizer must tolerate undecodable records, not fail on them")

	secrets, err := client.CoreV1().Secrets(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "name=" + testRelease + ",owner=helm"})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 2, "only the decodable records (compressed + legacy uncompressed) must survive the sanitize pass")
	survivors := map[string]bool{}
	for _, s := range secrets.Items {
		survivors[s.Name] = true
	}
	assert.True(t, survivors["sh.helm.release.v1."+testRelease+".v1"], "compressed decodable record must survive")
	assert.True(t, survivors["sh.helm.release.v1."+testRelease+".v2"], "legacy uncompressed decodable record must survive")
}

// minimalIndexYAML is the smallest document helm's repo.LoadIndex accepts.
const minimalIndexYAML = "apiVersion: v1\nentries: {}\ngenerated: \"2026-01-01T00:00:00Z\"\n"

// isolatedHelmSettings redirects helm's process-wide repository file and
// index cache into the test's temp tree so tests never touch the real
// ~/.config/helm state. AddOrUpdateChartRepo aligns the download's CachePath
// with settings.RepositoryCache, so both the repository file and the cached
// index land under these directories.
func isolatedHelmSettings(t *testing.T) {
	t.Helper()
	t.Setenv("HELM_CONFIG_HOME", t.TempDir())
	t.Setenv("HELM_DATA_HOME", t.TempDir())
	t.Setenv("HELM_REPOSITORY_CONFIG", filepath.Join(t.TempDir(), "repositories.yaml"))
	t.Setenv("HELM_REPOSITORY_CACHE", filepath.Join(t.TempDir(), "cache"))
}

// indexServer serves a valid chart-repository index and invokes hook on every
// index request.
func indexServer(t *testing.T, hook func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hook(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeIndex answers with a valid, empty index.
func writeIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/x-yaml")
	fmt.Fprint(w, minimalIndexYAML)
}

// TestAddOrUpdateChartRepoDownloadsInParallelPerRepo pins the M10 fix: the
// index download is a network call and must NOT run under the global
// repository-file lock. Two repos with different names download through the
// same server; the handler only succeeds once both downloads are in flight
// simultaneously. With the old global lock the second download could not
// start until the first returned, the barrier times out and the download
// fails.
func TestAddOrUpdateChartRepoDownloadsInParallelPerRepo(t *testing.T) {
	isolatedHelmSettings(t)

	var mu sync.Mutex
	inside := 0
	bothInFlight := make(chan struct{})
	var once sync.Once

	srv := indexServer(t, func(w http.ResponseWriter) {
		mu.Lock()
		inside++
		if inside == 2 {
			once.Do(func() { close(bothInFlight) })
		}
		mu.Unlock()

		select {
		case <-bothInFlight:
		case <-time.After(10 * time.Second):
			// The other download never started: the global lock is being
			// held across the network call.
			http.Error(w, "second index download never started (global lock held across network call)", http.StatusGatewayTimeout)
			return
		}
		writeIndex(w)
	})

	client := NewClient()
	names := []string{"repo-a", "repo-b"}
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			errs[i] = client.AddOrUpdateChartRepo(repo.Entry{Name: name, URL: srv.URL})
		}(i, name)
	}
	wg.Wait()

	for i, name := range names {
		require.NoError(t, errs[i], "repo %q download failed", name)
		_, err := os.Stat(filepath.Join(os.Getenv("HELM_REPOSITORY_CACHE"), helmpath.CacheIndexFile(name)))
		require.NoError(t, err, "index for %q must be cached in settings.RepositoryCache", name)
	}
}

// TestAddOrUpdateChartRepoSerializesSameRepoIndexDownload pins the companion
// guarantee of the M10 fix: concurrent downloads of the SAME repository are
// serialised per repo name, because helm writes the index cache with a
// non-atomic os.WriteFile and chart resolution reads that same file.
func TestAddOrUpdateChartRepoSerializesSameRepoIndexDownload(t *testing.T) {
	isolatedHelmSettings(t)

	var mu sync.Mutex
	active := 0
	overlap := false

	srv := indexServer(t, func(w http.ResponseWriter) {
		mu.Lock()
		active++
		if active > 1 {
			overlap = true
		}
		mu.Unlock()

		time.Sleep(150 * time.Millisecond) // widen the window so overlap is detectable

		mu.Lock()
		active--
		mu.Unlock()
		writeIndex(w)
	})

	client := NewClient()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = client.AddOrUpdateChartRepo(repo.Entry{Name: "repo-a", URL: srv.URL})
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.False(t, overlap, "same-repo index downloads must not run concurrently (torn cache write risk)")
}

// TestAddOrUpdateChartRepoIndexDownloadIsBounded pins the second half of the
// M10 fix: the index download must be bounded by repoIndexDownloadTimeout,
// not helm's getter default of 120s (getter.DefaultHTTPTimeout). The server
// never answers within the shrunken timeout, so the call must fail fast
// instead of hanging for the default budget.
func TestAddOrUpdateChartRepoIndexDownloadIsBounded(t *testing.T) {
	isolatedHelmSettings(t)

	srv := indexServer(t, func(w http.ResponseWriter) {
		time.Sleep(2 * time.Second) // longer than the shrunken timeout
		writeIndex(w)
	})

	client := NewClient()
	client.repoIndexDownloadTimeout = 200 * time.Millisecond

	start := time.Now()
	err := client.AddOrUpdateChartRepo(repo.Entry{Name: "repo-a", URL: srv.URL})
	elapsed := time.Since(start)

	require.Error(t, err, "a download past the timeout must fail, not hang")
	assert.Less(t, elapsed, 1500*time.Millisecond,
		"the download must fail promptly, not after helm's 120s getter default")
	assert.Contains(t, err.Error(), "cannot be reached")
}
