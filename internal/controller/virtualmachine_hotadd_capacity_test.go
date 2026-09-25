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
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// A VM whose VMClass enables memory hot-add is created with a balloon ceiling
// of contracts.HotplugCeilingMemoryMiB (4x its memory); its guest can deflate
// the balloon up to it, so it counts at that ceiling (review N1). CPU hot-add
// is not counted at its ceiling: vCPUs above the current count are offline.

// hotAddClass is smallVMClass (2 vCPU / 4096 MiB) with memory (and CPU)
// hot-add enabled.
func hotAddClass() *infravirtrigaudiov1beta1.VMClass {
	c := smallVMClass(capNS)
	c.Spec.PerformanceProfile = &infravirtrigaudiov1beta1.PerformanceProfile{MemoryHotAddEnabled: true, CPUHotAddEnabled: true}
	return c
}

// withMemory sets host's allocatable memory.
func withMemory(h *infravirtrigaudiov1beta1.Host, mib int64) *infravirtrigaudiov1beta1.Host {
	h.Status.AllocatableMemoryMiB = i64p(mib)
	return h
}

// withCeiling records a balloon ceiling on vm's placement.
func withCeiling(vm *infravirtrigaudiov1beta1.VirtualMachine, mib int64) *infravirtrigaudiov1beta1.VirtualMachine {
	vm.Status.Placement.MemoryCeilingMiB = i64p(mib)
	return vm
}

func TestHotAdd_CeilingMatchesTheLibvirtProvider(t *testing.T) {
	assert.Equal(t, int64(16384), contracts.HotplugCeilingMemoryMiB(4096))
	assert.Equal(t, int64(4<<20), contracts.HotplugCeilingMemoryMiB(1<<20), "memory has no cap")
	assert.Equal(t, int32(64), contracts.HotplugCeilingVCPUs(32), "vCPUs are capped")
	assert.Equal(t, int64(0), memoryCeilingFor(false, 4096), "no hot-add, no ceiling")
	assert.Equal(t, int64(16384), memoryCeilingFor(true, 4096))
}

func TestHotAdd_AdmittedFootprintCountsTheMemoryCeiling(t *testing.T) {
	bound := func() *infravirtrigaudiov1beta1.VirtualMachine { return sized("vm", 2) } // 2 vCPU / 4096 MiB recorded
	cases := []struct {
		name  string
		vm    *infravirtrigaudiov1beta1.VirtualMachine
		class *infravirtrigaudiov1beta1.VMClass
		want  scheduler.ResourceRequest
	}{
		{"recorded ceiling", withCeiling(bound(), 16384), hotAddClass(), scheduler.ResourceRequest{CPU: 2, MemoryMiB: 16384}},
		{"hot-add turned off since: the recorded ceiling still counts", withCeiling(bound(), 16384), smallVMClass(capNS),
			scheduler.ResourceRequest{CPU: 2, MemoryMiB: 16384}},
		{"created without hot-add: turning it on later adds nothing", withCeiling(bound(), 0), hotAddClass(),
			scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096}},
		{"nothing recorded (older manager, clone): the VMClass decides", bound(), hotAddClass(),
			scheduler.ResourceRequest{CPU: 2, MemoryMiB: 16384}},
		{"no hot-add", bound(), smallVMClass(capNS), scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, admittedFootprint(tc.vm, tc.class))
		})
	}
}

func TestHotAdd_CreateIsScheduledAtItsCeilingAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	providerCR := clusteredProviderCR("prov-cluster", capNS)

	// 12 GiB free: 4096 MiB fits, its 16384 MiB ceiling does not.
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), withMemory(capHost("host-alpha", 16), 12288), capVM("elastic"))...)
	prov := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "elastic"}}
	_, err := r.createVM(ctx, readVM(t, r, "elastic"), prov, providerCR, hotAddClass(), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	assert.Zero(t, prov.createCalls, "not placed where its balloon ceiling does not fit")
	stored := readVM(t, r, "elastic")
	assert.Equal(t, k8s.ReasonUnschedulable, placedCondition(stored).Reason)

	// 16 GiB free: placed, and the ceiling is recorded with the pending size.
	r = newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), withMemory(capHost("host-alpha", 16), 16384), capVM("elastic"))...)
	failing := &recordingCreateProvider{err: stderrors.New("transient")}
	_, err = r.createVM(ctx, readVM(t, r, "elastic"), failing, providerCR, hotAddClass(), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	require.Equal(t, 1, failing.createCalls)
	stored = readVM(t, r, "elastic")
	require.NotNil(t, stored.Status.Placement.MemoryCeilingMiB)
	assert.Equal(t, int64(16384), *stored.Status.Placement.MemoryCeilingMiB)
	assert.Equal(t, &infravirtrigaudiov1beta1.PlacementResources{CPU: 2, MemoryMiB: 4096}, stored.Status.Placement.PendingResources)
	assert.Equal(t, scheduler.ResourceRequest{CPU: 2, MemoryMiB: 16384}, admittedFootprint(stored, smallVMClass(capNS)),
		"while pending it counts at its ceiling, whatever its VMClass says now")

	// A plain VMClass records "none".
	r = newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), capHost("host-alpha", 16), capVM("plain"))...)
	_, err = r.createVM(ctx, readVM(t, r, "plain"), &recordingCreateProvider{err: stderrors.New("transient")},
		providerCR, smallVMClass(capNS), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	stored = readVM(t, r, "plain")
	require.NotNil(t, stored.Status.Placement.MemoryCeilingMiB)
	assert.Zero(t, *stored.Status.Placement.MemoryCeilingMiB)
}

