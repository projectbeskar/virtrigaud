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

package proxmox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestProxmoxProvider_Validate(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()
	resp, err := provider.Validate(ctx, &providerv1.ValidateRequest{})

	require.NoError(t, err)
	assert.True(t, resp.Ok)
	assert.Contains(t, resp.Message, "Proxmox VE provider is ready")
}

func TestProxmoxProvider_CreateAndDescribe(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Test VM creation
	createReq := &providerv1.CreateRequest{
		Name:      "test-vm-create",
		ClassJson: `{"cpus": 2, "memory": "2Gi"}`,
		ImageJson: `{"source": "ubuntu-22-template"}`,
		UserData:  []byte("#cloud-config\nhostname: test-vm"),
	}

	createResp, err := provider.Create(ctx, createReq)
	require.NoError(t, err)
	require.NotEmpty(t, createResp.Id)

	// Wait for creation task to complete if there is one
	if createResp.Task != nil {
		err = waitForTask(ctx, provider, createResp.Task.Id)
		require.NoError(t, err)
	}

	// Test VM description
	describeReq := &providerv1.DescribeRequest{
		Id: createResp.Id,
	}

	describeResp, err := provider.Describe(ctx, describeReq)
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
	assert.Equal(t, "Off", describeResp.PowerState)
	assert.NotEmpty(t, describeResp.ConsoleUrl)
}

