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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin finding V2 of the v0.4.0-rc5 validation: spec.snapshot.
// revertToRef was accepted but never acted on (the VM stayed Ready with no
// condition and no event). The revert is now sent only to a VM observed off
// whose spec.powerState is Off, at most once per request, with its attempt
// recorded before the call.

const (
	revertVMUID       = "uid-test-vm"
	revertSnapName    = "before-upgrade"
	revertSnapUID     = "uid-snap-1"
	revertSnapID      = "before-upgrade"
	revertTaskRef     = "UPID:pve:revert"
	revertTestTimeout = time.Minute
)

// revertProvider reports describe, answers SnapshotRevert from revertAnswers
// (the last one repeats) and IsTaskComplete from taskDone/taskErr; it records
// every SnapshotRevert and Power call.
type revertProvider struct {
	stubProvider
	describe contracts.DescribeResponse

	reverts       []string
	revertRefs    []contracts.VMRef
	revertAnswers []revertAnswer

	taskDone bool
	taskErr  error
	powers   int
}

// revertAnswer is one scripted SnapshotRevert answer.
type revertAnswer struct {
	taskRef string
	err     error
}

func (p *revertProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return p.describe, nil
}

func (p *revertProvider) Power(_ context.Context, _ contracts.VMRef, op contracts.PowerOp) (string, error) {
	p.powers++
	if op == contracts.PowerOpOn {
		p.describe.PowerState = string(contracts.PowerStateOn)
	} else {
		p.describe.PowerState = string(contracts.PowerStateOff)
	}
	return "", nil
}

func (p *revertProvider) SnapshotRevert(_ context.Context, vm contracts.VMRef, snapshotID string) (string, error) {
	p.reverts = append(p.reverts, snapshotID)
	p.revertRefs = append(p.revertRefs, vm)
	if len(p.revertAnswers) == 0 {
		return "", nil
	}
	a := p.revertAnswers[0]
	if len(p.revertAnswers) > 1 {
		p.revertAnswers = p.revertAnswers[1:]
	}
	return a.taskRef, a.err
}

func (p *revertProvider) IsTaskComplete(_ context.Context, _ string) (bool, error) {
	return p.taskDone, p.taskErr
}

// readySnapshot is a Ready VMSnapshot of the test VM (revertVMUID) taken
// through test-prov.
func revertSnapshotOf(name, uid string) *infravirtrigaudiov1beta1.VMSnapshot {
	return &infravirtrigaudiov1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "test-vm"}},
		Status: infravirtrigaudiov1beta1.VMSnapshotStatus{
			Phase:         infravirtrigaudiov1beta1.SnapshotPhaseReady,
			SnapshotID:    name,
			VMUID:         revertVMUID,
			BoundProvider: &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "test-prov"},
		},
	}
}

// revertFixture seeds a bound single-host VM (spec.powerState desired, the
// provider reporting reported, revertToRef naming revertSnapName) with its
// Provider (changed by mutateProvider, when given), class and the extra
// objects. The clock starts at a fixed time and is advanced with *now.
func revertFixture(t *testing.T, desired infravirtrigaudiov1beta1.PowerState, reported contracts.PowerState,
	mutateProvider func(*infravirtrigaudiov1beta1.Provider), extra ...client.Object) (*VirtualMachineReconciler, *revertProvider, *record.FakeRecorder, *time.Time) {
	t.Helper()
	prov := &revertProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: string(reported), IPs: []string{"10.0.0.5"}}}
	providerCR, class := providerAndClass("default")
	if mutateProvider != nil {
		mutateProvider(providerCR)
	}
	vm := baseVM("default")
	vm.UID = revertVMUID
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Spec.PowerState = desired
	vm.Spec.Snapshot = &infravirtrigaudiov1beta1.VMSnapshotOperation{
		RevertToRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: revertSnapName}}
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: providerCR.Name}
	cpu, mem := int32(4), int64(8192)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	objs := append([]client.Object{providerCR, class, vm}, extra...)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, objs...)
	rec := record.NewFakeRecorder(100)
	r.Recorder = rec
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	r.clock = func() time.Time { return now }
	return r, prov, rec, &now
}

