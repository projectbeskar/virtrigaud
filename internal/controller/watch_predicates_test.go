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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the other half of the VMClone and VMSnapshot watch
// predicates: since neither controller is re-run by its own status writes any
// more, every step that must follow such a write asks for it with an explicit
// requeue (hold_watch_envtest_test.go runs the predicates in a real manager).

// TestSnapshotUpdateNeedsReconcile: only a generation change (a spec change,
// or the start of the deletion) or a change of the force-delete annotation
// re-runs a VMSnapshot.
func TestSnapshotUpdateNeedsReconcile(t *testing.T) {
	base := &infrav1beta1.VMSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", Generation: 1,
		Annotations: map[string]string{"example.com/note": "a"}}}
	mutate := func(f func(s *infrav1beta1.VMSnapshot)) *infrav1beta1.VMSnapshot {
		s := base.DeepCopy()
		f(s)
		return s
	}
	for name, tc := range map[string]struct {
		newSnap *infrav1beta1.VMSnapshot
		want    bool
	}{
		"spec change (generation)": {mutate(func(s *infrav1beta1.VMSnapshot) { s.Generation = 2 }), true},
		"deletion started (generation)": {mutate(func(s *infrav1beta1.VMSnapshot) {
			now := metav1.Now()
			s.DeletionTimestamp, s.Generation = &now, 2
		}), true},
		"force-delete set": {mutate(func(s *infrav1beta1.VMSnapshot) { s.Annotations[forceDeleteAnnotation] = "true" }), true},
		"force-delete removed": {base.DeepCopy(), true},
		"another annotation":   {mutate(func(s *infrav1beta1.VMSnapshot) { s.Annotations["example.com/note"] = "b" }), false},
		"a label":              {mutate(func(s *infrav1beta1.VMSnapshot) { s.Labels = map[string]string{"x": "y"} }), false},
		"status only":          {mutate(func(s *infrav1beta1.VMSnapshot) { s.Status.Message = "held" }), false},
	} {
		t.Run(name, func(t *testing.T) {
			oldSnap := base
			if name == "force-delete removed" {
				oldSnap = mutate(func(s *infrav1beta1.VMSnapshot) { s.Annotations[forceDeleteAnnotation] = "true" })
			}
			assert.Equal(t, tc.want, snapshotUpdateNeedsReconcile(oldSnap, tc.newSnap))
			assert.Equal(t, tc.want, snapshotUpdatePredicate().Update(event.UpdateEvent{ObjectOld: oldSnap, ObjectNew: tc.newSnap}))
		})
	}
	p := snapshotUpdatePredicate()
	assert.True(t, p.Create(event.CreateEvent{Object: base}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}))
}

// TestVMClone_FailureRequeuesForTheTargetCleanup: the reconcile that fails a
// clustered clone requeues it, so the failed clone removes the target
// VirtualMachine it created without waiting for an event; that next reconcile
// records the decision and stops.
func TestVMClone_FailureRequeuesForTheTargetCleanup(t *testing.T) {
	cp := &clonerProvider{cloneErr: errors.New(`clone: failed to clone VM on host "host-alpha"`)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	key := client.ObjectKeyFromObject(clone)

	var res ctrl.Result
	for i := 0; i < 4 && getClone(t, r, clone).Status.Phase != infrav1beta1.ClonePhaseFailed; i++ {
		var err error
		res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	require.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase)
	assert.True(t, res.Requeue, "the failing reconcile asks for the one that removes the target")
	assert.False(t, targetGone(t, r), "not removed in the failing reconcile itself")

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.True(t, targetGone(t, r), "the requeued reconcile removes the target the failed clone created")
	assert.Equal(t, ctrl.Result{}, res, "and the clone then rests")
}

// TestVMSnapshot_ReadyRequeuesForItsRetentionCheck: a snapshot created
// synchronously, or whose create task completes, is requeued once, so the
// next reconcile schedules its retention check (handleRetention) without an
// event.
func TestVMSnapshot_ReadyRequeuesForItsRetentionCheck(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Namespace: snapPhaseNS, Name: snapPhaseName}

	t.Run("synchronous create", func(t *testing.T) {
		r, _ := newSnapPhaseReconciler(t, sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100"), runningProvider(snapPhaseNS, "prov"),
			pendingSnapshot())
		spy := &snapshotCreateSpy{}
		r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		snap := &infrav1beta1.VMSnapshot{}
		require.NoError(t, r.Get(ctx, key, snap))
		require.Equal(t, infrav1beta1.SnapshotPhaseReady, snap.Status.Phase)
		assert.True(t, res.Requeue)

		res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: snapshotRetentionCheckInterval}, res, "the retention check is scheduled")
		assert.EqualValues(t, 1, spy.creates.Load())
	})

	t.Run("create task completes", func(t *testing.T) {
		snap := pendingSnapshot()
		snap.Status.Phase = infrav1beta1.SnapshotPhaseCreating
		snap.Status.TaskRef = "task-1"
		snap.Status.SnapshotID = "snap-1"
		r, _ := newSnapPhaseReconciler(t, sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100"), runningProvider(snapPhaseNS, "prov"), snap)
		r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			return &taskStatusProvider{status: contracts.TaskStatus{IsCompleted: true}}, nil
		}
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		got := &infrav1beta1.VMSnapshot{}
		require.NoError(t, r.Get(ctx, key, got))
		require.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
		assert.True(t, res.Requeue)
	})
}

// taskStatusProvider answers TaskStatus with status.
type taskStatusProvider struct {
	stubProvider
	status contracts.TaskStatus
}

// TaskStatus returns the scripted status.
func (p *taskStatusProvider) TaskStatus(_ context.Context, _ string) (contracts.TaskStatus, error) {
	return p.status, nil
}
