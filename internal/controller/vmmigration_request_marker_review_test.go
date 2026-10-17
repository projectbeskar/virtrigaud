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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the review fixes of the requested-call markers (rc2 R6,
// PR #402 review M1 and L1–L7).

// erringMigrationProvider answers SnapshotCreate, ExportDisk and ImportDisk
// with err, counts the calls, and reports caps.
type erringMigrationProvider struct {
	stubProvider
	err   error
	caps  contracts.Capabilities
	calls int
}

func (p *erringMigrationProvider) SnapshotCreate(context.Context, contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.calls++
	return contracts.SnapshotCreateResponse{}, p.err
}

func (p *erringMigrationProvider) ExportDisk(context.Context, contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.calls++
	return contracts.ExportDiskResponse{}, p.err
}

func (p *erringMigrationProvider) ImportDisk(context.Context, contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.calls++
	return contracts.ImportDiskResponse{}, p.err
}

func (p *erringMigrationProvider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	return p.caps, nil
}

// deadlineExceeded is the transport's form of a call that ran past its
// deadline: a bare retryable error, with no "nothing done" mark.
func deadlineExceeded() error {
	return contracts.NewRetryableError("export disk: context deadline exceeded",
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"))
}

// migrationFixtureFor returns the objects and migration of op's phase.
func migrationFixtureFor(op migrationLongOp) ([]client.Object, *infrav1beta1.VMMigration) {
	switch op {
	case longOpSnapshot:
		vm, p, m := snapshottingMigration()
		return []client.Object{vm, p, m}, m
	case longOpExport:
		vm, p, m := raceMigrationFixture()
		return []client.Object{vm, p, m}, m
	default:
		p, m := importRaceFixture()
		return []client.Object{p, m}, m
	}
}

// TestMigrationRequest_OutcomeOfAFailedCall (review M1): a call answered
// without a definite outcome — a deadline, a bare Unavailable after the
// request was written, an unclassified error — keeps its marker and is held
// (or waits for its settle time where the backend is re-send safe); it never
// fails the migration, so the retry policy never re-sends it while the
// earlier call may still be writing. A definite answer — nothing done, or a
// refusal — fails it as before and removes the marker.
func TestMigrationRequest_OutcomeOfAFailedCall(t *testing.T) {
	for _, op := range []migrationLongOp{longOpSnapshot, longOpExport, longOpImport} {
		for name, tc := range map[string]struct {
			err   error
			known bool
		}{
			"deadline exceeded":     {err: deadlineExceeded()},
			"transport closing":     {err: contracts.NewRetryableError("transport is closing", nil)},
			"unclassified":          {err: errors.New("error reading from server: EOF")},
			"request not sent":      {err: contracts.NewRetryableError("dial", fmt.Errorf("%w: dial", contracts.ErrRequestNotSent)), known: true},
			"host unavailable":      {err: contracts.NewHostUnavailableError("host down", nil), known: true},
			"invalid spec (refuse)": {err: contracts.NewInvalidSpecError("bad request", nil), known: true},
		} {
			t.Run(string(op)+"/"+name, func(t *testing.T) {
				objs, m := migrationFixtureFor(op)
				c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
					WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
				prov := &erringMigrationProvider{err: tc.err}
				r := migrationReconcilerOn(c, prov)
				res, err := migrationPhaseCall(r, op, storedMigration(t, c, m))
				require.NoError(t, err)
				require.Equal(t, 1, prov.calls)
				got := storedMigration(t, c, m)
				if tc.known {
					assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
					assert.False(t, migrationRequested(got, op), "a definite answer removes the marker")
					return
				}
				assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase, "never failed: the call may still run")
				assert.True(t, migrationRequested(got, op), "the marker is kept")
				ready := migrationReady(got)
				require.NotNil(t, ready)
				assert.Equal(t, migrationRequests[op].unknownReason, ready.Reason)
				assert.Equal(t, migrationRequests[op].operatorMessage, ready.Message)
				assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)

				// Not sent again by the next reconcile either.
				_, err = migrationPhaseCall(restarted(r), op, storedMigration(t, c, m))
				require.NoError(t, err)
				assert.Equal(t, 1, prov.calls)
			})
		}
	}

	t.Run("export on a re-send-safe backend waits for the settle time", func(t *testing.T) {
		objs, m := migrationFixtureFor(longOpExport)
		c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		prov := &erringMigrationProvider{err: deadlineExceeded(),
			caps: contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}}}
		r := migrationReconcilerOn(c, prov)
		res, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
		require.NoError(t, err)
		assert.Greater(t, res.RequeueAfter, migrationResendSettleTime-time.Minute)
		got := storedMigration(t, c, m)
		assert.Equal(t, migrationRequests[longOpExport].resendMessage, migrationReady(got).Message)
		assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	})
}