// storedVM returns the test VM as stored.
func revertStoredVM(t *testing.T, r *VirtualMachineReconciler) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-vm"}, vm))
	return vm
}

// reconcileStored reconciles the VM as stored and returns it as stored after.
func reconcileStored(t *testing.T, r *VirtualMachineReconciler) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), revertTestTimeout)
	defer cancel()
	_, err := r.reconcileVM(ctx, revertStoredVM(t, r))
	require.NoError(t, err)
	return revertStoredVM(t, r)
}

func revertCondition(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return meta.FindStatusCondition(vm.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted)
}

func revertReadyCondition(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
}

// requireRevertCondition asserts the SnapshotReverted condition's status and
// reason, its observedGeneration, and that Ready agrees with it.
func requireRevertCondition(t *testing.T, vm *infravirtrigaudiov1beta1.VirtualMachine, st metav1.ConditionStatus, reason string) {
	t.Helper()
	c := revertCondition(vm)
	require.NotNil(t, c, "SnapshotReverted condition")
	assert.Equal(t, st, c.Status)
	assert.Equal(t, reason, c.Reason)
	assert.Equal(t, vm.Generation, c.ObservedGeneration)
	ready := revertReadyCondition(vm)
	require.NotNil(t, ready)
	if st == metav1.ConditionTrue {
		assert.Equal(t, metav1.ConditionTrue, ready.Status, "Ready once the revert is done")
	} else {
		assert.Equal(t, metav1.ConditionFalse, ready.Status, "Ready never claims success while a revert is pending or failed")
		assert.Equal(t, reason, ready.Reason)
	}
}

func revertEvents(rec *record.FakeRecorder, reason string) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, " "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}

// TestSnapshotRevert_HappyPathOnceOnly: a powered-off VM is reverted once; no
// later reconcile, and no unrelated spec change, reverts it again.
func TestSnapshotRevert_HappyPathOnceOnly(t *testing.T) {
	r, prov, rec, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))

	got := reconcileStored(t, r)
	assert.Equal(t, []string{revertSnapID}, prov.reverts, "exactly one SnapshotRevert with the VMSnapshot's provider id")
	assert.Equal(t, contracts.VMRef{ID: "vm-100", Owner: contracts.ObjectIdentity{UID: revertVMUID, Namespace: "default", Name: "test-vm"}},
		prov.revertRefs[0])
	require.NotNil(t, got.Status.SnapshotRevert)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	assert.Equal(t, revertSnapName, got.Status.SnapshotRevert.SnapshotName)
	assert.Equal(t, revertSnapUID, got.Status.SnapshotRevert.SnapshotUID)
	assert.Equal(t, int32(1), got.Status.SnapshotRevert.Attempts)
	assert.NotNil(t, got.Status.SnapshotRevert.RequestedAt)
	assert.NotNil(t, got.Status.SnapshotRevert.AttemptTime)
	assert.NotNil(t, got.Status.SnapshotRevert.CompletionTime)
	assert.Equal(t, 0, prov.powers, "the controller does not power the VM on after the revert")
	c := revertCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted, c.Reason)
	assert.Len(t, revertEvents(rec, eventReasonSnapshotRevertStarted), 1)
	assert.Empty(t, revertEvents(rec, eventReasonSnapshotRevertFailed))

	// The next reconcile reports Ready and sends nothing.
	got = reconcileStored(t, r)
	requireRevertCondition(t, got, metav1.ConditionTrue, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted)
	assert.Len(t, prov.reverts, 1)

	// An unrelated spec change (a new generation) reverts nothing.
	got.Spec.Tags = []string{"changed"}
	got.Generation++
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1, "never re-sent because another spec field changed")
	requireRevertCondition(t, got, metav1.ConditionTrue, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted)

	// Powering the VM on afterwards follows spec.powerState, and still no revert.
	got.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	require.NoError(t, r.Update(context.Background(), got))
	_ = reconcileStored(t, r)
	assert.Equal(t, 1, prov.powers)
	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)
}

