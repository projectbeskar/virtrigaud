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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a single-host clone whose Clone RPC fails transiently
// (an SSH or libvirtd blip, the provider briefly unreachable) waits and is
// retried with the blocked-VM backoff instead of failing for good, while a
// clone whose outcome is unknown, or that the provider refused, still fails.

// reconcileCloneOnce runs one reconcile of clone and returns its result.
func reconcileCloneOnce(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.NoError(t, err)
	return res
}

func TestVMClone_SingleHost_TransientCloneErrorIsHeldAndRetried(t *testing.T) {
	for name, transient := range map[string]error{
		// The provider answered that it did nothing (Unavailable with
		// VM_OPERATION_FAILED, as the transport maps it).
		"provider: libvirt host unreachable, nothing defined": contracts.NewRetryableError(
			"clone: failed to clone VM: the libvirt host could not be reached",
			fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed,
				status.Error(codes.Unavailable, "failed to clone VM: the libvirt host could not be reached"))),
		"provider: host unavailable": contracts.NewHostUnavailableError("clone: host unreachable", nil),
		"provider: disk check failed": contracts.NewRetryableError("clone: disk check could not run",
			contracts.ErrVMDiskCheckFailed),
		// The request never reached the provider.
		"request not sent: no connection": contracts.NewRetryableError("clone: connection error",
			fmt.Errorf("%w: %w", contracts.ErrRequestNotSent, transportFailure("clone"))),
		"request not sent: breaker open": contracts.NewRetryableError("clone: circuit breaker open",
			fmt.Errorf("%w: %w", contracts.ErrRequestNotSent, status.Error(codes.Unavailable, "circuit breaker open"))),
	} {
		t.Run(name, func(t *testing.T) {
			prov, src, clone := cloneDisclosureFixture()
			cp := &clonerProvider{cloneErr: transient}
			r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)

			reconcileCloneOnce(t, r, clone) // finalizer
			res := reconcileCloneOnce(t, r, clone)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the blocked-VM backoff starts at 15 s")
			assert.Equal(t, 1, cp.cloneCnt)

			got, texts := requireCloneNoLeak(t, r, clone, "10.96.12.7")
			assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "never Failed for a transient error")
			assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionFailed))
			ready := cloneReadyCondition(t, got)
			assert.Equal(t, cloneReasonRetrying, ready.Reason)
			assert.Equal(t, cloneTransientRetryMessage, ready.Message, "a constant message")
			assert.Equal(t, 1, strings.Count(texts, "Warning "+cloneReasonRetrying))

			// A repeated hold writes nothing and announces nothing; the
			// backoff grows with the time held.
			got.Status.Conditions = append([]metav1.Condition(nil), got.Status.Conditions...)
			for i := range got.Status.Conditions {
				if got.Status.Conditions[i].Type == infrav1beta1.VMCloneConditionReady {
					got.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Minute))
				}
			}
			require.NoError(t, r.Status().Update(context.Background(), got))
			// The backoff has passed (cloneAttemptPacer gates every attempt).
			r.cloneAttempts.elapse(clone.UID)
			rv := getClone(t, r, clone).ResourceVersion
			res = reconcileCloneOnce(t, r, clone)
			assert.InDelta(t, time.Minute.Seconds(), res.RequeueAfter.Seconds(), 5)
			assert.Equal(t, 2, cp.cloneCnt, "retried")
			assert.Equal(t, rv, getClone(t, r, clone).ResourceVersion, "a repeated hold writes nothing")
			assert.Empty(t, drainRecordedEvents(fakeRecorderOf(t, r.Recorder)))

			// A reconcile before the backoff has passed (e.g. a spec edit)
			// sends nothing and writes nothing.
			res = reconcileCloneOnce(t, r, clone)
			assert.Equal(t, 2, cp.cloneCnt, "the backoff gates every attempt")
			assert.Positive(t, res.RequeueAfter)
			assert.Equal(t, rv, getClone(t, r, clone).ResourceVersion)

			// The provider answers again: the clone completes.
			r.cloneAttempts.elapse(clone.UID)
			cp.cloneErr = nil
			cp.cloneResp = contracts.CloneResponse{TargetVmID: "vm-clone-1"}
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
		})
	}
}

func TestVMClone_SingleHost_UnknownOutcomeOrRefusalStillFails(t *testing.T) {
	for name, failure := range map[string]error{
		// H2: a transport failure after the request was sent — the provider
		// pod killed mid-RPC — may have made a VM (Proxmox takes a new VMID
		// on every clone): never sent again.
		"mid-RPC transport failure (outcome unknown)": transportFailure("clone"),
		"connection lost mid-RPC (outcome unknown)": contracts.NewRetryableError("clone: error reading from server: EOF",
			status.Error(codes.Unavailable, "error reading from server: EOF")),
		"bare provider Unavailable":  contracts.NewRetryableError("clone: busy", status.Error(codes.Unavailable, "busy")),
		"in progress without reason": contracts.NewInProgressError("clone: in progress", nil),
		"deadline exceeded (outcome unknown)": contracts.NewRetryableError("clone: deadline exceeded",
			status.Error(codes.DeadlineExceeded, "context deadline exceeded")),
		"provider timeout":     contracts.NewTimeoutError("clone: copy timed out", nil),
		"invalid spec":         contracts.NewInvalidSpecError("clone: target exists", nil),
		"plain provider error": fmt.Errorf("clone failed: %w", errors.New("define target domain: boom")),
	} {
		t.Run(name, func(t *testing.T) {
			prov, src, clone := cloneDisclosureFixture()
			cp := &clonerProvider{cloneErr: failure}
			r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)

			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			assert.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase)
			assert.Equal(t, 1, cp.cloneCnt, "a failed clone is never sent again")
		})
	}
}
