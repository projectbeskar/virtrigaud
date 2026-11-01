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
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	srcTargetHostField = "proto/provider/v1/provider.proto: DescribeRequest, DeleteRequest, PowerRequest, ReconfigureRequest target_host_id; ADR-0007 Addendum A, A1"
	srcUnknownHost     = "ADR-0007 Addendum A, A1 (an unknown or removed host returns a retryable Unavailable); contracts.HostUnavailableReason"
)

// perVMCall is one per-VM RPC the routing cases drive with a host id.
type perVMCall struct {
	name string
	call func(e *Env, host string, vm VM, owner Identity) error
}

func perVMCalls() []perVMCall {
	return []perVMCall{
		{"Describe", func(e *Env, host string, vm VM, owner Identity) error {
			_, err := e.Describe(host, vm.ID, owner)
			return err
		}},
		{"Delete", func(e *Env, host string, vm VM, owner Identity) error {
			_, err := e.Delete(host, vm.ID, owner)
			return err
		}},
		{"Power", func(e *Env, host string, vm VM, owner Identity) error {
			_, err := e.Power(host, vm.ID, owner, providerv1.PowerOp_POWER_OP_OFF)
			return err
		}},
		{"Reconfigure", func(e *Env, host string, vm VM, owner Identity) error {
			_, err := e.Reconfigure(host, vm.ID, owner)
			return err
		}},
	}
}