// TestSnapshotRevert_WaitsForPowerOff: no revert is sent to a VM that is on
// or whose spec.powerState is On; the controller never powers it off for it.
func TestSnapshotRevert_WaitsForPowerOff(t *testing.T) {
	t.Run("spec On, VM on", func(t *testing.T) {
		r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOn, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
		assert.Equal(t, 0, prov.powers, "never powered off for a revert")
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff)
		assert.Contains(t, revertCondition(got).Message, "set spec.powerState: Off")
		require.NotNil(t, got.Status.SnapshotRevert)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhasePending, got.Status.SnapshotRevert.Phase)
		assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, got.Status.Phase)

		// A repeated reconcile writes the same status.
		again := reconcileStored(t, r)
		assert.Equal(t, got.Status.SnapshotRevert, again.Status.SnapshotRevert)
		assert.Equal(t, revertCondition(got).LastTransitionTime, revertCondition(again).LastTransitionTime)
	})
	t.Run("spec On, VM off", func(t *testing.T) {
		r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts, "a VM whose spec says On is not reverted even while it is off")
		assert.Equal(t, 1, prov.powers, "it is powered on as its spec says")
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff)
	})
	t.Run("spec Off, VM still on: powered off as spec says, then reverted", func(t *testing.T) {
		r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOn, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
		assert.Equal(t, 1, prov.powers)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff, revertCondition(got).Reason)

		got = reconcileStored(t, r)
		assert.Equal(t, []string{revertSnapID}, prov.reverts)
		requireRevertCondition(t, got, metav1.ConditionTrue, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted)
	})
	t.Run("OffGraceful counts as off", func(t *testing.T) {
		r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOffGraceful, contracts.PowerStateOff, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		_ = reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1)
	})
}

// TestSnapshotRevert_RefusesSnapshot: a VMSnapshot that is missing, not
// Ready, being deleted, or not proven to be this VM's is never reverted to.
func TestSnapshotRevert_RefusesSnapshot(t *testing.T) {
	deleting := revertSnapshotOf(revertSnapName, revertSnapUID)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{infravirtrigaudiov1beta1.VMSnapshotFinalizer}

	notReady := revertSnapshotOf(revertSnapName, revertSnapUID)
	notReady.Status.Phase = infravirtrigaudiov1beta1.SnapshotPhaseCreating
	noID := revertSnapshotOf(revertSnapName, revertSnapUID)
	noID.Status.SnapshotID = ""
	otherVMRef := revertSnapshotOf(revertSnapName, revertSnapUID)
	otherVMRef.Spec.VMRef.Name = "other-vm"
	otherUID := revertSnapshotOf(revertSnapName, revertSnapUID)
	otherUID.Status.VMUID = "uid-earlier-vm"
	unbound := revertSnapshotOf(revertSnapName, revertSnapUID)
	unbound.Status.VMUID = ""
	otherProvider := revertSnapshotOf(revertSnapName, revertSnapUID)
	otherProvider.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "another-prov"}

	for _, tc := range []struct {
		name   string
		snap   client.Object
		reason string
	}{
		{"missing", nil, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotFound},
		{"not Ready", notReady, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady},
		{"no provider snapshot id", noID, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady},
		{"being deleted", deleting, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotNotReady},
		{"of another VM (spec.vmRef)", otherVMRef, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM},
		{"of an earlier VM of the same name (UID)", otherUID, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM},
		{"not bound to any VM UID", unbound, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM},
		{"taken through another Provider", otherProvider, infravirtrigaudiov1beta1.SnapshotRevertReasonSnapshotForAnotherVM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var extra []client.Object
			if tc.snap != nil {
				extra = append(extra, tc.snap)
			}
			r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil, extra...)
			ctx := context.Background()
			res, err := r.reconcileVM(ctx, revertStoredVM(t, r))
			require.NoError(t, err)
			got := revertStoredVM(t, r)
			assert.Empty(t, prov.reverts, "no SnapshotRevert")
			requireRevertCondition(t, got, metav1.ConditionFalse, tc.reason)
			assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhasePending, got.Status.SnapshotRevert.Phase)
			assert.Positive(t, res.RequeueAfter)
			assert.LessOrEqual(t, res.RequeueAfter, snapshotRevertRecheckInterval, "re-checked, not left for the slow poll")
		})
	}
}

