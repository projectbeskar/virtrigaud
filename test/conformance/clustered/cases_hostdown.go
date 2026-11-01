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

package clustered

import (
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const srcHostUnavailable = "ADR-0007 Addendum A, A1 and A2 (an unreachable host is a retryable Unavailable, never re-scheduled); internal/providers/contracts: HostUnavailableReason, HostUnavailableHostKey"

func hostDownCases() []Case {
	return []Case{
		{
			ID:     "hostdown.per-vm",
			Rule:   "a per-VM call routed to an unreachable host is codes.Unavailable with a HOST_UNAVAILABLE ErrorInfo naming that host: never NotFound (the VM is not gone), never a task result",
			Source: srcHostUnavailable,
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "down-vm", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				e.HostDown(e.HostA())
				for _, c := range perVMCalls() {
					err := c.call(e, e.HostA(), vm, OwnerA)
					requireHostUnavailable(e, err, e.HostA(), c.name+" on an unreachable host")
				}
				e.HostUp(e.HostA())
				requireUnchanged(e, vm, before, "calls on an unreachable host")
			},
		},
		{
			ID:     "hostdown.isolated",
			Rule:   "one unreachable host does not affect calls on the other hosts",
			Source: srcHostUnavailable + "; ADR-0007 D8 (failure handling is per host)",
			Run: func(e *Env) {
				ok := e.Seed(SeedSpec{Host: e.HostB(), Name: "down-peer", Owner: OwnerA, Running: false})
				e.HostDown(e.HostA())
				resp, err := e.Describe(e.HostB(), ok.ID, OwnerA)
				require.NoError(e.T, err, "a healthy host answers while another is down")
				assert.True(e.T, resp.Exists)
				pw, err := e.Power(e.HostB(), ok.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				require.NoError(e.T, err)
				e.Settle(pw)
				assert.True(e.T, e.Observe(ok).Running, "power on landed on the healthy host")
			},
		},
		{
			ID:     "hostdown.recovery",
			Rule:   "when the host comes back its VMs are found again, unchanged, and calls succeed: nothing was re-created or dropped meanwhile",
			Source: "ADR-0007 Addendum A, A4 (no automatic failover or re-creation) and D8",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "down-recover", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				e.HostDown(e.HostA())
				_, err := e.Describe(e.HostA(), vm.ID, OwnerA)
				requireHostUnavailable(e, err, e.HostA(), "Describe while down")
				e.HostUp(e.HostA())
				resp, err := e.Describe(e.HostA(), vm.ID, OwnerA)
				require.NoError(e.T, err, "the host answers again")
				assert.True(e.T, resp.Exists, "the VM is found again")
				requireUnchanged(e, vm, before, "a host outage")
			},
		},
		{
			ID:     "hostdown.not-counted-against-provider",
			Rule:   "a host-scoped unavailability is not an infrastructure failure of the Provider: the manager's circuit breaker stays closed and calls on healthy hosts still go through",
			Source: "internal/providers/contracts/errors.go: HostUnavailableReason (the manager \"keeps it out of its circuit breaker\")",
			Run: func(e *Env) {
				healthy := e.Seed(SeedSpec{Host: e.HostB(), Name: "down-breaker", Owner: OwnerA, Running: true})
				down := e.Seed(SeedSpec{Host: e.HostA(), Name: "down-breaker-a", Owner: OwnerA, Running: true})
				e.HostDown(e.HostA())
				mgr, cb := e.ManagerWithBreaker(3)
				for i := 0; i < 6; i++ {
					_, err := mgr.Describe(e.Ctx(), contracts.VMRef{ID: down.ID, HostID: down.Host, Owner: contracts.ObjectIdentity(OwnerA)})
					require.Error(e.T, err)
					assert.True(e.T, contracts.IsHostUnavailable(err), "call %d: %v", i, err)
				}
				resp, err := mgr.Describe(e.Ctx(), contracts.VMRef{ID: healthy.ID, HostID: healthy.Host, Owner: contracts.ObjectIdentity(OwnerA)})
				require.NoError(e.T, err, "the breaker must not have opened for a host-scoped failure")
				assert.True(e.T, resp.Exists)
				assert.Equal(e.T, "closed", cb.GetState().String())
			},
		},
		{
			ID:       "hostdown.blackholed-list",
			Rule:     "a host that accepts connections and never answers does not stall ListVMs: the call answers inside the caller's deadline and reports that host unreachable",
			Source:   "proto/provider/v1/provider.proto: ListVMsResponse.unreachable_host_ids (\"did not answer within its per-host deadline\"); ADR-0007 Addendum A, A3",
			Requires: []Feature{FeatureHostBlackhole},
			Run: func(e *Env) {
				onA := e.Seed(SeedSpec{Host: e.HostA(), Name: "bh-a", Owner: OwnerA})
				e.Seed(SeedSpec{Host: e.HostB(), Name: "bh-b", Owner: OwnerA})
				hb, ok := e.Target.(HostBlackholer)
				require.True(e.T, ok)
				require.NoError(e.T, hb.SetHostBlackholed(e.HostB(), true))
				e.T.Cleanup(func() { _ = hb.SetHostBlackholed(e.HostB(), false) })
				const deadline = 8 * time.Second
				ctx, cancel := contextWithTimeout(e, deadline)
				defer cancel()
				start := time.Now()
				resp, err := e.Raw().ListVMs(ctx, &providerv1.ListVMsRequest{})
				require.NoError(e.T, err, "a hung host must not fail or stall the whole listing")
				assert.Less(e.T, time.Since(start), deadline, "the answer arrives inside the caller's deadline")
				assert.Equal(e.T, []string{e.HostB()}, resp.UnreachableHostIds)
				assert.Equal(e.T, 1, listedKeys(resp.Vms)[listKey{onA.Host, onA.ID}], "the healthy host's VM is listed")
			},
		},
		{
			ID:       "hostdown.blackholed-per-vm",
			Level:    Recommended,
			Rule:     "a per-VM call to a host that never answers fails inside the caller's deadline with the host-scoped Unavailable, not a bare deadline error",
			Source:   "ADR-0007 Addendum A, A1 and A2 (an unreachable host is a retryable Unavailable); the contract does not say how a hung host is told from a dead one",
			Requires: []Feature{FeatureHostBlackhole},
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "bh-vm", Owner: OwnerA, Running: true})
				hb, ok := e.Target.(HostBlackholer)
				require.True(e.T, ok)
				require.NoError(e.T, hb.SetHostBlackholed(e.HostA(), true))
				e.T.Cleanup(func() { _ = hb.SetHostBlackholed(e.HostA(), false) })
				const deadline = 6 * time.Second
				ctx, cancel := contextWithTimeout(e, deadline)
				defer cancel()
				start := time.Now()
				_, err := e.Raw().Describe(ctx, &providerv1.DescribeRequest{Id: vm.ID, TargetHostId: vm.Host, Owner: OwnerA.Proto()})
				require.Error(e.T, err)
				assert.Less(e.T, time.Since(start), deadline+time.Second)
				requireHostUnavailable(e, err, vm.Host, "Describe on a host that never answers")
			},
		},
		{
			ID:       "hostdown.operation-failed",
			Rule:     "an operation that reaches its host and fails there carries VM_OPERATION_FAILED, is not NotFound or host-unavailable, changes nothing, and does not count toward the Provider's circuit breaker",
			Source:   "internal/providers/contracts/errors.go: VMOperationFailedReason (\"the manager keeps it out of its per-Provider circuit breaker\")",
			Requires: []Feature{FeatureHostFaults},
			Run: func(e *Env) {
				bad := e.Seed(SeedSpec{Host: e.HostA(), Name: "opfail-a", Owner: OwnerA, Running: false})
				good := e.Seed(SeedSpec{Host: e.HostB(), Name: "opfail-b", Owner: OwnerA, Running: false})
				hf, ok := e.Target.(HostFaulter)
				require.True(e.T, ok)
				require.NoError(e.T, hf.FailHostOperations(e.HostA(), true))
				e.T.Cleanup(func() { _ = hf.FailHostOperations(e.HostA(), false) })
				before := e.Observe(bad)
				_, err := e.Power(bad.Host, bad.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				w := WireOf(err)
				require.Error(e.T, err)
				_, ok = w.Reason(contracts.VMOperationFailedReason)
				assert.True(e.T, ok, "must carry %s: %s", contracts.VMOperationFailedReason, w)
				assert.NotEqual(e.T, codes.NotFound, w.Code, "the VM is not gone: %s", w)
				requireNoReason(e, w, contracts.HostUnavailableReason, "an operation that failed on its host")
				requireUnchanged(e, bad, before, "a failed power on")

				mgr, cb := e.ManagerWithBreaker(2)
				for i := 0; i < 4; i++ {
					_, err := mgr.Power(e.Ctx(), contracts.VMRef{ID: bad.ID, HostID: bad.Host, Owner: contracts.ObjectIdentity(OwnerA)}, contracts.PowerOpOn)
					require.Error(e.T, err)
					assert.False(e.T, contracts.IsNotFound(err))
				}
				_, err = mgr.Power(e.Ctx(), contracts.VMRef{ID: good.ID, HostID: good.Host, Owner: contracts.ObjectIdentity(OwnerA)}, contracts.PowerOpOn)
				require.NoError(e.T, err, "one VM's failing operation must not open the breaker for the others")
				assert.Equal(e.T, "closed", cb.GetState().String())
			},
		},
		{
			ID:       "hostdown.connection-lost",
			Rule:     "a host whose connection dies while it runs an operation that changes a VM is answered with HOST_CONNECTION_LOST together with VM_OPERATION_FAILED (never NotFound, never success); repeating the operation once the host answers succeeds",
			Source:   "internal/providers/contracts/errors.go: HostConnectionLostReason, OutcomeUnknownRetryReason, VMOperationFailedReason",
			Requires: []Feature{FeatureHostFaults},
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "lost-vm", Owner: OwnerA, Running: true})
				hf, ok := e.Target.(HostFaulter)
				require.True(e.T, ok)
				require.NoError(e.T, hf.DropHostDuringOperations(e.HostA(), true))
				e.T.Cleanup(func() { _ = hf.DropHostDuringOperations(e.HostA(), false) })
				_, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
				require.Error(e.T, err)
				w := WireOf(err)
				assert.NotEqual(e.T, codes.NotFound, w.Code, w.String())
				_, lost := w.Reason(contracts.HostConnectionLostReason)
				_, failed := w.Reason(contracts.VMOperationFailedReason)
				assert.True(e.T, lost, "must carry %s: %s", contracts.HostConnectionLostReason, w)
				assert.True(e.T, failed, "must carry %s with it: %s", contracts.VMOperationFailedReason, w)
				_, err = e.Manager().Power(e.Ctx(), contracts.VMRef{ID: vm.ID, HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)}, contracts.PowerOpOff)
				assert.True(e.T, contracts.IsHostConnectionLost(err), "manager mapping: %v", err)

				require.NoError(e.T, hf.DropHostDuringOperations(e.HostA(), false))
				resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
				require.NoError(e.T, err, "the retry succeeds")
				e.Settle(resp)
				assert.False(e.T, e.Observe(vm).Running)
			},
		},
		{
			ID:       "hostdown.removed-host",
			Rule:     "a host removed from the Provider's registry while VMs are bound to it is an unavailable host, not a missing VM: calls to it are the host-scoped Unavailable, its VMs are untouched, the other hosts are unaffected, and a task naming it is NotFound",
			Source:   "ADR-0007 Addendum A, A1 (\"An unknown or removed host returns a retryable Unavailable\"), A4 (never re-create a clustered VM); docs/clustered-provider-inventory.md (host-encoded task references)",
			Requires: []Feature{FeatureHostRemoval},
			Run: func(e *Env) {
				bound := e.Seed(SeedSpec{Host: e.HostA(), Name: "rm-bound", Owner: OwnerA, Running: true})
				peer := e.Seed(SeedSpec{Host: e.HostB(), Name: "rm-peer", Owner: OwnerA, Running: true})
				before := e.Observe(bound)
				hr, ok := e.Target.(HostRemover)
				require.True(e.T, ok)
				require.NoError(e.T, hr.RemoveHost(e.HostA()))
				for _, c := range perVMCalls() {
					err := c.call(e, bound.Host, bound, OwnerA)
					requireHostUnavailable(e, err, "", c.name+" on a removed host")
				}
				requireUnchanged(e, bound, before, "calls on a removed host")
				resp, err := e.Describe(peer.Host, peer.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists, "the other host is unaffected")
				_, err = e.TaskStatus(HostTaskPrefix + bound.Host + "/job-1")
				requireCode(e, err, codes.NotFound, "a task naming the removed host")
			},
		},
	}
}
