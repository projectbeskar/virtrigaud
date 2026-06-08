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

package libvirt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestServer_Clone_NilProvider verifies that the libvirt Clone RPC fails
// cleanly when no provider is wired, rather than panicking. Clone is now
// implemented (issue #153); the previous Unimplemented contract no longer
// applies. The full clone flow requires a live libvirt host and is covered by
// integration tests; here we only assert the nil-provider guard.
func TestServer_Clone_NilProvider(t *testing.T) {
	s := &Server{}

	resp, err := s.Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "vm-source",
		TargetName: "vm-target",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestServer_ImagePrepare_ReturnsUnimplemented verifies that the libvirt
// ImagePrepare RPC returns a gRPC Unimplemented status rather than a fabricated
// task reference. ImagePrepare is not implemented for libvirt (issue #154).
func TestServer_ImagePrepare_ReturnsUnimplemented(t *testing.T) {
	s := &Server{}

	resp, err := s.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		TargetName: "fedora-tmpl",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"ImagePrepare must report Unimplemented so callers do not treat a no-op as success")
}

// TestServer_GetCapabilities_HonestFlags verifies that the libvirt provider
// advertises capabilities that match its actual behavior: linked clones are now
// supported (Clone implemented, issue #153), image import remains unsupported
// (issue #154), and snapshots remain supported.
func TestServer_GetCapabilities_HonestFlags(t *testing.T) {
	s := &Server{}

	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.True(t, caps.SupportsLinkedClones,
		"libvirt advertises linked clones now that Clone is implemented (issue #153)")
	assert.False(t, caps.SupportsImageImport,
		"libvirt must not advertise image import while ImagePrepare is unimplemented (issue #154)")
	assert.True(t, caps.SupportsSnapshots,
		"libvirt snapshots are implemented and must remain advertised")
}

// TestServer_GetCapabilities_DiskMigration verifies the disk-migration
// capabilities advertised after wiring ExportDisk/GetDiskInfo (issue #177).
func TestServer_GetCapabilities_DiskMigration(t *testing.T) {
	s := &Server{}

	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.True(t, caps.SupportsDiskExport, "ExportDisk is now wired over gRPC (issue #177)")
	assert.True(t, caps.SupportsDiskImport, "ImportDisk is wired over gRPC")
	assert.Equal(t, []string{"qcow2", "raw"}, caps.SupportedExportFormats)
	assert.Equal(t, []string{"qcow2", "raw", "vmdk"}, caps.SupportedImportFormats)
	assert.True(t, caps.SupportsExportCompression, "ExportDisk honors req.Compress via qemu-img -c for qcow2 (#199)")
}

// fakeDiskProvider is a minimal contracts.Provider used to exercise the Server's
// ExportDisk/GetDiskInfo delegation and type conversion without a live libvirt.
// It embeds the interface so only the two methods under test need real bodies;
// no other interface method is invoked by these tests.
type fakeDiskProvider struct {
	contracts.Provider

	gotExport  contracts.ExportDiskRequest
	exportResp contracts.ExportDiskResponse
	exportErr  error

	gotDiskInfo contracts.GetDiskInfoRequest
	diskResp    contracts.GetDiskInfoResponse
	diskErr     error
}

func (f *fakeDiskProvider) ExportDisk(_ context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	f.gotExport = req
	return f.exportResp, f.exportErr
}

func (f *fakeDiskProvider) GetDiskInfo(_ context.Context, req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	f.gotDiskInfo = req
	return f.diskResp, f.diskErr
}

