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
	stderrors "errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestHandleCreatingPhase_NilDiskInfoFailsCleanly verifies the Creating-phase
// guard (Bug Y): a migration that reaches Creating without recorded disk info
// (status.diskInfo nil) must transition to Failed with a diagnosable message
// rather than dereference nil and panic in an endless recover/requeue loop.
func TestHandleCreatingPhase_NilDiskInfoFailsCleanly(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{} // not exercised on this path
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.DiskInfo = nil // import did not record disk info
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	// Must not panic; must fail cleanly.
	_, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)

	var got infrav1beta1.VMMigration
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(migration), &got))
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase,
		"a Creating migration with nil DiskInfo must transition to Failed")
	assert.Contains(t, got.Status.Message, "disk info",
		"the failure message must name the missing disk info")
}

// creatingHarness is a Creating migration (importRaceFixture, disk imported)
// against a fake API server (live). With lagging set, the reconciler's
// cached client does not show VirtualMachines yet (its Get answers NotFound)
// while the live reader sees them. funcs, when set, intercept the live
// client's calls (both the cached client and the APIReader go through it).
type creatingHarness struct {
	r         *VMMigrationReconciler
	live      client.WithWatch
	migration *infrav1beta1.VMMigration
}

func newCreatingHarness(t *testing.T, lagging bool, funcs *interceptor.Funcs, objs ...client.Object) *creatingHarness {
	t.Helper()
	targetProvider, migration := importRaceFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "disk-1"
	migration.Status.DiskInfo.TargetDiskID = "disk-1"
	migration.Status.DiskInfo.TargetPath = "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"
	for _, o := range objs {
		if m, ok := o.(*infrav1beta1.VMMigration); ok {
			migration = m
		}
	}
	all := []client.Object{targetProvider, migration}
	for _, o := range objs {
		if _, ok := o.(*infrav1beta1.VMMigration); !ok {
			all = append(all, o)
		}
	}
	var live client.WithWatch = fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(all...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	if funcs != nil {
		live = interceptor.NewClient(live, *funcs)
	}
	var cached client.Client = live
	if lagging {
		cached = interceptor.NewClient(live, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*infrav1beta1.VirtualMachine); ok {
					return apierrors.NewNotFound(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "virtualmachines"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}
	r := &VMMigrationReconciler{Client: cached, APIReader: live, Scheme: live.Scheme(), Recorder: record.NewFakeRecorder(20)}
	stored := &infrav1beta1.VMMigration{}
	require.NoError(t, live.Get(context.Background(), client.ObjectKeyFromObject(migration), stored))
	return &creatingHarness{r: r, live: live, migration: stored}
}

func (h *creatingHarness) get(t *testing.T) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, h.live.Get(context.Background(), client.ObjectKeyFromObject(h.migration), got))
	return got
}

func (h *creatingHarness) targetVM(t *testing.T) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, h.live.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "target-vm"}, vm))
	return vm
}

// plantedTargetVM is a VirtualMachine under the target's name that the
// migration did not create, carrying every forgeable field a planter can
// copy: the provenance annotation and spec.importedDisk naming the
// migration and its importID.
func plantedTargetVM() *infrav1beta1.VirtualMachine {
	return &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "target-vm", Namespace: "default", UID: "uid-planted",
			Annotations: map[string]string{migrationProvenanceAnnotation: "default/race-migration-import"}},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			ImportedDisk: &infrav1beta1.ImportedDiskRef{DiskID: "disk-1", Source: "migration",
				MigrationRef: &infrav1beta1.LocalObjectReference{Name: "race-migration-import"}},
		},
	}
}

// claimedMigration is the Creating migration with a claim recorded (its
// digest) and no UID: a Create that succeeded whose UID write was lost.
func claimedMigration(t *testing.T) (*infrav1beta1.VMMigration, string) {
	t.Helper()
	targetProvider, migration := importRaceFixture()
	_ = targetProvider
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "disk-1"
	token, digest, err := newMigrationTargetClaim()
	require.NoError(t, err)
	migration.Status.TargetVMClaim = digest
	return migration, token
}

func readyVM(vm *infrav1beta1.VirtualMachine) *infrav1beta1.VirtualMachine {
	vm.Status.ID = "vm-target-1"
	vm.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
		LastTransitionTime: metav1.Now()}}
	return vm
}

func assertFailedNotOwned(t *testing.T, got *infrav1beta1.VMMigration) {
	t.Helper()
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, targetVMNotOwnedMessage, got.Status.Message)
	failed := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, ReasonTargetVMNotOwned, failed.Reason)
	assert.NotEqual(t, "uid-planted", got.Status.TargetVMUID, "another VirtualMachine's UID is never recorded")
	assert.Empty(t, got.Status.TargetVMID, "another VirtualMachine's provider id is never recorded")
}

