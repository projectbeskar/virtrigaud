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

// VMMigrationFinalizer is the finalizer the VMMigration controller keeps on a
// VMMigration until the source snapshot it created and its intermediate
// storage are cleaned up (or it is released with
// virtrigaud.io/force-delete=true).
const VMMigrationFinalizer = "vmmigration.infra.virtrigaud.io/finalizer"

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

	// Metadata contains migration metadata. Descriptive only: no controller reads
	// it.
	// +optional
	Metadata *MigrationMetadata `json:"metadata,omitempty"`
}

// MigrationSource defines the source VM for migration
type MigrationSource struct {
	// VMRef references the source virtual machine
	VMRef LocalObjectReference `json:"vmRef"`

	// ProviderRef optionally names the source provider. It must be the Provider
	// the source VM runs on (the VM's spec.providerRef, with an empty namespace
	// meaning the VM's namespace); a migration naming any other Provider fails,
	// because that Provider would export an unrelated VM that happens to share
	// the source VM's provider ID. Leave it empty to use the source VM's
	// Provider.
	// +optional
	ProviderRef *ObjectRef `json:"providerRef,omitempty"`

	// SnapshotRef names an existing snapshot to export from instead of taking a
	// new one. The name is used as the provider's snapshot identifier (no
	// VMSnapshot object is looked up), and the migration never deletes it. It
	// is read only while createSnapshot is true; with createSnapshot false no
	// snapshot is used and this field is ignored.
	// +optional
	SnapshotRef *LocalObjectReference `json:"snapshotRef,omitempty"`

	// No `omitempty` on the Go field below: a defaulted bool with omitempty would
	// have an explicit false dropped on write and re-defaulted to true.

	// CreateSnapshot indicates whether to snapshot the source VM before
	// exporting its disk (default true). A snapshot the migration took is deleted
	// after the migration succeeds, after it fails for good, and when the
	// VMMigration is deleted, unless options.cleanupPolicy is Never. When false
	// the Snapshotting phase is skipped and the disk is exported without a
	// snapshot.
	// +optional
	// +kubebuilder:default=true
	CreateSnapshot bool `json:"createSnapshot"`

	// PowerOffBeforeMigration sets the source VirtualMachine's spec.powerState to
	// Off and waits, up to 10 minutes, until the provider reports it Off before
	// the snapshot and export; the migration fails if it is not. A vSphere
	// source should be powered off, since exporting a running vSphere VM fails
	// (its disk is locked). The migration does not power the source on again;
	// its spec.powerState stays Off.
	// +optional
	// +kubebuilder:default=false
	PowerOffBeforeMigration bool `json:"powerOffBeforeMigration,omitempty"`

	// DeleteAfterMigration deletes the source VirtualMachine after the migration
	// reaches Ready. Only the VirtualMachine the migration validated (recorded in
	// status.sourceVMUID) is deleted, with a UID precondition: a VM re-created
	// under the same name is kept.
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

	// Namespace is the namespace for the target VM (defaults to the
	// VMMigration's namespace, which is also the source VM's). A different
	// namespace is allowed only when that Namespace object carries the
	// annotation infra.virtrigaud.io/allowed-source-namespaces and its
	// comma-separated value lists the VMMigration's namespace (exact names, no
	// wildcards). Whoever can update the target Namespace can grant this
	// (normally a cluster administrator, since Namespaces are cluster-scoped).
	// A granted migration's disk is not attached in place: the target VM
	// treats it as a base image. Without the grant the migration is held
	// before any side effect (Ready=False, reason TargetNamespaceNotAllowed);
	// the grant is re-checked before the disk import, the target VM creation
	// and the cleanup, so revoking it stops a migration in flight. Granting the
	// annotation or changing this field resumes it.
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// ProviderRef references the target provider
	ProviderRef ObjectRef `json:"providerRef"`

	// ClassRef references the VMClass (in the VMMigration's namespace) of the
	// target VirtualMachine. It becomes the target VM's spec.classRef. The
	// target VirtualMachine requires a class, so set it.
	// +optional
	ClassRef *LocalObjectReference `json:"classRef,omitempty"`

	// ImageRef references the VM image. Accepted but not acted on in this
	// release: the target VM is created from the migrated disk, not from an
	// image.
	// +optional
	ImageRef *LocalObjectReference `json:"imageRef,omitempty"`

	// Networks defines network configuration for target VM. Copied to the target
	// VirtualMachine's spec.networks.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks defines disk configuration overrides. Accepted but not acted on in
	// this release: the target VirtualMachine gets only the migrated disk.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Disks []DiskSpec `json:"disks,omitempty"`

	// PlacementRef references the VMPlacementPolicy for the target VM. Copied to
	// the target VirtualMachine's spec.placementRef.
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// PowerOn indicates whether the target VM's desired power state is On (true)
	// or Off (false, the default) once it is created.
	// +optional
	// +kubebuilder:default=false
	PowerOn bool `json:"powerOn,omitempty"`

	// Labels defines labels to apply to the target VM. The label
	// virtrigaud.io/adopted is reserved for the manager and is not copied (a
	// Normal event, TargetLabelsDropped, names it); every other label is.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations defines annotations to apply to the target VM. Keys in the
	// operator's reserved virtrigaud.io domain (virtrigaud.io/... and
	// *.virtrigaud.io/...), such as virtrigaud.io/orphan-on-delete or the
	// provenance annotations, are not copied; the controller sets its own
	// provenance annotations.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Annotations map[string]string `json:"annotations,omitempty"`
}

