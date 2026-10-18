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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// landedTestPath is the landing path the fixtures' import returned.
const landedTestPath = "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"

// landedProvider is a provider whose DeleteLandedDisk and GetCapabilities
// answer from a script; it counts the deletes. Safe for concurrent use.
type landedProvider struct {
	stubProvider
	mu        sync.Mutex
	supported bool
	outcome   contracts.LandedDiskDeleteOutcome
	err       error
	lastReq   contracts.LandedDiskDeleteRequest
	calls     atomic.Int32
}

func newLandedProvider() *landedProvider {
	return &landedProvider{supported: true, outcome: contracts.LandedDiskDeleted}
}

func (p *landedProvider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contracts.Capabilities{SupportsLandedDiskDelete: p.supported}, nil
}

func (p *landedProvider) DeleteLandedDisk(_ context.Context, req contracts.LandedDiskDeleteRequest) (contracts.LandedDiskDeleteOutcome, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastReq = req
	return p.outcome, p.err
}

func (p *landedProvider) answer(outcome contracts.LandedDiskDeleteOutcome, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcome, p.err = outcome, err
}

func (p *landedProvider) lastRequest() contracts.LandedDiskDeleteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq
}

// landedTargetProvider is the fixtures' target Provider.
func landedTargetProvider() *infrav1beta1.Provider {
	return &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default", UID: "uid-target-1"}}
}

// landedFixture is a pvc migration in phase whose import was sent
// (LandedDiskCleanup=Unknown, import target recorded with the target
// Provider's UID) and returned landedTestPath, with its source VM and the
// source and target Providers. It created no source snapshot.
func landedFixture(phase infrav1beta1.MigrationPhase) (*infrav1beta1.VMMigration, []client.Object) {
	vm, srcProvider, m := snapshotFixture()
	m.Status.SnapshotID = ""
	m.Finalizers = []string{infrav1beta1.VMMigrationFinalizer}
	m.Status.Phase = phase
	target := landedTargetProvider()
	markLandedDiskMayExist(m, migrationTargetVMKey(m), target)
	m.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{TargetPath: landedTestPath}
	return m, []client.Object{vm, srcProvider, target}
}

// landedHarness is a VMMigrationReconciler over a fake client whose provider
// resolution returns prov.
type landedHarness struct {
	r   *VMMigrationReconciler
	c   client.Client
	rec *record.FakeRecorder
}

func newLandedHarness(t *testing.T, prov contracts.Provider, m *infrav1beta1.VMMigration, objs ...client.Object) *landedHarness {
	t.Helper()
	scheme := stagingScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, m)...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		Build()
	h := &landedHarness{c: c, rec: record.NewFakeRecorder(50)}
	h.r = &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: h.rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}
	return h
}

func (h *landedHarness) reconcile(t *testing.T, m *infrav1beta1.VMMigration) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	require.NoError(t, err)
	return res
}

func (h *landedHarness) get(t *testing.T, m *infrav1beta1.VMMigration) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		return nil
	}
	return got
}

func landedCondition(m *infrav1beta1.VMMigration) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, migrationConditionLandedDiskCleanup)
}

func markMigrationDeleting(m *infrav1beta1.VMMigration) {
	now := metav1.Now()
	m.DeletionTimestamp = &now
}

