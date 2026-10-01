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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that every wait (hold) outcome of a VMClone is idempotent.
// The VMClone controller reconciles on every update of a VMClone, its own
// status writes included, so a hold that rewrote a changed status on each
// retry re-triggered itself at once: on a real clustered libvirt Provider a
// clone of a running source called Clone 325 times in 33 s (each call three
// SSH round trips) while it waited for SourceMustBePoweredOff, and the hold's
// backoff never governed. A repeated hold must leave the stored VMClone
// untouched (same resourceVersion, same status), so only its requeue — or a
// real event such as the source's power-off — re-drives it.

// reconcileLikeTheWatch reconciles clone as the manager does: once, then at
// once again for as long as the previous reconcile changed the stored VMClone
// (its own watch fires on every update, status included). It fails the test
// if that chain does not settle within maxRounds reconciles, and returns the
// last reconcile's result.
func reconcileLikeTheWatch(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone, maxRounds int) reconcile.Result {
	t.Helper()
	rv := getClone(t, r, clone).ResourceVersion
	for i := 0; i < maxRounds; i++ {
		res := reconcileClone(t, r, clone, 1)
		got := getClone(t, r, clone)
		if got.ResourceVersion == rv {
			return res
		}
		rv = got.ResourceVersion
	}
	require.FailNowf(t, "hot loop", "the VMClone's own writes kept re-triggering it: still changing after %d reconciles", maxRounds)
	return reconcile.Result{}
}

// requireIdempotentHold runs one more reconcile of a clone that waits — the
// retry its requeue brings — and asserts it wrote nothing: the stored
// VMClone's resourceVersion and status are unchanged. It returns the result.
func requireIdempotentHold(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone) reconcile.Result {
	t.Helper()
	before := getClone(t, r, clone)
	res := reconcileClone(t, r, clone, 1)
	after := getClone(t, r, clone)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a repeated hold writes nothing")
	assert.Equal(t, before.Status, after.Status, "a repeated hold leaves the stored status byte-identical")
	return res
}

// requireHeldNotCloning asserts the stored clone waits honestly: Pending with
// reason on its Ready condition, and nothing that says a clone was started.
func requireHeldNotCloning(t *testing.T, got *infrav1beta1.VMClone, reason string) {
	t.Helper()
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, reason, cloneReadyCondition(t, got).Reason)
	assert.False(t, k8s.IsConditionTrue(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning),
		"a held clone never shows Cloning=True")
	assert.Nil(t, got.Status.StartTime, "a held clone has not started")
	assert.Empty(t, got.Status.ActualCloneType)
	assert.Empty(t, got.Status.TargetVMID)
}

// requireClonedAndReady asserts the stored clone completed: Ready, with its
// start and completion times, its clone type, and Cloning=False/Completed.
func requireClonedAndReady(t *testing.T, got *infrav1beta1.VMClone, targetVMID string) {
	t.Helper()
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Equal(t, targetVMID, got.Status.TargetVMID)
	assert.Equal(t, infrav1beta1.CloneTypeFullClone, got.Status.ActualCloneType)
	require.NotNil(t, got.Status.StartTime, "the start of the accepted clone is recorded")
	require.NotNil(t, got.Status.CompletionTime)
	assert.False(t, got.Status.CompletionTime.Before(got.Status.StartTime))
	ready := cloneReadyCondition(t, got)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	cloning := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning)
	require.NotNil(t, cloning)
	assert.Equal(t, metav1.ConditionFalse, cloning.Status)
	assert.Equal(t, infrav1beta1.VMCloneReasonCompleted, cloning.Reason)
}