// MigrationOptions defines migration options
type MigrationOptions struct {
	// DiskFormat specifies the disk format requested for the export and, with the
	// pvc storage backend, for the import (default qcow2). With the s3 and nfs
	// backends the format of the staged object is not taken from this field: s3
	// stages the source provider's native format (vmdk from vSphere, qcow2
	// otherwise) and nfs stages qcow2; the target provider converts on import.
	// +optional
	// +kubebuilder:validation:Enum=qcow2;vmdk;raw
	DiskFormat string `json:"diskFormat,omitempty"`

	// Compress asks the source provider to compress the exported disk. It is
	// sent with the export request and a provider may ignore it: it has no
	// effect on the libvirt s3 and nfs exports, on vSphere exports or on the
	// Proxmox nfs export; the Proxmox s3 export honours it.
	// +optional
	// +kubebuilder:default=false
	Compress bool `json:"compress,omitempty"`

	// VerifyChecksums asks the target provider to verify a pvc import against
	// the checksum the source export reported (default true). Setting it to
	// false only disables that pvc check: s3 imports always verify when the
	// export reported a checksum, and nfs carries no checksum.
	// +optional
	// +kubebuilder:default=true
	VerifyChecksums bool `json:"verifyChecksums,omitempty"`

	// Timeout bounds how long the migration waits for the source snapshot task
	// to finish (default 4h; 2h when options is unset). It is not applied to the
	// other phases in this release: a disk export call has a fixed limit of one
	// hour.
	// +optional
	// +kubebuilder:default="4h"
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RetryPolicy defines retry behavior for a failed migration. Without it a
	// failed migration is not retried. A retry restarts the migration from the
	// Pending phase.
	// +optional
	RetryPolicy *MigrationRetryPolicy `json:"retryPolicy,omitempty"`

	// CleanupPolicy decides when the artifacts the migration created are removed.
	// The source snapshot the migration took is deleted when the migration
	// reaches Ready, and when it fails for good, unless the policy is Never. The
	// staged disk of an nfs or s3 migration is deleted, with OnSuccess (the
	// default), when the migration reaches Ready and when the VMMigration is
	// deleted; a migration that failed for good keeps it until the VMMigration
	// is deleted. Always also deletes it when the migration fails for good. The
	// disk a failed import landed on the target is deleted when the VMMigration
	// is deleted (OnSuccess) or also when the migration fails for good (Always);
	// the disk of a migration that reached Ready belongs to the target VM and is
	// never deleted. Never keeps the snapshot, the staged disk and the landed
	// disk. The PVC a pvc migration created is deleted with the VMMigration
	// whatever the policy. A migration fails for good when it has no
	// retryPolicy, or its retries are used up.
	// +optional
	// +kubebuilder:default="OnSuccess"
	// +kubebuilder:validation:Enum=Always;OnSuccess;Never
	CleanupPolicy string `json:"cleanupPolicy,omitempty"`

	// ValidationChecks defines validation checks to perform. Accepted but not
	// acted on in this release: no check listed here runs.
	// +optional
	ValidationChecks *ValidationChecks `json:"validationChecks,omitempty"`
}

