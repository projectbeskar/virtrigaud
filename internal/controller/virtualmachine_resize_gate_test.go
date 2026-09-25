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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// The clustered resize gate (William's decision in the scheduler-accuracy
// review): a resize-up of a clustered VM is admitted against its host's free
// capacity, under the Provider's assume lock, before the provider applies it.

// sized is a VM bound to host-alpha, recorded at cpu vCPU / 4096 MiB.
func sized(name string, cpu int32) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := withPlacement(capVM(name), "host-alpha", "")
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(cpu), MemoryMiB: i64p(4096)}
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	return vm
}

// wantsCPU sets the VM's spec.resources CPU (its desired size).
func wantsCPU(vm *infravirtrigaudiov1beta1.VirtualMachine, cpu int32) *infravirtrigaudiov1beta1.VirtualMachine {
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(cpu), MemoryMiB: i64p(4096)}
	return vm
}

// resizeFixture: a clustered Provider whose host-alpha has 8 vCPU, a
// neighbour VM holding 4 of them, and vm.
func resizeFixture(t *testing.T, prov contracts.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine, extra ...client.Object) *VirtualMachineReconciler {
	t.Helper()
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", capNS))
	objs := append([]client.Object{providerCR, hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8),
		smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4), vm}, extra...)
	return newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, objs...)
}

func runningRoutingProvider() *routingProvider {
	return &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(infravirtrigaudiov1beta1.PowerStateOn)}}
}

func reconfiguringCondition(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
}

func TestResizeGate_FitsIsApplied(t *testing.T) {
	prov := runningRoutingProvider()
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4)) // 4 + 4 = 8 of 8
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "the resize is sent")
	assert.Equal(t, "host-alpha", prov.reconfigureRefs[0].HostID)
	got := getVM(t, r, "app")
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU)
	assert.NotEqual(t, k8s.ReasonInsufficientHostCapacity, reconfiguringCondition(got).Reason)
}

func TestResizeGate_DoesNotFitIsRefused(t *testing.T) {
	prov := runningRoutingProvider()
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 5)) // 4 + 5 = 9 of 8
	res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs, "no provider call for a refused resize")
	assert.Equal(t, placementUnschedulableRetryInterval, res.RequeueAfter, "retried with the placement backoff")

	got := getVM(t, r, "app")
	assert.Equal(t, int32(2), *got.Status.CurrentResources.CPU, "the VM keeps its size")
	c := reconfiguringCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonInsufficientHostCapacity, c.Reason)
	assert.Equal(t, got.Generation, c.ObservedGeneration)
	assert.Contains(t, c.Message, "resizing to 5 vCPU and 4096 MiB exceeds the free capacity of its host host-alpha")
	assert.Contains(t, c.Message, "the VM keeps 2 vCPU and 4096 MiB")
	assert.NotContains(t, c.Message, "committed", "no committed-capacity figure (review M3)")
	assert.NotContains(t, c.Message, "neighbour")

	// The next attempt backs off further.
	res, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Equal(t, 2*placementUnschedulableRetryInterval, res.RequeueAfter)
}

// ─── shrinks wait for power-off on a clustered Provider (review N1) ──────────

// overcommittedShrinkFixture: app holds 4 vCPU and asks for 1, on a host whose
// pool is far over-committed (a shrink is never refused on capacity).
func overcommittedShrinkFixture(t *testing.T, prov contracts.Provider, powerState infravirtrigaudiov1beta1.PowerState) *VirtualMachineReconciler {
	t.Helper()
	pool := hostPoolCR("pool-a", capNS, "prov-cluster")
	pool.Spec.Overcommit = &infravirtrigaudiov1beta1.OvercommitRatios{CPU: "0.1"}
	app := wantsCPU(sized("app", 4), 1)
	app.Spec.PowerState = powerState
	return newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov},
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), pool, capHost("host-alpha", 8),
		smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4), app)
}

