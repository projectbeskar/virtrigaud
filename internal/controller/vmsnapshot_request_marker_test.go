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
	"sync/atomic"
	"testing"
	"time"

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

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMSnapshot's durable SnapshotRequested marker (rc2 R6).

// statusWriteFaults selects which VMSnapshot status writes fail.
type statusWriteFaults struct {
	// all fails every status write; results fails every write that does not
	// carry the marker (the write of the create's outcome).
	all, results atomic.Bool
}

// markerSnapshotFixture is a VMSnapshot of vm on Provider prov-c (clustered
// when clustered), over a fake client whose status writes fail per faults.
func markerSnapshotFixture(t *testing.T, clustered bool, faults *statusWriteFaults) (*VMSnapshotReconciler, *snapshotRecorder, *infrav1beta1.VMSnapshot) {
	t.Helper()
	vm := boundSource()
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = infrav1beta1.ProviderTopologySingle
	if clustered {
		prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	} else {
		vm.Status.Placement = nil
	}
	snap := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-c", Namespace: "default", UID: "uid-snap-c",
			Finalizers: []string{infrav1beta1.VMSnapshotFinalizer}},
		Spec: infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: vm.Name}},
	}
	s := cloneTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, vm, snap).
		WithStatusSubresource(&infrav1beta1.VMSnapshot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sn, ok := obj.(*infrav1beta1.VMSnapshot); ok &&
					(faults.all.Load() || (faults.results.Load() && !snapshotRequested(sn))) {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	rec := &snapshotRecorder{}
	r := &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return rec, nil }
	return r, rec, snap
}

func reconcileSnapshotOnce(t *testing.T, r *VMSnapshotReconciler, snap *infrav1beta1.VMSnapshot) (ctrl.Result, *infrav1beta1.VMSnapshot) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.NoError(t, err)
	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	return res, got
}

// restartedSnapshotReconciler is r after a manager restart.
func restartedSnapshotReconciler(r *VMSnapshotReconciler) *VMSnapshotReconciler {
	return &VMSnapshotReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: record.NewFakeRecorder(20),
		providerInstanceFn: r.providerInstanceFn}
}

// TestVMSnapshotRequest_MarkerWriteFailureSendsNothing: no SnapshotCreate is
// sent unless SnapshotRequested was stored first.
func TestVMSnapshotRequest_MarkerWriteFailureSendsNothing(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.all.Store(true)
	r, rec, snap := markerSnapshotFixture(t, false, faults)
	res, got := reconcileSnapshotOnce(t, r, snap)
	assert.Empty(t, rec.creates)
	assert.Equal(t, snapshotStaleReadRetryInterval, res.RequeueAfter)
	assert.False(t, snapshotRequested(got))
}

// TestVMSnapshotRequest_RestartAfterCall_SingleHostIsHeld: a single-host
// create whose outcome was lost is never sent again (a second snapshot, or a
// refused name, on the VM): the VMSnapshot waits SnapshotOutcomeUnknown, with
// one Warning event naming the snapshot, until an operator confirms the
// attempt; then it is created.
func TestVMSnapshotRequest_RestartAfterCall_SingleHostIsHeld(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, false, faults)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, err, "the create's outcome could not be written")
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	events := record.NewFakeRecorder(20)
	r2.Recorder = events
	res, got := reconcileSnapshotOnce(t, r2, snap)
	assert.Len(t, rec.creates, 1, "not sent again")
	assert.Equal(t, requestMarkerRecheckInterval, res.RequeueAfter)
	assert.Equal(t, infrav1beta1.SnapshotPhase(""), got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, snapshotReasonOutcomeUnknown, ready.Reason)
	assert.Equal(t, snapshotOutcomeUnknownMessage, ready.Message)
	ev := drainRecordedEvents(events)
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], "Warning "+snapshotReasonOutcomeUnknown)
	assert.Contains(t, ev[0], `"snap-c"`)

	rv := got.ResourceVersion
	_, got = reconcileSnapshotOnce(t, r2, snap)
	assert.Equal(t, rv, got.ResourceVersion, "a repeated hold writes nothing")

	got.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
		requestMarkerAttemptID(got.Status.Conditions, snapshotConditionRequested)}
	require.NoError(t, r2.Update(context.Background(), got))
	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2, "created on the operator's confirmation")
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
	assert.Equal(t, "snap-1", got.Status.SnapshotID)
	assert.False(t, snapshotRequested(got))
	assert.NotContains(t, got.Annotations, operationOutcomeAnnotation)
}

// TestVMSnapshotRequest_RestartAfterCall_RoutedIsResent: a routed create is
// sent again with the same request token (the provider adopts the snapshot the
// first call made), paced from the last request.
func TestVMSnapshotRequest_RestartAfterCall_RoutedIsResent(t *testing.T) {
	faults := &statusWriteFaults{}
	faults.results.Store(true)
	r, rec, snap := markerSnapshotFixture(t, true, faults)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.Error(t, err)
	require.Len(t, rec.creates, 1)
	faults.results.Store(false)

	r2 := restartedSnapshotReconciler(r)
	res, _ := reconcileSnapshotOnce(t, r2, snap)
	assert.Len(t, rec.creates, 1, "paced from the last request")
	assert.Positive(t, res.RequeueAfter)

	got := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r2.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotConditionRequested)
	require.NotNil(t, cond)
	cond.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	require.NoError(t, r2.Status().Update(context.Background(), got))

	_, got = reconcileSnapshotOnce(t, r2, snap)
	require.Len(t, rec.creates, 2)
	assert.Equal(t, rec.creates[0].RequestToken, rec.creates[1].RequestToken)
	assert.Equal(t, "host-alpha", rec.creates[1].VM.HostID)
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
	assert.False(t, snapshotRequested(got))
}

// TestVMSnapshotRequest_FailedCreateClearsTheMarker: a definite failure is
// the outcome: the marker is removed with it.
func TestVMSnapshotRequest_FailedCreateClearsTheMarker(t *testing.T) {
	r, rec, snap := markerSnapshotFixture(t, false, &statusWriteFaults{})
	rec.createErr = contracts.NewInvalidSpecError("bad name", nil)
	_, got := reconcileSnapshotOnce(t, r, snap)
	require.Len(t, rec.creates, 1)
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase)
	assert.False(t, snapshotRequested(got))
}
