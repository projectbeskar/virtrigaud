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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the fix for the bug where a VirtualMachine's
// spec.resources CPU/memory override was compared against and recorded in
// status.currentResources (needsReconfigure, updateCurrentResources) but
// never actually sent to the provider (buildCreateRequest used only the
// VMClass's own CPU/memory) — so status could claim a size the VM never had.
// effectiveResources (vmclass_quantity.go) is now the single place that
// computes the VM's effective CPU/memory, used by all three.

// ─── effectiveResources ────────────────────────────────────────────────────

func TestEffectiveResources(t *testing.T) {
	baseClass := func() *infravirtrigaudiov1beta1.VMClass {
		return &infravirtrigaudiov1beta1.VMClass{
			Spec: infravirtrigaudiov1beta1.VMClassSpec{
				CPU:    4,
				Memory: resource.MustParse("8Gi"), // 8192 MiB
			},
		}
	}

	cases := []struct {
		name        string
		resources   *infravirtrigaudiov1beta1.VirtualMachineResources
		wantCPU     int32
		wantMemMiB  int32
		wantErr     bool
		errContains string
	}{
		{
			name:       "no override uses VMClass values",
			resources:  nil,
			wantCPU:    4,
			wantMemMiB: 8192,
		},
		{
			name:       "CPU override only",
			resources:  &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(8)},
			wantCPU:    8,
			wantMemMiB: 8192,
		},
		{
			name:       "memory override only",
			resources:  &infravirtrigaudiov1beta1.VirtualMachineResources{MemoryMiB: i64p(2048)},
			wantCPU:    4,
			wantMemMiB: 2048,
		},
		{
			name:       "both CPU and memory overridden",
			resources:  &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(16), MemoryMiB: i64p(32768)},
			wantCPU:    16,
			wantMemMiB: 32768,
		},
		{
			name:        "CPU override below the VMClass minimum is refused",
			resources:   &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(0)},
			wantErr:     true,
			errContains: "spec.resources.cpu",
		},
		{
			name:        "CPU override above the VMClass maximum is refused",
			resources:   &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(129)},
			wantErr:     true,
			errContains: "spec.resources.cpu",
		},
		{
			name:        "memory override below the minimum is refused",
			resources:   &infravirtrigaudiov1beta1.VirtualMachineResources{MemoryMiB: i64p(0)},
			wantErr:     true,
			errContains: "spec.resources.memoryMiB",
		},
		{
			name:        "memory override at the VMClass maximum is refused (exclusive bound)",
			resources:   &infravirtrigaudiov1beta1.VirtualMachineResources{MemoryMiB: i64p(maxVMClassMemoryBytes / bytesPerMiB)},
			wantErr:     true,
			errContains: "spec.resources.memoryMiB",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := &infravirtrigaudiov1beta1.VirtualMachine{
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{Resources: tc.resources},
			}
			cpu, memMiB, err := effectiveResources(vm, baseClass())
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, contracts.IsInvalidSpec(err), "expected an InvalidSpec error, got: %v", err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantCPU, cpu)
			assert.Equal(t, tc.wantMemMiB, memMiB)
		})
	}
}

// ─── buildCreateRequest ─────────────────────────────────────────────────────

func TestBuildCreateRequest_ResourcesOverride_AppliedToRequest(t *testing.T) {
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{
		CPU:       i32p(8),
		MemoryMiB: i64p(2048),
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, nil, vmClass, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(8), req.Class.CPU, "the override CPU must reach the provider request")
	assert.Equal(t, int32(2048), req.Class.MemoryMiB, "the override memory must reach the provider request")
}

func TestBuildCreateRequest_ResourcesOverride_Invalid_ReturnsInvalidSpecError(t *testing.T) {
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(500)}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}

	_, err := r.buildCreateRequest(context.Background(), vm, nil, vmClass, nil, nil)
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err))
}

// ─── createVM: single-host ──────────────────────────────────────────────────

// TestCreateVM_ResourcesOverride_SentToProviderAndRecorded proves Create
// honours a spec.resources override end to end: the provider receives the
// overridden CPU (memory untouched, falling back to the VMClass), and status
// records the same values Create was actually sent with — not the VMClass's
// own, unoverridden values.
func TestCreateVM_ResourcesOverride_SentToProviderAndRecorded(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-override", clusteredNS, "prov-single")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(8)}
	prov := &routingProvider{}
	providerCR := withRuntime(singleProviderCR("prov-single", clusteredNS))
	class := smallVMClass(clusteredNS) // CPU: 2, Memory: 4Gi (4096 MiB)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR, class, minimalVMImage(clusteredNS))

	_, err := r.createVM(ctx, getVM(t, r, "vm-override"), prov, providerCR, class, minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)

	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, int32(8), prov.createReqs[0].Class.CPU, "the CPU override must be sent to the provider")
	assert.Equal(t, int32(4096), prov.createReqs[0].Class.MemoryMiB, "memory falls back to the VMClass when not overridden")

	after := getVM(t, r, "vm-override")
	require.NotNil(t, after.Status.CurrentResources)
	require.NotNil(t, after.Status.CurrentResources.CPU)
	assert.Equal(t, int32(8), *after.Status.CurrentResources.CPU, "status must record the applied (overridden) size, not the VMClass size")
	require.NotNil(t, after.Status.CurrentResources.MemoryMiB)
	assert.Equal(t, int64(4096), *after.Status.CurrentResources.MemoryMiB)
}