func TestShrink_RunningClusteredVMIsDeferred(t *testing.T) {
	prov := runningRoutingProvider()
	r := overcommittedShrinkFixture(t, prov, infravirtrigaudiov1beta1.PowerStateOn)
	res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs, "no Reconfigure for a shrink while the VM runs")
	assert.Empty(t, prov.powerRefs, "and the VM is never powered off for it")
	assert.Equal(t, placementUnschedulableRetryInterval, res.RequeueAfter)

	got := getVM(t, r, "app")
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "it keeps counting at its current size")
	assert.Equal(t, int32(4), admittedFootprint(got, smallVMClass(capNS)).CPU)
	c := reconfiguringCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonShrinkPendingPowerOff, c.Reason)
	assert.Contains(t, c.Message, "set spec.powerState: Off")
}

func TestShrink_MixedChangeWaitsAsAWhole(t *testing.T) {
	prov := runningRoutingProvider()
	app := sized("app", 2)
	app.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3), MemoryMiB: i64p(2048)} // CPU up, memory down
	r := resizeFixture(t, prov, app)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs)
	assert.Equal(t, k8s.ReasonShrinkPendingPowerOff, reconfiguringCondition(getVM(t, r, "app")).Reason)
}

func TestShrink_AppliedOncePoweredOff(t *testing.T) {
	// The VM is found off while its spec still wants it on: the shrink is
	// applied (offline) and recorded first, and nothing is powered in this
	// reconcile; the next one powers it on as its spec asks.
	prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
	r := overcommittedShrinkFixture(t, prov, infravirtrigaudiov1beta1.PowerStateOn)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "applied while off, even on an over-committed host")
	assert.Empty(t, prov.powerRefs, "not powered on before the shrink is applied")
	assert.Equal(t, int32(1), *getVM(t, r, "app").Status.CurrentResources.CPU, "recorded only after the provider applied it")

	// Also when its spec wants it off.
	prov = &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
	r = overcommittedShrinkFixture(t, prov, infravirtrigaudiov1beta1.PowerStateOff)
	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1)
	assert.Equal(t, int32(1), *getVM(t, r, "app").Status.CurrentResources.CPU)
}

func TestShrink_SingleHostIsUnchanged(t *testing.T) {
	prov := runningRoutingProvider()
	single := withRuntime(singleProviderCR("prov-single", capNS))
	vm := clusterVM("small", capNS, single.Name)
	vm.Status.ID = "small"
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4), MemoryMiB: i64p(4096)}
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(1), MemoryMiB: i64p(4096)}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, single, smallVMClass(capNS), minimalVMImage(capNS), vm)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "small"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "a single-host shrink is sent while running, as before")
	assert.Nil(t, r.placements.Load())
}

func TestResizeGate_UnknownHostFailsClosed(t *testing.T) {
	prov := runningRoutingProvider()
	vm := wantsCPU(sized("app", 2), 4)
	vm.Status.Placement.Host = "host-gone"
	r := resizeFixture(t, prov, vm)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs)
	assert.Contains(t, reconfiguringCondition(getVM(t, r, "app")).Message, "host-gone is not registered")
}

// TestResizeGate_ConcurrentCreateAndResizeNeverOverbook: host-alpha has 2
// vCPU left; a resize of app by +2 and a create of a 2 vCPU VM race through a
// client whose reads never see the other's writes. Exactly one of them gets
// the capacity, every round.
func TestResizeGate_ConcurrentCreateAndResizeNeverOverbook(t *testing.T) {
	s := coverageTestScheme(t)
	for round := 0; round < 30; round++ {
		providerCR := withRuntime(clusteredProviderCR("prov-cluster", capNS))
		objs := []client.Object{providerCR, hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8),
			smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4), wantsCPU(sized("app", 2), 4), capVM("newvm")}
		lc := newLaggingClient(t, s, objs...)
		r := &VirtualMachineReconciler{Client: lc, Scheme: s}
		prov := &concurrentCreateProvider{}

		var (
			wg       sync.WaitGroup
			admitted bool
			start    = make(chan struct{})
			app      = readVM(t, lc, "app")
			newVM    = readVM(t, lc, "newvm")
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := r.admitClusteredResize(context.Background(), app, providerCR, smallVMClass(capNS), "host-alpha")
			assert.NoError(t, err)
			admitted = ok
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := r.createVM(context.Background(), newVM, prov, providerCR, smallVMClass(capNS), minimalVMImage(capNS), nil)
			assert.NoError(t, err)
		}()
		close(start)
		wg.Wait()

		placed := 0
		if admitted {
			placed++
		}
		placed += len(prov.created())
		require.Equal(t, 1, placed, "round %d: resize admitted=%v, creates=%v", round, admitted, prov.created())
	}
}