func routingCases() []Case {
	return []Case{
		{
			ID:     "route.missing-host",
			Rule:   "an empty or blank target_host_id is InvalidArgument on every per-VM call; the provider never picks a host, and the VM is not touched",
			Source: srcTargetHostField + " (\"answers an empty value with InvalidArgument and never looks up a VM's host itself\")",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "route-missing", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				for _, c := range perVMCalls() {
					for _, host := range []string{"", "   "} {
						err := c.call(e, host, vm, OwnerA)
						w := requireCode(e, err, codes.InvalidArgument, c.name+" with host "+quote(host))
						requireNoReason(e, w, contracts.HostUnavailableReason, c.name)
					}
				}
				requireUnchanged(e, vm, before, "calls without a host")

				// The manager maps it to a non-retryable InvalidSpec.
				_, err := e.Manager().Describe(e.Ctx(), contracts.VMRef{ID: vm.ID, Owner: contracts.ObjectIdentity(OwnerA)})
				require.Error(e.T, err)
				assert.True(e.T, contracts.IsInvalidSpec(err), "manager mapping of a missing host: %v", err)
				assert.False(e.T, contracts.IsRetryable(err), "a missing host id is never retried")
			},
		},
		{
			ID:     "route.unknown-host",
			Rule:   "a host id the provider does not front is a retryable, host-scoped Unavailable (HOST_UNAVAILABLE ErrorInfo); it is never dialed and never answered NotFound",
			Source: srcUnknownHost,
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "route-unknown", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				for _, c := range perVMCalls() {
					err := c.call(e, UnknownHost, vm, OwnerA)
					requireHostUnavailable(e, err, UnknownHost, c.name+" on an unknown host")
				}
				requireUnchanged(e, vm, before, "calls on an unknown host")
				requireNeverDialed(e, UnknownHost)
			},
		},
		{
			ID:     "route.invalid-host-id",
			Rule:   "a malformed or differently-cased host id is never routed: InvalidArgument or the host-scoped Unavailable, never success, never NotFound, never dialed",
			Source: srcUnknownHost + "; docs/adr/0007 A1 (host ids are Host object names, DNS-1123 subdomains)",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "route-invalid", Owner: OwnerA, Running: true})
				before := e.Observe(vm)
				bad := []string{
					strings.ToUpper(e.HostA()),
					"host a",
					"../" + e.HostA(),
					e.HostA() + "/../" + e.HostB(),
					"qemu+ssh://attacker.example.com/system",
					e.HostA() + ":22",
					strings.Repeat("a", 300),
				}
				for _, host := range bad {
					for _, c := range perVMCalls() {
						err := c.call(e, host, vm, OwnerA)
						require.Error(e.T, err, "%s with host %q must be refused", c.name, host)
						w := WireOf(err)
						switch w.Code {
						case codes.InvalidArgument:
						case codes.Unavailable:
							_, ok := w.Reason(contracts.HostUnavailableReason)
							assert.True(e.T, ok, "%s with host %q: Unavailable must be host-scoped: %s", c.name, host, w)
						default:
							e.T.Errorf("%s with host %q: unexpected answer %s", c.name, host, w)
						}
					}
				}
				requireUnchanged(e, vm, before, "calls with a malformed host id")
				requireNeverDialed(e, bad...)
			},
		},
		{
			ID:     "route.correct-host",
			Rule:   "a call acts on the VM at the named host only: the same VM name on another host is a different VM, and a VM that is not on the named host is not found there (no lookup on other hosts)",
			Source: srcTargetHostField + "; ADR-0007 Addendum A, A1 (\"the provider never looks a VM's host up by itself\")",
			Run: func(e *Env) {
				onA := e.Seed(SeedSpec{Host: e.HostA(), Name: "route-web", Owner: OwnerA, Running: true})
				onB := e.Seed(SeedSpec{Host: e.HostB(), Name: "route-web", Owner: OwnerB, Running: false})
				db := e.Seed(SeedSpec{Host: e.HostB(), Name: "route-db", Owner: OwnerA, Running: false})
				beforeB, beforeDB := e.Observe(onB), e.Observe(db)

				// Described where it is, it is found and in the state of that host.
				resp, err := e.Describe(e.HostA(), onA.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists, "route-web is on %s", e.HostA())
				runningState := resp.PowerState
				resp, err = e.Describe(e.HostB(), db.ID, OwnerA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Exists, "route-db is on %s", e.HostB())
				assert.NotEqual(e.T, runningState, resp.PowerState, "a running VM and a stopped VM report different power states")

				// Asked at the other host, OwnerA's route-web is not there: the
				// route-web of that host belongs to OwnerB, and the provider does not
				// fall back to the host that has OwnerA's.
				resp, err = e.Describe(e.HostB(), onA.ID, OwnerA)
				require.NoError(e.T, err)
				assert.False(e.T, resp.Exists, "OwnerA's VM is on %s, not on %s", e.HostA(), e.HostB())

				// A mutation sent to the wrong host finds nothing and changes nothing.
				_, err = e.Power(e.HostA(), db.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				requireCode(e, err, codes.NotFound, "Power on a host that does not have the VM")
				requireUnchanged(e, db, beforeDB, "Power sent to the wrong host")
				_, err = e.Delete(e.HostA(), db.ID, OwnerA)
				requireCode(e, err, codes.NotFound, "Delete on a host that does not have the VM")
				requireUnchanged(e, db, beforeDB, "Delete sent to the wrong host")

				// Routed correctly, the effect lands on that host's VM only.
				_, err = e.Power(e.HostA(), onA.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
				require.NoError(e.T, err)
				assert.False(e.T, e.Observe(onA).Running, "power off reached the named host's VM")
				requireUnchanged(e, onB, beforeB, "power off on the other host")
				_, err = e.Delete(e.HostA(), onA.ID, OwnerA)
				require.NoError(e.T, err)
				assert.False(e.T, e.Observe(onA).Exists, "delete removed the VM on the named host")
				requireUnchanged(e, onB, beforeB, "delete of the same name on the other host")
			},
		},
	}
}

// requireNeverDialed fails the case if the Target reports that the provider
// connected to one of hosts. Targets that cannot report dials are not checked.
func requireNeverDialed(e *Env, hosts ...string) {
	e.T.Helper()
	obs, ok := e.Target.(DialObserver)
	if !ok {
		return
	}
	dialed := map[string]bool{}
	for _, h := range obs.DialedHosts() {
		dialed[h] = true
	}
	for _, h := range hosts {
		assert.False(e.T, dialed[h], "a refused host id %q must never be dialed", h)
	}
}

func quote(s string) string { return "\"" + s + "\"" }
