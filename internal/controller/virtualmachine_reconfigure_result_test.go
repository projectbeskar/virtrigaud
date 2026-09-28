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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin how the manager records the honest Reconfigure result and
// the status.currentResources invariant (virtualmachine_reconfigure_result.go):
// never less than the VM can hold, now or after its next boot.

// resultProvider is a fakeDescribeProvider (Describe: running) whose
// Reconfigure answers with result / err and counts its calls.
type resultProvider struct {
	fakeDescribeProvider
	result contracts.ReconfigureResult
	err    error
	calls  int
}

func (p *resultProvider) Reconfigure(context.Context, contracts.VMRef, contracts.CreateRequest) (contracts.ReconfigureResult, error) {
	p.calls++
	return p.result, p.err
}

func newResultProvider() *resultProvider {
	return &resultProvider{fakeDescribeProvider: fakeDescribeProvider{
		DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.1"}}, nil
		},
	}}
}

// sizedSingleHostVM is a single-host VM recorded at cpu vCPU / memMiB MiB whose spec
// asks for wantCPU / wantMem (providerAndClass: 4 vCPU, 8 GiB class).
func sizedSingleHostVM(cpu int32, memMiB int64, wantCPU int32, wantMem int64) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := baseVM("default")
	vm.Generation = 1
	vm.Status.ID = "vm-1"
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(cpu), MemoryMiB: i64p(memMiB)}
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(wantCPU), MemoryMiB: i64p(wantMem)}
	return vm
}

// fakeClock is a settable reconciler clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func singleHostReconciler(t *testing.T, prov contracts.Provider) (*VirtualMachineReconciler, *fakeClock) {
	t.Helper()
	k8sProv, class := providerAndClass("default")
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, k8sProv, class)
	clock := &fakeClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	r.clock = clock.now
	return r, clock
}

func recorded(vm *infravirtrigaudiov1beta1.VirtualMachine) (int32, int64) {
	return *vm.Status.CurrentResources.CPU, *vm.Status.CurrentResources.MemoryMiB
}

// TestReconfigureVM_Error_LeavesCurrentResourcesAndSetsCondition: a failed
// Reconfigure leaves status.currentResources untouched and says why.
func TestReconfigureVM_Error_LeavesCurrentResourcesAndSetsCondition(t *testing.T) {
	prov := newResultProvider()
	prov.err = contracts.NewRetryableError("could not set 8 vCPUs in the VM's persistent definition", nil)
	r, _ := singleHostReconciler(t, prov)
	_, class := providerAndClass("default")
	vm := sizedSingleHostVM(4, 8192, 8, 8192)

	_, err := r.reconfigureVM(context.Background(), vm, prov, contracts.VMRef{ID: vm.Status.ID}, nil, class, nil, nil)
	require.NoError(t, err)

	cpu, mem := recorded(vm)
	assert.Equal(t, int32(4), cpu, "a failed Reconfigure never advances status.currentResources")
	assert.Equal(t, int64(8192), mem)
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonProviderError, c.Reason)
	assert.Contains(t, c.Message, "could not set 8 vCPUs")
}

// TestReconfigureVM_RestartRequired_Invariant: a change applied to the
// persistent definition only records, per resource, the larger of the running
// size and the next-boot size — a grow is counted at once, a shrink never
// lowers the counted size while the old size still runs.
func TestReconfigureVM_RestartRequired_Invariant(t *testing.T) {
	cases := []struct {
		name         string
		cpu, wantCPU int32
		mem, wantMem int64
		recordCPU    int32
		recordMem    int64
	}{
		{name: "grow", cpu: 2, wantCPU: 4, mem: 4096, wantMem: 8192, recordCPU: 4, recordMem: 8192},
		{name: "shrink", cpu: 4, wantCPU: 2, mem: 8192, wantMem: 4096, recordCPU: 4, recordMem: 8192},
		{name: "CPU up, memory down", cpu: 2, wantCPU: 4, mem: 8192, wantMem: 4096, recordCPU: 4, recordMem: 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := newResultProvider()
			prov.result = contracts.ReconfigureResult{RestartRequired: true}
			r, _ := singleHostReconciler(t, prov)
			_, class := providerAndClass("default")
			vm := sizedSingleHostVM(tc.cpu, tc.mem, tc.wantCPU, tc.wantMem)

			res, err := r.reconfigureVM(context.Background(), vm, prov, contracts.VMRef{ID: vm.Status.ID}, nil, class, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, restartPendingRecheckInterval, res.RequeueAfter)

			cpu, mem := recorded(vm)
			assert.Equal(t, tc.recordCPU, cpu)
			assert.Equal(t, tc.recordMem, mem)
			fp := admittedFootprint(vm, nil)
			assert.Equal(t, max(tc.cpu, tc.wantCPU), fp.CPU, "the clustered accounting counts the larger size")
			assert.Equal(t, max(tc.mem, tc.wantMem), fp.MemoryMiB)

			c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, k8s.ReasonRestartRequired, c.Reason)
			assert.Equal(t, vm.Generation, c.ObservedGeneration)
			assert.Contains(t, c.Message, "next power cycle")
			assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, vm.Status.Phase)
			ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionTrue, ready.Status, "a VM with a change pending a restart is still ready")
		})
	}
}

