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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the durable requested-call markers of the VMMigration's
// SnapshotCreate, ExportDisk and ImportDisk (rc2 R6): nothing is sent unless
// the marker was written; a restart between the call and the write of its
// result (simulated by a new reconciler: the in-memory guard and held result
// are gone) re-sends only where that is safe, and holds otherwise until an
// operator confirms.

// capsMigrationProvider is a countingMigrationProvider that reports caps.
type capsMigrationProvider struct {
	countingMigrationProvider
	caps contracts.Capabilities
}

func (p *capsMigrationProvider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	return p.caps, nil
}

// capsSnapshotProvider is a snapshotTaskProvider that records the request
// tokens it is sent.
type tokenSnapshotProvider struct {
	snapshotTaskProvider
	tokens []string
}

func (p *tokenSnapshotProvider) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.tokens = append(p.tokens, req.RequestToken)
	return p.snapshotTaskProvider.SnapshotCreate(ctx, req)
}

// allStatusWritesFail returns a fake client seeded with objs whose VMMigration
// status writes all answer ServiceUnavailable while *failing is true — the
// requested-call marker's included.
func allStatusWritesFail(t *testing.T, failing *atomic.Bool, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failing.Load() {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
}

// migrationReconcilerOn is a VMMigrationReconciler on c resolving every
// Provider to prov.
func migrationReconcilerOn(c client.Client, prov contracts.Provider) *VMMigrationReconciler {
	return &VMMigrationReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}
}

// restarted is r after a manager restart: the same API server, a new process
// (no in-memory guard or held result).
func restarted(r *VMMigrationReconciler) *VMMigrationReconciler {
	return &VMMigrationReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: r.providerInstanceFn}
}

// backdateMarker moves the stored marker of condType age into the past.
func backdateMarker(t *testing.T, c client.Client, m *infrav1beta1.VMMigration, condType string, age time.Duration) {
	t.Helper()
	got := storedMigration(t, c, m)
	cond := meta.FindStatusCondition(got.Status.Conditions, condType)
	require.NotNil(t, cond, "marker %s", condType)
	cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-age).Truncate(time.Second))
	require.NoError(t, c.Status().Update(context.Background(), got))
}

// confirmAttempt sets the operator's confirmation of the stored marker's
// attempt (or value, when not empty).
func confirmAttempt(t *testing.T, c client.Client, m *infrav1beta1.VMMigration, condType, value string) {
	t.Helper()
	got := storedMigration(t, c, m)
	if value == "" {
		value = operationOutcomeNotPerformedPrefix + requestMarkerAttemptID(got.Status.Conditions, condType)
	}
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations[operationOutcomeAnnotation] = value
	require.NoError(t, c.Update(context.Background(), got))
}

func migrationReady(m *infrav1beta1.VMMigration) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
}

// migrationPhaseCall runs the phase handler of op on m.
func migrationPhaseCall(r *VMMigrationReconciler, op migrationLongOp, m *infrav1beta1.VMMigration) (ctrl.Result, error) {
	switch op {
	case longOpSnapshot:
		return r.handleSnapshottingPhase(context.Background(), m)
	case longOpExport:
		return r.handleExportingPhase(context.Background(), m)
	default:
		return r.handleImportingPhase(context.Background(), m)
	}
}