// TestVMClone_SingleHost_SourceRunningHoldIsIdempotent is the lab regression
// on a single-host provider: the refused Clone writes the hold once, a
// repeated refusal writes nothing and is re-checked with the blocked-VM
// backoff, and the clone proceeds once the source is off.
func TestVMClone_SingleHost_SourceRunningHoldIsIdempotent(t *testing.T) {
	s := cloneTestScheme(t)
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{cloneErr: sourceRunningAnswer()}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, runningProvider("default", "prov-1"),
		sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), clone)

	reconcileLikeTheWatch(t, r, clone, 5)
	assert.LessOrEqual(t, cp.cloneCnt, 2, "the hold settles at once: no Clone RPC per status write")
	requireWaitingForPowerOff(t, getClone(t, r, clone))
	requireHeldNotCloning(t, getClone(t, r, clone), cloneReasonSourceMustBePoweredOff)

	calls := cp.cloneCnt
	res := requireIdempotentHold(t, r, clone)
	assert.Equal(t, calls+1, cp.cloneCnt, "the backoff retry asks the provider again")
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the retry is paced by the blocked-VM backoff")
	assert.Equal(t, 1, powerOffWarnings(t, r), "one Warning event, on the transition only")

	// The source is powered off: the clone proceeds and binds.
	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-target"}
	reconcileLikeTheWatch(t, r, clone, 5)
	requireClonedAndReady(t, getClone(t, r, clone), "default.clone-target")
}

// TestVMClone_Clustered_SourceRunningHoldIsIdempotent is the lab regression
// itself (a clustered libvirt Provider): the same, with the target the clone
// created before its Clone RPC kept on its pending host while it waits.
func TestVMClone_Clustered_SourceRunningHoldIsIdempotent(t *testing.T) {
	cp := &clonerProvider{cloneErr: sourceRunningAnswer()}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)

	reconcileLikeTheWatch(t, r, clone, 6)
	assert.LessOrEqual(t, cp.cloneCnt, 2, "the hold settles at once: no Clone RPC per status write")
	requireWaitingForPowerOff(t, getClone(t, r, clone))
	requireHeldNotCloning(t, getClone(t, r, clone), cloneReasonSourceMustBePoweredOff)

	calls := cp.cloneCnt
	res := requireIdempotentHold(t, r, clone)
	assert.Equal(t, calls+1, cp.cloneCnt)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.Equal(t, "host-alpha", pendingHostOf(getTarget(t, r)), "the target keeps its pending host")

	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-c-target"}
	reconcileLikeTheWatch(t, r, clone, 5)
	requireClonedAndReady(t, getClone(t, r, clone), "default.clone-c-target")
	assert.Equal(t, "host-alpha", getTarget(t, r).Status.Placement.Host)
}

// TestVMClone_AsyncCloneAfterAHoldShowsItIsCloning: a clone that waited and
// is then accepted with a task reports that it is cloning, not its old wait,
// while the task runs (and is no longer a clone waiting on its source).
func TestVMClone_AsyncCloneAfterAHoldShowsItIsCloning(t *testing.T) {
	cp := &clonerProvider{cloneErr: sourceRunningAnswer()}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileLikeTheWatch(t, r, clone, 6)
	requireWaitingForPowerOff(t, getClone(t, r, clone))

	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-c-target", TaskRef: "task-1"}
	reconcileClone(t, r, clone, 1)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseCloning, got.Status.Phase)
	assert.NotNil(t, got.Status.StartTime)
	assert.True(t, k8s.IsConditionTrue(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning))
	assert.Equal(t, infrav1beta1.VMCloneReasonCloning, cloneReadyCondition(t, got).Reason, "no stale SourceMustBePoweredOff")
	assert.False(t, waitsForSourcePowerOff(got, "src-c"))
}

// TestVMClone_FailedTaskIsNotCloning: a clone whose task fails is Failed and
// no longer shows Cloning=True.
func TestVMClone_FailedTaskIsNotCloning(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target", TaskRef: "task-1"}}
	cp.IsTaskCompleteFn = func(context.Context, string) (bool, error) {
		return false, errors.New("clone task failed on the host")
	}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileClone(t, r, clone, 4)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.False(t, k8s.IsConditionTrue(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning),
		"a failed clone is not cloning")
}

