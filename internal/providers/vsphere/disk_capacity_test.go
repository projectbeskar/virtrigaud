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

package vsphere

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestVMDiskCapacities(t *testing.T) {
	devices := []types.BaseVirtualDevice{
		&types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: 2000,
			Backing: &types.VirtualDiskFlatVer2BackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds] web/web.vmdk"}}},
			CapacityInBytes: 40 << 30},
		&types.VirtualCdrom{VirtualDevice: types.VirtualDevice{Key: 3000}},
		&types.VirtualDisk{VirtualDevice: types.VirtualDevice{Key: 2001}, CapacityInKB: 800 << 20},
	}
	got := vmDiskCapacities(devices)
	require.Len(t, got, 2, "the CD-ROM is not a disk")
	assert.Equal(t, "2000", got[0].DiskId)
	assert.Equal(t, "[ds] web/web.vmdk", got[0].Path)
	assert.Equal(t, int64(40<<30), got[0].VirtualSizeBytes)
	assert.Equal(t, int64(800<<30), got[1].VirtualSizeBytes, "CapacityInKB when CapacityInBytes is unset")
}

// simVM returns a provider on a fresh simulator and its first VM, powered off.
func simVM(t *testing.T) (*Provider, *object.VirtualMachine) {
	t.Helper()
	cfg, cleanup := newSimConfig(t)
	t.Cleanup(cleanup)
	client, err := createVSphereClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Logout(context.Background()) })
	ctx := context.Background()
	p := &Provider{client: client, config: cfg, logger: slog.Default()}
	vms, err := testFinder(t, p).VirtualMachineList(ctx, "*")
	require.NoError(t, err)
	require.NotEmpty(t, vms)
	vm := vms[0]
	if task, perr := vm.PowerOff(ctx); perr == nil {
		_, _ = task.WaitForResult(ctx, nil)
	}
	return p, vm
}

// diskCapacitiesOf returns the capacity in bytes of every disk of vm.
func diskCapacitiesOf(t *testing.T, vm *object.VirtualMachine) []int64 {
	t.Helper()
	var m mo.VirtualMachine
	require.NoError(t, vm.Properties(context.Background(), vm.Reference(), []string{"config.hardware.device"}, &m))
	var out []int64
	for _, d := range m.Config.Hardware.Device {
		if disk, ok := d.(*types.VirtualDisk); ok {
			out = append(out, disk.CapacityInKB*1024)
		}
	}
	return out
}

// TestGetDiskInfo_ListsEveryDisk: GetDiskInfo reports every disk of the VM
// with its capacity (the manager's disk-size limit check before a clone).
func TestGetDiskInfo_ListsEveryDisk(t *testing.T) {
	p, vm := simVM(t)
	resp, err := p.GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: vm.Reference().Value})
	require.NoError(t, err)
	want := diskCapacitiesOf(t, vm)
	require.NotEmpty(t, want)
	require.Len(t, resp.Disks, len(want))
	for i, d := range resp.Disks {
		assert.Equal(t, want[i], d.VirtualSizeBytes)
		assert.Positive(t, d.VirtualSizeBytes)
	}
}

// TestReconfigure_WithheldDiskSizeIsNoOp: the manager withholds a disk growth
// beyond the disk-size limit by sending its size as 0. The disk is not
// touched while the CPU change is applied; a positive size still grows it
// (the resize path is reachable).
func TestReconfigure_WithheldDiskSizeIsNoOp(t *testing.T) {
	p, vm := simVM(t)
	ctx := context.Background()
	before := diskCapacitiesOf(t, vm)
	require.NotEmpty(t, before)

	desired, err := json.Marshal(contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 4, MemoryMiB: 8192, DiskDefaults: &contracts.DiskDefaults{Type: "thin", SizeGiB: 0}},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 0}},
	})
	require.NoError(t, err)
	_, err = p.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: vm.Reference().Value, DesiredJson: string(desired)})
	require.NoError(t, err)
	assert.Equal(t, before, diskCapacitiesOf(t, vm), "a withheld size never resizes a disk")
	var m mo.VirtualMachine
	require.NoError(t, vm.Properties(ctx, vm.Reference(), []string{"config.hardware.numCPU"}, &m))
	assert.Equal(t, int32(4), m.Config.Hardware.NumCPU, "the CPU change is applied")

	grown := int32(before[0]>>30) + 10 // #nosec G115 -- a simulator disk size
	// A spec.disks size is never the root disk's (this VM has no additional
	// disk attached for "data"): before the fix it grew the first disk.
	desired, err = json.Marshal(contracts.CreateRequest{Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: grown}}})
	require.NoError(t, err)
	_, err = p.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: vm.Reference().Value, DesiredJson: string(desired)})
	require.NoError(t, err)
	assert.Equal(t, before, diskCapacitiesOf(t, vm), "a spec.disks size never grows the root disk")

	// Nor is the VMClass size: Reconfigure never touches the root disk
	// (Create applies the class size).
	desired, err = json.Marshal(contracts.CreateRequest{Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{SizeGiB: grown}}})
	require.NoError(t, err)
	_, err = p.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: vm.Reference().Value, DesiredJson: string(desired)})
	require.NoError(t, err)
	assert.Equal(t, before, diskCapacitiesOf(t, vm), "the VMClass size never grows the root disk on Reconfigure")
}