// TestHandleCreatingPhase_ForeignVMFailsWithoutTouchingIt pins M1: a
// VirtualMachine under the target's name that this migration did not create
// fails the migration — whether the cache shows it (no Create is sent) or
// only the API server does — whatever forgeable provenance it carries, and
// that VirtualMachine is left exactly as it was.
func TestHandleCreatingPhase_ForeignVMFailsWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	claimed, token := claimedMigration(t)
	for name, tc := range map[string]struct {
		migration *infrav1beta1.VMMigration
		vm        func() *infrav1beta1.VirtualMachine
	}{
		"forged provenance and imported disk, nothing recorded": {nil, plantedTargetVM},
		"another UID than the recorded one": {func() *infrav1beta1.VMMigration {
			_, m := importRaceFixture()
			m.Status.Phase = infrav1beta1.MigrationPhaseCreating
			m.Status.ImportID = "disk-1"
			m.Status.TargetVMUID = "uid-own"
			return m
		}(), plantedTargetVM},
		"the claim digest copied as the token": {claimed, func() *infrav1beta1.VirtualMachine {
			vm := plantedTargetVM()
			vm.Annotations[migrationTargetClaimAnnotation] = claimed.Status.TargetVMClaim
			return vm
		}},
		"the right token, but a UID is recorded": {func() *infrav1beta1.VMMigration {
			m := claimed.DeepCopy()
			m.Status.TargetVMUID = "uid-own"
			return m
		}(), func() *infrav1beta1.VirtualMachine {
			vm := plantedTargetVM()
			vm.Annotations[migrationTargetClaimAnnotation] = token
			return vm
		}},
	} {
		for _, lagging := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lagging=%v", name, lagging), func(t *testing.T) {
				var creates atomic.Int32
				funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					creates.Add(1)
					return c.Create(ctx, obj, opts...)
				}}
				objs := []client.Object{tc.vm()}
				if tc.migration != nil {
					objs = append(objs, tc.migration.DeepCopy())
				}
				h := newCreatingHarness(t, lagging, funcs, objs...)
				before := h.targetVM(t)

				_, err := h.r.handleCreatingPhase(ctx, h.migration)
				require.NoError(t, err)
				assertFailedNotOwned(t, h.get(t))
				assert.Zero(t, creates.Load(), "nothing is created")
				after := h.targetVM(t)
				assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the other VirtualMachine is not written")
			})
		}
	}
}

// TestHandleCreatingPhase_BindsItsOwnVMByClaim: a Create that succeeded
// whose UID write was lost (the restart / requeue window) is recognized by
// its claim token — from the cache or, when the cache lags, from the API
// server — and its UID is recorded; it is not created again.
func TestHandleCreatingPhase_BindsItsOwnVMByClaim(t *testing.T) {
	ctx := context.Background()
	for _, lagging := range []bool{false, true} {
		t.Run(fmt.Sprintf("lagging=%v", lagging), func(t *testing.T) {
			migration, token := claimedMigration(t)
			vm := plantedTargetVM()
			vm.UID = "uid-own"
			vm.Annotations[migrationTargetClaimAnnotation] = token
			h := newCreatingHarness(t, lagging, nil, migration, vm)

			res, err := h.r.handleCreatingPhase(ctx, h.migration)
			require.NoError(t, err)
			assert.Equal(t, targetVMWaitInterval, res.RequeueAfter)
			got := h.get(t)
			assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
			assert.Equal(t, "uid-own", got.Status.TargetVMUID)
			assert.Equal(t, targetVMRecordedMessage, got.Status.Message)

			// The next pass, with the UID recorded, writes nothing more.
			rv := got.ResourceVersion
			_, err = h.r.handleCreatingPhase(ctx, got)
			require.NoError(t, err)
			assert.Equal(t, rv, h.get(t).ResourceVersion, "status written only on change")
		})
	}
}

// TestHandleCreatingPhase_OwnReadyVMAdvances: the migration's own VM
// (status.targetVMUID) that is Ready moves the migration to ValidatingTarget
// with its provider id, never with an immediate requeue.
func TestHandleCreatingPhase_OwnReadyVMAdvances(t *testing.T) {
	_, migration := importRaceFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.TargetVMUID = "uid-own"
	vm := readyVM(plantedTargetVM())
	vm.UID = "uid-own"
	h := newCreatingHarness(t, false, nil, migration, vm)

	res, err := h.r.handleCreatingPhase(context.Background(), h.migration)
	require.NoError(t, err)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	got := h.get(t)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidatingTarget, got.Status.Phase)
	assert.Equal(t, "vm-target-1", got.Status.TargetVMID)
}

