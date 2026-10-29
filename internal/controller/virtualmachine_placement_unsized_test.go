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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// A placed VM whose VMClass cannot be resolved (deleted, or no longer granted
// to its namespace) is never counted at a guessed size: it counts from its
// status records alone, conservatively, or the accounting pass fails closed.

// unsizedFixtureVM is a VM of prov-cluster in capNS whose class is classRef.
func unsizedFixtureVM(name string, classRef infravirtrigaudiov1beta1.ObjectRef, mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := capVM(name)
	vm.Spec.ClassRef = classRef
	mutate(vm)
	return vm
}

func TestSizeForAccounting_UnresolvedClass(t *testing.T) {
	missing := infravirtrigaudiov1beta1.ObjectRef{Name: "deleted-class"}
	ungranted := infravirtrigaudiov1beta1.ObjectRef{Name: "test-class", Namespace: "infra"}
	present := infravirtrigaudiov1beta1.ObjectRef{Name: "test-class"}
	bound := func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.ID = vm.Name
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}
	}
	current := func(cpu int32, mem int64) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			bound(vm)
			vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(cpu), MemoryMiB: i64p(mem)}
		}
	}
	pending := func(res *infravirtrigaudiov1beta1.PlacementResources, ceiling *int64) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha",
				PendingResources: res, MemoryCeilingMiB: ceiling}
		}
	}
	withCeiling := func(f func(*infravirtrigaudiov1beta1.VirtualMachine), ceiling int64) func(*infravirtrigaudiov1beta1.VirtualMachine) {
		return func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			f(vm)
			vm.Status.Placement.MemoryCeilingMiB = i64p(ceiling)
		}
	}

	cases := []struct {
		name  string
		vm    *infravirtrigaudiov1beta1.VirtualMachine
		want  scheduler.ResourceRequest
		unset bool // errUnsizedPlacement
	}{
		{name: "legacy bound VM, no recorded size, class deleted", vm: unsizedFixtureVM("legacy-missing", missing, bound), unset: true},
		{name: "legacy bound VM, no recorded size, class no longer granted", vm: unsizedFixtureVM("legacy-ungranted", ungranted, bound), unset: true},
		{
			name: "bound VM with only its CPU recorded, class deleted",
			vm: unsizedFixtureVM("partial", missing, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				bound(vm)
				vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(8)}
			}),
			unset: true,
		},
		{name: "pending create from an older manager, no pendingResources, class deleted", vm: unsizedFixtureVM("pending-legacy", missing, pending(nil, nil)), unset: true},
		{
			name: "legacy bound VM, no recorded size, class present: the class size, as before",
			vm:   unsizedFixtureVM("legacy-present", present, bound),
			want: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096},
		},
		{
			name: "bound VM with recorded size and ceiling, class deleted: exactly the record",
			vm:   unsizedFixtureVM("recorded", missing, withCeiling(current(8, 16384), 0)),
			want: scheduler.ResourceRequest{CPU: 8, MemoryMiB: 16384},
		},
		{
			name: "bound VM with recorded size and no recorded ceiling, class deleted: counted at the hot-add ceiling",
			vm:   unsizedFixtureVM("no-ceiling", missing, current(8, 16384)),
			want: scheduler.ResourceRequest{CPU: 8, MemoryMiB: contracts.HotplugCeilingMemoryMiB(16384)},
		},
		{
			name: "bound VM with recorded size and no recorded ceiling, class no longer granted: counted at the hot-add ceiling",
			vm:   unsizedFixtureVM("no-ceiling-ungranted", ungranted, current(4, 2048)),
			want: scheduler.ResourceRequest{CPU: 4, MemoryMiB: contracts.HotplugCeilingMemoryMiB(2048)},
		},
		{
			name: "pending create (or clone target) with its admitted size, class deleted: the admitted size",
			vm: unsizedFixtureVM("pending", missing,
				pending(&infravirtrigaudiov1beta1.PlacementResources{CPU: 6, MemoryMiB: 12288}, i64p(0))),
			want: scheduler.ResourceRequest{CPU: 6, MemoryMiB: 12288},
		},
		{
			name: "a durable resize reservation still raises a VM sized from its record",
			vm: unsizedFixtureVM("reserved", missing, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				withCeiling(current(2, 4096), 0)(vm)
				vm.Status.Placement.ResizeReservation = &infravirtrigaudiov1beta1.PlacementResources{CPU: 4, MemoryMiB: 8192}
			}),
			want: scheduler.ResourceRequest{CPU: 4, MemoryMiB: 8192},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).
				WithObjects(smallVMClass(capNS), smallVMClass("infra"), tc.vm).Build()
			got, err := sizeForAccounting(context.Background(), c, tc.vm, classMemo{})
			if tc.unset {
				require.ErrorIs(t, err, errUnsizedPlacement)
				assert.Contains(t, err.Error(), tc.vm.Name, "the error names the VM for the operator")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCommittedOnHost_UnsizedVMFailsClosed: the Host gauges are not published
// from a sum that would under-count an unsized VM (publishCommitted keeps the
// last value on an error).
func TestCommittedOnHost_UnsizedVMFailsClosed(t *testing.T) {
	legacy := unsizedFixtureVM("legacy", infravirtrigaudiov1beta1.ObjectRef{Name: "deleted-class"},
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = vm.Name
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}
		})
	c := indexedClient(t, legacy)
	_, _, err := committedOnHost(context.Background(), c, types.NamespacedName{Namespace: capNS, Name: "prov-cluster"}, "host-alpha")
	require.ErrorIs(t, err, errUnsizedPlacement)

	// Another host of the same Provider is unaffected.
	cpu, mem, err := committedOnHost(context.Background(), c, types.NamespacedName{Namespace: capNS, Name: "prov-cluster"}, "host-beta")
	require.NoError(t, err)
	assert.Zero(t, cpu)
	assert.Zero(t, mem)
}

// TestClusteredResize_UnsizedNeighbourFailsClosed: a resize-up is not admitted
// against a host whose committed capacity cannot be established; the bound
// VM's own lifecycle is not otherwise held.
func TestClusteredResize_UnsizedNeighbourFailsClosed(t *testing.T) {
	legacy := unsizedFixtureVM("legacy", infravirtrigaudiov1beta1.ObjectRef{Name: "deleted-class"},
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = vm.Name
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}
		})
	vm := withPlacement(capVM("grower"), "host-alpha", "")
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(4096)}
	vm.Status.Placement.MemoryCeilingMiB = i64p(0)
	objs := append(capBase(), capHost("host-alpha", 64), legacy, vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

	locked, _, err := r.checkResizeUnderLock(context.Background(), r.placementAssumptions(),
		types.NamespacedName{Namespace: capNS, Name: "prov-cluster"}, vm,
		scheduler.ResizeRequest{Host: *capHost("host-alpha", 64), VMUID: vmSchedulingUID(vm),
			Current: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096}, Desired: scheduler.ResourceRequest{CPU: 4, MemoryMiB: 4096}})
	require.True(t, locked)
	var infraErr *placementInfraError
	require.ErrorAs(t, err, &infraErr, "an unsized neighbour is an accounting failure, not a capacity verdict")
	require.ErrorIs(t, err, errUnsizedPlacement)
	_, _, ok := r.placementAssumptions().Lookup(vmSchedulingUID(vm))
	assert.False(t, ok, "nothing is assumed for a resize that was not admitted")
}
