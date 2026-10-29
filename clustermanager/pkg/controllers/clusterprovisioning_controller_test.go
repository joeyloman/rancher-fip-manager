package controllers

import (
	"context"
	"strings"
	"testing"

	rbbv1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// fipCluster is a Rancher management cluster, which is cluster-scoped.
func fipCluster(labels map[string]string) *managementv3.Cluster {
	return &managementv3.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "guest-1", Labels: labels},
	}
}

func TestProvisioningTriggerChanged(t *testing.T) {
	tests := []struct {
		name          string
		oldObj        client.Object
		newObj        client.Object
		wantReconcile bool
	}{
		{
			name:          "enablement label added",
			oldObj:        fipCluster(map[string]string{}),
			newObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled"}),
			wantReconcile: true,
		},
		{
			name:          "enablement label value changed",
			oldObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled"}),
			newObj:        fipCluster(map[string]string{fipEnabledLabel: "disabled"}),
			wantReconcile: true,
		},
		{
			name:          "enablement label removed",
			oldObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled"}),
			newObj:        fipCluster(map[string]string{}),
			wantReconcile: true,
		},
		{
			name:          "load balancer type changed",
			oldObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled"}),
			newObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled", fipLBTypeLabel: "metallb"}),
			wantReconcile: true,
		},
		{
			name:          "unrelated label changed",
			oldObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled", "other": "old"}),
			newObj:        fipCluster(map[string]string{fipEnabledLabel: "enabled", "other": "new"}),
			wantReconcile: false,
		},
		{
			name:          "cluster stays disabled",
			oldObj:        fipCluster(map[string]string{}),
			newObj:        fipCluster(map[string]string{}),
			wantReconcile: false,
		},
		{
			name:          "old object missing",
			oldObj:        nil,
			newObj:        fipCluster(nil),
			wantReconcile: true,
		},
		{
			name:          "new object missing",
			oldObj:        fipCluster(nil),
			newObj:        nil,
			wantReconcile: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantReconcile, provisioningTriggerChanged(tc.oldObj, tc.newObj))
		})
	}
}

// TestClusterProvisioningPredicate pins the event contract the provisioning
// path depends on: every create and delete is reconciled, and an update is
// reconciled only when it changes what has to be provisioned.
func TestClusterProvisioningPredicate(t *testing.T) {
	p := clusterProvisioningPredicate()

	enabled := fipCluster(map[string]string{fipEnabledLabel: "enabled"})
	sameLabels := fipCluster(map[string]string{fipEnabledLabel: "enabled"})
	relabeled := fipCluster(map[string]string{fipEnabledLabel: "enabled", fipLBTypeLabel: "metallb"})

	assert.True(t, p.Create(event.CreateEvent{Object: enabled}), "a cluster seen for the first time must be provisioned")
	assert.True(t, p.Delete(event.DeleteEvent{Object: enabled}), "a deleted cluster must be reconciled so its FloatingIPs are released")
	assert.False(t, p.Generic(event.GenericEvent{Object: enabled}), "generic events carry no state change")

	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: enabled, ObjectNew: relabeled}), "a load balancer type change must re-run provisioning")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: enabled, ObjectNew: enabled}), "a resync of an unchanged cluster must not re-run provisioning")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: enabled, ObjectNew: sameLabels}), "an update that does not change the trigger labels must not re-run provisioning")
}