func TestHotAdd_NeighbourCountsAtItsCeiling(t *testing.T) {
	// 20000 MiB: a 4096 MiB neighbour leaves room for a 4096 MiB VM, the same
	// neighbour with a 16384 MiB ceiling does not.
	neighbour := withCeiling(withPlacement(capVM("elastic"), "host-alpha", ""), 16384)
	neighbour.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(4096)}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), withMemory(capHost("host-alpha", 16), 20000), neighbour, capVM("newcomer"))...)
	host, _ := resolve(t, r, readVM(t, r, "newcomer"))
	assert.Empty(t, host, "the neighbour's balloon ceiling is committed")

	plain := withCeiling(withPlacement(capVM("elastic"), "host-alpha", ""), 0)
	plain.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(4096)}
	r = newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), withMemory(capHost("host-alpha", 16), 20000), plain, capVM("newcomer"))...)
	host, _ = resolve(t, r, readVM(t, r, "newcomer"))
	assert.Equal(t, "host-alpha", host)
}

// hotAddResizeFixture: host-alpha has 8 vCPU and exactly 20480 MiB, held by a
// plain 4096 MiB neighbour and app, a hot-add VM recorded at 4096 MiB with a
// 16384 MiB ceiling, whose spec asks for wantMiB.
func hotAddResizeFixture(t *testing.T, prov contracts.Provider, wantMiB int64) *VirtualMachineReconciler {
	t.Helper()
	app := withCeiling(sized("app", 2), 16384)
	app.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(wantMiB)}
	return newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov},
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), hostPoolCR("pool-a", capNS, "prov-cluster"),
		withMemory(capHost("host-alpha", 8), 20480), smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 2), app)
}

func TestHotAdd_LiveMemoryGrowWithinTheCeilingCommitsNothingNew(t *testing.T) {
	prov := runningRoutingProvider()
	r := hotAddResizeFixture(t, prov, 8192) // the host is full, but 8192 is within app's 16384 ceiling
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "admitted: it was already counted at its ceiling")
	assert.Equal(t, int64(8192), *getVM(t, r, "app").Status.CurrentResources.MemoryMiB)
	assert.Empty(t, assumedUIDs(r), "nothing new is committed, so nothing is assumed")
}

func TestHotAdd_GrowBeyondTheCeilingIsChecked(t *testing.T) {
	prov := runningRoutingProvider()
	r := hotAddResizeFixture(t, prov, 20480) // beyond the ceiling on a full host
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs, "refused: the host has no room above the ceiling")
	got := getVM(t, r, "app")
	assert.Equal(t, int64(4096), *got.Status.CurrentResources.MemoryMiB)
	c := reconfiguringCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonInsufficientHostCapacity, c.Reason)
	assert.Contains(t, c.Message, "resizing to 2 vCPU and 20480 MiB", "the message names sizes, not ceilings")
}

func TestHotAdd_PendingRetryThatGainedACeilingIsNotSent(t *testing.T) {
	ctx := context.Background()
	providerCR := clusteredProviderCR("prov-cluster", capNS)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
		append(capBase(), capHost("host-alpha", 16), capVM("toggled"))...)
	// Admitted and recorded without hot-add; the Create fails transiently.
	_, err := r.createVM(ctx, readVM(t, r, "toggled"), &recordingCreateProvider{err: stderrors.New("transient")},
		providerCR, smallVMClass(capNS), minimalVMImage(capNS), nil)
	require.NoError(t, err)

	// Its VMClass turns memory hot-add on: same size, larger ceiling.
	retry := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "toggled"}}
	res, err := r.createVM(ctx, readVM(t, r, "toggled"), retry, providerCR, hotAddClass(), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	assert.Zero(t, retry.createCalls)
	assert.Equal(t, placementConfigRetryInterval, res.RequeueAfter)
	c := placedCondition(readVM(t, r, "toggled"))
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonPendingSizeGrew, c.Reason)
	assert.Contains(t, c.Message, "with memory hot-add, which it was not admitted with")
}
