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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMSnapshot controller's side of ADR-0007 Addendum A,
// slice 3: a snapshot of a clustered VM is sent to the VM's bound host with
// the VM's owner (which the provider checks against the domain's stamp), and a
// VM with no confirmed binding — including one whose create is only pending —
// is never sent a snapshot call: the snapshot waits with a condition.

// snapshotRecorder records the snapshot calls it receives; createErr, when
// set, is what SnapshotCreate answers.
type snapshotRecorder struct {
	stubProvider
	creates   []contracts.SnapshotCreateRequest
	deletes   []contracts.VMRef
	createErr error
}

func (p *snapshotRecorder) SnapshotCreate(_ context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.creates = append(p.creates, req)
	if p.createErr != nil {
		return contracts.SnapshotCreateResponse{}, p.createErr
	}
	return contracts.SnapshotCreateResponse{SnapshotId: "snap-1"}, nil
}

// TestVMSnapshot_Clustered_OutcomeUnknownIsRetriedNotFailed: a routed create
// that did not reach a definite outcome (retryable: its budget ran out while
// libvirtd may still write the snapshot, another job still runs, its host is
// unavailable, or #358's disk-dependents check could not run) leaves the
// snapshot in its initial phase to be retried — the provider's routed create
// is idempotent per request (request_token) — instead of failing it and
// leaving a snapshot that completes later untracked. A definite failure —
// among them #358's refusal because another VM depends on the disk — still
// fails it.
func TestVMSnapshot_Clustered_OutcomeUnknownIsRetriedNotFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantPhase infrav1beta1.SnapshotPhase
	}{
		"outcome unknown":  {contracts.NewRetryableError("create snapshot: its outcome is unknown", nil), ""},
		"host unavailable": {contracts.NewHostUnavailableError("create snapshot: host unavailable", nil), ""},
		"definite failure": {contracts.NewInvalidSpecError("create snapshot: bad name", nil), infrav1beta1.SnapshotPhaseFailed},
		// The transport's forms of #358's answers (VM_DISK_CHECK_FAILED is
		// Unavailable -> Retryable; VM_DISK_IN_USE is FailedPrecondition ->
		// Conflict wrapping ErrVMDiskInUse).
		"disk check failed": {contracts.NewRetryableError(
			`create snapshot: snapshot create of libvirt domain "web" not performed: could not verify that no other domain uses its disks`, nil), ""},
		"disk in use": {contracts.NewConflictError(
			`create snapshot: snapshot create of libvirt domain "web" refused: its disk is the backing file (or a disk) of 1 other domain(s)`,
			contracts.ErrVMDiskInUse), infrav1beta1.SnapshotPhaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			r, rec, snap := clusteredSnapshotFixture(t, boundSource(), infrav1beta1.VMSnapshotStatus{})
			rec.createErr = tc.err
			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
			require.NoError(t, err)
			got := &infrav1beta1.VMSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), got))
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Positive(t, res.RequeueAfter)
			if tc.wantPhase == "" {
				assert.Contains(t, got.Status.Message, "will be retried")
			}
		})
	}
}

// TestVMSnapshot_Clustered_SendsItsUIDAsRequestToken: the create carries the
// VMSnapshot's uid, so the provider adopts only a snapshot made for THIS
// VMSnapshot; a same-named leftover of another request is ALREADY_EXISTS and
// fails the VMSnapshot with the provider's message — it never goes Ready on
// an older snapshot.
func TestVMSnapshot_Clustered_SendsItsUIDAsRequestToken(t *testing.T) {
	r, rec, snap := clusteredSnapshotFixture(t, boundSource(), infrav1beta1.VMSnapshotStatus{})
	live := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), live))
	live.UID = "uid-snap-c"
	require.NoError(t, r.Update(context.Background(), live))
	rec.createErr = contracts.NewConflictError(`create snapshot: a snapshot named "snap-c" already exists on this VM and was not made for this request`, nil)

	got := reconcileClusteredSnapshot(t, r, snap)
	require.Len(t, rec.creates, 1)
	assert.Equal(t, "uid-snap-c", rec.creates[0].RequestToken)
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase, "a leftover of another request is never adopted")
	assert.Contains(t, got.Status.Message, "was not made for this request")
	assert.Empty(t, got.Status.SnapshotID)
}

