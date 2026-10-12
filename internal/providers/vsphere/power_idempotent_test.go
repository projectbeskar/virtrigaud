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
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestPower_IdempotentInRequestedState: Power is idempotent (v0.4.0-rc1 F3).
// vCenter answers InvalidPowerState to a power-off of a powered-off VM and a
// power-on of a running one — also when another actor won a race to the same
// change — and the provider answers success when the VM is already in the
// requested state.
func TestPower_IdempotentInRequestedState(t *testing.T) {
	ctx := context.Background()
	p, _ := newOwnershipSim(t)
	dc, err := p.finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	p.finder.SetDatacenter(dc)
	vm, err := p.finder.VirtualMachine(ctx, simUnstamped)
	require.NoError(t, err)
	id := vm.Reference().Value
	if runtimePowerState(t, p, id) != types.VirtualMachinePowerStatePoweredOn {
		task, err := vm.PowerOn(ctx)
		require.NoError(t, err)
		require.NoError(t, task.Wait(ctx))
	}

	power := func(op providerv1.PowerOp) error {
		_, err := p.Power(ctx, &providerv1.PowerRequest{Id: id, Op: op})
		return err
	}

	// The precondition the fix answers: vCenter refuses a second power-on.
	task, err := vm.PowerOn(ctx)
	require.NoError(t, err)
	require.Error(t, task.Wait(ctx), "vCenter fails a power-on of a running VM (InvalidPowerState)")

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_ON), "power-on of a running VM succeeds")
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, runtimePowerState(t, p, id))

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_OFF))
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, runtimePowerState(t, p, id))

	// The precondition the fix answers: vCenter refuses a second power-off.
	task, err = object.NewVirtualMachine(p.client.Client, vmRef(id)).PowerOff(ctx)
	require.NoError(t, err)
	require.Error(t, task.Wait(ctx), "vCenter fails a power-off of a powered-off VM (InvalidPowerState)")

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_OFF), "power-off of a powered-off VM succeeds")
	require.NoError(t, power(providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL), "a graceful shutdown of a powered-off VM succeeds")
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, runtimePowerState(t, p, id))

	require.NoError(t, power(providerv1.PowerOp_POWER_OP_ON))
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, runtimePowerState(t, p, id))
}
