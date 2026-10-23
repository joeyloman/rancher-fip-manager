package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeyloman/rancher-fip-api-server/internal/kubebuilder"
	"github.com/joeyloman/rancher-fip-api-server/pkg/types"
	fipv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// projectsGVR mirrors the management.cattle.io project resource used by the
// handlers' project lookup.
var projectsGVR = schema.GroupVersionResource{
	Group:    "management.cattle.io",
	Version:  "v3",
	Resource: "projects",
}

// createNamespaceAndProject creates a namespace plus a Rancher Project object
// named projectName in it, as the handlers expect.
func createNamespaceAndProject(t *testing.T, prefix, projectName string) *corev1.Namespace {
	t.Helper()
	ctx := context.Background()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, ns))
	t.Cleanup(func() { app.FipRestClient.Delete(context.Background(), ns) })

	project := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "management.cattle.io/v3",
			"kind":       "Project",
			"metadata": map[string]interface{}{
				"name":      projectName,
				"namespace": ns.GetName(),
			},
		},
	}
	_, err := app.DynamicClient.Resource(projectsGVR).Namespace(ns.GetName()).Create(ctx, project, metav1.CreateOptions{})
	require.NoError(t, err)

	return ns
}

// setFIPStatusLater waits delay, then applies mutate to the FloatingIP's
// status subresource, retrying until it lands or giveUp elapses. It simulates
// the manager controller allocating the IP while a request is in flight.
func setFIPStatusLater(t *testing.T, delay time.Duration, namespace, name string, mutate func(*fipv1beta2.FloatingIP)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			cur := &fipv1beta2.FloatingIP{}
			if err := app.FipRestClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cur); err == nil {
				mutate(cur)
				if err := app.FipRestClient.Status().Update(ctx, cur); err == nil {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { <-done })
}

// =============================================================================
// WatchFloatingIP Tests
// =============================================================================

// TestWatchFloatingIPReturnsAllocation verifies the watch observes a status
// update made after the watch started. If event delivery or decoding were
// broken the initial Get would never see the allocation and the 25s deadline
// would fail the test.
func TestWatchFloatingIPReturnsAllocation(t *testing.T) {
	ns := createNamespaceAndProject(t, "test-watch-ok-", "proj-watch-ok")
	ipAddr := "10.30.0.7"

	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{Name: "fip-watch-ok", Namespace: ns.GetName()},
		Spec:       fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-watch-ok"},
	}
	require.NoError(t, app.FipRestClient.Create(context.Background(), fip))

	setFIPStatusLater(t, 300*time.Millisecond, ns.GetName(), "fip-watch-ok", func(f *fipv1beta2.FloatingIP) {
		f.Status.IPAddr = ipAddr
		f.Status.State = "Allocated"
	})

	watchCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	got, err := kubebuilder.WatchFloatingIP(watchCtx, app.FipWatchClient, fip)
	require.NoError(t, err)
	assert.Equal(t, ipAddr, got.Status.IPAddr)
}

// TestWatchFloatingIPNotFoundIsTerminal verifies a missing FloatingIP aborts
// immediately with NotFound instead of polling until the deadline (~300
// pointless Gets per hopeless request).
func TestWatchFloatingIPNotFoundIsTerminal(t *testing.T) {
	ns := createNamespaceAndProject(t, "test-watch-nf-", "proj-watch-nf")

	watchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	got, err := kubebuilder.WatchFloatingIP(watchCtx, app.FipWatchClient, &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.GetName(), Name: "no-such-fip"},
	})
	require.Error(t, err)
	assert.True(t, k8serrors.IsNotFound(err), "expected NotFound, got %v", err)
	assert.Nil(t, got)
	assert.Less(t, time.Since(start), 5*time.Second, "NotFound must return immediately, not poll until the deadline")
}

// TestWatchFloatingIPWaitsForFloatingIPGroup verifies that with the
// floatingip-group label set, the watch does not return on IPAddr alone but
// waits for Status.Assigned.FloatingIPGroup as well.
func TestWatchFloatingIPWaitsForFloatingIPGroup(t *testing.T) {
	ns := createNamespaceAndProject(t, "test-watch-grp-", "proj-watch-grp")
	ipAddr := "10.30.1.7"

	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-watch-grp",
			Namespace: ns.GetName(),
			Labels:    map[string]string{"rancher.k8s.binbash.org/floatingip-group": "group-1"},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-watch-grp"},
	}
	require.NoError(t, app.FipRestClient.Create(context.Background(), fip))

	// Status has the address but no assignment yet; the group arrives later.
	setFIPStatusLater(t, 0, ns.GetName(), "fip-watch-grp", func(f *fipv1beta2.FloatingIP) {
		f.Status.IPAddr = ipAddr
		f.Status.State = "Allocated"
	})

	setFIPStatusLater(t, 400*time.Millisecond, ns.GetName(), "fip-watch-grp", func(f *fipv1beta2.FloatingIP) {
		f.Status.IPAddr = ipAddr
		f.Status.State = "Allocated"
		f.Status.Assigned = &fipv1beta2.AssignedInfo{ClusterName: "test-cluster", FloatingIPGroup: "group-1"}
	})

	watchCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	got, err := kubebuilder.WatchFloatingIP(watchCtx, app.FipWatchClient, fip)
	require.NoError(t, err)
	require.NotNil(t, got.Status.Assigned)
	assert.Equal(t, "group-1", got.Status.Assigned.FloatingIPGroup)
}

