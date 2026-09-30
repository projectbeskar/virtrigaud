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
	// the domain name — "<namespace>.<name>" for a new domain, derived from this
	// Owner, the bare name for a legacy one — stamped in the domain <metadata>;
	// vSphere: the bare name within the target folder, stamped in the VM's
	// ExtraConfig — see docs/vm-ownership.md) stamps it onto the VM it creates and binds to
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
	// Namespace is the object's namespace (informational: audit, diagnostics,
	// and naming the hypervisor-side object — never authorization).
	Namespace string
	// Name is the object's name (informational: audit, diagnostics, and
	// naming the hypervisor-side object — never authorization).
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
	// MaxMemoryMiB is the most memory, in MiB, the VM's guest can use without
	// any action on the host: the memory it was started with, including any
	// balloon headroom above its current allocation (libvirt's <memory>). 0
	// means the provider does not report it.
	MaxMemoryMiB int64
	// VCPUs is the number of vCPUs the VM has online now (the running VM's
	// count, or the count it boots with when off). 0 means the provider does
	// not report it.
	VCPUs int32
}

// ReconfigureResult is what a Reconfigure applied.
type ReconfigureResult struct {
	// TaskRef references an async operation if applicable. A provider that
	// returns one leaves RestartRequired false; the task's outcome decides.
	TaskRef string
	// RestartRequired reports that at least one requested change was applied
	// only to the VM's persistent definition and takes effect at its next power
	// cycle (power off, then on); until then the running VM keeps its previous
	// size. Every other requested change was applied to the running VM too.
	RestartRequired bool
	// Honest is the per-response marker of the honest result contract
	// (TaskResponse.honest_result): the provider vouches that this result
	// reports every requested change truthfully — applied, or applied to the
	// persistent definition only with RestartRequired. A provider that
	// implements the contract sets it on every Reconfigure result; false from
	// one that predates or does not implement it. The manager treats a
	// successful clustered Reconfigure without it as not honest.
	Honest bool
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
	// addresses. Every change the request asks for is either applied to the
	// running VM and its persistent definition, or applied to the persistent
	// definition only and reported with ReconfigureResult.RestartRequired, or
	// the call fails: it never reports success for a requested change it did
	// not apply. On a clustered provider vm.Owner is checked exactly as for
	// Power: a VM this VirtualMachine does not own is reported as not-found and
	// left unchanged. The result carries a TaskRef if the operation is
	// asynchronous.
	Reconfigure(ctx context.Context, vm VMRef, desired CreateRequest) (ReconfigureResult, error)

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

	// ListVMs returns all VMs managed by this provider. Used for discovery and
	// adoption of existing VMs. A clustered provider lists every host it
	// fronts (ADR-0007 Addendum A, A3): each VMInfo carries its HostID, and
	// VMList.UnreachableHostIDs names the hosts whose VMs could not be listed —
	// which a caller must treat as unknown, never as empty.
	ListVMs(ctx context.Context) (VMList, error)

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
	// HostID is the host (Host CR name) a clustered provider found the VM on
	// (ADR-0007 Addendum A, A3). On a clustered provider a VM is identified by
	// (HostID, ID), never by ID or name alone: two hosts may each have a VM of
	// the same name. Empty for single-host and thin-client providers.
	HostID string
	// OwnerNamespace and OwnerName are the namespace and name recorded in the
	// VM's owner stamp (its UID is ProviderRaw[VMInfoOwnerUIDKey]); empty when
	// the VM carries no stamp or more than one. Informational only — never an
	// authorization. Set by a clustered provider (ADR-0007 Addendum A, slice 4;
	// A6's R4 check uses them); single-host and thin-client providers leave
	// them empty.
	OwnerNamespace string
	OwnerName      string
}

// VMList is the result of ListVMs.
type VMList struct {
	// VMs are the VMs the provider listed.
	VMs []VMInfo
	// UnreachableHostIDs names every host of a clustered provider whose VMs
	// could not be listed in this call (unreachable, past its per-host deadline,
	// or a failed listing; ADR-0007 Addendum A, A3). Each is UNKNOWN, not empty:
	// a VM the caller knows on one of these hosts must never be treated as
	// gone. Always empty for single-host and thin-client providers.
	UnreachableHostIDs []string
	// OwnerFilterApplied is true when the provider applied an owner filter
	// (OwnerFilteredLister, ADR-0007 A6.2): VMs holds only that
	// VirtualMachine's candidates. Always false on an unfiltered listing, and
	// from a provider that ignored the filter.
	OwnerFilterApplied bool
}

// Unreachable reports whether hostID is one of the hosts l could not list.
func (l VMList) Unreachable(hostID string) bool {
	for _, h := range l.UnreachableHostIDs {
		if h == hostID {
			return true
		}
	}
	return false
}

// VMInfoOwnerUIDKey is the VMInfo.ProviderRaw key under which a provider that
// stamps the VMs it creates with their owner (#333; the libvirt provider)
// reports the owner UID(s) recorded on a listed VM, comma-separated. The
// adoption flow never adopts a VM stamped with the UID of a VirtualMachine that
// still exists: that VM is already managed (possibly through another Provider
// object or in another namespace).
const VMInfoOwnerUIDKey = "owner_uid"

// ProviderRawLinkedCloneDependentsKey is the DescribeResponse.ProviderRaw key
// in which a provider reports how many other VMs on the host depended on this
// VM's disk (as a backing file, e.g. linked clones of it) when the VM was last
// started — a decimal count; absent when not known. Powering such a VM on
// while those VMs are shut off corrupts them, so the manager surfaces a
// non-zero count as the LinkedClonesDependOnDisk condition.
const ProviderRawLinkedCloneDependentsKey = "linked_clone_dependents"

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
