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

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestProxmoxProvider_CreateUsesClusterNextID is the #261 P2-2 fix: VMIDs come
// from PVE's /cluster/nextid (collision-free) instead of a truncated wall-clock
// timestamp. The fake hands out ids from 200000 upward, skipping the seeded
// template (vmid 100), so the first allocation is a deterministic 200001 — a
// value the legacy time-based scheme would essentially never produce.
func TestProxmoxProvider_CreateUsesClusterNextID(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "nextid-create",
		ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	vmid, _, err := provider.parseVMReference(createResp.Id)
	require.NoError(t, err)
	assert.Equal(t, 200001, vmid, "VMID must be allocated from /cluster/nextid, not a wall-clock timestamp")
}

// TestProxmoxProvider_GetDiskInfoReportsRealValues is the #261 P2-1 fix:
// GetDiskInfo reads the disk's real size/used/format from the storage content
// API rather than guessing from the config string. The config advertises a
// 32 GiB disk with no format hint (which the old code defaulted to "qcow2");
// the storage volume reports a 20 GiB provisioned / 5 GiB thin-used raw disk.
// Distinct numbers prove the values came from the storage API, not the config.
func TestProxmoxProvider_GetDiskInfoReportsRealValues(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()

	createResp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "diskinfo-real",
		ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`,
	})
	require.NoError(t, err)
	if createResp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, createResp.Task.Id))
	}

	info, err := provider.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{VmId: createResp.Id})
	require.NoError(t, err)

	const (
		gib            = int64(1024 * 1024 * 1024)
		wantVirtual    = 20 * gib // from the storage volume, not the config's 32 GiB
		wantActualUsed = 5 * gib  // thin usage, distinct from virtual size
	)
	assert.Equal(t, "raw", info.Format, "format must come from the storage volume, not the qcow2 guess")
	assert.Equal(t, wantVirtual, info.VirtualSizeBytes, "virtual size must come from the storage volume")
	assert.Equal(t, wantActualUsed, info.ActualSizeBytes, "actual size must reflect thin usage from the storage volume")
}