// MigrationRetryPolicy defines retry behavior for a failed migration.
type MigrationRetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts (default 3).
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// RetryDelay is the delay before the first retry (default 5m). Each later
	// retry waits retryDelay multiplied by backoffMultiplier once more, up to a
	// maximum of 30 minutes.
	// +optional
	// +kubebuilder:default="5m"
	RetryDelay *metav1.Duration `json:"retryDelay,omitempty"`

	// BackoffMultiplier is the multiplier applied to the delay between retries
	// (default 2).
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	BackoffMultiplier *int32 `json:"backoffMultiplier,omitempty"`
}

// ValidationChecks defines validation checks to perform. Accepted but not acted
// on in this release.
type ValidationChecks struct {
	// CheckDiskSize verifies disk size matches. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=true
	CheckDiskSize bool `json:"checkDiskSize,omitempty"`

	// CheckChecksum verifies checksums match. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=true
	CheckChecksum bool `json:"checkChecksum,omitempty"`

	// CheckBoot verifies VM boots successfully. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	CheckBoot bool `json:"checkBoot,omitempty"`

	// CheckConnectivity tests network connectivity. Accepted but not acted on in this release.
	// +optional
	// +kubebuilder:default=false
	CheckConnectivity bool `json:"checkConnectivity,omitempty"`
}

// MigrationStorage defines storage backend configuration
type MigrationStorage struct {
	// Type specifies the storage backend used to stage the transferred disk
	// between the source and target providers: "pvc" (a PersistentVolumeClaim
	// the provider pods mount), "nfs" (an NFS export) or "s3" (an S3-compatible
	// bucket). Both the source provider's status.reportedCapabilities
	// supportedExportBackends and the target provider's supportedImportBackends
	// must list the backend, or the migration fails at the Validating phase.
	// The pvc backend is not supported when the source or the target Provider is
	// clustered (topology: cluster); use nfs or s3 there. It also requires the
	// VMMigration and both Providers to be in one namespace, because the PVC is
	// mounted into both provider pods.
	// +kubebuilder:validation:Enum=pvc;nfs;s3
	// +kubebuilder:default=pvc
	Type string `json:"type"`

	// TransferMode selects how disk bytes travel to and from a pvc or s3
	// staging backend. "relay" routes the bytes through the provider pod; "auto"
	// (the default) resolves to "relay". "direct" (host to backend, without the
	// provider pod) is not implemented: a migration that asks for it fails at
	// the Validating phase. The nfs backend ignores this field: the data moves
	// over NFS with qemu-img on the hypervisor host (libvirt, Proxmox) or in the
	// provider pod (vSphere).
	// +optional
	// +kubebuilder:validation:Enum=auto;relay;direct
	// +kubebuilder:default=auto
	TransferMode string `json:"transferMode,omitempty"`

	// PVC specifies PVC-based storage configuration
	// +optional
	PVC *PVCStorageConfig `json:"pvc,omitempty"`

	// NFS specifies NFS-based staging configuration. Required when type is nfs.
	// +optional
	NFS *NFSStorageConfig `json:"nfs,omitempty"`

	// S3 specifies S3-compatible object-storage staging configuration. Required
	// when type is s3.
	// +optional
	S3 *S3StorageConfig `json:"s3,omitempty"`
}

// S3StorageConfig defines S3-compatible object-storage configuration used to
// stage a migration's transferred disk.
type S3StorageConfig struct {
	// Bucket is the S3 bucket for the transfer object.
	Bucket string `json:"bucket"`
	// Endpoint overrides the S3 endpoint (empty = AWS default; set for MinIO/Ceph RGW).
	// The host is checked by the controller before it is used: loopback,
	// link-local (including the 169.254.169.254 cloud-metadata address),
	// unspecified and multicast addresses are refused, and the operator can
	// restrict the allowed hosts further.
	// +optional
	// +kubebuilder:validation:MaxLength=263
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 region for the bucket (empty = provider/SDK default).
	// +optional
	Region string `json:"region,omitempty"`
	// Prefix is the key prefix for this migration's objects.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// CredentialsSecretRef references a Secret, in the VMMigration's namespace,
	// holding the S3 credentials under the keys accessKeyID and secretAccessKey
	// and, optionally, sessionToken. Credentials are never given inline.
	CredentialsSecretRef ObjectRef `json:"credentialsSecretRef"`
	// UsePathStyle selects path-style addressing (needed by most non-AWS S3).
	// +optional
	// +kubebuilder:default=false
	UsePathStyle bool `json:"usePathStyle"`
}

