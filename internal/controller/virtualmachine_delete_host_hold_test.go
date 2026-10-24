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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin V3a: a clustered delete held because the VM's own host does
// not answer says so on DeleteBlocked=True/HostUnreachable, naming that host
// (status.placement.host) — only when the provider said it is that host — and
// the hold is lifted when the delete proceeds.

// hostUnavailableAnswer is a delete answered HOST_UNAVAILABLE, as the
// transport client maps it; host is the ErrorInfo's HostUnavailableHostKey
// ("" for none).
func hostUnavailableAnswer(t *testing.T, msg, host string) error {
	t.Helper()
	info := &errdetails.ErrorInfo{Reason: contracts.HostUnavailableReason, Domain: contracts.HostUnavailableErrorDomain}
	if host != "" {
		info.Metadata = map[string]string{contracts.HostUnavailableHostKey: host}
	}
	st, err := status.New(codes.Unavailable, msg).WithDetails(info)
	require.NoError(t, err)
	return contracts.NewHostUnavailableError("delete: "+msg, st.Err())
}

// hostLostAnswer is a delete answered HOST_CONNECTION_LOST (R25): not
// performed (code Unavailable) or an unknown outcome (code Unknown), as the
// transport client maps it.
func hostLostAnswer(t *testing.T, notPerformed bool) error {
	t.Helper()
	code, msg := codes.Unknown, `failed to delete VM on host "host-alpha": the host stopped answering while the operation ran; its outcome is unknown`
	if notPerformed {
		code, msg = codes.Unavailable, `failed to delete VM on host "host-alpha": the host stopped answering before the operation changed anything; nothing was done`
	}
	st := status.New(code, msg)
	for _, reason := range []string{contracts.VMOperationFailedReason, contracts.HostConnectionLostReason} {
		next, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain})
		require.NoError(t, err)
		st = next
	}
	if notPerformed {
		return contracts.NewRetryableError("delete: "+msg, fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed, st.Err()))
	}
	return fmt.Errorf("delete failed: %s: %w", msg, st.Err())
}

func TestHandleDeletion_Clustered_HostUnreachableNamesTheVMsOwnHost(t *testing.T) {
	cases := map[string]struct {
		err       func(t *testing.T) error
		wantHost  bool
		wantPhase string
	}{
		"HOST_UNAVAILABLE naming the VM's host": {
			err: func(t *testing.T) error {
				return hostUnavailableAnswer(t, `delete VM: host "host-alpha" is unreachable`, "host-alpha")
			},
			wantHost: true, wantPhase: "nothing was deleted",
		},
		"HOST_UNAVAILABLE naming another host (never named)": {
			err: func(t *testing.T) error {
				return hostUnavailableAnswer(t, "delete VM: a host is unreachable", "host-beta")
			},
		},
		"HOST_UNAVAILABLE naming no host (cluster guard, older provider)": {
			err: func(t *testing.T) error {
				return hostUnavailableAnswer(t, "delete of libvirt domain not performed: a host of this Provider could not be reached", "")
			},
		},
		"the host stopped answering before anything changed": {
			err:      func(t *testing.T) error { return hostLostAnswer(t, true) },
			wantHost: true, wantPhase: "nothing was deleted",
		},
		"the host stopped answering while the delete ran": {
			err:      func(t *testing.T) error { return hostLostAnswer(t, false) },
			wantHost: true, wantPhase: "not known how far the delete got",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			prov := &routingProvider{deleteErr: tc.err(t)}
			r := clusteredFixture(t, prov, boundClusterVMForDelete())
			rec := record.NewFakeRecorder(16)
			r.Recorder = rec

			res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)

			held := getVM(t, r, "web")
			assert.Contains(t, held.Finalizers, infrav1beta1.VirtualMachineFinalizer, "the safety hold is unchanged")
			assert.Equal(t, infrav1beta1.VirtualMachinePhaseDeleting, held.Status.Phase)
			blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
			require.NotNil(t, blocked)
			assert.Equal(t, metav1.ConditionTrue, blocked.Status)
			assert.Equal(t, k8s.ReasonHostUnreachable, blocked.Reason)
			assert.NotContains(t, blocked.Message, "host-beta", "another host is never named")
			assert.NotContains(t, blocked.Message, "qemu+ssh", "never an endpoint")
			if tc.wantHost {
				assert.Contains(t, blocked.Message, "this VirtualMachine's host host-alpha")
				assert.Contains(t, blocked.Message, tc.wantPhase)
				assert.Contains(t, blocked.Message, "completes once the host answers")
			} else {
				assert.Equal(t, deleteBlockedMessages[k8s.ReasonHostUnreachable], blocked.Message)
			}
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonDeleteBlocked, ready.Reason)
			assert.Equal(t, blocked.Message, ready.Message)
			events := drainEvents(rec)
			require.Len(t, events, 1)
			assert.True(t, strings.HasPrefix(events[0], "Warning "+k8s.ReasonDeleteBlocked), events[0])

			// The host answers again and the delete proceeds: the finalizer
			// is removed (and with it the hold).
			prov.deleteErr = nil
			_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
			require.NoError(t, err)
			gone := &infrav1beta1.VirtualMachine{}
			getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "web"}, gone)
			assert.True(t, apierrors.IsNotFound(getErr), "the delete completed once the host answered")
		})
	}
}

func TestHandleDeletion_Clustered_HostHoldClearedWhenTheDeleteFailsOtherwise(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{deleteErr: hostUnavailableAnswer(t, `delete VM: host "host-alpha" is unreachable`, "host-alpha")}
	r := clusteredFixture(t, prov, boundClusterVMForDelete())
	_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	require.NotNil(t, meta.FindStatusCondition(getVM(t, r, "web").Status.Conditions, k8s.ConditionDeleteBlocked))

	prov.deleteErr = fmt.Errorf("delete: the hypervisor said no")
	_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.Nil(t, meta.FindStatusCondition(getVM(t, r, "web").Status.Conditions, k8s.ConditionDeleteBlocked))
}

func TestDeleteHostHold_PendingHostIsTheVMsHost(t *testing.T) {
	vm := boundClusterVMForDelete()
	vm.Status.ID = ""
	vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
	reason, msg := deleteHostHold(vm, hostUnavailableAnswer(t, `delete VM: host "host-alpha" is unreachable`, "host-alpha"))
	assert.Equal(t, k8s.ReasonHostUnreachable, reason)
	assert.Contains(t, msg, "host host-alpha")
}
