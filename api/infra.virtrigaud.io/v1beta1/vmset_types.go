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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// VMSetSpec defines the desired state of VMSet
type VMSetSpec struct {
	// Replicas is the desired number of VMs in the set
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
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
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// RevisionHistoryLimit is the number of old VMSets to retain
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// PersistentVolumeClaimRetentionPolicy defines the retention policy for PVCs
	// +optional
	PersistentVolumeClaimRetentionPolicy *VMSetPersistentVolumeClaimRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy,omitempty"`

	// Ordinals configures the sequential ordering of VM indices
	// +optional
	Ordinals *VMSetOrdinals `json:"ordinals,omitempty"`

	// ServiceName is the name of the service that governs this VMSet
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ServiceName string `json:"serviceName,omitempty"`

	// VolumeClaimTemplates defines a list of claims that VMs are allowed to reference
	// +optional
	// +kubebuilder:validation:MaxItems=20
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
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
	// +kubebuilder:default="RollingUpdate"
	Type VMSetUpdateStrategyType `json:"type,omitempty"`

	// RollingUpdate is used when Type is RollingUpdate
	// +optional
	RollingUpdate *RollingUpdateVMSetStrategy `json:"rollingUpdate,omitempty"`
}

// VMSetUpdateStrategyType defines the type of update strategy
// +kubebuilder:validation:Enum=RollingUpdate;OnDelete;Recreate
type VMSetUpdateStrategyType string

const (
	// RollingUpdateVMSetStrategyType replaces VMs one by one
	RollingUpdateVMSetStrategyType VMSetUpdateStrategyType = "RollingUpdate"
	// OnDeleteVMSetStrategyType replaces VMs only when manually deleted
	OnDeleteVMSetStrategyType VMSetUpdateStrategyType = "OnDelete"
	// RecreateVMSetStrategyType deletes all VMs before creating new ones
	RecreateVMSetStrategyType VMSetUpdateStrategyType = "Recreate"
)

// RollingUpdateVMSetStrategy defines parameters for rolling updates
type RollingUpdateVMSetStrategy struct {
	// MaxUnavailable is the maximum number of VMs that can be unavailable during update
	// +optional
	// +kubebuilder:default="25%"
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// MaxSurge is the maximum number of VMs that can be created above desired replica count
	// +optional
	// +kubebuilder:default="25%"
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// Partition indicates the ordinal at which the VMSet should be partitioned for updates
	// +optional
	// +kubebuilder:validation:Minimum=0
	Partition *int32 `json:"partition,omitempty"`

	// PodManagementPolicy controls how VMs are created during initial scale up,
	// when replacing VMs on nodes, or when scaling down
	// +optional
	// +kubebuilder:default="OrderedReady"
	// +kubebuilder:validation:Enum=OrderedReady;Parallel
	PodManagementPolicy VMSetPodManagementPolicyType `json:"podManagementPolicy,omitempty"`
}

// VMSetPodManagementPolicyType defines the policy for creating VMs
// +kubebuilder:validation:Enum=OrderedReady;Parallel
type VMSetPodManagementPolicyType string

const (
	// OrderedReadyVMSetPodManagementPolicy creates VMs in order and waits for each to be ready
	OrderedReadyVMSetPodManagementPolicy VMSetPodManagementPolicyType = "OrderedReady"
	// ParallelVMSetPodManagementPolicy creates VMs in parallel
	ParallelVMSetPodManagementPolicy VMSetPodManagementPolicyType = "Parallel"
)

// VMSetPersistentVolumeClaimRetentionPolicy defines the retention policy for PVCs
type VMSetPersistentVolumeClaimRetentionPolicy struct {
	// WhenDeleted specifies what happens to PVCs created from VMSet VolumeClaimTemplates when the VMSet is deleted
	// +optional
	// +kubebuilder:default="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	WhenDeleted PersistentVolumeClaimRetentionPolicyType `json:"whenDeleted,omitempty"`

	// WhenScaled specifies what happens to PVCs created from VMSet VolumeClaimTemplates when the VMSet is scaled down
	// +optional
	// +kubebuilder:default="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	WhenScaled PersistentVolumeClaimRetentionPolicyType `json:"whenScaled,omitempty"`
}

// PersistentVolumeClaimRetentionPolicyType defines the retention policy type
// +kubebuilder:validation:Enum=Retain;Delete
type PersistentVolumeClaimRetentionPolicyType string

const (
	// RetainPersistentVolumeClaimRetentionPolicyType retains PVCs
	RetainPersistentVolumeClaimRetentionPolicyType PersistentVolumeClaimRetentionPolicyType = "Retain"
	// DeletePersistentVolumeClaimRetentionPolicyType deletes PVCs
	DeletePersistentVolumeClaimRetentionPolicyType PersistentVolumeClaimRetentionPolicyType = "Delete"
)

// VMSetOrdinals configures the sequential ordering of VM indices
type VMSetOrdinals struct {
	// Start is the number representing the first replica's index
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999999
	Start int32 `json:"start,omitempty"`
}

