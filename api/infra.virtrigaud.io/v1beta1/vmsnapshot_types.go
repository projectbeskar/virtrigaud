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

// VMSnapshotFinalizer is the finalizer the VMSnapshot controller keeps on a
// VMSnapshot until its provider snapshot is deleted (or provably gone), or
// the VMSnapshot is released with virtrigaud.io/force-delete=true.
const VMSnapshotFinalizer = "snapshot.infra.virtrigaud.io/finalizer"

// VMSnapshotSpec defines the desired state of VMSnapshot. A VMSnapshot takes
// one snapshot of one VirtualMachine, through the VM's Provider. Of the spec,
// vmRef, snapshotConfig (name, description, includeMemory) and retentionPolicy
// (maxAge, deleteOnVMDelete) are acted on; schedule and metadata are accepted
// but not acted on in this release.
type VMSnapshotSpec struct {
	// VMRef references the VirtualMachine to snapshot, by name, in the
	// VMSnapshot's own namespace.
	VMRef LocalObjectReference `json:"vmRef"`

	// SnapshotConfig defines snapshot configuration options. When it is
	// omitted, the snapshot is named after the VMSnapshot and has no description
	// and no memory state.
	// +optional
	SnapshotConfig *SnapshotConfig `json:"snapshotConfig,omitempty"`

	// RetentionPolicy defines when the VMSnapshot, and with it the provider
	// snapshot, is deleted automatically. Only maxAge and deleteOnVMDelete are
	// acted on; the other fields are accepted but not acted on in this release.
	// With no retentionPolicy a snapshot is kept until the VMSnapshot is deleted.
	// +optional
	RetentionPolicy *SnapshotRetentionPolicy `json:"retentionPolicy,omitempty"`

	// Schedule defines automated snapshot scheduling. Accepted but not acted on
	// in this release: no controller reads it, and no snapshot is taken on a
	// schedule. Create VMSnapshot objects yourself (for example from a CronJob).
	// +optional
	Schedule *SnapshotSchedule `json:"schedule,omitempty"`

	// Metadata contains snapshot metadata. Accepted but not acted on in this
	// release: no controller or provider reads it, and it is not sent to the
	// provider.
	// +optional
	Metadata *SnapshotMetadata `json:"metadata,omitempty"`
}

// SnapshotConfig defines snapshot configuration options. Name, description and
// includeMemory are sent to the provider. The other fields (quiesce, type,
// compression, encryption, consistencyLevel) are accepted but not acted on in
// this release.
type SnapshotConfig struct {
	// Name provides a name hint for the snapshot. It defaults to the
	// VMSnapshot's name. The provider may change it: the libvirt provider
	// replaces characters other than letters, digits, "_", "." and "-" with "-"
	// and truncates the name to 64 characters; the vSphere and Proxmox providers
	// use it as given. The provider's own identifier for the snapshot is in
	// status.snapshotID.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9]([a-zA-Z0-9\\-_]*[a-zA-Z0-9])?$"
	Name string `json:"name,omitempty"`

	// Description is a free-text description stored with the snapshot by the
	// vSphere, libvirt and Proxmox providers.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// IncludeMemory indicates whether to include the VM's memory state in the
	// snapshot. vSphere captures it only for a powered-on VM; libvirt takes a
	// memory snapshot of a running domain and falls back to a disk-only snapshot
	// when the domain is not running; Proxmox saves the VM state. When the manager
	// runs with --enforce-provider-capabilities and the Provider does not report
	// memory snapshots, the VMSnapshot goes Failed with reason
	// UnsupportedByProvider and no provider call is made.
	// +optional
	// +kubebuilder:default=false
	IncludeMemory bool `json:"includeMemory,omitempty"`

	// Quiesce indicates whether to quiesce the file system before snapshotting.
	// Accepted but not acted on in this release: the setting is not sent to any
	// provider. The vSphere provider never quiesces; the libvirt and Proxmox
	// providers take the snapshot without a guest quiesce step.
	// +optional
	// +kubebuilder:default=true
	Quiesce bool `json:"quiesce,omitempty"`

	// Type specifies the snapshot type. Accepted but not acted on in this
	// release.
	// +optional
	// +kubebuilder:default="Standard"
	// +kubebuilder:validation:Enum=Standard;Crash;Application
	Type SnapshotType `json:"type,omitempty"`

	// Compression enables snapshot compression. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:default=false
	Compression bool `json:"compression,omitempty"`

	// Encryption enables snapshot encryption. Accepted but not acted on in this
	// release: no snapshot is encrypted, whatever this block says.
	// +optional
	Encryption *SnapshotEncryption `json:"encryption,omitempty"`

	// ConsistencyLevel defines the consistency level required. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default="FilesystemConsistent"
	// +kubebuilder:validation:Enum=CrashConsistent;FilesystemConsistent;ApplicationConsistent
	ConsistencyLevel string `json:"consistencyLevel,omitempty"`
}

