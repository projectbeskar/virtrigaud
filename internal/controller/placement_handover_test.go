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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// These tests pin inherited admission revalidation (ADR-0007 A5) across a
// leader handover: leader A's checked admission write reaches the API server
// only after leader B — a fresh reconciler, with its own empty assume cache,
// on the same API server — has given the capacity to another VM. B must not
// execute A's late admission without checking it.

// delayedWrite holds back the first status write of one VM, as a write sent
// by a leader that lost its lease before the answer came: the sender gets a
// timeout, and the write is stored only when commit is called.
type delayedWrite struct {
	mu   sync.Mutex
	held client.Object
}

// funcs is the interceptor that holds back name's first status write.
func (d *delayedWrite) funcs(name string) interceptor.Funcs {
	var once sync.Once
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			hold := false
			if obj.GetName() == name {
				once.Do(func() { hold = true })
			}
			if !hold {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			copied, ok := obj.DeepCopyObject().(client.Object)
			if !ok {
				return apierrors.NewInternalError(nil)
			}
			d.held = copied
			return apierrors.NewTimeoutError("the leader lost its lease before the answer", 1)
		},
	}
}

// commit stores the held write now, with its original resourceVersion
// precondition: the late landing of the old leader's request.
func (d *delayedWrite) commit(t *testing.T, live client.Client) client.Object {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotNil(t, d.held, "a write was held")
	require.NoError(t, live.Status().Update(context.Background(), d.held), "the late write still lands")
	return d.held
}

// boundCPUOn sums the vCPUs of the VMs bound to host in the authoritative
// (live) store: what has actually been created there.
func boundCPUOn(t *testing.T, live client.Client, host string) int32 {
	t.Helper()
	var vms infrav1beta1.VirtualMachineList
	require.NoError(t, live.List(context.Background(), &vms))
	var sum int32
	for _, vm := range vms.Items {
		if vm.Status.Placement != nil && vm.Status.Placement.Host == host {
			sum += admittedFootprint(&vm, smallVMClass(capNS)).CPU
		}
	}
	return sum
}

// handoverObjects: host-alpha with 8 vCPU, a neighbour bound with 6 of them,
// and two new 2-vCPU VMs that cannot both fit.
func handoverObjects() []client.Object {
	return append(capBase(), capHost("host-alpha", 8), sized("neighbour", 6), capVM("vm-a"), capVM("vm-b"))
}

// placedReason returns the VM's Placed condition reason.
func placedReason(vm *infrav1beta1.VirtualMachine) string {
	if c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionPlaced); c != nil {
		return c.Reason
	}
	return ""
}

func TestHandover_LatePendingHostWriteIsNotExecuted(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var late delayedWrite
	lc := laggingClientWith(t, coverageTestScheme(t), late.funcs("vm-a"), handoverObjects()...)

	// Leader A admits vm-a to the last 2 vCPU; its pendingHost write is held
	// back and A loses its lease before the answer (no Create).
	leaderA, provA := safetyReconciler(lc, &now), &concurrentCreateProvider{}
	_, err := createOnce(t, leaderA, lc, provA, "vm-a")
	require.Error(t, err)
	require.Empty(t, provA.created())

	// Leader B, with a fresh assume cache, sees no pendingHost on vm-a and
	// admits and creates vm-b in the same 2 vCPU.
	lc.catchUp(t)
	leaderB, provB := safetyReconciler(lc, &now), &concurrentCreateProvider{}
	_, err = createOnce(t, leaderB, lc, provB, "vm-b")
	require.NoError(t, err)
	require.Equal(t, []string{"host-alpha"}, provB.created())

	// A's write lands now, and B reconciles vm-a.
	held := late.commit(t, lc.Client)
	heldVM, ok := held.(*infrav1beta1.VirtualMachine)
	require.True(t, ok)
	require.Equal(t, "host-alpha", heldVM.Status.Placement.PendingHost)
	lc.catchUp(t)
	_, err = createOnce(t, leaderB, lc, provB, "vm-a")
	require.NoError(t, err)

	assert.Equal(t, []string{"host-alpha"}, provB.created(), "vm-a is not created")
	got := readVM(t, lc.Client, "vm-a")
	assert.Equal(t, "host-alpha", got.Status.Placement.PendingHost, "its pending host is kept, not cleared or re-scheduled")
	assert.Equal(t, k8s.ReasonInheritedAdmissionOverCapacity, placedReason(got))
	assert.LessOrEqual(t, boundCPUOn(t, lc.Client, "host-alpha"), int32(8), "nothing created beyond the host's capacity")
}