// TestLandedDiskCleanupWanted_DecisionTable pins the cleanupPolicy semantics:
// never on success; OnSuccess on deletion only; Always on terminal failure and
// deletion; Never never; nothing without a LandedDiskCleanup marker or once it
// is True.
func TestLandedDiskCleanupWanted_DecisionTable(t *testing.T) {
	triggers := []stagingCleanupTrigger{stagingTriggerReady, stagingTriggerTerminalFailure, stagingTriggerDeletion}
	for policy, want := range map[string][3]bool{
		"":                                  {false, false, true},
		infrav1beta1.CleanupPolicyOnSuccess: {false, false, true},
		infrav1beta1.CleanupPolicyAlways:    {false, true, true},
		infrav1beta1.CleanupPolicyNever:     {false, false, false},
	} {
		for i, trigger := range triggers {
			m, _ := landedFixture(infrav1beta1.MigrationPhaseFailed)
			if policy != "" {
				m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: policy}
			}
			assert.Equal(t, want[i], landedDiskCleanupWanted(m, trigger), "policy %q trigger %d", policy, trigger)
		}
	}

	m, _ := landedFixture(infrav1beta1.MigrationPhaseFailed)
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionLandedDiskCleanup)
	assert.False(t, landedDiskCleanupWanted(m, stagingTriggerDeletion), "an import an older manager sent is left as it was")
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionLandedDiskCleanup,
		Status: metav1.ConditionTrue, Reason: migrationReasonLandedDiskDeleted, Message: migrationLandedDiskDeletedMessage})
	assert.False(t, landedDiskCleanupWanted(m, stagingTriggerDeletion), "done is done")
}

// TestMigrationHoldsProviderWhileLandedDiskCleanupIsPending: a failed
// migration whose landed disk its cleanupPolicy will delete keeps its target
// Provider from being deleted; once done, or with cleanupPolicy Never, or
// when Ready (the disk is the target VM's), it does not.
func TestMigrationHoldsProviderWhileLandedDiskCleanupIsPending(t *testing.T) {
	m, _ := landedFixture(infrav1beta1.MigrationPhaseFailed)
	assert.True(t, migrationHoldsProvider(m))

	ready, _ := landedFixture(infrav1beta1.MigrationPhaseReady)
	assert.False(t, migrationHoldsProvider(ready))

	never, _ := landedFixture(infrav1beta1.MigrationPhaseFailed)
	never.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
	assert.False(t, migrationHoldsProvider(never))

	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionLandedDiskCleanup,
		Status: metav1.ConditionTrue, Reason: migrationReasonLandedDiskDeleted, Message: migrationLandedDiskDeletedMessage})
	assert.False(t, migrationHoldsProvider(m))
}

// TestMarkLandedDiskMayExist records the VM, the Provider UID and the marker
// once.
func TestMarkLandedDiskMayExist(t *testing.T) {
	_, _, m := snapshotFixture()
	target := landedTargetProvider()
	key := migrationTargetVMKey(m)
	require.True(t, markLandedDiskMayExist(m, key, target))
	assert.Equal(t, &infrav1beta1.MigrationImportTarget{Namespace: "default", Name: "target-vm", ProviderUID: "uid-target-1"},
		m.Status.StorageInfo.ImportTarget)
	c := landedCondition(m)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, migrationReasonLandedDiskMayExist, c.Reason)
	assert.False(t, markLandedDiskMayExist(m, key, target), "a repeated mark writes nothing")

	other := target.DeepCopy()
	other.UID = "uid-target-2"
	assert.True(t, markLandedDiskMayExist(m, key, other), "a new target Provider is recorded")
	assert.Equal(t, "uid-target-2", m.Status.StorageInfo.ImportTarget.ProviderUID)
}

// landedMarkObserver is a provider whose ImportDisk reads the STORED
// migration and records whether the landed-disk marker was persisted when the
// import was sent.
type landedMarkObserver struct {
	countingMigrationProvider
	c      client.Client
	key    client.ObjectKey
	marked bool
	target *infrav1beta1.MigrationImportTarget
}

func (p *landedMarkObserver) ImportDisk(ctx context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	got := &infrav1beta1.VMMigration{}
	if err := p.c.Get(ctx, p.key, got); err == nil {
		p.marked = landedDiskMayExist(got)
		if got.Status.StorageInfo != nil {
			p.target = got.Status.StorageInfo.ImportTarget
		}
	}
	return p.countingMigrationProvider.ImportDisk(ctx, req)
}

