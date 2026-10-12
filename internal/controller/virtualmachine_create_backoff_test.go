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
	"fmt"
	"testing"
	"time"

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

// A Create answered with a retryable VM_DISK_CHECK_FAILED or
// VM_OPERATION_FAILED is retried with the blocked-VM backoff, not every 5 s.

// diskCheckFailedCreate is what the transport hands the controller for a
// Create the provider did not perform because it could not verify the disk.
func diskCheckFailedCreate() error {
	st := status.New(codes.Unavailable, "create: could not verify that no other domain uses the disk")
	return contracts.NewRetryableError("create: "+st.Message(), fmt.Errorf("%w: %w", contracts.ErrVMDiskCheckFailed, st.Err()))
}

// operationFailedCreate is a retryable Create that reached the host and failed
// there (VM_OPERATION_FAILED).
func operationFailedCreate(t *testing.T) error {
	t.Helper()
	st, err := status.New(codes.Unavailable, "create: the host could not complete the operation").WithDetails(
		&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	require.NoError(t, err)
	return contracts.NewRetryableError("create: "+st.Message(), st.Err())
}

func TestCreateVM_RetryableHostFailuresBackOff(t *testing.T) {
	cases := map[string]struct {
		err             func(t *testing.T) error
		markerReplaced  bool
		provisionReason string
	}{
		// Both may come from an operation that can still complete on the
		// host: the outcome is unknown, the marker stays.
		"VM_DISK_CHECK_FAILED (outcome unknown)": {err: func(*testing.T) error { return diskCheckFailedCreate() },
			markerReplaced: false, provisionReason: k8s.ReasonCreateRequested},
		"VM_OPERATION_FAILED (outcome unknown)": {err: operationFailedCreate,
			markerReplaced: false, provisionReason: k8s.ReasonCreateRequested},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
			createErr := tc.err(t)
			rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{}, createErr
			}

			vm, _ := storedVM(t, r)
			res, err := r.reconcileVM(ctx, vm)
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the first retry of the backoff, not 5 s")
			held, _ := storedVM(t, r)
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonCreateBackoff, ready.Reason)
			assert.Equal(t, createBackoffMessage, ready.Message, "a constant message")
			assert.NotContains(t, ready.Message, "could not complete the operation", "the provider's answer is in the log only")
			assert.Equal(t, tc.provisionReason, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionProvisioning).Reason)
			assert.Equal(t, !tc.markerReplaced, createRequested(held))
			assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateBackoff))

			// The backoff grows with the time already held; Ready is not
			// rewritten and no second event is recorded. The marker is kept,
			// so the re-send waits for the backoff from the last attempt.
			since := metav1.NewTime(time.Now().Add(-3 * time.Minute))
			meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).LastTransitionTime = since
			require.NoError(t, r.Status().Update(ctx, held))
			again, _ := storedVM(t, r)
			res, err = r.reconcileVM(ctx, again)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, res.RequeueAfter, 2*time.Minute+50*time.Second)
			assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)
			assert.Len(t, rp.createReqs, 1, "no re-send before the backoff")
			after, _ := storedVM(t, r)
			readyAfter := meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionReady)
			assert.Equal(t, since.Unix(), readyAfter.LastTransitionTime.Unix(), "the hold keeps its start")
			assert.Equal(t, createBackoffMessage, readyAfter.Message)
			assert.Zero(t, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateBackoff))

			// The backoff has passed (the last attempt is an hour old).
			aged := after.DeepCopy()
			meta.FindStatusCondition(aged.Status.Conditions, k8s.ConditionProvisioning).LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Hour))
			require.NoError(t, r.Status().Update(ctx, aged))
			after, _ = storedVM(t, r)

			// The host recovers: the Create succeeds and the VM becomes Ready.
			rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
			}
			rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}
			_, err = r.reconcileVM(ctx, after)
			require.NoError(t, err)
			created, _ := storedVM(t, r)
			assert.Equal(t, "vm-web", created.Status.ID)
			_, err = r.reconcileVM(ctx, created)
			require.NoError(t, err)
			ready2, _ := storedVM(t, r)
			assert.True(t, meta.IsStatusConditionTrue(ready2.Status.Conditions, k8s.ConditionReady))
		})
	}
}

