package floatingip

import (
	"context"
	"testing"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

const (
	testProjectName = "test-project"
	testPoolName    = "test-pool"
	testFipName     = "test-fip"
	testNamespace   = "test-ns"
)

// fixture holds the clients, informers, and controller for a test
type fixture struct {
	t *testing.T

	clientset  *fake.Clientset
	controller *Controller
}

func newFixture(t *testing.T, fipObjects []runtime.Object, kubeObjects []runtime.Object) *fixture {
	clientset := fake.NewSimpleClientset(fipObjects...)
	kubeclient := kubefake.NewSimpleClientset(kubeObjects...)

	fipInformerFactory := informers.NewSharedInformerFactory(clientset, 0)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeclient, 0)

	ipam := ipam.New()

	// Initialize IPAM with pools from the test objects
	for _, obj := range fipObjects {
		if pool, ok := obj.(*v1beta2.FloatingIPPool); ok {
			if pool.Spec.IPConfig != nil {
				err := ipam.NewSubnet(pool.Name, pool.Spec.IPConfig.Subnet, pool.Spec.IPConfig.Pool.Start, pool.Spec.IPConfig.Pool.End)
				require.NoError(t, err)
			}
		}
	}

	controller := New(
		clientset,
		kubeclient,
		fipInformerFactory.Rancher().V1beta2().FloatingIPs(),
		fipInformerFactory.Rancher().V1beta2().FloatingIPPools(),
		fipInformerFactory.Rancher().V1beta2().FloatingIPProjectQuotas(),
		ipam,
	)

	// Populate informers
	for _, obj := range fipObjects {
		switch obj := obj.(type) {
		case *v1beta2.FloatingIP:
			fipInformerFactory.Rancher().V1beta2().FloatingIPs().Informer().GetIndexer().Add(obj)
		case *v1beta2.FloatingIPPool:
			fipInformerFactory.Rancher().V1beta2().FloatingIPPools().Informer().GetIndexer().Add(obj)
		case *v1beta2.FloatingIPProjectQuota:
			fipInformerFactory.Rancher().V1beta2().FloatingIPProjectQuotas().Informer().GetIndexer().Add(obj)
		}
	}
	for _, obj := range kubeObjects {
		if ns, ok := obj.(*corev1.Namespace); ok {
			kubeInformerFactory.Core().V1().Namespaces().Informer().GetIndexer().Add(ns)
		}
	}

	return &fixture{
		t:          t,
		clientset:  clientset,
		controller: controller,
	}
}

func (f *fixture) run(key string) error {
	return f.controller.syncHandler(context.Background(), key)
}

// Helper functions to create test objects
func newNamespace(name, projectName string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				projectNameLabel: projectName,
			},
		},
	}
}

func newProject(name string) *v1beta2.FloatingIPProjectQuota {
	return &v1beta2.FloatingIPProjectQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta2.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1beta2.FloatingIPProjectQuotaSpec{
			DisplayName: "Test Project",
		},
	}
}

func newPool(name, start, end string) *v1beta2.FloatingIPPool {
	return &v1beta2.FloatingIPPool{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta2.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1beta2.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &v1beta2.IPConfig{
				Family: "IPv4",
				Subnet: "10.0.0.0/24",
				Pool: v1beta2.Pool{
					Start: start,
					End:   end,
				},
			},
		},
	}
}

func newFip(name, namespace, poolName string) *v1beta2.FloatingIP {
	return &v1beta2.FloatingIP{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta2.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1beta2.FloatingIPSpec{
			FloatingIPPool: poolName,
		},
	}
}

func getKey(fip *v1beta2.FloatingIP, t *testing.T) string {
	key, err := cache.MetaNamespaceKeyFunc(fip)
	require.NoError(t, err, "failed to get key for fip")
	return key
}

func TestSyncHandler_AddsFinalizer(t *testing.T) {
	fip := newFip(testFipName, testNamespace, testPoolName)
	f := newFixture(t, []runtime.Object{fip}, nil)
	key := getKey(fip, t)

	err := f.run(key)
	require.NoError(t, err)

	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, updatedFip.GetFinalizers(), finalizerName, "finalizer should be added")
}

func TestSyncHandler_AllocatesIP(t *testing.T) {
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})
	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.10")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})
	key := getKey(fip, t)

	err := f.run(key)
	require.NoError(t, err)

	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, updatedFip.Spec.IPAddr, "IPAddr should be set in spec")
	assert.Equal(t, "10.0.0.10", *updatedFip.Spec.IPAddr, "allocated IP should be the first in the pool")
}

