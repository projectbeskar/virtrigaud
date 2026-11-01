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
	"strings"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Literal wire values: the suite pins the strings themselves, not only the
// constants of internal/providers/contracts, so a change to either side is
// noticed.
const (
	wireHostUnavailableReason = "HOST_UNAVAILABLE"
	wireErrorInfoDomain       = "provider.virtrigaud.io"
)

func capabilityCases() []Case {
	return []Case{
		{
			ID:     "caps.clustering",
			Rule:   "a clustered provider reports supports_clustering, and the same capabilities on every call",
			Source: "proto/provider/v1/provider.proto: GetCapabilitiesResponse.supports_clustering; ADR-0007 D7",
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				first, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				require.NoError(e.T, err)
				assert.True(e.T, first.SupportsClustering, "a clustered provider says so")
				second, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				require.NoError(e.T, err)
				assert.True(e.T, proto.Equal(first, second), "capabilities are stable between calls")
				caps, err := e.Manager().GetCapabilities(e.Ctx())
				require.NoError(e.T, err)
				assert.True(e.T, caps.SupportsClustering, "the manager reads it")
			},
		},
		{
			ID:     "caps.advertised-rpcs-are-routed",
			Rule:   "an RPC the provider advertises is served and requires a host: the same otherwise valid request is refused InvalidArgument about the host when it names none, and is not Unimplemented or refused for its host when it names one",
			Source: "ADR-0007 D7 (capability-gated, honesty-first) and Addendum A, A1; proto/provider/v1/provider.proto: SnapshotCreateRequest, ExportDiskRequest, CloneRequest host fields",
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				caps, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				require.NoError(e.T, err)
				backend := "s3"
				if len(caps.SupportedExportBackends) > 0 {
					backend = caps.SupportedExportBackends[0]
				}
				type call func(host string) error
				calls := map[string]call{}
				if caps.SupportsSnapshots {
					calls["SnapshotCreate"] = func(h string) error {
						_, err := e.Raw().SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "x", NameHint: "s", TargetHostId: h, Owner: OwnerA.Proto()})
						return err
					}
					calls["SnapshotDelete"] = func(h string) error {
						_, err := e.Raw().SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "x", SnapshotId: "s", TargetHostId: h, Owner: OwnerA.Proto()})
						return err
					}
					calls["SnapshotRevert"] = func(h string) error {
						_, err := e.Raw().SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: "x", SnapshotId: "s", TargetHostId: h, Owner: OwnerA.Proto()})
						return err
					}
				}
				if caps.SupportsDiskExport {
					calls["ExportDisk"] = func(h string) error {
						_, err := e.Raw().ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "x", Owner: OwnerA.Proto(), TargetHostId: h,
							BackendType: backend, TransferMode: "relay", Format: "qcow2", DestinationUrl: "s3://bucket/key"})
						return err
					}
				}
				if caps.SupportsRoutedClone {
					calls["Clone"] = func(h string) error {
						_, err := e.Raw().Clone(ctx, &providerv1.CloneRequest{SourceVmId: "x", TargetName: "y", SourceHostId: h, TargetHostId: h, SourceOwner: OwnerA.Proto()})
						return err
					}
				}
				if len(calls) == 0 {
					e.T.Skipf("the provider advertises no routed per-VM capability, so there is nothing to check")
				}
				for name, do := range calls {
					w := requireCode(e, do(""), codes.InvalidArgument, name+" without a host")
					assert.Contains(e.T, strings.ToLower(w.Message), "host", "%s: the refusal must be about the missing host: %s", name, w)
					if err := do(e.HostA()); err != nil {
						w := WireOf(err)
						assert.NotEqual(e.T, codes.Unimplemented, w.Code, "%s is advertised but not served: %s", name, w)
						if w.Code == codes.InvalidArgument {
							assert.NotContains(e.T, strings.ToLower(w.Message), "target_host_id", "%s with a host is still refused for its host: %s", name, w)
						}
					}
				}
				e.T.Logf("capability-advertised RPCs checked: %d", len(calls))
			},
		},
	}
}