// NFSStorageConfig defines NFS-based staging configuration used to stage a
// migration's transferred disk.
type NFSStorageConfig struct {
	// Server is the NFS server hostname or IP exporting the share. The host is
	// checked by the controller before it is used, as for the S3 endpoint.
	// +kubebuilder:validation:MaxLength=253
	Server string `json:"server"`
	// Export is the exported NFS path on the server (e.g. "/exports/migrations").
	// +kubebuilder:validation:MaxLength=4096
	Export string `json:"export"`
	// Path is an optional sub-path within the export for this migration's data.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Path string `json:"path,omitempty"`
	// ReadOnly mounts the export read-only. Accepted but not acted on in this
	// release: it is not sent to the providers, and staging needs a writable
	// export.
	// +optional
	// +kubebuilder:default=false
	ReadOnly bool `json:"readOnly"`
	// UID is the AUTH_SYS user ID presented to the NFS server. The export's
	// squash policy is the real authorization boundary; this lets an operator
	// align the client identity with the export. 0 (root) is refused. Unset =
	// the data-plane process identity (the provider pod or SSH user).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	UID *int64 `json:"uid,omitempty"`
	// GID is the AUTH_SYS group ID presented to the NFS server. 0 (root) is
	// refused. Unset = the data-plane process identity.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	GID *int64 `json:"gid,omitempty"`
}

// PVCStorageConfig defines PVC storage configuration
type PVCStorageConfig struct {
	// Name of an existing PVC, in the VMMigration's namespace, to use for
	// migration storage. The PVC must carry the label
	// virtrigaud.io/component=migration-storage, which is how its owner agrees
	// to its use; the controller refuses an unlabelled PVC. If not specified,
	// the controller creates a labelled PVC for the migration and deletes it with
	// the VMMigration (a PVC named here is left for its owner).
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

	// MountPath within pods where PVC is mounted. Accepted but not acted on in
	// this release: provider pods always mount the PVC at
	// /mnt/migration-storage/<pvc-name>.
	// +optional
	// +kubebuilder:default="/mnt/migration-storage"
	MountPath string `json:"mountPath,omitempty"`
}

// MigrationMetadata contains migration metadata. Descriptive only: no controller
// reads it.
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
	// +kubebuilder:validation:Enum=dev;staging;prod;test;qa;uat
	Environment string `json:"environment,omitempty"`

	// Tags are key-value pairs for categorizing the migration
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	Tags map[string]string `json:"tags,omitempty"`
}

