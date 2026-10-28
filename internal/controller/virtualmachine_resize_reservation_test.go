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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// These tests pin the durable resize reservation (G3): an admitted clustered
// resize-up is written to status.placement.resizeReservation before its
// Reconfigure is sent, counts from then on whatever happens to the manager,
// and is released only once the provider has established the applied size.

// reportingProvider is a running routing provider whose Describe reports the
// VM's vCPUs and memory maximum, so a resize outcome can be established.
func reportingProvider(cpu int32, maxMemMiB int64) *routingProvider {
	p := runningRoutingProvider()
	p.describeResp.VCPUs = cpu
	p.describeResp.MaxMemoryMiB = maxMemMiB
	return p
}

// restartedVM is r after a manager restart or leader change: the same API
// server, an empty assume cache and no in-memory write records.
func restartedVM(r *VirtualMachineReconciler) *VirtualMachineReconciler {
	return &VirtualMachineReconciler{Client: r.Client, Scheme: r.Scheme, RemoteResolver: r.RemoteResolver, APIReader: r.APIReader, clock: r.clock}
}

// reservationOf returns the VM's durable resize reservation, nil when none.
func reservationOf(t *testing.T, r *VirtualMachineReconciler, name string) *infravirtrigaudiov1beta1.PlacementResources {
	t.Helper()
	vm := getVM(t, r, name)
	if vm.Status.Placement == nil {
		return nil
	}
	return vm.Status.Placement.ResizeReservation
}

// admitApp runs the resize gate for app on host-alpha.
func admitApp(t *testing.T, r *VirtualMachineReconciler, vm *infravirtrigaudiov1beta1.VirtualMachine) (bool, error) {
	t.Helper()
	_, admitted, err := r.admitClusteredResize(context.Background(), vm,
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), smallVMClass(capNS), "host-alpha")
	return admitted, err
}

// newVMFits reports whether a new 2-vCPU VM can still be placed on the
// Provider (host-alpha: 8 vCPU, the neighbour holds 4).
func newVMFits(t *testing.T, r *VirtualMachineReconciler) bool {
	t.Helper()
	host, _ := resolve(t, r, getVM(t, r, "newvm"))
	if host != "" {
		r.placementAssumptions().Forget("uid-newvm") // a probe, not a placement
	}
	return host != ""
}

// interceptedClient wraps the fake client c with funcs.
func interceptedClient(t *testing.T, c client.Client, funcs interceptor.Funcs) client.Client {
	t.Helper()
	ww, ok := c.(client.WithWatch)
	require.True(t, ok, "the fake client supports watches")
	return interceptor.NewClient(ww, funcs)
}

// TestResizeReservation_WrittenBeforeReconfigure: when the provider is asked
// to resize, the reservation is already stored.
func TestResizeReservation_WrittenBeforeReconfigure(t *testing.T) {
	prov := runningRoutingProvider()
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	var reservedAtCall *infravirtrigaudiov1beta1.PlacementResources
	prov.onReconfigure = func() { reservedAtCall = reservationOf(t, r, "app") }
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1)
	require.NotNil(t, reservedAtCall, "stored before the call")
	assert.Equal(t, int32(4), reservedAtCall.CPU)
	assert.False(t, newVMFits(t, restartedVM(r)), "a restarted manager counts it: 4 + 4 of 8 vCPU")
}

// TestResizeReservation_RestartBeforeReconfigure: the reservation was stored,
// then the manager stopped before the Reconfigure. The VM keeps counting at
// it, and the next manager sends the Reconfigure under it without admitting
// again.
func TestResizeReservation_RestartBeforeReconfigure(t *testing.T) {
	prov := reportingProvider(2, 4096) // the domain is still at its old size
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)
	require.Empty(t, prov.reconfigureRefs, "stopped before the call")

	r2 := restartedVM(r)
	assert.False(t, newVMFits(t, r2), "the reservation counts with an empty assume cache")
	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1, "the Reconfigure is sent under the recorded reservation")
	assert.Empty(t, assumedUIDs(r2), "without a new admission")
	assert.False(t, newVMFits(t, r2))
}

