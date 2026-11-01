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
	"sort"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const srcListVMs = "ADR-0007 Addendum A, A3; proto/provider/v1/provider.proto: ListVMsResponse.unreachable_host_ids, VMInfo.host_id"

// listKey identifies a VM on a clustered provider: (host id, VM id).
type listKey struct{ host, id string }

func listedKeys(vms []*providerv1.VMInfo) map[listKey]int {
	out := map[listKey]int{}
	for _, v := range vms {
		out[listKey{v.GetHostId(), v.GetId()}]++
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func listCases() []Case {
	return []Case{
		{
			ID:     "list.all-hosts",
			Rule:   "ListVMs answers for every host in one call; every VM is tagged with the host it is on, and a VM is identified by (host id, VM id): the same name on two hosts is two VMs",
			Source: srcListVMs,
			Run: func(e *Env) {
				a1 := e.Seed(SeedSpec{Host: e.HostA(), Name: "ls-web", Owner: OwnerA, Running: true})
				a2 := e.Seed(SeedSpec{Host: e.HostA(), Name: "ls-shared", Owner: OwnerB})
				b1 := e.Seed(SeedSpec{Host: e.HostB(), Name: "ls-shared", Owner: OwnerA})
				b2 := e.Seed(SeedSpec{Host: e.HostB(), Name: "ls-unstamped"})
				resp, err := e.ListVMs()
				require.NoError(e.T, err)
				got := listedKeys(resp.Vms)
				for _, vm := range []VM{a1, a2, b1, b2} {
					assert.Equal(e.T, 1, got[listKey{vm.Host, vm.ID}],
						"%s on %s must be listed once, tagged with its host (listed: %v)", vm.Name, vm.Host, got)
				}
				for _, v := range resp.Vms {
					assert.NotEmpty(e.T, v.GetHostId(), "VM %q has no host_id", v.GetId())
				}
				assert.Empty(e.T, resp.UnreachableHostIds, "every host answered")
			},
		},
		{
			ID:     "list.unreachable-host",
			Rule:   "an unreachable host is reported in unreachable_host_ids and never silently dropped; the call still succeeds and returns the VMs of the hosts that answered",
			Source: srcListVMs + " (\"A clustered provider never silently drops a host from the result\")",
			Run: func(e *Env) {
				onA := e.Seed(SeedSpec{Host: e.HostA(), Name: "lsd-a", Owner: OwnerA})
				onB := e.Seed(SeedSpec{Host: e.HostB(), Name: "lsd-b", Owner: OwnerA})
				e.HostDown(e.HostB())
				resp, err := e.ListVMs()
				require.NoError(e.T, err, "a dead host must not fail the whole listing")
				assert.Equal(e.T, []string{e.HostB()}, resp.UnreachableHostIds, "the dead host is reported unknown")
				got := listedKeys(resp.Vms)
				assert.Equal(e.T, 1, got[listKey{onA.Host, onA.ID}], "the healthy host's VM is listed")
				assert.Zero(e.T, got[listKey{onB.Host, onB.ID}], "nothing is invented for the dead host")

				// The manager keeps the distinction between unknown and empty.
				list, err := e.Manager().ListVMs(e.Ctx())
				require.NoError(e.T, err)
				assert.Equal(e.T, []string{e.HostB()}, list.UnreachableHostIDs)
				var hosts []string
				for _, v := range list.VMs {
					hosts = append(hosts, v.HostID)
				}
				assert.Contains(e.T, hosts, e.HostA(), "VMInfo.HostID is mapped")
			},
		},
		{
			ID:     "list.all-unreachable",
			Rule:   "when no host answers, ListVMs still succeeds, lists every host as unreachable and invents no VM",
			Source: srcListVMs,
			Run: func(e *Env) {
				e.Seed(SeedSpec{Host: e.HostA(), Name: "lsx-a", Owner: OwnerA})
				for _, h := range e.Hosts() {
					e.HostDown(h)
				}
				resp, err := e.ListVMs()
				require.NoError(e.T, err, "host-scoped failures never fail the call")
				assert.Equal(e.T, sortedCopy(e.Hosts()), sortedCopy(resp.UnreachableHostIds))
				assert.Empty(e.T, resp.Vms)
			},
		},
		{
			ID:     "list.recovery",
			Rule:   "once a host answers again it is no longer reported unreachable and its VMs are listed",
			Source: srcListVMs,
			Run: func(e *Env) {
				onB := e.Seed(SeedSpec{Host: e.HostB(), Name: "lsr-b", Owner: OwnerA})
				e.HostDown(e.HostB())
				resp, err := e.ListVMs()
				require.NoError(e.T, err)
				require.Equal(e.T, []string{e.HostB()}, resp.UnreachableHostIds)
				e.HostUp(e.HostB())
				resp, err = e.ListVMs()
				require.NoError(e.T, err)
				assert.Empty(e.T, resp.UnreachableHostIds)
				assert.Equal(e.T, 1, listedKeys(resp.Vms)[listKey{onB.Host, onB.ID}], "the recovered host's VM is listed again")
			},
		},
		{
			ID:     "list.not-counted-against-provider",
			Rule:   "an unreachable host in ListVMs never counts toward the manager's circuit breaker (the call succeeds)",
			Source: srcListVMs + "; internal/providers/contracts/errors.go: HostUnavailableReason (a host-scoped failure is kept out of the Provider's circuit breaker)",
			Run: func(e *Env) {
				e.Seed(SeedSpec{Host: e.HostA(), Name: "lsb-a", Owner: OwnerA})
				e.HostDown(e.HostB())
				mgr, cb := e.ManagerWithBreaker(2)
				for i := 0; i < 5; i++ {
					list, err := mgr.ListVMs(e.Ctx())
					require.NoError(e.T, err, "call %d", i)
					assert.Equal(e.T, []string{e.HostB()}, list.UnreachableHostIDs)
				}
				assert.Equal(e.T, "closed", cb.GetState().String())
			},
		},
	}
}
