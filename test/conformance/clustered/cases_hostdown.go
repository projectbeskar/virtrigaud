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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
				_, err = e.Power(e.HostB(), ok.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				require.NoError(e.T, err)
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
			Source: "internal/transport/grpc/client.go: isInfraFailure (\"one dead host trip[ping] the breaker and fast-fail every VM on every other host\")",
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
	}
}