func errorMappingCases() []Case {
	return []Case{
		{
			ID:     "errinfo.host-unavailable-wire-and-mapping",
			Rule:   "the wire carries google.rpc.ErrorInfo{reason: HOST_UNAVAILABLE, domain: provider.virtrigaud.io}, with metadata.host naming a known host that is unreachable; the manager maps it to a retryable HostUnavailable that is not NotFound, and keeps that host id",
			Source: "internal/providers/contracts/errors.go: HostUnavailableReason, ErrorInfoDomain; internal/providers/contracts/host_unavailable.go: HostUnavailableHostKey, UnreachableHost",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "map-vm", Owner: OwnerA, Running: true})
				ref := func(host string) contracts.VMRef {
					return contracts.VMRef{ID: vm.ID, HostID: host, Owner: contracts.ObjectIdentity(OwnerA)}
				}
				check := func(what string, err error) {
					require.Error(e.T, err, what)
					assert.True(e.T, contracts.IsHostUnavailable(err), "%s: %v", what, err)
					assert.True(e.T, contracts.IsRetryable(err), "%s: retryable", what)
					assert.False(e.T, contracts.IsNotFound(err), "%s: an unreachable host never says the VM is gone", what)
				}

				// An unknown host: the reason and domain, as literals.
				_, err := e.Describe(UnknownHost, vm.ID, OwnerA)
				w := requireCode(e, err, codes.Unavailable, "unknown host")
				r, ok := w.Reason(wireHostUnavailableReason)
				require.True(e.T, ok, "%s", w)
				assert.Equal(e.T, wireErrorInfoDomain, r.Domain)
				_, err = e.Manager().Describe(e.Ctx(), ref(UnknownHost))
				check("Describe on an unknown host", err)
				_, err = e.Manager().Delete(e.Ctx(), ref(UnknownHost))
				check("Delete on an unknown host", err)
				_, err = e.Manager().Power(e.Ctx(), ref(UnknownHost), contracts.PowerOpOn)
				check("Power on an unknown host", err)

				// A known host that is unreachable: also the host's id.
				e.HostDown(e.HostA())
				_, err = e.Describe(e.HostA(), vm.ID, OwnerA)
				w = requireCode(e, err, codes.Unavailable, "unreachable host")
				r, ok = w.Reason(wireHostUnavailableReason)
				require.True(e.T, ok, "%s", w)
				assert.Equal(e.T, wireErrorInfoDomain, r.Domain)
				assert.Equal(e.T, e.HostA(), r.Metadata["host"], "the ErrorInfo names the unreachable host")
				_, err = e.Manager().Describe(e.Ctx(), ref(e.HostA()))
				check("Describe on an unreachable host", err)
				assert.Equal(e.T, e.HostA(), contracts.UnreachableHost(err), "the host id survives the mapping")
			},
		},
		{
			ID:       "errinfo.provider-level-unavailable",
			Rule:     "when the Provider itself cannot serve, its Unavailable is not host-scoped (no HOST_UNAVAILABLE), so the manager counts it as the Provider's failure",
			Source:   "internal/providers/contracts/errors.go: HostUnavailableReason (only an unavailability \"scoped to one host\" is kept out of the circuit breaker)",
			Requires: []Feature{FeatureProviderNotReady},
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "notready", Owner: OwnerA, Running: true})
				ctl, ok := e.Target.(ProviderReadiness)
				require.True(e.T, ok, "a target offering %s implements ProviderReadiness", FeatureProviderNotReady)
				require.NoError(e.T, ctl.SetProviderReady(false))
				e.T.Cleanup(func() { _ = ctl.SetProviderReady(true) })
				_, err := e.Describe(vm.Host, vm.ID, OwnerA)
				w := requireCode(e, err, codes.Unavailable, "a Provider that is not ready")
				requireNoReason(e, w, contracts.HostUnavailableReason, "a Provider that is not ready")
				_, err = e.Manager().Describe(e.Ctx(), contracts.VMRef{ID: vm.ID, HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)})
				require.Error(e.T, err)
				assert.False(e.T, contracts.IsHostUnavailable(err), "not host-scoped: %v", err)
				assert.False(e.T, contracts.IsNotFound(err), "the VM is not gone: %v", err)
				assert.True(e.T, contracts.IsRetryable(err), "retryable: %v", err)
			},
		},
	}
}