// VMMigrationStatus defines the observed state of VMMigration
type VMMigrationStatus struct {
	// Phase represents the current phase of the migration. A migration goes
	// Pending, Validating, Snapshotting (when createSnapshot is true), Exporting,
	// Importing, Creating, Validating-Target and Ready, or Failed. The
	// Transferring and Converting phases exist in the enum but a migration does
	// not enter them in this release: the disk is moved during Exporting and
	// Importing, and the target provider converts the format on import.
	// +optional
	Phase MigrationPhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// TargetVMRef references the created target VM. Not populated in this
	// release; see targetVMUID and spec.target.name.
	// +optional
	TargetVMRef *LocalObjectReference `json:"targetVMRef,omitempty"`

	// SnapshotRef references the source snapshot used for migration
	// +optional
	SnapshotRef string `json:"snapshotRef,omitempty"`

	// SnapshotID is the provider-specific snapshot identifier
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// SourceVMUID is the UID of the source VirtualMachine the migration
	// validated and exported, recorded at Validating. With
	// spec.source.deleteAfterMigration the controller deletes only that
	// object (a UID precondition): a source VirtualMachine re-created under the
	// same name is never deleted. Written by the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	SourceVMUID string `json:"sourceVMUID,omitempty"`

	// ExportID is the export operation identifier the source provider returned.
	// +optional
	ExportID string `json:"exportID,omitempty"`

	// ImportID is the disk identifier the target provider returned for the
	// imported disk.
	// +optional
	ImportID string `json:"importID,omitempty"`

	// TaskRef is the current task reference for async operations
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// TargetVMID is the provider-specific target VM identifier
	// +optional
	TargetVMID string `json:"targetVMID,omitempty"`

	// TargetVMUID is the UID of the target VirtualMachine this migration
	// created, recorded from the response of its own Create (or, when that
	// record was lost, from the VirtualMachine that carries this migration's
	// claim, see targetVMClaim). Every later step — waiting for the target,
	// validating it, marking it migration-completed, deleting a partial one —
	// acts only on the VirtualMachine with this UID: another VirtualMachine
	// under the target's name (one created by someone else, before or after
	// the migration's own) fails the migration and is never touched. Written
	// by the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	TargetVMUID string `json:"targetVMUID,omitempty"`

	// TargetVMClaim is the hex SHA-256 digest of the one-time random token
	// the controller writes, as the virtrigaud.io/migration-claim annotation,
	// on the target VirtualMachine it creates. It is recorded BEFORE that
	// Create, so a Create that succeeded but whose targetVMUID was never
	// recorded (a lost status write, a manager restart) is still recognized
	// as this migration's: a VirtualMachine whose claim token hashes to this
	// digest. Only the digest is stored here; the token cannot be derived
	// from it. Written by the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	TargetVMClaim string `json:"targetVMClaim,omitempty"`

	// StartTime is when the migration started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the migration completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Progress shows the migration operation progress. Not populated in this
	// release.
	// +optional
	Progress *MigrationProgress `json:"progress,omitempty"`

	// DiskInfo contains information about the migrated disk, recorded when the
	// ExportDisk and ImportDisk calls return (which can be before an
	// asynchronous task finishes).
	// +optional
	DiskInfo *MigrationDiskInfo `json:"diskInfo,omitempty"`

	// StorageInfo contains information about intermediate storage. The controller
	// records cleanedUp, stagingLayout, stagingDigest and importTarget; url, size
	// and uploadedAt are not populated in this release.
	// +optional
	StorageInfo *MigrationStorageInfo `json:"storageInfo,omitempty"`

	// StoragePVCName is the name of the PVC used for migration storage
	// +optional
	StoragePVCName string `json:"storagePVCName,omitempty"`

	// StoragePVCUID is the UID of the migration PVC this migration created
	// (pvc backend, no spec.storage.pvc.name), taken from the API server's
	// answer to its create. Only the PVC with this UID is mounted into the
	// provider pods and deleted with the migration: a PVC of the same name
	// that anyone else created — even one carrying an ownerReference to this
	// migration — is refused. Empty for a PVC named in spec.storage.pvc.name.
	// Written by the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	StoragePVCUID string `json:"storagePVCUID,omitempty"`

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

	// ValidationResults contains results of validation checks. Not populated in
	// this release.
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

// Cleanup policy values for MigrationOptions.CleanupPolicy. These mirror the
// +kubebuilder:validation:Enum on that field; they exist as Go constants so the
// controller never compares against bare string literals.
const (
	// CleanupPolicyAlways removes the migration's source snapshot and staged
	// disk, and the disk a failed import landed on the target, also when the
	// migration fails for good. The disk of a migration that reached Ready
	// belongs to the target VM and is never removed.
	CleanupPolicyAlways = "Always"
	// CleanupPolicyOnSuccess removes the source snapshot and the staged disk when
	// the migration completes, and the snapshot also when it fails for good; a
	// migration that fails for good keeps its staged and landed disks until the
	// VMMigration is deleted. This is the default.
	CleanupPolicyOnSuccess = "OnSuccess"
	// CleanupPolicyNever keeps the source snapshot, the staged disk and the
	// landed disk; the user opts out of their cleanup. A PVC the migration
	// created is still deleted with the VMMigration.
	CleanupPolicyNever = "Never"
)

// MigrationProgress shows the migration operation progress. Not populated in
// this release.
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

	// TargetPath is the provider-native path of the imported disk as returned by
	// the target provider's ImportDisk (e.g. "[datastore1] disk-id/disk-id.vmdk"
	// for vSphere, "/var/lib/libvirt/images/disk-id.qcow2" for libvirt). It is
	// the authoritative location the created target VirtualMachine attaches; the
	// controller copies it into the target VirtualMachine's
	// spec.importedDisk.path. Empty until the import completes.
	// +optional
	TargetPath string `json:"targetPath,omitempty"`

	// TargetSize is the target disk size in bytes. Not populated in this
	// release.
	// +optional
	TargetSize *resource.Quantity `json:"targetSize,omitempty"`

	// Checksum is the SHA256 checksum of the disk. Not populated in this release;
	// see sourceChecksum and targetChecksum.
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
	// URL is the intermediate storage URL. Not populated in this release.
	// +optional
	URL string `json:"url,omitempty"`

	// Size is the size of data in intermediate storage. Not populated in this
	// release.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// UploadedAt is when the data was uploaded. Not populated in this release.
	// +optional
	UploadedAt *metav1.Time `json:"uploadedAt,omitempty"`

	// CleanedUp is set once the post-migration cleanup (deleting the source
	// snapshot and, when requested, the source VM) has run for a migration that
	// reached Ready. It is also set, with nothing deleted, for a migration that
	// reached Ready under an earlier release.
	// +optional
	CleanedUp bool `json:"cleanedUp,omitempty"`

	// StagingLayout is the naming layout of the migration's nfs / s3 staged
	// object, recorded by the controller when the export is sent. The import
	// and the staged-disk delete use it, so a migration keeps the name it was
	// exported under. "v2" (new migrations): nfs file
	// "vmmigrations_<namespace>_<name>_<stage>.qcow2". "v1" or empty (exported
	// before v2): "vmmigrations-<namespace>-<name>-<stage>.qcow2". The s3 key is
	// the same in both. Written by the controller only.
	// +optional
	// +kubebuilder:validation:Enum=v1;v2
	StagingLayout string `json:"stagingLayout,omitempty"`

	// StagingDigest is a digest of where the last export staged the object —
	// the staging backend and its non-secret options, and the target Provider
	// (namespace, name, UID) that will delete it — recorded by the controller
	// when the export is sent. The staged-disk delete is sent only
	// while the migration's current spec still yields this digest: after
	// spec.storage or spec.target.providerRef changed, the object is recorded
	// as left behind instead of being looked for somewhere else. It carries no
	// location. Written by the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=80
	StagingDigest string `json:"stagingDigest,omitempty"`

	// ImportTarget is the VirtualMachine a migration's ImportDisk was sent
	// for, recorded by the controller just before the import is sent (for every
	// storage backend). The staged-disk delete and the
	// landed-disk delete name only this VM, never the current spec.target
	// (which may have been edited since), so a target provider removes only
	// the copy it landed for that VM. Written by the controller only.
	// +optional
	ImportTarget *MigrationImportTarget `json:"importTarget,omitempty"`
}

// MigrationImportTarget is the namespace and name of the VirtualMachine a
// migration's import was sent for, and the Provider it was sent to.
type MigrationImportTarget struct {
	// Namespace is the target VirtualMachine's namespace.
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// Name is the target VirtualMachine's name.
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// ProviderUID is the UID of the target Provider the import was sent to. The
	// landed-disk delete is sent only to that Provider: after
	// spec.target.providerRef changed, or the Provider was replaced, the
	// landed disk is recorded as left behind instead. Empty for an import
	// recorded by an older manager.
	// +optional
	// +kubebuilder:validation:MaxLength=36
	ProviderUID string `json:"providerUID,omitempty"`

	// Host is the Host of a clustered target Provider the import was routed
	// to: the target VirtualMachine's pending host, recorded with ProviderUID
	// before the import is sent. A re-sent import
	// and the landed-disk and nfs staged-disk deletes go to this host only.
	// Empty unless the target Provider is clustered (topology: cluster). Written by
	// the controller only.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host,omitempty"`
}

// ValidationResults contains results of validation checks. Not populated in this
// release.
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