// TestImportPersistsTheLandedDiskMarkerBeforeTheRPC: a pvc migration's import
// is sent only after LandedDiskCleanup=Unknown and its target (VM and Provider
// UID) are stored.
func TestImportPersistsTheLandedDiskMarkerBeforeTheRPC(t *testing.T) {
	targetProvider, migration := importRaceFixture()
	targetProvider.UID = "uid-race-target"
	scheme := capGatingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(targetProvider, migration).
		WithStatusSubresource(migration).Build()
	prov := &landedMarkObserver{c: c, key: client.ObjectKeyFromObject(migration)}
	r := &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}

	held := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(migration), held))
	_, err := r.handleImportingPhase(context.Background(), held)
	require.NoError(t, err)
	require.EqualValues(t, 1, prov.importCalls.Load())
	assert.True(t, prov.marked, "the marker was stored before the import was sent")
	assert.Equal(t, &infrav1beta1.MigrationImportTarget{Namespace: "default", Name: "target-vm", ProviderUID: "uid-race-target"}, prov.target)
}

// TestTerminalFailureFollowsTheLandedDiskCleanupPolicy: a migration that failed
// for good deletes its landed disk only with cleanupPolicy Always, naming the
// recorded VM and path; OnSuccess keeps it until the VMMigration is deleted.
func TestTerminalFailureFollowsTheLandedDiskCleanupPolicy(t *testing.T) {
	for policy, wantCalls := range map[string]int32{
		infrav1beta1.CleanupPolicyOnSuccess: 0,
		infrav1beta1.CleanupPolicyAlways:    1,
	} {
		t.Run(policy, func(t *testing.T) {
			prov := newLandedProvider()
			m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
			m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: policy}
			h := newLandedHarness(t, prov, m, objs...)
			h.reconcile(t, m)
			assert.Equal(t, wantCalls, prov.calls.Load())
			c := landedCondition(h.get(t, m))
			require.NotNil(t, c)
			if wantCalls == 0 {
				assert.Equal(t, metav1.ConditionUnknown, c.Status, "kept until the VMMigration is deleted")
				return
			}
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, migrationReasonLandedDiskDeleted, c.Reason)
			req := prov.lastRequest()
			assert.Equal(t, contracts.ObjectIdentity{Namespace: "default", Name: "target-vm"}, req.TargetVM)
			assert.Equal(t, landedTestPath, req.Path)
			assert.Equal(t, "uid-snap-1", req.Migration.UID)
			assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonLandedDiskDeleted), 1)

			h.reconcile(t, h.get(t, m))
			assert.Equal(t, wantCalls, prov.calls.Load(), "done once: no second RPC")
		})
	}
}

// TestDeletionDeletesTheLandedDisk: a deleted VMMigration (OnSuccess) whose
// import landed a disk no VM uses deletes it, then releases its finalizer.
func TestDeletionDeletesTheLandedDisk(t *testing.T) {
	for _, phase := range []infrav1beta1.MigrationPhase{infrav1beta1.MigrationPhaseFailed, infrav1beta1.MigrationPhaseCreating} {
		t.Run(string(phase), func(t *testing.T) {
			prov := newLandedProvider()
			prov.answer(contracts.LandedDiskNotFound, nil)
			m, objs := landedFixture(phase)
			markMigrationDeleting(m)
			h := newLandedHarness(t, prov, m, objs...)
			h.reconcile(t, m)
			assert.EqualValues(t, 1, prov.calls.Load())
			assert.Nil(t, h.get(t, m), "released")
			assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonLandedDiskDeleted), 1)
		})
	}
}

