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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the controller half of the VirtualMachine provider binding
// (status.boundProvider): it is recorded at every VirtualMachine bind path,
// backfilled for VMs bound before it existed, and — while the VM is bound — no
// provider is resolved or called, and the finalizer deletes through no
// Provider, unless spec.providerRef still resolves to the bound Provider
// object. They also pin the orphan-on-delete annotation.

const bpNS = "team-a"

// countingResolver is a ProviderResolver that counts every resolution and
// hands out the same provider.
type countingResolver struct {
	provider contracts.Provider
	calls    atomic.Int32
}

func (c *countingResolver) GetProvider(_ context.Context, _ *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	c.calls.Add(1)
	return c.provider, nil
}

// providerCRWithUID returns a single-host Provider CR with a runtime status and
// the given UID.
func providerCRWithUID(ns, name, uid string) *infravirtrigaudiov1beta1.Provider {
	p := withRuntime(singleProviderCR(name, ns))
	p.UID = types.UID(uid)
	return p
}

// boundVM returns a VM bound (status.id) through bound, whose spec.providerRef
// is ref, carrying the finalizer.
func boundVM(name string, ref infravirtrigaudiov1beta1.ObjectRef, bound *infravirtrigaudiov1beta1.BoundProviderRef) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := creatableVM(bpNS)
	vm.Name = name
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Spec.ProviderRef = ref
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = bound
	return vm
}

// bpReconciler builds a VM reconciler over a fake client with a counting
// resolver and a fake event recorder.
func bpReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) (*VirtualMachineReconciler, *countingResolver, *record.FakeRecorder) {
	t.Helper()
	res := &countingResolver{provider: prov}
	r := newTestReconciler(coverageTestScheme(t), res, objs...)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	return r, res, rec
}

func getBPVM(t *testing.T, r *VirtualMachineReconciler, name string) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: bpNS, Name: name}, vm))
	return vm
}

// noProviderCalls asserts the routing provider saw no per-VM call at all.
func noProviderCalls(t *testing.T, p *routingProvider) {
	t.Helper()
	assert.Empty(t, p.createReqs, "no Create")
	assert.Empty(t, p.describeRefs, "no Describe")
	assert.Empty(t, p.deleteRefs, "no Delete")
	assert.Empty(t, p.powerRefs, "no Power")
	assert.Empty(t, p.reconfigureRefs, "no Reconfigure")
}

// ─── pure helpers ─────────────────────────────────────────────────────────────

func TestCheckBoundProvider(t *testing.T) {
	vm := func(bound *infravirtrigaudiov1beta1.BoundProviderRef) *infravirtrigaudiov1beta1.VirtualMachine {
		return &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: bpNS},
			Status:     infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "vm-1", BoundProvider: bound},
		}
	}
	key := func(ns, name string) types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: name} }
	bp := func(ns, name, uid string) *infravirtrigaudiov1beta1.BoundProviderRef {
		return &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: ns, Name: name, UID: uid}
	}

	cases := map[string]struct {
		bound    *infravirtrigaudiov1beta1.BoundProviderRef
		key      types.NamespacedName
		uid      types.UID
		mismatch bool
	}{
		"no record (pre-upgrade or unbound) is not checked": {nil, key("other", "x"), "u", false},
		"same provider and uid":                             {bp(bpNS, "p", "u1"), key(bpNS, "p"), "u1", false},
		"same provider, reference-only check":               {bp(bpNS, "p", "u1"), key(bpNS, "p"), "", false},
		"same provider, uid not recorded":                   {bp(bpNS, "p", ""), key(bpNS, "p"), "u2", false},
		"empty recorded namespace means the VM's":           {bp("", "p", "u1"), key(bpNS, "p"), "u1", false},
		"another name":                      {bp(bpNS, "p", "u1"), key(bpNS, "q"), "", true},
		"same name in another namespace":    {bp(bpNS, "p", "u1"), key("infra", "p"), "u1", true},
		"re-created Provider (uid changed)": {bp(bpNS, "p", "u1"), key(bpNS, "p"), "u2", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkBoundProvider(vm(tc.bound), tc.key, tc.uid)
			if !tc.mismatch {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, isProviderRefMismatch(err))
			assert.Equal(t, k8s.ReasonProviderRefMismatch, vmRefErrorReason(err))
			assert.False(t, isVMUnbound(err))
			assert.False(t, isPlacementTopologyMismatch(err))
		})
	}
}