func TestHandover_PendingHostVisibleBeforeTheNewLeaderDecides(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var late delayedWrite
	lc := laggingClientWith(t, coverageTestScheme(t), late.funcs("vm-a"), handoverObjects()...)
	leaderA := safetyReconciler(lc, &now)
	_, err := createOnce(t, leaderA, lc, &concurrentCreateProvider{}, "vm-a")
	require.Error(t, err)
	late.commit(t, lc.Client) // lands before B decides
	lc.catchUp(t)

	leaderB, provB := safetyReconciler(lc, &now), &concurrentCreateProvider{}
	_, err = createOnce(t, leaderB, lc, provB, "vm-b")
	require.NoError(t, err)
	assert.Empty(t, provB.created(), "vm-b cannot take capacity vm-a's record holds")

	_, err = createOnce(t, leaderB, lc, provB, "vm-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"host-alpha"}, provB.created(), "the inherited admission still fits (its own record is not counted twice): created")
	assert.LessOrEqual(t, boundCPUOn(t, lc.Client, "host-alpha"), int32(8))
}

func TestHandover_LateResizeReservationIsNotExecuted(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var late delayedWrite
	objs := []client.Object{withRuntime(clusteredProviderCR("prov-cluster", capNS)), hostPoolCR("pool-a", capNS, "prov-cluster"),
		capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4),
		wantsCPU(sized("app", 2), 4), capVM("newvm")}
	lc := laggingClientWith(t, coverageTestScheme(t), late.funcs("app"), objs...)

	leaderA := safetyReconciler(lc, &now)
	admitted, err := admitApp(t, leaderA, readVM(t, lc, "app"))
	require.Error(t, err)
	require.False(t, admitted, "A never learns the reservation was stored: no Reconfigure")

	lc.catchUp(t)
	leaderB, provB := safetyReconciler(lc, &now), &concurrentCreateProvider{}
	_, err = createOnce(t, leaderB, lc, provB, "newvm")
	require.NoError(t, err)
	require.Equal(t, []string{"host-alpha"}, provB.created(), "B gives the 2 vCPU to newvm")

	late.commit(t, lc.Client)
	lc.catchUp(t)
	admitted, err = admitApp(t, leaderB, readVM(t, lc, "app"))
	require.NoError(t, err)
	assert.False(t, admitted, "the inherited reservation's growth no longer fits: no Reconfigure")
	got := readVM(t, lc.Client, "app")
	require.NotNil(t, got.Status.Placement.ResizeReservation, "the reservation is kept")
	assert.Equal(t, int32(4), got.Status.Placement.ResizeReservation.CPU)
	assert.Equal(t, k8s.ReasonInheritedAdmissionOverCapacity, reconfiguringCondition(got).Reason)
	assert.Equal(t, int32(2), *got.Status.CurrentResources.CPU, "the applied size is unchanged")
}

func TestHandover_InheritedResizeThatFitsProceedsOnce(t *testing.T) {
	r := resizeFixture(t, runningRoutingProvider(), wantsCPU(sized("app", 2), 4))
	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)

	r2 := restartedVM(r)
	admitted, err = admitApp(t, r2, getVM(t, r2, "app"))
	require.NoError(t, err)
	assert.True(t, admitted, "the inherited reservation still fits: revalidated (its own reservation not counted twice)")
	adm, ok := resizeAdmissionOf(getVM(t, r2, "app"), providerKeyOf(clusteredProviderCR("prov-cluster", capNS)))
	require.True(t, ok)
	assert.True(t, r2.placementAssumptions().Established(vmSchedulingUID(getVM(t, r2, "app")), adm), "and established")
}

