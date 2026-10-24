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
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMSnapshot's durable SnapshotRequested marker (rc2 R6).

// statusWriteFaults selects which VMSnapshot status writes fail.
type statusWriteFaults struct {
	// all fails every status write; results fails every write that does not
	// carry the marker (the write of the create's outcome).
	all, results atomic.Bool
}

// markerSnapshotFixture is a VMSnapshot of vm on Provider prov-c (clustered
// when clustered), over a fake client whose status writes fail per faults.
func markerSnapshotFixture(t *testing.T, clustered bool, faults *statusWriteFaults) (*VMSnapshotReconciler, *snapshotRecorder, *infrav1beta1.VMSnapshot) {
	t.Helper()
	vm := boundSource()
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = infrav1beta1.ProviderTopologySingle
	if clustered {
		prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	} else {
		vm.Status.Placement = nil
	}
	snap := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-c", Namespace: "default", UID: "uid-snap-c",
			Finalizers: []string{infrav1beta1.VMSnapshotFinalizer}},
		Spec: infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: vm.Name}},
	}
	s := cloneTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, vm, snap).
		WithStatusSubresource(&infrav1beta1.VMSnapshot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sn, ok := obj.(*infrav1beta1.VMSnapshot); ok &&
					(faults.all.Load() || (faults.results.Load() && !snapshotRequested(sn))) {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	rec := &snapshotRecorder{}
	r := &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return rec, nil }
	return r, rec, snap
}

func reconcileSnapshotOnce(t *testing.T, r *VMSnapshotReconciler, snap *infrav1beta1.VMSnapshot) (ctrl.Result, *infrav1beta1.VMSnapshot) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.NoError(t, err)
	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	return res, got
}

// restartedSnapshotReconciler is r after a manager restart.
func restartedSnapshotReconciler(r *VMSnapshotReconciler) *VMSnapshotReconciler {
	return &VMSnapshotReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: record.NewFakeRecorder(20),
		providerInstanceFn: r.providerInstanceFn}
}

// TestVMSnapshotRequest_MarkerWriteFailureSendsNothing: no SnapshotCreate is
// sent unless SnapshotRequested was stored first.
func TestVMSnapshotRequest_MarkerWriteFailureSendsNothing(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.all.Store(true)
	r, rec, snap := markerSnapshotFixture(t, false, faults)
	res, got := reconcileSnapshotOnce(t, r, snap)
	assert.Empty(t, rec.creates)
	assert.Equal(t, snapshotStaleReadRetryInterval, res.RequeueAfter)
	assert.False(t, snapshotRequested(got))
}

// TestVMSnapshotRequest_RestartAfterCall_SingleHostIsHeld: a single-host
// create whose outcome was lost is never sent again (a second snapshot, or a
// refused name, on the VM): the VMSnapshot waits SnapshotOutcomeUnknown, with
// one Warning event naming the snapshot, until an operator confirms the
// attempt; then it is created.
func TestVMSnapshotRequest_RestartAfterCall_SingleHostIsHeld(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, false, faults)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, err, "the create's outcome could not be written")
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	events := record.NewFakeRecorder(20)
	r2.Recorder = events
	res, got := reconcileSnapshotOnce(t, r2, snap)
	assert.Len(t, rec.creates, 1, "not sent again")
	assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
	assert.Equal(t, infrav1beta1.SnapshotPhase(""), got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, snapshotReasonOutcomeUnknown, ready.Reason)
	assert.Equal(t, snapshotOutcomeUnknownMessage, ready.Message)
	ev := drainRecordedEvents(events)
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], "Warning "+snapshotReasonOutcomeUnknown)
	assert.Contains(t, ev[0], `"snap-c"`)

	rv := got.ResourceVersion
	_, got = reconcileSnapshotOnce(t, r2, snap)
	assert.Equal(t, rv, got.ResourceVersion, "a repeated hold writes nothing")

	got.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
		requestMarkerAttemptID(got.Status.Conditions, snapshotConditionRequested)}
	require.NoError(t, r2.Update(context.Background(), got))
	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2, "created on the operator's confirmation")
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
	assert.Equal(t, "snap-1", got.Status.SnapshotID)
	assert.False(t, snapshotRequested(got))
	assert.NotContains(t, got.Annotations, operationOutcomeAnnotation)
}