func TestProxmoxProvider_PowerOperations(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use existing test VM (ID 100 from fake server seed data)
	vmID := "100"

	// Test power on
	powerReq := &providerv1.PowerRequest{
		Id: vmID,
		Op: providerv1.PowerOp_POWER_OP_ON,
	}

	powerResp, err := provider.Power(ctx, powerReq)
	require.NoError(t, err)

	// Wait for power task to complete if there is one
	if powerResp.Task != nil {
		err = waitForTask(ctx, provider, powerResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM is running
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: vmID})
	require.NoError(t, err)
	assert.Equal(t, "On", describeResp.PowerState)

	// Test power off
	powerReq.Op = providerv1.PowerOp_POWER_OP_OFF
	powerResp, err = provider.Power(ctx, powerReq)
	require.NoError(t, err)

	// Wait for power task to complete if there is one
	if powerResp.Task != nil {
		err = waitForTask(ctx, provider, powerResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM is stopped
	describeResp, err = provider.Describe(ctx, &providerv1.DescribeRequest{Id: vmID})
	require.NoError(t, err)
	assert.Equal(t, "Off", describeResp.PowerState)
}

func TestProxmoxProvider_Clone(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use template VM (ID 9000 from fake server seed data)
	cloneReq := &providerv1.CloneRequest{
		SourceVmId: "9000",
		TargetName: "test-clone",
		Linked:     true,
	}

	cloneResp, err := provider.Clone(ctx, cloneReq)
	require.NoError(t, err)
	require.NotEmpty(t, cloneResp.TargetVmId)

	// Wait for clone task to complete if there is one
	if cloneResp.Task != nil {
		err = waitForTask(ctx, provider, cloneResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify cloned VM exists
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: cloneResp.TargetVmId})
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
}

func TestProxmoxProvider_Delete(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Create a VM first
	createReq := &providerv1.CreateRequest{
		Name:      "test-vm-delete",
		ClassJson: `{"cpus": 1, "memory": "1Gi"}`,
	}

	createResp, err := provider.Create(ctx, createReq)
	require.NoError(t, err)

	// Wait for creation task to complete if there is one
	if createResp.Task != nil {
		err = waitForTask(ctx, provider, createResp.Task.Id)
		require.NoError(t, err)
	}

	// Delete the VM
	deleteReq := &providerv1.DeleteRequest{
		Id: createResp.Id,
	}

	deleteResp, err := provider.Delete(ctx, deleteReq)
	require.NoError(t, err)

	// Wait for deletion task to complete if there is one
	if deleteResp.Task != nil {
		err = waitForTask(ctx, provider, deleteResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM no longer exists
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: createResp.Id})
	require.NoError(t, err)
	assert.False(t, describeResp.Exists)
}

func TestProxmoxProvider_GetCapabilities(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()
	resp, err := provider.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	assert.True(t, resp.SupportsSnapshots)
	assert.True(t, resp.SupportsLinkedClones)
	assert.True(t, resp.SupportsImageImport)
	assert.Contains(t, resp.SupportedDiskTypes, "raw")
	assert.Contains(t, resp.SupportedDiskTypes, "qcow2")
	assert.Contains(t, resp.SupportedNetworkTypes, "bridge")
	// Disk export/import are now advertised to match the implemented RPCs (#198).
	assert.True(t, resp.SupportsDiskExport, "Proxmox implements ExportDisk")
	assert.True(t, resp.SupportsDiskImport, "Proxmox implements ImportDisk")
	assert.Contains(t, resp.SupportedExportFormats, "qcow2")
	assert.Contains(t, resp.SupportedImportFormats, "vmdk")
	// ExportDisk honors req.Compress via a forced qemu-img convert pass with
	// `-c` for qcow2 targets (#219), so compression is advertised.
	assert.True(t, resp.SupportsExportCompression, "Proxmox ExportDisk compresses qcow2 targets when Compress=true (#219)")

	// ADR-0006: Proxmox advertises BOTH the legacy pvc path and the S3 relay
	// data path on export and import; nfs/direct remain unimplemented.
	assert.Equal(t, []string{"pvc", "s3"}, resp.SupportedExportBackends,
		"Proxmox advertises pvc + s3 export backends (ADR-0006)")
	assert.Equal(t, []string{"pvc", "s3"}, resp.SupportedImportBackends,
		"Proxmox advertises pvc + s3 import backends (ADR-0006)")
	assert.Equal(t, []string{"relay"}, resp.SupportedTransferModes,
		"both pvc and s3 paths are relay-shaped (bytes flow node ↔ pod ↔ backend)")
}

func TestExportNeedsConversion(t *testing.T) {
	tests := []struct {
		name         string
		sourceFormat string
		targetFormat string
		compress     bool
		want         bool
	}{
		{
			name:         "qcow2 to qcow2 without compression skips pass",
			sourceFormat: "qcow2",
			targetFormat: "qcow2",
			compress:     false,
			want:         false,
		},
		{
			name:         "qcow2 to qcow2 with compression forces a pass",
			sourceFormat: "qcow2",
			targetFormat: "qcow2",
			compress:     true,
			want:         true,
		},
		{
			name:         "format change always needs a pass",
			sourceFormat: "raw",
			targetFormat: "qcow2",
			compress:     false,
			want:         true,
		},
		{
			name:         "format change with compression needs a pass",
			sourceFormat: "vmdk",
			targetFormat: "qcow2",
			compress:     true,
			want:         true,
		},
		{
			name:         "raw to raw with compression does not force a pass (raw is not compressible)",
			sourceFormat: "raw",
			targetFormat: "raw",
			compress:     true,
			want:         false,
		},
		{
			name:         "vmdk to vmdk with compression does not force a pass (vmdk compression not produced here)",
			sourceFormat: "vmdk",
			targetFormat: "vmdk",
			compress:     true,
			want:         false,
		},
		{
			name:         "raw to raw without compression skips pass",
			sourceFormat: "raw",
			targetFormat: "raw",
			compress:     false,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exportNeedsConversion(tt.sourceFormat, tt.targetFormat, tt.compress)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Helper functions

func createTestProvider(endpoint string) *Provider {
	config := &pveapi.Config{
		Endpoint:           endpoint,
		TokenID:            "test@pve!token",
		TokenSecret:        "secret",
		InsecureSkipVerify: true,
	}

	client, err := pveapi.NewClient(config)
	if err != nil {
		panic(err)
	}

	provider := New()
	provider.client = client

	return provider
}

func TestProxmoxProvider_Reconfigure(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use existing test VM (ID 100 from fake server seed data)
	vmID := "100"

	// Test CPU reconfiguration
	reconfigReq := &providerv1.ReconfigureRequest{
		Id:          vmID,
		DesiredJson: `{"class": {"cpus": 4, "memory": "8Gi"}}`,
	}

	reconfigResp, err := provider.Reconfigure(ctx, reconfigReq)
	require.NoError(t, err)

	// Wait for reconfigure task to complete if there is one
	if reconfigResp.Task != nil {
		err = waitForTask(ctx, provider, reconfigResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify changes were applied
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: vmID})
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
}

func TestProxmoxProvider_Snapshots(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use existing test VM (ID 100 from fake server seed data)
	vmID := "100"

	// Test snapshot creation
	snapCreateReq := &providerv1.SnapshotCreateRequest{
		VmId:          vmID,
		NameHint:      "test-snapshot",
		Description:   "Test snapshot for integration tests",
		IncludeMemory: true,
	}

	snapCreateResp, err := provider.SnapshotCreate(ctx, snapCreateReq)
	require.NoError(t, err)
	require.NotEmpty(t, snapCreateResp.SnapshotId)

	// Wait for snapshot creation task
	if snapCreateResp.Task != nil {
		err = waitForTask(ctx, provider, snapCreateResp.Task.Id)
		require.NoError(t, err)
	}

	// Test snapshot revert
	snapRevertReq := &providerv1.SnapshotRevertRequest{
		VmId:       vmID,
		SnapshotId: snapCreateResp.SnapshotId,
	}

	snapRevertResp, err := provider.SnapshotRevert(ctx, snapRevertReq)
	require.NoError(t, err)

	// Wait for revert task
	if snapRevertResp.Task != nil {
		err = waitForTask(ctx, provider, snapRevertResp.Task.Id)
		require.NoError(t, err)
	}

	// Test snapshot deletion
	snapDeleteReq := &providerv1.SnapshotDeleteRequest{
		VmId:       vmID,
		SnapshotId: snapCreateResp.SnapshotId,
	}

	snapDeleteResp, err := provider.SnapshotDelete(ctx, snapDeleteReq)
	require.NoError(t, err)

	// Wait for deletion task
	if snapDeleteResp.Task != nil {
		err = waitForTask(ctx, provider, snapDeleteResp.Task.Id)
		require.NoError(t, err)
	}
}

// imagePrepareGRPCCode extracts the gRPC status code from an ImagePrepare error,
// which the provider returns as an *errors.ProviderError (a gRPC status).
func imagePrepareGRPCCode(t *testing.T, err error) codes.Code {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected a gRPC status error, got %T: %v", err, err)
	return st.Code()
}

// TestProxmoxProvider_ImagePrepare_ExistingTemplateByName verifies that a
// source.proxmox.templateName referencing the seeded template is a no-op success
// (verify-only, no import, no task).
func TestProxmoxProvider_ImagePrepare_ExistingTemplateByName(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	// ubuntu-22-template (VMID 9000, template=1) is seeded by the fake server.
	resp, err := provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"proxmox":{"templateName":"ubuntu-22-template"}}}`,
		TargetName: "ubuntu-22-template",
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Task, "verify-only must not return a task")
	assert.Equal(t, "ubuntu-22-template", resp.GetPreparedImageId(),
		"verify-only reports the existing template name as the prepared id (#214)")
	assert.Empty(t, resp.GetPreparedImagePath(), "Proxmox addresses templates by name/VMID, not path")
	assert.Nil(t, server.LastDownloadRequest(), "verify-only must not trigger a download")
}

// TestProxmoxProvider_ImagePrepare_ExistingTemplateByID verifies that a
// source.proxmox.templateID referencing the seeded template is a no-op success.
func TestProxmoxProvider_ImagePrepare_ExistingTemplateByID(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	resp, err := provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"proxmox":{"templateID":9000}}}`,
		TargetName: "ubuntu-22-template",
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Task)
	assert.Equal(t, "9000", resp.GetPreparedImageId(),
		"verify-only by VMID reports the template VMID as the prepared id (#214)")
	assert.Nil(t, server.LastDownloadRequest())
}

// TestProxmoxProvider_ImagePrepare_MissingTemplateName verifies that referencing a
// non-existent template by name returns an honest NotFound rather than a
// fabricated success.
func TestProxmoxProvider_ImagePrepare_MissingTemplateName(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"proxmox":{"templateName":"does-not-exist"}}}`,
		TargetName: "does-not-exist",
	})
	assert.Equal(t, codes.NotFound, imagePrepareGRPCCode(t, err))
}

// TestProxmoxProvider_ImagePrepare_MissingTemplateID verifies that referencing a
// non-existent template by VMID returns NotFound.
func TestProxmoxProvider_ImagePrepare_MissingTemplateID(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"proxmox":{"templateID":424242}}}`,
		TargetName: "missing",
	})
	assert.Equal(t, codes.NotFound, imagePrepareGRPCCode(t, err))
}

