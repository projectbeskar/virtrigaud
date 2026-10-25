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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin how the manager handles a SnapshotRevert the provider
// answered it did not perform (nothing was changed): neither an unknown
// outcome (never counted toward snapshotRevertMaxAttempts, never "may or may
// not have been reverted") nor a failure, but Pending (RevertNotPerformed) and
// sent again, as a new attempt, after a backoff.

// revertNotPerformedErr is a "not performed" answer as the transport maps it
// (codes.Unavailable with VM_OPERATION_FAILED: retryable, marked
// ErrOperationNotPerformed).
func revertNotPerformedErr() error {
	st := status.Error(codes.Unavailable, "snapshot revert not performed: a check or command on the libvirt host failed "+
		"before the revert ran; nothing was changed (details are in the provider log)")
	return contracts.NewRetryableError("snapshotRevert: "+status.Convert(st).Message(),
		fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed, st))
}

// revertOutcomeUnknownRetryErr is an "outcome unknown: retry" answer as the
// transport maps it.
func revertOutcomeUnknownRetryErr() error {
	st := status.Error(codes.Unavailable, "snapshot revert outcome unknown")
	return contracts.NewRetryableError("snapshotRevert: snapshot revert outcome unknown",
		fmt.Errorf("%w: %w", contracts.ErrOutcomeUnknownRetry, st))
}

func TestSnapshotRevertCallNotPerformed(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"not performed":                 {revertNotPerformedErr(), true},
		"host unavailable":              {contracts.NewHostUnavailableError("snapshotRevert: host unavailable", nil), true},
		"request not sent":              {contracts.NewRetryableError("snapshotRevert: dial", contracts.ErrRequestNotSent), true},
		"disk check failed":             {contracts.NewRetryableError("snapshotRevert: check", contracts.ErrVMDiskCheckFailed), true},
		"outcome unknown: retry":        {revertOutcomeUnknownRetryErr(), false},
		"deadline (unknown)":            {contracts.NewRetryableError("snapshotRevert: deadline", status.Error(codes.DeadlineExceeded, "x")), false},
		"a definite failure (terminal)": {fmt.Errorf("snapshotRevert failed: %w", status.Error(codes.Unknown, "failed")), false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, snapshotRevertCallNotPerformed(tc.err))
		})
	}
}

// TestSnapshotRevert_NotPerformedIsPendingAndResentAfterABackoff: each "not
// performed" answer puts the request back to Pending (RevertNotPerformed),
// keeps the attempt on record, and sends it again after a doubling backoff;
// however many there are, the request is never Failed for them and ends
// Succeeded when the provider performs it.
func TestSnapshotRevert_NotPerformedIsPendingAndResentAfterABackoff(t *testing.T) {
	r, prov, rec, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	answers := make([]revertAnswer, 0, snapshotRevertMaxAttempts+3)
	for range snapshotRevertMaxAttempts + 2 {
		answers = append(answers, revertAnswer{err: revertNotPerformedErr()})
	}
	prov.revertAnswers = append(answers, revertAnswer{})

	got := reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)
	sr := got.Status.SnapshotRevert
	require.NotNil(t, sr)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhasePending, sr.Phase)
	assert.Equal(t, int32(1), sr.Attempts, "the attempt that was not performed stays on record")
	require.NotNil(t, sr.AttemptTime)
	assert.Equal(t, revertSnapUID, sr.SnapshotUID)
	assert.Contains(t, sr.Message, "did not perform revert attempt 1")
	assert.Contains(t, sr.Message, "nothing was changed")
	assert.NotContains(t, sr.Message, "may or may not")
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonNotPerformed)
	assert.Len(t, revertEvents(rec, eventReasonSnapshotRevertNotPerformed), 1)

	// Not sent again before the backoff, whatever triggers the reconcile.
	*now = now.Add(snapshotRevertNotPerformedRetryBase - time.Second)
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonNotPerformed)

	// Sent again after it, as a new attempt; the backoff doubles up to its
	// cap, and the answers never exhaust snapshotRevertMaxAttempts.
	wait := snapshotRevertNotPerformedRetryBase
	for sent := 2; sent <= snapshotRevertMaxAttempts+3; sent++ {
		*now = now.Add(wait)
		got = reconcileStored(t, r)
		require.Len(t, prov.reverts, sent, "send %d", sent)
		if sent == snapshotRevertMaxAttempts+3 {
			break
		}
		assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhasePending, got.Status.SnapshotRevert.Phase, "send %d", sent)
		assert.Equal(t, int32(1), got.Status.SnapshotRevert.Attempts, "each send after 'not performed' is a new attempt")
		wait = min(2*wait, snapshotRevertNotPerformedRetryMax)
	}
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
	requireRevertCondition(t, got, metav1.ConditionTrue, infravirtrigaudiov1beta1.SnapshotRevertReasonReverted)
	assert.Empty(t, revertEvents(rec, eventReasonSnapshotRevertFailed), "never failed")
}