// TestFIPRequestHandlerTimeoutKeepsReusedFIP verifies that when the allocation
// watch times out, a FloatingIP that existed before the request is NOT deleted
// — only resources created by this very request may be cleaned up. Before the
// fix, the timeout destroyed pre-existing FloatingIPs whose allocation lagged
// behind the controller.
func TestFIPRequestHandlerTimeoutKeepsReusedFIP(t *testing.T) {
	const projectName = "proj-reuse-timeout"
	ns := createNamespaceAndProject(t, "test-reuse-", projectName)

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 1 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	ctx := context.Background()
	ipAddr := "10.31.0.5"
	// Pre-existing FIP for the requested static IP whose status allocation has
	// not landed yet (controller backlog) — the watch will therefore time out.
	existing := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-to-reuse",
			Namespace: ns.GetName(),
			Labels:    map[string]string{"rancher.k8s.binbash.org/project-name": projectName},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-reuse"},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, existing))

	fipRequest := &types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-reuse",
		ServiceName:      "svc-reuse",
		ServiceNamespace: "default",
		Cluster:          "cluster-reuse",
		IPAddress:        ipAddr,
	}
	body, err := json.Marshal(fipRequest)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, req)

	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.Contains(t, w.Body.String(), "timeout waiting for IP allocation")

	// The pre-existing FloatingIP must survive the timeout...
	got := &fipv1beta2.FloatingIP{}
	require.NoError(t, app.FipRestClient.Get(ctx, client.ObjectKey{Namespace: ns.GetName(), Name: "fip-to-reuse"}, got),
		"a reused pre-existing FloatingIP must not be deleted on timeout")
	// ...and must carry the labels this request applied before the watch.
	assert.Equal(t, "cluster-reuse", got.Labels["rancher.k8s.binbash.org/cluster-name"])
	assert.Equal(t, "svc-reuse", got.Labels["rancher.k8s.binbash.org/service-0-name"])
}

// TestFIPRequestHandlerClientDisconnectDeletesCreatedFIP verifies that when the
// client disconnects while the allocation watch is pending, the FloatingIP
// created by this request is cleaned up with a bounded background delete
// (before the fix disconnects leaked orphaned FIPs) and no response is written.
func TestFIPRequestHandlerClientDisconnectDeletesCreatedFIP(t *testing.T) {
	const projectName = "proj-disconnect"
	ns := createNamespaceAndProject(t, "test-disconnect-", projectName)

	fipRequest := &types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-disconnect",
		ServiceName:      "svc-disconnect",
		ServiceNamespace: "default",
		Cluster:          "cluster-disconnect",
		IPAddress:        "10.32.0.9",
	}
	body, err := json.Marshal(fipRequest)
	require.NoError(t, err)

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)).WithContext(reqCtx)

	// Disconnect while the watch is pending (the create has long landed).
	go func() {
		time.Sleep(2 * time.Second)
		reqCancel()
	}()

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, req)

	// Nothing is written for a disconnected client.
	assert.Empty(t, w.Body.String())

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, app.FipRestClient.List(context.Background(), fipList, &client.ListOptions{Namespace: ns.GetName()}))
	assert.Len(t, fipList.Items, 0, "a FloatingIP created by this request must be deleted when the client disconnects")
}

// =============================================================================
// Lost-update / conflict retry Tests
// =============================================================================

// conflictOnceClient injects a stale resourceVersion into the first Update to
// simulate a concurrent writer winning the race.
type conflictOnceClient struct {
	client.Client
	injected bool
}

func (c *conflictOnceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if !c.injected {
		c.injected = true
		obj.SetResourceVersion("999999999")
	}
	return c.Client.Update(ctx, obj, opts...)
}

// alwaysStaleClient forces every Update to conflict with the API server.
type alwaysStaleClient struct {
	client.Client
}

func (c alwaysStaleClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	obj.SetResourceVersion("999999999")
	return c.Client.Update(ctx, obj, opts...)
}

// TestUpdateFloatingIPRetriesOnConflict verifies the read-modify-write helper
// re-fetches and re-applies on a 409 so neither the concurrent writer's change
// nor this request's labels are lost, and no error surfaces.
func TestUpdateFloatingIPRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	ns := createNamespaceAndProject(t, "test-conflict-", "proj-conflict")
	key := client.ObjectKey{Namespace: ns.GetName(), Name: "fip-conflict"}

	ipAddr := "10.33.0.1"
	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-conflict",
			Namespace: ns.GetName(),
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name":        "proj-conflict",
				"rancher.k8s.binbash.org/cluster-name":        "cluster-a",
				"rancher.k8s.binbash.org/service-0-name":      "svc-a",
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-conflict"},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, fip))

	// Stale snapshot taken the way the handler takes it (from a List).
	stale := fip.DeepCopy()

	// A concurrent writer persists a change after the snapshot was taken.
	require.NoError(t, app.FipRestClient.Get(ctx, key, fip))
	fip.Annotations = map[string]string{"rancher.k8s.binbash.org/concurrent": "true"}
	require.NoError(t, app.FipRestClient.Update(ctx, fip))

	wrapper := &conflictOnceClient{Client: app.FipRestClient}
	err := updateFloatingIP(ctx, wrapper, stale, func(f *fipv1beta2.FloatingIP) {
		applyFIPRequestLabels(f, "proj-conflict", "cluster-b", "svc-b", "default", "")
	})
	require.NoError(t, err, "a single conflict must be retried transparently")
	require.True(t, wrapper.injected, "the test must have injected a conflict")

	final := &fipv1beta2.FloatingIP{}
	require.NoError(t, app.FipRestClient.Get(ctx, key, final))
	assert.Equal(t, "true", final.Annotations["rancher.k8s.binbash.org/concurrent"], "the concurrent write must survive")
	assert.Equal(t, "svc-a", final.Labels["rancher.k8s.binbash.org/service-0-name"], "the original service label must survive")
	assert.Equal(t, "svc-b", final.Labels["rancher.k8s.binbash.org/service-1-name"], "this request's service label must land")
	assert.Equal(t, "cluster-b", final.Labels["rancher.k8s.binbash.org/cluster-name"])
}

