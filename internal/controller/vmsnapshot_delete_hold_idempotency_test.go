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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestVMSnapshot_FailedDeleteHoldIsIdempotent pins that a held snapshot
// delete writes its status once. A deleting VMSnapshot is reconciled on every
// update of it, and the hold's message used to embed the next retry delay —
// the time since the first failure, which changes on every retry between 15 s
// and 5 min — so each retry rewrote the status and re-triggered itself at once:
// a SnapshotDelete RPC per reconcile for up to five minutes. A retry of the same
// failure, however long it has failed, must leave the stored VMSnapshot
// untouched and be paced by the backoff alone.
func TestVMSnapshot_FailedDeleteHoldIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	spy.answer(contracts.NewHostUnavailableError(`snapshotDelete: host "host-alpha" is unreachable`, nil))

	res, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Equal(t, snapshotDeleteRetryMin, res.RequeueAfter)
	evs := strings.Join(eventsOf(events), "\n")
	assert.Contains(t, evs, "Warning SnapshotDeleteFailed")
	assert.Contains(t, evs, `host "host-alpha" is unreachable`, "the provider's answer is in the event")
	assert.NotContains(t, get().Status.Message, "unreachable", "and not in the status")

	// The retry the status write itself would trigger: nothing changes.
	held := get()
	res, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	assert.EqualValues(t, 2, spy.calls.Load())
	after := get()
	assert.Equal(t, held.ResourceVersion, after.ResourceVersion, "a repeated hold writes nothing")
	assert.Equal(t, held.Status, after.Status)
	assert.Equal(t, snapshotDeleteRetryMin, res.RequeueAfter)
	assert.Empty(t, eventsOf(events), "no Warning for an unchanged failure")

	// The delete has failed for two minutes: the retry waits about that long,
	// and still writes nothing.
	for i := range held.Status.Conditions {
		held.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	}
	require.NoError(t, r.Status().Update(ctx, held))
	held = get()
	res, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	assert.InDelta(t, (2 * time.Minute).Seconds(), res.RequeueAfter.Seconds(), 5)
	after = get()
	assert.Equal(t, held.ResourceVersion, after.ResourceVersion, "the retry delay is not in the status")
	assert.Equal(t, held.Status, after.Status)
	assert.Contains(t, after.Status.Message, snapshotDeleteRetryMax.String(), "the message names the backoff's cap")
	require.NotNil(t, after)
	assert.Contains(t, after.Finalizers, "snapshot.infra.virtrigaud.io/finalizer")

	assert.Equal(t, infrav1beta1.SnapshotPhaseDeleting, get().Status.Phase)
}

// TestVMSnapshot_FailedDeleteHoldIgnoresVaryingProviderText: the provider's
// answer is never in the status, so a failure whose text differs on every
// attempt (a single-host error carrying a connection's ephemeral port) is
// still the same hold: written once, with one Warning event that carries the
// first answer.
func TestVMSnapshot_FailedDeleteHoldIgnoresVaryingProviderText(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	answer := func(port int) error {
		return contracts.NewUnavailableError(fmt.Sprintf("snapshotDelete: read tcp 10.0.0.5:%d->10.0.0.9:22: connection reset by peer", port), nil)
	}

	spy.answer(answer(40001))
	_, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	held := get()
	evs := eventsOf(events)
	require.Len(t, evs, 2, "SnapshotDeleting, then one SnapshotDeleteFailed")
	assert.Contains(t, evs[1], "10.0.0.5:40001", "the provider's answer is in the event")
	for _, c := range held.Status.Conditions {
		assert.NotContains(t, c.Message, "connection reset", "the provider's answer is never in a condition (%s)", c.Type)
	}
	assert.Equal(t, snapshotDeleteFailedMessage, held.Status.Message)

	for _, port := range []int{40002, 40003, 40004} {
		spy.answer(answer(port))
		_, err := r.handleDeletion(ctx, get())
		require.NoError(t, err)
		after := get()
		assert.Equal(t, held.ResourceVersion, after.ResourceVersion, "a different provider text is the same hold: nothing is written")
		assert.Equal(t, held.Status, after.Status)
	}
	assert.Empty(t, eventsOf(events), "no further Warning for the same hold")
	assert.EqualValues(t, 4, spy.calls.Load())
}

// TestVMSnapshot_UnaddressableDeleteHoldIsIdempotent: a delete that cannot be
// addressed to the provider at all (here, the VM's spec.providerRef names
// another Provider than the one it is bound through) is held without a
// provider call, written once, and a retry of the same refusal — however
// long it has lasted — writes nothing.
func TestVMSnapshot_UnaddressableDeleteHoldIsIdempotent(t *testing.T) {
	ctx := context.Background()
	snap := pendingSnapshot()
	snap.Status.SnapshotID = "snap-1"
	snap.Status.Phase = infrav1beta1.SnapshotPhaseReady
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: snapPhaseNS, Name: "prov-old", UID: "uid-old"}
	r, rec := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"), snap)
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
	require.NoError(t, r.Delete(ctx, snap))
	get := func() *infrav1beta1.VMSnapshot {
		cur := &infrav1beta1.VMSnapshot{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), cur))
		return cur
	}

	res, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Equal(t, snapshotDeleteRetryMin, res.RequeueAfter)
	held := get()
	deleting := meta.FindStatusCondition(held.Status.Conditions, infrav1beta1.VMSnapshotConditionDeleting)
	require.NotNil(t, deleting)
	assert.Equal(t, metav1.ConditionFalse, deleting.Status)
	assert.Equal(t, k8s.ReasonProviderRefMismatch, deleting.Reason)
	assert.Contains(t, strings.Join(eventsOf(rec.Events), "\n"), "Warning SnapshotDeleteFailed")

	for _, age := range []time.Duration{0, 20 * time.Second, 2 * time.Minute} {
		if age > 0 {
			cur := get()
			for i := range cur.Status.Conditions {
				cur.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-age))
			}
			require.NoError(t, r.Status().Update(ctx, cur))
			held = get()
		}
		res, err := r.handleDeletion(ctx, held.DeepCopy())
		require.NoError(t, err)
		after := get()
		assert.Equal(t, held.ResourceVersion, after.ResourceVersion, "a repeated refusal writes nothing (held %s)", age)
		assert.Equal(t, held.Status, after.Status)
		assert.GreaterOrEqual(t, res.RequeueAfter, snapshotDeleteRetryMin)
		assert.LessOrEqual(t, res.RequeueAfter, snapshotDeleteRetryMax)
	}
	assert.Empty(t, eventsOf(rec.Events), "no further Warning for the same refusal")
	assert.Zero(t, spy.calls.Load(), "no provider call is made for a VM that cannot be addressed")
}
