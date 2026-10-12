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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the re-review's M-c: a VMSnapshot is bound to the VM (by
// UID) and the Provider it was taken through. A VM re-created under the same
// name, or a VM that now goes through another Provider, never gets a
// SnapshotDelete for this snapshot's id: the VMSnapshot is released with a
// SnapshotLeftOnHypervisor Warning instead.

// snapshotVMWithUID is the provisioned VM "web" on Provider "prov" with uid, created at
// created.
func snapshotVMWithUID(uid string, created time.Time) *infrav1beta1.VirtualMachine {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	vm.UID = types.UID(uid)
	vm.CreationTimestamp = metav1.NewTime(created)
	return vm
}

func TestVMSnapshotBinding_RecordedWithTheCreate(t *testing.T) {
	prov := runningProvider(snapPhaseNS, "prov")
	prov.UID = "prov-uid"
	r, _ := newSnapPhaseReconciler(t, snapshotVMWithUID("vm-uid-1", time.Now().Add(-time.Hour)), prov, pendingSnapshot())
	spy := &snapshotCreateSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }

	snap := reconcileSnapshot(t, r)
	require.Equal(t, infrav1beta1.SnapshotPhaseReady, snap.Status.Phase)
	assert.Equal(t, "vm-uid-1", snap.Status.VMUID)
	require.NotNil(t, snap.Status.BoundProvider)
	assert.Equal(t, infrav1beta1.BoundProviderRef{Namespace: snapPhaseNS, Name: "prov", UID: "prov-uid"}, *snap.Status.BoundProvider)
}

// deletingBoundSnapshot is a Ready, deleting snapshot of the VM with uid
// snapVMUID through Provider snapProvider, next to vm.
func deletingBoundSnapshot(t *testing.T, vm *infrav1beta1.VirtualMachine, snapVMUID, snapProvider string) (*VMSnapshotReconciler, *snapshotDeleteSpy, chan string) {
	t.Helper()
	snap := readySnapshot(time.Minute, nil)
	snap.Status.VMUID = snapVMUID
	if snapProvider != "" {
		snap.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: snapPhaseNS, Name: snapProvider}
	}
	r, rec := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"), snap)
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
	require.NoError(t, r.Delete(context.Background(), snap))
	return r, spy, rec.Events
}

func TestVMSnapshotBinding_DeleteThroughTheSameVMAndProvider(t *testing.T) {
	r, spy, events := deletingBoundSnapshot(t, snapshotVMWithUID("vm-uid-1", time.Now().Add(-time.Hour)), "vm-uid-1", "prov")
	reconcileSnapshotKey(t, r)
	assert.True(t, snapshotGone(t, r))
	assert.EqualValues(t, 1, spy.calls.Load())
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Normal SnapshotDeleted")
}

func TestVMSnapshotBinding_NeverDeletedThroughAnotherVMOrProvider(t *testing.T) {
	for name, tc := range map[string]struct {
		vm           *infrav1beta1.VirtualMachine
		snapVMUID    string
		snapProvider string
	}{
		"VM re-created under the same name": {snapshotVMWithUID("vm-uid-2", time.Now()), "vm-uid-1", "prov"},
		"VM now on another Provider":        {snapshotVMWithUID("vm-uid-1", time.Now().Add(-time.Hour)), "vm-uid-1", "prov-old"},
		"legacy snapshot, VM created after": {snapshotVMWithUID("vm-uid-2", time.Now()), "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, spy, events := deletingBoundSnapshot(t, tc.vm, tc.snapVMUID, tc.snapProvider)
			reconcileSnapshotKey(t, r)
			assert.Zero(t, spy.calls.Load(), "no SnapshotDelete through another VM or Provider")
			assert.True(t, snapshotGone(t, r), "released")
			evs := strings.Join(eventsOf(events), "\n")
			assert.Contains(t, evs, "Warning "+eventReasonSnapshotLeftOnHypervisor)
			assert.NotContains(t, evs, "Normal SnapshotDeleted")
		})
	}
}

func TestVMSnapshotBinding_RetentionTreatsAReCreatedVMAsGone(t *testing.T) {
	snap := readySnapshot(time.Minute, &infrav1beta1.SnapshotRetentionPolicy{DeleteOnVMDelete: true})
	snap.Status.VMUID = "vm-uid-1"
	r, _ := newSnapPhaseReconciler(t, snapshotVMWithUID("vm-uid-2", time.Now()), runningProvider(snapPhaseNS, "prov"), snap)
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }

	reconcileSnapshotKey(t, r) // retention: the snapshot's VM is gone
	reconcileSnapshotKey(t, r) // finalizer: released without a provider call
	assert.True(t, snapshotGone(t, r))
	assert.Zero(t, spy.calls.Load())
}

func TestVMSnapshotBinding_LegacySnapshotIsBoundToTheVMThatPredatesIt(t *testing.T) {
	vm := snapshotVMWithUID("vm-uid-1", time.Now().Add(-24*time.Hour))
	vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: snapPhaseNS, Name: "prov", UID: "prov-uid"}
	r, _ := newSnapPhaseReconciler(t, vm, readySnapshot(time.Hour, nil))

	snap := reconcileSnapshot(t, r)
	assert.Equal(t, "vm-uid-1", snap.Status.VMUID, "a VM created before the snapshot is its VM")
	require.NotNil(t, snap.Status.BoundProvider)
	assert.Equal(t, "prov", snap.Status.BoundProvider.Name)
}