// TestUpdateFloatingIPSurfacesPersistentConflict verifies retries are bounded
// and a permanently conflicting object yields a Conflict error (which the
// handlers map to HTTP 409) instead of looping forever.
func TestUpdateFloatingIPSurfacesPersistentConflict(t *testing.T) {
	ctx := context.Background()
	ns := createNamespaceAndProject(t, "test-conflict-exhaust-", "proj-conflict-exhaust")
	key := client.ObjectKey{Namespace: ns.GetName(), Name: "fip-conflict-exhaust"}

	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{Name: "fip-conflict-exhaust", Namespace: ns.GetName()},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, fip))
	stale := fip.DeepCopy()

	start := time.Now()
	err := updateFloatingIP(ctx, alwaysStaleClient{Client: app.FipRestClient}, stale, func(f *fipv1beta2.FloatingIP) {
		applyFIPRequestLabels(f, "proj-conflict-exhaust", "cluster-x", "svc-x", "default", "")
	})
	require.Error(t, err)
	assert.True(t, k8serrors.IsConflict(err), "expected Conflict after exhausted retries, got %v", err)
	assert.Less(t, time.Since(start), 10*time.Second, "retries must be bounded")

	// The object was never modified.
	final := &fipv1beta2.FloatingIP{}
	require.NoError(t, app.FipRestClient.Get(ctx, key, final))
	assert.NotContains(t, final.Labels, "rancher.k8s.binbash.org/service-0-name")
}

// =============================================================================
// Atomic claim of an unassigned FloatingIP
// =============================================================================

// createFloatingIPPool creates the cluster-scoped pool the request handlers
// read the subnet from.
func createFloatingIPPool(t *testing.T, name, subnet, start, end string) {
	t.Helper()
	fipPoolGVR := schema.GroupVersionResource{
		Group:    "rancher.k8s.binbash.org",
		Version:  "v1beta2",
		Resource: "floatingippools",
	}
	fipPool := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rancher.k8s.binbash.org/v1beta2",
			"kind":       "FloatingIPPool",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"ipConfig": map[string]interface{}{
					"family": "IPv4",
					"subnet": subnet,
					"pool": map[string]interface{}{
						"start": start,
						"end":   end,
					},
				},
			},
		},
	}
	_, err := app.DynamicClient.Resource(fipPoolGVR).Create(context.Background(), fipPool, metav1.CreateOptions{})
	// Cluster-scoped and identical across -count runs; the handler only reads
	// the subnet.
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// createUnassignedFIP creates a FloatingIP with an allocated address and no
// assignment, the state the auto-allocation path selects from.
func createUnassignedFIP(t *testing.T, namespace, name, project, pool, ipAddr string) *fipv1beta2.FloatingIP {
	t.Helper()
	ctx := context.Background()
	addr := ipAddr
	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"rancher.k8s.binbash.org/project-name": project},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &addr, FloatingIPPool: pool},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, fip))
	fip.Status.IPAddr = addr
	fip.Status.State = "Allocated"
	require.NoError(t, app.FipRestClient.Status().Update(ctx, fip))
	return fip
}

// listBarrierClient blocks every List until want callers have entered it, so
// concurrent handler invocations all build their candidate snapshots before
// any of them writes. That turns the List-then-claim window into a
// deterministic race instead of a probabilistic one.
type listBarrierClient struct {
	client.Client
	want    int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newListBarrierClient(inner client.Client, want int) *listBarrierClient {
	return &listBarrierClient{Client: inner, want: want, release: make(chan struct{})}
}

func (c *listBarrierClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.mu.Lock()
	c.arrived++
	arrived := c.arrived
	if c.arrived >= c.want {
		select {
		case <-c.release:
		default:
			close(c.release)
		}
	}
	c.mu.Unlock()
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return fmt.Errorf("list barrier timed out after %d of %d callers", arrived, c.want)
	}
	return c.Client.List(ctx, list, opts...)
}

// runConcurrentNoIPRequests fires one auto-allocation request per service at
// the same time and returns their responses in order.
func runConcurrentNoIPRequests(t *testing.T, project, pool, cluster string, services ...string) []*httptest.ResponseRecorder {
	t.Helper()
	recorders := make([]*httptest.ResponseRecorder, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Add(1)
		go func(i int, svc string) {
			defer wg.Done()
			body, err := json.Marshal(&types.FIPRequest{
				Project:          project,
				FloatingIPPool:   pool,
				Cluster:          cluster,
				ServiceName:      svc,
				ServiceNamespace: "default",
			})
			if err != nil {
				t.Errorf("marshal request: %v", err)
				return
			}
			w := httptest.NewRecorder()
			FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))
			recorders[i] = w
		}(i, svc)
	}
	wg.Wait()
	return recorders
}