func idempotencyCases() []Case {
	return []Case{
		{
			ID:     "idempotency.reads",
			Rule:   "Describe and ListVMs are read-only: repeated calls give the same answer and change nothing",
			Source: "ADR-0007 Addendum A, A3 and A4 (reads never create or move anything)",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "idem-read", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				first, err := e.Describe(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				list1, err := e.ListVMs()
				require.NoError(e.T, err)
				for i := 0; i < 3; i++ {
					again, err := e.Describe(vm.Host, vm.ID, OwnerA)
					require.NoError(e.T, err)
					assert.Equal(e.T, first.Exists, again.Exists)
					assert.Equal(e.T, first.PowerState, again.PowerState)
					list2, err := e.ListVMs()
					require.NoError(e.T, err)
					assert.Equal(e.T, listedKeys(list1.Vms), listedKeys(list2.Vms))
				}
				requireUnchanged(e, vm, before, "repeated reads")
			},
		},
		{
			ID:     "idempotency.power",
			Rule:   "a power operation repeated for a state the VM is already in succeeds (the manager re-sends after a lost answer)",
			Source: "docs/provider-call-outcomes.md (\"on, off and shutdown can be repeated: a second attempt finds the domain already in the state asked for\")",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "idem-power", Owner: OwnerA, Running: false})
				for i := 0; i < 2; i++ {
					resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
					require.NoError(e.T, err, "power on, attempt %d", i+1)
					e.Settle(resp)
					assert.True(e.T, e.Observe(vm).Running)
				}
				for i := 0; i < 2; i++ {
					resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
					require.NoError(e.T, err, "power off, attempt %d", i+1)
					e.Settle(resp)
					assert.False(e.T, e.Observe(vm).Running)
				}
			},
		},
		{
			ID:     "idempotency.delete",
			Rule:   "a repeated Delete is a success or NotFound (the VM is gone), never another error; and it never deletes a different VM that has since taken the name",
			Source: "proto/provider/v1/provider.proto: DeleteRequest.owner (a VM that is not there or not the requester's is NOT_FOUND); docs/provider-call-outcomes.md (a Delete can be repeated)",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "idem-delete", Owner: OwnerA, Running: false})
				resp, err := e.Delete(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				e.Settle(resp)
				assert.False(e.T, e.Observe(vm).Exists)
				resp, err = e.Delete(vm.Host, vm.ID, OwnerA)
				if err != nil {
					requireCode(e, err, codes.NotFound, "second Delete")
				}
				e.Settle(resp)

				// Another tenant now has a VM of that name on the host. The check is
				// only meaningful when that VM is addressed by the same id as the one
				// deleted (ids derived from names); where the Target gives a new VM a
				// new id, the repeated Delete cannot reach it and the check is skipped.
				taken := e.Seed(SeedSpec{Host: e.HostA(), Name: "idem-delete", Owner: OwnerB, Running: true})
				if taken.ID != vm.ID {
					e.T.Logf("coverage reduced: the new VM has another id (%s), so the repeated Delete cannot address it", taken.ID)
					return
				}
				before := e.Observe(taken)
				_, err = e.Delete(vm.Host, vm.ID, OwnerA)
				requireCode(e, err, codes.NotFound, "Delete after the name was taken by another tenant")
				requireUnchanged(e, taken, before, "a repeated Delete")
			},
		},
		{
			ID:     "idempotency.host-unavailable",
			Rule:   "repeated calls to an unreachable host keep giving the same host-scoped answer and change nothing",
			Source: srcHostUnavailable,
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "idem-down", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				e.HostDown(e.HostA())
				for i := 0; i < 3; i++ {
					_, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
					requireHostUnavailable(e, err, vm.Host, "Power on an unreachable host")
					_, err = e.Delete(vm.Host, vm.ID, OwnerA)
					requireHostUnavailable(e, err, vm.Host, "Delete on an unreachable host")
				}
				e.HostUp(e.HostA())
				requireUnchanged(e, vm, before, "repeated calls to an unreachable host")
			},
		},
	}
}