// TestReconcileVM_RestartPending_RecheckedOnASlowCadence: while a shrink is
// pending a restart the provider is not asked again on every reconcile (the
// spec still differs from the recorded size), but every
// restartPendingRecheckInterval; once the VM has been power-cycled and the
// provider answers "applied", the smaller size is recorded.
func TestReconcileVM_RestartPending_RecheckedOnASlowCadence(t *testing.T) {
	prov := newResultProvider()
	prov.result = contracts.ReconfigureResult{RestartRequired: true}
	r, clock := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 2, 8192)
	ctx := context.Background()

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	require.Equal(t, 1, prov.calls)
	cpu, _ := recorded(vm)
	require.Equal(t, int32(4), cpu, "the shrink is pending: the old size is still counted")

	clock.t = clock.t.Add(30 * time.Second)
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 1, prov.calls, "not asked again within the re-check interval")
	assert.LessOrEqual(t, res.RequeueAfter, restartPendingRecheckInterval-30*time.Second)
	assert.Equal(t, k8s.ReasonRestartRequired, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)

	// Still pending at the next re-check.
	clock.t = clock.t.Add(restartPendingRecheckInterval)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "asked again once the interval has passed")
	cpu, _ = recorded(vm)
	assert.Equal(t, int32(4), cpu)

	// The VM has been power-cycled: the provider now finds it applied.
	prov.result = contracts.ReconfigureResult{}
	clock.t = clock.t.Add(restartPendingRecheckInterval)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 3, prov.calls)
	cpu, _ = recorded(vm)
	assert.Equal(t, int32(2), cpu, "the shrink is recorded once the provider confirms it applied")
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonReconcileSuccess, c.Reason)

	// Settled: nothing more is sent.
	clock.t = clock.t.Add(restartPendingRecheckInterval)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 3, prov.calls)
}

// TestReconcileVM_RestartPending_GrowIsReverified: a grow pending a restart is
// recorded at once (so the spec matches status), yet the provider is still
// asked again until it confirms the change applied, which clears the
// condition.
func TestReconcileVM_RestartPending_GrowIsReverified(t *testing.T) {
	prov := newResultProvider()
	prov.result = contracts.ReconfigureResult{RestartRequired: true}
	r, clock := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(2, 8192, 4, 8192)
	ctx := context.Background()

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	cpu, _ := recorded(vm)
	require.Equal(t, int32(4), cpu, "a grow pending a restart is counted at once")

	prov.result = contracts.ReconfigureResult{}
	clock.t = clock.t.Add(restartPendingRecheckInterval)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "re-verified although the spec already matches the recorded size")
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)
}

// TestReconcileVM_RestartPending_SpecChangeRechecksAtOnce: a spec change while
// a change is pending a restart is sent at once.
func TestReconcileVM_RestartPending_SpecChangeRechecksAtOnce(t *testing.T) {
	prov := newResultProvider()
	prov.result = contracts.ReconfigureResult{RestartRequired: true}
	r, clock := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 2, 8192)
	ctx := context.Background()

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	require.Equal(t, 1, prov.calls)

	// The owner reverts the shrink: the persistent definition must be put back,
	// even though the spec now equals the recorded size.
	vm.Generation++
	vm.Spec.Resources.CPU = i32p(4)
	prov.result = contracts.ReconfigureResult{}
	clock.t = clock.t.Add(10 * time.Second)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "a spec change is sent without waiting for the re-check interval")
	cpu, _ := recorded(vm)
	assert.Equal(t, int32(4), cpu)
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)
}

