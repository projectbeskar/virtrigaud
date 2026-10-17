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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin rc2 R4 in the VMClone controller: a single-host clone
// creates its target VirtualMachine before the Clone, sends its UID, and a
// Clone sent again — after a lost record or an unknown outcome — is answered
// with the clone the first attempt made, as a re-sent Create is.

// cloneStep scripts one Clone attempt of a stampingCloner: make records a
// clone for the request's target UID (when none exists), err is the answer
// (nil: the clone's id).
type cloneStep struct {
	make bool
	err  error
}

// stampingCloner is a single-host provider that stamps each clone with its
// target VirtualMachine's UID and re-binds a Clone whose target UID already
// has a clone, as the vSphere, libvirt and Proxmox providers do.
type stampingCloner struct {
	stubProvider
	clones map[string]string // target uid -> cloned VM id
	reqs   []contracts.CloneRequest
	script []cloneStep
	// noRebind makes it a provider that predates supports_owner_rebind (it
	// still records clones as the test scripts them); capsErr fails
	// GetCapabilities.
	noRebind bool
	capsErr  error
}

// GetCapabilities reports supports_owner_rebind unless noRebind.
func (p *stampingCloner) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	if p.capsErr != nil {
		return contracts.Capabilities{}, p.capsErr
	}
	return contracts.Capabilities{SupportsOwnerRebind: !p.noRebind}, nil
}

// elapse makes uid's next attempt due now, as if its backoff had passed; the
// failing streak keeps its length.
func (p *cloneAttemptPacer) elapse(uid types.UID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.m[uid]
	if !ok {
		return
	}
	b := cloneAttemptBackoff(rec)
	rec.last = rec.last.Add(-b)
	rec.streakSince = rec.streakSince.Add(-b)
	p.m[uid] = rec
}

func (p *stampingCloner) Clone(_ context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	step := cloneStep{make: true}
	if i := len(p.reqs); i < len(p.script) {
		step = p.script[i]
	}
	p.reqs = append(p.reqs, req)
	if p.clones == nil {
		p.clones = map[string]string{}
	}
	id, made := p.clones[req.TargetVM.UID]
	if !made && step.make {
		id = fmt.Sprintf("%s.%s", req.TargetVM.Namespace, req.TargetVM.Name)
		p.clones[req.TargetVM.UID] = id
	}
	if step.err != nil {
		return contracts.CloneResponse{}, step.err
	}
	return contracts.CloneResponse{TargetVmID: id}, nil
}

var _ contracts.Cloner = (*stampingCloner)(nil)

// stampFixture is a single-host clone of src-vm into clone-target through a
// libvirt Provider (a type that re-binds stamped clones).
func stampFixture(t *testing.T, cp contracts.Provider) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	prov, src, clone := cloneDisclosureFixture()
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)
	return r, clone
}

// cloneTarget reads the clone's target VirtualMachine.
func cloneTarget(t *testing.T, r *VMCloneReconciler) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, vm))
	return vm
}

// requireSameStampedTarget asserts every Clone carried the target
// VirtualMachine's identity with its UID.
func requireSameStampedTarget(t *testing.T, r *VMCloneReconciler, cp *stampingCloner) {
	t.Helper()
	target := cloneTarget(t, r)
	require.NotEmpty(t, target.UID)
	want := contracts.ObjectIdentity{UID: string(target.UID), Namespace: "default", Name: "clone-target"}
	for i, req := range cp.reqs {
		assert.Equal(t, want, req.TargetVM, "attempt %d carries the target's UID", i)
	}
}

func TestVMClone_SingleHost_LostRecordIsReboundNotRecloned(t *testing.T) {
	cp := &stampingCloner{}
	r, clone := stampFixture(t, cp)
	base := r.Client
	r.Client = &failingRecordClient{Client: base, failures: 1,
		err: apierrors.NewForbidden(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmclones/status"}, "clone-1",
			fmt.Errorf("denied"))}

	reconcileCloneOnce(t, r, clone) // finalizer
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.Error(t, err, "the provider made the clone; its record was lost")
	require.Len(t, cp.reqs, 1)
	got := getClone(t, r, clone)
	assert.Empty(t, got.Status.TargetVMID)
	assert.True(t, cloneRequested(got), "the requested Clone is on record, so the next one is a re-send")

	r.Client = base
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	got = getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Len(t, cp.reqs, 2, "the Clone was sent again")
	assert.Len(t, cp.clones, 1, "and re-bound: no second clone")
	requireSameStampedTarget(t, r, cp)
	assert.Equal(t, "default.clone-target", cloneTarget(t, r).Status.ID, "bound to the clone the first attempt made")
	assert.False(t, cloneRequested(got), "the outcome is recorded")
}