// TestSnapshotRevert_Unsupported: a Provider that reports no snapshot support
// is never sent a revert; an Unimplemented answer fails it as Unsupported.
func TestSnapshotRevert_Unsupported(t *testing.T) {
	t.Run("reported capabilities", func(t *testing.T) {
		r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff,
			func(p *infravirtrigaudiov1beta1.Provider) {
				p.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{SupportsSnapshots: false}
			}, revertSnapshotOf(revertSnapName, revertSnapUID))
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported)
	})
	t.Run("Unimplemented answer", func(t *testing.T) {
		r, prov, rec, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		prov.revertAnswers = []revertAnswer{{err: contracts.NewNotSupportedError("snapshotRevert: not implemented")}}
		got := reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1)
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
		assert.Len(t, revertEvents(rec, eventReasonSnapshotRevertFailed), 1)
		got = reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1, "terminal: not re-sent")
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonUnsupported)
	})
}

// TestSnapshotRevert_AsyncTaskPolledToCompletion: a revert answered with a
// task is polled; while it runs nothing else is done to the VM (a spec that
// now asks for On is not applied), and it ends Succeeded.
func TestSnapshotRevert_AsyncTaskPolledToCompletion(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{taskRef: revertTaskRef}}

	got := reconcileStored(t, r)
	require.NotNil(t, got.Status.SnapshotRevert)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase)
	assert.Equal(t, revertTaskRef, got.Status.SnapshotRevert.TaskRef)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting)

	// The user asks for On while the task runs: nothing is powered on.
	got.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	assert.Equal(t, 0, prov.powers, "nothing is powered on under a running revert")
	assert.Len(t, prov.reverts, 1)
	assert.Equal(t, revertTaskRef, got.Status.SnapshotRevert.TaskRef)

	prov.taskDone = true
	got = reconcileStored(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	assert.Empty(t, got.Status.SnapshotRevert.TaskRef)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted, revertCondition(got).Reason)

	// Then spec.powerState is applied, and nothing is reverted again.
	_ = reconcileStored(t, r)
	assert.Equal(t, 1, prov.powers)
	assert.Len(t, prov.reverts, 1)
}

// TestSnapshotRevert_AsyncTaskFailed: a task that ends in failure is terminal.
func TestSnapshotRevert_AsyncTaskFailed(t *testing.T) {
	r, prov, rec, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{taskRef: revertTaskRef}}
	_ = reconcileStored(t, r)

	prov.taskDone = true
	prov.taskErr = errors.New("task failed: rollback failed: storage busy")
	got := reconcileStored(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed)
	assert.Contains(t, got.Status.SnapshotRevert.Message, "storage busy")
	assert.Len(t, revertEvents(rec, eventReasonSnapshotRevertFailed), 1)

	prov.taskDone, prov.taskErr = false, nil
	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1, "a failed revert is not retried")
}

// TestSnapshotRevert_TaskUnknownToProviderIsResent: a task the provider no
// longer knows leaves the outcome unknown: the task is forgotten, and the
// revert is sent again (the VM is still off) once the resend wait is over.
func TestSnapshotRevert_TaskUnknownToProviderIsResent(t *testing.T) {
	r, prov, _, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{taskRef: revertTaskRef}, {}}
	_ = reconcileStored(t, r)

	prov.taskErr = contracts.NewNotFoundError("task not found", nil)
	got := reconcileStored(t, r)
	assert.Empty(t, got.Status.SnapshotRevert.TaskRef)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase)

	*now = now.Add(snapshotRevertResendWait)
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	assert.Equal(t, int32(2), got.Status.SnapshotRevert.Attempts)
}

