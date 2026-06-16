/*
Copyright 2025.

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// countingSnapshotProvider embeds stubProvider (defined in
// virtualmachine_controller_test.go) and instruments the snapshot-delete +
// task-await path exercised by deleteSourceSnapshot. It counts SnapshotDelete
// invocations, counts IsTaskComplete polls until it reports "done", and returns
// a configurable TaskStatus so a task that completes WITH an error can be
// simulated.
type countingSnapshotProvider struct {
	stubProvider

	// snapshotTaskRef is returned from every SnapshotDelete call. An empty value
	// models a synchronous (no-task) delete.
	snapshotTaskRef string

	// pollsUntilDone is the number of IsTaskComplete calls that report "not yet"
	// before one reports "done". 0 means the very first poll reports done.
	pollsUntilDone int32

	// taskError, when non-empty, is surfaced via TaskStatus.Error after the task
	// reports complete, modeling a task that finished but failed.
	taskError string

	snapshotDeleteCalls atomic.Int32
	isTaskCompleteCalls atomic.Int32
	taskStatusCalls     atomic.Int32
}

// SnapshotDelete records the call and returns the configured task ref.
func (p *countingSnapshotProvider) SnapshotDelete(_ context.Context, _, _ string) (string, error) {
	p.snapshotDeleteCalls.Add(1)
	return p.snapshotTaskRef, nil
}

// IsTaskComplete reports "not done" for the first pollsUntilDone calls, then
// "done" for every call thereafter.
func (p *countingSnapshotProvider) IsTaskComplete(_ context.Context, _ string) (bool, error) {
	n := p.isTaskCompleteCalls.Add(1)
	return n > p.pollsUntilDone, nil
}

// TaskStatus records the call and returns the configured terminal status.
func (p *countingSnapshotProvider) TaskStatus(_ context.Context, _ string) (contracts.TaskStatus, error) {
	p.taskStatusCalls.Add(1)
	return contracts.TaskStatus{IsCompleted: true, Error: p.taskError}, nil
}

var _ contracts.Provider = (*countingSnapshotProvider)(nil)

// newSnapshotReconciler builds a VMMigrationReconciler whose provider resolution
// is pinned to the supplied fake, seeded with the supplied objects and status
// subresource support. Mirrors newRaceReconciler.
func newSnapshotReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) (*VMMigrationReconciler, client.Client) {
	t.Helper()
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()

	r := &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(_ context.Context, _ *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}
	return r, c
}

// snapshotFixture returns a source VM, a source Provider, and a VMMigration that
// has a migration-created snapshot recorded in status (SnapshotID set,
// SnapshotRef nil). The migration's phase/options are left to the caller.
func snapshotFixture() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	sourceVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "source-vm", Namespace: "default"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "source-provider"},
		},
	}
	sourceVM.Status.ID = "vm-123"

	sourceProvider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "source-provider", Namespace: "default"},
	}

	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "snap-migration",
			Namespace:  "default",
			UID:        "uid-snap-1",
			Generation: 1,
		},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{
				VMRef: infrav1beta1.LocalObjectReference{Name: "source-vm"},
			},
			Target: infrav1beta1.MigrationTarget{
				Name:        "target-vm",
				ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			},
		},
	}
	migration.Status.SnapshotID = "snap-abc"
	return sourceVM, sourceProvider, migration
}

// TestDeleteSourceSnapshot_AwaitsTaskAndClearsID proves the primary fix: when
// SnapshotDelete returns a task ref, deleteSourceSnapshot polls the task to
// completion (instead of fire-and-forget) and clears Status.SnapshotID as an
// idempotency latch on success.
func TestDeleteSourceSnapshot_AwaitsTaskAndClearsID(t *testing.T) {
	ctx := context.Background()
	prov := &countingSnapshotProvider{
		snapshotTaskRef: "task-del-1",
		pollsUntilDone:  2, // first two polls report "not done"
	}
	sourceVM, sourceProvider, migration := snapshotFixture()
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	err := r.deleteSourceSnapshot(ctx, migration)
	require.NoError(t, err)

	assert.EqualValues(t, 1, prov.snapshotDeleteCalls.Load(), "exactly one SnapshotDelete RPC")
	// The await must have polled past the not-done responses (proves it waited,
	// rather than returning immediately on the first poll).
	assert.GreaterOrEqual(t, prov.isTaskCompleteCalls.Load(), int32(3),
		"must poll IsTaskComplete until the task reports done")
	assert.EqualValues(t, 1, prov.taskStatusCalls.Load(), "must verify final TaskStatus")

	// Idempotency latch: a completed delete clears the recorded snapshot ID.
	assert.Equal(t, "", migration.Status.SnapshotID, "SnapshotID must be cleared after a successful delete")
}

// TestDeleteSourceSnapshot_SurfacesTaskError proves the await path checks the
// final TaskStatus and returns an error when the task completed but failed; the
// snapshot ID is NOT cleared because the snapshot may still exist.
func TestDeleteSourceSnapshot_SurfacesTaskError(t *testing.T) {
	ctx := context.Background()
	prov := &countingSnapshotProvider{
		snapshotTaskRef: "task-del-err",
		taskError:       "device or resource busy",
	}
	sourceVM, sourceProvider, migration := snapshotFixture()
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	err := r.deleteSourceSnapshot(ctx, migration)
	require.Error(t, err, "a task that completes with an error must surface as an error")
	assert.Contains(t, err.Error(), "device or resource busy")

	// The snapshot may still exist, so the latch must NOT clear the ID.
	assert.Equal(t, "snap-abc", migration.Status.SnapshotID, "SnapshotID must be retained on task failure")
}

// TestHandleReadyPhase_NeverSkipsSnapshotDeletion proves CleanupPolicy=Never is
// honored on the Ready path: the source snapshot is NOT deleted.
func TestHandleReadyPhase_NeverSkipsSnapshotDeletion(t *testing.T) {
	ctx := context.Background()
	prov := &countingSnapshotProvider{snapshotTaskRef: "task-x"}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Options = &infrav1beta1.MigrationOptions{
		CleanupPolicy: infrav1beta1.CleanupPolicyNever,
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseReady
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	_, err := r.handleReadyPhase(ctx, migration)
	require.NoError(t, err)

	assert.EqualValues(t, 0, prov.snapshotDeleteCalls.Load(),
		"CleanupPolicy=Never must NOT delete the source snapshot on Ready")
	assert.Equal(t, "snap-abc", migration.Status.SnapshotID, "SnapshotID must be retained under Never")
}

// TestHandleReadyPhase_OnSuccessDeletesSnapshot is the positive control for the
// Ready path: the default OnSuccess policy deletes the snapshot and clears the
// latch.
func TestHandleReadyPhase_OnSuccessDeletesSnapshot(t *testing.T) {
	ctx := context.Background()
	prov := &countingSnapshotProvider{snapshotTaskRef: "task-x"}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Options = &infrav1beta1.MigrationOptions{
		CleanupPolicy: infrav1beta1.CleanupPolicyOnSuccess,
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseReady
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	_, err := r.handleReadyPhase(ctx, migration)
	require.NoError(t, err)

	assert.EqualValues(t, 1, prov.snapshotDeleteCalls.Load(),
		"OnSuccess must delete the source snapshot on Ready")
	assert.Equal(t, "", migration.Status.SnapshotID, "SnapshotID must be cleared after deletion")
}

// TestHandleFailedPhase_TerminalCleansSnapshot proves the terminal-failure
// cleanup: a migration that fails with no retry policy (immediately terminal)
// deletes its source snapshot under OnSuccess/Always but NOT under Never.
func TestHandleFailedPhase_TerminalCleansSnapshot(t *testing.T) {
	cases := []struct {
		name        string
		policy      string
		wantDeletes int32
		wantID      string
	}{
		{"OnSuccess deletes on terminal failure", infrav1beta1.CleanupPolicyOnSuccess, 1, ""},
		{"Always deletes on terminal failure", infrav1beta1.CleanupPolicyAlways, 1, ""},
		{"Never retains on terminal failure", infrav1beta1.CleanupPolicyNever, 0, "snap-abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prov := &countingSnapshotProvider{snapshotTaskRef: "task-fail"}
			sourceVM, sourceProvider, migration := snapshotFixture()
			migration.Spec.Options = &infrav1beta1.MigrationOptions{
				CleanupPolicy: tc.policy,
				// No RetryPolicy => immediately terminal (the lab scenario).
			}
			migration.Status.Phase = infrav1beta1.MigrationPhaseFailed
			migration.Status.Message = "export failed"
			r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

			_, err := r.handleFailedPhase(ctx, migration)
			require.NoError(t, err)

			assert.EqualValues(t, tc.wantDeletes, prov.snapshotDeleteCalls.Load())
			assert.Equal(t, tc.wantID, migration.Status.SnapshotID)
		})
	}
}

// TestHandleFailedPhase_TerminalCleanupBestEffort proves a snapshot-cleanup
// failure on terminal failure does NOT wedge the migration: handleFailedPhase
// still returns without error.
func TestHandleFailedPhase_TerminalCleanupBestEffort(t *testing.T) {
	ctx := context.Background()
	prov := &countingSnapshotProvider{
		snapshotTaskRef: "task-fail",
		taskError:       "snapshot delete failed on host",
	}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Options = &infrav1beta1.MigrationOptions{
		CleanupPolicy: infrav1beta1.CleanupPolicyOnSuccess,
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseFailed
	migration.Status.Message = "export failed"
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	_, err := r.handleFailedPhase(ctx, migration)
	require.NoError(t, err, "a best-effort cleanup failure must not wedge a terminal migration")

	assert.EqualValues(t, 1, prov.snapshotDeleteCalls.Load(), "cleanup was attempted")
	// The delete failed, so the latch must retain the ID for a later retry/delete.
	assert.Equal(t, "snap-abc", migration.Status.SnapshotID)
}

// TestCleanupAllowed exercises the policy gate directly.
func TestCleanupAllowed(t *testing.T) {
	cases := []struct {
		name    string
		options *infrav1beta1.MigrationOptions
		want    bool
	}{
		{"nil options allows cleanup", nil, true},
		{"empty policy allows cleanup", &infrav1beta1.MigrationOptions{}, true},
		{"OnSuccess allows cleanup", &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyOnSuccess}, true},
		{"Always allows cleanup", &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyAlways}, true},
		{"Never blocks cleanup", &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{Options: tc.options}}
			assert.Equal(t, tc.want, cleanupAllowed(m))
		})
	}
}