// TestMigrationRequest_MarkerWriteFailureSendsNothing: when the marker cannot
// be written — the API server unavailable, or the VMMigration changed since
// it was read (a Conflict on the resourceVersion precondition) — no call is
// sent.
func TestMigrationRequest_MarkerWriteFailureSendsNothing(t *testing.T) {
	type fixture struct {
		objs  []client.Object
		m     *infrav1beta1.VMMigration
		calls func() int32
	}
	snapProv := &snapshotTaskProvider{sync: true}
	exportProv := &countingMigrationProvider{}
	importProv := &countingMigrationProvider{}
	fixtures := map[migrationLongOp]func() (fixture, contracts.Provider){
		longOpSnapshot: func() (fixture, contracts.Provider) {
			vm, p, m := snapshottingMigration()
			return fixture{objs: []client.Object{vm, p, m}, m: m, calls: snapProv.creates.Load}, snapProv
		},
		longOpExport: func() (fixture, contracts.Provider) {
			vm, p, m := raceMigrationFixture()
			return fixture{objs: []client.Object{vm, p, m}, m: m, calls: exportProv.exportCalls.Load}, exportProv
		},
		longOpImport: func() (fixture, contracts.Provider) {
			p, m := importRaceFixture()
			return fixture{objs: []client.Object{p, m}, m: m, calls: importProv.importCalls.Load}, importProv
		},
	}
	for op, build := range fixtures {
		t.Run(string(op)+"/unavailable", func(t *testing.T) {
			fx, prov := build()
			before := fx.calls()
			var failing atomic.Bool
			failing.Store(true)
			c := allStatusWritesFail(t, &failing, fx.objs...)
			r := migrationReconcilerOn(c, prov)
			_, err := migrationPhaseCall(r, op, storedMigration(t, c, fx.m))
			require.Error(t, err)
			assert.Equal(t, before, fx.calls(), "nothing is sent without the marker")
			assert.False(t, migrationRequested(storedMigration(t, c, fx.m), op))
		})
		t.Run(string(op)+"/conflict", func(t *testing.T) {
			fx, prov := build()
			before := fx.calls()
			c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(fx.objs...).
				WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
			r := migrationReconcilerOn(c, prov)
			read := storedMigration(t, c, fx.m)
			// Another writer changes the VMMigration after it was read.
			other := read.DeepCopy()
			other.Annotations = map[string]string{"example.com/poke": "1"}
			require.NoError(t, c.Update(context.Background(), other))

			_, err := migrationPhaseCall(r, op, read)
			if op == longOpSnapshot {
				// The snapshot's own stale-read check stops it first.
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.True(t, isMigrationStatusContention(err), "a conflict is status contention: %v", err)
			}
			assert.Equal(t, before, fx.calls(), "nothing is sent from a stale read")
			assert.False(t, migrationRequested(storedMigration(t, c, fx.m), op))
		})
	}
}

// TestMigrationRequest_MarkerPrecedesTheCallAndTheResultClearsIt: the marker
// is stored when the call is sent, and the result's write removes it.
func TestMigrationRequest_MarkerPrecedesTheCallAndTheResultClearsIt(t *testing.T) {
	vm, p, m := raceMigrationFixture()
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	var markedAtCall atomic.Bool
	prov := &markerSpyProvider{check: func() {
		markedAtCall.Store(migrationRequested(storedMigration(t, c, m), longOpExport))
	}}
	r := migrationReconcilerOn(c, prov)
	_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	assert.True(t, markedAtCall.Load(), "the marker is stored before ExportDisk is sent")
	got := storedMigration(t, c, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
	assert.False(t, migrationRequested(got, longOpExport), "the result removes the marker")
}

// markerSpyProvider runs check when ExportDisk is called.
type markerSpyProvider struct {
	countingMigrationProvider
	check func()
}

func (p *markerSpyProvider) ExportDisk(ctx context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.check()
	return p.countingMigrationProvider.ExportDisk(ctx, req)
}

// TestMigrationSnapshot_RestartAfterCall_SingleHostIsHeldThenConfirmed: a
// SnapshotCreate whose result was lost to a restart is never re-sent to a
// single-host provider (it could take a second snapshot of the live source
// VM): the migration is held SnapshotOutcomeUnknown, with one Warning event
// naming the snapshot, and writes nothing more while held. A confirmation of
// another attempt changes nothing; the operator's confirmation of this one
// takes the snapshot again (the annotation is removed first).
func TestMigrationSnapshot_RestartAfterCall_SingleHostIsHeldThenConfirmed(t *testing.T) {
	vm, p, m := snapshottingMigration()
	prov := &snapshotTaskProvider{sync: true}
	var failing atomic.Bool
	failing.Store(true)
	c := failingStatusWrites(t, &failing, vm, p, m)
	r := migrationReconcilerOn(c, prov)

	_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.Error(t, err, "the snapshot's result could not be written")
	require.EqualValues(t, 1, prov.creates.Load())
	failing.Store(false)
	require.True(t, migrationRequested(storedMigration(t, c, m), longOpSnapshot), "the marker survived")

	r2 := restarted(r)
	rec := r2.Recorder.(*record.FakeRecorder)
	res, err := r2.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
	assert.EqualValues(t, 1, prov.creates.Load(), "not taken again")
	got := storedMigration(t, c, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseSnapshotting, got.Status.Phase)
	ready := migrationReady(got)
	require.NotNil(t, ready)
	assert.Equal(t, migrationReasonSnapshotOutcomeUnknown, ready.Reason)
	assert.Equal(t, migrationRequests[longOpSnapshot].operatorMessage, ready.Message)
	events := drainRecordedEvents(rec)
	require.Len(t, events, 1)
	name, _ := migrationSnapshotIdentity(got)
	assert.Contains(t, events[0], "Warning "+migrationReasonSnapshotOutcomeUnknown)
	assert.Contains(t, events[0], name)

	// A repeated hold writes nothing.
	rv := got.ResourceVersion
	_, err = r2.handleSnapshottingPhase(context.Background(), got)
	require.NoError(t, err)
	assert.Equal(t, rv, storedMigration(t, c, m).ResourceVersion)
	assert.Empty(t, drainRecordedEvents(rec))

	// A confirmation of another attempt is not this one's.
	confirmAttempt(t, c, m, migrationConditionSnapshotRequested, operationOutcomeNotPerformedPrefix+"2000-01-01T00:00:00Z")
	_, err = r2.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.creates.Load())

	confirmAttempt(t, c, m, migrationConditionSnapshotRequested, "")
	_, err = r2.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	assert.EqualValues(t, 2, prov.creates.Load(), "the operator's confirmation takes it again")
	got = storedMigration(t, c, m)
	assert.Equal(t, "snap-t", got.Status.SnapshotID)
	assert.NotContains(t, got.Annotations, operationOutcomeAnnotation, "the confirmation never outlives its attempt")
	assert.False(t, migrationRequested(got, longOpSnapshot))
	assert.Nil(t, migrationReady(got), "the hold is lifted with the result")
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
}

