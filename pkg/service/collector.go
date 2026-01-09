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

	poolCapacity   *prometheus.Desc
	poolAvailable  *prometheus.Desc
	poolUsed       *prometheus.Desc
	quotaTimestamp *prometheus.Desc
	quotaInfo      *prometheus.Desc
	floatingIPs    *prometheus.Desc
}

func NewCollector(poolLister listers.FloatingIPPoolLister, quotaLister listers.FloatingIPProjectQuotaLister, fipLister listers.FloatingIPLister) *Collector {
	return &Collector{
		floatingIPPoolLister:         poolLister,
		floatingIPProjectQuotaLister: quotaLister,
		floatingIPLister:             fipLister,
		poolCapacity: prometheus.NewDesc("rancherfipmanager_ippool_capacity",
			"Total IPs in the pool",
			[]string{"fippool", "subnet", "target_cluster", "target_network"}, nil,
		),
		poolAvailable: prometheus.NewDesc("rancherfipmanager_ippool_available",
			"Available IPs in the pool",
			[]string{"fippool", "subnet", "target_cluster", "target_network"}, nil,
		),
		poolUsed: prometheus.NewDesc("rancherfipmanager_ippool_used",
			"Used IPs in the pool",
			[]string{"fippool", "subnet", "target_cluster", "target_network"}, nil,
		),
		quotaTimestamp: prometheus.NewDesc("rancherfipmanager_floatingipprojectquota_created",
			"Object creation timestamp",
			[]string{"project_id"}, nil,
		),
		quotaInfo: prometheus.NewDesc("rancherfipmanager_floatingipprojectquota",
			"Quota information per project",
			[]string{"project_id", "fippool", "type"}, nil,
		),
		floatingIPs: prometheus.NewDesc("rancherfipmanager_floatingips",
			"Information about floating IPs",
			[]string{"fippool", "ip", "status", "project_id", "managed_cluster_id", "service_name", "service_namespace"}, nil,
		),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.poolCapacity
	ch <- c.poolAvailable
	ch <- c.poolUsed
	ch <- c.quotaTimestamp
	ch <- c.quotaInfo
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
		ch <- prometheus.MustNewConstMetric(c.poolCapacity, prometheus.GaugeValue, float64(pool.Status.Available+pool.Status.Used),
			pool.Name,
			pool.Spec.IPConfig.Subnet,
			pool.Spec.TargetCluster,
			pool.Spec.TargetNetwork,
		)
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
		ch <- prometheus.MustNewConstMetric(c.quotaTimestamp, prometheus.GaugeValue, float64(quota.CreationTimestamp.Unix()),
			quota.Name,
		)

		for poolName, hardQuota := range quota.Spec.FloatingIPQuota {
			ch <- prometheus.MustNewConstMetric(c.quotaInfo, prometheus.GaugeValue, float64(hardQuota),
				quota.Name,
				poolName,
				"hard",
			)

			if quota.Status.FloatingIPs[poolName] != nil {
				ch <- prometheus.MustNewConstMetric(c.quotaInfo, prometheus.GaugeValue, float64(quota.Status.FloatingIPs[poolName].Used),
					quota.Name,
					poolName,
					"used",
				)
			} else {
				ch <- prometheus.MustNewConstMetric(c.quotaInfo, prometheus.GaugeValue, 0,
					quota.Name,
					poolName,
					"used",
				)
			}
		}
	}
}

func (c *Collector) collectFloatingIPs(ch chan<- prometheus.Metric) {
	var status string

	fips, err := c.floatingIPLister.List(labels.Everything())
	if err != nil {
		logrus.Errorf("failed to list floating ips: %s", err)
		return
	}
	for _, fip := range fips {
		if fip.Status.Assigned != nil {
			status = "assigned"
		} else {
			status = "unassigned"
		}
		ch <- prometheus.MustNewConstMetric(c.floatingIPs, prometheus.GaugeValue, 1,
			fip.Spec.FloatingIPPool,
			fip.Status.IPAddr,
			status,
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/project-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/cluster-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/service-name"],
			fip.ObjectMeta.Labels["rancher.k8s.binbash.org/service-namespace"],
		)
	}
}
