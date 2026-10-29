package purelb

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group version for PureLB API
var GroupVersion = schema.GroupVersion{Group: "purelb.io", Version: "v1"}

// SchemeGroupVersion is used to register these objects
var SchemeGroupVersion = GroupVersion

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

var (
	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes adds the set of types defined in this package to the supplied scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&ServiceGroup{},
		&ServiceGroupList{},
		&LBNodeAgent{},
		&LBNodeAgentList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// ServiceGroup is a PureLB custom resource that defines address pools for load balancer services.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ServiceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServiceGroupSpec `json:"spec,omitempty"`
}

// DeepCopyObject implements runtime.Object.
func (in *ServiceGroup) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopy creates a deep copy of ServiceGroup.
func (in *ServiceGroup) DeepCopy() *ServiceGroup {
	if in == nil {
		return nil
	}
	out := new(ServiceGroup)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given ServiceGroup.
func (in *ServiceGroup) DeepCopyInto(out *ServiceGroup) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// GetObjectKind implements runtime.Object.
func (in *ServiceGroup) GetObjectKind() schema.ObjectKind {
	return &in.TypeMeta
}

// ServiceGroupSpec defines the specification for a ServiceGroup.
type ServiceGroupSpec struct {
	Local *ServiceGroupLocal `json:"local,omitempty"`
}

// DeepCopyInto copies the receiver into the given ServiceGroupSpec.
func (in *ServiceGroupSpec) DeepCopyInto(out *ServiceGroupSpec) {
	*out = *in
	if in.Local != nil {
		in, out := &in.Local, &out.Local
		*out = new(ServiceGroupLocal)
		(*in).DeepCopyInto(*out)
	}
}

// ServiceGroupLocal defines local address pools.
type ServiceGroupLocal struct {
	V4Pools []AddressPool `json:"v4pools,omitempty"`
	V6Pools []AddressPool `json:"v6pools,omitempty"`
}

// DeepCopyInto copies the receiver into the given ServiceGroupLocal.
func (in *ServiceGroupLocal) DeepCopyInto(out *ServiceGroupLocal) {
	*out = *in
	if in.V4Pools != nil {
		in, out := &in.V4Pools, &out.V4Pools
		*out = make([]AddressPool, len(*in))
		copy(*out, *in)
	}
	if in.V6Pools != nil {
		in, out := &in.V6Pools, &out.V6Pools
		*out = make([]AddressPool, len(*in))
		copy(*out, *in)
	}
}

// AddressPool defines an address pool configuration.
type AddressPool struct {
	Subnet      string `json:"subnet"`
	Pool        string `json:"pool"`
	Aggregation string `json:"aggregation,omitempty"`
}

// ServiceGroupList is a list of ServiceGroup resources.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ServiceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ServiceGroup `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (in *ServiceGroupList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopy creates a deep copy of ServiceGroupList.
func (in *ServiceGroupList) DeepCopy() *ServiceGroupList {
	if in == nil {
		return nil
	}
	out := new(ServiceGroupList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given ServiceGroupList.
func (in *ServiceGroupList) DeepCopyInto(out *ServiceGroupList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]ServiceGroup, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// GetObjectKind implements runtime.Object.
func (in *ServiceGroupList) GetObjectKind() schema.ObjectKind {
	return &in.TypeMeta
}

// LBNodeAgent is a PureLB custom resource that configures the load balancer node agent.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type LBNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LBNodeAgentSpec `json:"spec,omitempty"`
}

// DeepCopyObject implements runtime.Object.
func (in *LBNodeAgent) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopy creates a deep copy of LBNodeAgent.
func (in *LBNodeAgent) DeepCopy() *LBNodeAgent {
	if in == nil {
		return nil
	}
	out := new(LBNodeAgent)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given LBNodeAgent.
func (in *LBNodeAgent) DeepCopyInto(out *LBNodeAgent) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

// GetObjectKind implements runtime.Object.
func (in *LBNodeAgent) GetObjectKind() schema.ObjectKind {
	return &in.TypeMeta
}

// LBNodeAgentSpec defines the specification for an LBNodeAgent.
type LBNodeAgentSpec struct {
	Local *LBNodeAgentLocal `json:"local,omitempty"`
}

// DeepCopyInto copies the receiver into the given LBNodeAgentSpec.
func (in *LBNodeAgentSpec) DeepCopyInto(out *LBNodeAgentSpec) {
	*out = *in
	if in.Local != nil {
		in, out := &in.Local, &out.Local
		*out = new(LBNodeAgentLocal)
		(*in).DeepCopyInto(*out)
	}
}

// LBNodeAgentLocal defines local node agent configuration.
type LBNodeAgentLocal struct {
	ExtLBInt string `json:"extlbint,omitempty"`
	LocalInt string `json:"localint,omitempty"`
	SendGARP *bool  `json:"sendgarp,omitempty"`
}

// DeepCopyInto copies the receiver into the given LBNodeAgentLocal.
func (in *LBNodeAgentLocal) DeepCopyInto(out *LBNodeAgentLocal) {
	*out = *in
	if in.SendGARP != nil {
		in, out := &in.SendGARP, &out.SendGARP
		*out = new(bool)
		**out = **in
	}
}

// LBNodeAgentList is a list of LBNodeAgent resources.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type LBNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []LBNodeAgent `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (in *LBNodeAgentList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopy creates a deep copy of LBNodeAgentList.
func (in *LBNodeAgentList) DeepCopy() *LBNodeAgentList {
	if in == nil {
		return nil
	}
	out := new(LBNodeAgentList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into the given LBNodeAgentList.
func (in *LBNodeAgentList) DeepCopyInto(out *LBNodeAgentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]LBNodeAgent, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// GetObjectKind implements runtime.Object.
func (in *LBNodeAgentList) GetObjectKind() schema.ObjectKind {
	return &in.TypeMeta
}