// TestLandedDiskIsKeptForAVM: the disk is never deleted when the migration
// succeeded, when its target VirtualMachine (bound by UID, or any VM there
// naming an imported disk) exists, or when the provider reports a VM uses it.
func TestLandedDiskIsKeptForAVM(t *testing.T) {
	targetVM := func(uid string, imported bool) *infrav1beta1.VirtualMachine {
		vm := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "target-vm", Namespace: "default", UID: types.UID("uid-vm-" + uid)}}
		if imported {
			vm.Spec.ImportedDisk = &infrav1beta1.ImportedDiskRef{Path: landedTestPath}
		}
		return vm
	}
	for name, tc := range map[string]struct {
		phase     infrav1beta1.MigrationPhase
		vm        *infrav1beta1.VirtualMachine
		bind      bool
		provErr   error
		wantCalls int32
	}{
		"succeeded":                  {phase: infrav1beta1.MigrationPhaseReady},
		"own target VM (bound UID)":  {phase: infrav1beta1.MigrationPhaseFailed, vm: targetVM("a", false), bind: true},
		"a VM there naming the disk": {phase: infrav1beta1.MigrationPhaseFailed, vm: targetVM("b", true)},
		"provider: a VM uses the disk": {phase: infrav1beta1.MigrationPhaseFailed, wantCalls: 1,
			provErr: fmt.Errorf("deleteLandedDisk: %w", contracts.NewConflictError("in use", contracts.ErrVMDiskInUse))},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newLandedProvider()
			prov.answer("", tc.provErr)
			m, objs := landedFixture(tc.phase)
			markMigrationDeleting(m)
			if tc.vm != nil {
				objs = append(objs, tc.vm)
				if tc.bind {
					m.Status.TargetVMUID = string(tc.vm.UID)
				}
			}
			h := newLandedHarness(t, prov, m, objs...)
			hold, res, err := h.r.releaseLandedDiskForDeletion(context.Background(), h.get(t, m))
			assert.False(t, hold, "released")
			require.NoError(t, err)
			assert.Zero(t, res.RequeueAfter)
			assert.Equal(t, tc.wantCalls, prov.calls.Load())
			c := landedCondition(h.get(t, m))
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			want := migrationReasonLandedDiskInUse
			if tc.phase == infrav1beta1.MigrationPhaseReady {
				// A Ready migration still Unknown (it reached Ready under rc2).
				want = migrationReasonLandedDiskInUseByTarget
			}
			assert.Equal(t, want, c.Reason)
		})
	}
}

// TestMarkLandedDiskInUseByTarget (rc2 R23): Ready closes an open
// LandedDiskCleanup condition with True/LandedDiskInUseByTarget once, with
// ObservedGeneration; it leaves a migration without the condition, or with a
// closed one, as it is.
func TestMarkLandedDiskInUseByTarget(t *testing.T) {
	m, _ := landedFixture(infrav1beta1.MigrationPhaseReady)
	m.Generation = 3
	require.True(t, markLandedDiskInUseByTarget(m))
	c := landedCondition(m)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, migrationReasonLandedDiskInUseByTarget, c.Reason)
	assert.Equal(t, migrationLandedDiskInUseByTargetMessage, c.Message)
	assert.EqualValues(t, 3, c.ObservedGeneration)
	assert.False(t, markLandedDiskInUseByTarget(m), "a repeated mark writes nothing")
	assert.False(t, landedDiskMayExist(m))
	assert.False(t, landedDiskCleanupWanted(m, stagingTriggerDeletion), "nothing to delete once Ready")

	none, _ := landedFixture(infrav1beta1.MigrationPhaseReady)
	meta.RemoveStatusCondition(&none.Status.Conditions, migrationConditionLandedDiskCleanup)
	assert.False(t, markLandedDiskInUseByTarget(none), "no import was sent (or an older manager sent it)")
	assert.Nil(t, landedCondition(none))

	deleted, _ := landedFixture(infrav1beta1.MigrationPhaseReady)
	meta.SetStatusCondition(&deleted.Status.Conditions, metav1.Condition{Type: migrationConditionLandedDiskCleanup,
		Status: metav1.ConditionTrue, Reason: migrationReasonLandedDiskDeleted, Message: migrationLandedDiskDeletedMessage})
	assert.False(t, markLandedDiskInUseByTarget(deleted), "a closed condition is left as it is")
	assert.Equal(t, migrationReasonLandedDiskDeleted, landedCondition(deleted).Reason)
}

