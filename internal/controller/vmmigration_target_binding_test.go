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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func TestNewMigrationTargetClaim(t *testing.T) {
	token, digest, err := newMigrationTargetClaim()
	require.NoError(t, err)
	assert.Len(t, token, 2*migrationTargetClaimTokenBytes)
	assert.Len(t, digest, 64, "fits status.targetVMClaim (MaxLength=64)")
	assert.NotEqual(t, token, digest)
	assert.Equal(t, digest, migrationTargetClaimDigest(token))
	token2, digest2, err := newMigrationTargetClaim()
	require.NoError(t, err)
	assert.NotEqual(t, token, token2, "every claim is fresh")
	assert.NotEqual(t, digest, digest2)
}

func TestMigrationOwnsTargetVM(t *testing.T) {
	token, digest, err := newMigrationTargetClaim()
	require.NoError(t, err)
	vm := func(uid, claim string) *infrav1beta1.VirtualMachine {
		v := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: types.UID(uid)}}
		if claim != "" {
			v.Annotations = map[string]string{migrationTargetClaimAnnotation: claim}
		}
		return v
	}
	m := func(uid, claim string) *infrav1beta1.VMMigration {
		return &infrav1beta1.VMMigration{Status: infrav1beta1.VMMigrationStatus{TargetVMUID: uid, TargetVMClaim: claim}}
	}
	for name, tc := range map[string]struct {
		m    *infrav1beta1.VMMigration
		vm   *infrav1beta1.VirtualMachine
		want bool
	}{
		"recorded UID matches":                     {m("u1", ""), vm("u1", ""), true},
		"recorded UID differs":                     {m("u1", ""), vm("u2", ""), false},
		"recorded UID wins over a matching claim":  {m("u1", digest), vm("u2", token), false},
		"no UID, claim token matches":              {m("", digest), vm("u2", token), true},
		"no UID, claim token differs":              {m("", digest), vm("u2", "other"), false},
		"no UID, the digest is not the token":      {m("", digest), vm("u2", digest), false},
		"no UID, no claim recorded":                {m("", ""), vm("u2", token), false},
		"nothing recorded, nothing on the VM":      {m("", ""), vm("u2", ""), false},
		"a VM without a UID (never from a server)": {m("", digest), vm("", token), false},
		"nil VM": {m("u1", ""), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, migrationOwnsTargetVM(tc.m, tc.vm))
		})
	}
}

// TestValidatingPhase_ForeignTargetFailsBeforeAnySideEffect: a VirtualMachine
// under the target's name that the migration did not create fails it at
// Validating — before the source is powered off, a snapshot taken or
// anything exported — with the constant message, and is not touched.
func TestValidatingPhase_ForeignTargetFailsBeforeAnySideEffect(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Source.PowerOffBeforeMigration = true
	planted := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: xnsSource, UID: "uid-planted",
			Annotations: map[string]string{migrationProvenanceAnnotation: xnsSource + "/mig"}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "tgt-prov"},
			ImportedDisk: &infrav1beta1.ImportedDiskRef{Source: "migration",
				MigrationRef: &infrav1beta1.LocalObjectReference{Name: "mig"}}},
	}
	prov := &powerSpyProvider{}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration, planted)...)
	before := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(planted), before))

	_, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assertFailedNotOwned(t, got)
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, metav1.ConditionFalse, validating.Status)
	assert.Equal(t, ReasonTargetVMNotOwned, validating.Reason)
	assert.Empty(t, prov.ops(), "the source is not powered off")
	assert.Empty(t, got.Status.SourceVMUID, "validation did not complete")
	after := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(planted), after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
}

// TestValidatingPhase_OwnTargetFromAnEarlierAttemptProceeds: a retried
// migration whose earlier attempt created the target (status.targetVMUID)
// passes the check.
func TestValidatingPhase_OwnTargetFromAnEarlierAttemptProceeds(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Source.PowerOffBeforeMigration = true
	migration.Status.TargetVMUID = "uid-own"
	own := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: xnsSource, UID: "uid-own"}}
	prov := &powerSpyProvider{}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration, own)...)

	res, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, migrationPowerOffPollInterval, res.RequeueAfter, "on to the source power-off wait")
	assert.Equal(t, infrav1beta1.MigrationPhaseValidating, getXNSMigration(t, r, migration).Status.Phase)
}

