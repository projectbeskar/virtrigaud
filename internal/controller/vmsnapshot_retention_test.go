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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a Ready snapshot's retention policy applies once its
// VM is gone (it used to be unreachable: Reconcile returned at the failed VM
// read), and that a missing VM is recorded with a constant message.

// readySnapshot is pendingSnapshot, Ready since createdAgo, with retention.
func readySnapshot(createdAgo time.Duration, retention *infrav1beta1.SnapshotRetentionPolicy) *infrav1beta1.VMSnapshot {
	snap := pendingSnapshot()
	snap.Spec.RetentionPolicy = retention
	snap.Status.Phase = infrav1beta1.SnapshotPhaseReady
	snap.Status.SnapshotID = "snap-1"
	snap.Status.CreationTime = &metav1.Time{Time: time.Now().Add(-createdAgo)}
	return snap
}

// snapshotGone reports whether the VMSnapshot no longer exists.
func snapshotGone(t *testing.T, r *VMSnapshotReconciler) bool {
	t.Helper()
	err := r.Get(context.Background(), types.NamespacedName{Namespace: snapPhaseNS, Name: snapPhaseName}, &infrav1beta1.VMSnapshot{})
	if apierrors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err)
	return false
}

// reconcileSnapshotKey runs one reconcile of the VMSnapshot under test.
func reconcileSnapshotKey(t *testing.T, r *VMSnapshotReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: snapPhaseNS, Name: snapPhaseName}})
	require.NoError(t, err)
}

func TestVMSnapshotRetention_AppliesOnceTheVMIsGone(t *testing.T) {
	for name, retention := range map[string]*infrav1beta1.SnapshotRetentionPolicy{
		"deleteOnVMDelete": {DeleteOnVMDelete: true},
		"maxAge expired":   {MaxAge: &metav1.Duration{Duration: time.Hour}},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := newSnapPhaseReconciler(t, readySnapshot(2*time.Hour, retention))
			spy := &snapshotDeleteSpy{}
			r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }

			reconcileSnapshotKey(t, r) // retention: the VMSnapshot is deleted
			reconcileSnapshotKey(t, r) // finalizer: the VM is gone, nothing to call
			assert.True(t, snapshotGone(t, r))
			assert.Zero(t, spy.calls.Load(), "the snapshot went with its VM: no provider call")
		})
	}
}

func TestVMSnapshotRetention_KeepsASnapshotWithoutAPolicy(t *testing.T) {
	r, _ := newSnapPhaseReconciler(t, readySnapshot(2*time.Hour, nil))
	reconcileSnapshotKey(t, r)
	assert.False(t, snapshotGone(t, r))
}

// TestVMSnapshotRetention_VMBeingDeleted: deleteOnVMDelete fires while the VM
// is being deleted, so the snapshot is deleted through the provider before
// the VM goes (a libvirt VM's delete is refused while snapshot metadata
// remains).
func TestVMSnapshotRetention_VMBeingDeleted(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
	now := metav1.Now()
	vm.DeletionTimestamp = &now
	r, _ := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"),
		readySnapshot(time.Minute, &infrav1beta1.SnapshotRetentionPolicy{DeleteOnVMDelete: true}))
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }

	reconcileSnapshotKey(t, r)
	reconcileSnapshotKey(t, r)
	assert.True(t, snapshotGone(t, r))
	assert.EqualValues(t, 1, spy.calls.Load(), "deleted through the provider while the VM still exists")
}

// TestVMSnapshotRetention_VMWithoutProviderID: a Ready snapshot whose VM has
// no provider id (yet) still expires.
func TestVMSnapshotRetention_VMWithoutProviderID(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "")
	r, _ := newSnapPhaseReconciler(t, vm, readySnapshot(2*time.Hour, &infrav1beta1.SnapshotRetentionPolicy{MaxAge: &metav1.Duration{Duration: time.Hour}}))
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		return &snapshotDeleteSpy{}, nil
	}

	reconcileSnapshotKey(t, r)
	reconcileSnapshotKey(t, r)
	assert.True(t, snapshotGone(t, r))
}

func TestVMSnapshot_MissingVMIsRecordedWithAConstantMessage(t *testing.T) {
	r, _ := newSnapPhaseReconciler(t, pendingSnapshot())

	snap := reconcileSnapshot(t, r)
	c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, snapshotReasonVMNotFound, c.Reason)
	assert.Equal(t, snapshotVMNotFoundMessage, c.Message)
	assert.NotContains(t, c.Message, "virtualmachines.infra.virtrigaud.io", "no API error text")

	rv := snap.ResourceVersion
	again := reconcileSnapshot(t, r)
	assert.Equal(t, rv, again.ResourceVersion, "a re-check writes nothing")
}