// TestProxmoxProvider_ImagePrepare_ImportFromURL verifies the source.http.url
// import path issues the expected PVE download-url call with the target_name,
// resolved storage, and content=import propagated.
func TestProxmoxProvider_ImagePrepare_ImportFromURL(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	const imgURL = "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
	resp, err := provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:   `{"source":{"http":{"url":"` + imgURL + `"}}}`,
		TargetName:  "jammy-base",
		StorageHint: "local-lvm",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Task, "import must return a task to poll")
	// Async-location-at-trigger (#214): the prepared template name is deterministic
	// and returned in the SAME response as the task ref, before the task completes.
	assert.Equal(t, "jammy-base", resp.GetPreparedImageId(),
		"async import reports the prepared template id alongside the task ref")
	assert.Empty(t, resp.GetPreparedImagePath())

	dl := server.LastDownloadRequest()
	require.NotNil(t, dl, "import must POST a download-url request")
	assert.Equal(t, "pve", dl.Node)
	assert.Equal(t, "local-lvm", dl.Storage)
	assert.Equal(t, "import", dl.Content, "cloud image is imported as a disk, not an ISO")
	assert.Equal(t, "jammy-base.qcow2", dl.Filename, "filename derives from target_name + format")
	assert.Equal(t, imgURL, dl.URL)

	err = waitForTask(ctx, provider, resp.Task.Id)
	require.NoError(t, err)
}

