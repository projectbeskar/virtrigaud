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

// Package migration defines the storage-backend vocabulary shared by the
// migration controller and every provider's ExportDisk/ImportDisk
// implementation. ADR-0006 introduces pluggable staging backends; this package
// is the single source of truth for the backend/transfer-mode string literals
// so they never drift between the manager and the providers.
//
// ADR-0006 Slice 0 is surface-only: the constants and capability-advertisement
// helpers exist, but no provider implements anything beyond the legacy pvc
// path. EnsurePVCBackend lets every provider reject non-pvc requests uniformly
// with codes.Unimplemented.
package migration

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Storage backend identifiers (the values of MigrationStorage.Type and the
// proto ExportDiskRequest/ImportDiskRequest.backend_type field).
const (
	// BackendPVC stages the transferred disk on a Kubernetes PVC mounted into
	// the provider pods. It is the only backend with transfer logic today.
	BackendPVC = "pvc"
	// BackendNFS stages the transferred disk on an NFS export (ADR-0006;
	// surface-only in Slice 0).
	BackendNFS = "nfs"
	// BackendS3 stages the transferred disk on S3-compatible object storage
	// (ADR-0006; surface-only in Slice 0).
	BackendS3 = "s3"
)

// Transfer mode identifiers (the values of MigrationStorage.TransferMode and
// the proto ExportDiskRequest/ImportDiskRequest.transfer_mode field).
const (
	// TransferModeAuto lets the controller/provider pick relay or direct from
	// what both sides advertise.
	TransferModeAuto = "auto"
	// TransferModeRelay routes bytes host -> provider-pod -> backend (today's
	// pod-side path).
	TransferModeRelay = "relay"
	// TransferModeDirect routes bytes host -> backend with no provider-pod hop
	// (ADR-0006; surface-only in Slice 0).
	TransferModeDirect = "direct"
)

// PVCOnlyExportBackends is the honest export-backend set for a provider that
// only implements the legacy pvc staging path.
func PVCOnlyExportBackends() []string { return []string{BackendPVC} }

// PVCOnlyImportBackends is the honest import-backend set for a provider that
// only implements the legacy pvc staging path.
func PVCOnlyImportBackends() []string { return []string{BackendPVC} }

// PVCAndS3ExportBackends is the honest export-backend set for a provider that
// implements both the legacy pvc path and the S3 relay export path. Used by the
// vSphere provider as the SOURCE in ADR-0006 Slice 1 (vSphere → S3 → libvirt).
func PVCAndS3ExportBackends() []string { return []string{BackendPVC, BackendS3} }

// PVCAndS3ImportBackends is the honest import-backend set for a provider that
// implements both the legacy pvc path and the S3 relay import path. Used by the
// libvirt provider as the TARGET in ADR-0006 Slice 1 (vSphere → S3 → libvirt).
func PVCAndS3ImportBackends() []string { return []string{BackendPVC, BackendS3} }

// S3OnlyExportBackends is the honest export-backend set for a provider whose
// only working transfer is S3. The Proxmox provider uses this: its disks live on
// the PVE node and the provider pod is the S3 client (bytes flow node ↔ pod ↔ S3
// over SSH). The legacy pvc path does os.Open on node-local image paths, which a
// remote pod can never reach, so advertising pvc would be dishonest.
func S3OnlyExportBackends() []string { return []string{BackendS3} }

// S3OnlyImportBackends is the honest import-backend set for an S3-only provider
// (the Proxmox TARGET). See S3OnlyExportBackends for why pvc is excluded.
func S3OnlyImportBackends() []string { return []string{BackendS3} }

// RelayOnlyTransferModes is the honest transfer-mode set for a provider that
// only implements the pod-side (relay) path. Both the legacy pvc path and the
// ADR-0006 Slice 1 S3 path are relay-shaped (bytes flow host → provider-pod →
// backend); no provider advertises "direct" before Slice 2.
func RelayOnlyTransferModes() []string { return []string{TransferModeRelay} }

// EnsureRelayMode returns a codes.InvalidArgument error if transferMode names a
// mode other than relay/auto. Slice 1 implements only the relay (pod-as-backend-
// client) path; an explicit "direct" must fail loudly, never silently downgrade
// (ADR-0006 D2). Empty and "auto" are accepted: the controller resolves auto to
// relay before issuing the RPC, so a provider treats both as relay.
func EnsureRelayMode(transferMode string) error {
	switch transferMode {
	case "", TransferModeAuto, TransferModeRelay:
		return nil
	default:
		return status.Errorf(codes.InvalidArgument,
			"transfer mode %q not supported; only %q is implemented (ADR-0006 Slice 1)",
			transferMode, TransferModeRelay)
	}
}

// EnsurePVCBackend returns a codes.Unimplemented error if backendType names a
// staging backend other than pvc. An empty backendType means the legacy pvc
// path and is always accepted, preserving pre-ADR-0006 behavior. Providers that
// have NOT yet implemented a non-pvc path (libvirt export, vSphere import,
// proxmox, mock) call this at the top of ExportDisk/ImportDisk so non-pvc
// requests fail honestly instead of silently falling through to the pvc path.
func EnsurePVCBackend(backendType string) error {
	if backendType == "" || backendType == BackendPVC {
		return nil
	}
	return status.Errorf(codes.Unimplemented,
		"backend %q not yet supported on this provider/direction (ADR-0006)", backendType)
}

// EnsurePVCOrS3Backend returns a codes.Unimplemented error if backendType names
// a staging backend other than pvc or s3. An empty backendType means the legacy
// pvc path. Providers that implement the ADR-0006 Slice 1 S3 relay path in a
// given direction (vSphere export, libvirt import) call this so pvc and s3 are
// accepted while nfs/unknown still fail honestly.
func EnsurePVCOrS3Backend(backendType string) error {
	switch backendType {
	case "", BackendPVC, BackendS3:
		return nil
	default:
		return status.Errorf(codes.Unimplemented,
			"backend %q not yet supported on this provider/direction (ADR-0006)", backendType)
	}
}