// TestServer_ExportDisk_DelegatesAndConverts verifies the Server translates the
// gRPC request to the provider contract and the contract response back to gRPC,
// including the DestinationUrl→DestinationURL and TaskRef→Task field mappings.
func TestServer_ExportDisk_DelegatesAndConverts(t *testing.T) {
	fake := &fakeDiskProvider{
		exportResp: contracts.ExportDiskResponse{
			ExportId:           "export-123",
			TaskRef:            "task-abc",
			EstimatedSizeBytes: 4096,
			Checksum:           "deadbeef",
		},
	}
	s := &Server{provider: fake}

	resp, err := s.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId:           "vm-1",
		DiskId:         "disk-0",
		SnapshotId:     "snap-1",
		DestinationUrl: "pvc://mig/out.qcow2",
		Format:         "qcow2",
		Compress:       true,
		Credentials:    map[string]string{"k": "v"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Request fields converted correctly.
	assert.Equal(t, "vm-1", fake.gotExport.VmId)
	assert.Equal(t, "disk-0", fake.gotExport.DiskId)
	assert.Equal(t, "snap-1", fake.gotExport.SnapshotId)
	assert.Equal(t, "pvc://mig/out.qcow2", fake.gotExport.DestinationURL)
	assert.Equal(t, "qcow2", fake.gotExport.Format)
	assert.True(t, fake.gotExport.Compress)
	assert.Equal(t, map[string]string{"k": "v"}, fake.gotExport.Credentials)

	// Response fields converted correctly.
	assert.Equal(t, "export-123", resp.ExportId)
	assert.Equal(t, int64(4096), resp.EstimatedSizeBytes)
	assert.Equal(t, "deadbeef", resp.Checksum)
	require.NotNil(t, resp.Task)
	assert.Equal(t, "task-abc", resp.Task.Id)
}

// TestServer_ExportDisk_NoTaskRefOmitsTask verifies an empty TaskRef yields a nil
// Task (rather than an empty TaskRef object).
func TestServer_ExportDisk_NoTaskRefOmitsTask(t *testing.T) {
	fake := &fakeDiskProvider{exportResp: contracts.ExportDiskResponse{ExportId: "e1"}}
	s := &Server{provider: fake}

	resp, err := s.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{VmId: "vm-1"})

	require.NoError(t, err)
	assert.Nil(t, resp.Task, "empty TaskRef must map to a nil Task")
}

// TestServer_GetDiskInfo_DelegatesAndConverts verifies the full field mapping for
// GetDiskInfo.
func TestServer_GetDiskInfo_DelegatesAndConverts(t *testing.T) {
	fake := &fakeDiskProvider{
		diskResp: contracts.GetDiskInfoResponse{
			DiskId:           "vm-1-disk",
			Format:           "qcow2",
			VirtualSizeBytes: 10 << 30,
			ActualSizeBytes:  2 << 30,
			Path:             "/var/lib/libvirt/images/vm-1-disk",
			IsBootable:       true,
			Snapshots:        []string{"s1", "s2"},
			BackingFile:      "base.qcow2",
			Metadata:         map[string]string{"pool": "default"},
		},
	}
	s := &Server{provider: fake}

	resp, err := s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId:   "vm-1",
		DiskId: "disk-0",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, "vm-1", fake.gotDiskInfo.VmId)
	assert.Equal(t, "disk-0", fake.gotDiskInfo.DiskId)

	assert.Equal(t, "vm-1-disk", resp.DiskId)
	assert.Equal(t, "qcow2", resp.Format)
	assert.Equal(t, int64(10<<30), resp.VirtualSizeBytes)
	assert.Equal(t, int64(2<<30), resp.ActualSizeBytes)
	assert.Equal(t, "/var/lib/libvirt/images/vm-1-disk", resp.Path)
	assert.True(t, resp.IsBootable)
	assert.Equal(t, []string{"s1", "s2"}, resp.Snapshots)
	assert.Equal(t, "base.qcow2", resp.BackingFile)
	assert.Equal(t, map[string]string{"pool": "default"}, resp.Metadata)
}

// TestServer_DiskOps_NilProvider verifies both RPCs error cleanly when the
// provider is not initialized rather than panicking.
func TestServer_DiskOps_NilProvider(t *testing.T) {
	s := &Server{}

	_, err := s.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{VmId: "vm-1"})
	require.Error(t, err)

	_, err = s.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: "vm-1"})
	require.Error(t, err)
}