func TestSyncHandler_UpdatesStatuses(t *testing.T) {
	allocatedIP := "10.0.0.10"
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})
	fip.Spec.IPAddr = &allocatedIP

	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.20")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})
	key := getKey(fip, t)

	err := f.run(key)
	require.NoError(t, err)

	// Check FIP status
	finalFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, finalFip.Status.Assigned, "FIP status should be assigned")
	require.NotEmpty(t, finalFip.Status.Conditions, "FIP status should have conditions")
	assert.Equal(t, "Ready", finalFip.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, finalFip.Status.Conditions[0].Status)

	// Check Pool status
	finalPool, err := f.clientset.RancherV1beta2().FloatingIPPools().Get(context.Background(), testPoolName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, finalPool.Status.Allocated, allocatedIP, "pool status should show allocated IP")

	// Check Project status
	finalProject, err := f.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), testProjectName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, finalProject.Status.FloatingIPs, testPoolName)
	assert.Equal(t, 1, finalProject.Status.FloatingIPs[testPoolName].Used)
	assert.Contains(t, finalProject.Status.FloatingIPs[testPoolName].Allocated, allocatedIP)
}

func TestSyncHandler_Deletion(t *testing.T) {
	allocatedIP := "10.0.0.10"
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.Spec.IPAddr = &allocatedIP
	fip.Status.IPAddr = allocatedIP
	fip.SetFinalizers([]string{finalizerName})
	now := metav1.Now()
	fip.SetDeletionTimestamp(&now)
	fip.Status.Assigned = &v1beta2.AssignedInfo{} // Mark as assigned to trigger release logic

	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.20")
	pool.Status.Allocated = map[string]string{allocatedIP: "test-project [Test Project]"}

	project := newProject(testProjectName)
	project.Status.FloatingIPs = map[string]*v1beta2.FipInfo{
		testPoolName: {
			Used:      1,
			Allocated: map[string]string{allocatedIP: "Unassigned"},
		},
	}
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})
	key := getKey(fip, t)

	// Mark IP as used in IPAM
	_, err := f.controller.ipam.GetIP(pool.Name, allocatedIP)
	require.NoError(t, err)

	err = f.run(key)
	require.NoError(t, err)

	// Check finalizer is removed
	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updatedFip.GetFinalizers(), "finalizer should be removed")

	// Check Pool status
	finalPool, err := f.clientset.RancherV1beta2().FloatingIPPools().Get(context.Background(), testPoolName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, finalPool.Status.Allocated, allocatedIP, "IP should be released from pool status")

	// Check Project status
	finalProject, err := f.clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), testProjectName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, finalProject.Status.FloatingIPs, testPoolName)
	assert.Equal(t, 0, finalProject.Status.FloatingIPs[testPoolName].Used)
	assert.NotContains(t, finalProject.Status.FloatingIPs[testPoolName].Allocated, allocatedIP)

	// Check IPAM
	assert.Equal(t, 0, f.controller.ipam.Used(pool.Name), "IP should be released from IPAM")
}

func TestSyncHandler_PoolFull(t *testing.T) {
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})

	// Pool with only one IP
	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.10")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})
	key := getKey(fip, t)

	// Allocate the only IP to exhaust the pool
	_, err := f.controller.ipam.GetIP(pool.Name, "10.0.0.10")
	require.NoError(t, err)

	err = f.run(key)
	require.Error(t, err, "syncHandler should return an error when pool is full")
	assert.Contains(t, err.Error(), "failed to allocate an IP from pool")
}

func TestSyncHandler_ErrorStateRetriesAllocation(t *testing.T) {
	exhaustedIP := "10.0.0.10"
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})
	// Simulate a FIP wedged in Error state by an earlier failed allocation.
	fip.Status.State = "Error"
	fip.Status.Conditions = []metav1.Condition{{
		Type:    "Ready",
		Status:  metav1.ConditionFalse,
		Reason:  "AllocationFailed",
		Message: "failed to allocate an IP from pool test-pool: no more ips left in network test-pool",
	}}

	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.10")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})

	// Exhaust the pool so the retry fails exactly like the original failure.
	_, err := f.controller.ipam.GetIP(pool.Name, exhaustedIP)
	require.NoError(t, err)

	key := getKey(fip, t)

	// The Error state must not short-circuit: allocation is attempted again
	// and fails, so syncHandler returns an error (which requeues with backoff).
	f.clientset.ClearActions()
	err = f.run(key)
	require.Error(t, err, "Error-state FIP must be re-attempted, not skipped")
	assert.Contains(t, err.Error(), "failed to allocate an IP from pool")

	// The retry must not rewrite the identical Error status it already recorded.
	statusUpdates := 0
	for _, action := range f.clientset.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "floatingips" {
			statusUpdates++
		}
	}
	assert.Equal(t, 0, statusUpdates, "identical Error status must not be rewritten on retry")

	// Once capacity frees up, the next reconcile (the backoff requeue) must
	// recover the FIP into Allocated state.
	require.NoError(t, f.controller.ipam.ReleaseIP(pool.Name, exhaustedIP))
	require.NoError(t, f.run(key))

	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Allocated", updatedFip.Status.State, "Error state must clear after a successful retry")
	require.NotNil(t, updatedFip.Spec.IPAddr)
	assert.Equal(t, exhaustedIP, *updatedFip.Spec.IPAddr)
}