func TestVMClone_SingleHost_UnknownOutcomeIsHeldAndResent(t *testing.T) {
	unknown := transportFailure("clone") // after the request was sent: the clone may exist
	cp := &stampingCloner{script: []cloneStep{{make: true, err: unknown}, {make: false, err: unknown}}}
	r, clone := stampFixture(t, cp)

	reconcileCloneOnce(t, r, clone) // finalizer
	res := reconcileCloneOnce(t, r, clone)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the blocked-VM backoff starts at 15 s")
	got, texts := requireCloneNoLeak(t, r, clone, "10.96.12.7")
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "never Failed for an unknown outcome")
	ready := cloneReadyCondition(t, got)
	assert.Equal(t, cloneReasonOutcomeUnknown, ready.Reason)
	assert.Equal(t, cloneOutcomeUnknownMessage, ready.Message, "a constant message")
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, 1, strings.Count(texts, "Warning "+cloneReasonOutcomeUnknown))
	assert.True(t, cloneRequested(got))
	assert.Empty(t, cloneTarget(t, r).Status.ID, "the target waits unbound")

	// Before the backoff has passed nothing is sent, whatever reconciles.
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 1, "the re-send waits for its backoff")

	// A repeated hold writes nothing and announces nothing.
	r.cloneAttempts.elapse(clone.UID)
	rv := getClone(t, r, clone).ResourceVersion
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 2)
	assert.Equal(t, rv, getClone(t, r, clone).ResourceVersion, "a repeated hold writes nothing")
	assert.Empty(t, recordedEvents(t, r))

	// The provider answers: the re-send re-binds the clone the first attempt made.
	r.cloneAttempts.elapse(clone.UID)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	got = getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Len(t, cp.reqs, 3)
	assert.Len(t, cp.clones, 1, "one clone, whatever the number of attempts")
	requireSameStampedTarget(t, r, cp)
	assert.Equal(t, "default.clone-target", cloneTarget(t, r).Status.ID)
}

func TestVMClone_SingleHost_FirstAttemptConflictFails(t *testing.T) {
	conflict := contracts.NewConflictError(`clone: libvirt domain "default.clone-target" already exists on the host and is not `+
		`owned by the clone's target VirtualMachine; nothing was cloned`, nil)
	cp := &stampingCloner{script: []cloneStep{{err: conflict}}}
	r, clone := stampFixture(t, cp)

	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a refusal of a first attempt: nothing was made")
	ready := cloneReadyCondition(t, got)
	assert.Equal(t, k8s.ReasonProviderConflict, ready.Reason)
	assert.Equal(t, cloneTargetConflictMessage, ready.Message, "a constant message")
	assert.Contains(t, strings.Join(recordedEvents(t, r), "\n"), "Warning ProviderConflict Clone refused: clone: libvirt domain",
		"the provider's answer is in the event")
	assert.Len(t, cp.reqs, 1)

	// The failed clone removes the target it created: the provider made nothing.
	reconcileCloneOnce(t, r, clone)
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "the unbound target is removed")
	cleanup := meta.FindStatusCondition(getClone(t, r, clone).Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cleanup)
	assert.Equal(t, cloneReasonTargetRemoved, cleanup.Reason)
}