// TestSnapshotRevert_TerminalFailure: a definite provider failure is Failed
// with the provider's (sanitised) message, and never retried.
func TestSnapshotRevert_TerminalFailure(t *testing.T) {
	r, prov, rec, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: errors.New("snapshotRevert failed: failed to revert to snapshot: " +
		"https://admin:s3cret@vcenter.example/sdk refused\nsecond line")}}

	got := reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)
	require.NotNil(t, got.Status.SnapshotRevert)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
	assert.NotNil(t, got.Status.SnapshotRevert.CompletionTime)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed)
	msg := got.Status.SnapshotRevert.Message
	assert.Contains(t, msg, "failed to revert to snapshot")
	assert.Contains(t, msg, "remove spec.snapshot.revertToRef and set it again to retry")
	assert.NotContains(t, msg, "s3cret", "no credential reaches status")
	assert.NotContains(t, msg, "\n")
	assert.NotContains(t, revertReadyCondition(got).Message, "s3cret")
	failed := revertEvents(rec, eventReasonSnapshotRevertFailed)
	require.Len(t, failed, 1)
	assert.NotContains(t, failed[0], "s3cret")

	for i := 0; i < 3; i++ {
		_ = reconcileStored(t, r)
	}
	assert.Len(t, prov.reverts, 1, "a terminal failure is not retried")
}

// TestSnapshotRevert_RemovedClearsRecord: removing revertToRef clears the
// record and the condition, and Ready is True again.
func TestSnapshotRevert_RemovedClearsRecord(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: errors.New("revert failed")}}
	got := reconcileStored(t, r)
	require.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)

	got.Spec.Snapshot = nil
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	assert.Nil(t, got.Status.SnapshotRevert)
	assert.Nil(t, revertCondition(got))
	assert.Equal(t, metav1.ConditionTrue, revertReadyCondition(got).Status)
	assert.Len(t, prov.reverts, 1)
}

// TestSnapshotRevert_RemoveAndReAddRevertsAgain: removing the field, then
// setting it again to the same VMSnapshot, is a new request.
func TestSnapshotRevert_RemoveAndReAddRevertsAgain(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	got := reconcileStored(t, r)
	require.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)

	snapOp := got.Spec.Snapshot
	got.Spec.Snapshot = nil
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	require.Nil(t, got.Status.SnapshotRevert)

	got.Spec.Snapshot = snapOp
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2, "re-adding the field reverts again")
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	assert.Equal(t, int32(1), got.Status.SnapshotRevert.Attempts)
}

// TestSnapshotRevert_AnotherSnapshotRevertsAgain: naming another VMSnapshot,
// or a VMSnapshot re-created under the same name (another UID), is a new
// request.
func TestSnapshotRevert_AnotherSnapshotRevertsAgain(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID), revertSnapshotOf("after-upgrade", "uid-snap-2"))
	_ = reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)

	got := revertStoredVM(t, r)
	got.Spec.Snapshot.RevertToRef.Name = "after-upgrade"
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileStored(t, r)
	assert.Equal(t, []string{revertSnapID, "after-upgrade"}, prov.reverts)
	assert.Equal(t, "uid-snap-2", got.Status.SnapshotRevert.SnapshotUID)

	// The VMSnapshot it names is deleted and re-created under the same name.
	old := &infravirtrigaudiov1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "after-upgrade"}, old))
	require.NoError(t, r.Delete(context.Background(), old))
	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2, "a deleted VMSnapshot does not undo a finished revert, nor repeat it")
	require.NoError(t, r.Create(context.Background(), revertSnapshotOf("after-upgrade", "uid-snap-3")))
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 3, "a VMSnapshot re-created under the same name is another snapshot")
	assert.Equal(t, "uid-snap-3", got.Status.SnapshotRevert.SnapshotUID)
}