// TestReconcileVM_FailedReconfigure_ResentAfterSpecRevert: a failed
// Reconfigure may have changed part of the persistent definition, so it is
// sent again even once the spec is reverted to the recorded size, until one
// succeeds.
func TestReconcileVM_FailedReconfigure_ResentAfterSpecRevert(t *testing.T) {
	prov := newResultProvider()
	prov.err = contracts.NewRetryableError("could not set 8192 MiB of memory in the VM's persistent definition", nil)
	r, _ := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 8, 16384)
	ctx := context.Background()

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	require.Equal(t, 1, prov.calls)

	vm.Generation++
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4), MemoryMiB: i64p(8192)}
	prov.err = nil
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "re-sent to converge the definition, although the spec matches the recorded size")
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)

	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "settled: nothing more is sent")
}

// ─── clustered ────────────────────────────────────────────────────────────────

// TestClustered_ShrinkWhileOff_NotAppliedIsNotRecorded (review R1): an older
// provider reports a PM-suspended domain as Off, so the deferred shrink is
// attempted; the provider refuses it (the domain is active), or finds it
// running and applies it to the definition only. Either way the smaller size
// is never recorded while the old size can still run.
func TestClustered_ShrinkWhileOff_NotAppliedIsNotRecorded(t *testing.T) {
	for name, setup := range map[string]func(p *routingProvider){
		"refused": func(p *routingProvider) {
			p.reconfigureErr = contracts.NewRetryableError("the VM is \"pmsuspended\" (active, but not running)", nil)
		},
		"restart required": func(p *routingProvider) { p.reconfigureResult = contracts.ReconfigureResult{RestartRequired: true} },
	} {
		t.Run(name, func(t *testing.T) {
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
			setup(prov)
			r := overcommittedShrinkFixture(t, prov, infravirtrigaudiov1beta1.PowerStateOn)
			_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			require.Len(t, prov.reconfigureRefs, 1, "the shrink was attempted")
			got := getVM(t, r, "app")
			assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "never recorded as applied")
			assert.Equal(t, int32(4), admittedFootprint(got, smallVMClass(capNS)).CPU, "the VM keeps counting at its old size")
		})
	}
}

// TestClustered_MemoryCeiling_BackfilledOnceFromTheProvider (review R2, H1b):
// a bound clustered VM with no recorded ceiling gets the provider's memory
// maximum recorded; the VMClass's hot-add flag no longer sizes it, and a
// higher report raises it at once.
func TestClustered_MemoryCeiling_BackfilledOnceFromTheProvider(t *testing.T) {
	prov := runningRoutingProvider()
	prov.describeResp.MaxMemoryMiB = 16384
	app := sized("app", 2) // 4096 MiB recorded, no ceiling recorded
	require.Nil(t, app.Status.Placement.MemoryCeilingMiB)
	r := resizeFixture(t, prov, app)

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got := getVM(t, r, "app")
	require.NotNil(t, got.Status.Placement.MemoryCeilingMiB)
	assert.Equal(t, int64(16384), *got.Status.Placement.MemoryCeilingMiB)
	assert.Equal(t, int64(16384), admittedFootprint(got, nil).MemoryMiB, "counted at the ceiling the provider reports")

	// A higher report — the domain can now reach more, e.g. after a failed or
	// partial Reconfigure raised its <memory> — raises it at once (review H1b).
	prov.describeResp.MaxMemoryMiB = 32768
	_, err = r.reconcileVM(context.Background(), got)
	require.NoError(t, err)
	got = getVM(t, r, "app")
	assert.Equal(t, int64(32768), *got.Status.Placement.MemoryCeilingMiB, "raised to what the provider reports")
	assert.Equal(t, int64(32768), admittedFootprint(got, nil).MemoryMiB)

	// A report at or below the recorded memory never raises it.
	prov.describeResp.MaxMemoryMiB = 4096
	app2 := sized("app2", 2)
	app2.Status.Placement.MemoryCeilingMiB = i64p(0)
	r2 := resizeFixture(t, prov, app2)
	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app2"))
	require.NoError(t, err)
	assert.Zero(t, *getVM(t, r2, "app2").Status.Placement.MemoryCeilingMiB)
}

