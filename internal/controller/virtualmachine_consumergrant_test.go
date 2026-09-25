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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VirtualMachine controller's enforcement of
// spec.consumerNamespaceSelector: a VM in bpNS ("team-a") that references a
// Provider, VMClass or VMImage in cgOwnerNS ("infra") is refused
// (Ready=False/ConsumerNotAllowed, no provider resolved or called) unless the
// referenced object selects team-a — before and after the VM is bound, and on
// deletion.

// cgVM returns an unbound, creatable VM in bpNS whose Provider and VMClass are
// in provNS / classNS (an empty namespace means the VM's own).
func cgVM(provNS, classNS string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := creatableVM(bpNS)
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Spec.ProviderRef = infravirtrigaudiov1beta1.ObjectRef{Name: "shared", Namespace: provNS}
	vm.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "shared", Namespace: classNS}
	return vm
}

// cgObjects returns the consumer namespace (labelled tier=gold), a Provider
// and a VMClass named "shared" in ns with the given selectors, plus the same
// pair in the VM's own namespace.
func cgObjects(ns string, provSel, classSel *metav1.LabelSelector) []client.Object {
	return []client.Object{
		labeledNamespace(bpNS, map[string]string{"tier": "gold"}),
		labeledNamespace(cgOwnerNS, nil),
		grantedProvider(ns, "shared", provSel),
		grantedClass(ns, "shared", classSel),
	}
}

// requireConsumerNotAllowed asserts the VM reports Ready=False/ConsumerNotAllowed
// for kind, at the VM's generation.
func requireConsumerNotAllowed(t *testing.T, vm *infravirtrigaudiov1beta1.VirtualMachine, kind string) {
	t.Helper()
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonConsumerNotAllowed, c.Reason)
	assert.Equal(t, vm.Generation, c.ObservedGeneration)
	assert.Contains(t, c.Message, kind+" "+cgOwnerNS+"/")
	assert.Contains(t, c.Message, consumerNamespaceSelectorField)
}

func TestReconcileVM_SameNamespaceRefsNeedNoGrant(t *testing.T) {
	rp := &routingProvider{}
	vm := cgVM("", "")
	r, res, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), vm)...)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.calls.Load())
	require.Len(t, rp.createReqs, 1, "a VM using its own namespace's Provider and VMClass is created as before")
}

func TestReconcileVM_CrossNamespaceRefs(t *testing.T) {
	gold := sharedWith(map[string]string{"tier": "gold"})
	silver := sharedWith(map[string]string{"tier": "silver"})
	cases := map[string]struct {
		provSel, classSel *metav1.LabelSelector
		provNS, classNS   string
		deniedKind        string // "" = allowed
	}{
		"provider with no selector is refused":           {nil, gold, cgOwnerNS, "", consumerKindProvider},
		"class with no selector is refused":              {gold, nil, "", cgOwnerNS, consumerKindVMClass},
		"provider with a non-matching selector":          {silver, nil, cgOwnerNS, "", consumerKindProvider},
		"class with a non-matching selector":             {nil, silver, "", cgOwnerNS, consumerKindVMClass},
		"matching label selector allows":                 {gold, gold, cgOwnerNS, cgOwnerNS, ""},
		"empty selector allows any namespace":            {sharedWithAll(), sharedWithAll(), cgOwnerNS, cgOwnerNS, ""},
		"explicit namespace name allows":                 {sharedWithNamespace(bpNS), sharedWithNamespace(bpNS), cgOwnerNS, cgOwnerNS, ""},
		"explicit name of another namespace is refused":  {sharedWithNamespace("team-b"), gold, cgOwnerNS, cgOwnerNS, consumerKindProvider},
		"an allowed provider does not grant the class":   {gold, nil, cgOwnerNS, cgOwnerNS, consumerKindVMClass},
		"an allowed class does not grant the provider":   {nil, gold, cgOwnerNS, cgOwnerNS, consumerKindProvider},
		"own-namespace provider with cross-ns class too": {nil, silver, "", cgOwnerNS, consumerKindVMClass},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The selectors apply to whichever copy is referenced; the own
			// namespace's copies carry none.
			objs := []client.Object{
				labeledNamespace(bpNS, map[string]string{"tier": "gold"}),
				labeledNamespace(cgOwnerNS, nil),
				grantedProvider(cgOwnerNS, "shared", tc.provSel), grantedClass(cgOwnerNS, "shared", tc.classSel),
				grantedProvider(bpNS, "shared", nil), grantedClass(bpNS, "shared", nil),
			}
			vm := cgVM(tc.provNS, tc.classNS)
			rp := &routingProvider{}
			r, res, rec := bpReconciler(t, rp, append(objs, vm)...)

			result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
			require.NoError(t, err)
			after := getBPVM(t, r, vm.Name)
			if tc.deniedKind == "" {
				assert.EqualValues(t, 1, res.calls.Load())
				require.Len(t, rp.createReqs, 1, "a granted cross-namespace reference proceeds")
				return
			}
			assert.Equal(t, consumerNotAllowedRetryInterval, result.RequeueAfter, "slow recheck")
			assert.Zero(t, res.calls.Load(), "no Provider is resolved to a client")
			noProviderCalls(t, rp)
			requireConsumerNotAllowed(t, after, tc.deniedKind)
			assert.Empty(t, after.Status.ID)
			events := drainEvents(rec)
			require.Len(t, events, 1)
			assert.Contains(t, events[0], "Warning "+k8s.ReasonConsumerNotAllowed)
		})
	}
}