// TestSnapshotRevert_ManagerRestartMidCall: the record says Reverting with no
// task (the answer was lost): the revert is sent again once — after the
// resend wait, and only while a fresh Describe shows the VM off with
// spec.powerState Off.
func TestSnapshotRevert_ManagerRestartMidCall(t *testing.T) {
	setup := func(t *testing.T, reported contracts.PowerState) (*VirtualMachineReconciler, *revertProvider, *time.Time) {
		r, prov, _, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, reported, nil,
			revertSnapshotOf(revertSnapName, revertSnapUID))
		vm := revertStoredVM(t, r)
		attempt := metav1.NewTime(now.Add(-time.Minute))
		vm.Status.SnapshotRevert = &infravirtrigaudiov1beta1.SnapshotRevertStatus{
			SnapshotName: revertSnapName, SnapshotUID: revertSnapUID,
			Phase: infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, Attempts: 1,
			RequestedAt: &attempt, AttemptTime: &attempt, Message: "reverting",
		}
		require.NoError(t, r.Status().Update(context.Background(), vm))
		return r, prov, now
	}

	t.Run("VM still off: re-sent once after the wait", func(t *testing.T) {
		r, prov, now := setup(t, contracts.PowerStateOff)
		ctx := context.Background()
		res, err := r.reconcileVM(ctx, revertStoredVM(t, r))
		require.NoError(t, err)
		assert.Empty(t, prov.reverts, "not before the resend wait")
		assert.Equal(t, snapshotRevertResendWait-time.Minute, res.RequeueAfter)
		requireRevertCondition(t, revertStoredVM(t, r), metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting)

		*now = now.Add(snapshotRevertResendWait)
		got := reconcileStored(t, r)
		assert.Equal(t, []string{revertSnapID}, prov.reverts, "re-sent once")
		assert.Equal(t, int32(2), got.Status.SnapshotRevert.Attempts)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
		_ = reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1)
	})
	t.Run("VM powered on meanwhile: not re-sent", func(t *testing.T) {
		r, prov, now := setup(t, contracts.PowerStateOn)
		*now = now.Add(snapshotRevertResendWait)
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase,
			"the earlier attempt's outcome stays unknown")
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff)
		assert.Equal(t, 1, prov.powers, "powered off because spec.powerState is Off, not for the revert")

		got = reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1, "re-sent once the VM is observed off")
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	})
	t.Run("VMSnapshot deleted meanwhile: Failed, not re-sent", func(t *testing.T) {
		r, prov, now := setup(t, contracts.PowerStateOff)
		old := &infravirtrigaudiov1beta1.VMSnapshot{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: revertSnapName}, old))
		require.NoError(t, r.Delete(context.Background(), old))
		*now = now.Add(snapshotRevertResendWait)
		got := reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
		assert.Contains(t, got.Status.SnapshotRevert.Message, "may or may not have been reverted")
		requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed)
		_ = reconcileStored(t, r)
		assert.Empty(t, prov.reverts)
	})
	t.Run("VMSnapshot re-created under the name meanwhile: a new request", func(t *testing.T) {
		r, prov, _ := setup(t, contracts.PowerStateOff)
		old := &infravirtrigaudiov1beta1.VMSnapshot{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: revertSnapName}, old))
		require.NoError(t, r.Delete(context.Background(), old))
		require.NoError(t, r.Create(context.Background(), revertSnapshotOf(revertSnapName, "uid-snap-new")))
		got := reconcileStored(t, r)
		assert.Len(t, prov.reverts, 1, "the VMSnapshot the field names now is reverted to, with no resend wait")
		assert.Equal(t, "uid-snap-new", got.Status.SnapshotRevert.SnapshotUID)
		assert.Equal(t, int32(1), got.Status.SnapshotRevert.Attempts)
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	})
}

// TestSnapshotRevert_UnknownOutcomeBounded: an answer without a definite
// outcome keeps the record Reverting; the revert is re-sent on a doubling wait
// and, after snapshotRevertMaxAttempts calls, Failed.
func TestSnapshotRevert_UnknownOutcomeBounded(t *testing.T) {
	r, prov, rec, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: contracts.NewRetryableError("snapshotRevert: context deadline exceeded",
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"))}}

	got := reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase)
	assert.Empty(t, got.Status.SnapshotRevert.TaskRef)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting)
	assert.Contains(t, got.Status.SnapshotRevert.Message, providerNotReachedDetail)

	// Not re-sent before the wait.
	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)

	for attempt := 2; attempt <= snapshotRevertMaxAttempts; attempt++ {
		*now = now.Add(snapshotRevertResendMaxWait)
		got = reconcileStored(t, r)
		assert.Len(t, prov.reverts, attempt)
		assert.Equal(t, int32(attempt), got.Status.SnapshotRevert.Attempts)
	}
	*now = now.Add(snapshotRevertResendMaxWait)
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, snapshotRevertMaxAttempts, "bounded")
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonRevertFailed)
	assert.Len(t, revertEvents(rec, eventReasonSnapshotRevertFailed), 1)
}

