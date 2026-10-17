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
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A CPU/memory change of a running vSphere VM is applied only when vCenter can
// apply it online — CPU hot-add/hot-remove, memory hot-add — and otherwise
// refused before anything is changed with RECONFIGURE_NEEDS_POWER_OFF (rc1 lab
// finding V1: vCenter refused it and the manager retried forever).

// hotPlugVM returns the simulator's first VM with the given hot-plug settings,
// at 2 vCPUs / 2048 MiB, powered on.
func hotPlugVM(t *testing.T, cpuAdd, cpuRemove, memAdd bool) (*Provider, *object.VirtualMachine) {
	t.Helper()
	p, vm := simVM(t) // powered off
	ctx := context.Background()
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{
		NumCPUs:             2,
		MemoryMB:            2048,
		CpuHotAddEnabled:    types.NewBool(cpuAdd),
		CpuHotRemoveEnabled: types.NewBool(cpuRemove),
		MemoryHotAddEnabled: types.NewBool(memAdd),
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
	task, err = vm.PowerOn(ctx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
	return p, vm
}

// size reads the CPU count and memory (MiB) of vm.
func size(t *testing.T, vm *object.VirtualMachine) (int32, int32) {
	t.Helper()
	var m mo.VirtualMachine
	require.NoError(t, vm.Properties(context.Background(), vm.Reference(),
		[]string{"config.hardware.numCPU", "config.hardware.memoryMB"}, &m))
	return m.Config.Hardware.NumCPU, m.Config.Hardware.MemoryMB
}

// requireNeedsPowerOff asserts err is the RECONFIGURE_NEEDS_POWER_OFF refusal
// naming why.
func requireNeedsPowerOff(t *testing.T, err error, why string) {
	t.Helper()
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.ReconfigureNeedsPowerOffReason}, errorReasons(err))
	assert.Contains(t, err.Error(), why)
}

// powerOnVM powers VM id on.
func powerOnVM(t *testing.T, p *Provider, id string) *object.VirtualMachine {
	t.Helper()
	vm := object.NewVirtualMachine(p.client.Client, vmRef(id))
	task, err := vm.PowerOn(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	return vm
}

func TestReconfigure_RunningVMWithoutHotAdd_RefusedBeforeAnyChange(t *testing.T) {
	p, id, before := multiDiskVM(t) // no hot-add
	vm := powerOnVM(t, p, id)
	cpu0, mem0 := size(t, vm)

	err := reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: cpu0 + 1, MemoryMiB: mem0 + 1024},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}},
	})
	requireNeedsPowerOff(t, err, "CPU hot-add is not enabled on it; memory hot-add is not enabled on it")
	cpu, mem := size(t, vm)
	assert.Equal(t, cpu0, cpu)
	assert.Equal(t, mem0, mem)
	assert.Equal(t, before, capacities(t, p, id), "the disk grow in the same request is not applied either")

	// Unchanged CPU/memory: a disk grow alone goes ahead on the running VM.
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: cpu0, MemoryMiB: mem0},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}},
	}))
	assert.Equal(t, []int64{before[0], 3 * kibGiB, before[2]}, capacities(t, p, id))

	// Powered off, the CPU/memory change is applied.
	task, err := vm.PowerOff(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{Class: contracts.VMClass{CPU: cpu0 + 1, MemoryMiB: mem0 + 1024}}))
	cpu, mem = size(t, vm)
	assert.Equal(t, cpu0+1, cpu)
	assert.Equal(t, mem0+1024, mem)
}

func TestReconfigure_RunningVMWithHotAdd_GrowsOnline(t *testing.T) {
	p, vm := hotPlugVM(t, true, false, true)
	id := vm.Reference().Value

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{Class: contracts.VMClass{CPU: 4, MemoryMiB: 4096}}))
	cpu, mem := size(t, vm)
	assert.Equal(t, int32(4), cpu)
	assert.Equal(t, int32(4096), mem)

	// Fewer vCPUs need hot-remove; less memory is never taken online.
	requireNeedsPowerOff(t, reconfigure(t, p, id, contracts.CreateRequest{Class: contracts.VMClass{CPU: 2}}),
		"CPU hot-remove is not enabled on it")
	requireNeedsPowerOff(t, reconfigure(t, p, id, contracts.CreateRequest{Class: contracts.VMClass{MemoryMiB: 2048}}),
		"memory is never removed from a running VM")
	cpu, mem = size(t, vm)
	assert.Equal(t, int32(4), cpu)
	assert.Equal(t, int32(4096), mem)
}

