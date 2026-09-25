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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMSnapshot side of the libvirt delete-safety fix: a
// SnapshotDelete the provider refuses because other VMs depend on the VM's disk
// (a Conflict, mapped from VM_DISK_IN_USE) keeps the VMSnapshot and its
// finalizer — the snapshot is still on the hypervisor — and says why, until
// the dependents are gone or force-delete is set. Any other SnapshotDelete
// failure keeps the historical best-effort behaviour.

// snapshotDeleteSpy is a stubProvider whose SnapshotDelete answers err (nil:
// success) and counts the calls.
type snapshotDeleteSpy struct {
	stubProvider
	mu    sync.Mutex
	err   error
	calls atomic.Int32
}

// SnapshotDelete records the call and answers the scripted error.
func (p *snapshotDeleteSpy) SnapshotDelete(_ context.Context, _ contracts.VMRef, _ string) (string, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return "", p.err
}

// answer scripts the next SnapshotDelete answers (nil: success).
func (p *snapshotDeleteSpy) answer(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// diskInUseRefusal is the manager-side form of a provider's VM_DISK_IN_USE
// refusal of a SnapshotDelete (transport/grpc mapGRPCError).
func diskInUseRefusal() error {
	return contracts.NewConflictError(`snapshotDelete: failed to delete snapshot: snapshot delete of libvirt domain "default.web" `+
		`refused: its disk is the backing file (or a disk) of 1 other domain(s) on this host, such as a linked clone of this VM; `+
		`delete the linked clones first`, errors.New("rpc error: code = FailedPrecondition"))
}

// deletingSnapshot sets up a Ready snapshot of the bound VM "web" (Provider
// "prov") marked for deletion, and a reconciler whose provider is spy.
func deletingSnapshot(t *testing.T, annotations map[string]string) (*VMSnapshotReconciler, *snapshotDeleteSpy, func() *infrav1beta1.VMSnapshot, chan string) {
	t.Helper()
	ctx := context.Background()
	snap := pendingSnapshot()
	snap.Annotations = annotations
	snap.Status.SnapshotID = "snap-1"
	snap.Status.Phase = infrav1beta1.SnapshotPhaseReady
	r, rec := newSnapPhaseReconciler(t, sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100"), runningProvider(snapPhaseNS, "prov"), snap)
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
	require.NoError(t, r.Delete(ctx, snap))
	get := func() *infrav1beta1.VMSnapshot {
		cur := &infrav1beta1.VMSnapshot{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(snap), cur); err != nil {
			require.True(t, apierrors.IsNotFound(err), "%v", err)
			return nil
		}
		return cur
	}
	return r, spy, get, rec.Events
}

// eventsOf drains and returns the recorded events.
func eventsOf(ch chan string) []string {
	var out []string
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestVMSnapshot_DeleteRefusedForDependentsKeepsFinalizer(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	spy.answer(diskInUseRefusal())

	res, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Equal(t, vmDeleteBlockedRetryInterval, res.RequeueAfter, "re-checked at the VM's blocked-delete cadence")
	assert.EqualValues(t, 1, spy.calls.Load())

	snap := get()
	require.NotNil(t, snap, "the VMSnapshot (and the snapshot it tracks) must not be dropped")
	assert.Contains(t, snap.Finalizers, "snapshot.infra.virtrigaud.io/finalizer")
	for _, condType := range []string{infrav1beta1.VMSnapshotConditionReady, infrav1beta1.VMSnapshotConditionDeleting} {
		c := meta.FindStatusCondition(snap.Status.Conditions, condType)
		require.NotNil(t, c, condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonDeleteBlocked, c.Reason, condType)
		assert.Equal(t, snap.Generation, c.ObservedGeneration, condType)
		assert.Contains(t, c.Message, "delete the linked clones first")
		assert.Contains(t, c.Message, forceDeleteAnnotation, "the way out is named")
		assert.NotContains(t, c.Message, "rpc error", "the raw gRPC chain stays out of the condition")
	}
	evs := strings.Join(eventsOf(events), "\n")
	assert.Contains(t, evs, "Warning DeleteBlocked")
	assert.NotContains(t, evs, "SnapshotDeleted")
	assert.NotContains(t, evs, "SnapshotDeleteFailed")

	// A re-check of the same refusal neither re-announces the deletion nor
	// repeats the Warning.
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.EqualValues(t, 2, spy.calls.Load())
	assert.Empty(t, eventsOf(events), "no new events while the refusal is unchanged")
	require.NotNil(t, get())

	// The clones are gone: the next re-check deletes the snapshot and the
	// finalizer is released.
	spy.answer(nil)
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Nil(t, get(), "deleted once the provider allows it")
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Normal SnapshotDeleted")
}

func TestVMSnapshot_DeleteRefusedWithForceDeleteReleasesFinalizer(t *testing.T) {
	r, spy, get, events := deletingSnapshot(t, map[string]string{forceDeleteAnnotation: "true"})
	spy.answer(diskInUseRefusal())

	_, err := r.handleDeletion(context.Background(), get())
	require.NoError(t, err)
	assert.Nil(t, get(), "force-delete removes the VMSnapshot; the snapshot stays on the hypervisor")
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning SnapshotDeleteFailed")
}

// TestVMSnapshot_OtherDeleteFailuresKeepBestEffortBehaviour pins that only the
// dependents refusal holds the finalizer: any other SnapshotDelete failure is
// still reported in a Warning event and the finalizer is released, as before.
func TestVMSnapshot_OtherDeleteFailuresKeepBestEffortBehaviour(t *testing.T) {
	for name, failure := range map[string]error{
		"plain error": errors.New("snapshotDelete failed: boom"),
		"not found":   contracts.NewNotFoundError("snapshotDelete: domain not found", nil),
		"retryable":   contracts.NewRetryableError("snapshotDelete: host unreachable", nil),
	} {
		t.Run(name, func(t *testing.T) {
			r, spy, get, events := deletingSnapshot(t, nil)
			spy.answer(failure)
			_, err := r.handleDeletion(context.Background(), get())
			require.NoError(t, err)
			assert.Nil(t, get(), "historical best-effort delete: the finalizer is released")
			assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning SnapshotDeleteFailed")
		})
	}
}