// serviceNamesOnFIP returns the service-<id>-name label values of fip.
func serviceNamesOnFIP(fip *fipv1beta2.FloatingIP) []string {
	var names []string
	for key, val := range fip.Labels {
		if strings.HasPrefix(key, "rancher.k8s.binbash.org/service-") && strings.HasSuffix(key, "-name") {
			names = append(names, val)
		}
	}
	return names
}

// TestFIPRequestHandlerConcurrentNoIPRequestsDoNotShareSingleFIP pins the
// atomic-claim fix: two simultaneous auto-allocation requests must not claim
// the same unassigned FloatingIP. Before the fix both handlers wrote their
// service labels onto the single candidate (their conflict retries re-applied
// the labels without re-checking the claim) and both clients were handed the
// same address.
func TestFIPRequestHandlerConcurrentNoIPRequestsDoNotShareSingleFIP(t *testing.T) {
	const projectName = "proj-claim-single"
	ns := createNamespaceAndProject(t, "test-claim-single-", projectName)
	createFloatingIPPool(t, "pool-claim-single", "10.51.0.0/24", "10.51.0.1", "10.51.0.254")
	createUnassignedFIP(t, ns.GetName(), "fip-claim-single", projectName, "pool-claim-single", "10.51.0.10")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 10 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	inner := app.FipRestClient

	// Stand in for the manager: allocate an address for any FIP this request
	// creates (the pre-existing candidate already carries one).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			list := &fipv1beta2.FloatingIPList{}
			if err := inner.List(context.Background(), list, client.InNamespace(ns.GetName())); err != nil {
				continue
			}
			for i := range list.Items {
				f := list.Items[i]
				if f.Status.IPAddr != "" {
					continue
				}
				f.Status.IPAddr = "10.51.0.20"
				f.Status.State = "Allocated"
				_ = inner.Status().Update(context.Background(), &f)
			}
		}
	}()

	app.FipRestClient = newListBarrierClient(inner, 2)
	defer func() { app.FipRestClient = inner }()

	responses := runConcurrentNoIPRequests(t, projectName, "pool-claim-single", "cluster-claim", "svc-a", "svc-b")

	ips := make([]string, len(responses))
	for i, w := range responses {
		require.Equal(t, http.StatusOK, w.Code, "request %d failed: %s", i, w.Body.String())
		var resp types.FIPResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		ips[i] = resp.IPAddress
	}
	require.NotEqual(t, ips[0], ips[1], "concurrent requests must not be handed the same floating IP")

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, inner.List(context.Background(), fipList, client.InNamespace(ns.GetName())))
	require.Len(t, fipList.Items, 2, "the losing request must create its own FloatingIP")
	for i := range fipList.Items {
		assert.Len(t, serviceNamesOnFIP(&fipList.Items[i]), 1,
			"FloatingIP %s must carry exactly one service, got %v", fipList.Items[i].Name, serviceNamesOnFIP(&fipList.Items[i]))
	}
}

// TestFIPRequestHandlerConcurrentNoIPRequestsClaimDistinctFIPs verifies that
// with two unassigned FloatingIPs available, two simultaneous requests end up
// on one FIP each instead of both taking the first candidate.
func TestFIPRequestHandlerConcurrentNoIPRequestsClaimDistinctFIPs(t *testing.T) {
	const projectName = "proj-claim-two"
	ns := createNamespaceAndProject(t, "test-claim-two-", projectName)
	createFloatingIPPool(t, "pool-claim-two", "10.50.0.0/24", "10.50.0.1", "10.50.0.254")
	createUnassignedFIP(t, ns.GetName(), "fip-claim-two-a", projectName, "pool-claim-two", "10.50.0.10")
	createUnassignedFIP(t, ns.GetName(), "fip-claim-two-b", projectName, "pool-claim-two", "10.50.0.11")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 10 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	inner := app.FipRestClient
	app.FipRestClient = newListBarrierClient(inner, 2)
	defer func() { app.FipRestClient = inner }()

	responses := runConcurrentNoIPRequests(t, projectName, "pool-claim-two", "cluster-claim", "svc-a", "svc-b")

	ips := map[string]bool{}
	for i, w := range responses {
		require.Equal(t, http.StatusOK, w.Code, "request %d failed: %s", i, w.Body.String())
		var resp types.FIPResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		ips[resp.IPAddress] = true
	}
	assert.Equal(t, map[string]bool{"10.50.0.10": true, "10.50.0.11": true}, ips,
		"each concurrent request must claim a different unassigned FloatingIP")

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, inner.List(context.Background(), fipList, client.InNamespace(ns.GetName())))
	require.Len(t, fipList.Items, 2)
	for i := range fipList.Items {
		assert.Len(t, serviceNamesOnFIP(&fipList.Items[i]), 1,
			"FloatingIP %s must carry exactly one service, got %v", fipList.Items[i].Name, serviceNamesOnFIP(&fipList.Items[i]))
	}
}