func TestReconcileVM_CrossNamespaceImage(t *testing.T) {
	for name, tc := range map[string]struct {
		sel     *metav1.LabelSelector
		allowed bool
	}{
		"no selector":       {nil, false},
		"non-matching":      {sharedWith(map[string]string{"tier": "silver"}), false},
		"matching selector": {sharedWith(map[string]string{"tier": "gold"}), true},
	} {
		t.Run(name, func(t *testing.T) {
			vm := cgVM("", "")
			vm.Spec.ImportedDisk = nil
			vm.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "golden", Namespace: cgOwnerNS}
			img := imageWithSource("golden", "")
			img.Namespace = cgOwnerNS
			img.Spec.ConsumerNamespaceSelector = tc.sel
			rp := &routingProvider{}
			r, res, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), img, vm)...)

			_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
			require.NoError(t, err)
			after := getBPVM(t, r, vm.Name)
			if tc.allowed {
				assert.EqualValues(t, 1, res.calls.Load(), "a granted image lets the reconcile reach the provider")
				c := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady)
				if c != nil {
					assert.NotEqual(t, k8s.ReasonConsumerNotAllowed, c.Reason)
				}
				return
			}
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
			requireConsumerNotAllowed(t, after, consumerKindVMImage)
		})
	}
}

func TestReconcileVM_MissingCrossNamespaceObjectLooksLikeAnUngrantedOne(t *testing.T) {
	// No existence oracle: a missing Provider in another namespace is refused
	// with the same condition as an existing one that does not select the VM's
	// namespace.
	rp := &routingProvider{}
	vm := cgVM(cgOwnerNS, "")
	vm.Spec.ProviderRef.Name = "does-not-exist"
	r, res, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), vm)...)

	result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Equal(t, consumerNotAllowedRetryInterval, result.RequeueAfter)
	assert.Zero(t, res.calls.Load())
	after := getBPVM(t, r, vm.Name)
	requireConsumerNotAllowed(t, after, consumerKindProvider)
	assert.Equal(t,
		(&ConsumerNotAllowedError{Kind: consumerKindProvider, Namespace: cgOwnerNS, Name: "does-not-exist"}).Error(),
		meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady).Message)

	// A missing Provider in the VM's own namespace keeps its historical
	// handling.
	own := cgVM("", "")
	own.Name = "own"
	own.Spec.ProviderRef.Name = "does-not-exist"
	r2, _, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), own)...)
	_, err = r2.reconcileVM(context.Background(), getBPVM(t, r2, own.Name))
	require.NoError(t, err)
	c := meta.FindStatusCondition(getBPVM(t, r2, own.Name).Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonWaitingForDependencies, c.Reason)
}