func TestReconfigure_RunningVMWithHotRemove_ShrinksCPUOnline(t *testing.T) {
	p, vm := hotPlugVM(t, false, true, false)
	require.NoError(t, reconfigure(t, p, vm.Reference().Value, contracts.CreateRequest{Class: contracts.VMClass{CPU: 1}}))
	cpu, _ := size(t, vm)
	assert.Equal(t, int32(1), cpu)
}

// TestReconfigure_SuspendedVMTakesNoChange: vCenter changes no hardware of a
// suspended VM, hot-add or not, disks included.
func TestReconfigure_SuspendedVMTakesNoChange(t *testing.T) {
	p, vm := hotPlugVM(t, true, true, true)
	task, err := vm.Suspend(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	requireNeedsPowerOff(t, reconfigure(t, p, vm.Reference().Value, contracts.CreateRequest{Class: contracts.VMClass{CPU: 4}}),
		suspendedVMReason)

	p2, id, before := multiDiskVM(t)
	vm2 := powerOnVM(t, p2, id)
	task, err = vm2.Suspend(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	requireNeedsPowerOff(t, reconfigure(t, p2, id, contracts.CreateRequest{Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}}}),
		suspendedVMReason)
	assert.Equal(t, before, capacities(t, p2, id))
}

// TestReconfigure_CPUNotMultipleOfCoresPerSocketIsRefused: vCenter refuses a
// CPU count that is not a multiple of the cores per socket; it is refused
// first, before anything changes.
func TestReconfigure_CPUNotMultipleOfCoresPerSocketIsRefused(t *testing.T) {
	p, vm := simVM(t) // powered off
	ctx := context.Background()
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: 4, NumCoresPerSocket: 2})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))

	err = reconfigure(t, p, vm.Reference().Value, contracts.CreateRequest{Class: contracts.VMClass{CPU: 3, MemoryMiB: 4096}})
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "3 vCPUs is not a multiple of the VM's 2 cores per socket")
	cpu, _ := size(t, vm)
	assert.Equal(t, int32(4), cpu)

	require.NoError(t, reconfigure(t, p, vm.Reference().Value, contracts.CreateRequest{Class: contracts.VMClass{CPU: 6}}))
	cpu, _ = size(t, vm)
	assert.Equal(t, int32(6), cpu)
}

// diskVM is a synthetic VM read for planReconfigure: one recorded additional
// disk "data" (2 GiB) with backing, on a SCSI controller with sharedBus.
func diskVM(power types.VirtualMachinePowerState, backing types.BaseVirtualDeviceBackingInfo, sharedBus types.VirtualSCSISharing) *mo.VirtualMachine {
	unit := int32(1)
	return &mo.VirtualMachine{
		Runtime: types.VirtualMachineRuntimeInfo{PowerState: power},
		Config: &types.VirtualMachineConfigInfo{
			Hardware: types.VirtualHardware{
				NumCPU: 2, NumCoresPerSocket: 1, MemoryMB: 2048,
				Device: []types.BaseVirtualDevice{
					&types.ParaVirtualSCSIController{VirtualSCSIController: types.VirtualSCSIController{
						VirtualController: types.VirtualController{VirtualDevice: types.VirtualDevice{Key: 1000}},
						SharedBus:         sharedBus,
					}},
					&types.VirtualDisk{
						VirtualDevice: types.VirtualDevice{Key: 2001, ControllerKey: 1000, UnitNumber: &unit, Backing: backing},
						CapacityInKB:  2 * kibPerGiB,
					},
				},
			},
			ExtraConfig: []types.BaseOptionValue{
				&types.OptionValue{Key: additionalDiskExtraConfigKey(1), Value: "[ds] web/web-additional-1.vmdk"},
				&types.OptionValue{Key: additionalDiskNameKey(1), Value: "data"},
			},
		},
	}
}

func flatBacking(parent *types.VirtualDiskFlatVer2BackingInfo, sharing string) *types.VirtualDiskFlatVer2BackingInfo {
	return &types.VirtualDiskFlatVer2BackingInfo{
		VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds] web/web-additional-1.vmdk"},
		Parent:                       parent,
		Sharing:                      sharing,
	}
}

