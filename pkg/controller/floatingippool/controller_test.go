package floatingippool

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	v1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
)

func TestFloatingIPPoolController_syncHandler(t *testing.T) {
	// Test setup
	pool := &v1beta1.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
		Spec: v1beta1.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &v1beta1.IPConfig{
				Family: "IPv4",
				Subnet: "192.168.1.0/24",
				Pool: v1beta1.Pool{
					Start:   "192.168.1.10",
					End:     "192.168.1.20",
					Exclude: []string{"192.168.1.15"},
				},
			},
		},
	}

	project := &v1beta1.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
		Spec: v1beta1.FloatingIPProjectQuotaSpec{
			DisplayName: "Test Project",
		},
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
		},
	}

	fip := &v1beta1.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fip",
			Namespace: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
		},
		Spec: v1beta1.FloatingIPSpec{
			FloatingIPPool: "test-pool",
			IPAddr:         stringPtr("192.168.1.10"),
		},
		Status: v1beta1.FloatingIPStatus{
			IPAddr: "192.168.1.10",
		},
	}

	// Create fake clientset and informers
	objs := []runtime.Object{pool, project, fip}
	clientset := fake.NewSimpleClientset(objs...)
	kubeClient := k8sfake.NewSimpleClientset(ns)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	fipInformer := informerFactory.Rancher().V1beta1().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta1().FloatingIPPools()
	projectInformer := informerFactory.Rancher().V1beta1().FloatingIPProjectQuotas()

	// Populate informers
	fipInformer.Informer().GetIndexer().Add(fip)
	fipPoolInformer.Informer().GetIndexer().Add(pool)
	projectInformer.Informer().GetIndexer().Add(project)

	// Create controller
	ipam := ipam.New()
	reinitChan := make(chan struct{}, 1)
	controller := New(clientset, kubeClient, fipInformer, fipPoolInformer, projectInformer, ipam, reinitChan)

	// Run syncHandler
	key, err := cache.MetaNamespaceKeyFunc(pool)
	require.NoError(t, err)
	err = controller.syncHandler(context.Background(), key)
	require.NoError(t, err)

	// Assertions
	updatedPool, err := clientset.RancherV1beta1().FloatingIPPools().Get(context.Background(), "test-pool", metav1.GetOptions{})
	require.NoError(t, err)

	// Expected status
	expectedAllocated := map[string]string{
		"192.168.1.10": "test-project [Test Project]",
		"192.168.1.15": "excluded",
	}

	assert.Equal(t, 2, updatedPool.Status.Used)
	assert.Equal(t, 9, updatedPool.Status.Available) // 11 total, 2 used
	assert.Equal(t, expectedAllocated, updatedPool.Status.Allocated)
}

func stringPtr(s string) *string {
	return &s
}