// TestResizeReservation_RestartAfterApplyBeforeRecord: the provider applied
// the resize, then the manager stopped before status.currentResources
// recorded it. The reservation keeps the host from being overbooked; the
// next manager re-sends the declarative Reconfigure, records the size, and
// releases the reservation only once Describe has established it.
func TestResizeReservation_RestartAfterApplyBeforeRecord(t *testing.T) {
	prov := runningRoutingProvider()
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)
	_, err = prov.Reconfigure(context.Background(), contracts.VMRef{HostID: "host-alpha"}, contracts.CreateRequest{})
	require.NoError(t, err) // applied on the host; the manager dies here

	r2 := restartedVM(r)
	assert.Equal(t, int32(2), *getVM(t, r2, "app").Status.CurrentResources.CPU, "not recorded")
	assert.False(t, newVMFits(t, r2), "but reserved")

	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
	require.NoError(t, err)
	assert.Equal(t, int32(4), *getVM(t, r2, "app").Status.CurrentResources.CPU, "recorded after the re-sent Reconfigure")
	require.NotNil(t, reservationOf(t, r2, "app"), "kept: no Describe has reported the applied size yet")
	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
	require.NoError(t, err)
	require.NotNil(t, reservationOf(t, r2, "app"), "nor in a steady pass whose Describe reports no size")

	prov.describeResp.VCPUs, prov.describeResp.MaxMemoryMiB = 4, 4096
	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
	require.NoError(t, err)
	assert.Nil(t, reservationOf(t, r2, "app"), "released once established")
	assert.False(t, newVMFits(t, r2), "the record now counts the 4 vCPU")
}

// TestResizeReservation_AsyncTaskSurvivesFailedChecksAndRestart: a resize
// running as a provider task stays reserved while the task's checks fail,
// across a restart and past every TTL, and is released after it completes.
func TestResizeReservation_AsyncTaskSurvivesFailedChecksAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	prov := reportingProvider(2, 4096)
	prov.reconfigureResult.TaskRef = "task-1"
	var taskDone bool
	var taskErr error
	prov.IsTaskCompleteFn = func(context.Context, string) (bool, error) { return taskDone, taskErr }
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	r.clock = func() time.Time { return now }

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Equal(t, "task-1", getVM(t, r, "app").Status.ReconfigureTaskRef)

	taskErr = errors.New("task service unavailable")
	for range 3 {
		now = now.Add(resizeAssumeTTL)
		_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
	}
	assert.False(t, newVMFits(t, r), "failed task checks past the TTL release nothing")
	r2 := restartedVM(r)
	assert.False(t, newVMFits(t, r2), "nor does a restart")

	taskErr, taskDone = nil, true
	prov.describeResp.VCPUs = 4
	_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
	require.NoError(t, err)
	got := getVM(t, r2, "app")
	assert.Empty(t, got.Status.ReconfigureTaskRef)
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU)
	assert.Nil(t, got.Status.Placement.ResizeReservation, "released once the task completed and Describe confirmed it")
	assert.Len(t, prov.reconfigureRefs, 1, "the task was never sent a second time")
}

// TestResizeReservation_FailedReconfigureKeepsIt: a failed (possibly partly
// applied) Reconfigure releases nothing, even when Describe reports a
// smaller size; the reservation goes only after a later success.
func TestResizeReservation_FailedReconfigureKeepsIt(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	prov := reportingProvider(2, 4096)
	prov.reconfigureErr = contracts.NewRetryableError("reconfigure timed out", nil)
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	r.clock = func() time.Time { return now }

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got := getVM(t, r, "app")
	assert.Equal(t, k8s.ReasonProviderError, reconfiguringCondition(got).Reason)
	require.NotNil(t, got.Status.Placement.ResizeReservation)
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "counted at the larger size (review H1)")

	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app")) // inside the backoff
	require.NoError(t, err)
	assert.NotNil(t, reservationOf(t, r, "app"), "an unresolved failure keeps it")

	prov.reconfigureErr = nil
	prov.describeResp.VCPUs = 4
	now = now.Add(reconfigureRetryMaxInterval)
	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Nil(t, reservationOf(t, r, "app"), "released after the retry succeeded and was confirmed")
}

