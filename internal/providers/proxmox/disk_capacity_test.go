/*
Copyright 2026.

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

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestConfigDiskCapacities(t *testing.T) {
	p := &Provider{}
	got := p.configDiskCapacities(map[string]interface{}{
		"scsi0":    "local-lvm:vm-100-disk-0,size=32G",
		"virtio1":  "local-lvm:vm-100-disk-1,iothread=1,size=1T",
		"ide2":     "local:iso/installer.iso,media=cdrom",
		"ide3":     "local-lvm:vm-100-cloudinit,media=cdrom",
		"sata0":    "local-lvm:vm-100-disk-2",
		"unused0":  "local-lvm:vm-100-disk-9,size=8T",
		"efidisk0": "local-lvm:vm-100-disk-3,size=4M",
		"memory":   float64(2048),
	})
	require.Len(t, got, 3, "attached bus disks only: no media, no unused or EFI volumes")
	assert.Equal(t, "sata0", got[0].DiskId)
	assert.Zero(t, got[0].VirtualSizeBytes, "a disk without a size is reported as unknown")
	assert.Equal(t, "scsi0", got[1].DiskId)
	assert.Equal(t, int64(32<<30), got[1].VirtualSizeBytes)
	assert.Equal(t, "virtio1", got[2].DiskId)
	assert.Equal(t, int64(1<<40), got[2].VirtualSizeBytes)
}

// TestProxmoxProvider_GetDiskInfoListsDisksAndWithheldSizeIsNoOp: GetDiskInfo
// lists the VM's disks with their sizes (the fake seeds scsi0 at 32G), and a
// Reconfigure whose disk size the manager withheld (SizeGiB 0) resizes
// nothing and fails nothing.
func TestProxmoxProvider_GetDiskInfoListsDisksAndWithheldSizeIsNoOp(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "disk-cap",
		ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	info, err := provider.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{VmId: createResp.Id})
	require.NoError(t, err)
	require.NotEmpty(t, info.Disks)
	assert.Equal(t, "scsi0", info.Disks[0].DiskId)
	assert.Equal(t, int64(32<<30), info.Disks[0].VirtualSizeBytes)

	resp, err := provider.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          createResp.Id,
		DesiredJson: `{"Disks":[{"SizeGiB":0,"Type":"","Name":"data"}]}`,
	})
	require.NoError(t, err, "a withheld size is not a shrink")
	assert.Nil(t, resp.Task, "a withheld size resizes nothing")
}