func restartCases() []Case {
	const srcRestart = "docs/adr/0007 D1 guardrail (\"on provider restart the provider re-reads its connection config and reconnects. If the provider crashes, no decision or binding is lost, because none of it lived there\")"
	return []Case{
		{
			ID:     "restart.state-lives-on-hosts",
			Rule:   "after the provider restarts, its VMs are found on their hosts unchanged, ListVMs lists them, other tenants' VMs are still refused, and the capabilities are the same",
			Source: srcRestart,
			Run: func(e *Env) {
				mine := e.Seed(SeedSpec{Host: e.HostA(), Name: "rs-mine", Owner: OwnerA, Running: true})
				theirs := e.Seed(SeedSpec{Host: e.HostB(), Name: "rs-theirs", Owner: OwnerB, Running: true})
				ctx, cancel := e.Call()
				capsBefore, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				cancel()
				require.NoError(e.T, err)
				beforeMine, beforeTheirs := e.Observe(mine), e.Observe(theirs)

				e.RestartProvider()

				resp, err := e.Describe(mine.Host, mine.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists, "the VM is still known on its host")
				list, err := e.ListVMs()
				require.NoError(e.T, err)
				got := listedKeys(list.Vms)
				assert.Equal(e.T, 1, got[listKey{mine.Host, mine.ID}])
				assert.Equal(e.T, 1, got[listKey{theirs.Host, theirs.ID}])
				assert.Empty(e.T, list.UnreachableHostIds)
				// Owner stamps live on the hosts: the check still holds.
				_, err = e.Delete(theirs.Host, theirs.ID, OwnerA)
				requireCode(e, err, codes.NotFound, "Delete of another tenant's VM after a restart")
				requireUnchanged(e, mine, beforeMine, "a restart")
				requireUnchanged(e, theirs, beforeTheirs, "a restart and a refused delete")

				ctx, cancel = e.Call()
				capsAfter, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				cancel()
				require.NoError(e.T, err)
				assert.True(e.T, proto.Equal(capsBefore, capsAfter), "capabilities do not change across a restart")
			},
		},
		{
			ID:     "restart.no-resurrection",
			Rule:   "a VM deleted before a restart is still gone after it: the provider has no memory that could bring it back",
			Source: srcRestart + "; ADR-0007 Addendum A, A4",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "rs-gone", Owner: OwnerA, Running: false})
				resp, err := e.Delete(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				e.Settle(resp)
				e.RestartProvider()
				dresp, err := e.Describe(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				assert.False(e.T, dresp.Exists)
				list, err := e.ListVMs()
				require.NoError(e.T, err)
				assert.Zero(e.T, listedKeys(list.Vms)[listKey{vm.Host, vm.ID}])
				assert.False(e.T, e.Observe(vm).Exists)
			},
		},
		{
			ID:     "restart.host-down-across-restart",
			Rule:   "a host that is unreachable when the provider restarts is reported unreachable, not dropped, and its VMs are found again when it returns",
			Source: srcRestart + "; ADR-0007 Addendum A, A3",
			Run: func(e *Env) {
				onB := e.Seed(SeedSpec{Host: e.HostB(), Name: "rs-down", Owner: OwnerA})
				e.HostDown(e.HostB())
				e.RestartProvider()
				list, err := e.ListVMs()
				require.NoError(e.T, err)
				assert.Equal(e.T, []string{e.HostB()}, list.UnreachableHostIds)
				e.HostUp(e.HostB())
				resp, err := e.Describe(onB.Host, onB.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists)
			},
		},
		{
			ID:     "restart.task-routing",
			Rule:   "after a restart, task references are routed against the same host registry as before: a host it fronts that is unreachable is the host-scoped Unavailable, and a host it does not front is NotFound",
			Source: srcRestart + "; " + srcTaskRefs,
			Run: func(e *Env) {
				e.HostDown(e.HostA())
				down := HostTaskPrefix + e.HostA() + "/job-1"
				unknown := HostTaskPrefix + UnknownHost + "/job-1"
				answers := func(when string) {
					_, err := e.TaskStatus(down)
					requireHostUnavailable(e, err, e.HostA(), "task of an unreachable host "+when)
					_, err = e.TaskStatus(unknown)
					requireCode(e, err, codes.NotFound, "task of an unknown host "+when)
				}
				answers("before the restart")
				e.RestartProvider()
				answers("after the restart")
			},
		},
		{
			ID:       "restart.task-ref",
			Rule:     "a task reference issued before a restart is still routed afterwards: it names its host, and the provider keeps no task table that a restart could lose",
			Source:   srcRestart + "; " + srcTaskRefs,
			Requires: []Feature{FeatureAsyncTasks},
			Run: func(e *Env) {
				ctl, ok := e.Target.(TaskController)
				require.True(e.T, ok)
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "rs-task", Owner: OwnerA})
				resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				require.NoError(e.T, err)
				ref := resp.GetTask().GetId()
				require.NotEmpty(e.T, ref)
				e.RestartProvider()
				st, err := e.TaskStatus(ref)
				require.NoError(e.T, err, "a task of a host the provider fronts is answered after a restart")
				assert.False(e.T, st.Done)
				require.NoError(e.T, ctl.FinishTasks())
				st, err = e.TaskStatus(ref)
				require.NoError(e.T, err)
				assert.True(e.T, st.Done)
			},
		},
	}
}