// TestResizeReservation_RestartRequiredKeepsIt: a change the provider applied
// to the definition only keeps its reservation until a power cycle and a
// confirmed result.
func TestResizeReservation_RestartRequiredKeepsIt(t *testing.T) {
	prov := reportingProvider(2, 4096)
	prov.reconfigureResult.RestartRequired = true
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4), capVM("newvm"))
	for range 2 {
		_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
		require.NoError(t, err)
	}
	assert.True(t, restartPending(getVM(t, r, "app")))
	assert.NotNil(t, reservationOf(t, r, "app"))
}

// TestResizeReservation_AmbiguousWrite: a reservation write whose answer was
// lost sends no Reconfigure; its assumption keeps counting and is never
// replaced, whether the write landed or not, until a later admission sees or
// stores the reservation.
func TestResizeReservation_AmbiguousWrite(t *testing.T) {
	for _, landed := range []bool{true, false} {
		now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
		providerCR := withRuntime(clusteredProviderCR("prov-cluster", capNS))
		objs := []client.Object{providerCR, hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8),
			smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4), wantsCPU(sized("app", 2), 4), capVM("newvm")}
		lc := laggingClientWith(t, coverageTestScheme(t), ambiguousOnce("app", landed), objs...)
		r := safetyReconciler(lc, &now)

		admitted, err := admitApp(t, r, readVM(t, lc, "app"))
		require.Error(t, err, "landed=%v", landed)
		assert.False(t, admitted)
		a, _ := assumptionOf(t, r, "app")
		assert.Equal(t, assume.WriteInFlight, a.Write)

		now = now.Add(time.Hour)
		host, _ := resolve(t, r, readVM(t, lc, "newvm"))
		assert.Empty(t, host, "landed=%v: the unresolved reservation keeps the host from being overbooked", landed)

		// The next pass reads the lagging cache. A write that landed makes the
		// retry conflict (no Reconfigure yet); once the cache shows it, the
		// resize goes on under it. One that did not land is stored now.
		admitted, err = admitApp(t, r, readVM(t, lc, "app"))
		require.NoError(t, err)
		if landed {
			assert.False(t, admitted, "the conflicting retry is not admitted: no Reconfigure")
			a, _ = assumptionOf(t, r, "app")
			assert.Equal(t, assume.WriteInFlight, a.Write, "the conflict does not release a pick whose earlier write was ambiguous")
			host, _ = resolve(t, r, readVM(t, lc, "newvm"))
			assert.Empty(t, host, "and it still keeps the host from being overbooked")
			lc.catchUp(t)
			admitted, err = admitApp(t, r, readVM(t, lc, "app"))
			require.NoError(t, err)
		}
		assert.True(t, admitted, "landed=%v", landed)
		got := readVM(t, lc.Client, "app")
		require.NotNil(t, got.Status.Placement.ResizeReservation, "landed=%v", landed)
		assert.Equal(t, int32(4), got.Status.Placement.ResizeReservation.CPU)
	}
}

// TestResizeReservation_RejectedWriteReleases: a reservation write the API
// server refused outright releases the admission and sends nothing.
func TestResizeReservation_RejectedWriteReleases(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	refuse := interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "virtualmachines"}, "app", errors.New("denied"))
	}}
	lc := laggingClientWith(t, coverageTestScheme(t), refuse, withRuntime(clusteredProviderCR("prov-cluster", capNS)),
		hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS),
		sized("neighbour", 4), wantsCPU(sized("app", 2), 4))
	r := safetyReconciler(lc, &now)
	admitted, err := admitApp(t, r, readVM(t, lc, "app"))
	require.Error(t, err)
	assert.False(t, admitted)
	assert.Empty(t, assumedUIDs(r), "released")
}

