package floatingipprojectquota

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
)

func TestFloatingIPProjectQuotaController_syncHandler(t *testing.T) {
	// Test setup
	project := &v1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
		Spec: v1beta2.FloatingIPProjectQuotaSpec{
			DisplayName: "Test Project",
		},
	}

	pool := &v1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
		Spec: v1beta2.FloatingIPPoolSpec{
			IPConfig: &v1beta2.IPConfig{
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

	fip := &v1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fip",
			Namespace: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
		},
		Spec: v1beta2.FloatingIPSpec{
			FloatingIPPool: "test-pool",
		},
		Status: v1beta2.FloatingIPStatus{
			IPAddr: "192.168.1.10",
			Assigned: &v1beta2.AssignedInfo{
				ClusterName: "test-cluster",
			},
		},
	}

	// Create fake clientset and informers
	objs := []runtime.Object{project, pool, fip}
	clientset := fake.NewSimpleClientset(objs...)
	kubeClient := k8sfake.NewSimpleClientset(ns)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()

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
	updatedProject, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)

	// Expected status
	expectedFipsStatus := map[string]*v1beta2.FipInfo{
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

func TestFloatingIPProjectQuotaController_cascadeDeletion(t *testing.T) {
	quota := &v1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
			Finalizers: []string{
				"rancher.k8s.binbash.org/floatingipprojectquota-cleanup",
			},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
	}

	pool := &v1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pool",
		},
	}

	attachedFip := &v1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "attached-fip",
			Namespace: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
			Finalizers: []string{
				"rancher.k8s.binbash.org/floatingip-cleanup",
			},
		},
		Spec: v1beta2.FloatingIPSpec{
			FloatingIPPool: "test-pool",
		},
		Status: v1beta2.FloatingIPStatus{
			IPAddr: "192.168.1.10",
		},
	}

	otherFip := &v1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-fip",
			Namespace: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "other-project",
			},
		},
		Spec: v1beta2.FloatingIPSpec{
			FloatingIPPool: "test-pool",
		},
	}

	objs := []runtime.Object{quota, pool, attachedFip, otherFip}
	clientset := fake.NewSimpleClientset(objs...)
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()

	projectInformer.Informer().GetIndexer().Add(quota)
	fipInformer.Informer().GetIndexer().Add(attachedFip)
	fipInformer.Informer().GetIndexer().Add(otherFip)
	fipPoolInformer.Informer().GetIndexer().Add(pool)

	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)
	key, err := cache.MetaNamespaceKeyFunc(quota)
	require.NoError(t, err)

	// First sync: the attached FIP must be deleted, the unrelated one kept,
	// and the quota must still exist (finalizer holds it).
	err = controller.syncHandler(context.Background(), key)
	require.NoError(t, err)

	_, err = clientset.RancherV1beta2().FloatingIPs("test-ns").Get(context.Background(), "attached-fip", metav1.GetOptions{})
	assert.Error(t, err)
	assert.True(t, apimachineryerrors.IsNotFound(err))

	_, err = clientset.RancherV1beta2().FloatingIPs("test-ns").Get(context.Background(), "other-fip", metav1.GetOptions{})
	assert.NoError(t, err)

	_, err = clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	assert.NoError(t, err)

	// Second sync after the informer observed the deletion: the finalizer
	// must be removed so the quota can go away.
	fipInformer.Informer().GetIndexer().Delete(attachedFip)
	err = controller.syncHandler(context.Background(), key)
	require.NoError(t, err)

	updatedQuota, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updatedQuota.Finalizers, "rancher.k8s.binbash.org/floatingipprojectquota-cleanup")
}

func TestFloatingIPProjectQuotaController_addsFinalizer(t *testing.T) {
	quota := &v1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
	}

	clientset := fake.NewSimpleClientset(quota)
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()

	projectInformer.Informer().GetIndexer().Add(quota)

	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)
	key, err := cache.MetaNamespaceKeyFunc(quota)
	require.NoError(t, err)
	err = controller.syncHandler(context.Background(), key)
	require.NoError(t, err)

	updatedQuota, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, updatedQuota.Finalizers, "rancher.k8s.binbash.org/floatingipprojectquota-cleanup")
}

