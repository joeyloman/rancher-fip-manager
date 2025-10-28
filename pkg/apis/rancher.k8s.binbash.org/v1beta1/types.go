package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster

// FloatingIPProjectQuota defines project-specific configurations for floating IPs.
type FloatingIPProjectQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FloatingIPProjectQuotaSpec   `json:"spec,omitempty"`
	Status FloatingIPProjectQuotaStatus `json:"status,omitempty"`
}

// FloatingIPProjectQuotaSpec defines the desired state of FloatingIPProjectQuota.
type FloatingIPProjectQuotaSpec struct {
	DisplayName     string         `json:"displayName,omitempty"`
	FloatingIPQuota map[string]int `json:"floatingIPQuota,omitempty"`
}

// FloatingIPProjectQuotaStatus defines the observed state of FloatingIPProjectQuota.
type FloatingIPProjectQuotaStatus struct {
	// Fips is a map from pool name to FIP info.
	FloatingIPs map[string]*FipInfo `json:"fips,omitempty"`
}

// FipInfo contains allocation details for a specific IP version.
type FipInfo struct {
	Family    string            `json:"family,omitempty"` // "IPv4" or "IPv6"
	Allocated map[string]string `json:"allocated,omitempty"`
	Used      int               `json:"used,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FloatingIPProjectQuotaList contains a list of FloatingIPProjectQuota
type FloatingIPProjectQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FloatingIPProjectQuota `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster

// FloatingIPPool defines a pool of available floating IPs.
type FloatingIPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FloatingIPPoolSpec   `json:"spec,omitempty"`
	Status FloatingIPPoolStatus `json:"status,omitempty"`
}

// FloatingIPPoolSpec defines the desired state of FloatingIPPool.
type FloatingIPPoolSpec struct {
	IPConfig               *IPConfig `json:"ipConfig,omitempty"`
	TargetCluster          string    `json:"targetCluster,omitempty"`
	TargetNetwork          string    `json:"targetNetwork,omitempty"`
	TargetNetworkInterface string    `json:"targetNetworkInterface,omitempty"`
}

// IPConfig defines the configuration for an IP address family.
type IPConfig struct {
	Family string `json:"family,omitempty"` // "IPv4" or "IPv6"
	Subnet string `json:"subnet"`
	Pool   Pool   `json:"pool"`
}

// Pool defines the start and end of an IP range.
type Pool struct {
	Start   string   `json:"start"`
	End     string   `json:"end"`
	Exclude []string `json:"exclude,omitempty"`
}

// FloatingIPPoolStatus defines the observed state of FloatingIPPool.
type FloatingIPPoolStatus struct {
	Allocated map[string]string `json:"allocated,omitempty"`
	Used      int               `json:"used,omitempty"`
	Available int               `json:"available,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FloatingIPPoolList contains a list of FloatingIPPool
type FloatingIPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FloatingIPPool `json:"items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Namespaced

// FloatingIP represents a request for a floating IP.
type FloatingIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FloatingIPSpec   `json:"spec,omitempty"`
	Status FloatingIPStatus `json:"status,omitempty"`
}

// FloatingIPSpec defines the desired state of FloatingIP.
type FloatingIPSpec struct {
	IPAddr         *string `json:"ipAddr,omitempty"`
	FloatingIPPool string  `json:"floatingIPPool"`
}

// FloatingIPStatus defines the observed state of FloatingIP.
type FloatingIPStatus struct {
	IPAddr     string             `json:"ipAddr,omitempty"`
	State      string             `json:"state,omitempty"`
	Assigned   *AssignedInfo      `json:"assigned,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AssignedInfo contains details about the service a FloatingIP is assigned to.
type AssignedInfo struct {
	ClusterName      string `json:"clusterName,omitempty"`
	ProjectName      string `json:"projectName,omitempty"`
	ServiceNamespace string `json:"serviceNamespace,omitempty"`
	ServiceName      string `json:"serviceName,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FloatingIPList contains a list of FloatingIP
type FloatingIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FloatingIP `json:"items"`
}