func providerDownCases() []Case {
	return []Case{
		{
			ID:     "providerdown.outage-and-recovery",
			Rule:   "while the Provider is down a call fails as a retryable Provider-level Unavailable (not host-scoped, not NotFound); when it is back, its VMs answer as before",
			Source: "ADR-0007 D1 (a Provider failure cannot lose cluster state); internal/providers/contracts/errors.go: HostUnavailableReason (only a host-scoped Unavailable carries it)",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "pd-vm", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				e.StopProvider()

				ctx, cancel := e.Call()
				_, err := e.Raw().Describe(ctx, &providerv1.DescribeRequest{Id: vm.ID, TargetHostId: vm.Host, Owner: OwnerA.Proto()})
				cancel()
				w := requireCode(e, err, codes.Unavailable, "a Provider that is down")
				requireNoReason(e, w, contracts.HostUnavailableReason, "a Provider that is down")

				short, cancel := contextWithTimeout(e, time.Second)
				_, err = e.Manager().Describe(short, contracts.VMRef{ID: vm.ID, HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)})
				cancel()
				require.Error(e.T, err)
				assert.True(e.T, contracts.IsRetryable(err), "an outage is retried: %v", err)
				assert.False(e.T, contracts.IsHostUnavailable(err), "an outage is not host-scoped: %v", err)
				assert.False(e.T, contracts.IsNotFound(err), "an outage never says the VM is gone: %v", err)

				e.StartProvider()
				resp, err := e.Describe(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists, "the VM is still there after the outage")
				requireUnchanged(e, vm, before, "a Provider outage")
			},
		},
	}
}

