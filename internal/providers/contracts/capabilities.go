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
