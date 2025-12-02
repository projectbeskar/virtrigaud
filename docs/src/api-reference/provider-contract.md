# Provider Contract Reference

_Auto-generated from provider contract interfaces in `internal/providers/contracts/`_

This document describes the Go interface that all VirtRigaud providers must implement.

---

## Provider Interface

```go
package contracts // import "."

type Provider interface {
	// Validate ensures the provider session/credentials are healthy
	Validate(ctx context.Context) error

	// Create creates a new VM if it doesn't exist (idempotent)
	// Returns TaskRef if the operation is asynchronous
	Create(ctx context.Context, req CreateRequest) (CreateResponse, error)

	// Delete removes a VM (idempotent, succeeds even if VM doesn't exist)
	// Returns TaskRef if the operation is asynchronous
	Delete(ctx context.Context, id string) (taskRef string, err error)

	// Power performs a power operation on the VM
	// Returns TaskRef if the operation is asynchronous
	Power(ctx context.Context, id string, op PowerOp) (taskRef string, err error)

	// Reconfigure modifies VM resources (CPU/RAM/Disks)
	// May be no-op for unsupported fields
	// Returns TaskRef if the operation is asynchronous
	Reconfigure(ctx context.Context, id string, desired CreateRequest) (taskRef string, err error)

	// Describe returns the current state of the VM
	// Should be cheap and resilient to call frequently
	Describe(ctx context.Context, id string) (DescribeResponse, error)

	// IsTaskComplete checks if an async task is complete
	IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error)

	// TaskStatus returns detailed status of an async task
	TaskStatus(ctx context.Context, taskRef string) (TaskStatus, error)

	// SnapshotCreate creates a VM snapshot
	SnapshotCreate(ctx context.Context, req SnapshotCreateRequest) (SnapshotCreateResponse, error)

	// SnapshotDelete deletes a VM snapshot
	SnapshotDelete(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error)

	// SnapshotRevert reverts a VM to a snapshot
	SnapshotRevert(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error)

	// ExportDisk exports a VM disk for migration
	// Returns export identifier and optional task reference for async operations
	ExportDisk(ctx context.Context, req ExportDiskRequest) (ExportDiskResponse, error)

	// ImportDisk imports a disk from an external source
	// Returns disk identifier and optional task reference for async operations
	ImportDisk(ctx context.Context, req ImportDiskRequest) (ImportDiskResponse, error)

	// GetDiskInfo retrieves detailed information about a VM disk
	// Useful for migration planning and validation
	GetDiskInfo(ctx context.Context, req GetDiskInfoRequest) (GetDiskInfoResponse, error)
}
    Provider defines the interface that all providers must implement
```

## Contract Types

