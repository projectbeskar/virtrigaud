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
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// leakyResolverDetail is resolver-style text (a TLS Secret, a Service
// ClusterIP:port, a runtime phase) that must reach the manager log only.
const leakyResolverDetail = "provider runtime phase Pending; TLS secret virtrigaud-system/provider-tls; dial 10.96.12.7:9443"

// scriptedSnapshotDeleteProvider is a provider whose SnapshotDelete answers
// with err (nil: a synchronous success) and counts its calls.
type scriptedSnapshotDeleteProvider struct {
	stubProvider
	err   error
	calls atomic.Int32
}

// SnapshotDelete records the call and answers with p.err.
func (p *scriptedSnapshotDeleteProvider) SnapshotDelete(_ context.Context, _ contracts.VMRef, _ string) (string, error) {
	p.calls.Add(1)
	return "", p.err
}

// snapshotHoldHarness is a VMMigrationReconciler over a fake client whose
// provider resolution returns prov, or resolveErr when that is set.
type snapshotHoldHarness struct {
	r          *VMMigrationReconciler
	c          client.Client
	rec        *record.FakeRecorder
	prov       *scriptedSnapshotDeleteProvider
	resolveErr error
}

func newSnapshotHoldHarness(t *testing.T, prov *scriptedSnapshotDeleteProvider, objs ...client.Object) *snapshotHoldHarness {
	t.Helper()
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		Build()
	h := &snapshotHoldHarness{c: c, rec: record.NewFakeRecorder(50), prov: prov}
	h.r = &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: h.rec,
		providerInstanceFn: func(_ context.Context, _ *infrav1beta1.Provider) (contracts.Provider, error) {
			if h.resolveErr != nil {
				return nil, h.resolveErr
			}
			return prov, nil
		},
	}
	return h
}

func (h *snapshotHoldHarness) reconcile(t *testing.T, m *infrav1beta1.VMMigration) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	require.NoError(t, err)
	return res
}

// get returns the stored migration, or nil once it is gone.
func (h *snapshotHoldHarness) get(t *testing.T, m *infrav1beta1.VMMigration) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	err := h.c.Get(context.Background(), client.ObjectKeyFromObject(m), got)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return got
}

// drainRecordedEvents returns every event recorded so far.
func drainRecordedEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// fakeRecorderOf returns rec as the *record.FakeRecorder the test built it as.
func fakeRecorderOf(t *testing.T, rec record.EventRecorder) *record.FakeRecorder {
	t.Helper()
	fr, ok := rec.(*record.FakeRecorder)
	require.True(t, ok, "the reconciler's Recorder is not a FakeRecorder")
	return fr
}

// terminalFailedMigration is snapshotFixture's migration, finalized and in a
// terminal Failed state (no retry policy).
func terminalFailedMigration() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	vm, p, m := snapshotFixture()
	m.Finalizers = []string{vmMigrationFinalizer}
	m.Status.Phase = infrav1beta1.MigrationPhaseFailed
	m.Status.Message = "export failed"
	return vm, p, m
}

// deletingMigration is snapshotFixture's migration being deleted, in the
// Exporting phase (a migration deleted mid-flight).
func deletingMigration() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	vm, p, m := snapshotFixture()
	m.Finalizers = []string{vmMigrationFinalizer}
	now := metav1.Now()
	m.DeletionTimestamp = &now
	m.Status.Phase = infrav1beta1.MigrationPhaseExporting
	return vm, p, m
}