// TestReadyMigrationClosesItsLandedDiskAndDeletionKeepsIt (rc2 R23): a Ready
// migration still Unknown (it reached Ready under rc2) is closed once with
// LandedDiskInUseByTarget; deleting it — with any cleanupPolicy — sends no
// DeleteLandedDisk, writes nothing more and releases it.
func TestReadyMigrationClosesItsLandedDiskAndDeletionKeepsIt(t *testing.T) {
	for _, policy := range []string{"", infrav1beta1.CleanupPolicyAlways, infrav1beta1.CleanupPolicyNever} {
		t.Run("policy "+policy, func(t *testing.T) {
			prov := newLandedProvider()
			m, objs := landedFixture(infrav1beta1.MigrationPhaseReady)
			if policy != "" {
				m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: policy}
			}
			h := newLandedHarness(t, prov, m, objs...)
			h.reconcile(t, m)
			got := h.get(t, m)
			c := landedCondition(got)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, migrationReasonLandedDiskInUseByTarget, c.Reason)

			rv := got.ResourceVersion
			h.reconcile(t, got)
			assert.Equal(t, rv, h.get(t, m).ResourceVersion, "a closed condition writes nothing")

			deleting := h.get(t, m)
			markMigrationDeleting(deleting)
			hold, res, err := h.r.releaseLandedDiskForDeletion(context.Background(), deleting)
			require.NoError(t, err)
			assert.False(t, hold, "released")
			assert.Zero(t, res.RequeueAfter)
			assert.Equal(t, rv, h.get(t, m).ResourceVersion, "the deletion writes nothing for the landed disk")
			assert.Zero(t, prov.calls.Load(), "the target VM's disk is never sent for deletion")
		})
	}
}

// TestLandedDiskIsHandedOverToAnotherMigration: another in-flight migration
// that targets the same VM lands its disk under the same name: kept.
func TestLandedDiskIsHandedOverToAnotherMigration(t *testing.T) {
	prov := newLandedProvider()
	m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
	markMigrationDeleting(m)
	other := m.DeepCopy()
	other.Name, other.UID, other.DeletionTimestamp, other.Finalizers = "retry", "uid-retry", nil, nil
	other.Status = infrav1beta1.VMMigrationStatus{Phase: infrav1beta1.MigrationPhaseImporting}
	h := newLandedHarness(t, prov, m, append(objs, other)...)
	h.reconcile(t, m)
	assert.Zero(t, prov.calls.Load())
	assert.Nil(t, h.get(t, m), "released")
}

// TestValidatingTargetReadyClosesTheLandedDisk (rc2 R23): the status write that
// marks a migration Ready also writes LandedDiskCleanup=True with reason
// LandedDiskInUseByTarget at the migration's generation.
func TestValidatingTargetReadyClosesTheLandedDisk(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture("default")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
	migration.Status.TargetVMUID = readyTargetVMUID
	migration.Generation = 2
	markLandedDiskMayExist(migration, migrationTargetVMKey(migration), targetProvider)
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration,
		readyTargetVM("default"))

	_, err := r.handleValidatingTargetPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	require.Equal(t, infrav1beta1.MigrationPhaseReady, got.Status.Phase)
	c := landedCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, migrationReasonLandedDiskInUseByTarget, c.Reason)
	assert.Equal(t, migrationLandedDiskInUseByTargetMessage, c.Message)
	assert.Equal(t, got.Generation, c.ObservedGeneration)
}

