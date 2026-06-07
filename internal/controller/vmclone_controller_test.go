/*
Copyright 2025.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// clonerProvider is a contracts.Provider that ALSO implements contracts.Cloner
// (and optionally contracts.CapabilityReporter). It records the CloneRequest it
// received so tests can assert on it. It embeds stubProvider so it satisfies the
// full Provider interface with no boilerplate.
type clonerProvider struct {
	stubProvider

	cloneResp contracts.CloneResponse
	cloneErr  error
	lastClone *contracts.CloneRequest
	cloneCnt  int

	// capability reporting (optional). reportCaps gates whether this fake is
	// treated as a CapabilityReporter — when false the fake still has the
	// method but tests use clonerProviderNoCaps instead.
	caps    contracts.Capabilities
	capsErr error
}

func (p *clonerProvider) Clone(_ context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	p.cloneCnt++
	r := req
	p.lastClone = &r
	return p.cloneResp, p.cloneErr
}

func (p *clonerProvider) GetCapabilities(_ context.Context) (contracts.Capabilities, error) {
	if p.capsErr != nil {
		return contracts.Capabilities{}, p.capsErr
	}
	return p.caps, nil
}

// clonerProviderNoCaps is a Cloner that does NOT implement CapabilityReporter
// (fail-open path for linked clones).
type clonerProviderNoCaps struct {
	stubProvider
	cloneResp contracts.CloneResponse
	cloneCnt  int
}

func (p *clonerProviderNoCaps) Clone(_ context.Context, _ contracts.CloneRequest) (contracts.CloneResponse, error) {
	p.cloneCnt++
	return p.cloneResp, nil
}

// nonClonerProvider is a plain Provider that is NOT a Cloner.
type nonClonerProvider struct{ stubProvider }

var (
	_ contracts.Provider           = (*clonerProvider)(nil)
	_ contracts.Cloner             = (*clonerProvider)(nil)
	_ contracts.CapabilityReporter = (*clonerProvider)(nil)
	_ contracts.Provider           = (*clonerProviderNoCaps)(nil)
	_ contracts.Cloner             = (*clonerProviderNoCaps)(nil)
)

func cloneTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, infrav1beta1.AddToScheme(s))
	return s
}

// runningProvider returns a Provider CR whose runtime is Running (so the
// resolver path is satisfied — though we inject a stub resolver anyway).
func runningProvider(ns, name string) *infrav1beta1.Provider {
	return &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: infrav1beta1.ProviderStatus{
			Runtime: &infrav1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "localhost:50051"},
		},
	}
}

// sourceVMWithID returns a provisioned source VM (Status.ID set).
func sourceVMWithID(ns, name, provName, id string) *infrav1beta1.VirtualMachine {
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: provName},
			ClassRef:    infrav1beta1.ObjectRef{Name: "src-class"},
		},
	}
	vm.Status.ID = id
	return vm
}

func newCloneReconciler(s *runtime.Scheme, resolver ProviderResolver, objs ...client.Object) *VMCloneReconciler {
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(
			&infrav1beta1.VMClone{},
			&infrav1beta1.VirtualMachine{},
		).
		Build()
	return &VMCloneReconciler{
		Client:         fc,
		Scheme:         s,
		RemoteResolver: resolver,
		Recorder:       record.NewFakeRecorder(20),
	}
}

func reconcileTwice(t *testing.T, r *VMCloneReconciler, key client.ObjectKey) {
	t.Helper()
	// First reconcile adds the finalizer and requeues; subsequent reconciles
	// drive the phases. Reconcile a few times so async-free fakes settle.
	for i := 0; i < 4; i++ {
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		require.NoError(t, err)
	}
}

// TestVMClone_HappyPath: source VM with Status.ID → synchronous clone → target
// VM CR created with Status.ID set, adopted label, clone provenance, plus
// VMClone TargetRef + Phase=Ready.
func TestVMClone_HappyPath(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{
				Name:   "clone-target",
				Labels: map[string]string{"app": "demo"},
			},
		},
	}

	cp := &clonerProvider{
		caps:      contracts.Capabilities{SupportsLinkedClones: true},
		cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}, // synchronous (no TaskRef)
	}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	// VMClone is Ready with a TargetRef.
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	require.NotNil(t, got.Status.TargetRef)
	assert.Equal(t, "clone-target", got.Status.TargetRef.Name)
	assert.Equal(t, infrav1beta1.CloneTypeFullClone, got.Status.ActualCloneType)
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)

	// The Clone RPC was called with the source VM's provider ID + target name.
	require.Equal(t, 1, cp.cloneCnt)
	require.NotNil(t, cp.lastClone)
	assert.Equal(t, "vm-source-123", cp.lastClone.SourceVmID)
	assert.Equal(t, "clone-target", cp.lastClone.TargetName)
	assert.False(t, cp.lastClone.Linked)

	// Target VM CR exists, has Status.ID seeded, adopted label, provenance.
	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "clone-target"}, target))
	assert.Equal(t, "vm-clone-999", target.Status.ID)
	assert.Equal(t, AdoptedLabelValue, target.Labels[AdoptedLabel])
	assert.Equal(t, "demo", target.Labels["app"])
	assert.Equal(t, "src-vm", target.Annotations[CloneAnnotationClonedFrom])
	assert.Equal(t, "clone-1", target.Annotations[CloneAnnotationClone])
	assert.Equal(t, "prov-1", target.Spec.ProviderRef.Name)
	// ClassRef inherited from the source VM (no target ClassRef override).
	assert.Equal(t, "src-class", target.Spec.ClassRef.Name)
}

// TestVMClone_Idempotent_NoDoubleCreate: a second reconcile after Ready does not
// call Clone again or recreate the target VM.
func TestVMClone_Idempotent_NoDoubleCreate(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)

	// Many reconciles; Clone must only ever be invoked once.
	for i := 0; i < 8; i++ {
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
	}
	assert.Equal(t, 1, cp.cloneCnt, "Clone must be called exactly once")
}

// TestVMClone_LinkedBlockedWhenUnsupported: type=LinkedClone with a provider
// reporting !SupportsLinkedClones → Failed, no Clone call.
func TestVMClone_LinkedBlockedWhenUnsupported(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	linked := infrav1beta1.CloneTypeLinkedClone
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-linked", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source:  infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target:  infrav1beta1.VMCloneTarget{Name: "clone-target"},
			Options: &infrav1beta1.CloneOptions{Type: linked},
		},
	}
	cp := &clonerProvider{caps: contracts.Capabilities{SupportsLinkedClones: false}}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, 0, cp.cloneCnt, "Clone must not be called when linked clones are unsupported")
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, cloneReasonLinkedUnsupported, ready.Reason)
}

// TestVMClone_LinkedFailsOpen_NonReporter: type=LinkedClone with a provider that
// is NOT a CapabilityReporter → proceeds (fail open), clone happens.
func TestVMClone_LinkedFailsOpen_NonReporter(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	linked := infrav1beta1.CloneTypeLinkedClone
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-linked-open", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source:  infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target:  infrav1beta1.VMCloneTarget{Name: "clone-target"},
			Options: &infrav1beta1.CloneOptions{Type: linked},
		},
	}
	cp := &clonerProviderNoCaps{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-777"}}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Equal(t, 1, cp.cloneCnt, "fail-open: clone must proceed for non-CapabilityReporter providers")
	assert.Equal(t, infrav1beta1.CloneTypeLinkedClone, got.Status.ActualCloneType)
}

// TestVMClone_NonVMRefSource_Failed: a snapshotRef source (no vmRef) → Failed.
func TestVMClone_NonVMRefSource_Failed(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-snap", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{SnapshotRef: &infrav1beta1.LocalObjectReference{Name: "snap-1"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, 0, cp.cloneCnt)
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, cloneReasonUnsupportedSource, ready.Reason)
}

// TestVMClone_NonClonerProvider_Failed: provider does not implement Cloner → Failed.
func TestVMClone_NonClonerProvider_Failed(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-noclone", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	r := newCloneReconciler(s, &stubResolver{provider: &nonClonerProvider{}}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, infrav1beta1.VMCloneReasonUnsupported, ready.Reason)
}

// TestVMClone_SourceMissing_Pending: source VM does not exist → Pending, requeue.
func TestVMClone_SourceMissing_Pending(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-nosrc", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "ghost"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, clone)

	// First reconcile: add finalizer. Second: source missing → Pending + requeue.
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.NoError(t, err)
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.NoError(t, err)
	assert.True(t, res.RequeueAfter > 0, "expected a requeue while waiting for the source VM")

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, 0, cp.cloneCnt)
}

// TestVMClone_ResumeSeedsStatusIDOnExistingTarget is the regression test for the
// bind race found during lab E2E (PR #188 follow-up): a prior reconcile cloned
// the VM (TargetVMID persisted) and created the adopted target VM CR, but the
// Status.ID seed lost a race with the VirtualMachine controller and never landed
// — leaving the cloned VM orphaned (empty Status.ID, waiting forever) while the
// clone falsely reported Ready. On a subsequent reconcile, bindTargetVM must
// re-seed Status.ID from the persisted TargetVMID WITHOUT issuing a second
// clone, then finalize Ready.
func TestVMClone_ResumeSeedsStatusIDOnExistingTarget(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")

	// The partial-bind state: target VM CR exists (adopted) but Status.ID empty.
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clone-target",
			Namespace: ns,
			Labels:    map[string]string{AdoptedLabel: AdoptedLabelValue},
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-1"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "src-class"},
		},
	}

	// The clone already issued (TargetVMID persisted), not yet Ready.
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "clone-resume",
			Namespace:  ns,
			Finalizers: []string{vmCloneFinalizer},
		},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
		Status: infrav1beta1.VMCloneStatus{
			Phase:      infrav1beta1.ClonePhaseCloning,
			TargetVMID: "vm-clone-999",
		},
	}

	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, target, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	// Must NOT re-clone when a target VM ID is already recorded.
	assert.Equal(t, 0, cp.cloneCnt, "must not re-clone when TargetVMID is already set")

	// Status.ID must now be seeded on the pre-existing target VM.
	gotTarget := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "clone-target"}, gotTarget))
	assert.Equal(t, "vm-clone-999", gotTarget.Status.ID, "Status.ID must be seeded on resume (no orphan)")

	// And the clone finalizes Ready with a TargetRef.
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	require.NotNil(t, got.Status.TargetRef)
	assert.Equal(t, "clone-target", got.Status.TargetRef.Name)
}

// TestVMClone_DeletionDoesNotDeleteTarget: deleting a VMClone removes its
// finalizer but leaves the produced target VM intact.
func TestVMClone_DeletionDoesNotDeleteTarget(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"

	now := metav1.Now()
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-target", Namespace: ns},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-1"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "src-class"},
		},
	}
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "clone-del",
			Namespace:         ns,
			Finalizers:        []string{vmCloneFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
		Status: infrav1beta1.VMCloneStatus{
			Phase:     infrav1beta1.ClonePhaseReady,
			TargetRef: &infrav1beta1.LocalObjectReference{Name: "clone-target"},
		},
	}
	r := newCloneReconciler(s, &stubResolver{}, target, clone)

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.NoError(t, err)

	// VMClone finalizer removed → object can be GC'd (fake returns NotFound).
	got := &infrav1beta1.VMClone{}
	err = r.Get(context.Background(), client.ObjectKeyFromObject(clone), got)
	if err == nil {
		assert.NotContains(t, got.Finalizers, vmCloneFinalizer)
	}

	// Target VM must still exist.
	stillThere := &infrav1beta1.VirtualMachine{}
	assert.NoError(t, r.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "clone-target"}, stillThere))
}