// TestVMClone_SingleHost_ConflictAnsweringAResendIsHeld: a Conflict that
// answers a re-send says nothing about what the first attempt made (vSphere
// answers it for a clone still being made), so the clone keeps waiting.
func TestVMClone_SingleHost_ConflictAnsweringAResendIsHeld(t *testing.T) {
	cp := &stampingCloner{script: []cloneStep{
		{err: transportFailure("clone")},
		{err: contracts.NewConflictError("clone: vSphere VM \"clone-target\" already exists in the target folder", nil)},
	}}
	r, clone := stampFixture(t, cp)

	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	r.cloneAttempts.elapse(clone.UID)
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 2)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "never Failed on a re-send's answer")
	assert.Equal(t, cloneReasonOutcomeUnknown, cloneReadyCondition(t, got).Reason)
	cloneTarget(t, r) // kept
}

// TestVMClone_SingleHost_DeletedTargetIsNotReplacedAfterARequest: the target
// a held clone created is deleted; the clone is never sent again for a new
// target (a Clone stamped with another UID would not re-bind the first
// attempt's clone), and fails.
func TestVMClone_SingleHost_DeletedTargetIsNotReplacedAfterARequest(t *testing.T) {
	cp := &stampingCloner{script: []cloneStep{{make: true, err: transportFailure("clone")}}}
	r, clone := stampFixture(t, cp)
	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 1)
	require.NoError(t, r.Delete(context.Background(), cloneTarget(t, r)))

	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneReasonTargetConflict, cloneReadyCondition(t, got).Reason)
	assert.Contains(t, got.Status.Message, "was deleted while a Clone sent for it had no recorded outcome")
	assert.Len(t, cp.reqs, 1, "never sent again")
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "no new target is created")
}

// TestVMClone_SingleHost_NonRebindingProviderStillFails: a provider that does
// not report supports_owner_rebind (an image that predates it — even of a type
// whose current image re-binds) keeps the historical behaviour: an unknown
// outcome fails the clone (sending it again could make a second VM), and no
// CloneRequested record is written.
func TestVMClone_SingleHost_NonRebindingProviderStillFails(t *testing.T) {
	cp := &stampingCloner{noRebind: true, script: []cloneStep{{make: true, err: transportFailure("clone")}}}
	r, clone := stampFixture(t, cp) // a libvirt Provider: the capability decides, not the type

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.False(t, cloneRequested(got))
	require.Len(t, cp.reqs, 1, "never sent again")
	assert.Equal(t, string(clone.UID), "uid-default-clone-1")
	assert.Equal(t, "uid-default-clone-target", cp.reqs[0].TargetVM.UID, "the Clone still carries the target's UID")
}

// TestVMClone_SingleHost_NoCloneWithoutTheRequestRecord: when the
// CloneRequested record cannot be written, nothing is sent.
func TestVMClone_SingleHost_NoCloneWithoutTheRequestRecord(t *testing.T) {
	cp := &stampingCloner{}
	prov, src, clone := cloneDisclosureFixture()
	prov.Spec.Type = infrav1beta1.ProviderTypeVSphere
	ensureCloneUIDs(clone)
	s := cloneTestScheme(t)
	refuse := true
	funcs := assignUIDOnCreate
	funcs.SubResourceUpdate = func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if c, ok := obj.(*infrav1beta1.VMClone); ok && refuse && cloneRequested(c) {
			return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmclones"}, c.Name,
				fmt.Errorf("the object has been modified"))
		}
		return cl.SubResource(sub).Update(ctx, obj, opts...)
	}
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, src, clone).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).
		WithIndex(&infrav1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithInterceptorFuncs(funcs).Build()
	r := &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &okResolver{provider: cp}, Recorder: record.NewFakeRecorder(20)}

	for i := 0; i < 3; i++ {
		res := reconcileCloneOnce(t, r, clone)
		if i > 0 {
			assert.Equal(t, cloneStaleReadRetryInterval, res.RequeueAfter)
		}
	}
	assert.Empty(t, cp.reqs, "no Clone without its record")
	assert.False(t, cloneRequested(getClone(t, r, clone)))

	refuse = false
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	assert.Len(t, cp.reqs, 1)
}