// TestClustered_ReconfigureFailure_CountedAtTheLargerSize (review H1a): a
// clustered Reconfigure that fails may have applied part of the change, so
// the VM is counted at max(recorded, desired); reverting the spec afterwards
// never lowers the count while the domain may still hold the larger size.
func TestClustered_ReconfigureFailure_CountedAtTheLargerSize(t *testing.T) {
	prov := runningRoutingProvider()
	prov.reconfigureErr = contracts.NewRetryableError("could not grow the VM's disk to 1048576 GiB", nil)
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4)) // 4 + 4 = 8 of 8: admitted
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "the admitted grow was sent")

	got := getVM(t, r, "app")
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "a failed grow is counted at the desired size")
	assert.Equal(t, int32(4), admittedFootprint(got, nil).CPU)
	c := reconfiguringCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonProviderError, c.Reason)
	assert.Equal(t, got.Generation, c.ObservedGeneration)

	// The tenant reverts. The domain may still run with 4 vCPUs (it reports 4):
	// the count stays at 4 — the shrink waits for power-off — never 2.
	got.Spec.Resources.CPU = i32p(2)
	require.NoError(t, r.Update(context.Background(), got))
	prov.reconfigureErr = nil
	prov.describeResp.VCPUs = 4
	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got = getVM(t, r, "app")
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU)
	assert.Equal(t, int32(4), admittedFootprint(got, nil).CPU, "never counted below what the domain may hold")
}

// TestClustered_RecordedCPU_RaisedToWhatTheProviderReports (review H1d): a
// clustered VM whose provider reports more vCPUs online than recorded is
// counted at them; a lower report never lowers the record; single-host VMs are
// untouched.
func TestClustered_RecordedCPU_RaisedToWhatTheProviderReports(t *testing.T) {
	prov := runningRoutingProvider()
	prov.describeResp.VCPUs = 6
	app := sized("app", 2)
	app.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(6), MemoryMiB: i64p(4096)}
	r := resizeFixture(t, prov, app)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got := getVM(t, r, "app")
	assert.Equal(t, int32(6), *got.Status.CurrentResources.CPU)
	assert.Empty(t, prov.reconfigureRefs, "the recorded size now matches the spec: nothing to send")

	prov.describeResp.VCPUs = 1
	_, err = r.reconcileVM(context.Background(), got)
	require.NoError(t, err)
	assert.Equal(t, int32(6), *getVM(t, r, "app").Status.CurrentResources.CPU, "never lowered by a report")

	single := newResultProvider()
	single.DescribeFn = func(context.Context, string) (contracts.DescribeResponse, error) {
		return contracts.DescribeResponse{Exists: true, PowerState: "On", VCPUs: 16}, nil
	}
	rs, _ := singleHostReconciler(t, single)
	vm := sizedSingleHostVM(4, 8192, 4, 8192)
	_, err = rs.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Equal(t, int32(4), *vm.Status.CurrentResources.CPU, "single-host: status is not rewritten from Describe")
}

func TestClustered_MemoryCeiling_BackfillWithoutHeadroomIsZero(t *testing.T) {
	prov := runningRoutingProvider()
	prov.describeResp.MaxMemoryMiB = 4096 // == the recorded memory: no balloon headroom
	r := resizeFixture(t, prov, sized("app", 2))
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got := getVM(t, r, "app")
	require.NotNil(t, got.Status.Placement.MemoryCeilingMiB)
	assert.Zero(t, *got.Status.Placement.MemoryCeilingMiB)
}