// TestSnapshotRevert_StaleReadSendsNothing: the attempt is recorded with the
// optimistic lock of the resourceVersion the reconcile read; a VM read from a
// stale cache is not reverted.
func TestSnapshotRevert_StaleReadSendsNothing(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	stale := revertStoredVM(t, r)
	// Someone else writes the VM in the meantime.
	fresh := revertStoredVM(t, r)
	fresh.Labels = map[string]string{"touched": "true"}
	require.NoError(t, r.Update(context.Background(), fresh))

	_, err := r.reconcileVM(context.Background(), stale)
	require.NoError(t, err)
	assert.Empty(t, prov.reverts, "nothing is sent when the attempt cannot be recorded")

	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)
}

// TestSnapshotRevert_ClusteredRoutedWithOwner: on a clustered Provider the
// revert is routed to the VM's bound host and carries its owner.
func TestSnapshotRevert_ClusteredRoutedWithOwner(t *testing.T) {
	prov := &revertProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
	vm := resizedBoundClusterVM("app")
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	vm.Spec.Snapshot = &infravirtrigaudiov1beta1.VMSnapshotOperation{
		RevertToRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: revertSnapName}}
	snap := revertSnapshotOf(revertSnapName, revertSnapUID)
	snap.Spec.VMRef.Name = "app"
	snap.Status.VMUID = "uid-app"
	snap.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: clusteredNS, Name: "prov-cluster"}
	r := clusteredFixture(t, prov, vm, snap)
	// The clustered libvirt provider reports snapshot support.
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, providerCR))
	require.NotNil(t, providerCR.Status.ReportedCapabilities)
	providerCR.Status.ReportedCapabilities.SupportsSnapshots = true
	require.NoError(t, r.Update(context.Background(), providerCR))

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.revertRefs, 1)
	assert.Equal(t, contracts.VMRef{ID: "app", HostID: "host-alpha",
		Owner: contracts.ObjectIdentity{UID: "uid-app", Namespace: clusteredNS, Name: "app"}}, prov.revertRefs[0])
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, getVM(t, r, "app").Status.SnapshotRevert.Phase)
}

// TestApplySnapshotRevertReadiness: Ready is never left True while the
// SnapshotReverted condition is False; another False reason is kept.
func TestApplySnapshotRevertReadiness(t *testing.T) {
	vm := baseVM("default")
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonReconcileSuccess, vmReadyMessage, 1)
	applySnapshotRevertReadiness(vm)
	assert.Equal(t, metav1.ConditionTrue, revertReadyCondition(vm).Status, "no revert: unchanged")

	setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, "reverting")
	applySnapshotRevertReadiness(vm)
	assert.Equal(t, metav1.ConditionFalse, revertReadyCondition(vm).Status)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting, revertReadyCondition(vm).Reason)

	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, "describe failed", 1)
	applySnapshotRevertReadiness(vm)
	assert.Equal(t, k8s.ReasonProviderError, revertReadyCondition(vm).Reason, "another failure keeps its reason")

	setSnapshotRevertCondition(vm, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted, "done")
	setVMReady(vm)
	assert.Equal(t, metav1.ConditionTrue, revertReadyCondition(vm).Status)
}

// TestSnapshotRevertResendDue: the resend wait doubles per attempt and is
// capped.
func TestSnapshotRevertResendDue(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := &infravirtrigaudiov1beta1.SnapshotRevertStatus{AttemptTime: &metav1.Time{Time: at}, Attempts: 1}
	assert.Equal(t, snapshotRevertResendWait, snapshotRevertResendDue(rec, at))
	rec.Attempts = 2
	assert.Equal(t, 2*snapshotRevertResendWait, snapshotRevertResendDue(rec, at))
	rec.Attempts = 10
	assert.Equal(t, snapshotRevertResendMaxWait, snapshotRevertResendDue(rec, at))
	assert.Equal(t, time.Duration(0), snapshotRevertResendDue(rec, at.Add(snapshotRevertResendMaxWait)))
	assert.Equal(t, time.Duration(0), snapshotRevertResendDue(&infravirtrigaudiov1beta1.SnapshotRevertStatus{}, at))
}