func TestReconcileVM_GrantAddedLaterProceeds(t *testing.T) {
	ctx := context.Background()
	rp := &routingProvider{}
	vm := cgVM(cgOwnerNS, cgOwnerNS)
	r, res, rec := bpReconciler(t, rp, append(cgObjects(cgOwnerNS, nil, sharedWithAll()), vm)...)

	_, err := r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindProvider)
	assert.Len(t, drainEvents(rec), 1)

	// The recheck does not repeat the event.
	_, err = r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec), "the refusal event is emitted on the transition only")
	assert.Zero(t, res.calls.Load())

	// An administrator of namespace infra shares the Provider with tier=gold.
	prov := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: cgOwnerNS, Name: "shared"}, prov))
	prov.Spec.ConsumerNamespaceSelector = sharedWith(map[string]string{"tier": "gold"})
	require.NoError(t, r.Update(ctx, prov))

	_, err = r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1, "the VM is created once the grant exists")
	after := getBPVM(t, r, vm.Name)
	assert.NotEmpty(t, after.Status.ID)
	assert.Equal(t, &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: cgOwnerNS, Name: "shared", UID: string(prov.UID)},
		after.Status.BoundProvider)
}

func TestReconcileVM_NamespaceLabelledLaterProceeds(t *testing.T) {
	ctx := context.Background()
	rp := &routingProvider{}
	vm := cgVM(cgOwnerNS, "")
	objs := cgObjects(bpNS, nil, nil)
	objs = append(objs, grantedProvider(cgOwnerNS, "shared", sharedWith(map[string]string{"virtrigaud.io/shared-infra": "true"})), vm)
	r, _, _ := bpReconciler(t, rp, objs...)

	_, err := r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindProvider)

	ns := &corev1.Namespace{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: bpNS}, ns))
	ns.Labels["virtrigaud.io/shared-infra"] = "true"
	require.NoError(t, r.Update(ctx, ns))

	_, err = r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1)
}

// ─── existing bound VMs (upgrade) ────────────────────────────────────────────

// cgBoundVM is a VM bound through infra/shared before the grant existed.
func cgBoundVM() *infravirtrigaudiov1beta1.VirtualMachine {
	vm := cgVM(cgOwnerNS, "")
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: cgOwnerNS, Name: "shared"}
	return vm
}

func TestReconcileVM_BoundVMWithoutGrantFailsClosed(t *testing.T) {
	for name, withRecord := range map[string]bool{"with a recorded binding": true, "pre-#341, no binding recorded": false} {
		t.Run(name, func(t *testing.T) {
			vm := cgBoundVM()
			if !withRecord {
				vm.Status.BoundProvider = nil
			}
			rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
			r, res, _ := bpReconciler(t, rp, append(cgObjects(cgOwnerNS, nil, nil), vm)...)

			result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
			require.NoError(t, err)
			assert.Equal(t, consumerNotAllowedRetryInterval, result.RequeueAfter)
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
			after := getBPVM(t, r, vm.Name)
			requireConsumerNotAllowed(t, after, consumerKindProvider)
			assert.Equal(t, "vm-100", after.Status.ID, "nothing is unbound or orphaned")
			if !withRecord {
				assert.Nil(t, after.Status.BoundProvider, "an ungranted Provider is not trusted on first reconcile")
			}
		})
	}
}