// TestClustered_MemoryCeiling_LoweredAfterConfirmedShrink (review R3): once
// the provider reports a lower memory maximum (a confirmed shrink lowered the
// domain's <memory>), the recorded ceiling follows — but not while a change is
// pending a restart.
func TestClustered_MemoryCeiling_LoweredAfterConfirmedShrink(t *testing.T) {
	newApp := func() *infravirtrigaudiov1beta1.VirtualMachine {
		app := sized("app", 2)
		app.Status.CurrentResources.MemoryMiB = i64p(2048)
		app.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(2048)} // already applied
		app.Status.Placement.MemoryCeilingMiB = i64p(16384)
		return app
	}

	prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff), MaxMemoryMiB: 2048}}
	app := newApp()
	app.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	r := resizeFixture(t, prov, app)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got := getVM(t, r, "app")
	assert.Zero(t, *got.Status.Placement.MemoryCeilingMiB, "lowered: the domain can no longer reach more than its own size")
	assert.Equal(t, int64(2048), admittedFootprint(got, nil).MemoryMiB)

	// Pending a restart: left alone.
	prov = &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn), MaxMemoryMiB: 2048}}
	app = newApp()
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{Type: k8s.ConditionReconfiguring, Status: metav1.ConditionTrue,
		Reason: k8s.ReasonRestartRequired, Message: "pending"})
	r = resizeFixture(t, prov, app)
	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Equal(t, int64(16384), *getVM(t, r, "app").Status.Placement.MemoryCeilingMiB, "not lowered while a change is pending a restart")
}

func TestSingleHost_MemoryCeiling_NotRecorded(t *testing.T) {
	prov := newResultProvider()
	prov.DescribeFn = func(context.Context, string) (contracts.DescribeResponse, error) {
		return contracts.DescribeResponse{Exists: true, PowerState: "On", MaxMemoryMiB: 16384}, nil
	}
	r, _ := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 4, 8192)
	_, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Nil(t, vm.Status.Placement, "a single-host VM has no placement record")
}

// TestClustered_ResizeHeldWithoutHonestReconfigure (review H3): a clustered
// Provider that does not report supportsHonestReconfigure (an older provider
// image) gets no Reconfigure — neither a grow nor a shrink applied while off —
// and the VM says why; a single-host Provider is resized as before.
func TestClustered_ResizeHeldWithoutHonestReconfigure(t *testing.T) {
	older := func(p *infravirtrigaudiov1beta1.Provider) *infravirtrigaudiov1beta1.Provider {
		p.Status.ReportedCapabilities = nil
		return p
	}
	cases := map[string]struct {
		vm    *infravirtrigaudiov1beta1.VirtualMachine
		power string
	}{
		"grow":                  {wantsCPU(sized("app", 2), 4), string(contracts.PowerStateOn)},
		"shrink while off":      {wantsCPU(sized("app", 4), 1), string(contracts.PowerStateOff)},
		"shrink while running":  {wantsCPU(sized("app", 4), 1), string(contracts.PowerStateOn)},
		"memory grow (hot-add)": {sizedWithMem(sized("app", 2), 8192), string(contracts.PowerStateOn)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: tc.power}}
			tc.vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerState(tc.power)
			providerCR := older(withRuntime(clusteredProviderCR("prov-cluster", capNS)))
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, providerCR,
				hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS), tc.vm)
			res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			assert.Empty(t, prov.reconfigureRefs, "nothing is sent to a Provider without the honest Reconfigure result")
			assert.Equal(t, placementConfigRetryInterval, res.RequeueAfter)
			got := getVM(t, r, "app")
			c := reconfiguringCondition(got)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, k8s.ReasonProviderLacksHonestReconfigure, c.Reason)
			assert.Equal(t, got.Generation, c.ObservedGeneration)
			assert.Equal(t, tc.vm.Status.CurrentResources.CPU, got.Status.CurrentResources.CPU, "the VM keeps its size")
		})
	}

	// A single-host Provider without it is resized as before.
	single := newResultProvider()
	rs, _ := singleHostReconciler(t, single) // providerAndClass: no ReportedCapabilities
	vm := sizedSingleHostVM(2, 8192, 4, 8192)
	_, err := rs.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Equal(t, 1, single.calls, "single-host keeps today's behaviour")
}