// TestVMClone_Clustered_EveryHoldIsIdempotent: every wait of a clustered
// clone — a refused Clone RPC, the landing host, the target's pre-schedule
// check, its admission and the provider's capabilities — settles after the
// write that records it, and a repeated identical hold writes nothing and is
// re-checked at its own pace.
func TestVMClone_Clustered_EveryHoldIsIdempotent(t *testing.T) {
	notReady := readyCloneHost("host-alpha", "prov-c")
	notReady.Status.Health = infrav1beta1.HostHealthNotReady
	cordoned := readyCloneHost("host-alpha", "prov-c")
	cordoned.Spec.Schedulable = false
	excludedTarget := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c-target", Namespace: "default", UID: "uid-target",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"},
			ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}},
		Status: infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{ExcludedHosts: []string{"host-alpha"}}},
	}

	for name, tc := range map[string]struct {
		cloneErr error
		setup    func(cp *clonerProvider, src *infrav1beta1.VirtualMachine)
		extra    []client.Object
		reason   string
		// requeue is the expected retry of the repeated hold; zero means
		// "within the unschedulable backoff" (it doubles per check).
		requeue time.Duration
		// rpc: the hold is the Clone RPC's answer (re-sent on each retry).
		rpc bool
		// sentOnce: the hold is the Clone RPC's answer, which is never re-sent.
		sentOnce bool
	}{
		"source running": {cloneErr: sourceRunningAnswer(), reason: cloneReasonSourceMustBePoweredOff,
			requeue: blockedRetryMin, rpc: true},
		"previous incarnation": {cloneErr: previousIncarnationErr("default.clone-c-target"), reason: k8s.ReasonRestorePending,
			requeue: blockedRetryMin, rpc: true},
		"own domain on another host": {cloneErr: ownDomainElsewhereErr("default.clone-c-target"),
			reason: k8s.ReasonOwnDomainOnAnotherHost, requeue: blockedRetryMin, rpc: true},
		"host unavailable": {cloneErr: contracts.NewHostUnavailableError(`clone: host "host-alpha" is unreachable`, nil),
			reason: k8s.ReasonHostUnavailable, requeue: blockedRetryMin, rpc: true},
		"disk check failed": {cloneErr: contracts.NewRetryableError("clone: the disk check could not run on every host",
			fmt.Errorf("%w", contracts.ErrVMDiskCheckFailed)), reason: cloneReasonRetrying, requeue: blockedRetryMin, rpc: true},
		"copy in progress": {cloneErr: contracts.NewRetryableError(`clone: clone VM on host "host-alpha": the same copy is still running`, nil),
			reason: cloneReasonRetrying, requeue: blockedRetryMin, rpc: true},
		"name conflict": {cloneErr: contracts.NewConflictError(`clone: libvirt domain "default.clone-c-target" already exists`, nil),
			reason: cloneReasonSourceHostExcluded, requeue: cloneHostBlockedRetryInterval, sentOnce: true},
		"landing host not ready": {extra: []client.Object{notReady}, reason: cloneReasonSourceHostNotReady,
			requeue: cloneHostBlockedRetryInterval},
		"landing host cordoned": {extra: []client.Object{cordoned}, reason: cloneReasonSourceHostCordoned,
			requeue: cloneHostBlockedRetryInterval},
		"landing host gone": {extra: []client.Object{readyCloneHost("host-beta", "prov-c")}, reason: cloneReasonSourceHostGone,
			requeue: cloneHostBlockedRetryInterval},
		"landing host excluded for the target": {extra: []client.Object{readyCloneHost("host-alpha", "prov-c"), excludedTarget},
			reason: cloneReasonSourceHostExcluded, requeue: cloneHostBlockedRetryInterval},
		"pre-schedule: previous incarnation of the target": {
			setup: func(cp *clonerProvider, _ *infrav1beta1.VirtualMachine) {
				cp.ownerVMs = []contracts.VMInfo{
					stampedInfo("host-beta", "default.clone-c-target", "default", "clone-c-target", "uid-previous")}
			},
			reason: k8s.ReasonRestorePending, requeue: blockedRetryMin},
		"pre-schedule: provider lacks the owner filter": {
			setup:  func(cp *clonerProvider, _ *infrav1beta1.VirtualMachine) { cp.caps.SupportsListOwnerFilter = false },
			reason: k8s.ReasonProviderLacksListOwnerFilter, requeue: blockedRetryMin},
		"does not fit on its host": {
			setup: func(_ *clonerProvider, src *infrav1beta1.VirtualMachine) {
				cpu, mem := int32(4), int64(8192)
				src.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
			},
			extra:  []client.Object{smallCloneHost(6)},
			reason: k8s.ReasonUnschedulable},
		"size unknown": {
			setup: func(_ *clonerProvider, src *infrav1beta1.VirtualMachine) {
				src.Spec.ClassRef = infrav1beta1.ObjectRef{Name: "missing"}
			},
			reason: k8s.ReasonPlacementError, requeue: placementConfigRetryInterval},
		"capabilities unavailable": {
			setup: func(cp *clonerProvider, _ *infrav1beta1.VirtualMachine) {
				cp.capsErr = errors.New("rpc error: code = Unavailable")
			},
			reason: infrav1beta1.VMCloneReasonProviderError, requeue: 30 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneErr: tc.cloneErr}
			src := boundSource()
			r, clone := clusteredCloneFixture(t, src, cp, tc.extra...)
			// The fixture sets the provider's capabilities: adjust them (and
			// the source) after it, before the first reconcile.
			if tc.setup != nil {
				tc.setup(cp, src)
				require.NoError(t, r.Update(context.Background(), src.DeepCopy()))
				stored := &infrav1beta1.VirtualMachine{}
				require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(src), stored))
				stored.Status = src.Status
				require.NoError(t, r.Status().Update(context.Background(), stored))
			}

			reconcileLikeTheWatch(t, r, clone, 6)
			switch {
			case tc.rpc:
				assert.LessOrEqual(t, cp.cloneCnt, 2, "the hold settles at once: no Clone RPC per status write")
			case tc.sentOnce:
				assert.Equal(t, 1, cp.cloneCnt, "refused once, never re-sent")
			default:
				assert.Zero(t, cp.cloneCnt)
			}
			requireHeldNotCloning(t, getClone(t, r, clone), tc.reason)

			calls := cp.cloneCnt
			res := requireIdempotentHold(t, r, clone)
			if tc.rpc {
				assert.Equal(t, calls+1, cp.cloneCnt, "the retry asks the provider again")
			} else {
				assert.Equal(t, calls, cp.cloneCnt)
			}
			if tc.requeue != 0 {
				assert.Equal(t, tc.requeue, res.RequeueAfter)
			} else {
				assert.GreaterOrEqual(t, res.RequeueAfter, placementUnschedulableRetryInterval)
				assert.LessOrEqual(t, res.RequeueAfter, placementUnschedulableMaxRetryInterval)
			}
			// And again: still nothing written.
			requireIdempotentHold(t, r, clone)
		})
	}
}