// TestSnapshotRevert_NotPerformedWaitsAfterARestart: the in-memory backoff is
// gone after a manager restart; the recorded attempt time still keeps the
// base wait.
func TestSnapshotRevert_NotPerformedWaitsAfterARestart(t *testing.T) {
	r, prov, _, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: revertNotPerformedErr()}, {}}
	_ = reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)

	r.revertNotPerformed = unschedulableBackoff{} // a restarted manager
	*now = now.Add(snapshotRevertNotPerformedRetryBase / 2)
	_ = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1, "the recorded attempt time keeps the base wait")
	*now = now.Add(snapshotRevertNotPerformedRetryBase / 2)
	got := reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
}

// TestSnapshotRevert_NotPerformedThenPoweredOn: a VM powered on while the
// revert waits after a "not performed" answer waits for the power-off again
// (nothing was reverted, so nothing is failed), and is reverted once off.
func TestSnapshotRevert_NotPerformedThenPoweredOn(t *testing.T) {
	r, prov, _, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: contracts.NewHostUnavailableError("snapshotRevert: host unavailable", nil)}, {}}
	_ = reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)

	prov.describe.PowerState = string(contracts.PowerStateOn)
	*now = now.Add(snapshotRevertNotPerformedRetryMax)
	got := reconcileStored(t, r)
	assert.Len(t, prov.reverts, 1)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhasePending, got.Status.SnapshotRevert.Phase)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonWaitingForPowerOff)

	prov.describe.PowerState = string(contracts.PowerStateOff)
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded, got.Status.SnapshotRevert.Phase)
}

// TestSnapshotRevert_OutcomeUnknownRetryIsStillUnknown: the provider's
// "outcome unknown: retry" answer (the revert may have run) keeps the record
// Reverting and counts toward snapshotRevertMaxAttempts, as before.
func TestSnapshotRevert_OutcomeUnknownRetryIsStillUnknown(t *testing.T) {
	r, prov, _, _ := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{{err: revertOutcomeUnknownRetryErr()}}
	got := reconcileStored(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase)
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting)
}

// TestSnapshotRevert_NotPerformedAnsweringAResendStaysUnknown: "not performed"
// answering the manager's own re-send of an attempt whose outcome is unknown
// proves only that the re-send did nothing. The outcome stays unknown: the
// record stays Reverting, the attempts keep counting, and a VM seen running
// before the next send ends the request Failed instead of waiting for a
// power-off.
func TestSnapshotRevert_NotPerformedAnsweringAResendStaysUnknown(t *testing.T) {
	r, prov, _, now := revertFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOff, nil,
		revertSnapshotOf(revertSnapName, revertSnapUID))
	prov.revertAnswers = []revertAnswer{
		{err: contracts.NewRetryableError("snapshotRevert: context deadline exceeded",
			status.Error(codes.DeadlineExceeded, "context deadline exceeded"))},
		{err: revertNotPerformedErr()},
	}
	got := reconcileStored(t, r)
	require.Len(t, prov.reverts, 1)
	require.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase)

	*now = now.Add(snapshotRevertResendMaxWait)
	got = reconcileStored(t, r)
	require.Len(t, prov.reverts, 2, "the re-send")
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseReverting, got.Status.SnapshotRevert.Phase,
		"the earlier attempt's outcome is still unknown")
	assert.Equal(t, int32(2), got.Status.SnapshotRevert.Attempts, "the attempts keep counting")
	requireRevertCondition(t, got, metav1.ConditionFalse, infravirtrigaudiov1beta1.SnapshotRevertReasonReverting)

	prov.describe.PowerState = string(contracts.PowerStateOn)
	*now = now.Add(snapshotRevertResendMaxWait)
	got = reconcileStored(t, r)
	assert.Len(t, prov.reverts, 2, "never sent again once the VM may have run")
	assert.Equal(t, infravirtrigaudiov1beta1.SnapshotRevertPhaseFailed, got.Status.SnapshotRevert.Phase)
	assert.Contains(t, got.Status.SnapshotRevert.Message, "may or may not have been reverted")
}