// TestVMClone_StaleTargetUIDIsNeverStamped: a target re-created under the
// target name (carrying a copied clone-uid annotation) is not the object the
// clone recorded; its UID is never sent, and nothing is cloned.
func TestVMClone_StaleTargetUIDIsNeverStamped(t *testing.T) {
	cp := &stampingCloner{}
	r, clone := stampFixture(t, cp)
	reconcileCloneOnce(t, r, clone) // finalizer
	live := getClone(t, r, clone)
	live.Status.TargetUID = "uid-original"
	require.NoError(t, r.Status().Update(context.Background(), live))
	replaced := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-target", Namespace: "default",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: string(clone.UID)}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-1"}},
	}
	require.NoError(t, r.Create(context.Background(), replaced))

	reconcileCloneOnce(t, r, clone)
	assert.Empty(t, cp.reqs, "never stamped with another object's UID")
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase)
}

// TestVMClone_SingleHost_ResendNeverGoesToAProviderWithoutTheCapability: a
// Clone whose outcome is unknown is never sent again once the provider stops
// reporting supports_owner_rebind (e.g. an older image rolled back in): the
// clone fails with a constant message, and its target is kept, since the
// provider may hold a clone stamped with it.
func TestVMClone_SingleHost_ResendNeverGoesToAProviderWithoutTheCapability(t *testing.T) {
	cp := &stampingCloner{script: []cloneStep{{make: true, err: transportFailure("clone")}}}
	r, clone := stampFixture(t, cp)
	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 1)
	require.True(t, cloneRequested(getClone(t, r, clone)))

	cp.noRebind = true
	r.cloneAttempts.elapse(clone.UID)
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Len(t, cp.reqs, 1, "never sent again")
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneReasonOutcomeUnknown, cloneReadyCondition(t, got).Reason)
	assert.Equal(t, cloneResendNotSupportedMessage, got.Status.Message)

	reconcileCloneOnce(t, r, clone) // the failed clone's one-time target decision
	cloneTarget(t, r)               // kept
	cleanup := meta.FindStatusCondition(getClone(t, r, clone).Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cleanup)
	assert.Equal(t, cloneReasonTargetKept, cleanup.Reason)
}

// TestVMClone_SingleHost_UnreadableCapabilitiesSendNothing: nothing is sent
// while the provider cannot tell whether it re-binds stamped clones.
func TestVMClone_SingleHost_UnreadableCapabilitiesSendNothing(t *testing.T) {
	cp := &stampingCloner{capsErr: fmt.Errorf("provider unreachable")}
	r, clone := stampFixture(t, cp)
	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	assert.Empty(t, cp.reqs)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, cloneCapabilitiesRebindWaitMessage, got.Status.Message)
}

// TestVMClone_SingleHost_UnrecordedTargetUIDIsRecordedFirst (review M1): the
// manager stopped between creating the target and recording its UID. The
// next reconcile records it before any Clone is sent; and a clone whose
// requested Clone is on record never replaces a deleted target, whether or
// not its UID was recorded.
func TestVMClone_SingleHost_UnrecordedTargetUIDIsRecordedFirst(t *testing.T) {
	cp := &stampingCloner{script: []cloneStep{{make: true, err: transportFailure("clone")}}}
	prov, src, clone := cloneDisclosureFixture()
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	ensureCloneUIDs(clone)
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-target", Namespace: "default", UID: "uid-made-before-the-crash",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: string(clone.UID)}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-1"},
			ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}},
	}
	r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone, target)

	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	require.Len(t, cp.reqs, 1)
	assert.Equal(t, "uid-made-before-the-crash", cp.reqs[0].TargetVM.UID)
	assert.Equal(t, "uid-made-before-the-crash", getClone(t, r, clone).Status.TargetUID, "recorded before the Clone")

	// The record is lost again (worst case): the guard still holds.
	live := getClone(t, r, clone)
	live.Status.TargetUID = ""
	require.NoError(t, r.Status().Update(context.Background(), live))
	require.NoError(t, r.Delete(context.Background(), cloneTarget(t, r)))
	r.cloneAttempts.elapse(clone.UID)
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneReasonTargetConflict, cloneReadyCondition(t, got).Reason)
	assert.Len(t, cp.reqs, 1, "never sent again for a new target")
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "no new target is created")
}