// TestFIPRequestHandlerPinnedIPDoesNotStealPendingClaim verifies that a
// specific-IP request from another cluster cannot overwrite a claim whose
// Status.Assigned has not been published yet. The cluster-name label is
// written together with the service labels, so it is the authoritative claim.
func TestFIPRequestHandlerPinnedIPDoesNotStealPendingClaim(t *testing.T) {
	const projectName = "proj-pinned-claim"
	ns := createNamespaceAndProject(t, "test-pinned-claim-", projectName)
	createFloatingIPPool(t, "pool-pinned-claim", "10.52.0.0/24", "10.52.0.1", "10.52.0.254")

	ctx := context.Background()
	ipAddr := "10.52.0.10"
	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-pinned-claim",
			Namespace: ns.GetName(),
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name":        projectName,
				"rancher.k8s.binbash.org/cluster-name":        "cluster-a",
				"rancher.k8s.binbash.org/service-0-name":      "svc-a",
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-pinned-claim"},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, fip))
	fip.Status.IPAddr = ipAddr
	fip.Status.State = "Allocated"
	require.NoError(t, app.FipRestClient.Status().Update(ctx, fip))

	body, err := json.Marshal(&types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-pinned-claim",
		Cluster:          "cluster-b",
		ServiceName:      "svc-b",
		ServiceNamespace: "default",
		IPAddress:        ipAddr,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "already in use by another cluster")

	got := &fipv1beta2.FloatingIP{}
	require.NoError(t, app.FipRestClient.Get(ctx, client.ObjectKey{Namespace: ns.GetName(), Name: "fip-pinned-claim"}, got))
	assert.Equal(t, "cluster-a", got.Labels["rancher.k8s.binbash.org/cluster-name"], "the pending claim must not be overwritten")
	assert.Equal(t, []string{"svc-a"}, serviceNamesOnFIP(got), "the competing service must not be added")
}

// =============================================================================
// List-then-Create race on specific-IP requests
// =============================================================================

// createErrorClient fails every Create with err, standing in for the
// validating webhook's admission denial.
type createErrorClient struct {
	client.Client
	err error
}

func (c *createErrorClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return c.err
}

// runPinnedIPRequests fires one pinned-address request per (cluster, service)
// pair at the same time and returns their responses in order.
func runPinnedIPRequests(t *testing.T, project, pool, ipAddr string, clusters, services []string) []*httptest.ResponseRecorder {
	t.Helper()
	require.Equal(t, len(clusters), len(services))
	recorders := make([]*httptest.ResponseRecorder, len(services))
	var wg sync.WaitGroup
	for i := range services {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := json.Marshal(&types.FIPRequest{
				Project:          project,
				FloatingIPPool:   pool,
				Cluster:          clusters[i],
				ServiceName:      services[i],
				ServiceNamespace: "default",
				IPAddress:        ipAddr,
			})
			if err != nil {
				t.Errorf("marshal request: %v", err)
				return
			}
			w := httptest.NewRecorder()
			FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))
			recorders[i] = w
		}(i)
	}
	wg.Wait()
	return recorders
}

// startAllocationStandIn simulates the manager controller granting an address
// to any FloatingIP created by the test that has no status address yet.
func startAllocationStandIn(t *testing.T, namespace, ipAddr string) {
	t.Helper()
	inner := app.FipRestClient
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			list := &fipv1beta2.FloatingIPList{}
			if err := inner.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
				continue
			}
			for i := range list.Items {
				f := list.Items[i]
				if f.Status.IPAddr != "" {
					continue
				}
				f.Status.IPAddr = ipAddr
				f.Status.State = "Allocated"
				_ = inner.Status().Update(context.Background(), &f)
			}
		}
	}()
}

// TestFIPRequestHandlerConcurrentPinnedIPRequestsCreateOneFIP pins the
// create-collision fix: two simultaneous requests for the same absent address
// must end up on one FloatingIP, not two. Before the fix both handlers List-ed
// nothing and both created a resource (GenerateName), leaving two CRs claiming
// one address.
func TestFIPRequestHandlerConcurrentPinnedIPRequestsCreateOneFIP(t *testing.T) {
	const projectName = "proj-pinned-create"
	ns := createNamespaceAndProject(t, "test-pinned-create-", projectName)
	createFloatingIPPool(t, "pool-pinned-create", "10.53.0.0/24", "10.53.0.1", "10.53.0.254")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 10 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	startAllocationStandIn(t, ns.GetName(), "10.53.0.10")

	inner := app.FipRestClient
	app.FipRestClient = newListBarrierClient(inner, 2)
	defer func() { app.FipRestClient = inner }()

	responses := runPinnedIPRequests(t, projectName, "pool-pinned-create", "10.53.0.10",
		[]string{"cluster-pinned", "cluster-pinned"}, []string{"svc-a", "svc-b"})

	for i, w := range responses {
		require.Equal(t, http.StatusOK, w.Code, "request %d failed: %s", i, w.Body.String())
		var resp types.FIPResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "10.53.0.10", resp.IPAddress)
	}

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, inner.List(context.Background(), fipList, client.InNamespace(ns.GetName())))
	require.Len(t, fipList.Items, 1, "concurrent pinned requests must persist exactly one FloatingIP")
	assert.ElementsMatch(t, []string{"svc-a", "svc-b"}, serviceNamesOnFIP(&fipList.Items[0]),
		"both services must land on the single FloatingIP")
}