// TestHandleCreatingPhase_ClaimIsPersistedBeforeCreate: the claim digest is
// in the live status when the Create is sent, the token on the VM hashes to
// it, and the Create's UID is recorded afterwards.
func TestHandleCreatingPhase_ClaimIsPersistedBeforeCreate(t *testing.T) {
	ctx := context.Background()
	var claimAtCreate, tokenOnVM string
	var h *creatingHarness
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if vm, ok := obj.(*infrav1beta1.VirtualMachine); ok {
			m := &infrav1beta1.VMMigration{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(h.migration), m); err != nil {
				return err
			}
			claimAtCreate = m.Status.TargetVMClaim
			tokenOnVM = vm.Annotations[migrationTargetClaimAnnotation]
			vm.UID = "uid-created" // the API server assigns it; the fake client does not
		}
		return c.Create(ctx, obj, opts...)
	}}
	h = newCreatingHarness(t, false, funcs)

	_, err := h.r.handleCreatingPhase(ctx, h.migration)
	require.NoError(t, err)
	require.NotEmpty(t, tokenOnVM)
	assert.Equal(t, migrationTargetClaimDigest(tokenOnVM), claimAtCreate, "the claim is persisted before the Create")
	assert.NotEqual(t, tokenOnVM, claimAtCreate, "only the digest is stored in the VMMigration")
	got := h.get(t)
	assert.Equal(t, "uid-created", got.Status.TargetVMUID)
	assert.Equal(t, claimAtCreate, got.Status.TargetVMClaim)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
}

// TestHandleCreatingPhase_ClaimWriteFailureSendsNoCreate: a claim that is
// not persisted (a status conflict) sends no Create.
func TestHandleCreatingPhase_ClaimWriteFailureSendsNoCreate(t *testing.T) {
	var creates atomic.Int32
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates.Add(1)
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmmigrations"}, obj.GetName(),
				stderrors.New("changed"))
		},
	}
	h := newCreatingHarness(t, false, funcs)

	_, err := h.r.handleCreatingPhase(context.Background(), h.migration)
	require.Error(t, err)
	assert.True(t, isMigrationStatusContention(err))
	assert.Zero(t, creates.Load())
}

// TestHandleCreatingPhase_NameTakenBetweenCheckAndCreateFails: a
// VirtualMachine created under the target's name after the live check (the
// Create answers AlreadyExists) is not this migration's: it fails, and that
// VirtualMachine is not touched.
func TestHandleCreatingPhase_NameTakenBetweenCheckAndCreateFails(t *testing.T) {
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*infrav1beta1.VirtualMachine); ok {
			if err := c.Create(ctx, plantedTargetVM()); err != nil {
				return err
			}
		}
		return c.Create(ctx, obj, opts...)
	}}
	h := newCreatingHarness(t, false, funcs)

	_, err := h.r.handleCreatingPhase(context.Background(), h.migration)
	require.NoError(t, err)
	assertFailedNotOwned(t, h.get(t))
	vm := h.targetVM(t)
	assert.Equal(t, "uid-planted", string(vm.UID))
	assert.NotContains(t, vm.Annotations, migrationTargetClaimAnnotation)
}