// TestLandedDiskLeftBehind: an older provider image, a target Provider that is
// gone or was replaced, and a refused request are recorded as left behind
// with one Warning and release the deletion; nothing names a location.
func TestLandedDiskLeftBehind(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(p *landedProvider, objs []client.Object) []client.Object
		wantCalls int32
		message   string
	}{
		"older provider": {setup: func(p *landedProvider, o []client.Object) []client.Object { p.supported = false; return o },
			message: landedDiskLeftUnsupported},
		"provider answers Unimplemented": {wantCalls: 1, message: landedDiskLeftUnsupported,
			setup: func(p *landedProvider, o []client.Object) []client.Object {
				p.answer("", contracts.NewNotSupportedError("deleteLandedDisk: unimplemented"))
				return o
			}},
		"refused": {wantCalls: 1, message: landedDiskLeftRefused,
			setup: func(p *landedProvider, o []client.Object) []client.Object {
				p.answer("", contracts.NewInvalidSpecError("bad path "+landedTestPath, nil))
				return o
			}},
		"provider gone": {message: landedDiskLeftProviderGone,
			setup: func(_ *landedProvider, o []client.Object) []client.Object { return o[:2] }},
		"provider replaced": {message: landedDiskLeftProviderChanged,
			setup: func(_ *landedProvider, o []client.Object) []client.Object {
				replaced := landedTargetProvider()
				replaced.UID = "uid-target-2"
				return append(o[:2], replaced)
			}},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newLandedProvider()
			m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
			markMigrationDeleting(m)
			objs = tc.setup(prov, objs)
			h := newLandedHarness(t, prov, m, objs...)
			hold, res, err := h.r.releaseLandedDiskForDeletion(context.Background(), h.get(t, m))
			assert.False(t, hold, "released")
			require.NoError(t, err)
			assert.Zero(t, res.RequeueAfter)
			assert.Equal(t, tc.wantCalls, prov.calls.Load())
			c := landedCondition(h.get(t, m))
			require.NotNil(t, c)
			assert.Equal(t, migrationReasonLandedDiskLeftBehind, c.Reason)
			assert.Equal(t, tc.message+migrationLandedDiskLeftBehindSuffix, c.Message)
			events := drainRecordedEvents(h.rec)
			require.Len(t, filterEvents(events, migrationReasonLandedDiskLeftBehind), 1)
			for _, e := range events {
				assert.NotContains(t, e, landedTestPath)
			}
		})
	}
}

// TestLandedDiskDeletionHoldsReleasesAndForceDeletes: a failing delete keeps
// the VMMigration (held, constant message, backoff); a recovered provider
// releases it; force-delete releases it at once with a Warning; cleanupPolicy
// Never releases it without an RPC.
func TestLandedDiskDeletionHoldsReleasesAndForceDeletes(t *testing.T) {
	t.Run("held, then released when the provider recovers", func(t *testing.T) {
		prov := newLandedProvider()
		prov.answer("", contracts.NewRetryableError("unavailable: "+landedTestPath, nil))
		m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
		markMigrationDeleting(m)
		h := newLandedHarness(t, prov, m, objs...)

		res := h.reconcile(t, m)
		assert.Equal(t, blockedRetryMin, res.RequeueAfter)
		got := h.get(t, m)
		require.NotNil(t, got, "the finalizer is kept")
		c := landedCondition(got)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Equal(t, migrationReasonLandedDiskCleanupPending, c.Reason)
		assert.Equal(t, migrationLandedDiskCleanupPendingMessage, c.Message)
		assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonLandedDiskCleanupPending), 1)

		// A reconcile before the backoff sends nothing.
		res = h.reconcile(t, got)
		assert.Positive(t, res.RequeueAfter)
		assert.EqualValues(t, 1, prov.calls.Load())

		prov.answer(contracts.LandedDiskDeleted, nil)
		got = h.get(t, m)
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == migrationConditionLandedDiskCleanup {
				got.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute))
			}
		}
		require.NoError(t, h.c.Status().Update(context.Background(), got))
		h.reconcile(t, got)
		assert.Nil(t, h.get(t, m), "released once the landed disk is deleted")
		assert.EqualValues(t, 2, prov.calls.Load())
	})

	t.Run("force-delete releases with a Warning", func(t *testing.T) {
		prov := newLandedProvider()
		prov.answer("", contracts.NewRetryableError("unavailable", nil))
		m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
		markMigrationDeleting(m)
		m.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		h := newLandedHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		left := filterEvents(drainRecordedEvents(h.rec), migrationReasonLandedDiskLeftBehind)
		require.Len(t, left, 1)
		assert.Contains(t, left[0], forceDeleteAnnotation)
	})

	t.Run("cleanupPolicy Never releases without an RPC", func(t *testing.T) {
		prov := newLandedProvider()
		m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
		markMigrationDeleting(m)
		m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
		h := newLandedHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		assert.Zero(t, prov.calls.Load())
	})
}

