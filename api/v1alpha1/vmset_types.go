/*
Copyright 2025.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// VMSetSpec defines the desired state of VMSet
type VMSetSpec struct {
	// Replicas is the desired number of VMs in the set
	Replicas *int32 `json:"replicas,omitempty"`

	// Selector is a label query over VMs that should match the replica count
	Selector *metav1.LabelSelector `json:"selector"`

	// Template is the object that describes the VM that will be created
	Template VMSetTemplate `json:"template"`

	// UpdateStrategy defines how to replace existing VMs with new ones
	// +optional
	UpdateStrategy VMSetUpdateStrategy `json:"updateStrategy,omitempty"`

	// MinReadySeconds is the minimum number of seconds for which a newly created VM
	// should be ready without any of its containers crashing
	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// RevisionHistoryLimit is the number of old VMSets to retain
	// +optional
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

// VMSetTemplate defines the template for VMs in a VMSet
type VMSetTemplate struct {
	// ObjectMeta is metadata for VMs created from this template
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the VM specification
	Spec VirtualMachineSpec `json:"spec"`
}

// VMSetUpdateStrategy defines the update strategy for a VMSet
type VMSetUpdateStrategy struct {
	// Type can be "RollingUpdate" or "OnDelete"
	// +optional
	Type VMSetUpdateStrategyType `json:"type,omitempty"`

	// RollingUpdate is used when Type is RollingUpdate
	// +optional
	RollingUpdate *RollingUpdateVMSetStrategy `json:"rollingUpdate,omitempty"`
}

// VMSetUpdateStrategyType defines the type of update strategy
// +kubebuilder:validation:Enum=RollingUpdate;OnDelete
type VMSetUpdateStrategyType string

const (
	// RollingUpdateVMSetStrategyType replaces VMs one by one
	RollingUpdateVMSetStrategyType VMSetUpdateStrategyType = "RollingUpdate"
	// OnDeleteVMSetStrategyType replaces VMs only when manually deleted
	OnDeleteVMSetStrategyType VMSetUpdateStrategyType = "OnDelete"
)

// RollingUpdateVMSetStrategy defines parameters for rolling updates
type RollingUpdateVMSetStrategy struct {
	// MaxUnavailable is the maximum number of VMs that can be unavailable during update
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// MaxSurge is the maximum number of VMs that can be created above desired replica count
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// Partition indicates the ordinal at which the VMSet should be partitioned for updates
	// +optional
	Partition *int32 `json:"partition,omitempty"`
}

// VMSetStatus defines the observed state of VMSet
type VMSetStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the number of VMs created by the VMSet controller
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of VMs that are ready
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas is the number of VMs that are available
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// UpdatedReplicas is the number of VMs that have been updated
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// CurrentRevision is the revision of the current VMSet
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`

	// UpdateRevision is the revision of the updated VMSet
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`

	// CollisionCount is the count of hash collisions for the VMSet
	// +optional
	CollisionCount *int32 `json:"collisionCount,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VMSet condition types
const (
	// VMSetConditionReady indicates whether the VMSet is ready
	VMSetConditionReady = "Ready"
	// VMSetConditionProgressing indicates whether the VMSet is progressing
	VMSetConditionProgressing = "Progressing"
	// VMSetConditionReplicaFailure indicates a failure to create/delete replicas
	VMSetConditionReplicaFailure = "ReplicaFailure"
)

// VMSet condition reasons
const (
	// VMSetReasonAllReplicasReady indicates all replicas are ready
	VMSetReasonAllReplicasReady = "AllReplicasReady"
	// VMSetReasonCreatingReplicas indicates replicas are being created
	VMSetReasonCreatingReplicas = "CreatingReplicas"
	// VMSetReasonDeletingReplicas indicates replicas are being deleted
	VMSetReasonDeletingReplicas = "DeletingReplicas"
	// VMSetReasonUpdatingReplicas indicates replicas are being updated
	VMSetReasonUpdatingReplicas = "UpdatingReplicas"
	// VMSetReasonProviderError indicates a provider error occurred
	VMSetReasonProviderError = "ProviderError"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
//+kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
//+kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.replicas`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VMSet is the Schema for the vmsets API
type VMSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMSetSpec   `json:"spec,omitempty"`
	Status VMSetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMSetList contains a list of VMSet
type VMSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMSet `json:"items"`
}