// TestReconcile_TerminalFailureDeletesSourceSnapshot drives the real
// Reconcile (not handleFailedPhase) for a terminally failed migration — no
// retry policy, and retries used up — and proves the source snapshot cleanup
// runs and its result is persisted. On main the terminal short-circuit
// returned before any cleanup: no SnapshotDelete was ever sent.
func TestReconcile_TerminalFailureDeletesSourceSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *infrav1beta1.VMMigration)
	}{
		{"no retry policy", func(*infrav1beta1.VMMigration) {}},
		{"retries used up", func(m *infrav1beta1.VMMigration) {
			maxRetries := int32(2)
			m.Spec.Options = &infrav1beta1.MigrationOptions{RetryPolicy: &infrav1beta1.MigrationRetryPolicy{MaxRetries: &maxRetries}}
			m.Status.RetryCount = 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, p, m := terminalFailedMigration()
			tc.setup(m)
			prov := &scriptedSnapshotDeleteProvider{}
			h := newSnapshotHoldHarness(t, prov, vm, p, m)

			res := h.reconcile(t, m)
			assert.Equal(t, ctrl.Result{}, res)
			assert.EqualValues(t, 1, prov.calls.Load(), "the terminal failure must delete the source snapshot")

			got := h.get(t, m)
			assert.Empty(t, got.Status.SnapshotID, "the deleted snapshot is no longer recorded")
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Equal(t, "export failed", got.Status.Message, "the failure message is unchanged")

			// Nothing left to do: the next reconcile makes no provider call.
			h.reconcile(t, got)
			assert.EqualValues(t, 1, prov.calls.Load())
		})
	}
}

// TestReconcile_TerminalFailureHoldsFailedSnapshotCleanup proves a terminal
// failure whose snapshot delete fails records a constant hold (no cause text),
// retries with the blocked-VM backoff, and does not re-send the delete or
// re-write the status on the reconcile its own status write triggers.
func TestReconcile_TerminalFailureHoldsFailedSnapshotCleanup(t *testing.T) {
	vm, p, m := terminalFailedMigration()
	prov := &scriptedSnapshotDeleteProvider{err: contracts.NewRetryableError("snapshotDelete: host busy 10.0.0.9:16509", nil)}
	h := newSnapshotHoldHarness(t, prov, vm, p, m)

	res := h.reconcile(t, m)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.EqualValues(t, 1, prov.calls.Load())

	got := h.get(t, m)
	assert.Equal(t, "snap-abc", got.Status.SnapshotID, "a failed delete keeps the snapshot recorded")
	c := meta.FindStatusCondition(got.Status.Conditions, migrationConditionSnapshotCleanup)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, migrationReasonSnapshotCleanupPending, c.Reason)
	assert.Equal(t, migrationSnapshotCleanupPendingMessage, c.Message)
	assert.NotContains(t, c.Message, "10.0.0.9")
	events := drainRecordedEvents(h.rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], migrationReasonSnapshotCleanupPending)
	assert.NotContains(t, events[0], "10.0.0.9")

	// The reconcile the hold's own status write triggers: not due yet, so no
	// delete and no write.
	rv := got.ResourceVersion
	res = h.reconcile(t, got)
	assert.Greater(t, res.RequeueAfter, time.Duration(0))
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMin)
	assert.EqualValues(t, 1, prov.calls.Load(), "the hold is not retried ahead of its backoff")
	assert.Equal(t, rv, h.get(t, m).ResourceVersion)
	assert.Empty(t, drainRecordedEvents(h.rec))
}

// TestReconcile_TerminalFailureSnapshotHoldBacksOff proves a retry of a hold
// that began two minutes ago is paced by blockedRetryBackoff (the elapsed hold
// time), writes nothing, and records no new event.
func TestReconcile_TerminalFailureSnapshotHoldBacksOff(t *testing.T) {
	vm, p, m := terminalFailedMigration()
	m.Status.Conditions = []metav1.Condition{{
		Type: migrationConditionSnapshotCleanup, Status: metav1.ConditionFalse,
		Reason: migrationReasonSnapshotCleanupPending, Message: migrationSnapshotCleanupPendingMessage,
		ObservedGeneration: m.Generation,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
	}}
	prov := &scriptedSnapshotDeleteProvider{err: contracts.NewRetryableError("snapshotDelete: busy", nil)}
	h := newSnapshotHoldHarness(t, prov, vm, p, m)
	rv := h.get(t, m).ResourceVersion

	res := h.reconcile(t, m)
	assert.EqualValues(t, 1, prov.calls.Load(), "a due hold is retried")
	assert.InDelta(t, (2 * time.Minute).Seconds(), res.RequeueAfter.Seconds(), 5)
	assert.Equal(t, rv, h.get(t, m).ResourceVersion, "a repeated hold writes nothing")
	assert.Empty(t, drainRecordedEvents(h.rec))

	// Recovery: the next due retry deletes the snapshot and clears the hold.
	prov.err = nil
	h.reconcile(t, h.get(t, m))
	got := h.get(t, m)
	assert.Empty(t, got.Status.SnapshotID)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, migrationConditionSnapshotCleanup))
}

