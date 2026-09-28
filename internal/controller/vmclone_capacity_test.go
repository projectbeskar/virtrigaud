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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the capacity check of a CLUSTERED clone (vmclone_capacity.go):
// a clone is admitted against the free capacity of its source's host — the
// only host it can land on — under the per-Provider assume lock it shares with
// the VirtualMachine controller, before its target's pending host is recorded
// and before the Clone RPC; a clone that does not fit waits with
// Placed=False/Unschedulable on its target and is never sent or moved to
// another host; and a bound clone's target is recorded at the size actually
// cloned, with the source's balloon ceiling.

// sizedSource is boundSource with a recorded size (4 vCPU, 8 GiB, larger than
// its 2 vCPU class: a spec.resources override) and balloon ceiling.
func sizedSource() *infrav1beta1.VirtualMachine {
	src := boundSource()
	cpu, mem, ceiling := int32(4), int64(8192), int64(32768)
	src.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	src.Status.Placement.MemoryCeilingMiB = &ceiling
	return src
}

// smallCloneHost is host-alpha with cpu vCPUs of allocatable capacity.
func smallCloneHost(cpu int32) *infrav1beta1.Host {
	h := readyCloneHost("host-alpha", "prov-c")
	h.Status.AllocatableCPU = i32p(cpu)
	return h
}

func TestVMClone_Clustered_CloneThatFitsProceedsAtItsSize(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	src := sizedSource()
	// The source's own 4 vCPU are committed on the 8 vCPU host: 4 are free.
	r, clone := clusteredCloneFixture(t, src, cp, smallCloneHost(8))
	var atClone *infrav1beta1.VirtualMachine
	cp.onClone = func() { atClone = getTarget(t, r) }

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.Equal(t, 1, cp.cloneCnt, "a clone that fits is sent")
	require.NotNil(t, atClone)
	pl := atClone.Status.Placement
	require.NotNil(t, pl)
	assert.Equal(t, "host-alpha", pl.PendingHost)
	require.NotNil(t, pl.PendingResources, "the admitted size is recorded with the pending host")
	assert.Equal(t, infrav1beta1.PlacementResources{CPU: 4, MemoryMiB: 8192}, *pl.PendingResources,
		"without spec.target.classRef the clone has the source's own size")

	target := getTarget(t, r)
	require.NotNil(t, target.Status.CurrentResources, "the bound clone is recorded at the size actually cloned")
	assert.EqualValues(t, 4, *target.Status.CurrentResources.CPU)
	assert.EqualValues(t, 8192, *target.Status.CurrentResources.MemoryMiB)
	require.NotNil(t, target.Status.Placement.MemoryCeilingMiB, "the source's balloon ceiling is copied")
	assert.EqualValues(t, 32768, *target.Status.Placement.MemoryCeilingMiB)
	assert.Nil(t, target.Status.Placement.PendingResources, "a bound VM has no pending size")
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
}

func TestVMClone_Clustered_CloneThatDoesNotFitWaitsWithoutARPC(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	// The source's 4 vCPU fill the 6 vCPU host: the 4 vCPU clone does not fit.
	r, clone := clusteredCloneFixture(t, sizedSource(), cp, smallCloneHost(6), readyCloneHost("host-beta", "prov-c"))

	res := reconcileClone(t, r, clone, 3)

	assert.Zero(t, cp.cloneCnt, "a clone that does not fit is never sent")
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "it waits, it does not fail")
	assert.Equal(t, k8s.ReasonUnschedulable, cloneReadyCondition(t, got).Reason)
	assert.Positive(t, res.RequeueAfter, "it is re-checked with a backoff")

	target := getTarget(t, r)
	placed := meta.FindStatusCondition(target.Status.Conditions, k8s.ConditionPlaced)
	require.NotNil(t, placed, "the target VM says why")
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonUnschedulable, placed.Reason)
	assert.Contains(t, placed.Message, "4 vCPU")
	assert.Contains(t, placed.Message, "host-alpha")
	for _, leak := range []string{"committed", "free 2", "of 6", "src-c"} {
		assert.NotContains(t, placed.Message, leak, "no committed figure or other VM reaches the tenant (M3)")
	}
	if pl := target.Status.Placement; pl != nil {
		assert.Empty(t, pl.PendingHost, "nothing is recorded on the host")
		assert.Empty(t, pl.Host)
	}
	assert.NotContains(t, placed.Message, "host-beta", "no other host is considered")
}