func TestVMRefFor_RefusesMismatchedBoundProvider(t *testing.T) {
	prov := providerCRWithUID(bpNS, "prov-a", "uid-a")
	vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"},
		&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"})

	ref, err := vmRefFor(vm, prov)
	require.NoError(t, err)
	assert.Equal(t, contracts.VMRef{ID: "vm-100"}, ref, "the bound Provider gets the bare id, as before")

	other := providerCRWithUID(bpNS, "prov-b", "uid-b")
	_, err = vmRefFor(vm, other)
	require.Error(t, err)
	assert.True(t, isProviderRefMismatch(err), "vmRefFor never addresses the VM through another Provider")

	recreated := providerCRWithUID(bpNS, "prov-a", "uid-a2")
	_, err = vmRefFor(vm, recreated)
	require.Error(t, err)
	assert.True(t, isProviderRefMismatch(err))
	assert.Contains(t, err.Error(), "deleted and re-created")

	clustered := withRuntime(clusteredProviderCR("prov-c", bpNS))
	cvm := boundVM("cweb", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"},
		&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a"})
	cvm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a"}
	_, err = vmRefFor(cvm, clustered)
	require.Error(t, err)
	assert.True(t, isProviderRefMismatch(err), "the check comes before routing on a clustered Provider too")
}

// ─── bind paths ───────────────────────────────────────────────────────────────

func TestCreateVM_RecordsBoundProviderWithID(t *testing.T) {
	prov := providerCRWithUID(bpNS, "test-prov", "uid-prov-1")
	_, class := providerAndClass(bpNS)
	vm := creatableVM(bpNS)
	cp := &rejectingCreateProvider{}
	r, _, _ := bpReconciler(t, cp, prov, class, vm)

	_, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	require.Len(t, cp.createReqs, 1)

	after := getBPVM(t, r, vm.Name)
	assert.Equal(t, "web", after.Status.ID)
	assert.Equal(t, &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "test-prov", UID: "uid-prov-1"},
		after.Status.BoundProvider, "the bound Provider is persisted with the id it assigned")
}

func TestCreateVM_Clustered_PendingHostRecordsBoundProvider(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-bp", clusteredNS, "prov-cluster")
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm)
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, providerCR))

	var boundAtCreate *infravirtrigaudiov1beta1.BoundProviderRef
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		boundAtCreate = getVM(t, r, "vm-bp").Status.BoundProvider
		return contracts.CreateResponse{ID: req.Name}, nil
	}

	_, err := r.createVM(ctx, getVM(t, r, "vm-bp"), prov, providerCR, smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)

	want := &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: clusteredNS, Name: "prov-cluster", UID: string(providerCR.UID)}
	assert.Equal(t, want, boundAtCreate, "the bound Provider is persisted with pendingHost, before Create")
	assert.Equal(t, want, getVM(t, r, "vm-bp").Status.BoundProvider)
}

func TestReconcileVM_BackfillsBoundProviderForPreUpgradeVM(t *testing.T) {
	prov := providerCRWithUID(bpNS, "prov-a", "uid-a")
	_, class := providerAndClass(bpNS)
	vm := boundVM("legacy", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"}, nil)
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
	r, res, _ := bpReconciler(t, rp, prov, class, vm)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, "legacy"))
	require.NoError(t, err)

	after := getBPVM(t, r, "legacy")
	assert.Equal(t, &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"},
		after.Status.BoundProvider, "a VM bound before the field existed is backfilled from its current providerRef")
	require.Len(t, rp.describeRefs, 1, "the backfilled VM is reconciled normally")
	assert.Equal(t, "vm-100", rp.describeRefs[0].ID)
	assert.EqualValues(t, 1, res.calls.Load())
}

// ─── enforcement ──────────────────────────────────────────────────────────────

func TestReconcileVM_ProviderRefMismatch_NoProviderCalls(t *testing.T) {
	boundA := &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"}
	cases := map[string]struct {
		ref  infravirtrigaudiov1beta1.ObjectRef
		objs []client.Object
	}{
		"re-pointed at another existing Provider": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-b"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a"), providerCRWithUID(bpNS, "prov-b", "uid-b")},
		},
		"re-pointed at a missing Provider (the old un-adopt trick)": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "does-not-exist"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a")},
		},
		"same name in another namespace": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a", Namespace: "infra"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a"), providerCRWithUID("infra", "prov-a", "uid-infra")},
		},
		"Provider deleted and re-created under the same name": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a-recreated")},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, class := providerAndClass(bpNS)
			vm := boundVM("web", tc.ref, boundA.DeepCopy())
			rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
			r, res, rec := bpReconciler(t, rp, append(tc.objs, class, vm)...)

			result, err := r.reconcileVM(context.Background(), getBPVM(t, r, "web"))
			require.NoError(t, err)
			assert.Equal(t, providerRefMismatchRetryInterval, result.RequeueAfter, "slow recheck, no hot loop")

			assert.Zero(t, res.calls.Load(), "no Provider is even resolved to a client")
			noProviderCalls(t, rp)

			after := getBPVM(t, r, "web")
			c := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, k8s.ReasonProviderRefMismatch, c.Reason)
			assert.Equal(t, vm.Generation, c.ObservedGeneration)
			assert.Equal(t, boundA, after.Status.BoundProvider, "the binding is never rewritten to the new reference")
			assert.Equal(t, "vm-100", after.Status.ID)

			events := drainEvents(rec)
			require.NotEmpty(t, events)
			assert.Contains(t, events[0], "Warning "+k8s.ReasonProviderRefMismatch)
		})
	}
}