// TestResizeReservation_PrunedFieldFailsClosed: an API server whose installed
// CRD predates the field stores the status write without it. The Reconfigure
// is not sent and the admission is released.
func TestResizeReservation_PrunedFieldFailsClosed(t *testing.T) {
	prov := runningRoutingProvider()
	r := resizeFixture(t, prov, wantsCPU(sized("app", 2), 4))
	r.Client = interceptedClient(t, r.Client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && vm.Status.Placement != nil {
				vm.Status.Placement.ResizeReservation = nil // an older CRD's schema prunes it
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Empty(t, prov.reconfigureRefs, "no Reconfigure without a durable reservation")
	got := getVM(t, r, "app")
	assert.Equal(t, int32(2), *got.Status.CurrentResources.CPU)
	c := reconfiguringCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonPlacementError, c.Reason)
	assert.Contains(t, c.Message, "resizeReservation")
	assert.Empty(t, assumedUIDs(r))
}

// TestResizeReservation_MixedChangeAndMemoryCeiling: a change growing CPU and
// shrinking memory reserves the larger of both per resource, and memory at
// the VM's balloon ceiling.
func TestResizeReservation_MixedChangeAndMemoryCeiling(t *testing.T) {
	vm := sized("app", 2)
	vm.Status.CurrentResources.MemoryMiB = i64p(8192)
	ceiling := int64(16384)
	vm.Status.Placement.MemoryCeilingMiB = &ceiling
	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4), MemoryMiB: i64p(4096)}
	r := resizeFixture(t, runningRoutingProvider(), vm)
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)
	res := reservationOf(t, r, "app")
	require.NotNil(t, res)
	assert.Equal(t, infravirtrigaudiov1beta1.PlacementResources{CPU: 4, MemoryMiB: 16384}, *res,
		"CPU grown, memory kept at its ceiling (not the smaller size asked for)")
	assert.Equal(t, scheduler.ResourceRequest{CPU: 4, MemoryMiB: 16384}, admittedFootprint(getVM(t, r, "app"), smallVMClass(capNS)))
}

// TestResizeReservation_SpecChangeWhileUnresolved: a smaller request while a
// reservation is outstanding never lowers it; a larger one is admitted only
// for its growth beyond it.
func TestResizeReservation_SpecChangeWhileUnresolved(t *testing.T) {
	r := resizeFixture(t, runningRoutingProvider(), wantsCPU(sized("app", 2), 4))
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)

	smaller := wantsCPU(getVM(t, r, "app"), 3)
	admitted, err = admitApp(t, r, smaller)
	require.NoError(t, err)
	assert.True(t, admitted, "within the reservation: goes on under it")
	assert.Equal(t, int32(4), reservationOf(t, r, "app").CPU, "never lowered while unresolved")

	larger := wantsCPU(getVM(t, r, "app"), 5) // 4 + 5 > 8
	admitted, err = admitApp(t, r, larger)
	require.NoError(t, err)
	assert.False(t, admitted, "growth beyond the reservation is checked")
	assert.Equal(t, int32(4), reservationOf(t, r, "app").CPU)
}

// TestResizeReservation_ConcurrentCreateAfterRestart: with the reservation
// durable, a create racing the resize's Reconfigure on a fresh manager is
// refused room on the host.
func TestResizeReservation_ConcurrentCreateAfterRestart(t *testing.T) {
	r := resizeFixture(t, runningRoutingProvider(), wantsCPU(sized("app", 2), 4), capVM("newvm"))
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)
	r2 := restartedVM(r)
	host, _ := resolve(t, r2, getVM(t, r2, "newvm"))
	assert.Empty(t, host)
	assert.Equal(t, int32(4), admittedFootprint(getVM(t, r2, "app"), smallVMClass(capNS)).CPU)
}