// TestVMSnapshotRequest_RestartAfterCall_RoutedIsResent: a routed create is
// sent again with the same request token (the provider adopts the snapshot the
// first call made), paced from the last request.
func TestVMSnapshotRequest_RestartAfterCall_RoutedIsResent(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, true, faults)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, err)
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	res, _ := reconcileSnapshotOnce(t, r2, snap)
	assert.Len(t, rec.creates, 1, "paced from the last request")
	assert.Positive(t, res.RequeueAfter)

	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r2.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotConditionRequested)
	require.NotNil(t, cond)
	cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	require.NoError(t, r2.Status().Update(context.Background(), got))

	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2)
	assert.Equal(t, rec.creates[0].RequestToken, rec.creates[1].RequestToken)
	assert.Equal(t, "host-alpha", rec.creates[1].VM.HostID)
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
	assert.False(t, snapshotRequested(got))
}

// TestVMSnapshotRequest_FailedCreateClearsTheMarker: a definite failure is
// the outcome: the marker is removed with it.
func TestVMSnapshotRequest_FailedCreateClearsTheMarker(t *testing.T) {
	r, rec, snap := markerSnapshotFixture(t, false, &statusWriteFaults{})
	rec.createErr = contracts.NewInvalidSpecError("bad name", nil)
	_, got := reconcileSnapshotOnce(t, r, snap)
	require.Len(t, rec.creates, 1)
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase)
	assert.False(t, snapshotRequested(got))
}

// presetSnapshotMarker stores a SnapshotRequested marker with resends
// automatic re-sends, requested age ago.
func presetSnapshotMarker(t *testing.T, r *VMSnapshotReconciler, snap *infrav1beta1.VMSnapshot, resends int, age time.Duration) {
	t.Helper()
	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	setRequestMarker(&got.Status.Conditions, snapshotConditionRequested, snapshotConditionRequested,
		withResendCount(snapshotRequestedMessage, resends), got.Generation, time.Now().Add(-age))
	require.NoError(t, r.Status().Update(context.Background(), got))
}

// TestVMSnapshotRequest_RoutedResendsAreCapped (review L3): a routed create
// re-sent snapshotRoutedResendMax times without a recorded outcome is held
// with the constant exhausted message instead of being re-sent forever.
func TestVMSnapshotRequest_RoutedResendsAreCapped(t *testing.T) {
	r, rec, snap := markerSnapshotFixture(t, true, &statusWriteFaults{})
	presetSnapshotMarker(t, r, snap, snapshotRoutedResendMax, time.Hour)
	_, got := reconcileSnapshotOnce(t, r, snap)
	assert.Empty(t, rec.creates)
	ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, snapshotReasonOutcomeUnknown, ready.Reason)
	assert.Equal(t, snapshotResendsExhaustedMessage, ready.Message)

	// Below the cap it is still re-sent, and counts itself.
	r, rec, snap = markerSnapshotFixture(t, true, &statusWriteFaults{})
	presetSnapshotMarker(t, r, snap, snapshotRoutedResendMax-2, time.Hour)
	rec.createErr = contracts.NewRetryableError("outcome unknown", nil)
	_, got = reconcileSnapshotOnce(t, r, snap)
	require.Len(t, rec.creates, 1)
	assert.Equal(t, snapshotRoutedResendMax-1, requestResends(requestMarker(got.Status.Conditions, snapshotConditionRequested)))
}

// routedNotPerformedAnswers are the transport's forms (mapGRPCError) of a
// routed SnapshotCreate the provider answered it did not perform.
func routedNotPerformedAnswers() map[string]error {
	rpc := errors.New("rpc error")
	return map[string]error{
		// The lab's V8 answer: the disk in-use check could not run.
		"VM_DISK_CHECK_FAILED": contracts.NewRetryableError(`create snapshot: failed to create snapshot: snapshot create of `+
			`libvirt domain "web" not performed: could not verify that no other domain uses its disks`,
			fmt.Errorf("%w: %w", contracts.ErrVMDiskCheckFailed, rpc)),
		"HOST_UNAVAILABLE": contracts.NewHostUnavailableError("create snapshot: host unavailable", rpc),
		"VM_OPERATION_FAILED not performed": contracts.NewRetryableError("create snapshot: not performed",
			fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed, rpc)),
		"request not sent": contracts.NewRetryableError("create snapshot: dial", fmt.Errorf("%w: %w", contracts.ErrRequestNotSent, rpc)),
	}
}

