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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin how the manager reads the definite answers of a single-host
// libvirt provider (rc2 R21, internal/providers/libvirt/call_outcome.go): a
// typed failure of SnapshotCreate, ExportDisk or ImportDisk is the call's
// outcome, so it fails the attempt and clears the durable marker; the same
// call answered with an untyped codes.Unknown (what every libvirt failure was
// before R21) keeps the marker and holds the object for an operator.

// libvirtNotPerformed is a single-host libvirt "not performed" answer as the
// manager's transport maps it: codes.Unavailable carrying VM_OPERATION_FAILED
// becomes a retryable error marked contracts.ErrOperationNotPerformed
// (internal/transport/grpc, mapGRPCStatus; pinned there by
// TestMapGRPCError_SingleHostLibvirtDefiniteAnswers).
func libvirtNotPerformed(t *testing.T, op string) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, op+" not performed (details are in the provider log)").
		WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	return contracts.NewRetryableError(fmt.Sprintf("%s: %s", op, st.Message()),
		fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed, st.Err()))
}

// libvirtUntyped is a single-host libvirt failure the provider could not
// classify: a plain codes.Unknown, which the transport keeps untyped.
func libvirtUntyped(op string) error {
	return fmt.Errorf("%s failed: %w", op, status.Error(codes.Unknown, "host command failed"))
}

// definiteLibvirtAnswers are the typed answers of the libvirt provider and
// whether the manager reads each one as a definite outcome.
func definiteLibvirtAnswers(t *testing.T, op string) map[string]struct {
	err   error
	known bool
} {
	return map[string]struct {
		err   error
		known bool
	}{
		"not performed (a host command failed, nothing published)": {libvirtNotPerformed(t, op), true},
		"the source domain is missing (NotFound)":                  {contracts.NewNotFoundError(op+": the libvirt domain was not found on the host", nil), true},
		"the snapshot name is taken (Conflict)":                    {contracts.NewConflictError(op+": a snapshot with this name already exists", nil), true},
		"an invalid request (InvalidArgument)":                     {contracts.NewInvalidSpecError(op+": invalid storage configuration", nil), true},
		"a refusal before any side effect (FailedPrecondition)": {&untypedForTest{msg: op + " failed: the source VM's disk cannot be copied safely",
			cause: status.Error(codes.FailedPrecondition, "refused")}, true},
		"an untyped failure (killed command, lost connection)": {libvirtUntyped(op), false},
	}
}

// untypedForTest mirrors the transport's untypedRPCError: the text of an
// answer it does not categorize, with the gRPC status kept in its chain.
type untypedForTest struct {
	msg   string
	cause error
}

func (e *untypedForTest) Error() string { return e.msg }
func (e *untypedForTest) Unwrap() error { return e.cause }

// TestProviderCallOutcomeKnown_LibvirtAnswers: the classification of every
// answer shape the single-host libvirt provider gives.
func TestProviderCallOutcomeKnown_LibvirtAnswers(t *testing.T) {
	for name, tc := range definiteLibvirtAnswers(t, "exportDisk") {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.known, providerCallOutcomeKnown(tc.err))
		})
	}
}

// failingTransfersProvider answers ExportDisk, ImportDisk and SnapshotCreate
// with err.
type failingTransfersProvider struct {
	capsMigrationProvider
	err error
}

func (p *failingTransfersProvider) ExportDisk(context.Context, contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.exportCalls.Add(1)
	return contracts.ExportDiskResponse{}, p.err
}

func (p *failingTransfersProvider) ImportDisk(context.Context, contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.importCalls.Add(1)
	return contracts.ImportDiskResponse{}, p.err
}

func (p *failingTransfersProvider) SnapshotCreate(context.Context, contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	return contracts.SnapshotCreateResponse{}, p.err
}