// Regression: releasing (deleting) a FloatingIP must enqueue the owning
// project's quota so its status drops the released IP. Without the FloatingIP
// event handlers the quota kept the deleted IP in its allocated map forever,
// leaving the project over-counted and the webhook denying new requests
// despite free pool capacity.
func TestFloatingIPProjectQuotaController_fipDeleteEnqueuesQuota(t *testing.T) {
	quota := &v1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-project",
		},
	}
	fip := &v1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-fip",
			Namespace: "test-ns",
			Labels: map[string]string{
				"rancher.k8s.binbash.org/project-name": "test-project",
			},
		},
		Spec: v1beta2.FloatingIPSpec{
			FloatingIPPool: "test-pool",
		},
		Status: v1beta2.FloatingIPStatus{
			IPAddr: "192.168.1.10",
		},
	}

	clientset := fake.NewSimpleClientset(quota, fip)
	kubeClient := k8sfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	projectInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	fipPoolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()

	projectInformer.Informer().GetIndexer().Add(quota)

	controller := New(clientset, kubeClient, projectInformer, fipInformer, fipPoolInformer)

	// Simulate the delete event as the informer delivers it: the object may
	// arrive wrapped in a DeletedFinalStateUnknown tombstone.
	controller.enqueueQuotaForFloatingIP(cache.DeletedFinalStateUnknown{Key: "test-ns/test-fip", Obj: fip})

	// Assert queue membership before Get(): Get() blocks on an empty queue,
	// so a regression must fail here instead of hanging the suite.
	require.Equal(t, 1, controller.workqueue.Len(), "FIP delete event must enqueue the owning project's quota")
	queued, _ := controller.workqueue.Get()
	assert.Equal(t, "test-project", queued)
	controller.workqueue.Done(queued)

	// End-to-end status contract, present-then-absent: with the FloatingIP
	// still in the lister the rebuild must count it, and after it is deleted
	// the rebuild must drop it again.
	fipInformer.Informer().GetIndexer().Add(fip)
	fipPoolInformer.Informer().GetIndexer().Add(&v1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
		Spec: v1beta2.FloatingIPPoolSpec{
			IPConfig: &v1beta2.IPConfig{Family: "IPv4"},
		},
	})
	require.NoError(t, controller.syncHandler(context.Background(), "test-project"))
	updatedQuota, err := clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, updatedQuota.Status.FloatingIPs, "test-pool", "allocated FIP must be counted before release")
	assert.Equal(t, map[string]string{"192.168.1.10": "Unassigned"}, updatedQuota.Status.FloatingIPs["test-pool"].Allocated)
	assert.Equal(t, 1, updatedQuota.Status.FloatingIPs["test-pool"].Used)

	fipInformer.Informer().GetIndexer().Delete(fip)
	require.NoError(t, controller.syncHandler(context.Background(), "test-project"))
	updatedQuota, err = clientset.RancherV1beta2().FloatingIPProjectQuotas().Get(context.Background(), "test-project", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, updatedQuota.Status.FloatingIPs, "test-pool", "released FIP must be dropped from the quota status")

	// A FloatingIP without a project label must not enqueue anything.
	unlabeled := fip.DeepCopy()
	unlabeled.Labels = nil
	controller.enqueueQuotaForFloatingIP(unlabeled)
	controller.enqueueQuotaForFloatingIP(nil)
	assert.Equal(t, 0, controller.workqueue.Len(), "unlabeled or invalid objects must not enqueue a quota")

	// A labeled FloatingIP without a matching quota must not enqueue anything.
	orphan := fip.DeepCopy()
	orphan.Labels = map[string]string{"rancher.k8s.binbash.org/project-name": "no-such-quota"}
	controller.enqueueQuotaForFloatingIP(orphan)
	assert.Equal(t, 0, controller.workqueue.Len(), "FloatingIP without a matching quota must not enqueue")
}
