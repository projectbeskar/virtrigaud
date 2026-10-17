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

package grpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// needsPowerOffErr builds a provider's Reconfigure refusal of a change the
// running VM cannot take, with the given ErrorInfo reasons, in domain.
func needsPowerOffErr(t *testing.T, code codes.Code, domain string, reasons ...string) error {
	t.Helper()
	st := status.New(code, "the VM is powered on and CPU hot-add is not enabled on it; power it off to apply the change")
	for _, r := range reasons {
		var err error
		st, err = st.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: domain})
		require.NoError(t, err)
	}
	return st.Err()
}

// TestMapGRPCError_ReconfigureNeedsPowerOffIsRetryableAndMarked: the vSphere
// refusal of an online CPU/memory change (rc1 lab finding V1) reaches the
// manager as a retryable error marked ErrReconfigureNeedsPowerOff, and only
// VirtRigaud's FailedPrecondition carrying the reason is that refusal.
func TestMapGRPCError_ReconfigureNeedsPowerOffIsRetryableAndMarked(t *testing.T) {
	c := &Client{}
	mapped := c.mapGRPCError("reconfigure", needsPowerOffErr(t, codes.FailedPrecondition,
		contracts.ErrorInfoDomain, contracts.ReconfigureNeedsPowerOffReason))
	assert.True(t, contracts.IsReconfigureNeedsPowerOff(mapped), "marked: %v", mapped)
	assert.True(t, contracts.IsRetryable(mapped))
	assert.False(t, contracts.IsConflict(mapped))
	assert.Contains(t, mapped.Error(), "power it off")

	for name, err := range map[string]error{
		"another domain's reason": needsPowerOffErr(t, codes.FailedPrecondition, "example.com", contracts.ReconfigureNeedsPowerOffReason),
		"no reason":               needsPowerOffErr(t, codes.FailedPrecondition, contracts.ErrorInfoDomain),
		"another code":            needsPowerOffErr(t, codes.Unknown, contracts.ErrorInfoDomain, contracts.ReconfigureNeedsPowerOffReason),
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, contracts.IsReconfigureNeedsPowerOff(c.mapGRPCError("reconfigure", err)))
		})
	}
}

// TestReconfigureTaskFault_NeverCountsTowardTheBreaker: a vSphere
// ReconfigVM_Task fault or a Proxmox config update PVE failed is answered
// Unknown + VM_OPERATION_FAILED with a constant message: one VM's failing
// reconfigure never opens the Provider's breaker, and the manager shows the
// provider's message (not "could not be reached").
func TestReconfigureTaskFault_NeverCountsTowardTheBreaker(t *testing.T) {
	err := needsPowerOffErr(t, codes.Unknown, contracts.ErrorInfoDomain, contracts.VMOperationFailedReason)
	assert.False(t, isInfraFailure(err))
	assert.False(t, countsTowardBreaker(providerv1.Provider_Reconfigure_FullMethodName, err))
	plain := status.Error(codes.Unknown, "vCenter unreachable")
	assert.True(t, countsTowardBreaker(providerv1.Provider_Reconfigure_FullMethodName, plain), "an unmarked Unknown still counts")
}

// TestReconfigureNeedsPowerOff_NeverCountsTowardTheBreaker: the refusal is the
// provider's answer about one VM, retried on that VM's backoff; it must not
// open the Provider's circuit breaker for every other VM.
func TestReconfigureNeedsPowerOff_NeverCountsTowardTheBreaker(t *testing.T) {
	err := needsPowerOffErr(t, codes.FailedPrecondition, contracts.ErrorInfoDomain, contracts.ReconfigureNeedsPowerOffReason)
	assert.False(t, isInfraFailure(err))
	assert.False(t, countsTowardBreaker(providerv1.Provider_Reconfigure_FullMethodName, err))
}