// TestVMClone_Clustered_HotAddCloneCountsAtItsCeiling: a clone whose domain can
// balloon up to a memory ceiling is admitted at that ceiling (memoryCeilingOf,
// as the accounting counts it): the source's recorded ceiling when it has one,
// else its class's memory hot-add setting.
func TestVMClone_Clustered_HotAddCloneCountsAtItsCeiling(t *testing.T) {
	memHost := func(memMiB int64) *infrav1beta1.Host {
		h := readyCloneHost("host-alpha", "prov-c")
		h.Status.AllocatableMemoryMiB = i64p(memMiB)
		return h
	}
	t.Run("the source's recorded ceiling", func(t *testing.T) {
		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
		// The source counts at its 32768 MiB ceiling: 8192 MiB are free. The
		// clone's 8192 MiB would fit; its 32768 MiB ceiling does not.
		r, clone := clusteredCloneFixture(t, sizedSource(), cp, memHost(40960))
		reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
		assert.Zero(t, cp.cloneCnt)
		placed := meta.FindStatusCondition(getTarget(t, r).Status.Conditions, k8s.ConditionPlaced)
		require.NotNil(t, placed)
		assert.Contains(t, placed.Message, "32768 MiB", "the clone is counted at its balloon ceiling")
	})
	t.Run("the class's hot-add setting when the source records none", func(t *testing.T) {
		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
		src := boundSource() // 2 vCPU, 4 GiB class, nothing recorded
		hotAdd := srcCloneClass()
		hotAdd.Spec.PerformanceProfile = &infrav1beta1.PerformanceProfile{MemoryHotAddEnabled: true}
		// The source itself counts at its 16 GiB hot-add ceiling: 8 GiB are free.
		r, clone := clusteredCloneFixtureWithClass(t, src, cp, hotAdd, memHost(24576))
		reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
		assert.Zero(t, cp.cloneCnt, "4 GiB would fit, its 16 GiB hot-add ceiling does not")
		placed := meta.FindStatusCondition(getTarget(t, r).Status.Conditions, k8s.ConditionPlaced)
		require.NotNil(t, placed)
		assert.Contains(t, placed.Message, "16384 MiB")
	})
}

// TestVMClone_Clustered_AdmittedPendingCloneIsNotReChecked: a target whose
// pending host is already recorded holds its capacity durably (an earlier
// attempt admitted it); a retry is sent even if the host has filled since.
func TestVMClone_Clustered_AdmittedPendingCloneIsNotReChecked(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c-target", Namespace: "default", UID: "uid-target",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"}, ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}},
		Status: infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{PendingHost: "host-alpha",
			PendingResources: &infrav1beta1.PlacementResources{CPU: 4, MemoryMiB: 8192}}},
	}
	r, clone := clusteredCloneFixture(t, sizedSource(), cp, smallCloneHost(6), target)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	assert.Equal(t, 1, cp.cloneCnt, "the retry of an admitted clone is sent")
}

// TestVMClone_Clustered_TargetClassSizesTheClone: with spec.target.classRef the
// clone gets that class's size (the provider applies it), not the source's.
func TestVMClone_Clustered_TargetClassSizesTheClone(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	big := srcCloneClass()
	big.Name = "big"
	big.Spec.CPU = 16
	r, clone := clusteredCloneFixture(t, sizedSource(), cp, smallCloneHost(8), big)
	clone.Spec.Target.ClassRef = &infrav1beta1.LocalObjectReference{Name: "big"}
	require.NoError(t, r.Update(context.Background(), clone))

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "a 16 vCPU clone does not fit in the 4 free vCPU")
	placed := meta.FindStatusCondition(getTarget(t, r).Status.Conditions, k8s.ConditionPlaced)
	require.NotNil(t, placed)
	assert.Contains(t, placed.Message, "16 vCPU")
}

