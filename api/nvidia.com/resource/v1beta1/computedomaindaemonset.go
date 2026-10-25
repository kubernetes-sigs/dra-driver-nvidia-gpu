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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// ComputeDomainDaemonSetName is the name of the only ComputeDomainDaemonSet.
	ComputeDomainDaemonSetName = "default"

	ComputeDomainDaemonSetUpdateStrategyOnDelete      = "OnDelete"
	ComputeDomainDaemonSetUpdateStrategyRollingUpdate = "RollingUpdate"

	// ComputeDomainDaemonSetConditionReady is true when every scheduled IMEX
	// daemon is ready.
	ComputeDomainDaemonSetConditionReady = "Ready"
	// ComputeDomainDaemonSetConditionRolloutInProgress is true while daemons
	// run an outdated revision.
	ComputeDomainDaemonSetConditionRolloutInProgress = "RolloutInProgress"
	// ComputeDomainDaemonSetConditionDeletionBlocked is true while deletion
	// waits for workloads that use IMEX channels.
	ComputeDomainDaemonSetConditionDeletionBlocked = "DeletionBlocked"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster,shortName=cdds
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.numberReady`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredNumberScheduled`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedNumberScheduled`
// +kubebuilder:printcolumn:name="Cliques",type=integer,JSONPath=`.status.numberCliques`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.updateStrategy.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="the ComputeDomainDaemonSet must be named default"

// ComputeDomainDaemonSet runs one IMEX daemon on every node of every NVLink
// clique, shared by all ComputeDomains. It is used when imex.mode is
// driverManaged and imex.scope is cluster, where each ComputeDomain gets its
// own IMEX channels instead of its own IMEX daemons.
//
// The cluster admin creates and updates it, not Helm, so that driver upgrades
// do not restart IMEX daemons under running workloads. It must exist before
// workloads can use ComputeDomains.
type ComputeDomainDaemonSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:default:={}
	Spec   ComputeDomainDaemonSetSpec   `json:"spec,omitempty"`
	Status ComputeDomainDaemonSetStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComputeDomainDaemonSetList provides a list of ComputeDomainDaemonSets.
type ComputeDomainDaemonSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComputeDomainDaemonSet `json:"items"`
}

// ComputeDomainDaemonSetSpec configures the IMEX daemons.
type ComputeDomainDaemonSetSpec struct {
	// Image of the IMEX daemon pods. When empty, the driver image at creation
	// is kept, so that driver upgrades do not change the daemons. Set it to
	// upgrade them.
	// +kubebuilder:validation:Optional
	Image string `json:"image,omitempty"`

	// ImagePullSecrets in the driver namespace for the IMEX daemon pods.
	// +kubebuilder:validation:Optional
	// +listType=atomic
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// NodeSelector restricts the IMEX daemons to matching nodes. Daemons only
	// run on nodes that belong to an NVLink clique.
	// +kubebuilder:validation:Optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// PriorityClassName of the IMEX daemon pods.
	// +kubebuilder:validation:Optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// Resources of the IMEX daemon container.
	// +kubebuilder:validation:Optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// MaxNodesPerClique bounds the number of IMEX daemons per NVLink clique.
	// It sizes the static IMEX nodes config and cannot be changed.
	// +kubebuilder:default:=18
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="maxNodesPerClique is immutable"
	// +kubebuilder:validation:Optional
	MaxNodesPerClique int `json:"maxNodesPerClique,omitempty"`

	// IMEXConfig overrides, or adds, nvidia-imex config file settings.
	// +kubebuilder:validation:Optional
	IMEXConfig map[string]string `json:"imexConfig,omitempty"`

	// LogVerbosity of the IMEX daemon pods.
	// +kubebuilder:default:=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	// +kubebuilder:validation:Optional
	LogVerbosity int `json:"logVerbosity,omitempty"`

	// UpdateStrategy decides how IMEX daemons pick up spec changes.
	// +kubebuilder:default:={type: OnDelete}
	// +kubebuilder:validation:Optional
	UpdateStrategy ComputeDomainDaemonSetUpdateStrategy `json:"updateStrategy,omitempty"`
}

// ComputeDomainDaemonSetUpdateStrategy decides how IMEX daemons pick up spec
// changes. Restarting an IMEX daemon disrupts the workloads of its whole
// clique, so updates never restart daemons node by node.
// +kubebuilder:validation:XValidation:rule="self.type == 'RollingUpdate' || !has(self.rollingUpdate)",message="rollingUpdate requires type RollingUpdate"
type ComputeDomainDaemonSetUpdateStrategy struct {
	// Type is OnDelete (default) or RollingUpdate. With OnDelete, daemons
	// keep their revision until their pods are deleted. With RollingUpdate,
	// the controller restarts outdated daemons one batch of whole cliques at
	// a time.
	// +kubebuilder:validation:Enum=OnDelete;RollingUpdate
	// +kubebuilder:default:=OnDelete
	// +kubebuilder:validation:Optional
	Type string `json:"type,omitempty"`

	// RollingUpdate configures RollingUpdate.
	// +kubebuilder:validation:Optional
	RollingUpdate *ComputeDomainDaemonSetRollingUpdate `json:"rollingUpdate,omitempty"`
}

// ComputeDomainDaemonSetRollingUpdate configures rolling updates by clique.
type ComputeDomainDaemonSetRollingUpdate struct {
	// MaxUnavailableCliques is the number, or percentage, of cliques that
	// are updated at a time. A percentage rounds down, to at least 1.
	// +kubebuilder:default:=1
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:Optional
	MaxUnavailableCliques *intstr.IntOrString `json:"maxUnavailableCliques,omitempty"`

	// MinReadySeconds is how long all daemons of an updated clique must be
	// ready before the next batch starts.
	// +kubebuilder:default:=30
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// Paused stops starting new batches. A batch in progress completes.
	// +kubebuilder:validation:Optional
	Paused bool `json:"paused,omitempty"`
}

// ComputeDomainDaemonSetStatus reports the IMEX daemons.
type ComputeDomainDaemonSetStatus struct {
	// ObservedGeneration is the generation the status reflects.
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Image of the current revision.
	// +kubebuilder:validation:Optional
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:Optional
	DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
	// +kubebuilder:validation:Optional
	NumberReady int32 `json:"numberReady"`
	// UpdatedNumberScheduled is the number of daemons on the current
	// revision.
	// +kubebuilder:validation:Optional
	UpdatedNumberScheduled int32 `json:"updatedNumberScheduled"`
	// +kubebuilder:validation:Optional
	NumberCliques int32 `json:"numberCliques"`

	// Cliques reports the daemons of each NVLink clique, sorted by ID.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=id
	Cliques []ComputeDomainDaemonSetCliqueStatus `json:"cliques,omitempty"`

	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ComputeDomainDaemonSetCliqueStatus reports the IMEX daemons of one clique.
type ComputeDomainDaemonSetCliqueStatus struct {
	ID string `json:"id"`
	// +kubebuilder:validation:Optional
	Nodes int32 `json:"nodes"`
	// +kubebuilder:validation:Optional
	ReadyNodes int32 `json:"readyNodes"`
	// UpdatedNodes is the number of daemons on the current revision.
	// +kubebuilder:validation:Optional
	UpdatedNodes int32 `json:"updatedNodes"`
}
