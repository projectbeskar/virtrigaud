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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a migration polls a task-backed source snapshot to
// completion before it exports, never exports a snapshot that was not
// confirmed complete, and never takes the snapshot from a stale read.

// snapshotTaskProvider answers SnapshotCreate with snapshot "snap-t" and task
// "task-snap" (or synchronously, when sync is set), TaskStatus with status /
// statusErr, and counts SnapshotCreate, TaskStatus and SnapshotDelete calls.
type snapshotTaskProvider struct {
	stubProvider
	sync      bool
	status    contracts.TaskStatus
	statusErr error
	creates   atomic.Int32
	polls     atomic.Int32
	deletes   atomic.Int32
}

func (p *snapshotTaskProvider) SnapshotCreate(_ context.Context, _ contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.creates.Add(1)
	if p.sync {
		return contracts.SnapshotCreateResponse{SnapshotId: "snap-t"}, nil
	}
	return contracts.SnapshotCreateResponse{SnapshotId: "snap-t", Task: &contracts.TaskRef{ID: "task-snap"}}, nil
}

func (p *snapshotTaskProvider) TaskStatus(_ context.Context, _ string) (contracts.TaskStatus, error) {
	p.polls.Add(1)
	return p.status, p.statusErr
}

func (p *snapshotTaskProvider) SnapshotDelete(_ context.Context, _ contracts.VMRef, _ string) (string, error) {
	p.deletes.Add(1)
	return "", nil
}

// snapshottingMigration returns snapshotFixture's objects with the migration
// finalized, in the Snapshotting phase, and no snapshot recorded yet.
func snapshottingMigration() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	vm, p, m := snapshotFixture()
	m.Finalizers = []string{infrav1beta1.VMMigrationFinalizer}
	m.Spec.Source.CreateSnapshot = true
	m.Status.Phase = infrav1beta1.MigrationPhaseSnapshotting
	m.Status.SnapshotID = ""
	return vm, p, m
}

// newSnapshotTaskReconciler is a VMMigrationReconciler over a fake client
// seeded with objs, resolving every Provider to prov.
func newSnapshotTaskReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) *VMMigrationReconciler {
	t.Helper()
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	return &VMMigrationReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil },
	}
}

func reconcileSnapshotStep(t *testing.T, r *VMMigrationReconciler, m *infrav1beta1.VMMigration) (ctrl.Result, *infrav1beta1.VMMigration) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	require.NoError(t, err)
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	return res, got
}

func snapshottingCondition(m *infrav1beta1.VMMigration) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting)
}

// TestMigrationSnapshot_TaskIsPolledBeforeTheExport is the data-integrity
// fix: the export never starts while the snapshot task runs. On main the
// reconcile after the create moved to Exporting without polling.
func TestMigrationSnapshot_TaskIsPolledBeforeTheExport(t *testing.T) {
	vm, p, m := snapshottingMigration()
	prov := &snapshotTaskProvider{status: contracts.TaskStatus{IsCompleted: false}}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.EqualValues(t, 1, prov.creates.Load())
	assert.Equal(t, "task-snap", got.Status.TaskRef)
	assert.Equal(t, infrav1beta1.MigrationPhaseSnapshotting, got.Status.Phase)
	assert.Equal(t, migrationSnapshotTaskPollInterval, res.RequeueAfter)

	// The task runs: polled, still Snapshotting, never Exporting.
	res, got = reconcileSnapshotStep(t, r, got)
	assert.EqualValues(t, 1, prov.polls.Load())
	assert.Equal(t, infrav1beta1.MigrationPhaseSnapshotting, got.Status.Phase, "never exported while the snapshot is taken")
	assert.Equal(t, "task-snap", got.Status.TaskRef)
	assert.Equal(t, migrationReasonSnapshotInProgress, snapshottingCondition(got).Reason)
	assert.Equal(t, migrationSnapshotTaskPollInterval, res.RequeueAfter)

	// A repeated poll writes nothing.
	rv := got.ResourceVersion
	_, got = reconcileSnapshotStep(t, r, got)
	assert.Equal(t, rv, got.ResourceVersion)

	// The task completes: Exporting, with the snapshot task reference cleared.
	prov.status = contracts.TaskStatus{IsCompleted: true}
	res, got = reconcileSnapshotStep(t, r, got)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Empty(t, got.Status.TaskRef, "the export's task reference must not be the snapshot's")
	assert.Equal(t, "snap-t", got.Status.SnapshotID)
	assert.Equal(t, migrationReasonSnapshotComplete, snapshottingCondition(got).Reason)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter, "never an immediate requeue after the write")
	assert.EqualValues(t, 1, prov.creates.Load(), "one snapshot")
}

func TestMigrationSnapshot_SyncCreateAdvances(t *testing.T) {
	vm, p, m := snapshottingMigration()
	prov := &snapshotTaskProvider{sync: true}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.True(t, migrationSnapshotConfirmed(got))
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	assert.Zero(t, prov.polls.Load())
}

