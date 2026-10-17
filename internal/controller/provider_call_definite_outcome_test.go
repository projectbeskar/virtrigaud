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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
