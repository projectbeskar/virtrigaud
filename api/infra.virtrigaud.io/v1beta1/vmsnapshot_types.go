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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VMSnapshotSpec defines the desired state of VMSnapshot
type VMSnapshotSpec struct {
	// VMRef references the virtual machine to snapshot
	VMRef LocalObjectReference `json:"vmRef"`

	// SnapshotConfig defines snapshot configuration options
	// +optional
	SnapshotConfig *SnapshotConfig `json:"snapshotConfig,omitempty"`

	// RetentionPolicy defines how long to keep this snapshot
	// +optional
	RetentionPolicy *SnapshotRetentionPolicy `json:"retentionPolicy,omitempty"`

	// Schedule defines automated snapshot scheduling
	// +optional
	Schedule *SnapshotSchedule `json:"schedule,omitempty"`

	// Metadata contains snapshot metadata
	// +optional
	Metadata *SnapshotMetadata `json:"metadata,omitempty"`
}

// SnapshotConfig defines snapshot configuration options
type SnapshotConfig struct {
	// Name provides a name hint for the snapshot (provider may modify)
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9]([a-zA-Z0-9\\-_]*[a-zA-Z0-9])?$"
	Name string `json:"name,omitempty"`

	// Description provides additional context for the snapshot
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// IncludeMemory indicates whether to include memory state in the snapshot
	// +optional
	// +kubebuilder:default=false
	IncludeMemory bool `json:"includeMemory,omitempty"`

	// Quiesce indicates whether to quiesce the file system before snapshotting
	// +optional
	// +kubebuilder:default=true
	Quiesce bool `json:"quiesce,omitempty"`

	// Type specifies the snapshot type
	// +optional
	// +kubebuilder:default="Standard"
	// +kubebuilder:validation:Enum=Standard;Crash;Application
	Type SnapshotType `json:"type,omitempty"`

	// Compression enables snapshot compression
	// +optional
	// +kubebuilder:default=false
	Compression bool `json:"compression,omitempty"`

	// Encryption enables snapshot encryption
	// +optional
	Encryption *SnapshotEncryption `json:"encryption,omitempty"`

	// ConsistencyLevel defines the consistency level required
	// +optional
	// +kubebuilder:default="FilesystemConsistent"
	// +kubebuilder:validation:Enum=CrashConsistent;FilesystemConsistent;ApplicationConsistent
	ConsistencyLevel string `json:"consistencyLevel,omitempty"`
}

// SnapshotType represents the type of snapshot
// +kubebuilder:validation:Enum=Standard;Crash;Application
type SnapshotType string

const (
	// SnapshotTypeStandard indicates a standard snapshot
	SnapshotTypeStandard SnapshotType = "Standard"
	// SnapshotTypeCrash indicates a crash-consistent snapshot
	SnapshotTypeCrash SnapshotType = "Crash"
	// SnapshotTypeApplication indicates an application-consistent snapshot
	SnapshotTypeApplication SnapshotType = "Application"
)

// SnapshotEncryption defines snapshot encryption settings
type SnapshotEncryption struct {
	// Enabled indicates if encryption should be used
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// KeyProvider specifies the encryption key provider
	// +optional
	// +kubebuilder:validation:Enum=standard;hardware;external
	KeyProvider string `json:"keyProvider,omitempty"`

	// KeyRef references encryption keys
	// +optional
	KeyRef *LocalObjectReference `json:"keyRef,omitempty"`
}