// TestProxmoxProvider_ImagePrepare_StoragePrecedence verifies storage resolution:
// the request StorageHint wins over source.proxmox.storage, which wins over the
// local-lvm default.
func TestProxmoxProvider_ImagePrepare_StoragePrecedence(t *testing.T) {
	ctx := context.Background()
	const imgURL = "https://images.example.com/base.img"

	t.Run("hint wins over source storage", func(t *testing.T) {
		server, endpoint, err := pvefake.StartFakeServer()
		require.NoError(t, err)
		provider := createTestProvider(endpoint)

		_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
			ImageJson:   `{"source":{"proxmox":{"storage":"src-store"},"http":{"url":"` + imgURL + `"}}}`,
			TargetName:  "p1",
			StorageHint: "hint-store",
		})
		require.NoError(t, err)
		require.NotNil(t, server.LastDownloadRequest())
		assert.Equal(t, "hint-store", server.LastDownloadRequest().Storage)
	})

	t.Run("source storage wins over default", func(t *testing.T) {
		server, endpoint, err := pvefake.StartFakeServer()
		require.NoError(t, err)
		provider := createTestProvider(endpoint)

		_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
			ImageJson:  `{"source":{"proxmox":{"storage":"src-store"},"http":{"url":"` + imgURL + `"}}}`,
			TargetName: "p2",
		})
		require.NoError(t, err)
		require.NotNil(t, server.LastDownloadRequest())
		assert.Equal(t, "src-store", server.LastDownloadRequest().Storage)
	})

	t.Run("default when neither set", func(t *testing.T) {
		server, endpoint, err := pvefake.StartFakeServer()
		require.NoError(t, err)
		provider := createTestProvider(endpoint)

		_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
			ImageJson:  `{"source":{"http":{"url":"` + imgURL + `"}}}`,
			TargetName: "p3",
		})
		require.NoError(t, err)
		require.NotNil(t, server.LastDownloadRequest())
		assert.Equal(t, "local-lvm", server.LastDownloadRequest().Storage)
	})
}