// TestCreateVM_InvalidResourcesOverride_RefusedWithoutProviderCall proves an
// out-of-bounds override never reaches Create and surfaces as a clear
// Provisioning=False/ValidationError condition instead.
func TestCreateVM_InvalidResourcesOverride_RefusedWithoutProviderCall(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-bad-override", clusteredNS, "prov-single")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(0)}
	prov := &routingProvider{}
	providerCR := withRuntime(singleProviderCR("prov-single", clusteredNS))
	class := smallVMClass(clusteredNS)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR, class, minimalVMImage(clusteredNS))

	res, err := r.createVM(ctx, getVM(t, r, "vm-bad-override"), prov, providerCR, class, minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)
	assert.Empty(t, prov.createReqs, "an invalid override must never reach the provider")
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, res.RequeueAfter)
	assert.Equal(t, k8s.ReasonValidationError, provisioningReason(getVM(t, r, "vm-bad-override")))
}

// ─── createVM: clustered ────────────────────────────────────────────────────

// TestCreateVM_Clustered_ResourcesOverride_SentToProviderAndRecorded proves
// the override reaches Create's TargetHostID-carrying request the same way on
// a clustered (topology: cluster) provider, so a future capacity/footprint
// computation (ADR-0007) reading req.Class sees the real size.
func TestCreateVM_Clustered_ResourcesOverride_SentToProviderAndRecorded(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-cluster-override", clusteredNS, "prov-cluster")
	vm.UID = "uid-cluster-override"
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{MemoryMiB: i64p(16384)}
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm)

	_, err := r.createVM(ctx, getVM(t, r, "vm-cluster-override"), prov, clusteredProviderCR("prov-cluster", clusteredNS), smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)

	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, int32(2), prov.createReqs[0].Class.CPU, "CPU falls back to the VMClass when not overridden")
	assert.Equal(t, int32(16384), prov.createReqs[0].Class.MemoryMiB, "the memory override must be sent to the provider")
	assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID, "clustered scheduling is unaffected by the override fix")

	after := getVM(t, r, "vm-cluster-override")
	require.NotNil(t, after.Status.CurrentResources)
	require.NotNil(t, after.Status.CurrentResources.MemoryMiB)
	assert.Equal(t, int64(16384), *after.Status.CurrentResources.MemoryMiB)
}

// ─── needsReconfigure ───────────────────────────────────────────────────────

func TestNeedsReconfigure_OverrideRemoved_RevertsToVMClassSize(t *testing.T) {
	r := &VirtualMachineReconciler{}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}
	// Status still reflects a previously-applied override (CPU=8); the
	// override has since been removed from spec, so the VM must fall back to
	// the VMClass's own CPU (4) — a mismatch that needs a reconfigure.
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{Resources: nil},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
			CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
				CPU:       i32p(8),
				MemoryMiB: i64p(8192),
			},
		},
	}

	needs, err := r.needsReconfigure(vm, vmClass)
	require.NoError(t, err)
	assert.True(t, needs, "removing the override must revert the VM to the VMClass size")
}

func TestNeedsReconfigure_InvalidOverride_ReturnsError(t *testing.T) {
	r := &VirtualMachineReconciler{}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(0)},
		},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
			CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
				CPU:       i32p(4),
				MemoryMiB: i64p(8192),
			},
		},
	}

	needs, err := r.needsReconfigure(vm, vmClass)
	assert.False(t, needs)
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err))
}

// TestReconcileVM_InvalidResourcesOverride_RefusesWithoutReconfigure proves
// the full reconcile path never calls Reconfigure for an out-of-bounds
// override, and instead sets a Reconfiguring=False/ValidationError condition.
func TestReconcileVM_InvalidResourcesOverride_RefusesWithoutReconfigure(t *testing.T) {
	var reconfigureCalled bool
	prov := &fakeDescribeProvider{
		stubProvider: stubProvider{
			ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
				reconfigureCalled = true
				return "", nil
			},
		},
		DescribeFn: func(_ context.Context, _ string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.1"}}, nil
		},
	}
	s := coverageTestScheme(t)
	k8sProv, class := providerAndClass("default")
	resolver := &stubResolver{provider: prov}
	r := newTestReconciler(s, resolver, k8sProv, class)

	vm := baseVM("default")
	vm.Status.ID = "vm-xyz"
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(0)}
	cpu := class.Spec.CPU
	memMiB := class.Spec.Memory.Value() / (1024 * 1024)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &memMiB}

	result, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.False(t, reconfigureCalled, "an invalid override must never reach Reconfigure")
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, result.RequeueAfter)

	found := false
	for _, c := range vm.Status.Conditions {
		if c.Type == "Reconfiguring" {
			found = true
			assert.Equal(t, "False", string(c.Status))
			assert.Equal(t, k8s.ReasonValidationError, c.Reason)
		}
	}
	assert.True(t, found, "expected a Reconfiguring=False/ValidationError condition")
}