// TestHandleCreatingPhase_LostUIDWriteIsRecoveredOnTheNextReconcile: the
// Create succeeds, the status write that records its UID is lost; the next
// reconcile (a fresh read, the cache not showing the VM yet) recognizes the
// VM by its claim, records its UID and creates nothing more.
func TestHandleCreatingPhase_LostUIDWriteIsRecoveredOnTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	var creates, statusWrites atomic.Int32
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if vm, ok := obj.(*infrav1beta1.VirtualMachine); ok {
				creates.Add(1)
				vm.UID = "uid-created"
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if statusWrites.Add(1) == 2 { // the write after the Create
				return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmmigrations"},
					obj.GetName(), stderrors.New("lost"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	h := newCreatingHarness(t, true, funcs)

	_, err := h.r.handleCreatingPhase(ctx, h.migration)
	require.Error(t, err, "the UID write is lost")
	afterLoss := h.get(t)
	require.Empty(t, afterLoss.Status.TargetVMUID)
	require.NotEmpty(t, afterLoss.Status.TargetVMClaim)

	res, err := h.r.handleCreatingPhase(ctx, afterLoss)
	require.NoError(t, err)
	assert.Equal(t, targetVMWaitInterval, res.RequeueAfter)
	got := h.get(t)
	assert.Equal(t, "uid-created", got.Status.TargetVMUID)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	assert.EqualValues(t, 1, creates.Load(), "the VM is not created twice")
}

// TestHandleCreatingPhase_RecordedUIDWhoseVMIsGoneFails (review M-1): the
// VM this migration created is gone (confirmed live); the migration fails
// with TargetVMGone instead of re-creating its target under a new identity
// next to a stale UID.
func TestHandleCreatingPhase_RecordedUIDWhoseVMIsGoneFails(t *testing.T) {
	var creates atomic.Int32
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		creates.Add(1)
		return c.Create(ctx, obj, opts...)
	}}
	_, migration := importRaceFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "disk-1"
	migration.Status.TargetVMUID = "uid-v1"
	h := newCreatingHarness(t, true, funcs, migration)

	_, err := h.r.handleCreatingPhase(context.Background(), h.migration)
	require.NoError(t, err)
	got := h.get(t)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, targetVMGoneMessage, got.Status.Message)
	failed := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, ReasonTargetVMGone, failed.Reason)
	assert.Zero(t, creates.Load(), "nothing is created")
}

// retryableFailed turns m into a Failed migration whose retry is due.
func retryableFailed(m *infrav1beta1.VMMigration) {
	maxRetries := int32(3)
	m.Spec.Options = &infrav1beta1.MigrationOptions{RetryPolicy: &infrav1beta1.MigrationRetryPolicy{MaxRetries: &maxRetries}}
	m.Status.Phase = infrav1beta1.MigrationPhaseFailed
	m.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-2 * migrationMaxRetryDelay)}
}

