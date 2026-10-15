package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// WatchFloatingIP Tests (REPORT.md 4.1)
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
// immediately with NotFound instead of polling until the deadline (REPORT.md
// 4.1: ~300 pointless Gets per hopeless request).
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
// Lost-update / conflict retry Tests (REPORT.md 4.2)
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