// TestMigrationExport_LibvirtDefiniteFailureFailsTheAttempt: a typed definite
// ExportDisk failure fails the migration and clears the marker; an untyped one
// is held (the export may still be writing).
func TestMigrationExport_LibvirtDefiniteFailureFailsTheAttempt(t *testing.T) {
	for name, tc := range definiteLibvirtAnswers(t, "exportDisk") {
		t.Run(name, func(t *testing.T) {
			vm, p, m := raceMigrationFixture()
			prov := &failingTransfersProvider{err: tc.err}
			c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
				WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
			r := migrationReconcilerOn(c, prov)
			_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			require.EqualValues(t, 1, prov.exportCalls.Load())
			got := storedMigration(t, c, m)
			if tc.known {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase, "the attempt failed instead of holding")
				assert.False(t, migrationRequested(got, longOpExport), "the failure replaced the marker")
				return
			}
			assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.True(t, migrationRequested(got, longOpExport), "the marker is kept")
			require.NotNil(t, migrationReady(got))
			assert.Equal(t, migrationReasonExportOutcomeUnknown, migrationReady(got).Reason)
		})
	}
}

// TestMigrationImport_LibvirtDefiniteFailureFailsTheAttempt: the ImportDisk
// counterpart.
func TestMigrationImport_LibvirtDefiniteFailureFailsTheAttempt(t *testing.T) {
	for name, tc := range definiteLibvirtAnswers(t, "importDisk") {
		t.Run(name, func(t *testing.T) {
			p, m := importRaceFixture()
			prov := &failingTransfersProvider{err: tc.err}
			c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(p, m).
				WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
			r := migrationReconcilerOn(c, prov)
			_, err := r.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			require.EqualValues(t, 1, prov.importCalls.Load())
			got := storedMigration(t, c, m)
			if tc.known {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				assert.False(t, migrationRequested(got, longOpImport))
				return
			}
			assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.True(t, migrationRequested(got, longOpImport))
			require.NotNil(t, migrationReady(got))
			assert.Equal(t, migrationReasonImportOutcomeUnknown, migrationReady(got).Reason)
		})
	}
}

// TestMigrationSnapshot_LibvirtDefiniteFailureFailsTheAttempt: the source
// snapshot of a single-host libvirt migration.
func TestMigrationSnapshot_LibvirtDefiniteFailureFailsTheAttempt(t *testing.T) {
	for name, tc := range definiteLibvirtAnswers(t, "snapshotCreate") {
		t.Run(name, func(t *testing.T) {
			vm, p, m := snapshottingMigration()
			prov := &failingTransfersProvider{err: tc.err}
			c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
				WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
			r := migrationReconcilerOn(c, prov)
			_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			got := storedMigration(t, c, m)
			if tc.known {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				assert.False(t, migrationRequested(got, longOpSnapshot))
				return
			}
			assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.True(t, migrationRequested(got, longOpSnapshot))
		})
	}
}

// TestVMSnapshot_LibvirtDefiniteFailureFailsTheAttempt: a single-host
// VMSnapshot create.
func TestVMSnapshot_LibvirtDefiniteFailureFailsTheAttempt(t *testing.T) {
	for name, tc := range definiteLibvirtAnswers(t, "snapshotCreate") {
		t.Run(name, func(t *testing.T) {
			r, rec, snap := markerSnapshotFixture(t, false, &statusWriteFaults{})
			rec.createErr = tc.err
			_, got := reconcileSnapshotOnce(t, r, snap)
			require.Len(t, rec.creates, 1)
			if tc.known {
				assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase, "the attempt failed instead of holding")
				assert.False(t, snapshotRequested(got), "the failure replaced the marker")
				return
			}
			assert.NotEqual(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase)
			assert.True(t, snapshotRequested(got), "the marker is kept")
		})
	}
}

