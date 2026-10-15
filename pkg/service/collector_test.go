package service

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	"github.com/joeyloman/rancher-fip-manager/pkg/generated/clientset/versioned/fake"
	informers "github.com/joeyloman/rancher-fip-manager/pkg/generated/informers/externalversions"
)

// Regression for report 1.4: pools without Spec.IPConfig are legal (the pool
// controller skips them), but the collector dereferenced Spec.IPConfig.Subnet
// unconditionally — a nil-pointer panic in the /metrics handler goroutine.
func TestCollectorPoolsWithoutIPConfig(t *testing.T) {
	noConfigPool := &v1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "no-config"},
		Spec: v1beta2.FloatingIPPoolSpec{
			TargetCluster: "cluster-a",
			TargetNetwork: "net-a",
		},
		Status: v1beta2.FloatingIPPoolStatus{Available: 3, Used: 1},
	}
	withConfigPool := &v1beta2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "with-config"},
		Spec: v1beta2.FloatingIPPoolSpec{
			TargetCluster: "cluster-b",
			TargetNetwork: "net-b",
			IPConfig: &v1beta2.IPConfig{
				Family: "IPv4",
				Subnet: "10.10.10.0/24",
			},
		},
		Status: v1beta2.FloatingIPPoolStatus{Available: 8, Used: 2},
	}

	clientset := fake.NewSimpleClientset(noConfigPool, withConfigPool)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	poolInformer := informerFactory.Rancher().V1beta2().FloatingIPPools()
	fipInformer := informerFactory.Rancher().V1beta2().FloatingIPs()
	quotaInformer := informerFactory.Rancher().V1beta2().FloatingIPProjectQuotas()
	poolInformer.Informer().GetIndexer().Add(noConfigPool)
	poolInformer.Informer().GetIndexer().Add(withConfigPool)

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(NewCollector(poolInformer.Lister(), quotaInformer.Lister(), fipInformer.Lister())))

	families, err := registry.Gather()
	require.NoError(t, err)

	subnetsByPool := map[string]string{}
	capacityByPool := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "rancherfipmanager_ippool_capacity" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			subnetsByPool[labels["fippool"]] = labels["subnet"]
			capacityByPool[labels["fippool"]] = m.GetGauge().GetValue()
		}
	}

	assert.Equal(t, map[string]string{"no-config": "", "with-config": "10.10.10.0/24"}, subnetsByPool)
	assert.Equal(t, map[string]float64{"no-config": 4, "with-config": 10}, capacityByPool)
}