// TestVMSnapshot_SingleHost_RetryableFailureStillFails pins that the retry
// applies to routed (clustered) creates only: a single-host provider's
// retryable failure fails the snapshot, as before.
func TestVMSnapshot_SingleHost_RetryableFailureStillFails(t *testing.T) {
	vm := boundSource()
	vm.Status.Placement = nil
	r, rec, snap := clusteredSnapshotFixture(t, vm, infrav1beta1.VMSnapshotStatus{})
	prov := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "prov-c"}, prov))
	prov.Spec.Topology = infrav1beta1.ProviderTopologySingle
	require.NoError(t, r.Update(context.Background(), prov))
	rec.createErr = contracts.NewRetryableError("create snapshot: unavailable", nil)

	got := reconcileClusteredSnapshot(t, r, snap)
	require.Len(t, rec.creates, 1)
	assert.Empty(t, rec.creates[0].VM.HostID, "a single-host request carries no host")
	assert.Equal(t, infrav1beta1.SnapshotPhaseFailed, got.Status.Phase)
}

func (p *snapshotRecorder) SnapshotDelete(_ context.Context, vm contracts.VMRef, _ string) (string, error) {
	p.deletes = append(p.deletes, vm)
	return "", nil
}

// clusteredSnapshotFixture is a VMSnapshot of vm on the clustered Provider
// prov-c.
func clusteredSnapshotFixture(t *testing.T, vm *infrav1beta1.VirtualMachine, snapStatus infrav1beta1.VMSnapshotStatus) (*VMSnapshotReconciler, *snapshotRecorder, *infrav1beta1.VMSnapshot) {
	t.Helper()
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	snap := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-c", Namespace: "default", Finalizers: []string{"snapshot.infra.virtrigaud.io/finalizer"}},
		Spec:       infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: vm.Name}},
		Status:     snapStatus,
	}
	s := cloneTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, vm, snap).
		WithStatusSubresource(&infrav1beta1.VMSnapshot{}).Build()
	rec := &snapshotRecorder{}
	r := &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return rec, nil }
	return r, rec, snap
}

func reconcileClusteredSnapshot(t *testing.T, r *VMSnapshotReconciler, snap *infrav1beta1.VMSnapshot) *infrav1beta1.VMSnapshot {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.NoError(t, err)
	got := &infrav1beta1.VMSnapshot{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(snap), got); err != nil {
		return nil
	}
	return got
}

func TestVMSnapshot_Clustered_RoutedWithHostAndOwner(t *testing.T) {
	vm := boundSource()
	r, rec, snap := clusteredSnapshotFixture(t, vm, infrav1beta1.VMSnapshotStatus{})
	got := reconcileClusteredSnapshot(t, r, snap)

	require.Len(t, rec.creates, 1)
	assert.Equal(t, contracts.VMRef{ID: "default.src-c", HostID: "host-alpha",
		Owner: contracts.ObjectIdentity{UID: "uid-src-c", Namespace: "default", Name: "src-c"}}, rec.creates[0].VM)
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
}

// TestVMSnapshot_Clustered_OnlyPendingHostIsRefused: a clustered VM that has a
// provider id but only a pending host (no confirmed binding) — and one whose
// create is still pending (no id) — is never sent a snapshot call.
func TestVMSnapshot_Clustered_OnlyPendingHostIsRefused(t *testing.T) {
	t.Run("id but only a pending host", func(t *testing.T) {
		vm := boundSource()
		vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha"}
		r, rec, snap := clusteredSnapshotFixture(t, vm, infrav1beta1.VMSnapshotStatus{})
		got := reconcileClusteredSnapshot(t, r, snap)

		assert.Empty(t, rec.creates, "no host is guessed")
		assert.Empty(t, string(got.Status.Phase), "the snapshot stays in its initial phase and is retried once bound")
		cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating)
		require.NotNil(t, cond)
		assert.Equal(t, reasonVMUnbound, cond.Reason)
	})
	t.Run("create still pending", func(t *testing.T) {
		vm := boundSource()
		vm.Status.ID = ""
		vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha"}
		r, rec, snap := clusteredSnapshotFixture(t, vm, infrav1beta1.VMSnapshotStatus{})
		got := reconcileClusteredSnapshot(t, r, snap)

		assert.Empty(t, rec.creates)
		cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating)
		require.NotNil(t, cond)
		assert.Contains(t, cond.Message, "Waiting for VM to be provisioned")
	})
}

func TestVMSnapshot_Clustered_DeleteRoutedWithHostAndOwner(t *testing.T) {
	vm := boundSource()
	r, rec, snap := clusteredSnapshotFixture(t, vm, infrav1beta1.VMSnapshotStatus{Phase: infrav1beta1.SnapshotPhaseReady, SnapshotID: "snap-1"})
	require.NoError(t, r.Delete(context.Background(), snap))
	reconcileClusteredSnapshot(t, r, snap)

	require.Len(t, rec.deletes, 1)
	assert.Equal(t, "host-alpha", rec.deletes[0].HostID)
	assert.Equal(t, "uid-src-c", rec.deletes[0].Owner.UID)
}