func hostInventoryCases() []Case {
	const srcHosts = "proto/provider/v1/provider.proto: ListHostsResponse, HostInfo, HostHealth; ADR-0007 D1, D3"
	return []Case{
		{
			ID:       "hosts.inventory",
			Rule:     "ListHosts reports exactly the hosts the provider fronts, Ready; an unreachable host is still reported, NotReady, and GetHostInfo refuses an unknown host",
			Source:   srcHosts,
			Requires: []Feature{FeatureHostInventory},
			Run: func(e *Env) {
				ids := func() ([]string, map[string]providerv1.HostHealth) {
					ctx, cancel := e.Call()
					defer cancel()
					resp, err := e.Raw().ListHosts(ctx, &providerv1.ListHostsRequest{})
					require.NoError(e.T, err)
					var out []string
					health := map[string]providerv1.HostHealth{}
					for _, h := range resp.Hosts {
						out = append(out, h.Id)
						health[h.Id] = h.Health
					}
					sort.Strings(out)
					return out, health
				}
				got, health := ids()
				assert.Equal(e.T, sortedCopy(e.Hosts()), got, "the fronted hosts, no more and no fewer")
				for _, h := range e.Hosts() {
					assert.Equal(e.T, providerv1.HostHealth_HOST_HEALTH_READY, health[h], "%s is reachable", h)
				}

				e.HostDown(e.HostB())
				// A provider may cache a host's health for a short while: NotReady
				// has to show within a bound, not instantly.
				deadline := time.Now().Add(3 * time.Second)
				for {
					got, health = ids()
					if health[e.HostB()] == providerv1.HostHealth_HOST_HEALTH_NOT_READY || time.Now().After(deadline) {
						break
					}
					time.Sleep(250 * time.Millisecond)
				}
				assert.Equal(e.T, sortedCopy(e.Hosts()), got, "an unreachable host is still reported")
				assert.Equal(e.T, providerv1.HostHealth_HOST_HEALTH_NOT_READY, health[e.HostB()], "NotReady within 3 s")
				assert.Equal(e.T, providerv1.HostHealth_HOST_HEALTH_READY, health[e.HostA()])

				ctx, cancel := e.Call()
				defer cancel()
				_, err := e.Raw().GetHostInfo(ctx, &providerv1.GetHostInfoRequest{HostId: UnknownHost})
				require.Error(e.T, err, "an unknown host has no info")
			},
		},
	}
}