// TestClustered_UnmarkedAnswerIsNotTrusted (review L1): a clustered Provider
// whose capability snapshot says honest but whose answer lacks the
// honest-result marker (a rolled-back provider image) is not trusted: the VM is
// counted at the larger size, the capability hold's condition is set, and the
// resize is re-checked every restartPendingRecheckInterval until a marked
// answer records what was applied.
func TestClustered_UnmarkedAnswerIsNotTrusted(t *testing.T) {
	t.Run("grow", func(t *testing.T) {
		prov := runningRoutingProvider()
		prov.reconfigureUnmarked = true
		r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4))
		res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
		require.Len(t, prov.reconfigureRefs, 1)
		assert.Equal(t, restartPendingRecheckInterval, res.RequeueAfter)
		got := getVM(t, r, "app")
		assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "counted at the larger size")
		c := reconfiguringCondition(got)
		require.NotNil(t, c)
		assert.Equal(t, k8s.ReasonProviderLacksHonestReconfigure, c.Reason)
		assert.Contains(t, c.Message, "honest-result marker")
	})

	t.Run("shrink while off, then a marked answer", func(t *testing.T) {
		prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
		prov.reconfigureUnmarked = true
		r := overcommittedShrinkFixture(t, prov, infravirtrigaudiov1beta1.PowerStateOff)
		clock := &fakeClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
		r.clock = clock.now
		_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
		require.Len(t, prov.reconfigureRefs, 1)
		assert.Equal(t, int32(4), *getVM(t, r, "app").Status.CurrentResources.CPU, "an unmarked shrink never lowers the count")

		clock.t = clock.t.Add(time.Minute)
		_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
		assert.Len(t, prov.reconfigureRefs, 1, "not re-sent within the re-check interval")

		prov.reconfigureUnmarked = false // the provider is rolled forward
		clock.t = clock.t.Add(restartPendingRecheckInterval)
		_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
		require.Len(t, prov.reconfigureRefs, 2)
		got := getVM(t, r, "app")
		assert.Equal(t, int32(1), *got.Status.CurrentResources.CPU, "a marked answer records what was applied")
		assert.Equal(t, k8s.ReasonReconcileSuccess, reconfiguringCondition(got).Reason)
	})

	t.Run("single-host ignores the marker", func(t *testing.T) {
		single := newResultProvider() // answers without the marker
		rs, _ := singleHostReconciler(t, single)
		vm := sizedSingleHostVM(4, 8192, 2, 8192)
		_, err := rs.reconcileVM(context.Background(), vm)
		require.NoError(t, err)
		assert.Equal(t, int32(2), *vm.Status.CurrentResources.CPU, "single-host records the answer as before")
	})
}

// sizedWithMem asks vm for memMiB of memory at its recorded CPU.
func sizedWithMem(vm *infravirtrigaudiov1beta1.VirtualMachine, memMiB int64) *infravirtrigaudiov1beta1.VirtualMachine {
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: vm.Status.CurrentResources.CPU, MemoryMiB: i64p(memMiB)}
	return vm
}

// TestReconcileVM_FailedReconfigure_BacksOff (review H2): a Reconfigure that
// keeps failing is re-sent on a per-VM backoff — 5 s doubling to 5 min — not
// on every reconcile; a spec change is sent at once, and a success resets it.
func TestReconcileVM_FailedReconfigure_BacksOff(t *testing.T) {
	prov := newResultProvider()
	prov.err = contracts.NewRetryableError("could not set 8 vCPUs in the VM's persistent definition", nil)
	r, clock := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 8, 8192)
	ctx := context.Background()

	var waits []time.Duration
	for range 8 {
		res, err := r.reconcileVM(ctx, vm)
		require.NoError(t, err)
		waits = append(waits, res.RequeueAfter)
		// A reconcile before the wait is over sends nothing.
		clock.t = clock.t.Add(res.RequeueAfter / 2)
		calls := prov.calls
		_, err = r.reconcileVM(ctx, vm)
		require.NoError(t, err)
		require.Equal(t, calls, prov.calls, "not re-sent within the backoff")
		clock.t = clock.t.Add(res.RequeueAfter - res.RequeueAfter/2)
	}
	assert.Equal(t, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute}, waits)

	// A spec change is sent at once, whatever is left of the wait.
	calls := prov.calls
	vm.Generation++
	vm.Spec.Resources.CPU = i32p(6)
	prov.err = nil
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, calls+1, prov.calls)
	assert.Zero(t, r.reconfigureRetry.remaining(vmSchedulingUID(vm), clock.t), "a success resets the backoff")
}
