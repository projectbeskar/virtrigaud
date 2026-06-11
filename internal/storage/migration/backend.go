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
// only implements the legacy pvc staging path (every production provider in
// ADR-0006 Slice 0).
func PVCOnlyExportBackends() []string { return []string{BackendPVC} }

// PVCOnlyImportBackends is the honest import-backend set for a provider that
// only implements the legacy pvc staging path (every production provider in
// ADR-0006 Slice 0).
func PVCOnlyImportBackends() []string { return []string{BackendPVC} }

// RelayOnlyTransferModes is the honest transfer-mode set for a provider that
// only implements the pod-side (relay-shaped) path. The existing pvc path
// stages bytes through the provider pod, i.e. it is relay-shaped; no provider
// advertises "direct" in ADR-0006 Slice 0.
func RelayOnlyTransferModes() []string { return []string{TransferModeRelay} }

// EnsurePVCBackend returns a codes.Unimplemented error if backendType names a
// staging backend other than pvc. An empty backendType means the legacy pvc
// path and is always accepted, preserving pre-ADR-0006 behavior. Providers call
// this at the top of ExportDisk/ImportDisk so that non-pvc requests fail
// honestly instead of silently falling through to the pvc path (ADR-0006 Slice
// 0).
func EnsurePVCBackend(backendType string) error {
	if backendType == "" || backendType == BackendPVC {
		return nil
	}
	return status.Errorf(codes.Unimplemented,
		"backend %q not yet supported (ADR-0006 Slice 0)", backendType)
}
