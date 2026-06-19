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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// vmConfigSize reads back the cores/memory(MiB) a VM actually has on the fake PVE.
func vmConfigSize(t *testing.T, p *Provider, id string) (cores int, memMiB int) {
	t.Helper()
	vmid, node, err := p.parseVMReference(id)
	require.NoError(t, err)
	cfg, err := p.client.GetVMConfig(context.Background(), node, vmid)
	require.NoError(t, err)
	if c, ok := cfg["cores"].(float64); ok {
		cores = int(c)
	}
	if m, ok := cfg["memory"].(float64); ok {
		memMiB = int(m)
	}
	return cores, memMiB
}

// TestProxmoxProvider_CreateHonorsVMClassSizing is the #261 P1-1 fix: Create
// parses the VMClass CPU/MemoryMiB (the marshaled contracts.VMClass keys) rather
// than the non-existent "cpus"/"memory" keys, so the VM is sized to the class
// instead of falling back to the PVE default of 1 core / 512 MB.
func TestProxmoxProvider_CreateHonorsVMClassSizing(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "sized-create",
		ClassJson: `{"CPU": 4, "MemoryMiB": 8192}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	cores, memMiB := vmConfigSize(t, provider, createResp.Id)
	assert.Equal(t, 4, cores, "Create must apply VMClass CPU")
	assert.Equal(t, 8192, memMiB, "Create must apply VMClass MemoryMiB")
}

// TestProxmoxProvider_CloneHonorsVMClassSizing covers the template-clone path
// (distinct from the diskless create): a PVE clone inherits the template's
// cores/memory, so the provider must reconfigure the clone to the VMClass size —
// and that must happen for a plain VM, not only when cloud-init is present. The
// fake's seeded template (vmid 100) is 2 CPU / 2048 MiB; the class asks for 4 /
// 8192, so inheriting the template would leave 2 / 2048.
func TestProxmoxProvider_CloneHonorsVMClassSizing(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "sized-clone",
		ClassJson: `{"CPU": 4, "MemoryMiB": 8192}`,
		ImageJson: `{"TemplateName": "100"}`, // clone the seeded VM (2 CPU / 2048 MiB)
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	cores, memMiB := vmConfigSize(t, provider, createResp.Id)
	assert.Equal(t, 4, cores, "clone must apply the VMClass CPU, not inherit the template's")
	assert.Equal(t, 8192, memMiB, "clone must apply the VMClass MemoryMiB, not inherit the template's")
}

// TestProxmoxProvider_ReconfigureHonorsVMClassSizing verifies the Reconfigure
// half of P1-1: the nested class is under "Class" with "CPU"/"MemoryMiB".
func TestProxmoxProvider_ReconfigureHonorsVMClassSizing(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "sized-reconfig",
		ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	// DesiredJson is a marshaled contracts.CreateRequest → {"Class":{"CPU":..,"MemoryMiB":..}}.
	_, err = provider.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          createResp.Id,
		DesiredJson: `{"Name":"sized-reconfig","Class":{"CPU":4,"MemoryMiB":8192}}`,
	})
	require.NoError(t, err)

	cores, memMiB := vmConfigSize(t, provider, createResp.Id)
	assert.Equal(t, 4, cores, "Reconfigure must apply the new CPU")
	assert.Equal(t, 8192, memMiB, "Reconfigure must apply the new MemoryMiB")
}

// TestExportDisk_DirectModeRejected verifies #261 P1-3: Proxmox advertises
// relay-only transfer, so a `direct` request must fail loudly instead of silently
// running as relay.
func TestExportDisk_DirectModeRejected(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId:         "100",
		BackendType:  "s3",
		TransferMode: "direct",
	})
	assert.Equal(t, codes.InvalidArgument, s3GRPCCode(t, err),
		"ExportDisk must reject the unimplemented direct transfer mode")
}

// TestImportDisk_DirectModeRejected mirrors the export rejection for import.
func TestImportDisk_DirectModeRejected(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	_, err = provider.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		BackendType:  "s3",
		TransferMode: "direct",
	})
	assert.Equal(t, codes.InvalidArgument, s3GRPCCode(t, err),
		"ImportDisk must reject the unimplemented direct transfer mode")
}