// cgBoundVMWithOwnProvider is a VM bound through its own namespace's Provider,
// whose class (and, when image is true, image) is infra/shared.
func cgBoundVMWithOwnProvider(image bool) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := cgBoundVM()
	vm.Spec.ProviderRef.Namespace = ""
	vm.Status.BoundProvider.Namespace = bpNS
	vm.Spec.ClassRef.Namespace = cgOwnerNS
	if image {
		vm.Spec.ClassRef.Namespace = ""
		vm.Spec.ImportedDisk = nil
		vm.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "golden", Namespace: cgOwnerNS}
	}
	return vm
}

func TestReconcileVM_BoundVMWithRevokedClass(t *testing.T) {
	t.Run("describe runs, the class is not applied, Ready=False/ConsumerNotAllowed", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(false)
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
		r, _, _ := bpReconciler(t, rp, append(append(cgObjects(bpNS, nil, nil), grantedClass(cgOwnerNS, "shared", nil)), vm)...)

		result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Equal(t, consumerNotAllowedRetryInterval, result.RequeueAfter)
		require.Len(t, rp.describeRefs, 1, "the VM exists already: describing it uses no class content")
		assert.Empty(t, rp.reconfigureRefs, "reconfiguring to a class the namespace may not use is refused")
		assert.Empty(t, rp.createReqs)
		assert.Empty(t, rp.deleteRefs)
		requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindVMClass)
	})
	t.Run("power is still reconciled", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(false)
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
		r, _, _ := bpReconciler(t, rp, append(append(cgObjects(bpNS, nil, nil), grantedClass(cgOwnerNS, "shared", nil)), vm)...)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		require.Len(t, rp.powerRefs, 1, "revoking a class share does not stop power operations")
	})
	t.Run("a missing VM is not re-created from it", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(false)
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: false}}
		r, _, _ := bpReconciler(t, rp, append(append(cgObjects(bpNS, nil, nil), grantedClass(cgOwnerNS, "shared", nil)), vm)...)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs, "a re-create uses the class")
		after := getBPVM(t, r, vm.Name)
		requireConsumerNotAllowed(t, after, consumerKindVMClass)
		assert.Equal(t, "vm-100", after.Status.ID, "the binding is kept")
	})
	t.Run("a completed reconfigure task is not applied from it", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(false)
		vm.Status.ReconfigureTaskRef = "task-1"
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
		r, _, _ := bpReconciler(t, rp, append(append(cgObjects(bpNS, nil, nil), grantedClass(cgOwnerNS, "shared", nil)), vm)...)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		after := getBPVM(t, r, vm.Name)
		requireConsumerNotAllowed(t, after, consumerKindVMClass)
		assert.Equal(t, "task-1", after.Status.ReconfigureTaskRef, "kept until access is restored")
	})
}

func TestReconcileVM_BoundVMWithRevokedImageKeepsWorking(t *testing.T) {
	img := imageWithSource("golden", "")
	img.Namespace = cgOwnerNS // no selector: not shared with team-a
	t.Run("describe and Ready are unaffected", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(true)
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
		r, _, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), img.DeepCopy(), vm)...)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		require.Len(t, rp.describeRefs, 1)
		c := meta.FindStatusCondition(getBPVM(t, r, vm.Name).Status.Conditions, k8s.ConditionReady)
		require.NotNil(t, c)
		assert.Equal(t, metav1.ConditionTrue, c.Status, "revoking an image share does not stop a VM created from it")
	})
	t.Run("a missing VM is not re-created from it", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(true)
		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: false}}
		r, _, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), img.DeepCopy(), vm)...)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs)
		requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindVMImage)
	})
	t.Run("a pending clustered create is not issued from it", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(true)
		vm.Status.ID = ""
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha"}
		rp := &routingProvider{}
		clustered := withRuntime(clusteredProviderCR("shared", bpNS))
		r, _, _ := bpReconciler(t, rp, labeledNamespace(bpNS, nil), clustered, grantedClass(bpNS, "shared", nil), img.DeepCopy(), vm)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs)
		requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindVMImage)
	})
	t.Run("the image is not sent with a reconfigure", func(t *testing.T) {
		vm := cgBoundVMWithOwnProvider(true)
		r, _, _ := bpReconciler(t, &routingProvider{}, append(cgObjects(bpNS, nil, nil), img.DeepCopy(), vm)...)
		deps, err := r.getDependencies(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Nil(t, deps.image)
		var cna *ConsumerNotAllowedError
		require.ErrorAs(t, deps.imageRefusal, &cna)
		assert.Equal(t, consumerKindVMImage, cna.Kind)
		assert.NoError(t, deps.classRefusal)
	})
}

