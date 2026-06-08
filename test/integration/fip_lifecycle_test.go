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

	rancherfipv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
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
	project := &rancherfipv1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: projectName},
		Spec: rancherfipv1beta2.FloatingIPProjectQuotaSpec{
			DisplayName:     "My Test Project",
			FloatingIPQuota: map[string]int{poolName: 5},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, project), "failed to create FloatingIPProjectQuota")

	// Create FloatingIPPool
	pool := &rancherfipv1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: poolName},
		Spec: rancherfipv1beta2.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &rancherfipv1beta2.IPConfig{
				Family: "IPv4",
				Subnet: "192.168.100.0/24",
				Pool: rancherfipv1beta2.Pool{
					Start: "192.168.100.10",
					End:   "192.168.100.20",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pool), "failed to create FloatingIPPool")

	// Create FloatingIP
	fip := &rancherfipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fipName,
			Namespace: namespaceName,
			Labels:    map[string]string{projectLabel: projectName},
		},
		Spec: rancherfipv1beta2.FloatingIPSpec{
			FloatingIPPool: poolName,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, fip), "failed to create FloatingIP")

	// === Verify Allocation ===
	fipKey := types.NamespacedName{Name: fipName, Namespace: namespaceName}
	poolKey := types.NamespacedName{Name: poolName, Namespace: ""}
	projectKey := types.NamespacedName{Name: projectName, Namespace: ""}

	// 1. Check FloatingIP status
	var fetchedFIP rancherfipv1beta2.FloatingIP
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
	var fetchedPool rancherfipv1beta2.FloatingIPPool
	expectedPoolAllocationValue := "my-project [My Test Project]"
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, poolKey, &fetchedPool); err != nil {
			return false
		}
		val, ok := fetchedPool.Status.Allocated[allocatedIP]
		return ok && val == expectedPoolAllocationValue
	}, timeout, interval, "FloatingIPPool status should show the IP as allocated")

	// 3. Check FloatingIPProjectQuota status
	var fetchedProject rancherfipv1beta2.FloatingIPProjectQuota
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
	fip := &rancherfipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newFipName,
			Namespace: startupNamespaceName,
			Labels:    map[string]string{projectLabel: "startup-project"},
		},
		Spec: rancherfipv1beta2.FloatingIPSpec{
			FloatingIPPool: startupPoolName,
			IPAddr:         &preAllocatedIP,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, fip), "failed to create FloatingIP")

	// === Verify Allocation Failure ===
	fipKey := types.NamespacedName{Name: newFipName, Namespace: startupNamespaceName}
	preExistingFipKey := types.NamespacedName{Name: "fip-with-ip", Namespace: startupNamespaceName}

	// 1. Check the new FloatingIP status is Error
	var fetchedFIP rancherfipv1beta2.FloatingIP
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, fipKey, &fetchedFIP); err != nil {
			return false
		}
		return fetchedFIP.Status.State == "Error"
	}, timeout, interval, "new FloatingIP should be in Error state")

	// 2. Check that the pre-existing FloatingIP was also reconciled correctly and not in an error state
	var preExistingFIP rancherfipv1beta2.FloatingIP
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