// TestVMClone_SingleHost_PendingWaitsAreIdempotent: the single-host waits
// before any Clone RPC (a source not provisioned yet, a linked clone whose
// capability query fails) write once and then nothing.
func TestVMClone_SingleHost_PendingWaitsAreIdempotent(t *testing.T) {
	for name, tc := range map[string]struct {
		src    *infrav1beta1.VirtualMachine
		linked bool
		cp     *clonerProvider
		reason string
	}{
		"source not provisioned": {src: sourceVMWithID("default", "src-vm", "prov-1", ""), cp: &clonerProvider{},
			reason: infrav1beta1.VMCloneReasonCloning},
		"linked clone capabilities unavailable": {src: sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), linked: true,
			cp: &clonerProvider{capsErr: errors.New("rpc error: code = Unavailable")}, reason: cloneReasonLinkedUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			clone := &infrav1beta1.VMClone{
				ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
				Spec: infrav1beta1.VMCloneSpec{
					Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
					Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
				},
			}
			if tc.linked {
				clone.Spec.Options = &infrav1beta1.CloneOptions{Type: infrav1beta1.CloneTypeLinkedClone}
			}
			r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: tc.cp}, runningProvider("default", "prov-1"), tc.src, clone)
			reconcileLikeTheWatch(t, r, clone, 4)
			requireHeldNotCloning(t, getClone(t, r, clone), tc.reason)
			res := requireIdempotentHold(t, r, clone)
			assert.Equal(t, 30*time.Second, res.RequeueAfter)
			assert.Zero(t, tc.cp.cloneCnt)
		})
	}
}
