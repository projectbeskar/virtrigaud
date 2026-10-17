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

func TestReconfigure_RunningVMWithoutHotAdd_RefusedBeforeAnyChange(t *testing.T) {
	p, vm := hotPlugVM(t, false, false, false)
	id := vm.Reference().Value
	before := diskCapacitiesOf(t, vm)

	err := reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 4, MemoryMiB: 4096, DiskDefaults: &contracts.DiskDefaults{SizeGiB: int32(before[0]>>30) + 5}}, // #nosec G115 -- a simulator disk size
	})
	requireNeedsPowerOff(t, err, "CPU hot-add is not enabled on it; memory hot-add is not enabled on it")
	cpu, mem := size(t, vm)
	assert.Equal(t, int32(2), cpu)
	assert.Equal(t, int32(2048), mem)
	assert.Equal(t, before, diskCapacitiesOf(t, vm), "the disk grow in the same request is not applied either")

	// Unchanged CPU/memory: a disk grow alone goes ahead on the running VM.
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 2, MemoryMiB: 2048, DiskDefaults: &contracts.DiskDefaults{SizeGiB: int32(before[0]>>30) + 5}}, // #nosec G115 -- a simulator disk size
	}))
	assert.Equal(t, before[0]+5<<30, diskCapacitiesOf(t, vm)[0])

	// Powered off, the same change is applied.
	task, err := vm.PowerOff(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{Class: contracts.VMClass{CPU: 4, MemoryMiB: 4096}}))
	cpu, mem = size(t, vm)
	assert.Equal(t, int32(4), cpu)
	assert.Equal(t, int32(4096), mem)
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

func TestReconfigure_SuspendedVMIsTreatedAsRunning(t *testing.T) {
	p, vm := hotPlugVM(t, false, false, false)
	task, err := vm.Suspend(context.Background())
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))
	requireNeedsPowerOff(t, reconfigure(t, p, vm.Reference().Value, contracts.CreateRequest{Class: contracts.VMClass{CPU: 4}}),
		"CPU hot-add is not enabled on it")
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