// ─── reconfigureVM ──────────────────────────────────────────────────────────

// TestReconfigureVM_ResourcesOverride_SentToProviderAndRecorded proves a
// synchronous Reconfigure carries the override and records the applied
// (overridden) size in status.currentResources.
func TestReconfigureVM_ResourcesOverride_SentToProviderAndRecorded(t *testing.T) {
	ctx := context.Background()
	vm := baseVM("default")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(8)}
	vm.Status.ID = "vm-123"
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}
	var gotReq contracts.CreateRequest
	provider := &stubProvider{
		ReconfigureFn: func(_ context.Context, _ string, desired contracts.CreateRequest) (string, error) {
			gotReq = desired
			return "", nil // synchronous completion
		},
	}
	r := newTestReconciler(coverageTestScheme(t), nil, vm)

	result, err := r.reconfigureVM(ctx, vm, provider, contracts.VMRef{ID: vm.Status.ID}, nil, vmClass, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)

	assert.Equal(t, int32(8), gotReq.Class.CPU, "the provider must receive the overridden CPU")

	require.NotNil(t, vm.Status.CurrentResources)
	require.NotNil(t, vm.Status.CurrentResources.CPU)
	assert.Equal(t, int32(8), *vm.Status.CurrentResources.CPU, "status must record the applied (overridden) CPU")
}

// TestReconfigureVM_ProviderError_LeavesCurrentResourcesUnchanged proves
// status.currentResources is never advanced to an unapplied desired value: a
// failed Reconfigure call must not move it.
func TestReconfigureVM_ProviderError_LeavesCurrentResourcesUnchanged(t *testing.T) {
	ctx := context.Background()
	origCPU := int32(4)
	origMem := int64(8192)
	vm := baseVM("default")
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(8)}
	vm.Status.ID = "vm-123"
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{
		CPU:       &origCPU,
		MemoryMiB: &origMem,
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}
	provider := &stubProvider{
		ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
			return "", assertErr("provider unavailable")
		},
	}
	r := newTestReconciler(coverageTestScheme(t), nil, vm)

	_, err := r.reconfigureVM(ctx, vm, provider, contracts.VMRef{ID: vm.Status.ID}, nil, vmClass, nil, nil)
	require.NoError(t, err) // the controller absorbs provider errors into conditions

	require.NotNil(t, vm.Status.CurrentResources)
	require.NotNil(t, vm.Status.CurrentResources.CPU)
	assert.Equal(t, origCPU, *vm.Status.CurrentResources.CPU, "a failed Reconfigure must not advance status.currentResources")
	assert.Equal(t, origMem, *vm.Status.CurrentResources.MemoryMiB)
}

// ─── updateCurrentResources ─────────────────────────────────────────────────

func TestUpdateCurrentResources_ReflectsOverride_NotVMClass(t *testing.T) {
	r := &VirtualMachineReconciler{}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{MemoryMiB: i64p(2048)},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}

	r.updateCurrentResources(vm, vmClass)

	require.NotNil(t, vm.Status.CurrentResources)
	require.NotNil(t, vm.Status.CurrentResources.CPU)
	assert.Equal(t, int32(4), *vm.Status.CurrentResources.CPU, "CPU falls back to the VMClass when not overridden")
	require.NotNil(t, vm.Status.CurrentResources.MemoryMiB)
	assert.Equal(t, int64(2048), *vm.Status.CurrentResources.MemoryMiB, "memory must record the override, not the VMClass value")
}

func TestUpdateCurrentResources_InvalidOverride_LeavesPreviousValueUnchanged(t *testing.T) {
	r := &VirtualMachineReconciler{}
	origCPU := int32(4)
	origMem := int64(8192)
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(0)},
		},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
			CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &origCPU, MemoryMiB: &origMem},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}

	r.updateCurrentResources(vm, vmClass)

	require.NotNil(t, vm.Status.CurrentResources)
	require.NotNil(t, vm.Status.CurrentResources.CPU)
	assert.Equal(t, origCPU, *vm.Status.CurrentResources.CPU, "an unrecordable effective value must never overwrite the previous one")
}

// ─── small local helpers ────────────────────────────────────────────────────

// assertErr is a trivial error type for provider-failure test fixtures.
type assertErr string

func (e assertErr) Error() string { return string(e) }
