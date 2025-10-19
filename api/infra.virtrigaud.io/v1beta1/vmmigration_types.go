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

// VMMigrationSpec defines the desired state of VMMigration
type VMMigrationSpec struct {
	// Source defines the source VM to migrate from
	Source MigrationSource `json:"source"`

	// Target defines the target provider and configuration
	Target MigrationTarget `json:"target"`

	// Options defines migration options
	// +optional
	Options *MigrationOptions `json:"options,omitempty"`

	// Storage defines storage backend configuration for transfer
	// +optional
	Storage *MigrationStorage `json:"storage,omitempty"`

	// Metadata contains migration metadata
	// +optional
	Metadata *MigrationMetadata `json:"metadata,omitempty"`
}

// MigrationSource defines the source VM for migration
type MigrationSource struct {
	// VMRef references the source virtual machine
	VMRef LocalObjectReference `json:"vmRef"`

	// ProviderRef explicitly specifies the source provider (optional, auto-detected from VM)
	// +optional
	ProviderRef *ObjectRef `json:"providerRef,omitempty"`

	// SnapshotRef references a specific snapshot to migrate from
	// +optional
	SnapshotRef *LocalObjectReference `json:"snapshotRef,omitempty"`

	// CreateSnapshot indicates whether to create a snapshot before migration
	// +optional
	// +kubebuilder:default=true
	CreateSnapshot bool `json:"createSnapshot,omitempty"`

	// PowerOffBeforeMigration ensures VM is powered off before migration
	// +optional
	// +kubebuilder:default=false
	PowerOffBeforeMigration bool `json:"powerOffBeforeMigration,omitempty"`

	// DeleteAfterMigration deletes source VM after successful migration
	// +optional
	// +kubebuilder:default=false
	DeleteAfterMigration bool `json:"deleteAfterMigration,omitempty"`
}

// MigrationTarget defines the target provider and VM configuration
type MigrationTarget struct {
	// Name is the name for the target VM
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace for the target VM (defaults to source namespace)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// ProviderRef references the target provider
	ProviderRef ObjectRef `json:"providerRef"`

	// ClassRef references the VM class for resource allocation
	// +optional
	ClassRef *LocalObjectReference `json:"classRef,omitempty"`

	// ImageRef references the VM image (usually not needed for migration)
	// +optional
	ImageRef *LocalObjectReference `json:"imageRef,omitempty"`

	// Networks defines network configuration for target VM
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks defines disk configuration overrides
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Disks []DiskSpec `json:"disks,omitempty"`

	// PlacementRef references placement policy for the target VM
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// PowerOn indicates whether to power on the target VM after migration
	// +optional
	// +kubebuilder:default=false
	PowerOn bool `json:"powerOn,omitempty"`

	// Labels defines labels to apply to the target VM
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations defines annotations to apply to the target VM
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// MigrationOptions defines migration options
type MigrationOptions struct {
	// DiskFormat specifies the desired disk format for the target
	// +optional
	// +kubebuilder:validation:Enum=qcow2;vmdk;raw
	DiskFormat string `json:"diskFormat,omitempty"`

	// Compress enables compression during transfer
	// +optional
	// +kubebuilder:default=false
	Compress bool `json:"compress,omitempty"`

	// VerifyChecksums enables checksum verification
	// +optional
	// +kubebuilder:default=true
	VerifyChecksums bool `json:"verifyChecksums,omitempty"`

	// Timeout defines the maximum time for the entire migration
	// +optional
	// +kubebuilder:default="4h"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RetryPolicy defines retry behavior for failed operations
	// +optional
	RetryPolicy *MigrationRetryPolicy `json:"retryPolicy,omitempty"`

	// CleanupPolicy defines cleanup behavior
	// +optional
	// +kubebuilder:default="OnSuccess"
	// +kubebuilder:validation:Enum=Always;OnSuccess;Never
	CleanupPolicy string `json:"cleanupPolicy,omitempty"`

	// ValidationChecks defines validation checks to perform
	// +optional
	ValidationChecks *ValidationChecks `json:"validationChecks,omitempty"`
}

// MigrationRetryPolicy defines retry behavior for failed operations
type MigrationRetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// RetryDelay is the delay between retry attempts
	// +optional
	// +kubebuilder:default="5m"
	RetryDelay *metav1.Duration `json:"retryDelay,omitempty"`

	// BackoffMultiplier is the multiplier for exponential backoff
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	BackoffMultiplier *int32 `json:"backoffMultiplier,omitempty"`
}