// TestResizeGate_SingleHostIsUnchanged: a single-host Provider's resize goes
// straight to the provider — no Host, no capacity check, no assume cache.
func TestResizeGate_SingleHostIsUnchanged(t *testing.T) {
	prov := runningRoutingProvider()
	single := withRuntime(singleProviderCR("prov-single", capNS))
	vm := clusterVM("big", capNS, single.Name)
	vm.Status.ID = "big"
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2), MemoryMiB: i64p(4096)}
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(64), MemoryMiB: i64p(4096)}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, single, smallVMClass(capNS), minimalVMImage(capNS), vm)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "big"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1)
	assert.Empty(t, prov.reconfigureRefs[0].HostID)
	assert.Nil(t, r.placements.Load(), "the assume cache is never created")
}

// TestResizeAssumptionSettles: an admitted resize counts until the VM's
// recorded size reaches it, or the VM leaves the host or is gone — not merely
// because its record names the host (it always does).
func TestResizeAssumptionSettles(t *testing.T) {
	a := assume.Assumption{UID: "u", HostID: "host-alpha", Resize: true,
		Resources: scheduler.ResourceRequest{CPU: 4, MemoryMiB: 4096}}
	snap := func(rec recordedVM) committedSnapshot {
		return committedSnapshot{recorded: map[string]recordedVM{"u": rec}}
	}
	on := []string{"host-alpha"}
	assert.False(t, snap(recordedVM{hosts: on, size: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096}, hasSize: true}).settled(a),
		"still at its old size")
	assert.False(t, snap(recordedVM{hosts: on}).settled(a), "no recorded size")
	assert.True(t, snap(recordedVM{hosts: on, size: scheduler.ResourceRequest{CPU: 4, MemoryMiB: 4096}, hasSize: true}).settled(a),
		"recorded at the admitted size")
	assert.True(t, snap(recordedVM{hosts: []string{"host-beta"}}).settled(a), "left the host")
	assert.True(t, committedSnapshot{recorded: map[string]recordedVM{}}.settled(a), "gone")

	create := assume.Assumption{UID: "u", HostID: "host-alpha"}
	assert.True(t, snap(recordedVM{hosts: on}).settled(create), "a create settles on its record")
	assert.False(t, snap(recordedVM{hosts: []string{"host-beta"}}).settled(create), "not on a record elsewhere")
}

// TestResizeAssumptionOutlivesTheReconfigureCall (review N4): an admitted
// resize stays assumed for the Reconfigure deadline plus the status-write
// bound, not only the create path's shorter TTL.
func TestResizeAssumptionOutlivesTheReconfigureCall(t *testing.T) {
	require.GreaterOrEqual(t, resizeAssumeTTL, contracts.ReconfigureCallTimeout+placementStatusWriteTimeout)
	require.Greater(t, resizeAssumeTTL, placementAssumeTTL)

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	vm := wantsCPU(sized("app", 2), 4)
	r := resizeFixture(t, runningRoutingProvider(), vm)
	r.clock = func() time.Time { return now }
	_, admitted, err := r.admitClusteredResize(context.Background(), getVM(t, r, "app"),
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), smallVMClass(capNS), "host-alpha")
	require.NoError(t, err)
	require.True(t, admitted)

	now = now.Add(placementAssumeTTL + time.Minute)
	assert.Equal(t, []string{"uid-app"}, assumedUIDs(r), "still assumed after the create TTL")
	now = now.Add(resizeAssumeTTL)
	assert.Empty(t, assumedUIDs(r), "gone after its own TTL")
}