// TestVMClone_Clustered_UnknownSizeWaits: a clone whose size cannot be read
// (its VMClass is missing) is not admitted blind: it waits.
func TestVMClone_Clustered_UnknownSizeWaits(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	src := boundSource()
	src.Spec.ClassRef = infrav1beta1.ObjectRef{Name: "missing"}
	r, clone := clusteredCloneFixture(t, src, cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	assert.Zero(t, cp.cloneCnt)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Contains(t, cloneReadyCondition(t, got).Message, "size cannot be determined")
}

// ─── concurrent create and clone ─────────────────────────────────────────────

// createAndCloneFixture is a clustered Provider whose host-a has room for ONE
// more 2 vCPU VM (4 vCPU; the clone's 2 vCPU source already holds 2), a VM
// "a" to create and a clone of the source, both read through a lagging
// (informer-like) client and sharing the VirtualMachine controller's assume
// cache, as in the manager.
func createAndCloneFixture(t *testing.T) (*VirtualMachineReconciler, *VMCloneReconciler, *clonerProvider, *infrav1beta1.VMClone, *laggingClient) {
	t.Helper()
	cpu, mem := int32(2), int64(4096)
	src := capVM("src")
	src.Status.ID = "default.src"
	src.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: capNS, Name: "prov-cluster"}
	src.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-a", Pool: "pool-a"}
	src.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "copy", Namespace: capNS, UID: "uid-copy",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-clone"}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-cluster"}, ClassRef: infrav1beta1.ObjectRef{Name: "test-class"}},
	}
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone", Namespace: capNS, UID: "uid-clone", Finalizers: []string{vmCloneFinalizer}},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src"}},
			Target: infrav1beta1.VMCloneTarget{Name: "copy"},
		},
	}
	objs := append(capBase(), capHost("host-a", 4), capVM("a"), src, target, clone)
	s := cloneTestScheme(t)
	lc := newCloneLaggingClient(t, s, objs...)

	vmr := &VirtualMachineReconciler{Client: lc, Scheme: s}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.copy"}}
	cp.caps.SupportsClustering = true
	cp.caps.SupportsRoutedClone = true
	cr := &VMCloneReconciler{Client: lc, Scheme: s, RemoteResolver: &stubResolver{provider: cp},
		Recorder: record.NewFakeRecorder(50), Placements: vmr.PlacementAssumptions()}
	return vmr, cr, cp, clone, lc
}