// TestReconcile_DeletionHoldsWhileSourceSnapshotMayRemain proves a VMMigration
// deletion keeps its finalizer while the source snapshot may still exist and
// its delete can still succeed — a failed provider delete, or a provider that
// cannot be resolved — with a constant condition and event that carry no
// provider or resolver text. On main the finalizer was released at once.
func TestReconcile_DeletionHoldsWhileSourceSnapshotMayRemain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deleteErr  error
		resolveErr error
		wantCalls  int32
	}{
		{"provider delete fails", contracts.NewRetryableError("snapshotDelete: "+leakyResolverDetail, nil), nil, 1},
		{"provider task fails", stderrors.New("snapshot delete task t-1 failed: device busy"), nil, 1},
		{"provider cannot be resolved", nil, stderrors.New(leakyResolverDetail), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, p, m := deletingMigration()
			prov := &scriptedSnapshotDeleteProvider{err: tc.deleteErr}
			h := newSnapshotHoldHarness(t, prov, vm, p, m)
			h.resolveErr = tc.resolveErr

			res := h.reconcile(t, m)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			assert.EqualValues(t, tc.wantCalls, prov.calls.Load())

			got := h.get(t, m)
			require.NotNil(t, got, "the finalizer must be kept")
			assert.Contains(t, got.Finalizers, vmMigrationFinalizer)
			assert.Equal(t, "snap-abc", got.Status.SnapshotID)
			c := meta.FindStatusCondition(got.Status.Conditions, migrationConditionSnapshotCleanup)
			require.NotNil(t, c)
			assert.Equal(t, migrationSnapshotCleanupPendingMessage, c.Message)
			for _, text := range append(drainRecordedEvents(h.rec), got.Status.Message, c.Message) {
				assert.NotContains(t, text, "10.96.12.7")
				assert.NotContains(t, text, "provider-tls")
				assert.NotContains(t, text, "device busy")
			}

			// Recovery releases the finalizer.
			prov.err, h.resolveErr = nil, nil
			c.LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute))
			meta.SetStatusCondition(&got.Status.Conditions, *c)
			require.NoError(t, h.c.Status().Update(context.Background(), got))
			h.reconcile(t, got)
			assert.Nil(t, h.get(t, m), "a successful delete releases the finalizer")
		})
	}
}

// TestReconcile_DeletionReleasesWhenSnapshotIsGone proves the deletion
// proceeds when nothing is left to delete: the source VM is gone, its Provider
// in the migration's own namespace is gone, or the provider answers that the
// snapshot is not found.
func TestReconcile_DeletionReleasesWhenSnapshotIsGone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropVM    bool
		dropProv  bool
		deleteErr error
		wantCalls int32
	}{
		{name: "source VM gone", dropVM: true},
		{name: "source Provider gone", dropProv: true},
		{name: "snapshot not found", deleteErr: contracts.NewNotFoundError("snapshotDelete: snapshot snap-abc not found", nil), wantCalls: 1},
		// The wire answers of the vSphere and Proxmox providers (H1), as the
		// transport maps a gRPC NotFound.
		{name: "vSphere: VM has no snapshots", wantCalls: 1, deleteErr: contracts.NewNotFoundError("snapshotDelete: VM has no snapshots",
			status.Error(codes.NotFound, "VM has no snapshots"))},
		{name: "Proxmox: snapshot not found", wantCalls: 1, deleteErr: contracts.NewNotFoundError(`snapshotDelete: snapshot "snap-abc" not found`,
			status.Error(codes.NotFound, `snapshot "snap-abc" not found`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, p, m := deletingMigration()
			objs := []client.Object{m}
			if !tc.dropVM {
				objs = append(objs, vm)
			}
			if !tc.dropProv {
				objs = append(objs, p)
			}
			prov := &scriptedSnapshotDeleteProvider{err: tc.deleteErr}
			h := newSnapshotHoldHarness(t, prov, objs...)

			res := h.reconcile(t, m)
			assert.Equal(t, ctrl.Result{}, res)
			assert.EqualValues(t, tc.wantCalls, prov.calls.Load())
			assert.Nil(t, h.get(t, m), "the finalizer is released")
		})
	}
}

