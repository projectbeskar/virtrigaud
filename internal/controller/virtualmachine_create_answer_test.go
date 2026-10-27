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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the record of a provider's answer to a failed Create
// (writeCreateAnswer): an edit of the VirtualMachine made while the provider
// ran the Create must not lose the answer (v0.4.1-rc2 lab, B2), and the
// answer is never written over anything newer.

// notPerformedCreate is what the transport hands the controller for a Create
// a provider answered it did not perform, scoped to one VM: gRPC Unavailable
// with CREATE_NOT_PERFORMED and VM_OPERATION_FAILED (the Proxmox provider's
// vmCreateNotPerformed, e.g. a clone task that ended on a config lock
// timeout), marked by the client (MarkCreateNotPerformed).
func notPerformedCreate(t *testing.T) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "create: the clone task ended before it cloned anything: "+
		"a configuration lock timed out; nothing was cloned (will retry)").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.CreateNotPerformedReason, Domain: contracts.ErrorInfoDomain},
		&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	return contracts.MarkCreateNotPerformed(contracts.NewRetryableError(st.Message(), st.Err()))
}

// providerWideNotPerformedCreate is a Create a provider answered it did not
// perform for a reason not scoped to the VM: gRPC Unavailable with
// CREATE_NOT_PERFORMED alone (the Proxmox provider's createNotPerformed).
func providerWideNotPerformedCreate(t *testing.T) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "create: no free VMID could be allocated; nothing was created (will retry)").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.CreateNotPerformedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	return contracts.MarkCreateNotPerformed(contracts.NewRetryableError(st.Message(), st.Err()))
}

// editDuringCreate makes the provider stub label the stored VM (moving its
// resourceVersion on, as a tenant or a tool would) and then answer err.
func editDuringCreate(t *testing.T, r *VirtualMachineReconciler, rp *routingProvider, err error) {
	t.Helper()
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		vm, _ := storedVM(t, r)
		vm.Labels = map[string]string{"loadgen": "touched"}
		require.NoError(t, r.Update(context.Background(), vm))
		return contracts.CreateResponse{}, err
	}
}

// TestCreateVM_NotPerformedAnswerSurvivesAnEditDuringCreate is B2: a Proxmox
// Create answered "not performed" while the VM was edited. The answer's status
// write conflicted and was lost, so the create marker stayed, and the delete
// that followed was held as CreateOutcomeUnknown for an operator although
// nothing existed on the hypervisor. The answer is now re-applied to the live
// object, and the delete is released without a provider call.
func TestCreateVM_NotPerformedAnswerSurvivesAnEditDuringCreate(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
	editDuringCreate(t, r, rp, notPerformedCreate(t))

	vm, _ := storedVM(t, r)
	gen := vm.Generation
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "retried with the create backoff")
	require.Len(t, rp.createReqs, 1)

	got, _ := storedVM(t, r)
	assert.Equal(t, map[string]string{"loadgen": "touched"}, got.Labels, "the edit is kept")
	assert.False(t, createRequested(got), "the provider's definite answer replaced the create marker")
	prov := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning)
	require.NotNil(t, prov)
	assert.Equal(t, metav1.ConditionFalse, prov.Status)
	assert.Equal(t, k8s.ReasonCreateBackoff, prov.Reason)
	assert.Equal(t, gen, prov.ObservedGeneration)
	ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonCreateBackoff, ready.Reason)
	assert.Equal(t, createNotPerformedBackoffMessage, ready.Message)
	assert.Empty(t, got.Status.ID)

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning "+k8s.ReasonCreateBackoff+" "+createNotPerformedBackoffMessage), events[0])
	assert.Contains(t, events[0], "Provider answer: create: the clone task ended before it cloned anything",
		"the provider's answer is kept in the event")

	// The VM is deleted right after (loadgen's interrupt): nothing was made,
	// so it is released without a provider call and without an operator.
	require.NoError(t, r.Delete(ctx, got))
	deleting, exists := storedVM(t, r)
	require.True(t, exists)
	_, err = r.handleDeletion(ctx, deleting)
	require.NoError(t, err)
	_, exists = storedVM(t, r)
	assert.False(t, exists, "the delete is not held as CreateOutcomeUnknown")
	assert.Empty(t, rp.deleteRefs, "nothing to delete on the hypervisor")
	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, k8s.ReasonCreateOutcomeUnknown)
	}
}