// TestMarkMigrationRequested_AttemptIDsIncreaseAndResendsCount (review L1,
// L3): every marker write is a strictly later attempt — within one second
// too — and an automatic re-send counts itself; an operator's confirmation
// starts the count again.
func TestMarkMigrationRequested_AttemptIDsIncreaseAndResendsCount(t *testing.T) {
	objs, m := migrationFixtureFor(longOpExport)
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := migrationReconcilerOn(c, &erringMigrationProvider{})
	ctx := context.Background()

	cur := storedMigration(t, c, m)
	require.NoError(t, r.markMigrationRequested(ctx, cur, longOpExport))
	first := requestMarker(cur.Status.Conditions, migrationConditionExportRequested)
	require.NotNil(t, first)
	assert.Zero(t, requestResends(first))
	firstAt := first.LastTransitionTime

	require.NoError(t, r.markMigrationRequested(ctx, cur, longOpExport))
	second := requestMarker(storedMigration(t, c, m).Status.Conditions, migrationConditionExportRequested)
	assert.True(t, second.LastTransitionTime.After(firstAt.Time), "a strictly later attempt id")
	assert.Equal(t, 1, requestResends(second))

	cur = storedMigration(t, c, m)
	cur.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
		requestMarkerAttemptID(cur.Status.Conditions, migrationConditionExportRequested)}
	require.NoError(t, c.Update(ctx, cur))
	cur = storedMigration(t, c, m)
	require.NoError(t, r.markMigrationRequested(ctx, cur, longOpExport))
	third := requestMarker(storedMigration(t, c, m).Status.Conditions, migrationConditionExportRequested)
	assert.Zero(t, requestResends(third), "a confirmed send starts the count again")
	assert.True(t, third.LastTransitionTime.After(second.LastTransitionTime.Time))
}

// TestMigrationRequest_ResendsAreCapped (review L3): past maxResends
// automatic re-sends the call is not sent again: the migration is held with
// the constant exhausted message.
func TestMigrationRequest_ResendsAreCapped(t *testing.T) {
	t.Run("routed snapshot", func(t *testing.T) {
		vm, p, m := clusteredSnapshottingMigration()
		setRequestMarker(&m.Status.Conditions, migrationConditionSnapshotRequested, migrationConditionSnapshotRequested,
			withResendCount(migrationRequests[longOpSnapshot].requestedMessage, migrationSnapshotResendMax), 1,
			time.Now().Add(-time.Hour))
		prov := &erringMigrationProvider{}
		c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		r := migrationReconcilerOn(c, prov)
		_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
		require.NoError(t, err)
		assert.Zero(t, prov.calls)
		assert.Equal(t, migrationRequests[longOpSnapshot].exhaustedMessage, migrationReady(storedMigration(t, c, m)).Message)
	})
	t.Run("export on a re-send-safe backend", func(t *testing.T) {
		objs, m := migrationFixtureFor(longOpExport)
		setRequestMarker(&m.Status.Conditions, migrationConditionExportRequested, migrationConditionExportRequested,
			withResendCount(migrationRequests[longOpExport].requestedMessage, migrationDiskResendMax), 1,
			time.Now().Add(-2*migrationResendSettleTime))
		prov := &erringMigrationProvider{caps: contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}}}
		c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		r := migrationReconcilerOn(c, prov)
		_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
		require.NoError(t, err)
		assert.Zero(t, prov.calls)
		assert.Equal(t, migrationRequests[longOpExport].exhaustedMessage, migrationReady(storedMigration(t, c, m)).Message)
	})
}

