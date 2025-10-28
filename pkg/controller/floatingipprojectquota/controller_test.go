package floatingipprojectquota

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
)

func TestFloatingIPProjectQuotaController_syncHandler(t *testing.T) {
	// Test setup
	project := &v1beta1.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
		Spec: v1beta1.FloatingIPProjectQuotaSpec{
			DisplayName: "Test Project",
		},
	}

	pool := &v1beta1.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
		Spec: v1beta1.FloatingIPPoolSpec{
			IPConfig: &v1beta1.IPConfig{
				Family: "IPv4",
			},
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
		},
		Status: v1beta1.FloatingIPStatus{
			IPAddr: "192.168.1.10",
			Assigned: &v1beta1.AssignedInfo{
				ClusterName: "test-cluster",
			},
		},
	}

	// Create fake clientset and informers
	objs := []runtime.Object{project, pool, fip}
	clientset := fake.NewSimpleClientset(objs...)
	kubeClient := k8sfake.NewSimpleClientset(ns)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	projectInformer := informerFactory.Rancher().V1beta1().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta1().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta1().FloatingIPPools()

	// Populate informers
	projectInformer.Informer().GetIndexer().Add(project)
	fipInformer.Informer().GetIndexer().Add(fip)
	fipPoolInformer.Informer().GetIndexer().Add(pool)

	// Create controller
	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)

	// Run syncHandler
	key, err := cache.MetaNamespaceKeyFunc(project)
	require.NoError(t, err)
	err = controller.syncHandler(context.Background(), key)
	require.NoError(t, err)

	// Assertions
	updatedProject, err := clientset.RancherV1beta1().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)

	// Expected status
	expectedFipsStatus := map[string]*v1beta1.FipInfo{
		"test-pool": {
			Family: "IPv4",
			Used:   1,
			Allocated: map[string]string{
				"192.168.1.10": "test-cluster",
			},
		},
	}

	assert.Equal(t, expectedFipsStatus, updatedProject.Status.FloatingIPs)
}