// TestVMSnapshotRequest_RoutedNotPerformedClosesTheAttempt (V8): a routed
// create the provider answers it did NOT perform is recorded as such — the
// attempt is closed (marker removed), the create is retried after a backoff
// as a new attempt — however often it is answered so, and is never held for
// an operator as an unknown outcome. The re-send carries the same request
// token.
func TestVMSnapshotRequest_RoutedNotPerformedClosesTheAttempt(t *testing.T) {
	for name, answer := range routedNotPerformedAnswers() {
		t.Run(name, func(t *testing.T) {
			r, rec, snap := markerSnapshotFixture(t, true, &statusWriteFaults{})
			// An earlier attempt one re-send short of the cap.
			presetSnapshotMarker(t, r, snap, snapshotRoutedResendMax-1, time.Hour)
			rec.createErr = answer
			for i := 1; i <= snapshotRoutedResendMax+2; i++ {
				expireNotPerformedBackoff(r, snap)
				res, got := reconcileSnapshotOnce(t, r, snap)
				require.Len(t, rec.creates, i, "sent again once its backoff has passed: a new attempt")
				assert.False(t, snapshotRequested(got), "the attempt is closed")
				assert.Empty(t, got.Status.Phase)
				assert.GreaterOrEqual(t, res.RequeueAfter, requestMarkerRecheckInterval, "retried after a backoff")
				ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
				require.NotNil(t, ready)
				assert.NotEqual(t, snapshotReasonOutcomeUnknown, ready.Reason, "never held for an operator")
				assert.Equal(t, snapshotCreateNotPerformedMessage, ready.Message)
			}
			for _, c := range rec.creates {
				assert.Equal(t, "uid-snap-c", c.RequestToken)
			}

			// Once the provider performs it, the snapshot is Ready.
			rec.createErr = nil
			expireNotPerformedBackoff(r, snap)
			_, got := reconcileSnapshotOnce(t, r, snap)
			assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
		})
	}
}

// expireNotPerformedBackoff ends the current wait of snap's not-performed
// backoff, keeping its count of consecutive answers: the next reconcile
// sends the create as if the wait had passed.
func expireNotPerformedBackoff(r *VMSnapshotReconciler, snap *infrav1beta1.VMSnapshot) {
	b := &r.notPerformedBackoff
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entries[string(snap.UID)]; ok {
		e.until = time.Time{}
	}
}