// TestFIPRequestHandlerConcurrentPinnedIPRequestsDifferentClustersConflict
// verifies the create collision is resolved through the claim checks: a second
// cluster requesting an address another cluster is concurrently claiming gets
// a 409 and never lands on the winner's FloatingIP.
func TestFIPRequestHandlerConcurrentPinnedIPRequestsDifferentClustersConflict(t *testing.T) {
	const projectName = "proj-pinned-clash"
	ns := createNamespaceAndProject(t, "test-pinned-clash-", projectName)
	createFloatingIPPool(t, "pool-pinned-clash", "10.54.0.0/24", "10.54.0.1", "10.54.0.254")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 10 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	startAllocationStandIn(t, ns.GetName(), "10.54.0.10")

	inner := app.FipRestClient
	app.FipRestClient = newListBarrierClient(inner, 2)
	defer func() { app.FipRestClient = inner }()

	clusters := []string{"cluster-x", "cluster-y"}
	services := []string{"svc-x", "svc-y"}
	responses := runPinnedIPRequests(t, projectName, "pool-pinned-clash", "10.54.0.10", clusters, services)

	codes := map[int]int{}
	winner := -1
	for i, w := range responses {
		codes[w.Code]++
		if w.Code == http.StatusOK {
			winner = i
		}
	}
	assert.Equal(t, 1, codes[http.StatusOK], "exactly one cluster may claim the address: %v", codes)
	assert.Equal(t, 1, codes[http.StatusConflict], "the losing cluster must get a retryable 409: %v", codes)
	require.NotEqual(t, -1, winner)

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, inner.List(context.Background(), fipList, client.InNamespace(ns.GetName())))
	require.Len(t, fipList.Items, 1)
	assert.Equal(t, clusters[winner], fipList.Items[0].Labels["rancher.k8s.binbash.org/cluster-name"],
		"the winning cluster's claim must survive")
	assert.Equal(t, []string{services[winner]}, serviceNamesOnFIP(&fipList.Items[0]),
		"the losing cluster's service must not be added")
}

// TestFIPRequestHandlerPinnedIPAdmissionDenialIsClientError pins the
// admission-denial half: a validating-webhook rejection with no FloatingIP to
// adopt (a quota or pool rejection) keeps the API server's 4xx code and message
// instead of a raw 500 carrying API internals.
func TestFIPRequestHandlerPinnedIPAdmissionDenialIsClientError(t *testing.T) {
	const projectName = "proj-pinned-denied"
	ns := createNamespaceAndProject(t, "test-pinned-denied-", projectName)
	createFloatingIPPool(t, "pool-pinned-denied", "10.55.0.0/24", "10.55.0.1", "10.55.0.254")

	inner := app.FipRestClient
	app.FipRestClient = &createErrorClient{
		Client: inner,
		err: &k8serrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusBadRequest,
			Message: `admission webhook "rancher-fip-manager-validator" denied the request: project quota exceeded for pool pool-pinned-denied`,
		}},
	}
	defer func() { app.FipRestClient = inner }()

	body, err := json.Marshal(&types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-pinned-denied",
		Cluster:          "cluster-denied",
		ServiceName:      "svc-denied",
		ServiceNamespace: "default",
		IPAddress:        "10.55.0.10",
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	assert.Equal(t, http.StatusBadRequest, w.Code, "the webhook's denial code must reach the client, body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "denied the request")
	assert.Contains(t, w.Body.String(), "quota exceeded")

	got := &fipv1beta2.FloatingIP{}
	err = inner.Get(context.Background(), client.ObjectKey{
		Namespace: ns.GetName(),
		Name:      specificFIPName("pool-pinned-denied", "10.55.0.10"),
	}, got)
	assert.True(t, k8serrors.IsNotFound(err), "a denied create must not persist a FloatingIP, got %v", err)
}

// interceptCreateClient replaces Create with fn, so a test can persist a
// concurrent winner's FloatingIP and then return the webhook denial the real
// admission chain would produce for the loser.
type interceptCreateClient struct {
	client.Client
	fn func(context.Context) error
}

func (c *interceptCreateClient) Create(ctx context.Context, _ client.Object, _ ...client.CreateOption) error {
	return c.fn(ctx)
}

// persistConcurrentPinnedFIP writes the FloatingIP a winning request would
// have persisted for (project, pool, ip) under its deterministic name, with
// service svc-winner and optionally an allocated status address.
func persistConcurrentPinnedFIP(t *testing.T, cl client.Client, namespace, project, pool, ipAddr, cluster, svc string, allocated bool) *fipv1beta2.FloatingIP {
	t.Helper()
	ctx := context.Background()
	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      specificFIPName(pool, ipAddr),
			Namespace: namespace,
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name":        project,
				"rancher.k8s.binbash.org/cluster-name":        cluster,
				"rancher.k8s.binbash.org/service-0-name":      svc,
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: pool},
	}
	require.NoError(t, cl.Create(ctx, fip))
	if allocated {
		fip.Status.IPAddr = ipAddr
		fip.Status.State = "Allocated"
		require.NoError(t, cl.Status().Update(ctx, fip))
	}
	return fip
}

// webhookDuplicateIPDenial is the StatusError the API server returns when the
// validating webhook's duplicate-IP reservation denies a concurrent create.
func webhookDuplicateIPDenial(ipAddr string) error {
	return &k8serrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    http.StatusBadRequest,
		Message: fmt.Sprintf(`admission webhook "rancher-fip-manager-validator" denied the request: requested IP %s is pending allocation in floatingippool`, ipAddr),
	}}
}