// ValidationChecks defines validation checks to perform
type ValidationChecks struct {
	// CheckDiskSize verifies disk size matches
	// +optional
	// +kubebuilder:default=true
	CheckDiskSize bool `json:"checkDiskSize,omitempty"`

	// CheckChecksum verifies checksums match
	// +optional
	// +kubebuilder:default=true
	CheckChecksum bool `json:"checkChecksum,omitempty"`

	// CheckBoot verifies VM boots successfully
	// +optional
	// +kubebuilder:default=false
	CheckBoot bool `json:"checkBoot,omitempty"`

	// CheckConnectivity tests network connectivity
	// +optional
	// +kubebuilder:default=false
	CheckConnectivity bool `json:"checkConnectivity,omitempty"`
}

// MigrationStorage defines storage backend configuration
type MigrationStorage struct {
	// Type specifies the storage backend type
	// +kubebuilder:validation:Enum=pvc
	// +kubebuilder:default=pvc
	Type string `json:"type"`

	// PVC specifies PVC-based storage configuration
	// +optional
	PVC *PVCStorageConfig `json:"pvc,omitempty"`
}

// PVCStorageConfig defines PVC storage configuration
type PVCStorageConfig struct {
	// Name of an existing PVC to use for migration storage
	// If not specified, a temporary PVC will be created
	// +optional
	Name string `json:"name,omitempty"`

	// StorageClassName for auto-created PVC
	// Required if Name is not specified
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size for auto-created PVC (e.g., "100Gi")
	// Required if Name is not specified
	// +optional
	// +kubebuilder:validation:Pattern="^[0-9]+(\\.[0-9]+)?(Ei?|Pi?|Ti?|Gi?|Mi?|Ki?)$"
	Size string `json:"size,omitempty"`

	// AccessMode for auto-created PVC
	// +optional
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany;ReadOnlyMany
	// +kubebuilder:default=ReadWriteMany
	AccessMode string `json:"accessMode,omitempty"`

	// MountPath within pods where PVC is mounted
	// +optional
	// +kubebuilder:default="/mnt/migration-storage"
	MountPath string `json:"mountPath,omitempty"`
}

