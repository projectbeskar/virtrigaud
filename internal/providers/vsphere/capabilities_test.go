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

package vsphere

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestGetCapabilities_DiskMigration verifies vSphere advertises its implemented
// disk export/import capabilities and formats accurately (issue #178). These
// were previously left at the zero value, understating real support — which
// would wrongly block vSphere migrations once capability gating is enabled (#176).
func TestGetCapabilities_DiskMigration(t *testing.T) {
	p := &Provider{}

	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	require.NotNil(t, caps)

	// The corrected disk-migration flags (issue #178).
	assert.True(t, caps.SupportsDiskExport, "vSphere implements ExportDisk")
	assert.True(t, caps.SupportsDiskImport, "vSphere implements ImportDisk")
	assert.Equal(t, []string{"vmdk", "qcow2", "raw"}, caps.SupportedExportFormats)
	assert.Equal(t, []string{"vmdk", "qcow2", "raw"}, caps.SupportedImportFormats)
	assert.True(t, caps.SupportsExportCompression, "export uses compressed streamOptimized VMDK")

	// Sanity: existing flags remain unchanged.
	assert.True(t, caps.SupportsSnapshots)
	assert.True(t, caps.SupportsLinkedClones)
	// vSphere captures RAM-inclusive snapshots via CreateSnapshot(memory=true) when the
	// VM is powered on; SnapshotCreate already honours req.IncludeMemory (issue #200).
	assert.True(t, caps.SupportsMemorySnapshots)
}

// TestGetCapabilities_StorageBackends verifies vSphere advertises the honest
// ADR-0006 per-direction surface: it exports to pvc AND s3 (Slice 1 SOURCE,
// vCenter datastore stream → pod → S3) AND, as of Slice 2, imports from pvc AND
// s3 (TARGET of libvirt→S3→vSphere: download qcow2 → monolithicSparse vmdk →
// datastore-HTTP → CopyVirtualDisk). Transfer is relay-only.
func TestGetCapabilities_StorageBackends(t *testing.T) {
	p := &Provider{}

	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)

	assert.Equal(t, []string{"pvc", "s3", "nfs"}, caps.SupportedExportBackends,
		"vSphere exports to pvc, s3 (Slice 1/2) and nfs (Slice 4)")
	assert.Equal(t, []string{"pvc", "s3", "nfs"}, caps.SupportedImportBackends,
		"vSphere imports from pvc, s3 (Slice 2) and nfs (Slice 4)")
	assert.Equal(t, []string{"relay"}, caps.SupportedTransferModes)
}

// TestExportDisk_BackendGate verifies the ADR-0006 backend/mode gate at the top of
// ExportDisk: as of Slice 4, nfs PASSES the backend gate (it is implemented) and
// fails LATER at the nil-client check with codes.Unavailable, NOT Unimplemented;
// an explicit direct mode on the s3 path is still InvalidArgument (loud-fail, never
// a silent downgrade). The gate + nil-client checks run on a zero-value Provider.
func TestExportDisk_BackendGate(t *testing.T) {
	p := &Provider{}

	_, nfsErr := p.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "vm-1", BackendType: "nfs",
	})
	require.Error(t, nfsErr)
	assert.NotEqual(t, codes.Unimplemented, status.Code(nfsErr),
		"nfs export is implemented in Slice 4 and must pass the gate")
	assert.Equal(t, codes.Unavailable, status.Code(nfsErr),
		"nfs export must pass the gate and fail later at the nil-client check")

	_, directErr := p.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "vm-1", BackendType: "s3", TransferMode: "direct",
	})
	require.Error(t, directErr, "explicit direct mode must fail loudly (ADR-0006 D2)")
}

// TestImportDisk_S3BackendPassesGate verifies that, as of ADR-0006 Slice 2, an
// s3 ImportDisk now PASSES the backend gate (vSphere is the TARGET of the
// libvirt→S3→vSphere reverse relay) and fails LATER — here at the nil-client
// check inside importDiskFromS3 with codes.Unavailable, NOT with
// codes.Unimplemented. This is the inversion of the old Slice-1 "s3 import
// rejected" assertion and mirrors the export-S3 direct-mode gate test above.
func TestImportDisk_S3BackendPassesGate(t *testing.T) {
	p := &Provider{}

	_, s3Err := p.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		SourceUrl:   "s3://bucket/disk.qcow2",
		BackendType: "s3",
		TargetName:  "imported",
	})
	require.Error(t, s3Err)
	assert.NotEqual(t, codes.Unimplemented, status.Code(s3Err),
		"vSphere ImportDisk must NOT reject s3 in Slice 2: s3 import is implemented (ADR-0006)")
	assert.Equal(t, codes.Unavailable, status.Code(s3Err),
		"s3 import must pass the gate and fail later at the nil-client check")
}

// TestImportDisk_NFSBackendPassesGate verifies that, as of ADR-0006 Slice 4, an nfs
// ImportDisk now PASSES the backend gate (vSphere is a TARGET over NFS) and fails
// LATER at the nil-client check inside importDiskFromNFS with codes.Unavailable,
// NOT codes.Unimplemented. Mirrors the s3-import-passes-gate test above.
func TestImportDisk_NFSBackendPassesGate(t *testing.T) {
	p := &Provider{}

	_, nfsErr := p.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		SourceUrl:   "nfs://server/export/disk.qcow2",
		BackendType: "nfs",
		TargetName:  "imported",
	})
	require.Error(t, nfsErr)
	assert.NotEqual(t, codes.Unimplemented, status.Code(nfsErr),
		"nfs import is implemented in Slice 4 and must pass the gate")
	assert.Equal(t, codes.Unavailable, status.Code(nfsErr),
		"nfs import must pass the gate and fail later at the nil-client check")
}
