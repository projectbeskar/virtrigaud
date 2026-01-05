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

package contracts

import (
	"context"
)

// PowerOp represents a power operation type
type PowerOp string

const (
	// PowerOpOn powers on the VM
	PowerOpOn PowerOp = "On"
	// PowerOpOff powers off the VM
	PowerOpOff PowerOp = "Off"
	// PowerOpReboot reboots the VM
	PowerOpReboot PowerOp = "Reboot"
	// PowerOpShutdownGraceful gracefully shuts down the VM using guest tools
	PowerOpShutdownGraceful PowerOp = "ShutdownGraceful"
)

// CreateRequest contains all information needed to create a VM
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

// CreateResponse contains the result of a create operation
type CreateResponse struct {
	// ID is the provider-specific identifier
	ID string
	// TaskRef references an async operation if applicable
	TaskRef string
}

// DescribeResponse contains the current state of a VM
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

// Provider defines the interface that all providers must implement
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

	// ListVMs returns all VMs managed by this provider
	// Used for discovery and adoption of existing VMs
	ListVMs(ctx context.Context) ([]VMInfo, error)
}

// VMInfo contains basic information about a VM for discovery
type VMInfo struct {
	// ID is the provider-specific VM identifier
	ID string
	// Name is the VM name
	Name string
	// PowerState is the current power state
	PowerState string
	// IPs contains assigned IP addresses
	IPs []string
	// CPU is the number of virtual CPUs
	CPU int32
	// MemoryMiB is the amount of memory in MiB
	MemoryMiB int64
	// Disks contains disk information
	Disks []DiskInfo
	// Networks contains network information
	Networks []NetworkInfo
	// ProviderRaw contains provider-specific metadata
	ProviderRaw map[string]string
}

// DiskInfo contains information about a VM disk
type DiskInfo struct {
	// ID is the disk identifier
	ID string
	// Path is the disk path
	Path string
	// SizeGiB is the disk size in GiB
	SizeGiB int32
	// Format is the disk format (qcow2, vmdk, etc.)
	Format string
}

// NetworkInfo contains information about a VM network interface
type NetworkInfo struct {
	// Name is the network name
	Name string
	// MAC is the MAC address
	MAC string
	// IPAddress is the IP address if static
	IPAddress string
}