// TestProviderCallOutcomeKnownFor: how a call was sent decides what its failure
// proves. A first send is read as providerCallOutcomeKnown; the manager's own
// re-send proves nothing; a send on an operator's confirmation is a first send
// unless the answer contradicts the confirmation.
func TestProviderCallOutcomeKnownFor(t *testing.T) {
	notPerformed := libvirtNotPerformed(t, "exportDisk")
	invalid := contracts.NewInvalidSpecError("bad", nil)
	taken := contracts.NewConflictError("a snapshot with this name already exists", nil)
	size := contracts.NewConflictError("too big", contracts.ErrDiskSizeExceedsLimit)
	inUse := contracts.NewConflictError("a linked clone depends on it", contracts.ErrVMDiskInUse)
	unknown := libvirtUntyped("exportDisk")

	first, auto, confirmed := callAttempt{}, callAttempt{resend: true}, callAttempt{confirmed: true}
	for name, tc := range map[string]struct {
		err           error
		first, a, cfm bool
	}{
		"not performed":               {notPerformed, true, false, true},
		"invalid spec":                {invalid, true, false, true},
		"name taken (plain conflict)": {taken, true, false, false},
		"disk above the size limit":   {size, true, false, true},
		"disk another VM uses":        {inUse, true, false, true},
		"untyped":                     {unknown, false, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.first, providerCallOutcomeKnownFor(tc.err, first), "first send")
			assert.Equal(t, tc.a, providerCallOutcomeKnownFor(tc.err, auto), "automatic re-send")
			assert.Equal(t, tc.cfm, providerCallOutcomeKnownFor(tc.err, confirmed), "operator-confirmed send")
		})
	}
	assert.Equal(t, callAttempt{}, newCallAttempt(nil, false))
	assert.Equal(t, callAttempt{}, newCallAttempt(nil, true), "a confirmation without a marker is no attempt")
	marker := &metav1.Condition{}
	assert.Equal(t, callAttempt{resend: true}, newCallAttempt(marker, false))
	assert.Equal(t, callAttempt{confirmed: true}, newCallAttempt(marker, true))
}

// confirmedSnapshotSend leaves a VMSnapshot whose first create's result was
// lost, held after a restart, and confirmed by the operator; the provider then
// answers the confirmed create with err.
func confirmedSnapshotSend(t *testing.T, err error) (*snapshotRecorder, *infrav1beta1.VMSnapshot) {
	t.Helper()
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, false, faults)
	_, rerr := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, rerr, "the first create's outcome could not be written")
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	_, got := reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 1, "held")
	got.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
		requestMarkerAttemptID(got.Status.Conditions, snapshotConditionRequested)}
	require.NoError(t, r2.Update(context.Background(), got))

	rec.createErr = err
	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2, "sent again on the confirmation")
	return rec, got
}

// TestVMSnapshot_ConfirmedSendIsAFirstSendExceptContradictions: after the
// operator's confirmation a definite refusal fails the VMSnapshot (as a first
// send's would; holding forever is no outcome), but a Conflict - a snapshot of
// that name exists - contradicts the confirmation and holds again.
func TestVMSnapshot_ConfirmedSendIsAFirstSendExceptContradictions(t *testing.T) {
	_, got := confirmedSnapshotSend(t, contracts.NewInvalidSpecError("snapshotCreate: invalid name", nil))
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase, "a definite failure answering the confirmed send fails it")
	assert.False(t, snapshotRequested(got))

	_, got = confirmedSnapshotSend(t, libvirtNotPerformed(t, "snapshotCreate"))
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase)

	_, got = confirmedSnapshotSend(t, contracts.NewConflictError("snapshotCreate: a snapshot with this name already exists", nil))
	assert.NotEqual(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase, "the answer contradicts the confirmation: held again")
	assert.True(t, snapshotRequested(got), "the marker is kept")

	_, got = confirmedSnapshotSend(t, libvirtUntyped("snapshotCreate"))
	assert.True(t, snapshotRequested(got), "an unknown outcome is held, as ever")
}

// TestVMSnapshot_RoutedResendKeepsItsDefiniteAnswers: a clustered provider
// adopts the earlier snapshot by request token, so a definite refusal answering
// the manager's own re-send of a routed create is still a definite outcome.
func TestVMSnapshot_RoutedResendKeepsItsDefiniteAnswers(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, true, faults)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, err)
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r2.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotConditionRequested)
	require.NotNil(t, cond)
	cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	require.NoError(t, r2.Status().Update(context.Background(), got))

	rec.createErr = contracts.NewNotFoundError("snapshotCreate: the domain is gone", nil)
	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2, "the routed create is sent again")
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase, "a definite answer to a routed re-send stays definite")
	assert.False(t, snapshotRequested(got))
}

