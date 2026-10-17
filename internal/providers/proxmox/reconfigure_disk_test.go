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
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Reconfigure changes no disk (reconfigure.go): no spec.disks size (the
// Proxmox Create attaches none) and not the VMClass size. Before this fix
// every spec.disks size went to scsi0 — the root disk — and a disk grow
// returned before the CPU/memory change was sent. Validated against pvefake
// only, not a real PVE.

// twoDiskVM creates a VM on a fake PVE and gives it the disks scsi0 (32G, the
// boot disk) and scsi1 (10G, a template's data disk).
func twoDiskVM(t *testing.T) (*Provider, *pvefake.Server, string, int) {
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
	provider, server, id, vmid := twoDiskVM(t)

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

// TestProxmoxReconfigure_ClassSizeChangesNoDisk: the VMClass disk size is not
// applied by Reconfigure (Create applies it, which the Proxmox Create does not
// do yet): no disk is resized, and the CPU/memory change is applied.
func TestProxmoxReconfigure_ClassSizeChangesNoDisk(t *testing.T) {
	provider, server, id, vmid := twoDiskVM(t)

	require.NoError(t, reconfigureTo(t, provider, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 4, MemoryMiB: 4096, DiskDefaults: &contracts.DiskDefaults{SizeGiB: 64}},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 64}},
	}))
	assert.Zero(t, resizes(server))
	vm, _ := server.GetVM(vmid)
	assert.Contains(t, vm.Config["scsi0"], "size=32G")
	assert.Contains(t, vm.Config["scsi1"], "size=10G")
	assert.Equal(t, 4, vm.CPUs)
	assert.Equal(t, int64(4096)<<20, vm.Memory)
}

// TestProxmoxReconfigure_ConfigFailureStaysOutOfTheBreaker: a CPU/memory
// config update PVE fails is answered with VM_OPERATION_FAILED and a constant
// message, which the manager never counts toward the Provider's circuit
// breaker.
func TestProxmoxReconfigure_ConfigFailureStaysOutOfTheBreaker(t *testing.T) {
	provider, server, id, _ := twoDiskVM(t)
	server.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/qemu/" + id + "/config",
		Status: http.StatusInternalServerError, Body: `{"data":null,"message":"secret-node hotplug problem"}`})

	err := reconfigureTo(t, provider, id, contracts.CreateRequest{Class: contracts.VMClass{CPU: 4}})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Equal(t, reconfigureFailedMessage, st.Message())
	var reasons []string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
		}
	}
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, reasons)
}
