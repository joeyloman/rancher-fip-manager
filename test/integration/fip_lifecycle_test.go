package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rancherfipv1beta1 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta1"
)

const (
	projectName        = "my-project"
	poolName           = "prod-v4-pool"
	namespaceName      = "my-project-ns"
	fipName            = "my-app-fip"
	timeout            = time.Second * 10
	interval           = time.Millisecond * 250
	finalizerName      = "rancher.k8s.binbash.org/floatingip-cleanup"
	projectLabel       = "rancher.k8s.binbash.org/project-name"
	allocatedIP        = "192.168.100.10"
	fipStatusAllocated = "Allocated"
)

func TestFloatingIPLifecycle(t *testing.T) {
	ctx := context.Background()

	// Create Namespace with project label
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespaceName,
			Labels: map[string]string{projectLabel: projectName},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns), "failed to create test namespace")

	// Create FloatingIPProjectQuota
	project := &rancherfipv1beta1.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: projectName},
		Spec: rancherfipv1beta1.FloatingIPProjectQuotaSpec{
			DisplayName:     "My Test Project",
			FloatingIPQuota: map[string]int{poolName: 5},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, project), "failed to create FloatingIPProjectQuota")

	// Create FloatingIPPool
	pool := &rancherfipv1beta1.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName},
		Spec: rancherfipv1beta1.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &rancherfipv1beta1.IPConfig{
				Family: "IPv4",
				Subnet: "192.168.100.0/24",
				Pool: rancherfipv1beta1.Pool{
					Start: "192.168.100.10",
					End:   "192.168.100.20",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pool), "failed to create FloatingIPPool")

	// Create FloatingIP
	fip := &rancherfipv1beta1.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fipName,
			Namespace: namespaceName,
			Labels:    map[string]string{projectLabel: projectName},
		},
		Spec: rancherfipv1beta1.FloatingIPSpec{
			FloatingIPPool: poolName,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, fip), "failed to create FloatingIP")

	// === Verify Allocation ===
	fipKey := types.NamespacedName{Name: fipName, Namespace: namespaceName}
	poolKey := types.NamespacedName{Name: poolName, Namespace: ""}
	projectKey := types.NamespacedName{Name: projectName, Namespace: ""}

	// 1. Check FloatingIP status
	var fetchedFIP rancherfipv1beta1.FloatingIP
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, fipKey, &fetchedFIP); err != nil {
			return false
		}
		return len(fetchedFIP.Finalizers) > 0 &&
			fetchedFIP.Status.IPAddr == allocatedIP &&
			fetchedFIP.Status.State == fipStatusAllocated
	}, timeout, interval, "FloatingIP should be updated with finalizer and allocated status")
	require.Equal(t, finalizerName, fetchedFIP.Finalizers[0])

	// 2. Check FloatingIPPool status
	var fetchedPool rancherfipv1beta1.FloatingIPPool
	expectedPoolAllocationValue := "my-project [My Test Project]"
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, poolKey, &fetchedPool); err != nil {
			return false
		}
		val, ok := fetchedPool.Status.Allocated[allocatedIP]
		return ok && val == expectedPoolAllocationValue
	}, timeout, interval, "FloatingIPPool status should show the IP as allocated")

	// 3. Check FloatingIPProjectQuota status
	var fetchedProject rancherfipv1beta1.FloatingIPProjectQuota
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, projectKey, &fetchedProject); err != nil {
			return false
		}
		fipInfo, ok := fetchedProject.Status.FloatingIPs[poolName]
		return ok && fipInfo.Used == 1
	}, timeout, interval, "FloatingIPProjectQuota status should show 1 used IP")

	// === Verify Deletion ===
	require.NoError(t, k8sClient.Delete(ctx, &fetchedFIP), "failed to delete FloatingIP")

	// 1. Check FloatingIP is deleted
	require.Eventually(t, func() bool {
		err := k8sClient.Get(ctx, fipKey, &fetchedFIP)
		return errors.IsNotFound(err)
	}, timeout, interval, "FloatingIP should be deleted after finalizer is removed")

	// 2. Check FloatingIPPool status
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, poolKey, &fetchedPool); err != nil {
			return false
		}
		_, ok := fetchedPool.Status.Allocated[allocatedIP]
		return !ok
	}, timeout, interval, "FloatingIPPool status should show the IP as released")

	// 3. Check FloatingIPProjectQuota status
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, projectKey, &fetchedProject); err != nil {
			return false
		}
		fipInfo, ok := fetchedProject.Status.FloatingIPs[poolName]
		// It might be that the map key is removed entirely if usage is 0
		return !ok || fipInfo.Used == 0
	}, timeout, interval, "FloatingIPProjectQuota status should show 0 used IPs")
}

func TestFloatingIP_PreExistingIP(t *testing.T) {
	const (
		startupPoolName      = "startup-pool"
		startupNamespaceName = "startup-ns"
		newFipName           = "fip-requesting-existing-ip"
	)
	preAllocatedIP := "10.10.10.1"
	ctx := context.Background()

	// This FIP is created to test that requesting an already allocated IP results in an error state.
	fip := &rancherfipv1beta1.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newFipName,
			Namespace: startupNamespaceName,
			Labels:    map[string]string{projectLabel: "startup-project"},
		},
		Spec: rancherfipv1beta1.FloatingIPSpec{
			FloatingIPPool: startupPoolName,
			IPAddr:         &preAllocatedIP,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, fip), "failed to create FloatingIP")

	// === Verify Allocation Failure ===
	fipKey := types.NamespacedName{Name: newFipName, Namespace: startupNamespaceName}
	preExistingFipKey := types.NamespacedName{Name: "fip-with-ip", Namespace: startupNamespaceName}

	// 1. Check the new FloatingIP status is Error
	var fetchedFIP rancherfipv1beta1.FloatingIP
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, fipKey, &fetchedFIP); err != nil {
			return false
		}
		return fetchedFIP.Status.State == "Error"
	}, timeout, interval, "new FloatingIP should be in Error state")

	// 2. Check that the pre-existing FloatingIP was also reconciled correctly and not in an error state
	var preExistingFIP rancherfipv1beta1.FloatingIP
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, preExistingFipKey, &preExistingFIP); err != nil {
			return false
		}
		return len(preExistingFIP.Finalizers) > 0 &&
			preExistingFIP.Status.IPAddr == preAllocatedIP &&
			preExistingFIP.Status.State == fipStatusAllocated
	}, timeout, interval, "pre-existing FloatingIP should be reconciled correctly and not in an error state")

	// Cleanup
	require.NoError(t, k8sClient.Delete(ctx, &fetchedFIP), "failed to delete new FloatingIP")
}
