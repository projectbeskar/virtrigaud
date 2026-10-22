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

import "context"

// Capabilities is the manager-side, transport-agnostic view of what a provider
// supports. It mirrors the provider.v1 GetCapabilitiesResponse and is consumed
// by the manager to surface provider features on the Provider CR status and
// (when enabled) to gate capability-dependent operations (issue #176).
type Capabilities struct {
	// SupportsReconfigureOnline reports whether CPU/memory can be reconfigured
	// without a power cycle.
	SupportsReconfigureOnline bool
	// SupportsDiskExpansionOnline reports whether disks can be expanded online.
	SupportsDiskExpansionOnline bool
	// SupportsSnapshots reports whether the provider supports VM snapshots.
	SupportsSnapshots bool
	// SupportsMemorySnapshots reports whether snapshots can include memory state.
	SupportsMemorySnapshots bool
	// SupportsLinkedClones reports whether copy-on-write linked clones are supported.
	SupportsLinkedClones bool
	// SupportsImageImport reports whether the provider can import/prepare images.
	SupportsImageImport bool
	// SupportedDiskTypes lists the disk formats the provider supports.
	SupportedDiskTypes []string
	// SupportedNetworkTypes lists the NIC models the provider supports.
	SupportedNetworkTypes []string
	// SupportsDiskExport reports whether the provider can export disks (migration source).
	SupportsDiskExport bool
	// SupportsDiskImport reports whether the provider can import disks (migration target).
	SupportsDiskImport bool
	// SupportedExportFormats lists the formats the provider can export to.
	SupportedExportFormats []string
	// SupportedImportFormats lists the formats the provider can import from.
	SupportedImportFormats []string
	// SupportsExportCompression reports whether disk export can be compressed.
	SupportsExportCompression bool
	// SupportedExportBackends lists the storage backends the provider can export
	// disks to ("pvc", "nfs", "s3"). Empty means pvc-only (ADR-0006 Slice 0).
	SupportedExportBackends []string
	// SupportedImportBackends lists the storage backends the provider can import
	// disks from ("pvc", "nfs", "s3"). Empty means pvc-only (ADR-0006 Slice 0).
	SupportedImportBackends []string
	// SupportedTransferModes lists the disk-transfer modes the provider supports
	// ("relay", "direct"). Empty means relay-only (ADR-0006 Slice 0).
	SupportedTransferModes []string
	// SupportsClustering reports whether the provider fronts multiple hosts and
	// implements host inventory (ListHosts/GetHostInfo) plus host-targeted
	// placement (ADR-0007 P1, D7). False for single-host and thin-client
	// providers. The remaining clustering capabilities (live/block migration,
	// host evacuation, storage modes) are added by the migration slice.
	SupportsClustering bool
	// SupportsImageArtifactIdentity reports that the provider's image prepare
	// implements the prepared-image artifact identity contract (ADR-0009 D7):
	// it names and stamps artifacts from the request's VMImage identity and
	// source digest, reuses an artifact only on a matching stamp, and echoes
	// the stamp (ImagePrepareResponse.Artifact). The manager holds
	// import-style prepares through an import-capable provider that does not
	// report it, and sends no RPC.
	SupportsImageArtifactIdentity bool
	// SupportsHonestReconfigure reports that the provider's Reconfigure
	// implements the honest result contract: every requested change is
	// applied live and persistently, or persistently only with
	// ReconfigureResult.RestartRequired, or the call fails. The manager holds
	// resizes of VMs on a clustered Provider that does not report it (its
	// committed-capacity accounting trusts the result); single-host Providers
	// are resized as before, with a warning.
	SupportsHonestReconfigure bool
	// SupportsRoutedClone reports that a clustered provider routes Clone to the
	// source VM's bound host and stamps the clone with its target VM (ADR-0007
	// Addendum A, slice 3). The VMClone controller checks it before it creates
	// a clustered clone's target VirtualMachine. False for single-host and
	// thin-client providers (their Clone does not depend on it).
	SupportsRoutedClone bool
	// SupportsRoutedAdoption reports that a clustered provider lists VMs across
	// all of its hosts (every VMInfo carries its HostID; VMList.
	// UnreachableHostIDs names the hosts it could not list) and implements
	// TransferOwner (ADR-0007 Addendum A, slice 4). The adoption controller adopts
	// from a clustered provider only when it reports it. False for single-host
	// and thin-client providers (their adoption does not depend on it).
	SupportsRoutedAdoption bool
	// SupportsListOwnerFilter reports that ListVMs honours an owner filter
	// (OwnerFilteredLister): only one VirtualMachine's candidates are returned,
	// and the answer is marked VMList.OwnerFilterApplied (ADR-0007 A6.2, R4).
	// The VirtualMachine controller runs its pre-schedule uniqueness check
	// only through a clustered provider that reports it, and holds a clustered
	// VM's first placement otherwise. False for single-host and thin-client
	// providers (they never schedule).
	SupportsListOwnerFilter bool
	// SupportsImportSizeLimit reports that ImportDisk enforces
	// ImportDiskRequest.MaxVirtualSizeBytes on the disk it lands. A provider
	// that predates it ignores the limit; while one applies, the migration
	// controller holds a migration into a Provider that does not report it.
	SupportsImportSizeLimit bool
	// SupportsOwnerTransfer reports that a single-host provider's
	// TransferOwner re-stamps a VM its ListVMs reported (addressed by id
	// alone) with the VirtualMachine that adopts it, compare-and-swap. The
	// adoption controller stamps the VMs it adopts from a single-host Provider
	// only when it is reported; through an older provider it adopts them
	// unstamped, as before, and says so in the Provider's adoption status. It
	// does not change clustered adoption (SupportsRoutedAdoption).
	SupportsOwnerTransfer bool
	// SupportsOwnerRebind reports that Create and Clone stamp the VM they make
	// with its owner and return the VM an earlier attempt made when sent again
	// for the same owner (GetCapabilitiesResponse.supports_owner_rebind, rc2
	// R4). The manager re-sends a Create or Clone whose outcome is unknown
	// only to a provider that reports it; false from a provider that predates
	// the field, which is never sent one again.
	SupportsOwnerRebind bool
	// SupportedStagedDiskDeleteBackends lists the staging backends ("nfs",
	// "s3") whose migration staging objects the provider's DeleteStagedDisk
	// removes (StagedDiskDeleter, ADR-0011 D7). Empty — an older provider, or
	// one that stages nothing outside the cluster — means the VMMigration
	// controller never asks it and records the staged object as left behind.
	SupportedStagedDiskDeleteBackends []string
	// ResendSafeExportBackends lists the staging backends on which an
	// ExportDisk sent again for the same destination, once the earlier call's
	// deadline has passed, replaces what the earlier call wrote and leaves
	// nothing else behind (GetCapabilitiesResponse.resend_safe_export_backends,
	// rc2 R6). The VMMigration controller re-sends an ExportDisk whose outcome
	// it lost only on a listed backend; empty from a provider that predates
	// the field, which is never sent one again.
	ResendSafeExportBackends []string
	// ResendSafeImportBackends is ResendSafeExportBackends for ImportDisk: on
	// a listed backend a re-sent ImportDisk lands the disk at the same name
	// and never makes a second one (rc2 R6).
	ResendSafeImportBackends []string
	// SupportsLandedDiskDelete reports that the provider's DeleteLandedDisk
	// removes the disk a migration's ImportDisk landed for a target VM that
	// does not use it (LandedDiskDeleter, rc2 R7). False — a provider that
	// predates it — means the VMMigration controller never asks it and
	// records the landed disk as left behind.
	SupportsLandedDiskDelete bool
	// SupportsRoutedImport reports that a CLUSTERED provider routes ImportDisk
	// to ImportDiskRequest.TargetHostID (with TargetVM carrying the target
	// VirtualMachine's UID), DeleteLandedDisk to
	// LandedDiskDeleteRequest.TargetHostID with a cross-host in-use scan, and
	// the nfs DeleteStagedDisk to StagedDiskDeleteRequest.TargetHostID
	// (GetCapabilitiesResponse.supports_routed_import, ADR-0007 Addendum B,
	// B5). The VMMigration controller accepts a clustered Provider as a
	// migration target only when it is reported; false from single-host and
	// thin-client providers and from a clustered provider that predates it.
	SupportsRoutedImport bool
}

// CapabilityReporter is an optional capability of a Provider: it reports the
// provider's supported features. The manager gRPC client implements it; callers
// type-assert a Provider to CapabilityReporter to consume capabilities without
// widening the core Provider interface, so in-process provider implementations
// and test fakes are unaffected (issue #176).
type CapabilityReporter interface {
	// GetCapabilities returns the provider's advertised capabilities.
	GetCapabilities(ctx context.Context) (Capabilities, error)
}
