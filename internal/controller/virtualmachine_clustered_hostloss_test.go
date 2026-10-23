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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestReconcileVM_Clustered_HostConnectionLossIsPacedAsAnUnavailableHost
// (R25 L3): a Power or Reconfigure of a clustered VM that a clustered provider
// answered "not performed" or "unknown" because its host's connection was lost
// (VM_OPERATION_FAILED with HOST_CONNECTION_LOST) is re-checked like an
// unavailable host, every 30 s, not every 5 s; any other provider error keeps
// the 5 s cadence.
func TestReconcileVM_Clustered_HostConnectionLossIsPacedAsAnUnavailableHost(t *testing.T) {
	lost := func(code codes.Code) error {
		return fmt.Errorf("power failed: %w", routedStatus(t, code, nil, contracts.VMOperationFailedReason, contracts.HostConnectionLostReason))
	}
	other := fmt.Errorf("power failed: %w", routedStatus(t, codes.Unknown, nil, contracts.VMOperationFailedReason))

	for name, tc := range map[string]struct {
		prov *routingProvider
	}{
		"power, not performed": {prov: &routingProvider{
			describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}, powerErr: lost(codes.Unavailable)}},
		"power, unknown": {prov: &routingProvider{
			describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}, powerErr: lost(codes.Unknown)}},
		"reconfigure, unknown": {prov: &routingProvider{
			describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}, reconfigureErr: lost(codes.Unknown)}},
	} {
		t.Run(name, func(t *testing.T) {
			vm := boundClusterVM("lossy")
			cpu, mem := int32(1), int64(4096)
			vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
			r := clusteredFixture(t, tc.prov, vm)
			res, err := r.reconcileVM(context.Background(), getVM(t, r, "lossy"))
			require.NoError(t, err)
			assert.Equal(t, boundHostUnavailableRetryInterval, res.RequeueAfter)
		})
	}

	t.Run("another provider error keeps the 5 s cadence", func(t *testing.T) {
		vm := boundClusterVM("plain")
		r := clusteredFixture(t, &routingProvider{
			describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}, powerErr: other}, vm)
		res, err := r.reconcileVM(context.Background(), getVM(t, r, "plain"))
		require.NoError(t, err)
		assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	})

	t.Run("a single-host VM is not paced by it", func(t *testing.T) {
		assert.Equal(t, providerErrorRetryInterval, routedCallRetryAfter(contracts.VMRef{ID: "x"}, lost(codes.Unknown)))
	})
}
