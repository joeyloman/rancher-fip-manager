package service

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/labels"

	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta1"
)

type Collector struct {
	floatingIPPoolLister         listers.FloatingIPPoolLister
	floatingIPProjectQuotaLister listers.FloatingIPProjectQuotaLister
	floatingIPLister             listers.FloatingIPLister

	poolAvailable *prometheus.Desc
	poolUsed      *prometheus.Desc
	quotaUsage    *prometheus.Desc
	floatingIPs   *prometheus.Desc
}

func NewCollector(poolLister listers.FloatingIPPoolLister, quotaLister listers.FloatingIPProjectQuotaLister, fipLister listers.FloatingIPLister) *Collector {
	return &Collector{
		floatingIPPoolLister:         poolLister,
		floatingIPProjectQuotaLister: quotaLister,
		floatingIPLister:             fipLister,
		poolAvailable: prometheus.NewDesc("rancherfipmanager_ippool_available",
			"Available IPs in the pool",
			[]string{"fippool", "subnet", "targetcluster", "targetnetwork"}, nil,
		),
		poolUsed: prometheus.NewDesc("rancherfipmanager_ippool_used",
			"Used IPs in the pool",
			[]string{"fippool", "subnet", "targetcluster", "targetnetwork"}, nil,
		),
		quotaUsage: prometheus.NewDesc("rancherfipmanager_floatingipprojectquota_usage",
			"Used IPs per project quota",
			[]string{"project", "fippool"}, nil,
		),
		floatingIPs: prometheus.NewDesc("rancherfipmanager_floatingips",
			"Information about floating IPs",
			[]string{"fippool", "ipaddr", "state", "project", "cluster", "servicename", "servicenamespace"}, nil,
		),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.poolAvailable
	ch <- c.poolUsed
	ch <- c.quotaUsage
	ch <- c.floatingIPs
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.collectPools(ch)
	c.collectQuotas(ch)
	c.collectFloatingIPs(ch)
}

func (c *Collector) collectPools(ch chan<- prometheus.Metric) {
	pools, err := c.floatingIPPoolLister.List(labels.Everything())
	if err != nil {
		logrus.Errorf("failed to list floating ip pools: %s", err)
		return
	}
	for _, pool := range pools {
		ch <- prometheus.MustNewConstMetric(c.poolAvailable, prometheus.GaugeValue, float64(pool.Status.Available),
			pool.Name,
			pool.Spec.IPConfig.Subnet,
			pool.Spec.TargetCluster,
			pool.Spec.TargetNetwork,
		)
		ch <- prometheus.MustNewConstMetric(c.poolUsed, prometheus.GaugeValue, float64(pool.Status.Used),
			pool.Name,
			pool.Spec.IPConfig.Subnet,
			pool.Spec.TargetCluster,
			pool.Spec.TargetNetwork,
		)
	}
}

func (c *Collector) collectQuotas(ch chan<- prometheus.Metric) {
	quotas, err := c.floatingIPProjectQuotaLister.List(labels.Everything())
	if err != nil {
		logrus.Errorf("failed to list floating ip project quotas: %s", err)
		return
	}
	for _, quota := range quotas {
		for poolName, fipInfo := range quota.Status.FloatingIPs {
			ch <- prometheus.MustNewConstMetric(c.quotaUsage, prometheus.GaugeValue, float64(fipInfo.Used),
				quota.Name,
				poolName,
			)
		}
	}
}

func (c *Collector) collectFloatingIPs(ch chan<- prometheus.Metric) {
	fips, err := c.floatingIPLister.List(labels.Everything())
	if err != nil {
		logrus.Errorf("failed to list floating ips: %s", err)
		return
	}
	for _, fip := range fips {
		ch <- prometheus.MustNewConstMetric(c.floatingIPs, prometheus.GaugeValue, 1,
			fip.Spec.FloatingIPPool,
			fip.Status.IPAddr,
			fip.Status.State,
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/project-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/cluster-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/service-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/service-namespace"],
		)
	}
}