func TestReconcileVM_UnchangedRefusalIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	vm := cgVM(cgOwnerNS, "")
	var statusWrites atomic.Int32
	c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).
		WithObjects(append(cgObjects(bpNS, nil, nil), grantedProvider(cgOwnerNS, "shared", nil), vm)...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			statusWrites.Add(1)
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	r := &VirtualMachineReconciler{Client: c, Scheme: c.Scheme(), RemoteResolver: &countingResolver{provider: &routingProvider{}}}

	_, err := r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	require.EqualValues(t, 1, statusWrites.Load(), "the refusal is recorded once")
	for i := 0; i < 3; i++ {
		_, err = r.reconcileVM(ctx, getBPVM(t, r, vm.Name))
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, statusWrites.Load(), "a recheck of the same refusal writes nothing")
}

func TestHandleDeletion_MissingCrossNamespaceProviderIsRefusedLikeAnUngrantedOne(t *testing.T) {
	vm := cgBoundVM()
	vm.Spec.ProviderRef.Name = "gone"
	vm.Status.BoundProvider.Name = "gone"
	rp := &routingProvider{}
	r, res, _ := bpReconciler(t, rp, append(cgObjects(bpNS, nil, nil), vm)...)

	gone, requeue := deleteBP(t, r, vm.Name)
	assert.False(t, gone, "a missing cross-namespace Provider keeps the finalizer, like an ungranted one")
	assert.True(t, requeue)
	assert.Zero(t, res.calls.Load())
	noProviderCalls(t, rp)
	requireConsumerNotAllowed(t, getBPVM(t, r, vm.Name), consumerKindProvider)
}

func TestReconcileVM_BoundVMWithGrantKeepsWorking(t *testing.T) {
	vm := cgBoundVM()
	rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
	objs := append(cgObjects(cgOwnerNS, sharedWithNamespace(bpNS), nil), grantedClass(bpNS, "shared", nil), vm)
	r, _, _ := bpReconciler(t, rp, objs...)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	require.Len(t, rp.describeRefs, 1)
	assert.Equal(t, "vm-100", rp.describeRefs[0].ID)
}