// PersistentVolumeClaimTemplate describes a PVC template for VMSet VMs
type PersistentVolumeClaimTemplate struct {
	// ObjectMeta is the standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired characteristics of the volume
	Spec PersistentVolumeClaimSpec `json:"spec"`
}

// PersistentVolumeClaimSpec describes the common attributes of storage devices
type PersistentVolumeClaimSpec = corev1.PersistentVolumeClaimSpec

// TypedLocalObjectReference contains enough information to let you locate the typed referenced object
type TypedLocalObjectReference = corev1.TypedLocalObjectReference

// TypedObjectReference represents a typed object reference
type TypedObjectReference = corev1.TypedObjectReference

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

	// CurrentReplicas is the number of VMs currently running
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// UpdateStatus provides detailed update operation status
	// +optional
	UpdateStatus *VMSetUpdateStatus `json:"updateStatus,omitempty"`

	// VMStatus provides per-VM status information
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	VMStatus []VMSetVMStatus `json:"vmStatus,omitempty"`
}

// VMSetUpdateStatus provides detailed update operation status
type VMSetUpdateStatus struct {
	// Phase represents the current phase of the update
	// +optional
	Phase VMSetUpdatePhase `json:"phase,omitempty"`

	// Message provides additional details about the update
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the update started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the update completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// UpdatedVMs lists VMs that have been updated
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	UpdatedVMs []string `json:"updatedVMs,omitempty"`

	// PendingVMs lists VMs that are pending update
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	PendingVMs []string `json:"pendingVMs,omitempty"`

	// FailedVMs lists VMs that failed to update
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	FailedVMs []VMSetFailedVM `json:"failedVMs,omitempty"`
}

// VMSetUpdatePhase represents the phase of a VMSet update
// +kubebuilder:validation:Enum=Pending;InProgress;Paused;Completed;Failed
type VMSetUpdatePhase string

const (
	// VMSetUpdatePhasePending indicates the update is pending
	VMSetUpdatePhasePending VMSetUpdatePhase = "Pending"
	// VMSetUpdatePhaseInProgress indicates the update is in progress
	VMSetUpdatePhaseInProgress VMSetUpdatePhase = "InProgress"
	// VMSetUpdatePhasePaused indicates the update is paused
	VMSetUpdatePhasePaused VMSetUpdatePhase = "Paused"
	// VMSetUpdatePhaseCompleted indicates the update is completed
	VMSetUpdatePhaseCompleted VMSetUpdatePhase = "Completed"
	// VMSetUpdatePhaseFailed indicates the update failed
	VMSetUpdatePhaseFailed VMSetUpdatePhase = "Failed"
)

// VMSetFailedVM represents a VM that failed to update
type VMSetFailedVM struct {
	// Name is the name of the failed VM
	Name string `json:"name"`

	// Reason provides the reason for failure
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message provides additional details about the failure
	// +optional
	Message string `json:"message,omitempty"`

	// LastAttempt is when the last update attempt was made
	// +optional
	LastAttempt *metav1.Time `json:"lastAttempt,omitempty"`

	// RetryCount is the number of retry attempts
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`
}

// VMSetVMStatus provides per-VM status information
type VMSetVMStatus struct {
	// Name is the VM name
	Name string `json:"name"`

	// Phase is the VM phase
	// +optional
	Phase VirtualMachinePhase `json:"phase,omitempty"`

	// Ready indicates if the VM is ready
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Revision is the VM revision
	// +optional
	Revision string `json:"revision,omitempty"`

	// CreationTime is when the VM was created
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// LastUpdateTime is when the VM was last updated
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Message provides additional VM status information
	// +optional
	Message string `json:"message,omitempty"`
}

// VMSet condition types
const (
	// VMSetConditionReady indicates whether the VMSet is ready
	VMSetConditionReady = "Ready"
	// VMSetConditionProgressing indicates whether the VMSet is progressing
	VMSetConditionProgressing = "Progressing"
	// VMSetConditionReplicaFailure indicates a failure to create/delete replicas
	VMSetConditionReplicaFailure = "ReplicaFailure"
	// VMSetConditionUpdateInProgress indicates an update is in progress
	VMSetConditionUpdateInProgress = "UpdateInProgress"
	// VMSetConditionScaling indicates scaling is in progress
	VMSetConditionScaling = "Scaling"
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
	// VMSetReasonScalingUp indicates the VMSet is scaling up
	VMSetReasonScalingUp = "ScalingUp"
	// VMSetReasonScalingDown indicates the VMSet is scaling down
	VMSetReasonScalingDown = "ScalingDown"
	// VMSetReasonProviderError indicates a provider error occurred
	VMSetReasonProviderError = "ProviderError"
	// VMSetReasonInsufficientResources indicates insufficient resources
	VMSetReasonInsufficientResources = "InsufficientResources"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
//+kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
//+kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.replicas`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmset

// VMSet is the Schema for the vmsets API
// +kubebuilder:storageversion
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

func init() {
	SchemeBuilder.Register(&VMSet{}, &VMSetList{})
}