func TestReconcileVM_MatchingBoundProviderProceeds(t *testing.T) {
	prov := providerCRWithUID(bpNS, "prov-a", "uid-a")
	_, class := providerAndClass(bpNS)
	vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a", Namespace: bpNS},
		&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"})
	rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
	r, _, _ := bpReconciler(t, rp, prov, class, vm)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, rp.describeRefs, 1, "an explicit namespace naming the VM's own is the same Provider")
}

func TestReconcileVM_UnboundVMIsNotHeldByAStaleBinding(t *testing.T) {
	// A VM whose id was cleared (unbound) may be re-pointed (the CRD allows it)
	// and is created on the Provider it now references, which it is then bound
	// through.
	provB := providerCRWithUID(bpNS, "prov-b", "uid-b")
	_, class := providerAndClass(bpNS)
	vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-b"},
		&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"})
	vm.Status.ID = ""
	cp := &rejectingCreateProvider{}
	r, _, _ := bpReconciler(t, cp, provB, class, vm)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, cp.createReqs, 1)
	assert.Equal(t, &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-b", UID: "uid-b"},
		getBPVM(t, r, "web").Status.BoundProvider)
}

// ─── deletion ─────────────────────────────────────────────────────────────────

// deleteBP marks vm for deletion through the fake client and runs the
// finalizer once. It reports whether the VM is gone (finalizer removed).
func deleteBP(t *testing.T, r *VirtualMachineReconciler, name string) (gone bool, requeue bool) {
	t.Helper()
	ctx := context.Background()
	marked := markForDeletion(t, r, getBPVM(t, r, name))
	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	getErr := r.Get(ctx, types.NamespacedName{Namespace: bpNS, Name: name}, &infravirtrigaudiov1beta1.VirtualMachine{})
	if apierrors.IsNotFound(getErr) {
		return true, res.RequeueAfter > 0
	}
	require.NoError(t, getErr)
	return false, res.RequeueAfter > 0
}

func TestHandleDeletion_ProviderRefMismatch(t *testing.T) {
	boundA := &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"}
	scenarios := map[string]struct {
		ref  infravirtrigaudiov1beta1.ObjectRef
		objs []client.Object
	}{
		"re-pointed at another existing Provider": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-b"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a"), providerCRWithUID(bpNS, "prov-b", "uid-b")},
		},
		"re-pointed at a missing Provider": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "does-not-exist"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a")},
		},
		"Provider re-created under the same name": {
			ref:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"},
			objs: []client.Object{providerCRWithUID(bpNS, "prov-a", "uid-a-recreated")},
		},
	}
	for name, sc := range scenarios {
		t.Run(name+"/retains the finalizer", func(t *testing.T) {
			vm := boundVM("web", sc.ref, boundA.DeepCopy())
			rp := &routingProvider{}
			r, res, rec := bpReconciler(t, rp, append(sc.objs, vm)...)

			gone, requeue := deleteBP(t, r, "web")
			assert.False(t, gone, "the finalizer is retained: the VM is not deleted through a Provider it is not bound to")
			assert.True(t, requeue)
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
			c := meta.FindStatusCondition(getBPVM(t, r, "web").Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, c)
			assert.Equal(t, k8s.ReasonProviderRefMismatch, c.Reason)
			events := strings.Join(drainEvents(rec), "\n")
			assert.Contains(t, events, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation,
				"the event tells the operator how to detach it")
		})
		t.Run(name+"/force-delete drops the finalizer without a provider call", func(t *testing.T) {
			vm := boundVM("web", sc.ref, boundA.DeepCopy())
			vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			rp := &routingProvider{}
			r, res, _ := bpReconciler(t, rp, append(sc.objs, vm)...)

			gone, _ := deleteBP(t, r, "web")
			assert.True(t, gone)
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
		})
		t.Run(name+"/orphan-on-delete drops the finalizer without a provider call", func(t *testing.T) {
			vm := boundVM("web", sc.ref, boundA.DeepCopy())
			vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
			rp := &routingProvider{}
			r, res, _ := bpReconciler(t, rp, append(sc.objs, vm)...)

			gone, _ := deleteBP(t, r, "web")
			assert.True(t, gone)
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
		})
	}
}