// SnapshotRetentionPolicy defines snapshot retention rules
type SnapshotRetentionPolicy struct {
	// MaxAge is the maximum age before snapshot should be deleted
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`

	// MaxCount is the maximum number of snapshots to retain
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxCount *int32 `json:"maxCount,omitempty"`

	// DeleteOnVMDelete indicates whether to delete snapshot when VM is deleted
	// +optional
	// +kubebuilder:default=true
	DeleteOnVMDelete bool `json:"deleteOnVMDelete,omitempty"`

	// PreservePinned indicates whether to preserve pinned snapshots
	// +optional
	// +kubebuilder:default=true
	PreservePinned bool `json:"preservePinned,omitempty"`

	// GracePeriod is the grace period before deletion
	// +optional
	// +kubebuilder:default="24h"
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
}

// SnapshotSchedule defines automated snapshot scheduling
type SnapshotSchedule struct {
	// Enabled indicates if scheduled snapshots are enabled
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// CronSpec defines the schedule in cron format
	// +optional
	// +kubebuilder:validation:Pattern="^(@(annually|yearly|monthly|weekly|daily|hourly|reboot))|(@every (\\d+(ns|us|µs|ms|s|m|h))+)|((((\\d+,)+\\d+|(\\d+(\\/|-)\\d+)|\\d+|\\*) ?){5,7})$"
	CronSpec string `json:"cronSpec,omitempty"`

	// Timezone specifies the timezone for the schedule
	// +optional
	// +kubebuilder:default="UTC"
	Timezone string `json:"timezone,omitempty"`

	// Suspend indicates whether to suspend scheduled snapshots
	// +optional
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy specifies how to handle concurrent snapshot jobs
	// +optional
	// +kubebuilder:default="Forbid"
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`

	// SuccessfulJobsHistoryLimit limits retained successful jobs
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// FailedJobsHistoryLimit limits retained failed jobs
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// SnapshotMetadata contains snapshot metadata
type SnapshotMetadata struct {
	// Tags are key-value pairs for categorizing the snapshot
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`

	// Pinned indicates whether the snapshot is pinned (protected from automatic deletion)
	// +optional
	// +kubebuilder:default=false
	Pinned bool `json:"pinned,omitempty"`

	// Application specifies the application that created the snapshot
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Application string `json:"application,omitempty"`

	// Purpose describes the purpose of the snapshot
	// +optional
	// +kubebuilder:validation:Enum=backup;testing;migration;restore-point;update;other
	Purpose string `json:"purpose,omitempty"`

	// Environment specifies the environment
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test
	Environment string `json:"environment,omitempty"`
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

	// CompletionTime is when the snapshot creation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Size is the size of the snapshot
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// VirtualSize is the virtual size of the snapshot
	// +optional
	VirtualSize *resource.Quantity `json:"virtualSize,omitempty"`

	// TaskRef tracks any ongoing async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Progress shows the snapshot creation progress
	// +optional
	Progress *SnapshotProgress `json:"progress,omitempty"`

	// ProviderStatus contains provider-specific status information
	// +optional
	ProviderStatus map[string]ProviderSnapshotStatus `json:"providerStatus,omitempty"`

	// Children lists child snapshots (for snapshot trees)
	// +optional
	Children []SnapshotRef `json:"children,omitempty"`

	// Parent references the parent snapshot (for snapshot trees)
	// +optional
	Parent *SnapshotRef `json:"parent,omitempty"`

	// ExpiryTime is when the snapshot will expire (based on retention policy)
	// +optional
	ExpiryTime *metav1.Time `json:"expiryTime,omitempty"`
}

// SnapshotPhase represents the phase of a snapshot
// +kubebuilder:validation:Enum=Pending;Creating;Ready;Deleting;Failed;Expired
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
	// SnapshotPhaseExpired indicates the snapshot has expired
	SnapshotPhaseExpired SnapshotPhase = "Expired"
)

// SnapshotProgress shows the snapshot creation progress
type SnapshotProgress struct {
	// TotalBytes is the total number of bytes to snapshot
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// CompletedBytes is the number of bytes completed
	// +optional
	CompletedBytes *int64 `json:"completedBytes,omitempty"`

	// Percentage is the completion percentage (0-100)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentage *int32 `json:"percentage,omitempty"`

	// StartTime is when the snapshot creation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// ETA is the estimated time to completion
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`
}

// ProviderSnapshotStatus contains provider-specific snapshot status
type ProviderSnapshotStatus struct {
	// Available indicates if the snapshot is available on this provider
	Available bool `json:"available"`

	// ID is the provider-specific snapshot identifier
	// +optional
	ID string `json:"id,omitempty"`

	// Path is the provider-specific snapshot path
	// +optional
	Path string `json:"path,omitempty"`

	// State is the provider-specific snapshot state
	// +optional
	State string `json:"state,omitempty"`

	// LastUpdated is when the status was last updated
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message provides provider-specific status information
	// +optional
	Message string `json:"message,omitempty"`
}

// SnapshotRef references a snapshot
type SnapshotRef struct {
	// Name is the snapshot name
	Name string `json:"name"`

	// Namespace is the snapshot namespace
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// UID is the snapshot UID
	// +optional
	UID string `json:"uid,omitempty"`
}

// VMSnapshot condition types
const (
	// VMSnapshotConditionReady indicates whether the snapshot is ready
	VMSnapshotConditionReady = "Ready"
	// VMSnapshotConditionCreating indicates whether the snapshot is being created
	VMSnapshotConditionCreating = "Creating"
	// VMSnapshotConditionDeleting indicates whether the snapshot is being deleted
	VMSnapshotConditionDeleting = "Deleting"
	// VMSnapshotConditionExpired indicates whether the snapshot has expired
	VMSnapshotConditionExpired = "Expired"
)

// VMSnapshot condition reasons
const (
	// VMSnapshotReasonCreated indicates the snapshot was successfully created
	VMSnapshotReasonCreated = "Created"
	// VMSnapshotReasonCreating indicates the snapshot is being created
	VMSnapshotReasonCreating = "Creating"
	// VMSnapshotReasonDeleting indicates the snapshot is being deleted
	VMSnapshotReasonDeleting = "Deleting"
	// VMSnapshotReasonExpired indicates the snapshot has expired
	VMSnapshotReasonExpired = "Expired"
	// VMSnapshotReasonProviderError indicates a provider error occurred
	VMSnapshotReasonProviderError = "ProviderError"
	// VMSnapshotReasonUnsupported indicates snapshots are not supported
	VMSnapshotReasonUnsupported = "Unsupported"
	// VMSnapshotReasonQuiesceFailed indicates file system quiesce failed
	VMSnapshotReasonQuiesceFailed = "QuiesceFailed"
	// VMSnapshotReasonMemoryIncluded indicates memory state was included
	VMSnapshotReasonMemoryIncluded = "MemoryIncluded"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="VM",type=string,JSONPath=`.spec.vmRef.name`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.size`
//+kubebuilder:printcolumn:name="Created",type=date,JSONPath=`.status.creationTime`
//+kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiryTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmsnap

// VMSnapshot is the Schema for the vmsnapshots API
// +kubebuilder:storageversion
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

// Hub marks this version as the conversion hub
func (*VMSnapshot) Hub() {}

func init() {
	SchemeBuilder.Register(&VMSnapshot{}, &VMSnapshotList{})
}
