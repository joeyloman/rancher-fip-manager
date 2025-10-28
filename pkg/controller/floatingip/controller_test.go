package floatingip

import (
	"context"
	"testing"

	v1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
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
		if pool, ok := obj.(*v1beta1.FloatingIPPool); ok {
			if pool.Spec.IPConfig != nil {
				err := ipam.NewSubnet(pool.Name, pool.Spec.IPConfig.Subnet, pool.Spec.IPConfig.Pool.Start, pool.Spec.IPConfig.Pool.End)
				require.NoError(t, err)
			}
		}
	}

	controller := New(
		clientset,
		kubeclient,
		fipInformerFactory.Rancher().V1beta1().FloatingIPs(),
		fipInformerFactory.Rancher().V1beta1().FloatingIPPools(),
		fipInformerFactory.Rancher().V1beta1().FloatingIPProjectQuotas(),
		ipam,
	)

	// Populate informers
	for _, obj := range fipObjects {
		switch obj := obj.(type) {
		case *v1beta1.FloatingIP:
			fipInformerFactory.Rancher().V1beta1().FloatingIPs().Informer().GetIndexer().Add(obj)
		case *v1beta1.FloatingIPPool:
			fipInformerFactory.Rancher().V1beta1().FloatingIPPools().Informer().GetIndexer().Add(obj)
		case *v1beta1.FloatingIPProjectQuota:
			fipInformerFactory.Rancher().V1beta1().FloatingIPProjectQuotas().Informer().GetIndexer().Add(obj)
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

func newProject(name string) *v1beta1.FloatingIPProjectQuota {
	return &v1beta1.FloatingIPProjectQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1beta1.FloatingIPProjectQuotaSpec{
			DisplayName: "Test Project",
		},
	}
}

func newPool(name, start, end string) *v1beta1.FloatingIPPool {
	return &v1beta1.FloatingIPPool{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1beta1.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &v1beta1.IPConfig{
				Family: "IPv4",
				Subnet: "10.0.0.0/24",
				Pool: v1beta1.Pool{
					Start: start,
					End:   end,
				},
			},
		},
	}
}

func newFip(name, namespace, poolName string) *v1beta1.FloatingIP {
	return &v1beta1.FloatingIP{
		TypeMeta: metav1.TypeMeta{APIVersion: v1beta1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1beta1.FloatingIPSpec{
			FloatingIPPool: poolName,
		},
	}
}

func getKey(fip *v1beta1.FloatingIP, t *testing.T) string {
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

	updatedFip, err := f.clientset.RancherV1beta1().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
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

	updatedFip, err := f.clientset.RancherV1beta1().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
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
	finalFip, err := f.clientset.RancherV1beta1().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, finalFip.Status.Assigned, "FIP status should be assigned")
	require.NotEmpty(t, finalFip.Status.Conditions, "FIP status should have conditions")
	assert.Equal(t, "Ready", finalFip.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, finalFip.Status.Conditions[0].Status)

	// Check Pool status
	finalPool, err := f.clientset.RancherV1beta1().FloatingIPPools().Get(context.Background(), testPoolName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, finalPool.Status.Allocated, allocatedIP, "pool status should show allocated IP")

	// Check Project status
	finalProject, err := f.clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(context.Background(), testProjectName, metav1.GetOptions{})
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
	fip.Status.Assigned = &v1beta1.AssignedInfo{} // Mark as assigned to trigger release logic

	pool := newPool(testPoolName, "10.0.0.10", "10.0.0.20")
	pool.Status.Allocated = map[string]string{allocatedIP: "test-project [Test Project]"}

	project := newProject(testProjectName)
	project.Status.FloatingIPs = map[string]*v1beta1.FipInfo{
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
	updatedFip, err := f.clientset.RancherV1beta1().FloatingIPs(testNamespace).Get(context.Background(), testFipName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, updatedFip.GetFinalizers(), "finalizer should be removed")

	// Check Pool status
	finalPool, err := f.clientset.RancherV1beta1().FloatingIPPools().Get(context.Background(), testPoolName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, finalPool.Status.Allocated, allocatedIP, "IP should be released from pool status")

	// Check Project status
	finalProject, err := f.clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(context.Background(), testProjectName, metav1.GetOptions{})
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