// clusteredSnapshottingMigration is snapshottingMigration with a clustered
// source Provider and a source VM bound to host-a.
func clusteredSnapshottingMigration() (*infrav1beta1.VirtualMachine, *infrav1beta1.Provider, *infrav1beta1.VMMigration) {
	vm, p, m := snapshottingMigration()
	vm.UID = "uid-src-vm"
	vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-a"}
	p.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	p.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	return vm, p, m
}

// TestMigrationSnapshot_RestartAfterCall_RoutedIsResent: through a clustered
// provider the same SnapshotCreate (same request token) is sent again — the
// provider adopts the snapshot the first call made — paced from the last
// request; a marker an earlier migration attempt left is never re-sent (its
// token was another).
func TestMigrationSnapshot_RestartAfterCall_RoutedIsResent(t *testing.T) {
	vm, p, m := clusteredSnapshottingMigration()
	prov := &tokenSnapshotProvider{snapshotTaskProvider: snapshotTaskProvider{sync: true}}
	var failing atomic.Bool
	failing.Store(true)
	c := failingStatusWrites(t, &failing, vm, p, m)
	r := migrationReconcilerOn(c, prov)
	_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.Error(t, err)
	require.EqualValues(t, 1, prov.creates.Load())
	failing.Store(false)

	r2 := restarted(r)
	// Within the pacing window it waits (the resend message, no call).
	res, err := r2.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	assert.EqualValues(t, 1, prov.creates.Load())
	assert.Equal(t, migrationRequests[longOpSnapshot].resendMessage, migrationReady(storedMigration(t, c, m)).Message)

	backdateMarker(t, c, m, migrationConditionSnapshotRequested, 2*time.Minute)
	_, err = r2.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	require.EqualValues(t, 2, prov.creates.Load(), "re-sent")
	assert.Equal(t, prov.tokens[0], prov.tokens[1], "the same request token: the provider adopts the earlier snapshot")
	got := storedMigration(t, c, m)
	assert.Equal(t, "snap-t", got.Status.SnapshotID)
	assert.False(t, migrationRequested(got, longOpSnapshot))
	assert.Nil(t, migrationReady(got))

	t.Run("a marker of an earlier attempt is held", func(t *testing.T) {
		vm, p, m := clusteredSnapshottingMigration()
		retried := metav1.NewTime(time.Now().Truncate(time.Second))
		m.Status.RetryCount = 1
		m.Status.LastRetryTime = &retried
		setRequestMarker(&m.Status.Conditions, migrationConditionSnapshotRequested, migrationConditionSnapshotRequested,
			"x", 1, retried.Add(-10*time.Minute))
		prov := &tokenSnapshotProvider{snapshotTaskProvider: snapshotTaskProvider{sync: true}}
		c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		r := migrationReconcilerOn(c, prov)
		_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
		require.NoError(t, err)
		assert.Zero(t, prov.creates.Load())
		ready := migrationReady(storedMigration(t, c, m))
		require.NotNil(t, ready)
		assert.Equal(t, migrationRequests[longOpSnapshot].operatorMessage, ready.Message)
	})
}