func TestHandleDeletion_UngrantedCrossNamespaceProvider(t *testing.T) {
	t.Run("retains the finalizer and makes no provider call", func(t *testing.T) {
		vm := cgBoundVM()
		rp := &routingProvider{}
		r, res, rec := bpReconciler(t, rp, append(cgObjects(cgOwnerNS, nil, nil), vm)...)

		gone, requeue := deleteBP(t, r, vm.Name)
		assert.False(t, gone, "the hypervisor VM is never deleted through an ungranted Provider, nor orphaned silently")
		assert.True(t, requeue)
		assert.Zero(t, res.calls.Load())
		noProviderCalls(t, rp)
		c := meta.FindStatusCondition(getBPVM(t, r, vm.Name).Status.Conditions, k8s.ConditionReady)
		require.NotNil(t, c)
		assert.Equal(t, k8s.ReasonConsumerNotAllowed, c.Reason)
		events := strings.Join(drainEvents(rec), "\n")
		assert.Contains(t, events, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
		assert.Contains(t, events, forceDeleteAnnotation)
	})
	for _, ann := range []string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation} {
		t.Run(ann+" drops the finalizer without a provider call", func(t *testing.T) {
			vm := cgBoundVM()
			vm.Annotations = map[string]string{ann: "true"}
			rp := &routingProvider{}
			r, res, _ := bpReconciler(t, rp, append(cgObjects(cgOwnerNS, nil, nil), vm)...)

			gone, _ := deleteBP(t, r, vm.Name)
			assert.True(t, gone)
			assert.Zero(t, res.calls.Load())
			noProviderCalls(t, rp)
		})
	}
	t.Run("with the grant the provider Delete runs", func(t *testing.T) {
		vm := cgBoundVM()
		rp := &routingProvider{}
		r, _, _ := bpReconciler(t, rp, append(cgObjects(cgOwnerNS, sharedWithAll(), nil), vm)...)

		gone, _ := deleteBP(t, r, vm.Name)
		assert.True(t, gone)
		require.Len(t, rp.deleteRefs, 1)
		assert.Equal(t, "vm-100", rp.deleteRefs[0].ID)
	})
	t.Run("an ungranted class or image does not block deletion", func(t *testing.T) {
		vm := cgBoundVM()
		vm.Spec.ProviderRef.Namespace = ""
		vm.Status.BoundProvider.Namespace = bpNS
		vm.Spec.ClassRef.Namespace = cgOwnerNS
		rp := &routingProvider{}
		r, _, _ := bpReconciler(t, rp, append(append(cgObjects(bpNS, nil, nil), grantedClass(cgOwnerNS, "shared", nil)), vm)...)

		gone, _ := deleteBP(t, r, vm.Name)
		assert.True(t, gone)
		require.Len(t, rp.deleteRefs, 1, "the delete goes through the VM's own-namespace Provider")
	})
}

// ─── image prepare ───────────────────────────────────────────────────────────

func TestEnsureImageOnProvider_RefusesUngrantedPair(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		imageSel, provSel *metav1.LabelSelector
		kind              string
	}{
		"ungranted image":    {nil, sharedWithAll(), consumerKindVMImage},
		"ungranted provider": {sharedWithAll(), nil, consumerKindProvider},
	} {
		t.Run(name, func(t *testing.T) {
			img := imageWithSource("golden", "")
			img.Namespace = cgOwnerNS
			img.Spec.ConsumerNamespaceSelector = tc.imageSel
			provider := importCapableProvider("shared")
			provider.Namespace = cgOwnerNS
			provider.Spec.ConsumerNamespaceSelector = tc.provSel
			r, _ := newEnsureReconciler(t, img)
			inst := &preparerProvider{}
			vm := vmForImage(provider.Name, img.Name)
			vm.Namespace = bpNS

			requeue, err := r.EnsureImageOnProvider(ctx, vm, img, provider, inst)
			assert.False(t, requeue)
			var cna *ConsumerNotAllowedError
			require.ErrorAs(t, err, &cna)
			assert.Equal(t, tc.kind, cna.Kind)
			assert.Zero(t, inst.calls(), "no PrepareImage RPC")
			got := &infravirtrigaudiov1beta1.VMImage{}
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: cgOwnerNS, Name: img.Name}, got))
			assert.Empty(t, got.Status.ProviderStatus, "no VMImage status write")
		})
	}
}

func TestEnsureImageOnProvider_NeverSendsTheSelector(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	img.Spec.ConsumerNamespaceSelector = sharedWith(map[string]string{"tenant-label-secret": "x"})
	r, _ := newEnsureReconciler(t, img)
	inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu"}}
	provider := importCapableProvider("libvirt-1")

	_, err := r.EnsureImageOnProvider(context.Background(), vmForImage(provider.Name, img.Name), img, provider, inst)
	require.NoError(t, err)
	require.Equal(t, 1, inst.calls())
	assert.Contains(t, inst.lastPrepareReq.ImageJSON, `"source"`)
	assert.NotContains(t, inst.lastPrepareReq.ImageJSON, "consumerNamespaceSelector")
	assert.NotContains(t, inst.lastPrepareReq.ImageJSON, "tenant-label-secret")
	assert.NotNil(t, img.Spec.ConsumerNamespaceSelector, "the caller's object is not modified")
}