func createCases() []Case {
	return []Case{
		{
			ID:       "create.requires-routable-host",
			Rule:     "Create without a host id is InvalidArgument and with an unknown or unreachable host is the host-scoped Unavailable; no VM is made in either case, and once the host is back Create works",
			Source:   "proto/provider/v1/provider.proto: CreateRequest.target_host_id; ADR-0007 Addendum A, A2",
			Requires: []Feature{FeatureCreate},
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				_, err := e.Raw().Create(ctx, e.CreateRequest("cr-none", "", OwnerA))
				requireCode(e, err, codes.InvalidArgument, "Create without a host")
				_, err = e.Raw().Create(ctx, e.CreateRequest("cr-none", UnknownHost, OwnerA))
				requireHostUnavailable(e, err, "", "Create on an unknown host")
				e.HostDown(e.HostB())
				_, err = e.Raw().Create(ctx, e.CreateRequest("cr-none", e.HostB(), OwnerA))
				requireHostUnavailable(e, err, "", "Create on an unreachable host")
				e.HostUp(e.HostB())

				list, err := e.ListVMs()
				require.NoError(e.T, err)
				for _, v := range list.Vms {
					assert.NotContains(e.T, v.GetName()+v.GetId(), "cr-none", "a refused Create made a VM")
				}
				resp, err := e.Raw().Create(ctx, e.CreateRequest("cr-none", e.HostB(), OwnerA))
				require.NoError(e.T, err, "Create works once the host is back")
				assert.NotEmpty(e.T, resp.Id)
			},
		},
		{
			ID:       "create.unavailable-names-host",
			Level:    Recommended,
			Rule:     "the HOST_UNAVAILABLE answer of a Create to a known host that is unreachable names that host in its ErrorInfo metadata, as the per-VM calls' answers do",
			Source:   "internal/providers/contracts/host_unavailable.go: HostUnavailableHostKey (set when the host that could not be reached is the one the call was routed to); the manager reads the key only on the Delete path, so a Create answer without it loses nothing today",
			Requires: []Feature{FeatureCreate},
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				e.HostDown(e.HostB())
				_, err := e.Raw().Create(ctx, e.CreateRequest("cr-names", e.HostB(), OwnerA))
				requireHostUnavailable(e, err, e.HostB(), "Create on an unreachable host")
			},
		},
		{
			ID:       "create.routed-to-target-host",
			Rule:     "Create lands the VM on the host named by target_host_id and stamps the requester as its owner; the VM is then found there by that owner and by no other",
			Source:   "proto/provider/v1/provider.proto: CreateRequest.target_host_id, CreateRequest.owner",
			Requires: []Feature{FeatureCreate},
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				resp, err := e.Raw().Create(ctx, e.CreateRequest("cr-routed", e.HostB(), OwnerA))
				require.NoError(e.T, err)
				vm := VM{ID: resp.Id, Host: e.HostB(), Name: "cr-routed"}
				obs := e.Observe(vm)
				assert.True(e.T, obs.Exists, "the VM is on the named host")
				assert.Equal(e.T, OwnerA.UID, obs.OwnerUID, "the owner is stamped")
				assert.False(e.T, e.Observe(VM{ID: resp.Id, Host: e.HostA(), Name: "cr-routed"}).Exists, "and not on the other host")

				d, err := e.Describe(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, d.Exists)
				d, err = e.Describe(vm.Host, vm.ID, OwnerB)
				require.NoError(e.T, err)
				assert.False(e.T, d.Exists, "another tenant does not see it")
			},
		},
		{
			ID:       "create.owner-rebind",
			Rule:     "a repeated Create by the same owner is an idempotent success (the same VM), and a Create by another owner for a taken name fails AlreadyExists and leaves the VM and its stamp alone",
			Source:   "proto/provider/v1/provider.proto: CreateRequest.owner, GetCapabilitiesResponse.supports_owner_rebind; ADR-0007 Addendum A, A2",
			Requires: []Feature{FeatureCreate, FeatureCreateOwnerRebind},
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				first, err := e.Raw().Create(ctx, e.CreateRequest("cr-idem", e.HostA(), OwnerA))
				require.NoError(e.T, err)
				second, err := e.Raw().Create(ctx, e.CreateRequest("cr-idem", e.HostA(), OwnerA))
				require.NoError(e.T, err, "the retry after a lost answer succeeds")
				assert.Equal(e.T, first.Id, second.Id, "and binds the same VM")
				list, err := e.ListVMs()
				require.NoError(e.T, err)
				assert.Equal(e.T, 1, listedKeys(list.Vms)[listKey{e.HostA(), first.Id}], "no second VM was made")

				_, err = e.Raw().Create(ctx, e.CreateRequest("cr-idem", e.HostA(), OwnerB))
				requireCode(e, err, codes.AlreadyExists, "Create of a taken name by another owner")
				_, err = e.Manager().Create(e.Ctx(), contracts.CreateRequest{Name: "cr-idem", TargetHostID: e.HostA(), Owner: contracts.ObjectIdentity{UID: OwnerB.UID, Namespace: OwnerB.Namespace, Name: "cr-idem"}})
				assert.True(e.T, contracts.IsConflict(err), "manager mapping: %v", err)
				vm := VM{ID: first.Id, Host: e.HostA(), Name: "cr-idem"}
				assert.Equal(e.T, OwnerA.UID, e.Observe(vm).OwnerUID, "the stamp is not overwritten")
			},
		},
	}
}