func TestFloatingIP_SameFloatingIPGroupDifferentClusters(t *testing.T) {
	const (
		cluster1Name        = "cluster-1"
		cluster2Name        = "cluster-2"
		floatingIPGroupName = "my-shared-lb"
		sharedPoolName      = "shared-pool"
		sharedProjectName   = "shared-project"
		cluster1Namespace   = "cluster-1-ns"
		cluster2Namespace   = "cluster-2-ns"
		cluster1FipName     = "fip-cluster-1"
		cluster2FipName     = "fip-cluster-2"
	)

	ctx := context.Background()

	// Create the FloatingIPPool for the test
	sharedPool := &rancherfipv1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: sharedPoolName},
		Spec: rancherfipv1beta2.FloatingIPPoolSpec{
			TargetNetworkInterface: "eth0",
			IPConfig: &rancherfipv1beta2.IPConfig{
				Family: "IPv4",
				Subnet: "192.168.200.0/24",
				Pool: rancherfipv1beta2.Pool{
					Start: "192.168.200.10",
					End:   "192.168.200.30",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, sharedPool), "failed to create shared FloatingIPPool")

	// Create FloatingIPProjectQuota
	sharedProject := &rancherfipv1beta2.FloatingIPProjectQuota{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProjectName},
		Spec: rancherfipv1beta2.FloatingIPProjectQuotaSpec{
			DisplayName:     "Shared Project",
			FloatingIPQuota: map[string]int{sharedPoolName: 10},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, sharedProject), "failed to create FloatingIPProjectQuota")

	// Create namespaces for both clusters with cluster-name and project labels
	cluster1Ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster1Namespace,
			Labels: map[string]string{
				projectLabel:                           sharedProjectName,
				"rancher.k8s.binbash.org/cluster-name": cluster1Name,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster1Ns), "failed to create cluster-1 namespace")

	cluster2Ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: cluster2Namespace,
			Labels: map[string]string{
				projectLabel:                           sharedProjectName,
				"rancher.k8s.binbash.org/cluster-name": cluster2Name,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster2Ns), "failed to create cluster-2 namespace")

	// Create FloatingIPs for both clusters with the same floatingip-group label
	cluster1Fip := &rancherfipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster1FipName,
			Namespace: cluster1Namespace,
			Labels: map[string]string{
				projectLabel:                               sharedProjectName,
				"rancher.k8s.binbash.org/cluster-name":     cluster1Name,
				"rancher.k8s.binbash.org/floatingip-group": floatingIPGroupName,
			},
		},
		Spec: rancherfipv1beta2.FloatingIPSpec{
			FloatingIPPool: sharedPoolName,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster1Fip), "failed to create cluster-1 FloatingIP")

	cluster2Fip := &rancherfipv1beta2.FloatingIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster2FipName,
			Namespace: cluster2Namespace,
			Labels: map[string]string{
				projectLabel:                               sharedProjectName,
				"rancher.k8s.binbash.org/cluster-name":     cluster2Name,
				"rancher.k8s.binbash.org/floatingip-group": floatingIPGroupName,
			},
		},
		Spec: rancherfipv1beta2.FloatingIPSpec{
			FloatingIPPool: sharedPoolName,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster2Fip), "failed to create cluster-2 FloatingIP")

	// === Verify Allocation ===
	cluster1FipKey := types.NamespacedName{Name: cluster1FipName, Namespace: cluster1Namespace}
	cluster2FipKey := types.NamespacedName{Name: cluster2FipName, Namespace: cluster2Namespace}
	sharedPoolKey := types.NamespacedName{Name: sharedPoolName, Namespace: ""}

	// Verify both FloatingIPs are allocated with different IP addresses
	var fetchedCluster1FIP rancherfipv1beta2.FloatingIP
	var fetchedCluster2FIP rancherfipv1beta2.FloatingIP

	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, cluster1FipKey, &fetchedCluster1FIP); err != nil {
			return false
		}
		return len(fetchedCluster1FIP.Finalizers) > 0 &&
			fetchedCluster1FIP.Status.IPAddr != "" &&
			fetchedCluster1FIP.Status.State == fipStatusAllocated
	}, timeout, interval, "Cluster-1 FloatingIP should be allocated")

	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, cluster2FipKey, &fetchedCluster2FIP); err != nil {
			return false
		}
		return len(fetchedCluster2FIP.Finalizers) > 0 &&
			fetchedCluster2FIP.Status.IPAddr != "" &&
			fetchedCluster2FIP.Status.State == fipStatusAllocated
	}, timeout, interval, "Cluster-2 FloatingIP should be allocated")

	// Verify they have different IP addresses
	require.NotEqual(t, fetchedCluster1FIP.Status.IPAddr, fetchedCluster2FIP.Status.IPAddr,
		"Each cluster should get a different floating IP address")

	// Verify both IPs are in the pool's allocated list
	var fetchedPool rancherfipv1beta2.FloatingIPPool
	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, sharedPoolKey, &fetchedPool); err != nil {
			return false
		}
		_, cluster1IPExists := fetchedPool.Status.Allocated[fetchedCluster1FIP.Status.IPAddr]
		_, cluster2IPExists := fetchedPool.Status.Allocated[fetchedCluster2FIP.Status.IPAddr]
		return cluster1IPExists && cluster2IPExists
	}, timeout, interval, "Both IPs should be marked as allocated in the pool")

	// Verify both FloatingIPs have the same FloatingIPGroup in their status
	require.Equal(t, floatingIPGroupName, fetchedCluster1FIP.Status.Assigned.FloatingIPGroup,
		"Cluster-1 FIP should have the correct floatingip-group")
	require.Equal(t, floatingIPGroupName, fetchedCluster2FIP.Status.Assigned.FloatingIPGroup,
		"Cluster-2 FIP should have the correct floatingip-group")

	// Verify both FloatingIPs have different ClusterNames
	require.Equal(t, cluster1Name, fetchedCluster1FIP.Status.Assigned.ClusterName,
		"Cluster-1 FIP should have the correct cluster name")
	require.Equal(t, cluster2Name, fetchedCluster2FIP.Status.Assigned.ClusterName,
		"Cluster-2 FIP should have the correct cluster name")

	// === Cleanup ===
	require.NoError(t, k8sClient.Delete(ctx, &fetchedCluster1FIP), "failed to delete cluster-1 FloatingIP")
	require.NoError(t, k8sClient.Delete(ctx, &fetchedCluster2FIP), "failed to delete cluster-2 FloatingIP")
	require.NoError(t, k8sClient.Delete(ctx, cluster1Ns), "failed to delete cluster-1 namespace")
	require.NoError(t, k8sClient.Delete(ctx, cluster2Ns), "failed to delete cluster-2 namespace")
	require.NoError(t, k8sClient.Delete(ctx, sharedPool), "failed to delete shared pool")
	require.NoError(t, k8sClient.Delete(ctx, sharedProject), "failed to delete shared project")
}