// TestResizeGate_UnusablePoolFailsClosed (review N6): a host whose HostPool is
// missing or belongs to another Provider has no known overcommit ratio, so a
// resize-up on it is refused instead of assuming 1.0.
func TestResizeGate_UnusablePoolFailsClosed(t *testing.T) {
	for name, pool := range map[string]*infravirtrigaudiov1beta1.HostPool{
		"missing": nil,
		"foreign": hostPoolCR("pool-a", capNS, "another-provider"),
	} {
		t.Run(name, func(t *testing.T) {
			prov := runningRoutingProvider()
			objs := []client.Object{withRuntime(clusteredProviderCR("prov-cluster", capNS)), capHost("host-alpha", 64),
				smallVMClass(capNS), minimalVMImage(capNS), wantsCPU(sized("app", 2), 4)}
			if pool != nil {
				objs = append(objs, pool)
			}
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, objs...)
			res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			assert.Empty(t, prov.reconfigureRefs, "no provider call")
			assert.Equal(t, placementConfigRetryInterval, res.RequeueAfter)
			c := reconfiguringCondition(getVM(t, r, "app"))
			require.NotNil(t, c)
			assert.Equal(t, k8s.ReasonPlacementError, c.Reason)
			assert.Contains(t, c.Message, "does not exist or belongs to another Provider")
		})
	}
}

// deadlineRecordingClient records whether each VirtualMachine List carried a
// deadline no later than bound from its start.
type deadlineRecordingClient struct {
	client.Client
	mu      sync.Mutex
	bounded []bool
}

func (c *deadlineRecordingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*infravirtrigaudiov1beta1.VirtualMachineList); ok {
		d, has := ctx.Deadline()
		c.mu.Lock()
		c.bounded = append(c.bounded, has && time.Until(d) <= placementLockWait)
		c.mu.Unlock()
	}
	return c.Client.List(ctx, list, opts...)
}

// TestReadsUnderTheLockAreBounded (review N6): every cache read made while a
// Provider's assume lock is held carries a deadline of at most
// placementLockWait, in the create and the resize path.
func TestReadsUnderTheLockAreBounded(t *testing.T) {
	vm := wantsCPU(sized("app", 2), 4)
	r := resizeFixture(t, runningRoutingProvider(), vm, capVM("new"))
	rec := &deadlineRecordingClient{Client: r.Client}
	r.Client = rec

	_, _ = resolve(t, r, readVM(t, r, "new"))
	_, _, err := r.admitClusteredResize(context.Background(), readVM(t, r, "app"),
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), smallVMClass(capNS), "host-alpha")
	require.NoError(t, err)
	require.Len(t, rec.bounded, 2)
	assert.Equal(t, []bool{true, true}, rec.bounded)
}

// panickingListClient panics on every VirtualMachine List: a stand-in for a
// bug anywhere under the Provider's assume lock.
type panickingListClient struct{ client.Client }

func (c panickingListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*infravirtrigaudiov1beta1.VirtualMachineList); ok {
		panic("boom under the assume lock")
	}
	return c.Client.List(ctx, list, opts...)
}

// TestAssumeLockIsReleasedOnPanic (review N2): controller-runtime recovers a
// panicking reconcile, so a panic inside the create or resize critical section
// must not leave the Provider's lock held.
func TestAssumeLockIsReleasedOnPanic(t *testing.T) {
	recovered := func(fn func()) (p any) {
		defer func() { p = recover() }()
		fn()
		return nil
	}
	lockIsFree := func(t *testing.T, r *VirtualMachineReconciler) {
		t.Helper()
		unlock, ok := r.placementAssumptions().LockWithin(context.Background(), capNS+"/prov-cluster", 100*time.Millisecond)
		require.True(t, ok, "the Provider's lock was left held by the panic")
		unlock()
	}

	t.Run("create", func(t *testing.T) {
		vm := capVM("new")
		r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}},
			append(capBase(), capHost("host-alpha", 8), vm)...)
		r.Client = panickingListClient{Client: r.Client}
		require.NotNil(t, recovered(func() { _, _ = resolve(t, r, vm) }))
		lockIsFree(t, r)
	})

	t.Run("resize", func(t *testing.T) {
		vm := wantsCPU(sized("app", 2), 4)
		r := resizeFixture(t, runningRoutingProvider(), vm)
		r.Client = panickingListClient{Client: r.Client}
		require.NotNil(t, recovered(func() {
			_, _, _ = r.admitClusteredResize(context.Background(), getVM(t, r, "app"),
				withRuntime(clusteredProviderCR("prov-cluster", capNS)), smallVMClass(capNS), "host-alpha")
		}))
		lockIsFree(t, r)
	})
}