```go
package contracts // import "github.com/projectbeskar/virtrigaud/internal/providers/contracts"


TYPES

type CreateRequest struct {
	// Name of the VM to create
	Name string
	// Class defines the VM resource allocation
	Class VMClass
	// Image defines the base template/image
	Image VMImage
	// Networks defines network attachments
	Networks []NetworkAttachment
	// Disks defines additional disks
	Disks []DiskSpec
	// UserData contains cloud-init/ignition configuration
	UserData *UserData
	// Placement provides placement hints
	Placement *Placement
	// Tags are applied to the VM
	Tags []string
}
    CreateRequest contains all information needed to create a VM

type CreateResponse struct {
	// ID is the provider-specific identifier
	ID string
	// TaskRef references an async operation if applicable
	TaskRef string
}
    CreateResponse contains the result of a create operation

type DescribeResponse struct {
	// Exists indicates if the VM exists
	Exists bool
	// PowerState is the current power state
	PowerState string
	// IPs contains assigned IP addresses
	IPs []string
	// ConsoleURL provides console access
	ConsoleURL string
	// ProviderRaw contains provider-specific details
	ProviderRaw map[string]string
}
    DescribeResponse contains the current state of a VM

type DiskDefaults struct {
	// Type specifies the default disk type
	Type string
	// SizeGiB specifies the default root disk size
	SizeGiB int32
}
    DiskDefaults provides default disk settings

type DiskSpec struct {
	// SizeGiB specifies disk size in GiB
	SizeGiB int32
	// Type specifies disk type (thin, thick, etc.)
	Type string
	// Name provides a name for the disk
	Name string
}
    DiskSpec defines disk requirements (provider-agnostic)

type ErrorType string
    ErrorType represents the category of error

const (
	// ErrorTypeNotFound indicates resource not found
	ErrorTypeNotFound ErrorType = "NotFound"
	// ErrorTypeInvalidSpec indicates invalid specification
	ErrorTypeInvalidSpec ErrorType = "InvalidSpec"
	// ErrorTypeRetryable indicates a transient error
	ErrorTypeRetryable ErrorType = "Retryable"
	// ErrorTypeUnauthorized indicates authentication/authorization failure
	ErrorTypeUnauthorized ErrorType = "Unauthorized"
	// ErrorTypeNotSupported indicates unsupported operation
	ErrorTypeNotSupported ErrorType = "NotSupported"
	// ErrorTypeRateLimit indicates rate limiting
	ErrorTypeRateLimit ErrorType = "RateLimit"
	// ErrorTypeUnavailable indicates service unavailable
	ErrorTypeUnavailable ErrorType = "Unavailable"
	// ErrorTypeTimeout indicates operation timeout
	ErrorTypeTimeout ErrorType = "Timeout"
	// ErrorTypeQuotaExceeded indicates quota exceeded
	ErrorTypeQuotaExceeded ErrorType = "QuotaExceeded"
	// ErrorTypeConflict indicates resource conflict
	ErrorTypeConflict ErrorType = "Conflict"
)
type ExportDiskRequest struct {
	// VmId is the VM identifier
	VmId string
	// DiskId identifies which disk to export (empty = primary disk)
	DiskId string
	// SnapshotId specifies a snapshot to export from (optional)
	SnapshotId string
	// DestinationURL where to upload the disk (S3, HTTP, etc.)
	DestinationURL string
	// Format specifies the desired export format (qcow2, vmdk, raw)
	Format string
	// Compress enables compression during export
	Compress bool
	// Credentials for accessing the destination
	Credentials map[string]string
}
    ExportDiskRequest defines a disk export request for migration

type ExportDiskResponse struct {
	// ExportId is the export operation identifier
	ExportId string
	// TaskRef references an async operation if applicable
	TaskRef string
	// EstimatedSizeBytes is the estimated size of the export
	EstimatedSizeBytes int64
	// Checksum is the SHA256 checksum of the exported disk
	Checksum string
}
    ExportDiskResponse contains the result of a disk export operation

type GetDiskInfoRequest struct {
	// VmId is the VM identifier
	VmId string
	// DiskId identifies which disk (empty = primary disk)
	DiskId string
	// SnapshotId gets info for a specific snapshot (optional)
	SnapshotId string
}
    GetDiskInfoRequest defines a request for disk information

type GetDiskInfoResponse struct {
	// DiskId is the disk identifier
	DiskId string
	// Format is the disk format (qcow2, vmdk, raw)
	Format string
	// VirtualSizeBytes is the virtual size (capacity)
	VirtualSizeBytes int64
	// ActualSizeBytes is the actual size (allocated)
	ActualSizeBytes int64
	// Path is the path or location of the disk
	Path string
	// IsBootable indicates if this is a boot disk
	IsBootable bool
	// Snapshots lists available snapshots for this disk
	Snapshots []string
	// BackingFile is the backing file (for linked clones)
	BackingFile string
	// Metadata contains additional provider-specific metadata
	Metadata map[string]string
}
    GetDiskInfoResponse contains detailed disk information

type IPAddress struct {
	// IP is the IP address
	IP string
	// Type specifies the IP type (IPv4, IPv6)
	Type string
	// Source specifies how the IP was assigned (DHCP, static, etc.)
	Source string
}
    IPAddress represents an assigned IP address

type ImportDiskRequest struct {
	// SourceURL where to download the disk from (S3, HTTP, etc.)
	SourceURL string
	// StorageHint suggests target storage location (datastore, pool, etc.)
	StorageHint string
	// Format specifies the source disk format (qcow2, vmdk, raw)
	Format string
	// TargetName is the name for the imported disk
	TargetName string
	// VerifyChecksum enables checksum verification after import
	VerifyChecksum bool
	// ExpectedChecksum is the expected SHA256 checksum
	ExpectedChecksum string
	// Credentials for accessing the source
	Credentials map[string]string
}
    ImportDiskRequest defines a disk import request for migration

type ImportDiskResponse struct {
	// DiskId is the imported disk identifier
	DiskId string
	// Path to the imported disk in provider storage
	Path string
	// TaskRef references an async operation if applicable
	TaskRef string
	// ActualSizeBytes is the actual size of the imported disk
	ActualSizeBytes int64
	// Checksum is the SHA256 checksum of the imported disk
	Checksum string
}
    ImportDiskResponse contains the result of a disk import operation

type NetworkAttachment struct {
	// Name identifies the network
	Name string
	// Portgroup for vSphere
	Portgroup string
	// NetworkName for generic networks
```

---

_Generated on: 2025-12-02 01:05:54 UTC_