// TestFIPRequestHandlerPinnedIPDeniedCreateAdoptsConcurrentFIP verifies the
// create-failure re-list: the loser of a concurrent pinned-IP race is denied
// by the webhook's reservation (admission runs before persistence, so it never
// sees AlreadyExists) and must adopt the winner's FloatingIP instead of
// failing, sharing the address as the same cluster.
func TestFIPRequestHandlerPinnedIPDeniedCreateAdoptsConcurrentFIP(t *testing.T) {
	const projectName = "proj-pinned-adopt"
	ns := createNamespaceAndProject(t, "test-pinned-adopt-", projectName)
	createFloatingIPPool(t, "pool-pinned-adopt", "10.56.0.0/24", "10.56.0.1", "10.56.0.254")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 10 * time.Second
	defer func() { allocationWaitTimeout = origTimeout }()

	inner := app.FipRestClient
	app.FipRestClient = &interceptCreateClient{
		Client: inner,
		fn: func(ctx context.Context) error {
			persistConcurrentPinnedFIP(t, inner, ns.GetName(), projectName, "pool-pinned-adopt", "10.56.0.10", "cluster-adopt", "svc-winner", true)
			return webhookDuplicateIPDenial("10.56.0.10")
		},
	}
	defer func() { app.FipRestClient = inner }()

	body, err := json.Marshal(&types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-pinned-adopt",
		Cluster:          "cluster-adopt",
		ServiceName:      "svc-loser",
		ServiceNamespace: "default",
		IPAddress:        "10.56.0.10",
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	require.Equal(t, http.StatusOK, w.Code, "the loser must adopt the winner's FloatingIP: %s", w.Body.String())
	var resp types.FIPResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "10.56.0.10", resp.IPAddress)

	fipList := &fipv1beta2.FloatingIPList{}
	require.NoError(t, inner.List(context.Background(), fipList, client.InNamespace(ns.GetName())))
	require.Len(t, fipList.Items, 1)
	assert.ElementsMatch(t, []string{"svc-winner", "svc-loser"}, serviceNamesOnFIP(&fipList.Items[0]),
		"the adopting request must add its service to the winner's FloatingIP")
}

// TestFIPRequestHandlerPinnedIPAdoptedFIPSurvivesAllocationTimeout pins the
// destructive half of the adoption rule: a FIP adopted from a concurrent
// request was not created here, so the allocation-timeout cleanup must keep it
// (createdNew stays false).
func TestFIPRequestHandlerPinnedIPAdoptedFIPSurvivesAllocationTimeout(t *testing.T) {
	const projectName = "proj-pinned-adopt-timeout"
	ns := createNamespaceAndProject(t, "test-pinned-adopt-timeout-", projectName)
	createFloatingIPPool(t, "pool-pinned-adopt-timeout", "10.57.0.0/24", "10.57.0.1", "10.57.0.254")

	origTimeout := allocationWaitTimeout
	allocationWaitTimeout = 500 * time.Millisecond
	defer func() { allocationWaitTimeout = origTimeout }()

	inner := app.FipRestClient
	app.FipRestClient = &interceptCreateClient{
		Client: inner,
		fn: func(ctx context.Context) error {
			persistConcurrentPinnedFIP(t, inner, ns.GetName(), projectName, "pool-pinned-adopt-timeout", "10.57.0.10", "cluster-adopt-timeout", "svc-winner", false)
			return webhookDuplicateIPDenial("10.57.0.10")
		},
	}
	defer func() { app.FipRestClient = inner }()

	body, err := json.Marshal(&types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-pinned-adopt-timeout",
		Cluster:          "cluster-adopt-timeout",
		ServiceName:      "svc-loser",
		ServiceNamespace: "default",
		IPAddress:        "10.57.0.10",
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	require.Equal(t, http.StatusGatewayTimeout, w.Code, "no allocation arrives: %s", w.Body.String())

	got := &fipv1beta2.FloatingIP{}
	require.NoError(t, inner.Get(context.Background(), client.ObjectKey{
		Namespace: ns.GetName(),
		Name:      specificFIPName("pool-pinned-adopt-timeout", "10.57.0.10"),
	}, got), "an adopted FloatingIP must survive the allocation-timeout cleanup")
	assert.ElementsMatch(t, []string{"svc-winner", "svc-loser"}, serviceNamesOnFIP(got))
}

// TestSpecificFIPNameCanonicalizesAddress verifies the name is a pure function
// of the webhook's uniqueness domain (pool + canonical address), so textual
// IPv6 variants collide on the API server instead of racing through.
func TestSpecificFIPNameCanonicalizesAddress(t *testing.T) {
	base := specificFIPName("pool-a", "2001:db8::1")
	assert.Equal(t, base, specificFIPName("pool-a", "2001:0db8:0000:0000:0000:0000:0000:0001"),
		"equivalent IPv6 forms must map to one name")
	assert.Equal(t, base, specificFIPName("pool-a", "2001:DB8::1"))
	assert.NotEqual(t, base, specificFIPName("pool-b", "2001:db8::1"), "pools have independent address spaces")
	assert.NotEqual(t, base, specificFIPName("pool-a", "2001:db8::2"))
	assert.Equal(t, specificFIPName("pool-a", "10.0.0.1"), specificFIPName("pool-a", "10.0.0.1"))
	assert.NotEqual(t, specificFIPName("pool-a", "10.0.0.1"), specificFIPName("pool-a", "10.0.0.10"))

	for _, name := range []string{
		specificFIPName("pool-a", "10.0.0.1"),
		specificFIPName("pool/a b", "not-an-ip"),
	} {
		assert.Empty(t, validation.IsDNS1123Subdomain(name), "name %q must be a valid object name", name)
	}
}

// TestFIPRequestHandlerReusesServiceAllocation: a repeated
// request for a Service that already holds an allocation returns that
// FloatingIP instead of allocating a second address. The controller writes its
// annotation only after the request returns, so a crash, timeout or dropped
// response leaves an identical retry with no local record of the allocation.
func TestFIPRequestHandlerReusesServiceAllocation(t *testing.T) {
	const projectName = "proj-service-reuse"
	ns := createNamespaceAndProject(t, "test-svc-reuse-", projectName)
	createFloatingIPPool(t, "pool-svc-reuse", "10.32.0.0/24", "10.32.0.1", "10.32.0.254")

	ctx := context.Background()
	ipAddr := "10.32.0.7"
	existing := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-svc-reuse",
			Namespace: ns.GetName(),
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name":        projectName,
				"rancher.k8s.binbash.org/cluster-name":        "cluster-reuse",
				"rancher.k8s.binbash.org/service-0-name":      "svc-reuse",
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-svc-reuse"},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, existing))
	existing.Status.IPAddr = ipAddr
	existing.Status.State = "Allocated"
	require.NoError(t, app.FipRestClient.Status().Update(ctx, existing))

	fipRequest := &types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-svc-reuse",
		ServiceName:      "svc-reuse",
		ServiceNamespace: "default",
		Cluster:          "cluster-reuse",
	}
	body, err := json.Marshal(fipRequest)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	require.Equal(t, http.StatusOK, w.Code)
	var fipResponse types.FIPResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fipResponse))
	assert.Equal(t, ipAddr, fipResponse.IPAddress)

	var list fipv1beta2.FloatingIPList
	require.NoError(t, app.FipRestClient.List(ctx, &list, client.InNamespace(ns.GetName())))
	assert.Len(t, list.Items, 1, "a repeated request must not allocate a second FloatingIP")
}