// SnapshotType represents the type of snapshot. Accepted but not acted on in
// this release (see SnapshotConfig.Type).
// +kubebuilder:validation:Enum=Standard;Crash;Application
type SnapshotType string

const (
	// SnapshotTypeStandard is the default snapshot type
	SnapshotTypeStandard SnapshotType = "Standard"
	// SnapshotTypeCrash names a crash-consistent snapshot
	SnapshotTypeCrash SnapshotType = "Crash"
	// SnapshotTypeApplication names an application-consistent snapshot
	SnapshotTypeApplication SnapshotType = "Application"
)

// SnapshotEncryption defines snapshot encryption settings. Accepted but not
// acted on in this release.
type SnapshotEncryption struct {
	// Enabled indicates if encryption should be used. Accepted but not acted on
	// in this release.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// KeyProvider specifies the encryption key provider. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:validation:Enum=standard;hardware;external
	KeyProvider string `json:"keyProvider,omitempty"`

	// KeyRef references encryption keys. Accepted but not acted on in this
	// release: the referenced object is never read.
	// +optional
	KeyRef *LocalObjectReference `json:"keyRef,omitempty"`
}

// SnapshotRetentionPolicy defines snapshot retention rules. The controller acts
// on maxAge and deleteOnVMDelete only, and only once the snapshot is Ready; it
// checks them about once an hour. When a rule applies it deletes the
// VMSnapshot, and the VMSnapshot's finalizer deletes the provider snapshot.
// maxCount, preservePinned and gracePeriod are accepted but not acted on in
// this release.
type SnapshotRetentionPolicy struct {
	// MaxAge is the maximum age of a Ready snapshot, counted from
	// status.creationTime. Once the snapshot is older (checked about once an
	// hour), the VMSnapshot is deleted, whatever the state of its VM.
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`

	// MaxCount is the maximum number of snapshots to retain. Accepted but not
	// acted on in this release: no snapshots are removed because of a count.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxCount *int32 `json:"maxCount,omitempty"`

	// DeleteOnVMDelete indicates whether to delete the snapshot when its
	// VirtualMachine is deleted (or no longer exists, or exists only as another VM
	// of the same name). It defaults to true when retentionPolicy is set. A VM
	// deleted with the annotation virtrigaud.io/orphan-on-delete=true is left
	// running on the hypervisor and its snapshots are not deleted with it.
	// +optional
	// +kubebuilder:default=true
	DeleteOnVMDelete bool `json:"deleteOnVMDelete,omitempty"`

	// PreservePinned indicates whether to preserve pinned snapshots. Accepted
	// but not acted on in this release (see also metadata.pinned).
	// +optional
	// +kubebuilder:default=true
	PreservePinned bool `json:"preservePinned,omitempty"`

	// GracePeriod is the grace period before deletion. Accepted but not acted on
	// in this release: a snapshot is deleted as soon as maxAge or deleteOnVMDelete
	// applies.
	// +optional
	// +kubebuilder:default="24h"
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
}

// SnapshotSchedule defines automated snapshot scheduling. Accepted but not
// acted on in this release: no controller reads any field of it and no snapshot
// is taken on a schedule.
type SnapshotSchedule struct {
	// Enabled indicates if scheduled snapshots are enabled. Accepted but not
	// acted on in this release.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// CronSpec defines the schedule in cron format. Accepted but not acted on in
	// this release.
	// +optional
	// +kubebuilder:validation:Pattern="^(@(annually|yearly|monthly|weekly|daily|hourly|reboot))|(@every (\\d+(ns|us|µs|ms|s|m|h))+)|((((\\d+,)+\\d+|(\\d+(\\/|-)\\d+)|\\d+|\\*) ?){5,7})$"
	CronSpec string `json:"cronSpec,omitempty"`

	// Timezone specifies the timezone for the schedule. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:default="UTC"
	Timezone string `json:"timezone,omitempty"`

	// Suspend indicates whether to suspend scheduled snapshots. Accepted but not
	// acted on in this release.
	// +optional
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy specifies how to handle concurrent snapshot jobs.
	// Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default="Forbid"
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`

	// SuccessfulJobsHistoryLimit limits retained successful jobs. Accepted but
	// not acted on in this release.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// FailedJobsHistoryLimit limits retained failed jobs. Accepted but not acted
	// on in this release.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// SnapshotMetadata contains snapshot metadata. Accepted but not acted on in
// this release: no controller or provider reads any field of it. It is
// descriptive only.
type SnapshotMetadata struct {
	// Tags are key-value pairs for categorizing the snapshot. Descriptive: no
	// controller reads them.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`

	// Pinned indicates whether the snapshot is pinned (protected from automatic
	// deletion). Accepted but not acted on in this release: a pinned snapshot is
	// still deleted by retentionPolicy.maxAge or deleteOnVMDelete.
	// +optional
	// +kubebuilder:default=false
	Pinned bool `json:"pinned,omitempty"`

	// Application specifies the application that created the snapshot.
	// Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Application string `json:"application,omitempty"`

	// Purpose describes the purpose of the snapshot. Descriptive: no controller
	// reads it.
	// +optional
	// +kubebuilder:validation:Enum=backup;testing;migration;restore-point;update;other
	Purpose string `json:"purpose,omitempty"`

	// Environment specifies the environment the snapshot belongs to.
	// Descriptive: no controller reads it.
	// +optional
	// +kubebuilder:validation:Enum=dev;staging;prod;test;qa;uat
	Environment string `json:"environment,omitempty"`
}

// VMSnapshotStatus defines the observed state of VMSnapshot. The controller
// writes snapshotID, phase, message, creationTime, taskRef, vmUID,
// boundProvider and conditions. completionTime, size, virtualSize, progress,
// providerStatus, children, parent and expiryTime are not populated in this
// release.
type VMSnapshotStatus struct {
	// SnapshotID is the provider-specific identifier for the snapshot, as
	// returned by the provider when the snapshot is created. On libvirt and
	// Proxmox it is the snapshot's name; on vSphere it is the snapshot's
	// managed object ID.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// Phase represents the current phase of the snapshot. It is empty until the
	// controller issues the create, then Creating, then Ready or Failed; it is
	// Deleting while the VMSnapshot is being deleted.
	// +optional
	Phase SnapshotPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// CreationTime is when the controller received the provider's answer to the
	// create. It is the start of the age counted by retentionPolicy.maxAge.
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// CompletionTime is when the snapshot creation completed. Not populated in
	// this release.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Size is the size of the snapshot. Not populated in this release.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// VirtualSize is the virtual size of the snapshot. Not populated in this
	// release.
	// +optional
	VirtualSize *resource.Quantity `json:"virtualSize,omitempty"`

	// TaskRef is the provider task of an in-flight asynchronous snapshot create.
	// The controller polls it until the task completes, then clears it.
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// VMUID is the UID of the VirtualMachine the snapshot was taken of,
	// recorded when its create is issued. A VirtualMachine deleted and
	// re-created under the same name has another UID: the snapshot is never
	// deleted or retained through it (no provider call is made), because on a
	// provider whose snapshot ids are names the call could reach another
	// VM's snapshot. Empty for a snapshot taken by an earlier release, which is
	// bound on first reconcile when its VM was created before the snapshot.
	// Written only by the operator.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	VMUID string `json:"vmUID,omitempty"`

	// BoundProvider is the Provider the snapshot was taken through, recorded
	// with VMUID. The snapshot is deleted only through this same Provider
	// (namespace, name and, when recorded, UID). Written only by the
	// operator.
	// +optional
	BoundProvider *BoundProviderRef `json:"boundProvider,omitempty"`

	// Conditions represent the latest observations of the snapshot. Ready is
	// True once the snapshot exists; Creating and Deleting are True while a
	// create or a delete is in progress.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation the controller recorded when it
	// refused the snapshot because its VM uses a Provider in another namespace
	// that does not allow it; it is not populated otherwise. Each condition
	// carries its own observedGeneration.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Progress shows the snapshot creation progress. Not populated in this
	// release.
	// +optional
	Progress *SnapshotProgress `json:"progress,omitempty"`

	// ProviderStatus contains provider-specific status information. Not
	// populated in this release.
	// +optional
	ProviderStatus map[string]ProviderSnapshotStatus `json:"providerStatus,omitempty"`

	// Children lists child snapshots (for snapshot trees). Not populated in this
	// release.
	// +optional
	Children []SnapshotRef `json:"children,omitempty"`

	// Parent references the parent snapshot (for snapshot trees). Not populated
	// in this release.
	// +optional
	Parent *SnapshotRef `json:"parent,omitempty"`

	// ExpiryTime is when the snapshot will expire. Not populated in this
	// release, so the Expires column of kubectl get is empty; a snapshot is
	// deleted when retentionPolicy.maxAge has passed since status.creationTime.
	// +optional
	ExpiryTime *metav1.Time `json:"expiryTime,omitempty"`
}

// SnapshotPhase represents the phase of a snapshot. The controller sets
// Creating, Ready, Failed and Deleting; Pending and Expired are never set in
// this release.
// +kubebuilder:validation:Enum=Pending;Creating;Ready;Deleting;Failed;Expired
type SnapshotPhase string

const (
	// SnapshotPhasePending is never set in this release: a VMSnapshot whose create
	// has not been issued yet has an empty phase
	SnapshotPhasePending SnapshotPhase = "Pending"
	// SnapshotPhaseCreating indicates the snapshot is being created
	SnapshotPhaseCreating SnapshotPhase = "Creating"
	// SnapshotPhaseReady indicates the snapshot is ready for use
	SnapshotPhaseReady SnapshotPhase = "Ready"
	// SnapshotPhaseDeleting indicates the snapshot is being deleted
	SnapshotPhaseDeleting SnapshotPhase = "Deleting"
	// SnapshotPhaseFailed indicates the snapshot operation failed
	SnapshotPhaseFailed SnapshotPhase = "Failed"
	// SnapshotPhaseExpired is never set in this release: an expired snapshot is
	// deleted, not marked
	SnapshotPhaseExpired SnapshotPhase = "Expired"
)

// SnapshotProgress shows the snapshot creation progress. Not populated in this
// release.
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

// ProviderSnapshotStatus contains provider-specific snapshot status. Not
// populated in this release.
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

// SnapshotRef references a snapshot. It is used by status.parent and
// status.children, which are not populated in this release.
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
	// VMSnapshotConditionExpired is never set in this release: an expired
	// snapshot is deleted, not marked
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
	// VMSnapshotReasonExpired is never used in this release
	VMSnapshotReasonExpired = "Expired"
	// VMSnapshotReasonProviderError indicates a provider error occurred
	VMSnapshotReasonProviderError = "ProviderError"
	// VMSnapshotReasonUnsupported is never used in this release (a Provider that
	// cannot snapshot is reported with reason UnsupportedByProvider)
	VMSnapshotReasonUnsupported = "Unsupported"
	// VMSnapshotReasonQuiesceFailed is never used in this release
	VMSnapshotReasonQuiesceFailed = "QuiesceFailed"
	// VMSnapshotReasonMemoryIncluded is never used in this release
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

func init() {
	SchemeBuilder.Register(&VMSnapshot{}, &VMSnapshotList{})
}