// TestVMClone_SingleHost_MissingTargetIsNeverRecreatedForABind (review L2):
// the provider made the clone, and the target this clone created is gone
// before the bind: no new VirtualMachine is created for the cloned VM.
func TestVMClone_SingleHost_MissingTargetIsNeverRecreatedForABind(t *testing.T) {
	cp := &stampingCloner{}
	prov, src, clone := cloneDisclosureFixture()
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	clone.Finalizers = []string{infrav1beta1.VMCloneFinalizer}
	clone.Status = infrav1beta1.VMCloneStatus{Phase: infrav1beta1.ClonePhaseCloning, TargetVMID: "default.clone-target",
		TargetUID: "uid-gone"}
	r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)

	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneReasonTargetConflict, cloneReadyCondition(t, got).Reason)
	assert.Contains(t, got.Status.Message, `the cloned VM "default.clone-target" is not bound to a new one`)
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "no new target is created")
	assert.Empty(t, cp.reqs)
}

// TestVMClone_SingleHost_NothingDoneDropsTheRequest (review L4): a first
// attempt answered "nothing done" drops the CloneRequested record in the
// same hold write, so a real refusal of the next attempt fails the clone
// instead of being held as an unknown outcome.
func TestVMClone_SingleHost_NothingDoneDropsTheRequest(t *testing.T) {
	cp := &stampingCloner{script: []cloneStep{
		{err: contracts.NewHostUnavailableError("clone: host unreachable", nil)},
		{err: contracts.NewConflictError("clone: libvirt domain \"default.clone-target\" already exists", nil)},
	}}
	r, clone := stampFixture(t, cp)
	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, cloneReasonRetrying, cloneReadyCondition(t, got).Reason)
	assert.False(t, cloneRequested(got), "nothing was made: no request on record")

	r.cloneAttempts.elapse(clone.UID)
	reconcileCloneOnce(t, r, clone)
	got = getClone(t, r, clone)
	require.Len(t, cp.reqs, 2)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a first attempt's refusal fails the clone")
	assert.Equal(t, k8s.ReasonProviderConflict, cloneReadyCondition(t, got).Reason)
}

// TestVMClone_FailedSingleHostCloneKeepsATargetWithARequestOnRecord (review
// L1): a single-host clone that failed with a Clone of unknown outcome on
// record (e.g. a re-send stopped by a pre-clone check) keeps its target: the
// provider may hold a clone stamped with it.
func TestVMClone_FailedSingleHostCloneKeepsATargetWithARequestOnRecord(t *testing.T) {
	r, clone := failedCloneWithTarget(t, cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{}),
		infrav1beta1.ProviderTopologySingle)
	live := getClone(t, r, clone)
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: cloneConditionRequested,
		Status: metav1.ConditionTrue, Reason: cloneReasonRequested, Message: cloneRequestedMessage})
	require.NoError(t, r.Status().Update(context.Background(), live))

	reconcileClone(t, r, clone, 1)
	assert.False(t, targetGone(t, r), "kept")
	cleanup := meta.FindStatusCondition(getClone(t, r, clone).Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cleanup)
	assert.Equal(t, cloneReasonTargetKept, cleanup.Reason)
	assert.Contains(t, cleanup.Message, "its outcome is unknown")
}

// TestCloneAttemptPacer pins the backoff of held attempts: 15 s after the
// first failure, then as long as the streak has lasted (up to 5 min), the
// streak counted from the earlier of the recorded start and the hold's.
func TestCloneAttemptPacer(t *testing.T) {
	var p cloneAttemptPacer
	t0 := time.Now()
	assert.Zero(t, p.wait("u", t0), "no record: due")
	assert.Equal(t, blockedRetryMin, p.failed("u", time.Time{}, t0))
	assert.Equal(t, 10*time.Second, p.wait("u", t0.Add(5*time.Second)))
	assert.Equal(t, time.Minute, p.failed("u", time.Time{}, t0.Add(time.Minute)), "the streak has lasted a minute")
	assert.Equal(t, blockedRetryMax, p.failed("u", t0.Add(-time.Hour), t0.Add(2*time.Minute)), "capped; an older hold counts")
	p.forget("u")
	assert.Zero(t, p.wait("u", t0.Add(2*time.Minute)))
}