// lostExport leaves a VMMigration whose first ExportDisk's result was lost (the
// status write failed), after a restart.
func lostExport(t *testing.T, caps contracts.Capabilities) (*VMMigrationReconciler, client.Client, *infrav1beta1.VMMigration) {
	t.Helper()
	vm, p, m := raceMigrationFixture()
	first := &capsMigrationProvider{caps: caps}
	var failing atomic.Bool
	failing.Store(true)
	c := failingStatusWrites(t, &failing, vm, p, m)
	r := migrationReconcilerOn(c, first)
	_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
	require.Error(t, err)
	require.EqualValues(t, 1, first.exportCalls.Load())
	failing.Store(false)
	return restarted(r), c, m
}

func sendExportAgain(t *testing.T, r *VMMigrationReconciler, c client.Client, m *infrav1beta1.VMMigration, again *failingTransfersProvider) {
	t.Helper()
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return again, nil }
	_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
}

// TestMigrationExport_AutomaticResendIsNeverDefinite: the manager's own re-send
// of an ExportDisk (the backend is re-send safe, the settle time has passed),
// answered "not performed", keeps the marker: the answer says nothing about the
// first call.
func TestMigrationExport_AutomaticResendIsNeverDefinite(t *testing.T) {
	r, c, m := lostExport(t, contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}})
	backdateMarker(t, c, m, migrationConditionExportRequested, migrationResendSettleTime+time.Minute)
	again := &failingTransfersProvider{capsMigrationProvider: capsMigrationProvider{caps: contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}}},
		err: libvirtNotPerformed(t, "exportDisk")}
	sendExportAgain(t, r, c, m, again)
	require.EqualValues(t, 1, again.exportCalls.Load(), "re-sent")
	got := storedMigration(t, c, m)
	assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.True(t, migrationRequested(got, longOpExport), "the marker is kept")
}

// TestMigrationExport_ConfirmedSendIsAFirstSendExceptContradictions: after the
// operator's confirmation a "not performed" fails the migration, a plain
// Conflict holds it again.
func TestMigrationExport_ConfirmedSendIsAFirstSendExceptContradictions(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		failed bool
	}{
		"not performed":        {err: libvirtNotPerformed(t, "exportDisk"), failed: true},
		"invalid request":      {err: contracts.NewInvalidSpecError("exportDisk: bad", nil), failed: true},
		"conflict":             {err: contracts.NewConflictError("exportDisk: already exists", nil), failed: false},
		"untyped (lost, held)": {err: libvirtUntyped("exportDisk"), failed: false},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, m := lostExport(t, contracts.Capabilities{})
			again := &failingTransfersProvider{err: tc.err}
			sendExportAgain(t, r, c, m, again)
			require.EqualValues(t, 0, again.exportCalls.Load(), "held first")
			confirmAttempt(t, c, m, migrationConditionExportRequested, "")
			sendExportAgain(t, r, c, m, again)
			require.EqualValues(t, 1, again.exportCalls.Load(), "sent again on the confirmation")
			got := storedMigration(t, c, m)
			if tc.failed {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				assert.False(t, migrationRequested(got, longOpExport))
				return
			}
			assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.True(t, migrationRequested(got, longOpExport), "the marker is kept")
		})
	}
}

// TestMigrationImport_ConfirmedSendLandingNameInUseHolds: an import sent on an
// operator's confirmation, refused because the landing name is in use (a
// Conflict), contradicts the confirmation and holds; a "not performed" fails.
func TestMigrationImport_ConfirmedSendLandingNameInUseHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		failed bool
	}{
		"landing name in use": {err: contracts.NewConflictError("importDisk: the disk already exists and is in use by another domain", nil), failed: false},
		"not performed":       {err: libvirtNotPerformed(t, "importDisk"), failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, m := importRaceFixture()
			var failing atomic.Bool
			failing.Store(true)
			c := failingStatusWrites(t, &failing, p, m)
			r := migrationReconcilerOn(c, &capsMigrationProvider{})
			_, err := r.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.Error(t, err)
			failing.Store(false)

			again := &failingTransfersProvider{err: tc.err}
			r2 := restarted(r)
			r2.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return again, nil }
			_, err = r2.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			require.EqualValues(t, 0, again.importCalls.Load(), "held first")
			confirmAttempt(t, c, m, migrationConditionImportRequested, "")
			_, err = r2.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			require.EqualValues(t, 1, again.importCalls.Load())
			got := storedMigration(t, c, m)
			if tc.failed {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				return
			}
			assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.True(t, migrationRequested(got, longOpImport))
		})
	}
}
