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
// ADR-0006 Slice 1 per-direction surface: as the SOURCE it exports to pvc AND s3
// (vCenter datastore stream → pod → S3), but it still only IMPORTS from pvc
// (S3→vSphere import is a later slice). Transfer is relay-only.
func TestGetCapabilities_StorageBackends(t *testing.T) {
	p := &Provider{}

	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)

	assert.Equal(t, []string{"pvc", "s3"}, caps.SupportedExportBackends,
		"vSphere exports to pvc and s3 in Slice 1 (SOURCE of vSphere→S3→libvirt)")
	assert.Equal(t, []string{"pvc"}, caps.SupportedImportBackends,
		"vSphere import stays pvc-only in Slice 1 (vSphere is SOURCE, not TARGET)")
	assert.Equal(t, []string{"relay"}, caps.SupportedTransferModes)
}

// TestExportDisk_BackendGate verifies the ADR-0006 backend/mode gate at the top
// of ExportDisk: nfs is Unimplemented, an explicit direct mode is InvalidArgument
// (loud-fail, never a silent downgrade). These guards run before the nil-client
// check, so a zero-value Provider suffices.
func TestExportDisk_BackendGate(t *testing.T) {
	p := &Provider{}

	_, nfsErr := p.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "vm-1", BackendType: "nfs",
	})
	require.Error(t, nfsErr)

	_, directErr := p.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "vm-1", BackendType: "s3", TransferMode: "direct",
	})
	require.Error(t, directErr, "explicit direct mode must fail loudly (ADR-0006 D2)")
}