// TestLandedDiskCleanup_SecurityReviewCases pins the review's cases: the own
// target VM recognised by its claim alone is kept; a VM at the target name
// that is neither the migration's nor names an imported disk does not stop
// the RPC (the provider's in-use check is the backstop); an import target
// recorded without a Provider UID, and a target namespace that no longer
// grants the migration's, are left behind without an RPC; a target Provider
// that no longer grants this namespace holds the cleanup (retryably, like the
// staged disk) without an RPC.
func TestLandedDiskCleanup_SecurityReviewCases(t *testing.T) {
	type outcome struct {
		hold    bool
		calls   int32
		reason  string
		message string
	}
	for name, tc := range map[string]struct {
		setup func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object
		want  outcome
	}{
		"own target VM by claim only": {
			setup: func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object {
				m.Status.TargetVMClaim = migrationTargetClaimDigest("claim-token")
				return append(objs, &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{
					Name: "target-vm", Namespace: "default", UID: "uid-claimed",
					Annotations: map[string]string{migrationTargetClaimAnnotation: "claim-token"}}})
			},
			want: outcome{reason: migrationReasonLandedDiskInUse, message: migrationLandedDiskInUseMessage},
		},
		"a VM there that is not the migration's and names no imported disk": {
			setup: func(_ *infrav1beta1.VMMigration, objs []client.Object) []client.Object {
				return append(objs, &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{
					Name: "target-vm", Namespace: "default", UID: "uid-someone-else"}})
			},
			want: outcome{calls: 1, reason: migrationReasonLandedDiskDeleted, message: migrationLandedDiskDeletedMessage},
		},
		"import target recorded without a Provider UID": {
			setup: func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object {
				m.Status.StorageInfo.ImportTarget.ProviderUID = ""
				return objs
			},
			want: outcome{reason: migrationReasonLandedDiskLeftBehind,
				message: landedDiskLeftUnrecorded + migrationLandedDiskLeftBehindSuffix},
		},
		"target namespace grant withdrawn": {
			setup: func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object {
				m.Spec.Target.Namespace = "team-b" // no Namespace object: no grant
				markLandedDiskMayExist(m, migrationTargetVMKey(m), landedTargetProvider())
				return objs
			},
			want: outcome{reason: migrationReasonLandedDiskLeftBehind,
				message: landedDiskLeftNamespace + migrationLandedDiskLeftBehindSuffix},
		},
		"target Provider consumer grant withdrawn": {
			setup: func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object {
				shared := landedTargetProvider()
				shared.Namespace = "shared-providers" // selects no consumer namespace
				m.Spec.Target.ProviderRef = infrav1beta1.ObjectRef{Name: shared.Name, Namespace: shared.Namespace}
				markLandedDiskMayExist(m, migrationTargetVMKey(m), shared)
				return append(objs[:2], shared)
			},
			want: outcome{hold: true, reason: migrationReasonLandedDiskCleanupPending, message: migrationLandedDiskCleanupPendingMessage},
		},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newLandedProvider()
			m, objs := landedFixture(infrav1beta1.MigrationPhaseFailed)
			markMigrationDeleting(m)
			objs = tc.setup(m, objs)
			h := newLandedHarness(t, prov, m, objs...)
			if tc.want.hold {
				_, cerr := h.r.cleanupLandedDisk(context.Background(), h.get(t, m))
				assert.True(t, isConsumerNotAllowed(cerr), "held for the consumer grant: %v", cerr)
			}
			hold, _, err := h.r.releaseLandedDiskForDeletion(context.Background(), h.get(t, m))
			require.NoError(t, err)
			assert.Equal(t, tc.want.hold, hold)
			assert.Equal(t, tc.want.calls, prov.calls.Load())
			c := landedCondition(h.get(t, m))
			require.NotNil(t, c)
			assert.Equal(t, tc.want.reason, c.Reason)
			assert.Equal(t, tc.want.message, c.Message)
		})
	}
}