// MigrationMetadata contains migration metadata
type MigrationMetadata struct {
	// Purpose describes the purpose of the migration
	// +optional
	// +kubebuilder:validation:Enum=disaster-recovery;cloud-migration;provider-change;testing;maintenance
	Purpose string `json:"purpose,omitempty"`

	// CreatedBy identifies who or what created the migration
	// +optional
	// +kubebuilder:validation:MaxLength=255
	CreatedBy string `json:"createdBy,omitempty"`

	// Project identifies the project this migration belongs to
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Project string `json:"project,omitempty"`

	// Environment specifies the environment
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing the migration
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMMigrationStatus defines the observed state of VMMigration
type VMMigrationStatus struct {
	// Phase represents the current phase of the migration
	// +optional
	Phase MigrationPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// TargetVMRef references the created target VM
	// +optional
	TargetVMRef *LocalObjectReference `json:"targetVMRef,omitempty"`

	// SnapshotRef references the source snapshot used for migration
	// +optional
	SnapshotRef string `json:"snapshotRef,omitempty"`

	// SnapshotID is the provider-specific snapshot identifier
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// ExportID is the export operation identifier
	// +optional
	ExportID string `json:"exportID,omitempty"`

	// ImportID is the import operation identifier
	// +optional
	ImportID string `json:"importID,omitempty"`

	// TaskRef is the current task reference for async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// TargetVMID is the provider-specific target VM identifier
	// +optional
	TargetVMID string `json:"targetVMID,omitempty"`

	// StartTime is when the migration started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the migration completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Progress shows the migration operation progress
	// +optional
	Progress *MigrationProgress `json:"progress,omitempty"`

	// DiskInfo contains information about the migrated disk
	// +optional
	DiskInfo *MigrationDiskInfo `json:"diskInfo,omitempty"`

	// StorageInfo contains information about intermediate storage
	// +optional
	StorageInfo *MigrationStorageInfo `json:"storageInfo,omitempty"`

	// StoragePVCName is the name of the PVC used for migration storage
	// +optional
	StoragePVCName string `json:"storagePVCName,omitempty"`

	// Conditions represent the current service state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RetryCount is the number of times the migration has been retried
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// LastRetryTime is when the migration was last retried
	// +optional
	LastRetryTime *metav1.Time `json:"lastRetryTime,omitempty"`

	// ValidationResults contains results of validation checks
	// +optional
	ValidationResults *ValidationResults `json:"validationResults,omitempty"`
}

// MigrationPhase represents the phase of a migration operation
// +kubebuilder:validation:Enum=Pending;Validating;Snapshotting;Exporting;Transferring;Converting;Importing;Creating;Validating-Target;Ready;Failed
type MigrationPhase string

const (
	// MigrationPhasePending indicates the migration is waiting to be processed
	MigrationPhasePending MigrationPhase = "Pending"
	// MigrationPhaseValidating indicates the migration is being validated
	MigrationPhaseValidating MigrationPhase = "Validating"
	// MigrationPhaseSnapshotting indicates a snapshot is being created
	MigrationPhaseSnapshotting MigrationPhase = "Snapshotting"
	// MigrationPhaseExporting indicates the disk is being exported
	MigrationPhaseExporting MigrationPhase = "Exporting"
	// MigrationPhaseTransferring indicates the disk is being transferred
	MigrationPhaseTransferring MigrationPhase = "Transferring"
	// MigrationPhaseConverting indicates the disk format is being converted
	MigrationPhaseConverting MigrationPhase = "Converting"
	// MigrationPhaseImporting indicates the disk is being imported
	MigrationPhaseImporting MigrationPhase = "Importing"
	// MigrationPhaseCreating indicates the target VM is being created
	MigrationPhaseCreating MigrationPhase = "Creating"
	// MigrationPhaseValidatingTarget indicates the target VM is being validated
	MigrationPhaseValidatingTarget MigrationPhase = "Validating-Target"
	// MigrationPhaseReady indicates the migration is complete
	MigrationPhaseReady MigrationPhase = "Ready"
	// MigrationPhaseFailed indicates the migration failed
	MigrationPhaseFailed MigrationPhase = "Failed"
)

// MigrationProgress shows the migration operation progress
type MigrationProgress struct {
	// CurrentPhase is the current phase being executed
	CurrentPhase MigrationPhase `json:"currentPhase,omitempty"`

	// TotalBytes is the total bytes to transfer
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`

	// TransferredBytes is the bytes transferred so far
	// +optional
	TransferredBytes *int64 `json:"transferredBytes,omitempty"`

	// Percentage is the overall completion percentage (0-100)
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentage *int32 `json:"percentage,omitempty"`

	// ETA is the estimated time to completion
	// +optional
	ETA *metav1.Duration `json:"eta,omitempty"`

	// TransferRate is the current transfer rate in bytes per second
	// +optional
	TransferRate *int64 `json:"transferRate,omitempty"`

	// PhaseStartTime is when the current phase started
	// +optional
	PhaseStartTime *metav1.Time `json:"phaseStartTime,omitempty"`
}

// MigrationDiskInfo contains information about the migrated disk
type MigrationDiskInfo struct {
	// SourceDiskID is the source disk identifier
	SourceDiskID string `json:"sourceDiskID,omitempty"`

	// SourceFormat is the source disk format
	SourceFormat string `json:"sourceFormat,omitempty"`

	// SourceSize is the source disk size in bytes
	SourceSize *resource.Quantity `json:"sourceSize,omitempty"`

	// TargetDiskID is the target disk identifier
	// +optional
	TargetDiskID string `json:"targetDiskID,omitempty"`

	// TargetFormat is the target disk format
	// +optional
	TargetFormat string `json:"targetFormat,omitempty"`

	// TargetSize is the target disk size in bytes
	// +optional
	TargetSize *resource.Quantity `json:"targetSize,omitempty"`

	// Checksum is the SHA256 checksum of the disk
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// SourceChecksum is the SHA256 checksum of the source disk
	// +optional
	SourceChecksum string `json:"sourceChecksum,omitempty"`

	// TargetChecksum is the SHA256 checksum of the target disk
	// +optional
	TargetChecksum string `json:"targetChecksum,omitempty"`
}

// MigrationStorageInfo contains information about intermediate storage
type MigrationStorageInfo struct {
	// URL is the intermediate storage URL
	// +optional
	URL string `json:"url,omitempty"`

	// Size is the size of data in intermediate storage
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// UploadedAt is when the data was uploaded
	// +optional
	UploadedAt *metav1.Time `json:"uploadedAt,omitempty"`

	// CleanedUp indicates if intermediate storage was cleaned up
	// +optional
	CleanedUp bool `json:"cleanedUp,omitempty"`
}

// ValidationResults contains results of validation checks
type ValidationResults struct {
	// DiskSizeMatch indicates if disk sizes match
	// +optional
	DiskSizeMatch *bool `json:"diskSizeMatch,omitempty"`

	// ChecksumMatch indicates if checksums match
	// +optional
	ChecksumMatch *bool `json:"checksumMatch,omitempty"`

	// BootSuccess indicates if the target VM booted successfully
	// +optional
	BootSuccess *bool `json:"bootSuccess,omitempty"`

	// ConnectivitySuccess indicates if network connectivity works
	// +optional
	ConnectivitySuccess *bool `json:"connectivitySuccess,omitempty"`

	// ValidationErrors lists any validation errors
	// +optional
	ValidationErrors []string `json:"validationErrors,omitempty"`
}

// VMMigration condition types
const (
	// VMMigrationConditionReady indicates whether the migration is ready
	VMMigrationConditionReady = "Ready"
	// VMMigrationConditionValidating indicates whether validation is in progress
	VMMigrationConditionValidating = "Validating"
	// VMMigrationConditionSnapshotting indicates whether snapshotting is in progress
	VMMigrationConditionSnapshotting = "Snapshotting"
	// VMMigrationConditionExporting indicates whether disk export is in progress
	VMMigrationConditionExporting = "Exporting"
	// VMMigrationConditionTransferring indicates whether transfer is in progress
	VMMigrationConditionTransferring = "Transferring"
	// VMMigrationConditionImporting indicates whether disk import is in progress
	VMMigrationConditionImporting = "Importing"
	// VMMigrationConditionFailed indicates whether the migration has failed
	VMMigrationConditionFailed = "Failed"
)

// VMMigration condition reasons
const (
	// VMMigrationReasonCompleted indicates the migration was successfully completed
	VMMigrationReasonCompleted = "Completed"
	// VMMigrationReasonValidating indicates validation is in progress
	VMMigrationReasonValidating = "Validating"
	// VMMigrationReasonExporting indicates disk export is in progress
	VMMigrationReasonExporting = "Exporting"
	// VMMigrationReasonTransferring indicates disk transfer is in progress
	VMMigrationReasonTransferring = "Transferring"
	// VMMigrationReasonImporting indicates disk import is in progress
	VMMigrationReasonImporting = "Importing"
	// VMMigrationReasonSourceNotFound indicates the source VM was not found
	VMMigrationReasonSourceNotFound = "SourceNotFound"
	// VMMigrationReasonProviderError indicates a provider error occurred
	VMMigrationReasonProviderError = "ProviderError"
	// VMMigrationReasonStorageError indicates a storage error occurred
	VMMigrationReasonStorageError = "StorageError"
	// VMMigrationReasonValidationFailed indicates validation failed
	VMMigrationReasonValidationFailed = "ValidationFailed"
	// VMMigrationReasonTimeout indicates the migration timed out
	VMMigrationReasonTimeout = "Timeout"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.vmRef.name`
//+kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Progress",type=string,JSONPath=`.status.progress.percentage`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:resource:shortName=vmmig

// VMMigration is the Schema for the vmmigrations API
// +kubebuilder:storageversion
type VMMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMMigrationSpec   `json:"spec,omitempty"`
	Status VMMigrationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VMMigrationList contains a list of VMMigration
type VMMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMMigration{}, &VMMigrationList{})
}