// TestVMSnapshotRequest_RoutedNotPerformedBacksOff (V8 security review): the
// re-sends of a routed create the provider keeps answering it did not perform
// back off exponentially (1 minute, doubling, capped at 10 minutes), and a
// reconcile triggered before the wait has passed sends nothing. Any other
// answer resets the backoff.
func TestVMSnapshotRequest_RoutedNotPerformedBacksOff(t *testing.T) {
	r, rec, snap := markerSnapshotFixture(t, true, &statusWriteFaults{})
	rec.createErr = routedNotPerformedAnswers()["VM_DISK_CHECK_FAILED"]
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		snapshotNotPerformedRetryMax, snapshotNotPerformedRetryMax}
	for i, w := range want {
		expireNotPerformedBackoff(r, snap)
		res, _ := reconcileSnapshotOnce(t, r, snap)
		require.Len(t, rec.creates, i+1)
		assert.Equal(t, w, res.RequeueAfter, "answer %d", i+1)

		// An event before the wait has passed sends nothing.
		res, _ = reconcileSnapshotOnce(t, r, snap)
		require.Len(t, rec.creates, i+1, "paced: not sent before its backoff passed")
		assert.Positive(t, res.RequeueAfter)
		assert.LessOrEqual(t, res.RequeueAfter, w)
	}

	// An unknown outcome resets the backoff and is handled as before (marker
	// kept, re-sent at the marker's one-minute pacing).
	rec.createErr = contracts.NewRetryableError("create snapshot: unavailable", nil)
	expireNotPerformedBackoff(r, snap)
	_, got := reconcileSnapshotOnce(t, r, snap)
	require.Len(t, rec.creates, len(want)+1)
	assert.True(t, snapshotRequested(got), "an unknown outcome keeps its marker")
	assert.Zero(t, r.notPerformedBackoff.remaining(string(snap.UID), time.Now()), "reset")

	// A not-performed answer after it starts again at one minute.
	rec.createErr = routedNotPerformedAnswers()["HOST_UNAVAILABLE"]
	// The marker's own pacing has passed: replace it with one an hour old.
	got.Status.Conditions = slices.DeleteFunc(got.Status.Conditions,
		func(c metav1.Condition) bool { return c.Type == snapshotConditionRequested })
	require.NoError(t, r.Status().Update(context.Background(), got))
	presetSnapshotMarker(t, r, snap, 1, time.Hour)
	res, _ := reconcileSnapshotOnce(t, r, snap)
	require.Len(t, rec.creates, len(want)+2)
	assert.Equal(t, time.Minute, res.RequeueAfter)
}

// TestVMSnapshotRequest_RoutedUnknownOutcomeKeepsTheMarker: an answer that
// leaves the outcome unknown (OUTCOME_UNKNOWN_RETRY: the host stopped
// answering after the snapshot command was sent; a bare Unavailable) keeps
// the marker and counts the re-send, so the cap still holds it for an
// operator.
func TestVMSnapshotRequest_RoutedUnknownOutcomeKeepsTheMarker(t *testing.T) {
	for name, answer := range map[string]error{
		"OUTCOME_UNKNOWN_RETRY": contracts.NewRetryableError("create snapshot: outcome unknown: retry",
			fmt.Errorf("%w: %w", contracts.ErrOutcomeUnknownRetry, contracts.ErrOperationNotPerformed)),
		"bare Unavailable": contracts.NewRetryableError("create snapshot: unavailable", nil),
	} {
		t.Run(name, func(t *testing.T) {
			r, rec, snap := markerSnapshotFixture(t, true, &statusWriteFaults{})
			presetSnapshotMarker(t, r, snap, snapshotRoutedResendMax-1, time.Hour)
			rec.createErr = answer
			_, got := reconcileSnapshotOnce(t, r, snap)
			require.Len(t, rec.creates, 1)
			require.True(t, snapshotRequested(got), "the outcome is unknown: the marker stays")
			assert.Equal(t, snapshotRoutedResendMax, requestResends(requestMarker(got.Status.Conditions, snapshotConditionRequested)))
			ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, snapshotCreateRetryingMessage, ready.Message)

			// The next reconcile (once paced) reaches the cap: held.
			presetSnapshotMarker(t, r, snap, snapshotRoutedResendMax, time.Hour)
			_, got = reconcileSnapshotOnce(t, r, snap)
			require.Len(t, rec.creates, 1, "not sent again")
			ready = meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, snapshotReasonOutcomeUnknown, ready.Reason)
		})
	}
}

// TestVMSnapshotRequest_UndecidableIsRetriedQuietly (review L7): a marker
// whose Provider cannot be read yet is re-checked with no hold and no event.
func TestVMSnapshotRequest_UndecidableIsRetriedQuietly(t *testing.T) {
	r, rec, snap := markerSnapshotFixture(t, false, &statusWriteFaults{})
	presetSnapshotMarker(t, r, snap, 0, time.Hour)
	prov := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "prov-c"}, prov))
	require.NoError(t, r.Delete(context.Background(), prov))
	events := record.NewFakeRecorder(10)
	r.Recorder = events

	before := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), before))
	res, got := reconcileSnapshotOnce(t, r, snap)
	assert.Empty(t, rec.creates)
	assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
	assert.Equal(t, before.ResourceVersion, got.ResourceVersion, "nothing is written")
	assert.Empty(t, drainRecordedEvents(events))
}
