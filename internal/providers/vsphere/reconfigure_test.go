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

package vsphere

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/vim25/mo"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestReconfigure_AppliesVMClassCPUMemory is the #266 fix: Reconfigure parses the
// marshaled contracts.CreateRequest keys (Class.CPU / Class.MemoryMiB), not the
// non-existent lowercase cpus/memory. The old code read the wrong keys, so a
// VMClass resize silently no-opped while the RPC reported success — leaving the
// hardware unchanged. This drives a real ReconfigVM_Task against the govmomi
// simulator and asserts the VM's NumCPU/MemoryMB actually change. Marshaling a
// real contracts.CreateRequest (not a hand-built map) is essential: a map with
// lowercase keys would mask the bug.
func TestReconfigure_AppliesVMClassCPUMemory(t *testing.T) {
	cfg, cleanup := newSimConfig(t)
	defer cleanup()

	client, finder, err := createVSphereClient(cfg)
	require.NoError(t, err)
	defer func() { _ = client.Logout(context.Background()) }()

	p := &Provider{client: client, finder: finder, config: cfg, logger: slog.Default()}
	ctx := context.Background()

	dc, err := finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	finder.SetDatacenter(dc)

	vms, err := finder.VirtualMachineList(ctx, "*")
	require.NoError(t, err)
	require.NotEmpty(t, vms, "simulator should seed at least one VM")
	vm := vms[0]
	moid := vm.Reference().Value

	// Power off first so the reconfigure is unconditionally allowed (no hot-add
	// assumptions) and the test is deterministic.
	if task, perr := vm.PowerOff(ctx); perr == nil {
		_, _ = task.WaitForResult(ctx, nil)
	}

	// Confirm the starting size differs from what we'll request, so a passing
	// assertion can only mean the reconfigure took effect.
	var before mo.VirtualMachine
	require.NoError(t, vm.Properties(ctx, vm.Reference(),
		[]string{"config.hardware.numCPU", "config.hardware.memoryMB"}, &before))
	require.NotEqual(t, int32(4), before.Config.Hardware.NumCPU)
	require.NotEqual(t, int32(8192), before.Config.Hardware.MemoryMB)

	// DesiredJson is a marshaled contracts.CreateRequest — exactly what the manager
	// sends. VMClass has no json tags, so the keys are Class.CPU / Class.MemoryMiB.
	desired, err := json.Marshal(contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 4, MemoryMiB: 8192},
	})
	require.NoError(t, err)

	_, err = p.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          moid,
		DesiredJson: string(desired),
	})
	require.NoError(t, err)

	var after mo.VirtualMachine
	require.NoError(t, vm.Properties(ctx, vm.Reference(),
		[]string{"config.hardware.numCPU", "config.hardware.memoryMB"}, &after))
	assert.Equal(t, int32(4), after.Config.Hardware.NumCPU, "Reconfigure must apply VMClass CPU")
	assert.Equal(t, int32(8192), after.Config.Hardware.MemoryMB, "Reconfigure must apply VMClass MemoryMiB")
}