func TestMigrationSnapshot_FailedOrUnknownTaskFailsTheMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		status    contracts.TaskStatus
		statusErr error
		want      string
	}{
		// Re-review L-d: the task's free-form text never reaches the status.
		"task failed": {status: contracts.TaskStatus{IsCompleted: true, Error: "datastore full\nat https://admin:pw@vc.example/sdk"},
			want: "Snapshot creation failed: " + migrationTaskFailedDetail},
		"task failed, not marked done": {status: contracts.TaskStatus{Error: "quiesce failed on /vmfs/volumes/ds1"},
			want: migrationTaskFailedDetail},
		"task unknown to the provider": {statusErr: contracts.NewNotFoundError("taskStatus: unknown task", nil),
			want: "cannot be confirmed"},
	} {
		t.Run(name, func(t *testing.T) {
			vm, p, m := snapshottingMigration()
			m.Status.SnapshotID = "snap-t"
			m.Status.TaskRef = "task-snap"
			prov := &snapshotTaskProvider{status: tc.status, statusErr: tc.statusErr}
			r := newSnapshotTaskReconciler(t, prov, vm, p, m)

			_, got := reconcileSnapshotStep(t, r, m)
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Contains(t, got.Status.Message, tc.want)
			assert.NotContains(t, got.Status.Message, "pw@")
			assert.NotContains(t, got.Status.Message, "datastore full")
			assert.NotContains(t, got.Status.Message, "/vmfs/")
			assert.Empty(t, got.Status.TaskRef)
			assert.Equal(t, "snap-t", got.Status.SnapshotID, "kept so the cleanup removes it")
			assert.Equal(t, migrationReasonSnapshotFailed, snapshottingCondition(got).Reason)
			assert.False(t, migrationSnapshotConfirmed(got))
		})
	}
}

func TestMigrationSnapshot_TransientPollErrorIsRetried(t *testing.T) {
	vm, p, m := snapshottingMigration()
	m.Status.SnapshotID = "snap-t"
	m.Status.TaskRef = "task-snap"
	prov := &snapshotTaskProvider{statusErr: contracts.NewRetryableError("taskStatus: unavailable", nil)}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseSnapshotting, got.Status.Phase)
	assert.Equal(t, "task-snap", got.Status.TaskRef)
	assert.Equal(t, migrationSnapshotTaskPollInterval, res.RequeueAfter)
}

// TestMigrationSnapshot_PollErrorsEndAtTheWaitLimit: a task whose polls keep
// failing is not polled for ever: once the task's wait limit has passed since
// the Snapshotting=True/SnapshotInProgress transition, the snapshot fails as if
// the task had not finished in time.
func TestMigrationSnapshot_PollErrorsEndAtTheWaitLimit(t *testing.T) {
	vm, p, m := snapshottingMigration()
	m.Status.SnapshotID = "snap-t"
	m.Status.TaskRef = "task-snap"
	started := metav1.NewTime(time.Now().Add(-migrationDefaultSnapshotWait - time.Minute))
	m.Status.Conditions = []metav1.Condition{{
		Type: infrav1beta1.VMMigrationConditionSnapshotting, Status: metav1.ConditionTrue,
		Reason: migrationReasonSnapshotInProgress, Message: migrationSnapshotInProgressMessage, LastTransitionTime: started,
	}}
	prov := &snapshotTaskProvider{statusErr: contracts.NewRetryableError("taskStatus: unavailable", nil)}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	_, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "did not finish within")
	assert.Empty(t, got.Status.TaskRef)
	assert.Equal(t, migrationReasonSnapshotFailed, snapshottingCondition(got).Reason)
}

// TestMigrationSnapshot_RetryDiscardsAnUnconfirmedSnapshot: a retried
// migration whose recorded snapshot was never confirmed complete removes it
// and takes a new one; a confirmed one is reused.
func TestMigrationSnapshot_RetryDiscardsAnUnconfirmedSnapshot(t *testing.T) {
	vm, p, m := snapshottingMigration()
	m.Status.SnapshotID = "snap-old"
	m.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMMigrationConditionSnapshotting,
		Status: metav1.ConditionFalse, Reason: migrationReasonSnapshotFailed, Message: "x", LastTransitionTime: metav1.Now()}}
	prov := &snapshotTaskProvider{sync: true}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	_, got := reconcileSnapshotStep(t, r, m)
	assert.EqualValues(t, 1, prov.deletes.Load(), "the unconfirmed snapshot is removed")
	assert.Zero(t, prov.creates.Load())
	assert.Empty(t, got.Status.SnapshotID)
	assert.Equal(t, infrav1beta1.MigrationPhaseSnapshotting, got.Status.Phase)

	_, got = reconcileSnapshotStep(t, r, got)
	assert.EqualValues(t, 1, prov.creates.Load(), "a new snapshot is taken")
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
}

func TestMigrationSnapshot_RetryReusesAConfirmedSnapshot(t *testing.T) {
	vm, p, m := snapshottingMigration()
	m.Status.SnapshotID = "snap-ok"
	m.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMMigrationConditionSnapshotting,
		Status: metav1.ConditionTrue, Reason: migrationReasonSnapshotComplete, Message: "x", LastTransitionTime: metav1.Now()}}
	prov := &snapshotTaskProvider{}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	_, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Equal(t, "snap-ok", got.Status.SnapshotID)
	assert.Zero(t, prov.creates.Load()+prov.deletes.Load())
}

// TestMigrationSnapshot_NeverCreatedFromAStaleRead: a reconcile that read a
// VMMigration older than the live one sends no SnapshotCreate.
func TestMigrationSnapshot_NeverCreatedFromAStaleRead(t *testing.T) {
	vm, p, m := snapshottingMigration()
	prov := &snapshotTaskProvider{sync: true}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	stale := &infrav1beta1.VMMigration{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(m), stale))
	live := stale.DeepCopy()
	live.Status.SnapshotID = "snap-made-by-an-earlier-reconcile"
	require.NoError(t, r.Status().Update(context.Background(), live))

	res, err := r.handleSnapshottingPhase(context.Background(), stale)
	require.NoError(t, err)
	assert.Zero(t, prov.creates.Load(), "no second snapshot from a stale read")
	assert.Equal(t, migrationStaleReadRetryInterval, res.RequeueAfter)

}