// TestMigrationExport_RestartAfterCall: an ExportDisk whose result was lost
// is held for an operator when the source provider does not report the
// backend re-send safe; when it does, it is re-sent once the earlier call's
// deadline has passed (held with the resend message until then), and the
// re-send's result (its own checksum) is the one recorded.
func TestMigrationExport_RestartAfterCall(t *testing.T) {
	run := func(t *testing.T, caps contracts.Capabilities) (*VMMigrationReconciler, *capsMigrationProvider, *infrav1beta1.VMMigration) {
		vm, p, m := raceMigrationFixture()
		prov := &capsMigrationProvider{caps: caps}
		var failing atomic.Bool
		failing.Store(true)
		c := failingStatusWrites(t, &failing, vm, p, m)
		r := migrationReconcilerOn(c, prov)
		_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
		require.Error(t, err)
		require.EqualValues(t, 1, prov.exportCalls.Load())
		failing.Store(false)
		return restarted(r), prov, m
	}

	t.Run("not re-send safe: held for an operator", func(t *testing.T) {
		r, prov, m := run(t, contracts.Capabilities{ResendSafeExportBackends: []string{"s3"}})
		res, err := r.handleExportingPhase(context.Background(), storedMigration(t, r.Client, m))
		require.NoError(t, err)
		assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
		assert.EqualValues(t, 1, prov.exportCalls.Load())
		got := storedMigration(t, r.Client, m)
		assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
		assert.Equal(t, migrationReasonExportOutcomeUnknown, migrationReady(got).Reason)
		assert.Equal(t, migrationRequests[longOpExport].operatorMessage, migrationReady(got).Message)

		confirmAttempt(t, r.Client, m, migrationConditionExportRequested, "")
		_, err = r.handleExportingPhase(context.Background(), storedMigration(t, r.Client, m))
		require.NoError(t, err)
		assert.EqualValues(t, 2, prov.exportCalls.Load(), "sent again on the operator's confirmation")
		got = storedMigration(t, r.Client, m)
		assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
		assert.Equal(t, "checksum-2", got.Status.DiskInfo.SourceChecksum)
		assert.NotContains(t, got.Annotations, operationOutcomeAnnotation)
	})

	t.Run("re-send safe: re-sent after the settle time", func(t *testing.T) {
		r, prov, m := run(t, contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}})
		res, err := r.handleExportingPhase(context.Background(), storedMigration(t, r.Client, m))
		require.NoError(t, err)
		assert.EqualValues(t, 1, prov.exportCalls.Load(), "not while the earlier call may still run")
		assert.Greater(t, res.RequeueAfter, migrationResendSettleTime-time.Minute)
		assert.LessOrEqual(t, res.RequeueAfter, migrationResendSettleTime)
		assert.Equal(t, migrationRequests[longOpExport].resendMessage, migrationReady(storedMigration(t, r.Client, m)).Message)

		backdateMarker(t, r.Client, m, migrationConditionExportRequested, migrationResendSettleTime+time.Minute)
		_, err = r.handleExportingPhase(context.Background(), storedMigration(t, r.Client, m))
		require.NoError(t, err)
		assert.EqualValues(t, 2, prov.exportCalls.Load(), "re-sent")
		got := storedMigration(t, r.Client, m)
		assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
		assert.Equal(t, "export-2", got.Status.ExportID)
		assert.Equal(t, "checksum-2", got.Status.DiskInfo.SourceChecksum, "the re-send's checksum is the one recorded")
		assert.False(t, migrationRequested(got, longOpExport))
		assert.Nil(t, migrationReady(got))
	})

	t.Run("a stale cache is never an unknown outcome", func(t *testing.T) {
		vm, p, m := raceMigrationFixture()
		prov := &capsMigrationProvider{}
		c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		r := migrationReconcilerOn(c, prov)
		// A cached copy taken after the marker was written but before the
		// result: the call is sent and its result recorded meanwhile.
		var cached *infrav1beta1.VMMigration
		spy := &markerSpyProvider{check: func() { cached = storedMigration(t, c, m) }}
		r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
		_, err := r.handleExportingPhase(context.Background(), storedMigration(t, c, m))
		require.NoError(t, err)
		require.True(t, migrationRequested(cached, longOpExport))

		r2 := restarted(r)
		r2.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }
		res, err := r2.handleExportingPhase(context.Background(), cached)
		require.NoError(t, err)
		assert.Equal(t, migrationStaleReadRetryInterval, res.RequeueAfter)
		assert.Zero(t, prov.exportCalls.Load())
		assert.Nil(t, migrationReady(storedMigration(t, c, m)), "nothing is held")
	})
}

