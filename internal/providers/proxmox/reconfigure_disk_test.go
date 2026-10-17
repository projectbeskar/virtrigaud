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
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Reconfigure sends a size only to the disk it is for (reconfigure.go): the
// VMClass size to the primary disk, and no spec.disks size to any disk (the
// Proxmox Create attaches none). Before this fix every spec.disks size went to
// scsi0 — the root disk — and a disk grow returned before the CPU/memory
// change was sent. Validated against pvefake only, not a real PVE.

// twoDiskVM creates a VM on a fake PVE and gives it the disks scsi0 (32G, the
// boot disk) and scsi1 (10G, a template's data disk).
func twoDiskVM(t *testing.T, extra map[string]string) (*Provider, *pvefake.Server, string, int) {
	t.Helper()
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)
	ctx := context.Background()
	resp, err := provider.Create(ctx, &providerv1.CreateRequest{Name: "disks", ClassJson: `{"CPU": 1, "MemoryMiB": 1024}`})
	require.NoError(t, err)
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
	}
	vmid, err := strconv.Atoi(resp.Id)
	require.NoError(t, err)
	vm, ok := server.GetVM(vmid)
	require.True(t, ok)
	vm.Config = map[string]string{
		"scsi0": "local-lvm:vm-" + resp.Id + "-disk-0,size=32G",
		"scsi1": "local-lvm:vm-" + resp.Id + "-disk-1,size=10G",
		"ide2":  "local-lvm:vm-" + resp.Id + "-cloudinit,media=cdrom",
		"boot":  "order=scsi0;ide2;net0",
	}
	for k, v := range extra {
		vm.Config[k] = v
	}
	server.PutVM(vm)
	return provider, server, resp.Id, vmid
}

// reconfigureTo sends desired to VM id and waits for the task it returns.
func reconfigureTo(t *testing.T, provider *Provider, id string, desired contracts.CreateRequest) error {
	t.Helper()
	body, err := json.Marshal(desired)
	require.NoError(t, err)
	ctx := context.Background()
	resp, err := provider.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: id, DesiredJson: string(body)})
	if err != nil {
		return err
	}
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
	}
	return nil
}

// resizes counts the resize requests the fake received.
func resizes(server *pvefake.Server) int {
	n := 0
	for _, r := range server.MutatingRequests() {
		if strings.HasSuffix(r.Path, "/resize") {
			n++
		}
	}
	return n
}

// TestProxmoxReconfigure_SpecDisksResizeNoDisk is the regression test for the
// wrong-disk resize: spec.disks sizes reach neither scsi0 (the root disk) nor
// scsi1, while the CPU/memory change in the same request is applied.
func TestProxmoxReconfigure_SpecDisksResizeNoDisk(t *testing.T) {
	provider, server, id, vmid := twoDiskVM(t, nil)

	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 2, MemoryMiB: 2048},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 64}, {Name: "logs", SizeGiB: 128}},
	}))
	assert.Zero(t, resizes(server), "no spec.disks size is sent to any disk")
	vm, _ := server.GetVM(vmid)
	assert.Contains(t, vm.Config["scsi0"], "size=32G")
	assert.Contains(t, vm.Config["scsi1"], "size=10G")
	assert.Equal(t, 2, vm.CPUs)
	assert.Equal(t, int64(2048)<<20, vm.Memory)

	// A spec.disks size smaller than scsi0 is no shrink request either (it
	// used to be refused as one).
	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 8}},
	}))
	assert.Zero(t, resizes(server))
}

// TestProxmoxReconfigure_ClassSizeGrowsPrimaryDiskThenCPUMemory: the VMClass
// size grows the primary disk only, and the CPU/memory change in the same
// request is applied too (a grow used to return before sending it).
func TestProxmoxReconfigure_ClassSizeGrowsPrimaryDiskThenCPUMemory(t *testing.T) {
	provider, server, id, vmid := twoDiskVM(t, nil)

	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 4, MemoryMiB: 4096, DiskDefaults: &contracts.DiskDefaults{SizeGiB: 64}},
	}))
	assert.Equal(t, 1, resizes(server))
	vm, _ := server.GetVM(vmid)
	assert.Contains(t, vm.Config["scsi0"], "size=64G", "the primary disk is grown")
	assert.Contains(t, vm.Config["scsi1"], "size=10G", "another disk is not")
	assert.Equal(t, 4, vm.CPUs, "the CPU change is applied after the grow")
	assert.Equal(t, int64(4096)<<20, vm.Memory)

	// The VMClass size is a floor: equal or smaller sends no resize, no error.
	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{SizeGiB: 64}},
	}))
	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{SizeGiB: 8}},
	}))
	assert.Equal(t, 1, resizes(server))
}

// TestProxmoxReconfigure_PrimaryDiskFollowsBootOrder: the primary disk is the
// first disk of the boot order, not always scsi0.
func TestProxmoxReconfigure_PrimaryDiskFollowsBootOrder(t *testing.T) {
	provider, server, id, vmid := twoDiskVM(t, map[string]string{"boot": "order=ide2;scsi1;scsi0"})
	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{SizeGiB: 20}},
	}))
	vm, _ := server.GetVM(vmid)
	assert.Contains(t, vm.Config["scsi1"], "size=20G", "the boot disk is grown (the CD-ROM is skipped)")
	assert.Contains(t, vm.Config["scsi0"], "size=32G")
}

func TestPVEPrimaryDisk(t *testing.T) {
	for name, tc := range map[string]struct {
		config map[string]any
		want   string
	}{
		"boot order":            {map[string]any{"boot": "order=virtio1;net0", "scsi0": "a:1,size=1G", "virtio1": "a:2,size=1G"}, "virtio1"},
		"legacy boot string":    {map[string]any{"boot": "cdn", "scsi0": "a:1,size=1G"}, "scsi0"},
		"no boot: bus order":    {map[string]any{"scsi0": "a:1,size=1G", "virtio0": "a:2,size=1G"}, "virtio0"},
		"cd-rom and cloud-init": {map[string]any{"ide2": "a:cloudinit,media=cdrom", "sata0": "a:vm-1-cloudinit"}, ""},
		"unused is not a disk":  {map[string]any{"unused0": "a:3", "ide0": "a:4,size=2G"}, "ide0"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, pvePrimaryDisk(tc.config))
		})
	}
}