// TestReconcile_DeletionHoldsForMissingCrossNamespaceProvider proves a
// cross-namespace source Provider that does not exist is refused like an
// ungranted one and keeps the cleanup pending (no existence oracle), instead
// of being read as "Provider gone".
func TestReconcile_DeletionHoldsForMissingCrossNamespaceProvider(t *testing.T) {
	vm, _, m := deletingMigration()
	vm.Spec.ProviderRef = infrav1beta1.ObjectRef{Name: "shared", Namespace: "infra"}
	prov := &scriptedSnapshotDeleteProvider{}
	h := newSnapshotHoldHarness(t, prov, vm, m)

	res := h.reconcile(t, m)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.Zero(t, prov.calls.Load())
	require.NotNil(t, h.get(t, m))
}

// TestReconcile_DeletionForceDeleteLeavesSourceSnapshot proves the escape
// hatch: virtrigaud.io/force-delete=true releases the finalizer although the
// snapshot could not be deleted, with a SnapshotLeftOnHypervisor Warning that
// names the snapshot — whether it is set before the first attempt or while the
// hold waits for its backoff (no further provider call then).
func TestReconcile_DeletionForceDeleteLeavesSourceSnapshot(t *testing.T) {
	t.Run("set before the attempt", func(t *testing.T) {
		vm, p, m := deletingMigration()
		m.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		prov := &scriptedSnapshotDeleteProvider{err: contracts.NewRetryableError("snapshotDelete: busy", nil)}
		h := newSnapshotHoldHarness(t, prov, vm, p, m)

		h.reconcile(t, m)
		assert.EqualValues(t, 1, prov.calls.Load(), "force-delete still tries the delete once")
		assert.Nil(t, h.get(t, m))
		events := strings.Join(drainRecordedEvents(h.rec), "\n")
		assert.Contains(t, events, eventReasonSnapshotLeftOnHypervisor)
		assert.Contains(t, events, `"snap-abc"`)
	})

	t.Run("set while held", func(t *testing.T) {
		vm, p, m := deletingMigration()
		prov := &scriptedSnapshotDeleteProvider{err: contracts.NewRetryableError("snapshotDelete: busy", nil)}
		h := newSnapshotHoldHarness(t, prov, vm, p, m)
		h.reconcile(t, m)
		require.NotNil(t, h.get(t, m))
		drainRecordedEvents(h.rec)

		held := h.get(t, m)
		held.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		require.NoError(t, h.c.Update(context.Background(), held))
		h.reconcile(t, held)
		assert.EqualValues(t, 1, prov.calls.Load(), "a hold not yet due is released without another call")
		assert.Nil(t, h.get(t, m))
		assert.Contains(t, strings.Join(drainRecordedEvents(h.rec), "\n"), eventReasonSnapshotLeftOnHypervisor)
	})
}

// TestReconcile_DeletionSkipsSnapshotStepWhenNotOwned proves the deletion
// does not touch a snapshot the migration did not create (spec.source.snapshotRef)
// or may not remove (CleanupPolicy=Never): no provider call, finalizer released.
func TestReconcile_DeletionSkipsSnapshotStepWhenNotOwned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *infrav1beta1.VMMigration)
	}{
		{"user-supplied snapshot", func(m *infrav1beta1.VMMigration) {
			m.Spec.Source.SnapshotRef = &infrav1beta1.LocalObjectReference{Name: "snap-abc"}
		}},
		{"CleanupPolicy Never", func(m *infrav1beta1.VMMigration) {
			m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, p, m := deletingMigration()
			tc.setup(m)
			prov := &scriptedSnapshotDeleteProvider{err: stderrors.New("must not be called")}
			h := newSnapshotHoldHarness(t, prov, vm, p, m)

			h.reconcile(t, m)
			assert.Zero(t, prov.calls.Load())
			assert.Nil(t, h.get(t, m))
		})
	}
}