func TestHandover_InheritedCloneOverCapacityIsNotCloned(t *testing.T) {
	for _, tc := range []struct {
		hostCPU int32
		cloned  int
	}{{hostCPU: 6, cloned: 0}, {hostCPU: 8, cloned: 1}} {
		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
		target := clonePendingTarget()
		r, clone := clusteredCloneFixture(t, sizedSource(), cp, smallCloneHost(tc.hostCPU), target)
		reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
		assert.Equal(t, tc.cloned, cp.cloneCnt, "host %d vCPU (source 4 + clone 4)", tc.hostCPU)
		if tc.cloned == 0 {
			got := getTarget(t, r)
			assert.Equal(t, "host-alpha", got.Status.Placement.PendingHost, "the pending host is kept")
			assert.Equal(t, k8s.ReasonInheritedAdmissionOverCapacity, placedReason(got))
		}
	}
}

// clonePendingTarget is a clone target an earlier manager instance admitted
// on host-alpha at 4 vCPU / 8192 MiB.
func clonePendingTarget() *infrav1beta1.VirtualMachine {
	vm := &infrav1beta1.VirtualMachine{}
	vm.Name, vm.Namespace, vm.UID = "clone-c-target", "default", "uid-target"
	vm.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue}
	vm.Annotations = map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}
	vm.Spec = infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"}, ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}}
	vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha",
		PendingResources: &infrav1beta1.PlacementResources{CPU: 4, MemoryMiB: 8192}}
	return vm
}

// TestHandover_ChangedAdmissionIsRevalidated: an admission this instance
// established authorizes only exactly itself; a record that differs (here, a
// larger pending size) is checked again.
func TestHandover_ChangedAdmissionIsRevalidated(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	vmA := withPlacement(capVM("vm-a"), "", "host-alpha")
	vmA.Status.Placement.PendingResources = &infrav1beta1.PlacementResources{CPU: 2, MemoryMiB: 4096}
	vmB := withPlacement(capVM("vm-b"), "host-alpha", "")
	lc := newLaggingClient(t, coverageTestScheme(t), append(capBase(), capHost("host-alpha", 8), sized("neighbour", 6), vmA, vmB)...)
	r, prov := safetyReconciler(lc, &now), &concurrentCreateProvider{}
	key := providerKeyOf(clusteredProviderCR("prov-cluster", capNS))

	stale := pendingAdmissionOf(readVM(t, lc, "vm-a"), key, assume.AdmissionCreate, "")
	stale.Resources = scheduler.ResourceRequest{CPU: 1, MemoryMiB: 4096} // what this instance once admitted
	r.placementAssumptions().Establish("uid-vm-a", stale)
	_, err := createOnce(t, r, lc, prov, "vm-a")
	require.NoError(t, err)
	assert.Empty(t, prov.created(), "a changed admission is revalidated, and 6 + 2 + 2 > 8 holds it")
	assert.Equal(t, k8s.ReasonInheritedAdmissionOverCapacity, placedReason(readVM(t, lc.Client, "vm-a")))

	r.placementAssumptions().Establish("uid-vm-a", pendingAdmissionOf(readVM(t, lc, "vm-a"), key, assume.AdmissionCreate, ""))
	_, err = createOnce(t, r, lc, prov, "vm-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"host-alpha"}, prov.created(), "exactly the admission this instance established proceeds")
}

// TestHandover_BoundVMLifecycleIsNotHeld: a bound VM on an over-committed
// host, holding an inherited reservation that no longer fits, is still
// described and powered — and a shrink-free steady state is never held.
func TestHandover_BoundVMLifecycleIsNotHeld(t *testing.T) {
	app := sized("app", 2)
	app.Status.Placement.ResizeReservation = &infrav1beta1.PlacementResources{CPU: 4, MemoryMiB: 4096}
	app.Spec.PowerState = infrav1beta1.PowerStateOff
	prov := runningRoutingProvider()
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov},
		withRuntime(clusteredProviderCR("prov-cluster", capNS)), hostPoolCR("pool-a", capNS, "prov-cluster"),
		capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 8), app)
	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.NotEmpty(t, prov.describeRefs, "described")
	assert.Len(t, prov.powerRefs, 1, "powered off as its spec asks")
	assert.Empty(t, prov.reconfigureRefs)
	if c := reconfiguringCondition(getVM(t, r, "app")); c != nil {
		assert.NotEqual(t, k8s.ReasonInheritedAdmissionOverCapacity, c.Reason)
	}
}