func TestHandleDeletion_Clustered_PendingCreateOnRecreatedProviderIsNotDeleted(t *testing.T) {
	// A clustered create in flight (pendingHost, no id) whose Provider object
	// was re-created: the owner-checked cleanup Delete is not sent through it.
	vm := clusterVM("vm-pend", clusteredNS, "prov-cluster")
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha"}
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: clusteredNS, Name: "prov-cluster", UID: "uid-old"}
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm)
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, providerCR))
	providerCR.UID = "uid-new"
	require.NoError(t, r.Update(context.Background(), providerCR))

	marked := markForDeletion(t, r, getVM(t, r, "vm-pend"))
	_, err := r.handleDeletion(context.Background(), marked)
	require.NoError(t, err)
	assert.Empty(t, prov.deleteRefs, "no cleanup Delete through a re-created Provider")
	assert.Contains(t, getVM(t, r, "vm-pend").Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
}

func TestHandleDeletion_OrphanOnDelete(t *testing.T) {
	boundA := &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"}

	t.Run("with the annotation: finalizer removed, no provider call, event recorded", func(t *testing.T) {
		vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"}, boundA.DeepCopy())
		vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
		rp := &routingProvider{}
		r, res, rec := bpReconciler(t, rp, providerCRWithUID(bpNS, "prov-a", "uid-a"), vm)

		gone, _ := deleteBP(t, r, "web")
		assert.True(t, gone, "the finalizer is removed")
		assert.Zero(t, res.calls.Load(), "no Provider is resolved")
		noProviderCalls(t, rp)
		events := drainEvents(rec)
		require.Len(t, events, 1)
		assert.Contains(t, events[0], "Normal "+eventReasonOrphaned)
		assert.Contains(t, events[0], `"vm-100"`)
		assert.Contains(t, events[0], bpNS+"/prov-a")
	})

	t.Run("a pre-upgrade VM without a recorded binding is detached the same way", func(t *testing.T) {
		vm := boundVM("legacy", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"}, nil)
		vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
		rp := &routingProvider{}
		r, _, _ := bpReconciler(t, rp, providerCRWithUID(bpNS, "prov-a", "uid-a"), vm)

		gone, _ := deleteBP(t, r, "legacy")
		assert.True(t, gone)
		noProviderCalls(t, rp)
	})

	t.Run("a clustered VM is detached without a routed delete", func(t *testing.T) {
		vm := boundClusterVM("vm-orphan")
		vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
		vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
		prov := &routingProvider{}
		r := clusteredFixture(t, prov, vm)

		marked := markForDeletion(t, r, getVM(t, r, "vm-orphan"))
		_, err := r.handleDeletion(context.Background(), marked)
		require.NoError(t, err)
		assert.Empty(t, prov.deleteRefs)
		err = r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "vm-orphan"}, &infravirtrigaudiov1beta1.VirtualMachine{})
		assert.True(t, apierrors.IsNotFound(err))
	})

	for _, val := range []string{"", "false", "yes"} {
		t.Run("annotation "+val+" is not orphan-on-delete: the provider Delete runs", func(t *testing.T) {
			vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"}, boundA.DeepCopy())
			if val != "" {
				vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: val}
			}
			rp := &routingProvider{}
			r, _, rec := bpReconciler(t, rp, providerCRWithUID(bpNS, "prov-a", "uid-a"), vm)

			gone, _ := deleteBP(t, r, "web")
			assert.True(t, gone)
			require.Len(t, rp.deleteRefs, 1, "the matching bound Provider deletes the hypervisor VM")
			assert.Equal(t, "vm-100", rp.deleteRefs[0].ID)
			assert.Empty(t, drainEvents(rec), "no orphan event")
		})
	}
}

func TestHandleDeletion_BoundProviderGoneStillReleases(t *testing.T) {
	// Unchanged semantics: when the Provider the VM is bound through (and
	// still references) no longer exists, there is nothing to delete through
	// and the finalizer is removed.
	vm := boundVM("web", infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"},
		&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: bpNS, Name: "prov-a", UID: "uid-a"})
	rp := &routingProvider{}
	r, res, _ := bpReconciler(t, rp, vm)

	gone, _ := deleteBP(t, r, "web")
	assert.True(t, gone)
	assert.Zero(t, res.calls.Load())
	noProviderCalls(t, rp)
}
