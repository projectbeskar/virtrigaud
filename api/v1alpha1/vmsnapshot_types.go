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
)

// VMSnapshotSpec defines the desired state of VMSnapshot
type VMSnapshotSpec struct {
	// VMRef references the virtual machine to snapshot
	VMRef LocalObjectReference `json:"vmRef"`

	// NameHint suggests a name for the snapshot (provider may modify)
	// +optional
	NameHint string `json:"nameHint,omitempty"`

	// Memory indicates whether to include memory state in the snapshot
	// +optional
	Memory bool `json:"memory,omitempty"`

	// Description provides additional context for the snapshot
	// +optional
	Description string `json:"description,omitempty"`

	// RetentionPolicy defines how long to keep this snapshot
	// +optional
	RetentionPolicy *SnapshotRetentionPolicy `json:"retentionPolicy,omitempty"`
}

// SnapshotRetentionPolicy defines snapshot retention rules
type SnapshotRetentionPolicy struct {
	// MaxAge is the maximum age before snapshot should be deleted
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`

	// DeleteOnVMDelete indicates whether to delete snapshot when VM is deleted
	// +optional
	DeleteOnVMDelete bool `json:"deleteOnVMDelete,omitempty"`
}

// VMSnapshotStatus defines the observed state of VMSnapshot
type VMSnapshotStatus struct {
	// SnapshotID is the provider-specific identifier for the snapshot
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// Phase represents the current phase of the snapshot
	// +optional
	Phase SnapshotPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// CreationTime is when the snapshot was created
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// SizeBytes is the size of the snapshot in bytes (if available)
	// +optional
	SizeBytes *int64 `json:"sizeBytes,omitempty"`

	// TaskRef tracks any ongoing async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SnapshotPhase represents the phase of a snapshot
// +kubebuilder:validation:Enum=Pending;Creating;Ready;Deleting;Failed
type SnapshotPhase string

const (
	// SnapshotPhasePending indicates the snapshot is waiting to be processed
	SnapshotPhasePending SnapshotPhase = "Pending"
	// SnapshotPhaseCreating indicates the snapshot is being created
	SnapshotPhaseCreating SnapshotPhase = "Creating"
	// SnapshotPhaseReady indicates the snapshot is ready for use
	SnapshotPhaseReady SnapshotPhase = "Ready"
	// SnapshotPhaseDeleting indicates the snapshot is being deleted
	SnapshotPhaseDeleting SnapshotPhase = "Deleting"
	// SnapshotPhaseFailed indicates the snapshot operation failed
	SnapshotPhaseFailed SnapshotPhase = "Failed"
)

// VMSnapshot condition types
const (
	// VMSnapshotConditionReady indicates whether the snapshot is ready
	VMSnapshotConditionReady = "Ready"
	// VMSnapshotConditionCreating indicates whether the snapshot is being created
	VMSnapshotConditionCreating = "Creating"
	// VMSnapshotConditionDeleting indicates whether the snapshot is being deleted
	VMSnapshotConditionDeleting = "Deleting"
)

// VMSnapshot condition reasons
const (
	// VMSnapshotReasonCreated indicates the snapshot was successfully created
	VMSnapshotReasonCreated = "Created"
	// VMSnapshotReasonCreating indicates the snapshot is being created
	VMSnapshotReasonCreating = "Creating"
	// VMSnapshotReasonDeleting indicates the snapshot is being deleted
	VMSnapshotReasonDeleting = "Deleting"
	// VMSnapshotReasonProviderError indicates a provider error occurred
	VMSnapshotReasonProviderError = "ProviderError"
	// VMSnapshotReasonUnsupported indicates snapshots are not supported
	VMSnapshotReasonUnsupported = "Unsupported"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="VM",type=string,JSONPath=`.spec.vmRef.name`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Snapshot ID",type=string,JSONPath=`.status.snapshotID`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.sizeBytes`

// VMSnapshot is the Schema for the vmsnapshots API
type VMSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMSnapshotSpec   `json:"spec,omitempty"`
	Status VMSnapshotStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMSnapshotList contains a list of VMSnapshot
type VMSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMSnapshot `json:"items"`
}