func TestCreateVM_OtherRetryableErrorsKeepTheirCadence(t *testing.T) {
	r, rp, _ := createRecordFixture(t, "", nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewRetryableError("create: host busy", nil)
	}
	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	assert.False(t, createNeedsBackoff(contracts.NewRetryableError("create: host busy", nil)))
}

// TestCreateVM_BackoffCountsFromTheHoldStart (review L1): a Ready that was
// False for another reason long before does not shorten the backoff's start —
// the first retry of a new CreateBackoff hold is the first step.
func TestCreateVM_BackoffCountsFromTheHoldStart(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, diskCheckFailedCreate()
	}
	vm, _ := storedVM(t, r)
	vm.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionReady, Status: metav1.ConditionFalse,
		Reason: k8s.ReasonProviderError, Message: "an older failure", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}}
	require.NoError(t, r.Status().Update(ctx, vm))
	stored, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the first retry of the hold is 15 s")
	held, _ := storedVM(t, r)
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	assert.WithinDuration(t, time.Now(), ready.LastTransitionTime.Time, 5*time.Second, "the hold starts now")
}

// TestCreateVM_NonRetryableRefusalBacksOff (Q): an untyped FailedPrecondition
// (e.g. "imported disk not found ... run the VMMigration again") or
// PermissionDenied answering a Create is a refusal: the create marker is
// replaced, Ready and Provisioning say CreateRefused with a constant message,
// one Warning event carries the provider's answer, and the Create is
// re-checked on the blocked-VM backoff instead of every 5 s.
func TestCreateVM_NonRetryableRefusalBacksOff(t *testing.T) {
	for name, st := range map[string]*status.Status{
		"FailedPrecondition": status.New(codes.FailedPrecondition, "imported disk not found; run the VMMigration again"),
		"PermissionDenied":   status.New(codes.PermissionDenied, "permission VM.Allocate denied"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeVSphere, nil)
			// What the transport hands the controller: untyped text, the
			// status kept in the chain.
			createErr := fmt.Errorf("create failed: %s: %w", st.Message(), st.Err())
			rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{}, createErr
			}
			vm, _ := storedVM(t, r)
			res, err := r.reconcileVM(ctx, vm)
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			held, _ := storedVM(t, r)
			assert.False(t, createRequested(held), "nothing was done: the marker is replaced")
			for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
				c := meta.FindStatusCondition(held.Status.Conditions, condType)
				require.NotNil(t, c, condType)
				assert.Equal(t, k8s.ReasonCreateRefused, c.Reason, condType)
				assert.Equal(t, createRefusedMessage, c.Message, condType)
			}
			events := drainEvents(rec)
			require.Len(t, events, 1)
			assert.Contains(t, events[0], "Warning "+k8s.ReasonCreateRefused)
			assert.Contains(t, events[0], st.Message(), "the event carries the provider's answer")

			// Repeated: no rewrite of Ready, no second event, the backoff grows.
			since := metav1.NewTime(time.Now().Add(-3 * time.Minute))
			meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).LastTransitionTime = since
			require.NoError(t, r.Status().Update(ctx, held))
			again, _ := storedVM(t, r)
			res, err = r.reconcileVM(ctx, again)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, res.RequeueAfter, 3*time.Minute)
			assert.Empty(t, drainEvents(rec))
		})
	}
	assert.False(t, createRefusedByProvider(contracts.NewRetryableError("busy", status.Error(codes.FailedPrecondition, "x"))),
		"a typed answer is not an untyped refusal")
}