// TestMigrationImport_RestartAfterCall: the ImportDisk counterpart — held for
// an operator unless the target provider reports the backend re-send safe,
// re-sent after the settle time when it does.
func TestMigrationImport_RestartAfterCall(t *testing.T) {
	for name, tc := range map[string]struct {
		caps     contracts.Capabilities
		resent   bool
		wantHold string
	}{
		"older provider (no capabilities field)": {caps: contracts.Capabilities{}, wantHold: migrationRequests[longOpImport].operatorMessage},
		"re-send safe on pvc":                    {caps: contracts.Capabilities{ResendSafeImportBackends: []string{"pvc"}}, resent: true},
	} {
		t.Run(name, func(t *testing.T) {
			p, m := importRaceFixture()
			prov := &capsMigrationProvider{caps: tc.caps}
			var failing atomic.Bool
			failing.Store(true)
			c := failingStatusWrites(t, &failing, p, m)
			r := migrationReconcilerOn(c, prov)
			_, err := r.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.Error(t, err)
			require.EqualValues(t, 1, prov.importCalls.Load())
			failing.Store(false)

			r2 := restarted(r)
			backdateMarker(t, c, m, migrationConditionImportRequested, migrationResendSettleTime+time.Minute)
			_, err = r2.handleImportingPhase(context.Background(), storedMigration(t, c, m))
			require.NoError(t, err)
			got := storedMigration(t, c, m)
			if !tc.resent {
				assert.EqualValues(t, 1, prov.importCalls.Load(), "never re-sent: it could land a second disk")
				assert.Equal(t, migrationReasonImportOutcomeUnknown, migrationReady(got).Reason)
				assert.Equal(t, tc.wantHold, migrationReady(got).Message)
				assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
				return
			}
			assert.EqualValues(t, 2, prov.importCalls.Load())
			assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
			assert.Equal(t, "disk-2", got.Status.ImportID)
			assert.False(t, migrationRequested(got, longOpImport))
		})
	}
}

// TestMigrationRequest_FailedCallClearsTheMarker: a call the provider
// answered with a failure has a known outcome: the failure's write removes
// the marker, so the retry is not mistaken for an unknown outcome.
func TestMigrationRequest_FailedCallClearsTheMarker(t *testing.T) {
	vm, p, m := snapshottingMigration()
	prov := &snapshotTaskProvider{sync: true}
	failingProv := &failingSnapshotProvider{snapshotTaskProvider: prov}
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(vm, p, m).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := migrationReconcilerOn(c, failingProv)
	_, err := r.handleSnapshottingPhase(context.Background(), storedMigration(t, c, m))
	require.NoError(t, err)
	got := storedMigration(t, c, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.False(t, migrationRequested(got, longOpSnapshot))
}

// failingSnapshotProvider answers every SnapshotCreate with an invalid-spec
// failure.
type failingSnapshotProvider struct {
	*snapshotTaskProvider
}

func (p *failingSnapshotProvider) SnapshotCreate(context.Context, contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.creates.Add(1)
	return contracts.SnapshotCreateResponse{}, contracts.NewInvalidSpecError("bad snapshot name", nil)
}

// TestForgetMigrationLongOps_LogsEvictedResults (review L1 of #388): a result
// held in memory and dropped when the VMMigration is released is logged at
// Error with the provider's ids, never silently.
func TestForgetMigrationLongOps_LogsEvictedResults(t *testing.T) {
	_, _, m := snapshotFixture()
	r := &VMMigrationReconciler{}
	r.longOpInFlight.Store(longOpKey(m, longOpExport), struct{}{})
	r.longOpInFlight.Store(longOpKey(m, longOpImport), pendingMigrationResult{
		apply: func(*infrav1beta1.VMMigration) {}, ids: []any{"import_id", "disk-9", "target_path", "/pool/x"}})
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{})
	r.forgetMigrationLongOps(log.IntoContext(context.Background(), logger), m)

	require.Len(t, lines, 1, "only the held result is logged")
	assert.Contains(t, lines[0], `"error"`)
	assert.Contains(t, lines[0], "disk-9")
	assert.Contains(t, lines[0], "/pool/x")
	assert.True(t, strings.Contains(lines[0], errMigrationResultEvicted.Error()))
	n := 0
	r.longOpInFlight.Range(func(any, any) bool { n++; return true })
	assert.Zero(t, n)
}
