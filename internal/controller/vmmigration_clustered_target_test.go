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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Unit tests of the pure helpers of a migration into a clustered provider
// (ADR-0007 Addendum B). The flow itself is pinned in envtest
// (vmmigration_clustered_target_envtest_test.go).

func TestRoutedImportRefusalClassification(t *testing.T) {
	plain := contracts.NewConflictError("importDisk: name taken", nil)
	prevInc := contracts.NewConflictError("importDisk: previous incarnation", contracts.ErrVMPreviousIncarnation)
	inUse := contracts.NewConflictError("importDisk: in use", contracts.ErrVMDiskInUse)
	tooBig := contracts.NewConflictError("importDisk: too big", contracts.ErrDiskSizeExceedsLimit)
	assert.True(t, routedImportNameConflict(plain), "a plain AlreadyExists releases the host")
	for name, err := range map[string]error{"previous incarnation": prevInc, "in use": inUse, "size limit": tooBig,
		"invalid": contracts.NewInvalidSpecError("bad", nil), "nil": nil} {
		assert.False(t, routedImportNameConflict(err), name)
	}

	guard := fmt.Errorf("importDisk failed: %w", status.Error(codes.FailedPrecondition, "lock directory not owned"))
	assert.True(t, routedImportPreconditionRefused(guard), "an untyped FailedPrecondition is a pre-write refusal")
	assert.False(t, routedImportPreconditionRefused(inUse), "a typed refusal is not")
	assert.False(t, routedImportPreconditionRefused(status.Error(codes.Unavailable, "x")))
	assert.False(t, routedImportPreconditionRefused(errors.New("plain")))
}

func TestDiskSizeGiB(t *testing.T) {
	assert.Equal(t, int32(1), diskSizeGiB(0))
	assert.Equal(t, int32(1), diskSizeGiB(1))
	assert.Equal(t, int32(10), diskSizeGiB(10<<30))
	assert.Equal(t, int32(11), diskSizeGiB(10<<30+1), "rounded up")
	assert.Equal(t, int32(math.MaxInt32), diskSizeGiB(math.MaxInt64))
}

func TestAwaitingMigrationImportAndPlacementBytes(t *testing.T) {
	ref := &infravirtrigaudiov1beta1.LocalObjectReference{Name: "mig"}
	vm := func(d *infravirtrigaudiov1beta1.ImportedDiskRef) *infravirtrigaudiov1beta1.VirtualMachine {
		return &infravirtrigaudiov1beta1.VirtualMachine{Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{ImportedDisk: d}}
	}
	assert.True(t, awaitingMigrationImport(vm(&infravirtrigaudiov1beta1.ImportedDiskRef{MigrationRef: ref})))
	assert.False(t, awaitingMigrationImport(vm(&infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "d", MigrationRef: ref})))
	assert.False(t, awaitingMigrationImport(vm(&infravirtrigaudiov1beta1.ImportedDiskRef{})))
	assert.False(t, awaitingMigrationImport(vm(nil)))

	assert.Equal(t, int64(10<<30), importedDiskPlacementBytes(vm(&infravirtrigaudiov1beta1.ImportedDiskRef{MigrationRef: ref, SizeGiB: 10})))
	assert.Zero(t, importedDiskPlacementBytes(vm(&infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "d", SizeGiB: 10})),
		"only a migration target's disk size filters placement")
	assert.Zero(t, importedDiskPlacementBytes(vm(nil)))
}

func TestMigrationForTargetVMAndPlacementPredicate(t *testing.T) {
	annotated := func(pending string, del bool) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "team-b", Annotations: map[string]string{migrationProvenanceAnnotation: "team-a/mig"},
		}}
		if pending != "" {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: pending}
		}
		if del {
			now := metav1.Now()
			vm.DeletionTimestamp = &now
		}
		return vm
	}
	reqs := migrationForTargetVM(context.Background(), annotated("", false))
	if assert.Len(t, reqs, 1) {
		assert.Equal(t, "team-a", reqs[0].Namespace)
		assert.Equal(t, "mig", reqs[0].Name)
	}
	assert.Empty(t, migrationForTargetVM(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{}))

	p := targetPlacementChanged()
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: annotated("", false), ObjectNew: annotated("kvm-01", false)}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: annotated("kvm-01", false), ObjectNew: annotated("kvm-01", false)}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: annotated("kvm-01", false), ObjectNew: annotated("kvm-01", true)}))
	plain := &infravirtrigaudiov1beta1.VirtualMachine{}
	plainPlaced := &infravirtrigaudiov1beta1.VirtualMachine{Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
		Placement: &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "kvm-01"}}}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: plain, ObjectNew: plainPlaced}), "not a migration's VM")
}

func TestVMUpdateNeedsReconcile_ReleasedPendingHostOfAMigrationTarget(t *testing.T) {
	waiting := func(pending string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "mig"}},
		}}
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: pending}
		return vm
	}
	assert.True(t, vmUpdateNeedsReconcile(waiting("kvm-01"), waiting("")), "a released pending host re-places the target at once")
	assert.False(t, vmUpdateNeedsReconcile(waiting(""), waiting("kvm-01")), "its own placement write does not")
	other := func(pending string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: pending}
		return vm
	}
	assert.False(t, vmUpdateNeedsReconcile(other("kvm-01"), other("")), "any other VM keeps its backoff (slice 2)")
}
