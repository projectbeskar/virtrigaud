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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
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
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning SnapshotDeleteFailed")

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

	// A different failure is a new hold: it is written, with a Warning.
	spy.answer(contracts.NewRetryableError("snapshotDelete: the disk check could not run", nil))
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.NotEqual(t, after.ResourceVersion, get().ResourceVersion)
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning SnapshotDeleteFailed")
	assert.Equal(t, infrav1beta1.SnapshotPhaseDeleting, get().Status.Phase)
}