// TestSyncHandler_RecoversFromPartiallyPersistedAllocation covers the window
// where the spec update (Spec.IPAddr) landed but the status update failed: the
// IPAM claim from the aborted attempt must be idempotently re-claimed by the
// same FIP, and the next reconcile must reach Allocated instead of looping on
// "already allocated".
func TestSyncHandler_RecoversFromPartiallyPersistedAllocation(t *testing.T) {
	stuckIP := "10.0.0.10"

	// Case 1: claim belongs to this FIP (spec landed, status did not).
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})
	fip.Spec.IPAddr = &stuckIP

	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.20")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})

	// Simulate the claim left behind by the aborted attempt.
	_, err := f.controller.ipam.GetIPOwned(testPoolName, stuckIP, testNamespace+"/"+testFipName)
	require.NoError(t, err)

	err = f.run(getKey(fip, t))
	require.NoError(t, err, "own stale claim must be re-claimed idempotently, not rejected")

	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Allocated", updatedFip.Status.State)
	require.NotNil(t, updatedFip.Status.IPAddr)
	assert.Equal(t, stuckIP, *updatedFip.Spec.IPAddr)
	assert.Equal(t, stuckIP, updatedFip.Status.IPAddr)

	// Case 2: the IP is held by someone else (unowned claim) — the retry must
	// fail with "already allocated", never steal the address.
	fip2 := newFip("other-fip", testNamespace, testPoolName)
	fip2.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip2.SetFinalizers([]string{finalizerName})
	otherIP := "10.0.0.11"
	fip2.Spec.IPAddr = &otherIP

	f2 := newFixture(t, []runtime.Object{fip2, pool, project}, []runtime.Object{ns})
	_, err = f2.controller.ipam.GetIP(testPoolName, otherIP)
	require.NoError(t, err)

	err = f2.run(getKey(fip2, t))
	require.Error(t, err, "foreign-held IP must not be re-claimed")
	assert.Contains(t, err.Error(), "already allocated")
}

// TestSyncHandler_DeletionReleasesUnclaimedSpecIP covers deleting a FIP that
// is wedged with Spec.IPAddr set but Status.IPAddr empty: the owned IPAM claim
// from the aborted allocation must be released, while a claim held by another
// FIP must survive.
func TestSyncHandler_DeletionReleasesUnclaimedSpecIP(t *testing.T) {
	stuckIP := "10.0.0.10"
	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.20")
	project := newProject(testProjectName)
	ns := newNamespace(testNamespace, testProjectName)

	// Case 1: the claim belongs to this FIP — it must be released on delete.
	fip := newFip(testFipName, testNamespace, testPoolName)
	fip.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip.SetFinalizers([]string{finalizerName})
	fip.Spec.IPAddr = &stuckIP
	now := metav1.Now()
	fip.SetDeletionTimestamp(&now)

	f := newFixture(t, []runtime.Object{fip, pool, project}, []runtime.Object{ns})
	_, err := f.controller.ipam.GetIPOwned(testPoolName, stuckIP, testNamespace+"/"+testFipName)
	require.NoError(t, err)

	require.NoError(t, f.run(getKey(fip, t)))

	updatedFip, err := f.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updatedFip.GetFinalizers(), "finalizer should be removed")
	assert.Equal(t, 0, f.controller.ipam.Used(testPoolName), "own unclaimed spec IP must be released on deletion")

	// Case 2: the requested IP is held by another owner — deleting this FIP
	// must not release the other owner's claim.
	fip2 := newFip("other-fip", testNamespace, testPoolName)
	fip2.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip2.SetFinalizers([]string{finalizerName})
	otherIP := "10.0.0.11"
	fip2.Spec.IPAddr = &otherIP
	later := metav1.Now()
	fip2.SetDeletionTimestamp(&later)

	f2 := newFixture(t, []runtime.Object{fip2, pool, project}, []runtime.Object{ns})
	_, err = f2.controller.ipam.GetIPOwned(testPoolName, otherIP, "test-ns/someone-else")
	require.NoError(t, err)

	require.NoError(t, f2.run(getKey(fip2, t)))
	assert.Equal(t, 1, f2.controller.ipam.Used(testPoolName), "foreign-held IP must not be released by another FIP's deletion")
	updatedFip2, err := f2.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), "other-fip", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updatedFip2.GetFinalizers(), "deletion must proceed even when the claim is foreign")

	// Case 3: the requested IP was never granted (no IPAM claim at all) —
	// deletion is a normal no-op for IPAM and still removes the finalizer.
	fip3 := newFip("ungranted-fip", testNamespace, testPoolName)
	fip3.SetLabels(map[string]string{projectNameLabel: testProjectName})
	fip3.SetFinalizers([]string{finalizerName})
	neverGranted := "10.0.0.12"
	fip3.Spec.IPAddr = &neverGranted
	last := metav1.Now()
	fip3.SetDeletionTimestamp(&last)

	f3 := newFixture(t, []runtime.Object{fip3, pool, project}, []runtime.Object{ns})
	require.NoError(t, f3.run(getKey(fip3, t)))
	assert.Equal(t, 0, f3.controller.ipam.Used(testPoolName), "never-granted spec IP must not change IPAM state")
	updatedFip3, err := f3.clientset.RancherV1beta2().FloatingIPs(testNamespace).Get(context.Background(), "ungranted-fip", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updatedFip3.GetFinalizers(), "finalizer should be removed")
}