// TestCreateVM_DefiniteAnswersSurviveAnEditDuringCreate covers every other
// answer path of a single-host Create whose status write may conflict:
// handleRejectedCreate (Conflict, InvalidSpec), holdCreateBackoff (a refusal,
// and an answer whose outcome stays unknown), paceRefusedCreate (NotFound) and
// the generic path. Whatever the answer, it is recorded on the live object:
// the marker is replaced exactly when the outcome is known, and kept
// otherwise.
func TestCreateVM_DefiniteAnswersSurviveAnEditDuringCreate(t *testing.T) {
	cases := map[string]struct {
		err          func(t *testing.T) error
		markerKept   bool
		readyReason  string
		provisioning string
	}{
		"name conflict": {err: func(*testing.T) error { return wireConflict() },
			readyReason: k8s.ReasonProviderConflict, provisioning: k8s.ReasonProviderConflict},
		"invalid spec": {err: func(*testing.T) error { return contracts.NewInvalidSpecError("create: bad name", nil) },
			readyReason: k8s.ReasonValidationError, provisioning: k8s.ReasonValidationError},
		"refused (FailedPrecondition)": {err: func(*testing.T) error {
			return status.Error(codes.FailedPrecondition, "create: run the migration again")
		}, readyReason: k8s.ReasonCreateRefused, provisioning: k8s.ReasonCreateRefused},
		"not found (paced)": {err: func(*testing.T) error { return contracts.NewNotFoundError("create: template gone", nil) },
			readyReason: "", provisioning: k8s.ReasonProviderError},
		"outcome unknown (VM_OPERATION_FAILED)": {err: operationFailedCreate,
			markerKept: true, readyReason: k8s.ReasonCreateBackoff, provisioning: k8s.ReasonCreateRequested},
		// The generic path: a provider-wide CREATE_NOT_PERFORMED (no
		// VM_OPERATION_FAILED: e.g. no VMID could be allocated) replaces the
		// marker; a transport failure keeps it.
		"provider-wide not performed": {err: providerWideNotPerformedCreate,
			readyReason: "", provisioning: k8s.ReasonProviderError},
		"transport failure (outcome unknown)": {err: func(*testing.T) error { return transportFailure("create") },
			markerKept: true, readyReason: k8s.ReasonProviderError, provisioning: k8s.ReasonCreateRequested},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
			editDuringCreate(t, r, rp, tc.err(t))

			vm, _ := storedVM(t, r)
			_, err := r.reconcileVM(ctx, vm)
			require.NoError(t, err)

			got, _ := storedVM(t, r)
			assert.Equal(t, "touched", got.Labels["loadgen"], "the edit is kept")
			assert.Equal(t, tc.markerKept, createRequested(got))
			prov := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning)
			require.NotNil(t, prov)
			assert.Equal(t, tc.provisioning, prov.Reason, "the answer is recorded although the VM was edited")
			if tc.readyReason != "" {
				ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, tc.readyReason, ready.Reason)
			}
		})
	}
}

// TestCreateVM_ProviderWideNotPerformedSaysTheProviderAnswered (review): a
// provider-wide CREATE_NOT_PERFORMED is the provider's answer, not a
// transport failure, so its detail is shown.
func TestCreateVM_ProviderWideNotPerformedSaysTheProviderAnswered(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
	createErr := providerWideNotPerformedCreate(t)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, createErr
	}
	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	prov := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning)
	require.NotNil(t, prov)
	assert.Contains(t, prov.Message, "no free VMID could be allocated")
	assert.NotContains(t, prov.Message, providerNotReachedDetail)
}

