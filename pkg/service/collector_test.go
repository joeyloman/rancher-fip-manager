package service

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	rfmv2 "github.com/joeyloman/rancher-fip-manager/pkg/apis/rancher.k8s.binbash.org/v1beta2"
	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta2"
)

func newTestCollector(t *testing.T, pools ...*rfmv2.FloatingIPPool) *Collector {
	t.Helper()

	newIndexer := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	}

	poolIdx := newIndexer()
	for _, pool := range pools {
		require.NoError(t, poolIdx.Add(pool))
	}

	return NewCollector(
		listers.NewFloatingIPPoolLister(poolIdx),
		listers.NewFloatingIPProjectQuotaLister(newIndexer()),
		listers.NewFloatingIPLister(newIndexer()),
	)
}

// A pool without ipConfig must not panic the metrics collector
// (promhttp performs no panic recovery; a panic here breaks every /metrics scrape).
func TestCollectorPoolWithoutIPConfig(t *testing.T) {
	healthy := &rfmv2.FloatingIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy"},
		Spec: rfmv2.FloatingIPPoolSpec{
			IPConfig: &rfmv2.IPConfig{
				Subnet: "10.0.0.0/24",
				Pool:   rfmv2.Pool{Start: "10.0.0.10", End: "10.0.0.20"},
			},
			TargetCluster: "cluster-a",
			TargetNetwork: "net-a",
		},
		Status: rfmv2.FloatingIPPoolStatus{Available: 5, Used: 3},
	}
	broken := &rfmv2.FloatingIPPool{ObjectMeta: metav1.ObjectMeta{Name: "broken"}}

	c := newTestCollector(t, healthy, broken)

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))

	metrics, err := reg.Gather()
	require.NoError(t, err)

	got := map[string]float64{}
	for _, mf := range metrics {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if pool, ok := labels["fippool"]; ok {
				got[mf.GetName()+"|"+pool] = m.GetGauge().GetValue()
			}
		}
	}

	assert.Equal(t, 8.0, got["rancherfipmanager_ippool_capacity|healthy"])
	assert.Equal(t, 5.0, got["rancherfipmanager_ippool_available|healthy"])
	assert.Equal(t, 3.0, got["rancherfipmanager_ippool_used|healthy"])
	assert.NotContains(t, got, "rancherfipmanager_ippool_capacity|broken")
}
