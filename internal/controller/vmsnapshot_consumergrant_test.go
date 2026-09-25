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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// These tests pin the VMSnapshot controller's enforcement of the Provider's
// spec.consumerNamespaceSelector: a snapshot of a VM whose Provider lives in
// another namespace that does not select the VM's is refused on create, on
// task poll and on delete, without resolving the Provider to a client.

// snapConsumerFixture returns a bound VM in bpNS whose Provider is
// cgOwnerNS/shared (with sel), and a snapshot of it.
func snapConsumerFixture(sel *metav1.LabelSelector) (*infrav1beta1.VirtualMachine, *infrav1beta1.VMSnapshot, []client.Object) {
	vm := sourceVMWithID(bpNS, "web", "shared", "vm-100")
	vm.Spec.ProviderRef.Namespace = cgOwnerNS
	vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: cgOwnerNS, Name: "shared"}
	snap := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: bpNS, Generation: 2},
		Spec:       infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: "web"}},
	}
	return vm, snap, []client.Object{
		labeledNamespace(bpNS, nil), labeledNamespace(cgOwnerNS, nil),
		grantedProvider(cgOwnerNS, "shared", sel), vm,
	}
}

// newSnapConsumerReconciler has no RemoteResolver: resolving any Provider to a
// client fails with a ProviderError, so a ConsumerNotAllowed outcome proves no
// resolution was attempted.
func newSnapConsumerReconciler(t *testing.T, objs ...client.Object) (*VMSnapshotReconciler, *record.FakeRecorder) {
	t.Helper()
	s := coverageTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMSnapshot{}).Build()
	rec := record.NewFakeRecorder(10)
	return &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: rec}, rec
}

func requireSnapshotRefused(t *testing.T, snap *infrav1beta1.VMSnapshot) {
	t.Helper()
	c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonConsumerNotAllowed, c.Reason)
	assert.Equal(t, snap.Generation, c.ObservedGeneration)
	assert.Contains(t, c.Message, consumerKindProvider+" "+cgOwnerNS+"/shared")
}

func TestVMSnapshot_CreateRefusesUngrantedProvider(t *testing.T) {
	vm, snap, objs := snapConsumerFixture(nil)
	r, rec := newSnapConsumerReconciler(t, append(objs, snap)...)

	res, err := r.createSnapshot(context.Background(), snap, vm)
	require.NoError(t, err)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	requireSnapshotRefused(t, snap)
	assert.Empty(t, snap.Status.Phase, "the create is retried unchanged once granted")
	assert.Nil(t, meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating),
		"nothing is reported as started")
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+k8s.ReasonConsumerNotAllowed)

	// A recheck does not repeat the event.
	_, err = r.createSnapshot(context.Background(), snap, vm)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec))
}

func TestVMSnapshot_CreateWithGrantReachesTheProvider(t *testing.T) {
	vm, snap, objs := snapConsumerFixture(sharedWithAll())
	r, _ := newSnapConsumerReconciler(t, append(objs, snap)...)

	_, err := r.createSnapshot(context.Background(), snap, vm)
	require.NoError(t, err)
	c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, infrav1beta1.VMSnapshotReasonProviderError, c.Reason,
		"a granted Provider is resolved (and fails here only because the test has no resolver)")
	assert.Contains(t, c.Message, "no remote resolver")
}

func TestVMSnapshot_TaskPollRefusesUngrantedProvider(t *testing.T) {
	vm, snap, objs := snapConsumerFixture(sharedWithNamespace("team-b"))
	snap.Status.Phase = infrav1beta1.SnapshotPhaseCreating
	snap.Status.TaskRef = "task-1"
	r, _ := newSnapConsumerReconciler(t, append(objs, snap)...)

	res, err := r.checkSnapshotCreation(context.Background(), snap, vm)
	require.NoError(t, err)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	requireSnapshotRefused(t, snap)
	assert.Equal(t, infrav1beta1.SnapshotPhaseCreating, snap.Status.Phase, "an in-flight snapshot keeps its phase")
	assert.Equal(t, "task-1", snap.Status.TaskRef)
}

func TestVMSnapshot_DeleteNeverUsesUngrantedProvider(t *testing.T) {
	ctx := context.Background()
	_, snap, objs := snapConsumerFixture(nil)
	snap.Finalizers = []string{"snapshot.infra.virtrigaud.io/finalizer"}
	snap.Status.SnapshotID = "snap-1"
	r, rec := newSnapConsumerReconciler(t, append(objs, snap)...)
	require.NoError(t, r.Delete(ctx, snap))
	marked := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), marked))

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	// Resolving the Provider would have failed (no resolver) and retained the
	// finalizer; the snapshot is gone, so no resolution was attempted.
	err = r.Get(ctx, client.ObjectKeyFromObject(snap), &infrav1beta1.VMSnapshot{})
	assert.True(t, apierrors.IsNotFound(err), "the best-effort delete releases the finalizer")
	events := strings.Join(drainEvents(rec), "\n")
	assert.Contains(t, events, "SnapshotDeleteFailed")
	assert.Contains(t, events, consumerNamespaceSelectorField)
}