// TestCreateVM_Clustered_AnswerSurvivesAnEditDuringCreate: a clustered
// Create's answer (Placed, Provisioning, Ready) is recorded although the VM
// was edited while the provider ran it, and the pending host is kept.
func TestCreateVM_Clustered_AnswerSurvivesAnEditDuringCreate(t *testing.T) {
	cases := map[string]struct {
		err          func(t *testing.T) error
		placed       string
		provisioning string
	}{
		"host unavailable": {err: func(*testing.T) error {
			return contracts.NewHostUnavailableError("create: connect to target host \"host-alpha\"", nil)
		}, placed: k8s.ReasonHostUnavailable, provisioning: k8s.ReasonProviderError},
		"not performed (backoff)": {err: notPerformedCreate,
			placed: k8s.ReasonCreatePending, provisioning: k8s.ReasonCreateBackoff},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			vm := clusterVM("vm-edit", clusteredNS, "prov-cluster")
			createErr := tc.err(t)
			var r *VirtualMachineReconciler
			prov := &routingProvider{onCreate: func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				live := getVM(t, r, "vm-edit")
				live.Labels = map[string]string{"loadgen": "touched"}
				require.NoError(t, r.Update(ctx, live))
				return contracts.CreateResponse{}, createErr
			}}
			r = clusteredFixture(t, prov, vm)

			_, err := r.createVM(ctx, getVM(t, r, "vm-edit"), prov, clusteredProviderCR("prov-cluster", clusteredNS),
				smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
			require.NoError(t, err)
			require.Len(t, prov.createReqs, 1)
			after := getVM(t, r, "vm-edit")
			assert.Equal(t, "touched", after.Labels["loadgen"], "the edit is kept")
			require.NotNil(t, after.Status.Placement)
			assert.Equal(t, "host-alpha", after.Status.Placement.PendingHost, "the pending host is kept")
			require.NotNil(t, placedCondition(after))
			assert.Equal(t, tc.placed, placedCondition(after).Reason, "the answer is recorded although the VM was edited")
			assert.Equal(t, tc.provisioning, provisioningReason(after))
		})
	}
}

// TestWriteCreateAnswer_NeverOverwritesSomethingNewer pins the guard: after a
// conflict the answer is written only to a VirtualMachine that still waits for
// it — the same create marker and pending host, and no id.
func TestWriteCreateAnswer_NeverOverwritesSomethingNewer(t *testing.T) {
	cases := map[string]struct {
		attempt func(stored *infravirtrigaudiov1beta1.VirtualMachine) createAttempt
		change  func(stored *infravirtrigaudiov1beta1.VirtualMachine)
		written bool
	}{
		"still waits (single-host)": {
			attempt: func(s *infravirtrigaudiov1beta1.VirtualMachine) createAttempt {
				return createAttempt{sentMarkerID: markerAttemptID(s)}
			},
			change:  func(*infravirtrigaudiov1beta1.VirtualMachine) {},
			written: true,
		},
		"an id was recorded meanwhile": {
			attempt: func(s *infravirtrigaudiov1beta1.VirtualMachine) createAttempt {
				return createAttempt{sentMarkerID: markerAttemptID(s)}
			},
			change: func(s *infravirtrigaudiov1beta1.VirtualMachine) { s.Status.ID = "vm-recorded" },
		},
		"another attempt's marker": {
			attempt: func(s *infravirtrigaudiov1beta1.VirtualMachine) createAttempt {
				return createAttempt{sentMarkerID: markerAttemptID(s)}
			},
			change: func(s *infravirtrigaudiov1beta1.VirtualMachine) {
				c := meta.FindStatusCondition(s.Status.Conditions, k8s.ConditionProvisioning)
				c.LastTransitionTime = metav1.NewTime(c.LastTransitionTime.Add(30e9))
			},
		},
		"still waits (clustered, pending host)": {
			attempt: func(*infravirtrigaudiov1beta1.VirtualMachine) createAttempt { return createAttempt{sentHost: "host-a"} },
			change: func(s *infravirtrigaudiov1beta1.VirtualMachine) {
				meta.RemoveStatusCondition(&s.Status.Conditions, k8s.ConditionProvisioning)
				s.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-a"}
			},
			written: true,
		},
		"another pending host": {
			attempt: func(*infravirtrigaudiov1beta1.VirtualMachine) createAttempt { return createAttempt{sentHost: "host-a"} },
			change: func(s *infravirtrigaudiov1beta1.VirtualMachine) {
				meta.RemoveStatusCondition(&s.Status.Conditions, k8s.ConditionProvisioning)
				s.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-b"}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
			stored, _ := storedVM(t, r)
			setCreateRequested(stored, r.now())
			require.NoError(t, r.Status().Update(ctx, stored))
			stored, _ = storedVM(t, r)
			attempt := tc.attempt(stored)

			// This reconcile's copy: the answer computed on the status it read.
			vm := stored.DeepCopy()
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonCreateBackoff,
				createNotPerformedBackoffMessage, vm.Generation)

			// Meanwhile the stored object moves on: an edit (the conflict), and
			// whatever the case changes in its status.
			tc.change(stored)
			require.NoError(t, r.Status().Update(ctx, stored))
			stored, _ = storedVM(t, r)
			stored.Labels = map[string]string{"edited": "yes"}
			require.NoError(t, r.Update(ctx, stored))
			before, _ := storedVM(t, r)

			r.writeCreateAnswer(ctx, vm, attempt)

			after, _ := storedVM(t, r)
			prov := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionProvisioning)
			if tc.written {
				require.NotNil(t, prov)
				assert.Equal(t, k8s.ReasonCreateBackoff, prov.Reason, "the answer is re-applied to the live object")
				assert.Equal(t, before.Status.ObservedGeneration, after.Status.ObservedGeneration,
					"status.observedGeneration keeps its stored value")
				assert.Equal(t, after.ResourceVersion, vm.ResourceVersion, "the caller's copy takes the written object")
				return
			}
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is written over something newer")
			assert.Equal(t, before.Status, after.Status)
		})
	}
}