// TestCheckSpecificIPClaimableAllowsSameServiceReclaim: the
// manager must accept a pinned-address request when the assigned FloatingIP
// already carries the requesting Service's labels. The controller persists the
// allocation only after the request returns, so a retry after a crash in
// between otherwise loops on 409 forever.
func TestCheckSpecificIPClaimableAllowsSameServiceReclaim(t *testing.T) {
	ipAddr := "10.35.0.7"
	fip := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"rancher.k8s.binbash.org/project-name":        "p-reclaim",
			"rancher.k8s.binbash.org/cluster-name":        "cluster-reclaim",
			"rancher.k8s.binbash.org/service-0-name":      "svc-reclaim",
			"rancher.k8s.binbash.org/service-0-namespace": "default",
		}},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-reclaim"},
		Status: fipv1beta2.FloatingIPStatus{
			Assigned: &fipv1beta2.AssignedInfo{ClusterName: "cluster-reclaim"},
		},
	}

	req := types.FIPRequest{
		Project:          "p-reclaim",
		Cluster:          "cluster-reclaim",
		FloatingIPPool:   "pool-reclaim",
		ServiceName:      "svc-reclaim",
		ServiceNamespace: "default",
		IPAddress:        ipAddr,
	}
	require.NoError(t, checkSpecificIPClaimable(fip, req), "the Service that owns the address must be able to re-claim it")

	req.ServiceName = "another-svc"
	err := checkSpecificIPClaimable(fip, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IP address is already assigned")

	// A same-named Service in another cluster does not own the address.
	req.ServiceName = "svc-reclaim"
	req.Cluster = "other-cluster"
	err = checkSpecificIPClaimable(fip, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IP address is already assigned")
}

// TestFIPRequestHandlerReclaimsAssignedStaticIP is the end-to-end H3 case: a
// Service retries its pinned address after the allocation was made but before
// the controller persisted it. The FIP is already assigned, yet the request
// must return that allocation instead of a 409 that the controller retries
// forever.
func TestFIPRequestHandlerReclaimsAssignedStaticIP(t *testing.T) {
	const projectName = "proj-static-reclaim"
	ns := createNamespaceAndProject(t, "test-static-reclaim-", projectName)
	createFloatingIPPool(t, "pool-static-reclaim", "10.36.0.0/24", "10.36.0.1", "10.36.0.254")

	ctx := context.Background()
	ipAddr := "10.36.0.7"
	existing := &fipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fip-static-reclaim",
			Namespace: ns.GetName(),
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name":        projectName,
				"rancher.k8s.binbash.org/cluster-name":        "cluster-static-reclaim",
				"rancher.k8s.binbash.org/service-0-name":      "svc-static-reclaim",
				"rancher.k8s.binbash.org/service-0-namespace": "default",
			},
		},
		Spec: fipv1beta2.FloatingIPSpec{IPAddr: &ipAddr, FloatingIPPool: "pool-static-reclaim"},
	}
	require.NoError(t, app.FipRestClient.Create(ctx, existing))
	existing.Status.IPAddr = ipAddr
	existing.Status.State = "Allocated"
	existing.Status.Assigned = &fipv1beta2.AssignedInfo{ClusterName: "cluster-static-reclaim"}
	require.NoError(t, app.FipRestClient.Status().Update(ctx, existing))

	fipRequest := &types.FIPRequest{
		Project:          projectName,
		FloatingIPPool:   "pool-static-reclaim",
		ServiceName:      "svc-static-reclaim",
		ServiceNamespace: "default",
		Cluster:          "cluster-static-reclaim",
		IPAddress:        ipAddr,
	}
	body, err := json.Marshal(fipRequest)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	FIPRequestHandler(app)(w, httptest.NewRequest(http.MethodPost, "/v1/fip/request", bytes.NewReader(body)))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var fipResponse types.FIPResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fipResponse))
	assert.Equal(t, ipAddr, fipResponse.IPAddress)

	var list fipv1beta2.FloatingIPList
	require.NoError(t, app.FipRestClient.List(ctx, &list, client.InNamespace(ns.GetName())))
	assert.Len(t, list.Items, 1, "a re-claim must not allocate a second FloatingIP")
}
