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
	stderrors "errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the post-Create readiness path (F7): the status write that
// records a new VM's id wakes the VM's first Describe at once, instead of the
// fixed requeue after Create, without a second Create and without a loop.

// TestVMUpdateNeedsReconcile_CreateRecorded: only the first recording of an id
// passes the filter; every other status-only update is still filtered out.
func TestVMUpdateNeedsReconcile_CreateRecorded(t *testing.T) {
	unbound := clusterVM("web", clusteredNS, "prov-cluster")
	recorded := unbound.DeepCopy()
	recorded.Status.ID = "web"
	assert.True(t, vmUpdateNeedsReconcile(unbound, recorded), "the id recorded by a Create is reconciled at once")

	described := recorded.DeepCopy()
	described.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
	described.Status.IPs = []string{"10.0.0.5"}
	assert.False(t, vmUpdateNeedsReconcile(recorded, described), "a later status write of the same VM is not")

	replaced := recorded.DeepCopy()
	replaced.Status.ID = "web-2"
	assert.False(t, vmUpdateNeedsReconcile(recorded, replaced), "an id changing is not a first recording")

	cleared := recorded.DeepCopy()
	cleared.Status.ID = ""
	assert.False(t, vmUpdateNeedsReconcile(recorded, cleared), "an id cleared is not a first recording")

	pending := unbound.DeepCopy()
	pending.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha"}
	assert.False(t, vmUpdateNeedsReconcile(unbound, pending), "the pendingHost write before Create wakes nothing")

	deleting := unbound.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deletingRecorded := deleting.DeepCopy()
	deletingRecorded.Status.ID = "web"
	assert.False(t, vmUpdateNeedsReconcile(deleting, deletingRecorded), "a VM being deleted keeps the delete-path filter")
}

// createThenObserve runs the reconcile that creates the clustered VM name and
// returns its result and the VM as stored just before the status write that
// recorded the id (its pendingHost written, no id yet): the old side of the
// update event that write produces.
func createThenObserve(t *testing.T, r *VirtualMachineReconciler, prov *r4Provider, name string) (res time.Duration, beforeRecord *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		beforeRecord = getVM(t, r, name)
		return contracts.CreateResponse{ID: req.Name}, nil
	}
	// The first reconciles record the restore marker; the one after creates.
	var result ctrl.Result
	for i := 0; i < 3 && len(prov.createReqs) == 0; i++ {
		var err error
		result, err = r.reconcileVM(context.Background(), getVM(t, r, name))
		require.NoError(t, err)
	}
	require.Len(t, prov.createReqs, 1)
	require.NotNil(t, beforeRecord)
	require.Empty(t, beforeRecord.Status.ID)
	return result.RequeueAfter, beforeRecord
}

// TestCreateVM_Clustered_RecordedCreateIsDescribedAtOnce: the id write passes
// the filter, the reconcile it starts describes the VM and reports Ready, no
// second Create is sent, and the VM then polls at its steady-state interval;
// the Describe's own status write wakes nothing.
func TestCreateVM_Clustered_RecordedCreateIsDescribedAtOnce(t *testing.T) {
	prov := newR4Provider(contracts.VMList{})
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}
	r := clusteredFixture(t, prov, clusterVM("fast", clusteredNS, "prov-cluster"))

	requeue, beforeRecord := createThenObserve(t, r, prov, "fast")
	assert.Equal(t, createdDescribeFallback, requeue, "the fallback requeue is kept")
	created := getVM(t, r, "fast")
	require.Equal(t, "fast", created.Status.ID)
	assert.True(t, vmUpdateNeedsReconcile(beforeRecord, created), "the id write wakes the VM")
	assert.Empty(t, prov.describeRefs, "Create itself describes nothing")

	// The reconcile the event starts.
	res, err := r.reconcileVM(context.Background(), created)
	require.NoError(t, err)
	assert.Len(t, prov.createReqs, 1, "a VM whose id is recorded is never created again")
	assert.Len(t, prov.describeRefs, 1)
	ready := getVM(t, r, "fast")
	cond := meta.FindStatusCondition(ready.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, 2*time.Minute, res.RequeueAfter, "a Ready VM polls at the steady-state interval")
	assert.False(t, vmUpdateNeedsReconcile(created, ready), "the Describe's status write starts no loop")

	// The fallback requeue still fires once: one more Describe, no Create.
	res, err = r.reconcileVM(context.Background(), ready)
	require.NoError(t, err)
	assert.Len(t, prov.createReqs, 1)
	assert.Len(t, prov.describeRefs, 2)
	assert.Equal(t, 2*time.Minute, res.RequeueAfter)
}

