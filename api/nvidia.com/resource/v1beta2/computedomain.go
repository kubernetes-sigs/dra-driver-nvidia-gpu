/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ComputeDomainStatusNone     = ""
	ComputeDomainStatusReady    = "Ready"
	ComputeDomainStatusNotReady = "NotReady"

	ComputeDomainChannelAllocationModeSingle = "Single"
	ComputeDomainChannelAllocationModeAll    = "All"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:storageversion
// +kubebuilder:subresource:status

// ComputeDomain prepares a set of nodes to run a multi-node workload in.
//
// Hub is the storage / conversion hub for ComputeDomain API versions.
type ComputeDomain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ComputeDomainSpec `json:"spec,omitempty"`
	// Global ComputeDomain status. Can be used to guide debugging efforts.
	// Workload however should not rely on inspecting this field at any point
	// during its lifecycle.
	Status ComputeDomainStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComputeDomainList provides a list of ComputeDomains.
type ComputeDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComputeDomain `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="A computeDomain.spec is immutable"

// ComputeDomainSpec provides the spec for a ComputeDomain.
//
// The deprecated resource.nvidia.com/v1beta1 API carries `spec.numNodes`; that
// value is not part of this version and is preserved on the stored object via
// metadata.annotations["resource.nvidia.com/computedomain-num-nodes"].
type ComputeDomainSpec struct {
	Channel *ComputeDomainChannelSpec `json:"channel"`
}

// ComputeDomainChannelSpec provides the spec for a channel used to run a workload inside a ComputeDomain.
type ComputeDomainChannelSpec struct {
	ResourceClaimTemplate ComputeDomainResourceClaimTemplate `json:"resourceClaimTemplate"`
	// Allows for requesting all IMEX channels (the maximum per IMEX domain) or
	// precisely one.
	// +kubebuilder:validation:Enum=All;Single
	// +kubebuilder:default:=Single
	// +kubebuilder:validation:Optional
	AllocationMode string `json:"allocationMode,omitempty"`
}

// ComputeDomainResourceClaimTemplate provides the details of the ResourceClaimTemplate to generate.
type ComputeDomainResourceClaimTemplate struct {
	Name string `json:"name"`
}

// ComputeDomainStatus provides the status for a ComputeDomain.
type ComputeDomainStatus struct {
	// +kubebuilder:validation:Enum=Ready;NotReady
	// +kubebuilder:default=NotReady
	Status string `json:"status"`
	// +listType=map
	// +listMapKey=name
	Nodes []*ComputeDomainNode `json:"nodes,omitempty"`
}

// ComputeDomainNode provides information about each node added to a ComputeDomain.
type ComputeDomainNode struct {
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	CliqueID  string `json:"cliqueID"`
	// The Index field is used to ensure a consistent IP-to-DNS name
	// mapping across all machines within an IMEX domain. Each node's index
	// directly determines its DNS name within a given NVLink partition
	// (i.e. clique). In other words, the 2-tuple of (CliqueID, Index) will
	// always be unique. This field is marked as optional (but not
	// omitempty) in order to support downgrades and avoid an API bump.
	// +kubebuilder:validation:Optional
	Index int `json:"index"`
	// The Status field tracks the readiness of the IMEX daemon running on
	// this node. It gets switched to Ready whenever the IMEX daemon is
	// ready to broker GPU memory exchanges and switches to NotReady when
	// it is not. It is marked as optional in order to support downgrades
	// and avoid an API bump.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Ready;NotReady
	// +kubebuilder:default:=NotReady
	Status string `json:"status,omitempty"`
}