// TestPlanReconfigure_DiskPreChecks: a disk vCenter cannot extend (delta,
// physical-mode RDM) is left alone without an error; a disk on a shared SCSI
// bus or with multi-writer sharing is grown only while the VM is off.
func TestPlanReconfigure_DiskPreChecks(t *testing.T) {
	p := &Provider{logger: slog.Default()}
	grow := &reconfigureDesired{}
	grow.Disks = append(grow.Disks, struct {
		Name    string `json:"Name"`
		SizeGiB int32  `json:"SizeGiB"`
	}{Name: "data", SizeGiB: 5})
	on, off := types.VirtualMachinePowerStatePoweredOn, types.VirtualMachinePowerStatePoweredOff
	noSharing := types.VirtualSCSISharingNoSharing

	spec, err := p.planReconfigure("vm-1", diskVM(on, flatBacking(nil, ""), noSharing), grow)
	require.NoError(t, err)
	require.NotNil(t, spec, "a plain disk is grown on a running VM")
	require.Len(t, spec.DeviceChange, 1)

	for name, b := range map[string]types.BaseVirtualDeviceBackingInfo{
		"delta (snapshot or linked clone)": flatBacking(&types.VirtualDiskFlatVer2BackingInfo{}, ""),
		"physical-mode RDM": &types.VirtualDiskRawDiskMappingVer1BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "[ds] web/web-additional-1.vmdk"},
			CompatibilityMode:            string(types.VirtualDiskCompatibilityModePhysicalMode),
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec, err := p.planReconfigure("vm-1", diskVM(off, b, noSharing), grow)
			require.NoError(t, err, "left alone, not an error")
			assert.Nil(t, spec, "nothing to change")
		})
	}

	for name, vm := range map[string]func(types.VirtualMachinePowerState) *mo.VirtualMachine{
		"shared SCSI bus": func(s types.VirtualMachinePowerState) *mo.VirtualMachine {
			return diskVM(s, flatBacking(nil, ""), types.VirtualSCSISharingPhysicalSharing)
		},
		"multi-writer": func(s types.VirtualMachinePowerState) *mo.VirtualMachine {
			return diskVM(s, flatBacking(nil, string(types.VirtualDiskSharingSharingMultiWriter)), noSharing)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.planReconfigure("vm-1", vm(on), grow)
			requireNeedsPowerOff(t, err, "spec.disks[data]")
			spec, err := p.planReconfigure("vm-1", vm(off), grow)
			require.NoError(t, err)
			require.NotNil(t, spec, "grown while the VM is off")
		})
	}
}

// TestOnlineResizeBlockers pins the rules, including the hot-plug memory
// limit vcsim does not model.
func TestOnlineResizeBlockers(t *testing.T) {
	on := types.NewBool(true)
	cases := []struct {
		name           string
		cfg            types.VirtualMachineConfigInfo
		cpuFrom, cpuTo int32
		memFrom, memTo int64
		want           []string
	}{
		{name: "no change", cpuFrom: 2, cpuTo: 2, memFrom: 2048, memTo: 2048},
		{name: "CPU up without hot-add", cpuFrom: 2, cpuTo: 4, memFrom: 2048, memTo: 2048,
			want: []string{"CPU hot-add is not enabled on it"}},
		{name: "CPU up with hot-add", cfg: types.VirtualMachineConfigInfo{CpuHotAddEnabled: on}, cpuFrom: 2, cpuTo: 4, memFrom: 2048, memTo: 2048},
		{name: "CPU down with hot-add only", cfg: types.VirtualMachineConfigInfo{CpuHotAddEnabled: on}, cpuFrom: 4, cpuTo: 2, memFrom: 2048, memTo: 2048,
			want: []string{"CPU hot-remove is not enabled on it"}},
		{name: "memory up within the limit", cfg: types.VirtualMachineConfigInfo{MemoryHotAddEnabled: on, HotPlugMemoryLimit: 8192},
			cpuFrom: 2, cpuTo: 2, memFrom: 2048, memTo: 8192},
		{name: "memory up above the limit", cfg: types.VirtualMachineConfigInfo{MemoryHotAddEnabled: on, HotPlugMemoryLimit: 8192},
			cpuFrom: 2, cpuTo: 2, memFrom: 2048, memTo: 16384,
			want: []string{"16384 MiB is above its hot-plug memory limit of 8192 MiB"}},
		{name: "memory down", cfg: types.VirtualMachineConfigInfo{MemoryHotAddEnabled: on}, cpuFrom: 2, cpuTo: 2, memFrom: 4096, memTo: 2048,
			want: []string{"memory is never removed from a running VM"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			assert.Equal(t, tc.want, onlineResizeBlockers(&cfg, tc.cpuFrom, tc.cpuTo, tc.memFrom, tc.memTo))
		})
	}
}