// TestMigrationRequest_UndecidableIsRetriedQuietly (review L7): when it
// cannot be told yet whether a call may be sent again (the provider cannot
// be resolved), the migration is re-checked with no hold and no event.
func TestMigrationRequest_UndecidableIsRetriedQuietly(t *testing.T) {
	objs, m := migrationFixtureFor(longOpExport)
	setRequestMarker(&m.Status.Conditions, migrationConditionExportRequested, migrationConditionExportRequested,
		migrationRequests[longOpExport].requestedMessage, 1, time.Now().Add(-time.Hour))
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	rec := record.NewFakeRecorder(10)
	r := &VMMigrationReconciler{Client: c, Scheme: c.Scheme(), Recorder: rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			return nil, errors.New("provider unreachable")
		}}
	before := storedMigration(t, c, m)
	res, err := r.handleExportingPhase(context.Background(), before.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
	after := storedMigration(t, c, m)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is written")
	assert.Nil(t, migrationReady(after))
	assert.Empty(t, drainRecordedEvents(rec))
}

// TestExportCallMayStillWrite (review L5): the staged object of a deleting
// migration is not deleted while an ExportDisk of unknown outcome may still
// be writing it (until its settle time).
func TestExportCallMayStillWrite(t *testing.T) {
	_, m := migrationFixtureFor(longOpExport)
	r := &VMMigrationReconciler{}
	running, err := r.exportTaskMayBeRunning(context.Background(), m)
	require.NoError(t, err)
	assert.False(t, running, "no marker: nothing may still write")

	setRequestMarker(&m.Status.Conditions, migrationConditionExportRequested, migrationConditionExportRequested,
		"x", 1, time.Now().Add(-time.Minute))
	running, err = r.exportTaskMayBeRunning(context.Background(), m)
	assert.True(t, running)
	assert.ErrorIs(t, err, errExportCallMayStillWrite)

	meta.FindStatusCondition(m.Status.Conditions, migrationConditionExportRequested).LastTransitionTime =
		metav1.NewTime(time.Now().Add(-migrationResendSettleTime - time.Minute))
	running, err = r.exportTaskMayBeRunning(context.Background(), m)
	require.NoError(t, err)
	assert.False(t, running, "past the settle time the earlier call has ended")

	m.Status.ExportID = "export-1"
	assert.False(t, exportCallMayStillWrite(m, time.Now().Add(-time.Hour)), "a recorded export is not an unknown one")
}

// TestOperationOutcomeManagers (review L6): the confirmation's log line names
// the field managers of the annotation.
func TestOperationOutcomeManagers(t *testing.T) {
	obj := &metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{
		{Manager: "kubectl-annotate", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:annotations":{"f:` + operationOutcomeAnnotation + `":{}}}}`)}},
		{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{}}`)}},
	}}
	assert.Equal(t, []string{"kubectl-annotate/Update"}, operationOutcomeManagers(obj))
}

// TestProviderCallOutcomeKnown pins the classification of failed answers.
func TestProviderCallOutcomeKnown(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"deadline":            {deadlineExceeded(), false},
		"bare unavailable":    {contracts.NewRetryableError("unavailable", nil), false},
		"unclassified":        {errors.New("boom"), false},
		"request not sent":    {fmt.Errorf("%w", contracts.ErrRequestNotSent), true},
		"operation not done":  {contracts.NewRetryableError("x", contracts.ErrOperationNotPerformed), true},
		"host unavailable":    {contracts.NewHostUnavailableError("x", nil), true},
		"invalid spec":        {contracts.NewInvalidSpecError("x", nil), true},
		"not found":           {contracts.NewNotFoundError("x", nil), true},
		"conflict":            {contracts.NewConflictError("x", nil), true},
		"disk size over":      {contracts.NewConflictError("x", contracts.ErrDiskSizeExceedsLimit), true},
		"owner stamp refused": {contracts.NewNotFoundError("x", contracts.ErrVMNotOwned), true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, providerCallOutcomeKnown(tc.err))
		})
	}
}