// TestHarvesterNetworkName pins the two encodings the Harvester node driver
// accepts. An empty networkInfo is the legacy format, not a malformed value:
// failing to unmarshal it aborted the reconcile for every HarvesterConfig
// that predates multi-NIC support.
func TestHarvesterNetworkName(t *testing.T) {
	config := func(fields map[string]interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: fields}
	}

	tests := []struct {
		name    string
		config  *unstructured.Unstructured
		want    string
		wantErr string
	}{
		{
			name:   "networkInfo",
			config: config(map[string]interface{}{"networkInfo": `{"interfaces":[{"networkName":"default/mgmt","macAddress":"52:54:00:00:00:01"}]}`}),
			want:   "default/mgmt",
		},
		{
			name:   "legacy networkName when networkInfo is empty",
			config: config(map[string]interface{}{"networkInfo": "", "networkName": "default/mgmt"}),
			want:   "default/mgmt",
		},
		{
			name:   "legacy networkName when networkInfo is absent",
			config: config(map[string]interface{}{"networkName": "default/mgmt"}),
			want:   "default/mgmt",
		},
		{
			name:    "malformed networkInfo",
			config:  config(map[string]interface{}{"networkInfo": "{"}),
			wantErr: "unable to unmarshal networkInfo",
		},
		{
			name:    "networkInfo without interfaces",
			config:  config(map[string]interface{}{"networkInfo": `{"interfaces":[]}`}),
			wantErr: "networkName not found in networkInfo",
		},
		{
			name:    "networkInfo with empty networkName",
			config:  config(map[string]interface{}{"networkInfo": `{"interfaces":[{"networkName":""}]}`}),
			wantErr: "networkName not found in networkInfo",
		},
		{
			name:   "no network configured",
			config: config(map[string]interface{}{}),
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := harvesterNetworkName(tc.config)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestReleaseFloatingIPsDropsClaimLabels pins what "released" means for a
// FloatingIP whose cluster was deleted: every claim marker has to go, so the
// rancher-fip-manager controller can hand the address out again. The service
// labels are indexed and unbounded, so a fixed key list is not enough.
func TestReleaseFloatingIPsDropsClaimLabels(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, rbbv1beta2.AddToScheme(scheme))

	claims := func(cluster string) map[string]string {
		return map[string]string{
			fipClusterNameLabel:                           cluster,
			fipGroupLabel:                                 "dmz",
			"rancher.k8s.binbash.org/project-name":        "p-abc",
			"rancher.k8s.binbash.org/service-0-name":      "svc-a",
			"rancher.k8s.binbash.org/service-0-namespace": "default",
			"rancher.k8s.binbash.org/service-1-name":      "svc-b",
			"rancher.k8s.binbash.org/service-1-namespace": "kube-system",
			"unrelated": "keep",
		}
	}

	deleted := &rbbv1beta2.FloatingIP{ObjectMeta: metav1.ObjectMeta{Name: "fip-deleted", Labels: claims("guest-1")}}
	survivor := &rbbv1beta2.FloatingIP{ObjectMeta: metav1.ObjectMeta{Name: "fip-survivor", Labels: claims("guest-2")}}
	unclaimed := &rbbv1beta2.FloatingIP{ObjectMeta: metav1.ObjectMeta{Name: "fip-unclaimed"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deleted, survivor, unclaimed).Build()
	r := &ClusterProvisioningReconciler{Client: c}
	require.NoError(t, r.releaseFloatingIPs(context.Background(), logrus.NewEntry(logrus.New()), "guest-1"))

	get := func(name string) *rbbv1beta2.FloatingIP {
		var fip rbbv1beta2.FloatingIP
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &fip))
		return &fip
	}

	released := get("fip-deleted").Labels
	assert.NotContains(t, released, fipClusterNameLabel)
	assert.NotContains(t, released, fipGroupLabel)
	for key := range released {
		assert.False(t, strings.HasPrefix(key, fipServiceLabelPrefix), "service label %s survived the release", key)
	}
	assert.Equal(t, "p-abc", released["rancher.k8s.binbash.org/project-name"], "the project label keeps quota accounting intact")
	assert.Equal(t, "keep", released["unrelated"], "labels not owned by the claim must not be touched")

	// A different cluster's claim, and an unclaimed FloatingIP, are untouched.
	assert.Equal(t, claims("guest-2"), get("fip-survivor").Labels)
	assert.Empty(t, get("fip-unclaimed").Labels)
}