// newCloneLaggingClient is newLaggingClient whose stores also serve the
// VMClone status subresource (the clone reconciler writes it).
func newCloneLaggingClient(t *testing.T, s *runtime.Scheme, objs ...client.Object) *laggingClient {
	t.Helper()
	build := func() client.Client {
		copies := make([]client.Object, 0, len(objs))
		for _, o := range objs {
			c, ok := o.DeepCopyObject().(client.Object)
			require.True(t, ok, "deep copy of %T is not a client.Object", o)
			copies = append(copies, c)
		}
		return fake.NewClientBuilder().WithScheme(s).WithObjects(copies...).
			WithIndex(&infrav1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
			WithStatusSubresource(&infrav1beta1.VirtualMachine{}, &infrav1beta1.VMClone{}).Build()
	}
	return &laggingClient{Client: build(), scheme: s, reader: build()}
}

// catchUpWithClones is laggingClient.catchUp for a store that also holds
// VMClones: the snapshot shows everything the live store has.
func catchUpWithClones(t *testing.T, c *laggingClient) {
	t.Helper()
	ctx := context.Background()
	clones := &infrav1beta1.VMCloneList{}
	require.NoError(t, c.Client.List(ctx, clones))
	c.catchUp(t)
	var objs []client.Object
	for _, l := range []client.ObjectList{
		&infrav1beta1.ProviderList{}, &infrav1beta1.HostPoolList{}, &infrav1beta1.HostList{},
		&infrav1beta1.VMClassList{}, &infrav1beta1.VMImageList{}, &infrav1beta1.VirtualMachineList{},
	} {
		require.NoError(t, c.Client.List(ctx, l))
		items, err := metaItems(l)
		require.NoError(t, err)
		objs = append(objs, items...)
	}
	for i := range clones.Items {
		objs = append(objs, &clones.Items[i])
	}
	fresh := fake.NewClientBuilder().WithScheme(c.scheme).WithObjects(objs...).
		WithIndex(&infrav1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithStatusSubresource(&infrav1beta1.VirtualMachine{}, &infrav1beta1.VMClone{}).Build()
	c.mu.Lock()
	c.reader = fresh
	c.mu.Unlock()
}

func TestClusteredCapacity_ConcurrentCreateAndCloneNeverOverbook(t *testing.T) {
	t.Run("the create schedules first", func(t *testing.T) {
		vmr, cr, cp, clone, lc := createAndCloneFixture(t)
		host, _ := resolve(t, vmr, readVM(t, lc, "a"))
		require.Equal(t, "host-a", host, "the create takes the host's last 2 vCPU")
		// Its pendingHost write is not in the (lagging) cache yet: only the
		// shared assumption says the capacity is taken.
		_, err := cr.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
		assert.Zero(t, cp.cloneCnt, "the clone sees the create's assumption and is not sent")
		live := &infrav1beta1.VMClone{}
		require.NoError(t, lc.Client.Get(context.Background(), client.ObjectKeyFromObject(clone), live))
		assert.Equal(t, k8s.ReasonUnschedulable, cloneReadyCondition(t, live).Reason)
	})
	t.Run("the clone is admitted first", func(t *testing.T) {
		vmr, cr, cp, clone, lc := createAndCloneFixture(t)
		_, err := cr.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
		require.Equal(t, 1, cp.cloneCnt, "the clone takes the host's last 2 vCPU")
		// The clone's pendingHost / binding are not in the (lagging) cache
		// yet: only the shared assumption says the capacity is taken.
		host, res := resolve(t, vmr, readVM(t, lc, "a"))
		assert.Empty(t, host, "the create sees the clone's assumption: no host can take it")
		assert.Positive(t, res.RequeueAfter)
	})
	t.Run("once the cache catches up, the records alone keep it", func(t *testing.T) {
		vmr, cr, cp, clone, lc := createAndCloneFixture(t)
		_, err := cr.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
		require.Equal(t, 1, cp.cloneCnt)
		catchUpWithClones(t, lc)
		pendingCopy := readVM(t, lc, "copy")
		require.NotNil(t, pendingCopy.Status.Placement)
		assert.Equal(t, "host-a", pendingCopy.Status.Placement.PendingHost)
		require.NotNil(t, pendingCopy.Status.Placement.PendingResources, "the pending clone records its admitted size")
		host, _ := resolve(t, vmr, readVM(t, lc, "a"))
		assert.Empty(t, host, "the pending clone counts at its admitted size")

		// The bind (its reads now current) records the size actually cloned.
		_, err = cr.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
		catchUpWithClones(t, lc)
		copyVM := readVM(t, lc, "copy")
		assert.Equal(t, "host-a", boundHost(copyVM))
		require.NotNil(t, copyVM.Status.CurrentResources)
		assert.EqualValues(t, 2, *copyVM.Status.CurrentResources.CPU)
		assert.EqualValues(t, 4096, *copyVM.Status.CurrentResources.MemoryMiB)
		host, _ = resolve(t, vmr, readVM(t, lc, "a"))
		assert.Empty(t, host, "the bound clone counts at its recorded size")
	})
}

// TestVMCloneReconciler_SharesTheVMControllersAssumeCache pins the wiring the
// manager relies on: the clone admission uses the cache it is given.
func TestVMCloneReconciler_SharesTheVMControllersAssumeCache(t *testing.T) {
	vmr := &VirtualMachineReconciler{}
	cr := &VMCloneReconciler{Placements: vmr.PlacementAssumptions()}
	assert.Same(t, vmr.PlacementAssumptions(), cr.placementAssumptions())
	own := &VMCloneReconciler{}
	assert.NotNil(t, own.placementAssumptions(), "a reconciler built without one uses its own")
	assert.Same(t, own.placementAssumptions(), own.placementAssumptions())
}
