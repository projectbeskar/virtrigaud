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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// v11Refusal is the manager-side form (transport/grpc mapGRPCError: an
// untyped error carrying the gRPC status) of the libvirt provider's refusal to
// delete a disk-only snapshot while a memory snapshot is stored in its overlay
// (V11).
func v11Refusal() error {
	msg := `failed to delete snapshot: delete refused, nothing changed: delete snapshot "mem" first; it is stored in a disk ` +
		`file that deleting snapshot "live" would merge away, and would be lost`
	return &untypedForTest{msg: "snapshotDelete failed: " + msg, cause: status.Error(codes.FailedPrecondition, msg)}
}

// TestVMSnapshot_DeleteRefusedByTheProviderKeepsFinalizer pins the manager
// side of V11: a SnapshotDelete the provider refuses with a definite
// FailedPrecondition (other than VM_DISK_IN_USE) keeps the VMSnapshot and its
// finalizer — the snapshot is still on the hypervisor — with
// Ready=False/DeleteBlocked carrying the provider's guidance, re-checks with
// the failed-delete backoff, writes and announces nothing new while the
// refusal is unchanged, and releases the finalizer once the delete succeeds.
func TestVMSnapshot_DeleteRefusedByTheProviderKeepsFinalizer(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	spy.answer(v11Refusal())

	res, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Equal(t, snapshotDeleteRetryMin, res.RequeueAfter, "no hot loop: the failed-delete backoff")
	snap := get()
	require.NotNil(t, snap, "the VMSnapshot (and the snapshot it tracks) must not be dropped")
	assert.Contains(t, snap.Finalizers, infrav1beta1.VMSnapshotFinalizer)
	for _, condType := range []string{infrav1beta1.VMSnapshotConditionReady, infrav1beta1.VMSnapshotConditionDeleting} {
		c := meta.FindStatusCondition(snap.Status.Conditions, condType)
		require.NotNil(t, c, condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonDeleteBlocked, c.Reason, condType)
		assert.Equal(t, snap.Generation, c.ObservedGeneration, condType)
		assert.Contains(t, c.Message, `delete refused, nothing changed: delete snapshot "mem" first`, "the guidance is shown")
		assert.Contains(t, c.Message, forceDeleteAnnotation, "the way out is named")
		assert.NotContains(t, c.Message, "rpc error", "the raw gRPC chain stays out of the condition")
		assert.Equal(t, c.Message, snap.Status.Message)
	}
	evs := strings.Join(eventsOf(events), "\n")
	assert.Contains(t, evs, "Warning SnapshotDeleteFailed")
	assert.NotContains(t, evs, "Normal SnapshotDeleted")

	// The same refusal again: nothing new is written or announced, and the
	// next re-check waits longer the longer the delete has been refused.
	snap = get()
	for i := range snap.Status.Conditions {
		if snap.Status.Conditions[i].Type == infrav1beta1.VMSnapshotConditionDeleting {
			snap.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
		}
	}
	res, err = r.handleDeletion(ctx, snap)
	require.NoError(t, err)
	assert.InDelta(t, (2 * time.Minute).Seconds(), res.RequeueAfter.Seconds(), 5)
	assert.Empty(t, eventsOf(events), "no new events while the refusal is unchanged")
	assert.EqualValues(t, 2, spy.calls.Load())

	// The memory snapshot was deleted first: the delete goes through.
	spy.answer(nil)
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Nil(t, get(), "deleted once the provider allows it")
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Normal SnapshotDeleted")
}

// TestVMSnapshot_DeleteRefusedByTheProviderWithForceDeleteReleases: the
// force-delete escape hatch still releases a refused delete, naming the
// snapshot left on the hypervisor.
func TestVMSnapshot_DeleteRefusedByTheProviderWithForceDeleteReleases(t *testing.T) {
	r, spy, get, events := deletingSnapshot(t, map[string]string{forceDeleteAnnotation: "true"})
	spy.answer(v11Refusal())
	_, err := r.handleDeletion(context.Background(), get())
	require.NoError(t, err)
	assert.Nil(t, get())
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning "+eventReasonSnapshotLeftOnHypervisor)
}

// TestIsSnapshotDeleteRefusal: only a FailedPrecondition other than
// VM_DISK_IN_USE is a refusal; a plain failure is not.
func TestIsSnapshotDeleteRefusal(t *testing.T) {
	assert.True(t, isSnapshotDeleteRefusal(v11Refusal()))
	assert.False(t, isSnapshotDeleteRefusal(diskInUseRefusal()))
	assert.False(t, isSnapshotDeleteRefusal(&untypedForTest{msg: "snapshotDelete failed: boom", cause: status.Error(codes.Unknown, "boom")}))
	assert.False(t, isSnapshotDeleteRefusal(nil))
}