// ─── watches ─────────────────────────────────────────────────────────────────

func TestVMsAffectedByGrantChange(t *testing.T) {
	refused := cgVM(cgOwnerNS, "")
	refused.Name = "refused"
	refused.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonConsumerNotAllowed}}
	cross := cgVM("", cgOwnerNS)
	cross.Name = "cross"
	local := cgVM("", "")
	local.Name = "local"
	elsewhere := cgVM(cgOwnerNS, "")
	elsewhere.Name = "elsewhere"
	elsewhere.Namespace = "team-b"
	r, _, _ := bpReconciler(t, &routingProvider{}, refused, cross, local, elsewhere)

	names := func(reqs []reconcile.Request) []string {
		var out []string
		for _, q := range reqs {
			out = append(out, q.String())
		}
		return out
	}
	assert.ElementsMatch(t, []string{bpNS + "/refused", bpNS + "/cross"}, names(r.vmsAffectedByGrantChange(context.Background(), bpNS, nil)),
		"a Namespace label change re-drives that namespace's refused and cross-namespace VMs only")
	assert.ElementsMatch(t, []string{bpNS + "/refused", "team-b/elsewhere"},
		names(r.vmsAffectedByGrantChange(context.Background(), "", grantedProvider(cgOwnerNS, "shared", nil))),
		"a Provider selector change re-drives the VMs in other namespaces that reference it, in every namespace")
	assert.ElementsMatch(t, []string{bpNS + "/cross"},
		names(r.vmsAffectedByGrantChange(context.Background(), "", grantedClass(cgOwnerNS, "shared", nil))),
		"a VMClass selector change re-drives only the VMs that reference that class")
	assert.Empty(t, names(r.vmsAffectedByGrantChange(context.Background(), "", grantedProvider(bpNS, "shared", nil))),
		"a same-namespace reference is never affected by a selector")
	assert.Empty(t, names(r.vmsAffectedByGrantChange(context.Background(), "", grantedImage(cgOwnerNS, "other", nil))))
}

func TestConsumerGrantPredicates(t *testing.T) {
	nsPred := namespaceLabelsChanged()
	oldNS := labeledNamespace(bpNS, nil)
	newNS := labeledNamespace(bpNS, map[string]string{"tier": "gold"})
	assert.True(t, nsPred.Update(event.UpdateEvent{ObjectOld: oldNS, ObjectNew: newNS}), "a label change passes")
	annotated := oldNS.DeepCopy()
	annotated.Annotations = map[string]string{"x": "y"}
	assert.False(t, nsPred.Update(event.UpdateEvent{ObjectOld: oldNS, ObjectNew: annotated}), "an annotation-only change does not")
	assert.False(t, nsPred.Create(event.CreateEvent{Object: newNS}))
	assert.False(t, nsPred.Delete(event.DeleteEvent{Object: newNS}))

	selPred := consumerSelectorChanged()
	plain := grantedProvider(cgOwnerNS, "p", nil)
	shared := grantedProvider(cgOwnerNS, "p", sharedWithAll())
	assert.True(t, selPred.Update(event.UpdateEvent{ObjectOld: plain, ObjectNew: shared}), "granting passes")
	assert.True(t, selPred.Update(event.UpdateEvent{ObjectOld: shared, ObjectNew: plain}), "revoking passes")
	statusOnly := shared.DeepCopy()
	statusOnly.Status.Runtime.Phase = "Degraded"
	assert.False(t, selPred.Update(event.UpdateEvent{ObjectOld: shared, ObjectNew: statusOnly}), "a status update does not")
	assert.True(t, selPred.Create(event.CreateEvent{Object: grantedClass(cgOwnerNS, "c", sharedWithAll())}))
	assert.False(t, selPred.Create(event.CreateEvent{Object: grantedImage(cgOwnerNS, "i", nil)}))
	assert.False(t, selPred.Delete(event.DeleteEvent{Object: shared}))
}
