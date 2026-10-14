package controllers

import (
	"testing"

	managementv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