// TestCreateVM_Clustered_RecordedCreateNotReadyYet: a VM the provider reports
// running without an address yet is not Ready and is re-checked at the
// existing fast cadence, never created again.
func TestCreateVM_Clustered_RecordedCreateNotReadyYet(t *testing.T) {
	prov := newR4Provider(contracts.VMList{})
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "On"}
	r := clusteredFixture(t, prov, clusterVM("slow-ip", clusteredNS, "prov-cluster"))

	createThenObserve(t, r, prov, "slow-ip")
	res, err := r.reconcileVM(context.Background(), getVM(t, r, "slow-ip"))
	require.NoError(t, err)
	assert.Len(t, prov.createReqs, 1)
	assert.Len(t, prov.describeRefs, 1)
	assert.Equal(t, 10*time.Second, res.RequeueAfter, "waiting for an address keeps the existing 10 s poll")
	assert.Empty(t, getVM(t, r, "slow-ip").Status.IPs)
}

// TestCreateVM_Clustered_RecordedCreateDescribeFails: a failed first Describe
// reports the provider error and keeps the existing retry; its status write
// (same id) wakes nothing, and nothing is created again.
func TestCreateVM_Clustered_RecordedCreateDescribeFails(t *testing.T) {
	prov := newR4Provider(contracts.VMList{})
	prov.describeErr = stderrors.New("provider briefly unreachable")
	r := clusteredFixture(t, prov, clusterVM("flaky", clusteredNS, "prov-cluster"))

	createThenObserve(t, r, prov, "flaky")
	created := getVM(t, r, "flaky")
	res, err := r.reconcileVM(context.Background(), created)
	require.NoError(t, err)
	assert.Len(t, prov.createReqs, 1)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter, "the existing Describe retry is kept")
	failed := getVM(t, r, "flaky")
	cond := meta.FindStatusCondition(failed.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, k8s.ReasonProviderError, cond.Reason)
	assert.Equal(t, "flaky", failed.Status.ID, "the recorded id is kept")
	assert.False(t, vmUpdateNeedsReconcile(created, failed), "the failure's status write starts no loop")
}

// TestCreateVM_Clustered_UncertainCreateWakesNothing: a Create whose outcome is
// unknown records no id, so its status write wakes nothing; the pending host
// is kept for the existing retry on the same host.
func TestCreateVM_Clustered_UncertainCreateWakesNothing(t *testing.T) {
	prov := newR4Provider(contracts.VMList{})
	r := clusteredFixture(t, prov, clusterVM("unsure", clusteredNS, "prov-cluster"))
	var beforeCreate *infravirtrigaudiov1beta1.VirtualMachine
	prov.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		beforeCreate = getVM(t, r, "unsure")
		return contracts.CreateResponse{}, contracts.NewTimeoutError("create deadline exceeded", nil)
	}

	var res ctrl.Result
	for i := 0; i < 3 && len(prov.createReqs) == 0; i++ {
		var err error
		res, err = r.reconcileVM(context.Background(), getVM(t, r, "unsure"))
		require.NoError(t, err)
	}
	require.Len(t, prov.createReqs, 1)
	assert.NotZero(t, res.RequeueAfter, "the existing retry requeue is kept")
	after := getVM(t, r, "unsure")
	assert.Empty(t, after.Status.ID, "an uncertain Create records no id")
	require.NotNil(t, after.Status.Placement)
	assert.Equal(t, "host-alpha", after.Status.Placement.PendingHost, "the pending host is kept for the retry")
	require.NotNil(t, beforeCreate)
	assert.False(t, vmUpdateNeedsReconcile(beforeCreate, after), "no id recorded: nothing is woken early")
	assert.Empty(t, prov.describeRefs)
}
