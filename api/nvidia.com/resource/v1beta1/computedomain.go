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

package v1beta1

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
// +kubebuilder:subresource:status

// ComputeDomain prepares a set of nodes to run a multi-node workload in.
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
// +kubebuilder:validation:XValidation:rule="has(self.channel) != has(self.channels)", message="exactly one of channel and channels must be set"

// ComputeDomainSpec provides the spec for a ComputeDomain.
type ComputeDomainSpec struct {
	// Intended number of IMEX daemons (i.e., individual compute nodes) in the
	// ComputeDomain. Must be zero or greater.
	//
	// With `featureGates.IMEXDaemonsWithDNSNames=true` (the default), this is
	// recommended to be set to zero (default). Workload must implement and consult its
	// own source of truth for the number of workers online before trying to
	// share GPU memory (and hence triggering IMEX interaction). When non-zero,
	// `numNodes` is used only for automatically updating the global
	// ComputeDomain `Status` (indicating `Ready` when the number of ready IMEX
	// daemons equals `numNodes`). In this mode, a `numNodes` value greater than
	// zero in particular does not gate the startup of IMEX daemons: individual
	// IMEX daemons are started immediately without waiting for its peers, and
	// any workload pod gets released right after its local IMEX daemon has
	// started.
	//
	// With `featureGates.IMEXDaemonsWithDNSNames=false`, `numNodes` must be set
	// to the expected number of worker nodes joining the ComputeDomain. In that
	// mode, all workload pods are held back (with containers in state
	// `ContainerCreating`) until the underlying IMEX domain has been joined by
	// `numNodes` IMEX daemons. Pods from more than `numNodes` nodes trying to
	// join the ComputeDomain may lead to unexpected behavior.
	//
	// The `numNodes` parameter is deprecated and will be removed in the next
	// API version.
	// +kubebuilder:default:=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Optional
	NumNodes int `json:"numNodes,omitempty"`
	// A single workload ResourceClaimTemplate. Exactly one of channel and
	// channels must be set.
	// +kubebuilder:validation:Optional
	Channel *ComputeDomainChannelSpec `json:"channel,omitempty"`
	// Workload ResourceClaimTemplates, each backed by its own IMEX channel.
	// Pods using the same template share its channel. Requires the
	// ComputeDomainChannelReservations feature gate.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:XValidation:rule="self.all(c, self.exists_one(d, d.resourceClaimTemplate.name == c.resourceClaimTemplate.name))", message="resourceClaimTemplate names must be unique"
	// +kubebuilder:validation:Optional
	Channels []ComputeDomainChannelEntry `json:"channels,omitempty"`
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

// ComputeDomainChannelEntry provides the spec for one of several channels of
// a ComputeDomain.
type ComputeDomainChannelEntry struct {
	ResourceClaimTemplate ComputeDomainResourceClaimTemplate `json:"resourceClaimTemplate"`
}

// ComputeDomainResourceClaimTemplate provides the details of the ResourceClaimTemplate to generate.
type ComputeDomainResourceClaimTemplate struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// GetChannels returns channel or channels as a list, so that callers handle
// both forms alike.
func (s *ComputeDomainSpec) GetChannels() []ComputeDomainChannelSpec {
	if s.Channel != nil {
		return []ComputeDomainChannelSpec{*s.Channel}
	}
	channels := make([]ComputeDomainChannelSpec, 0, len(s.Channels))
	for _, c := range s.Channels {
		channels = append(channels, ComputeDomainChannelSpec{
			ResourceClaimTemplate: c.ResourceClaimTemplate,
			AllocationMode:        ComputeDomainChannelAllocationModeSingle,
		})
	}
	return channels
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