// TestValidatingTargetPhase_OnlyItsOwnVM: Validating-Target completes only
// with the VirtualMachine the migration created; another one under the
// target's name fails it and is never marked migration-completed; a VM
// recognized by its claim gets its UID recorded; a VM that is gone fails it.
func TestValidatingTargetPhase_OnlyItsOwnVM(t *testing.T) {
	ctx := context.Background()
	token, digest, err := newMigrationTargetClaim()
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		uid, claim string
		vm         func() *infrav1beta1.VirtualMachine
		wantPhase  infrav1beta1.MigrationPhase
		wantReason string
	}{
		"planted, nothing recorded": {"", "", func() *infrav1beta1.VirtualMachine { return readyTargetVM("default") },
			infrav1beta1.MigrationPhaseFailed, ReasonTargetVMNotOwned},
		"another UID than the recorded one": {"uid-other", "", func() *infrav1beta1.VirtualMachine { return readyTargetVM("default") },
			infrav1beta1.MigrationPhaseFailed, ReasonTargetVMNotOwned},
		"own, by its claim": {"", digest, func() *infrav1beta1.VirtualMachine {
			vm := readyTargetVM("default")
			vm.Annotations[migrationTargetClaimAnnotation] = token
			return vm
		}, infrav1beta1.MigrationPhaseReady, "MigrationComplete"},
		"gone": {readyTargetVMUID, "", nil, infrav1beta1.MigrationPhaseFailed, ReasonTargetVMGone},
	} {
		t.Run(name, func(t *testing.T) {
			sourceVM, sourceProvider, targetProvider, migration := creatingFixture("default")
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
			migration.Status.TargetVMUID = tc.uid
			migration.Status.TargetVMClaim = tc.claim
			objs := []client.Object{sourceVM, sourceProvider, targetProvider, migration}
			if tc.vm != nil {
				objs = append(objs, tc.vm())
			}
			r, _ := directionReconciler(t, &capturingMigrationProvider{}, objs...)

			_, err := r.handleValidatingTargetPhase(ctx, migration)
			require.NoError(t, err)
			got := getXNSMigration(t, r, migration)
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			ready := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantReason, ready.Reason)
			if tc.vm == nil {
				assert.Equal(t, targetVMGoneMessage, got.Status.Message)
				return
			}
			vm := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target-vm"}, vm))
			if tc.wantPhase == infrav1beta1.MigrationPhaseReady {
				assert.Equal(t, readyTargetVMUID, got.Status.TargetVMUID, "the claimed VM's UID is recorded")
				assert.Equal(t, "true", vm.Annotations["virtrigaud.io/migration-completed"])
				return
			}
			assert.Equal(t, targetVMNotOwnedMessage, got.Status.Message)
			assert.NotContains(t, vm.Annotations, "virtrigaud.io/migration-completed", "another VM is never marked")
			assert.Empty(t, got.Status.TargetVMID)
		})
	}
}

// TestHandleDeletion_LeavesAVMItDidNotCreate: deleting a migration that
// failed in Creating never deletes a VirtualMachine under the target's name
// it did not create, whatever provenance annotation that VM carries.
func TestHandleDeletion_LeavesAVMItDidNotCreate(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture("default")
	migration.Spec.Storage = nil
	migration.Finalizers = []string{infrav1beta1.VMMigrationFinalizer}
	planted := readyTargetVM("default")
	planted.Annotations[migrationProvenanceAnnotation] = migration.Namespace + "/" + migration.Name
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration, planted)
	require.NoError(t, r.Delete(ctx, migration))

	_, err := r.handleDeletion(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(planted), &infrav1beta1.VirtualMachine{}), "the VM is kept")
	err = r.Get(ctx, client.ObjectKeyFromObject(migration), &infrav1beta1.VMMigration{})
	assert.True(t, apierrors.IsNotFound(err), "the finalizer is released")
}

