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

// creatingWithLaggingCache stages a Creating migration whose target VM
// "target-vm" already exists in the API server (vm) while the reconciler's
// cache does not show it yet: the cached Get answers NotFound, the live
// reader sees it.
func creatingWithLaggingCache(t *testing.T, vm *infrav1beta1.VirtualMachine) (*VMMigrationReconciler, client.Client, *infrav1beta1.VMMigration) {
	t.Helper()
	targetProvider, migration := importRaceFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "disk-1"
	migration.Status.DiskInfo.TargetDiskID = "disk-1"
	live := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(targetProvider, migration, vm).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	cached := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*infrav1beta1.VirtualMachine); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "virtualmachines"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &VMMigrationReconciler{Client: cached, APIReader: live, Scheme: live.Scheme(), Recorder: record.NewFakeRecorder(20)}
	return r, live, migration
}

// creatingTargetVM is the VirtualMachine race-migration-import created.
func creatingTargetVM() *infrav1beta1.VirtualMachine {
	return &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "target-vm", Namespace: "default",
			Annotations: map[string]string{migrationProvenanceAnnotation: "default/race-migration-import"}},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			ImportedDisk: &infrav1beta1.ImportedDiskRef{DiskID: "disk-1", Source: "migration",
				MigrationRef: &infrav1beta1.LocalObjectReference{Name: "race-migration-import"}},
		},
	}
}

// TestHandleCreatingPhase_AlreadyExistsForItsOwnVMIsNotAFailure pins that a
// Create answering AlreadyExists for the VM this migration created (an
// earlier Create the cache did not show yet) is that VM, not a failure.
func TestHandleCreatingPhase_AlreadyExistsForItsOwnVMIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	r, c, migration := creatingWithLaggingCache(t, creatingTargetVM())

	res, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, res.RequeueAfter)
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(migration), got))
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
}

// TestHandleCreatingPhase_AlreadyExistsForAnotherVMStillFails pins that a
// VirtualMachine of the target's name that this migration did not create
// still fails the migration.
func TestHandleCreatingPhase_AlreadyExistsForAnotherVMStillFails(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*infrav1beta1.VirtualMachine){
		"no provenance":         func(vm *infrav1beta1.VirtualMachine) { delete(vm.Annotations, migrationProvenanceAnnotation) },
		"another migration":     func(vm *infrav1beta1.VirtualMachine) { vm.Annotations[migrationProvenanceAnnotation] = "default/other" },
		"another imported disk": func(vm *infrav1beta1.VirtualMachine) { vm.Spec.ImportedDisk.DiskID = "disk-9" },
		"no imported disk":      func(vm *infrav1beta1.VirtualMachine) { vm.Spec.ImportedDisk = nil },
	} {
		t.Run(name, func(t *testing.T) {
			vm := creatingTargetVM()
			mutate(vm)
			r, c, migration := creatingWithLaggingCache(t, vm)

			_, err := r.handleCreatingPhase(ctx, migration)
			require.NoError(t, err)
			got := &infrav1beta1.VMMigration{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(migration), got))
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
		})
	}
}