// TestHoldCreateBackoff_MessageSaysWhatIsKnown is O1: a Create the provider
// did not perform is not described as a disk check or a host failure (it may
// be a VMID another Create took first, or a lock), and the event keeps the
// provider's own answer.
func TestHoldCreateBackoff_MessageSaysWhatIsKnown(t *testing.T) {
	cases := map[string]struct {
		err  func(t *testing.T) error
		msg  string
		keep string
	}{
		"not performed": {err: notPerformedCreate, msg: createNotPerformedBackoffMessage,
			keep: "the clone task ended before it cloned anything"},
		"failed on its host": {err: operationFailedCreate, msg: createBackoffMessage,
			keep: "the host could not complete the operation"},
		"disk check failed": {err: func(*testing.T) error { return diskCheckFailedCreate() }, msg: createBackoffMessage,
			keep: "could not verify that no other domain uses the disk"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
			createErr := tc.err(t)
			rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{}, createErr
			}
			vm, _ := storedVM(t, r)
			_, err := r.reconcileVM(ctx, vm)
			require.NoError(t, err)

			got, _ := storedVM(t, r)
			ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.msg, ready.Message)
			assert.NotContains(t, createNotPerformedBackoffMessage, "disk")
			events := drainEvents(rec)
			require.Len(t, events, 1)
			assert.Contains(t, events[0], tc.msg)
			assert.Contains(t, events[0], "Provider answer: ")
			assert.Contains(t, events[0], tc.keep, "the provider's detail is kept")
		})
	}
}

// TestDeleteVM_StaleCacheMarkerIsNotHeld (review): a delete reconciled from a
// cache that still shows the create marker, while the stored VirtualMachine
// already records the provider's answer (writeCreateAnswer, the VM deleted
// while its Create ran), is neither held as CreateOutcomeUnknown nor
// announced as such: it is re-checked from the next read, which releases it.
func TestDeleteVM_StaleCacheMarkerIsNotHeld(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
	stored, _ := storedVM(t, r)
	setCreateRequested(stored, r.now())
	require.NoError(t, r.Status().Update(ctx, stored))
	stale, _ := storedVM(t, r)

	// The answer lands on the stored object, then the VM is deleted.
	stored, _ = storedVM(t, r)
	k8s.SetProvisioningCondition(&stored.Status.Conditions, metav1.ConditionFalse, k8s.ReasonCreateBackoff,
		createNotPerformedBackoffMessage, stored.Generation)
	require.NoError(t, r.Status().Update(ctx, stored))
	require.NoError(t, r.Delete(ctx, stored))
	deleting, _ := storedVM(t, r)
	require.False(t, deleting.DeletionTimestamp.IsZero())

	// A reconcile from the stale cache: the marker, the deletion, phase
	// Deleting already shown.
	stale.DeletionTimestamp = deleting.DeletionTimestamp
	stale.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	require.True(t, createRequested(stale))
	res, err := r.handleDeletion(ctx, stale)
	require.NoError(t, err)
	assert.Equal(t, deletingPhaseConflictRetry, res.RequeueAfter, "re-checked from a fresh read")
	_, exists := storedVM(t, r)
	assert.True(t, exists)
	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, k8s.ReasonCreateOutcomeUnknown, "no spurious hold is announced")
	}

	fresh, _ := storedVM(t, r)
	_, err = r.handleDeletion(ctx, fresh)
	require.NoError(t, err)
	_, exists = storedVM(t, r)
	assert.False(t, exists, "released once the read shows the answer")
	assert.Empty(t, rp.deleteRefs)
}