// TestHandleCreatingPhase_StaleMigrationCacheIsNotAFailure: a reconcile that
// read the VMMigration before the previous reconcile's claim write sees the
// migration's own VM as unclaimed; it must retry, not fail.
func TestHandleCreatingPhase_StaleMigrationCacheIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	migration, token := claimedMigration(t)
	vm := plantedTargetVM()
	vm.UID = "uid-own"
	vm.Annotations[migrationTargetClaimAnnotation] = token
	h := newCreatingHarness(t, false, nil, migration, vm)
	stale := h.migration.DeepCopy()
	stale.Status.TargetVMClaim = "" // as read before the claim write
	live := h.get(t)
	live.Status.Message = "advanced"
	require.NoError(t, h.live.Status().Update(ctx, live))

	res, err := h.r.handleCreatingPhase(ctx, stale)
	require.NoError(t, err)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, h.get(t).Status.Phase)
}

// TestIsOwnMigrationDisk_RequiresTheMigrationsOwnVM: the landing disk is
// attached in place only for the VirtualMachine the migration created. A
// cache that does not show the claim yet (it is written before the Create)
// is answered from the API server.
func TestIsOwnMigrationDisk_RequiresTheMigrationsOwnVM(t *testing.T) {
	ctx := context.Background()
	token, digest, err := newMigrationTargetClaim()
	require.NoError(t, err)
	claimed := landingMigration("m1", "ns-a")
	claimed.Status.TargetVMUID = ""
	claimed.Status.TargetVMClaim = digest
	staleCopy := claimed.DeepCopy()
	staleCopy.Status.TargetVMClaim = ""
	vm := baseVM("ns-a")
	vm.Name = "web"
	vm.UID = "uid-own"
	vm.Annotations = map[string]string{migrationTargetClaimAnnotation: token}
	vm.Spec.ImportedDisk = &infrav1beta1.ImportedDiskRef{Path: importedDiskPath, Source: "migration",
		MigrationRef: &infrav1beta1.LocalObjectReference{Name: "m1"}}
	s := coverageTestScheme(t)

	r := newTestReconciler(s, nil, staleCopy)
	own, err := r.isOwnMigrationDisk(ctx, vm, importedDiskPath)
	require.NoError(t, err)
	assert.False(t, own, "without an API reader the stale cache decides")

	r.APIReader = fake.NewClientBuilder().WithScheme(s).WithObjects(claimed).Build()
	own, err = r.isOwnMigrationDisk(ctx, vm, importedDiskPath)
	require.NoError(t, err)
	assert.True(t, own, "the live migration's claim proves the VM")

	planted := vm.DeepCopy()
	planted.UID = "uid-planted"
	planted.Annotations = nil
	own, err = r.isOwnMigrationDisk(ctx, planted, importedDiskPath)
	require.NoError(t, err)
	assert.False(t, own, "a same-named VM with a copied spec.importedDisk never gets the landing disk")
}

// TestBuildCreateRequest_PlantedVMDoesNotGetTheLandingDisk exercises the
// same rule through buildCreateRequest.
func TestBuildCreateRequest_PlantedVMDoesNotGetTheLandingDisk(t *testing.T) {
	vmClass := &infrav1beta1.VMClass{Spec: infrav1beta1.VMClassSpec{CPU: 1, Memory: resource.MustParse("1Gi")}}
	planted := baseVM("ns-a")
	planted.Name = "web"
	planted.UID = "uid-planted"
	planted.Spec.ImportedDisk = &infrav1beta1.ImportedDiskRef{Path: importedDiskPath, Source: "migration",
		MigrationRef: &infrav1beta1.LocalObjectReference{Name: "m1"}}
	r := newTestReconciler(coverageTestScheme(t), nil, landingMigration("m1", "ns-a"))
	req, err := r.buildCreateRequest(context.Background(), planted, nil, vmClass, nil, nil)
	require.NoError(t, err)
	assert.False(t, req.Image.ImportedDisk)
}
