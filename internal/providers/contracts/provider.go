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
	// MetaData contains cloud-init metadata configuration
	MetaData *MetaData
	// Placement provides placement hints
	Placement *Placement
	// Tags are applied to the VM
	Tags []string
	// TargetHostID names the specific host a clustered ("brain-in-operator")
	// provider must create this VM on (ADR-0007 P1, D4). It is the operator
	// scheduler's binding, threaded to the wire as CreateRequest.target_host_id —
	// deliberately separate from Placement (external-orchestrator hints). It is
	// empty for single-host and thin-client providers, which ignore it; the
	// operator populates it only for a Provider with topology: cluster, and a
	// clustered provider rejects an empty value rather than defaulting to a host.
	TargetHostID string
	// Owner identifies the Kubernetes object (the VirtualMachine) this create is
	// performed for, threaded to the wire as CreateRequest.owner. A provider that
	// keys hypervisor VMs by a name that is not unique across tenants (libvirt:
	// the bare VirtualMachine name, stamped in the domain <metadata>; vSphere: the
	// bare name within the target folder, stamped in the VM's ExtraConfig — see
	// docs/vm-ownership.md) stamps it onto the VM it creates and binds to
	// an already-existing VM of the requested name ONLY when that VM carries this
	// Owner.UID — otherwise it fails closed with a Conflict error instead of
	// silently binding to another tenant's VM. The zero value (e.g. from an older
	// manager) never authorizes a bind. Providers that do not key by a shared
	// name ignore it.
	Owner ObjectIdentity
}

// ObjectIdentity names a Kubernetes object: the authoritative UID plus the
// informational namespace and name. It is the manager-side mirror of the
// provider.v1 ObjectIdentity message.
type ObjectIdentity struct {
	// UID is the object's Kubernetes UID — unique for its whole lifetime and
	// never reused, so it is the only field that may be used for authorization.
	UID string
	// Namespace is the object's namespace (informational: audit, diagnostics).
	Namespace string
	// Name is the object's name (informational: audit, diagnostics).
	Name string
}

// IsZero reports whether the identity carries no UID, i.e. no owner is known.
// Only the UID is considered: a namespace/name without a UID proves nothing.
func (o ObjectIdentity) IsZero() bool {
	return o.UID == ""
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

	// Delete removes the VM vm addresses (idempotent, succeeds even if the VM
	// doesn't exist). On a clustered provider vm.Owner is checked (ADR-0007
	// Addendum A, A2): the VM is destroyed only when its recorded owner UID
	// equals vm.Owner.UID, and anything else is reported as not-found without
	// touching it; single-host and thin-client providers ignore the owner.
	// Returns TaskRef if the operation is asynchronous.
	Delete(ctx context.Context, vm VMRef) (taskRef string, err error)

	// Power performs a power operation on the VM vm addresses. On a clustered
	// provider vm.Owner is checked (ADR-0007 Addendum A, slice 2): a VM whose
	// recorded owner UID is not vm.Owner.UID is reported as not-found and is
	// never powered; single-host and thin-client providers ignore the owner.
	// Returns TaskRef if the operation is asynchronous
	Power(ctx context.Context, vm VMRef, op PowerOp) (taskRef string, err error)

	// Reconfigure modifies the resources (CPU/RAM/Disks) of the VM vm
	// addresses. May be no-op for unsupported fields. On a clustered provider
	// vm.Owner is checked exactly as for Power: a VM this VirtualMachine does
	// not own is reported as not-found and left unchanged.
	// Returns TaskRef if the operation is asynchronous
	Reconfigure(ctx context.Context, vm VMRef, desired CreateRequest) (taskRef string, err error)

	// Describe returns the current state of the VM vm addresses. On a clustered
	// provider a VM whose owner stamp does not record vm.Owner.UID is reported
	// as not existing, and none of its state is returned.
	// Should be cheap and resilient to call frequently
	Describe(ctx context.Context, vm VMRef) (DescribeResponse, error)

	// IsTaskComplete checks if an async task is complete
	IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error)

	// TaskStatus returns detailed status of an async task
	TaskStatus(ctx context.Context, taskRef string) (TaskStatus, error)

	// SnapshotCreate creates a VM snapshot
	SnapshotCreate(ctx context.Context, req SnapshotCreateRequest) (SnapshotCreateResponse, error)

	// SnapshotDelete deletes snapshot snapshotID of the VM vm addresses.
	SnapshotDelete(ctx context.Context, vm VMRef, snapshotID string) (taskRef string, err error)

	// SnapshotRevert reverts the VM vm addresses to snapshot snapshotID.
	SnapshotRevert(ctx context.Context, vm VMRef, snapshotID string) (taskRef string, err error)

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

	// ListHosts returns every hypervisor host fronted by this provider.
	// Clustered providers (ADR-0007 P1) report their host inventory here so the
	// operator can schedule VMs across hosts; single-host and thin-client
	// providers return an Unimplemented error and advertise
	// Capabilities.SupportsClustering = false.
	ListHosts(ctx context.Context) ([]HostInfo, error)

	// GetHostInfo returns the current inventory for a single host — a cheaper
	// refresh than a full ListHosts poll. Like ListHosts it is only implemented
	// by clustered providers; others return an Unimplemented error.
	GetHostInfo(ctx context.Context, hostID string) (HostInfo, error)
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