// TestProxmoxProvider_ImagePrepare_ImportRequiresTargetName verifies the import
// path refuses to fabricate a name from the URL when target_name is empty.
func TestProxmoxProvider_ImagePrepare_ImportRequiresTargetName(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	_, err = provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"http":{"url":"https://images.example.com/base.img"}}}`,
		TargetName: "",
	})
	assert.Equal(t, codes.InvalidArgument, imagePrepareGRPCCode(t, err))
	assert.Nil(t, server.LastDownloadRequest(), "no download without a target name")
}

// TestProxmoxProvider_ImagePrepare_EmptySource verifies that an empty or
// source-less spec yields InvalidSpec rather than a fabricated success.
func TestProxmoxProvider_ImagePrepare_EmptySource(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	for name, imageJSON := range map[string]string{
		"empty json":       ``,
		"empty object":     `{}`,
		"empty source":     `{"source":{}}`,
		"unrelated source": `{"source":{"registry":{"image":"foo:bar"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
				ImageJson:  imageJSON,
				TargetName: "whatever",
			})
			assert.Equal(t, codes.InvalidArgument, imagePrepareGRPCCode(t, err))
		})
	}
	assert.Nil(t, server.LastDownloadRequest())
}

// TestProxmoxProvider_ImagePrepare_Idempotent verifies that importing to a
// target_name that already exists as a template is a no-op success (no download).
func TestProxmoxProvider_ImagePrepare_Idempotent(t *testing.T) {
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	// ubuntu-22-template already exists as a template in the seed data; importing
	// to that name must skip the download entirely.
	resp, err := provider.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"http":{"url":"https://images.example.com/base.img"}}}`,
		TargetName: "ubuntu-22-template",
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Task, "idempotent no-op must not return a task")
	assert.Nil(t, server.LastDownloadRequest(), "idempotent no-op must not download")
}

func TestProxmoxProvider_MultiNIC(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Test VM creation with multiple network interfaces
	createReq := &providerv1.CreateRequest{
		Name:      "test-vm-multi-nic",
		ClassJson: `{"cpus": 2, "memory": "4Gi"}`,
		ImageJson: `{"source": "ubuntu-22-template"}`,
		NetworksJson: `[
			{
				"name": "lan",
				"static_ip": {
					"address": "192.168.1.100/24",
					"gateway": "192.168.1.1",
					"dns": ["8.8.8.8", "1.1.1.1"]
				}
			},
			{
				"name": "dmz",
				"vlan": 100
			},
			{
				"name": "vmbr2",
				"mac": "02:00:00:aa:bb:cc"
			}
		]`,
		UserData: []byte(`#cloud-config
hostname: test-vm-multi-nic
users:
  - name: ubuntu
    ssh_authorized_keys:
      - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... test@example.com"
`),
	}

	createResp, err := provider.Create(ctx, createReq)
	require.NoError(t, err)
	require.NotEmpty(t, createResp.Id)

	// Wait for creation task to complete
	if createResp.Task != nil {
		err = waitForTask(ctx, provider, createResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM was created with multiple NICs
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: createResp.Id})
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
}

func TestProxmoxProvider_UpdatedCapabilities(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()
	resp, err := provider.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	assert.True(t, resp.SupportsSnapshots)
	assert.True(t, resp.SupportsLinkedClones)
	assert.True(t, resp.SupportsImageImport)
	assert.True(t, resp.SupportsReconfigureOnline) // Enhanced for hot-plug
	assert.True(t, resp.SupportsDiskExpansionOnline)

	// Verify disk and network types
	assert.Contains(t, resp.SupportedDiskTypes, "raw")
	assert.Contains(t, resp.SupportedDiskTypes, "qcow2")
	assert.Contains(t, resp.SupportedNetworkTypes, "bridge")
	assert.Contains(t, resp.SupportedNetworkTypes, "vlan")
}

func waitForTask(ctx context.Context, provider *Provider, taskID string) error {
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return context.DeadlineExceeded
		case <-ticker.C:
			resp, err := provider.TaskStatus(ctx, &providerv1.TaskStatusRequest{
				Task: &providerv1.TaskRef{Id: taskID},
			})
			if err != nil {
				return err
			}
			if resp.Done {
				if resp.Error != "" {
					return fmt.Errorf("task failed: %s", resp.Error)
				}
				return nil
			}
		}
	}
}