// TestRetry_ReleasesABindingWhoseVMIsGone_ThenAFreshCreateBinds (review
// M-1): a retry releases the UID and claim of a target VM that no longer
// exists, so the retried migration creates a new VM and binds it — even when
// that Create's UID write is lost (the claim recovers it).
func TestRetry_ReleasesABindingWhoseVMIsGone_ThenAFreshCreateBinds(t *testing.T) {
	ctx := context.Background()
	var statusWrites atomic.Int32
	failNextUIDWrite := atomic.Bool{}
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if vm, ok := obj.(*infrav1beta1.VirtualMachine); ok {
				vm.UID = "uid-v2"
				failNextUIDWrite.Store(true)
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			statusWrites.Add(1)
			if failNextUIDWrite.CompareAndSwap(true, false) {
				return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmmigrations"},
					obj.GetName(), stderrors.New("lost"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	_, migration := importRaceFixture()
	migration.Status.ImportID = "disk-1"
	migration.Status.DiskInfo.TargetPath = "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"
	migration.Status.TargetVMUID = "uid-v1" // V1 was deleted
	migration.Status.TargetVMClaim = migrationTargetClaimDigest("v1-token")
	retryableFailed(migration)
	h := newCreatingHarness(t, true, funcs, migration)

	_, err := h.r.handleFailedPhase(ctx, h.migration)
	require.NoError(t, err)
	got := h.get(t)
	require.Equal(t, infrav1beta1.MigrationPhasePending, got.Status.Phase)
	assert.Empty(t, got.Status.TargetVMUID, "the stale UID is released")
	assert.Empty(t, got.Status.TargetVMClaim, "the stale claim is released")

	// The retried migration reaches Creating again (export/import elided).
	got.Status.Phase = infrav1beta1.MigrationPhaseCreating
	require.NoError(t, h.live.Status().Update(ctx, got))
	_, err = h.r.handleCreatingPhase(ctx, h.get(t))
	require.Error(t, err, "the UID write after the Create is lost")
	_, err = h.r.handleCreatingPhase(ctx, h.get(t))
	require.NoError(t, err)
	got = h.get(t)
	assert.Equal(t, "uid-v2", got.Status.TargetVMUID, "the new VM is bound by its claim")
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
}

// TestRetry_KeepsTheBindingOfAnExistingOwnVM: a retry keeps the binding
// while the bound VM still exists, so the VM stays the migration's (reused by
// the retry, deleted with the migration if it ends failed).
func TestRetry_KeepsTheBindingOfAnExistingOwnVM(t *testing.T) {
	_, migration := importRaceFixture()
	migration.Status.TargetVMUID = "uid-own"
	retryableFailed(migration)
	own := plantedTargetVM()
	own.UID = "uid-own"
	h := newCreatingHarness(t, false, nil, migration, own)

	_, err := h.r.handleFailedPhase(context.Background(), h.migration)
	require.NoError(t, err)
	got := h.get(t)
	assert.Equal(t, infrav1beta1.MigrationPhasePending, got.Status.Phase)
	assert.Equal(t, "uid-own", got.Status.TargetVMUID)
}

// TestValidatingTargetPhase_DecidesOnTheLiveVM (review L-3): the committing
// step re-reads the target VM from the API server. A cache that still shows
// the migration's own Ready VM does not complete the migration when the live
// VM is gone, being deleted, or another object.
func TestValidatingTargetPhase_DecidesOnTheLiveVM(t *testing.T) {
	ctx := context.Background()
	own := func() *infrav1beta1.VirtualMachine {
		vm := readyVM(plantedTargetVM())
		vm.UID = "uid-own"
		return vm
	}
	for name, tc := range map[string]struct {
		live       func() *infrav1beta1.VirtualMachine
		wantReason string
	}{
		"deleted": {nil, ReasonTargetVMGone},
		"being deleted": {func() *infrav1beta1.VirtualMachine {
			vm := own()
			vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
			vm.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			return vm
		}, ReasonTargetVMGone},
		"replaced":        {func() *infrav1beta1.VirtualMachine { return readyVM(plantedTargetVM()) }, ReasonTargetVMNotOwned},
		"own (completes)": {own, "MigrationComplete"},
	} {
		t.Run(name, func(t *testing.T) {
			_, migration := importRaceFixture()
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
			migration.Status.TargetVMUID = "uid-own"
			objs := []client.Object{migration}
			if tc.live != nil {
				objs = append(objs, tc.live())
			}
			stale := own()
			h := newCreatingHarness(t, false, nil, objs...)
			// The reconciler's cache still shows the migration's own Ready VM.
			h.r.Client = interceptor.NewClient(h.live, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if vm, ok := obj.(*infrav1beta1.VirtualMachine); ok {
						stale.DeepCopyInto(vm)
						return nil
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})

			_, err := h.r.handleValidatingTargetPhase(ctx, h.migration)
			require.NoError(t, err)
			got := h.get(t)
			ready := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantReason, ready.Reason)
		})
	}
}

// TestHandleCreatingPhase_AlreadyExistsWithAnotherClaimRequeues (review
// L-2): a VM that took the name between the live check and the Create and
// carries a claim token (only a manager writes one with the admission policy
// on: an overlapping leader's Create) is re-checked, not failed on that
// answer; one without a claim fails.
func TestHandleCreatingPhase_AlreadyExistsWithAnotherClaimRequeues(t *testing.T) {
	for name, tc := range map[string]struct {
		claim     string
		wantPhase infrav1beta1.MigrationPhase
	}{
		"another claim": {"other-leaders-token", infrav1beta1.MigrationPhaseCreating},
		"no claim":      {"", infrav1beta1.MigrationPhaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*infrav1beta1.VirtualMachine); ok {
					racer := plantedTargetVM()
					if tc.claim != "" {
						racer.Annotations[migrationTargetClaimAnnotation] = tc.claim
					}
					if err := c.Create(ctx, racer); err != nil {
						return err
					}
				}
				return c.Create(ctx, obj, opts...)
			}}
			h := newCreatingHarness(t, false, funcs)

			res, err := h.r.handleCreatingPhase(context.Background(), h.migration)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, h.get(t).Status.Phase)
			if tc.wantPhase == infrav1beta1.MigrationPhaseCreating {
				assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
			}
		})
	}
}

// TestHandleCreatingPhase_ReservedTargetLabelIsDroppedWithAnEvent (review
// L-6): a reserved label in spec.target.labels is not put on the target VM,
// other labels are, and one Normal event lists what was dropped.
func TestHandleCreatingPhase_ReservedTargetLabelIsDroppedWithAnEvent(t *testing.T) {
	funcs := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetUID("uid-created")
		return c.Create(ctx, obj, opts...)
	}}
	_, migration := importRaceFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "disk-1"
	migration.Status.DiskInfo.TargetPath = "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"
	migration.Spec.Target.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue, "virtrigaud.io/team": "a"}
	h := newCreatingHarness(t, false, funcs, migration)

	_, err := h.r.handleCreatingPhase(context.Background(), h.migration)
	require.NoError(t, err)
	vm := h.targetVM(t)
	assert.Equal(t, map[string]string{"virtrigaud.io/team": "a"}, vm.Labels)
	rec, ok := h.r.Recorder.(*record.FakeRecorder)
	require.True(t, ok)
	assert.Len(t, filterEvents(drainRecordedEvents(rec), eventReasonTargetLabelsDropped), 1)
}
