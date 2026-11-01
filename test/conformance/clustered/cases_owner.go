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
	"fmt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	srcOwnerCheck = "proto/provider/v1/provider.proto: DescribeRequest.owner, DeleteRequest.owner, PowerRequest.owner; ADR-0007 Addendum A, A2"
)

// foreignVMs seeds, on host, a running and a stopped VM for every way a VM can
// fail to be OwnerA's: stamped for another tenant, and unstamped.
func foreignVMs(e *Env, host string) []VM {
	var vms []VM
	for _, spec := range []SeedSpec{
		{Name: "foreign-run", Owner: OwnerB, Running: true},
		{Name: "foreign-off", Owner: OwnerB, Running: false},
		{Name: "unstamped-run", Owner: Identity{}, Running: true},
		{Name: "unstamped-off", Owner: Identity{}, Running: false},
	} {
		spec.Host = host
		vms = append(vms, e.Seed(spec))
	}
	return vms
}

func ownerCases() []Case {
	return []Case{
		{
			ID:     "owner.describe-foreign",
			Rule:   "Describe of a VM whose stamp is missing or records another owner is exists=false and returns none of its state",
			Source: srcOwnerCheck + " (\"reports a VM whose stamp is missing or records another owner as exists=false and returns none of its state\")",
			Run: func(e *Env) {
				for _, vm := range foreignVMs(e, e.HostA()) {
					resp, err := e.Describe(vm.Host, vm.ID, OwnerA)
					require.NoError(e.T, err, "%s: a foreign VM is reported absent, not as an error", vm.Name)
					assert.False(e.T, resp.Exists, "%s: must be exists=false", vm.Name)
					assert.Empty(e.T, resp.PowerState, "%s: no power state of another tenant's VM", vm.Name)
					assert.Empty(e.T, resp.Ips, "%s: no IPs", vm.Name)
					assert.Empty(e.T, resp.ConsoleUrl, "%s: no console", vm.Name)
					assert.Zero(e.T, resp.Vcpus, "%s: no size", vm.Name)
				}
			},
		},
		{
			ID:     "owner.mutate-foreign",
			Rule:   "Power (every op), Delete and Reconfigure of a VM whose stamp is missing or records another owner are NotFound, and the VM is not started, stopped, rebooted, resized or destroyed",
			Source: srcOwnerCheck + " (\"is reported NOT_FOUND and is never started, stopped or rebooted\"; \"is never destroyed\")",
			Run: func(e *Env) {
				mgr := e.Manager()
				for _, vm := range foreignVMs(e, e.HostA()) {
					before := e.Observe(vm)
					for _, op := range []providerv1.PowerOp{
						providerv1.PowerOp_POWER_OP_ON, providerv1.PowerOp_POWER_OP_OFF,
						providerv1.PowerOp_POWER_OP_REBOOT, providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL,
					} {
						_, err := e.Power(vm.Host, vm.ID, OwnerA, op)
						requireCode(e, err, codes.NotFound, fmt.Sprintf("Power %s on %s", op, vm.Name))
						requireUnchanged(e, vm, before, fmt.Sprintf("Power %s", op))
					}
					_, err := e.Reconfigure(vm.Host, vm.ID, OwnerA)
					requireCode(e, err, codes.NotFound, "Reconfigure on "+vm.Name)
					requireUnchanged(e, vm, before, "Reconfigure")
					_, err = e.Delete(vm.Host, vm.ID, OwnerA)
					requireCode(e, err, codes.NotFound, "Delete on "+vm.Name)
					requireUnchanged(e, vm, before, "Delete")

					// The manager sees a NotFound, never a success or a retry.
					ref := contracts.VMRef{ID: vm.ID, HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)}
					_, err = mgr.Delete(e.Ctx(), ref)
					require.Error(e.T, err)
					assert.True(e.T, contracts.IsNotFound(err), "%s: manager mapping of a refused Delete: %v", vm.Name, err)
				}
			},
		},
		{
			ID:     "owner.request-without-owner",
			Rule:   "a request that carries no owner identity never operates on a VM, stamped or not",
			Source: "proto/provider/v1/provider.proto: DeleteRequest.owner (\"only when its recorded owner uid equals this uid\"), CreateRequest.owner (\"An unset owner ... never authorizes\")",
			Run: func(e *Env) {
				stamped := e.Seed(SeedSpec{Host: e.HostA(), Name: "ownerless-stamped", Owner: OwnerA, Running: true})
				unstamped := e.Seed(SeedSpec{Host: e.HostA(), Name: "ownerless-unstamped", Running: true})
				for _, vm := range []VM{stamped, unstamped} {
					before := e.Observe(vm)
					resp, err := e.Describe(vm.Host, vm.ID, Identity{})
					require.NoError(e.T, err)
					assert.False(e.T, resp.Exists, "%s: Describe without an owner", vm.Name)
					_, err = e.Power(vm.Host, vm.ID, Identity{}, providerv1.PowerOp_POWER_OP_OFF)
					requireCode(e, err, codes.NotFound, "Power without an owner on "+vm.Name)
					_, err = e.Delete(vm.Host, vm.ID, Identity{})
					requireCode(e, err, codes.NotFound, "Delete without an owner on "+vm.Name)
					requireUnchanged(e, vm, before, "calls without an owner")
				}
			},
		},
		{
			ID:     "owner.refusal-discloses-nothing",
			Rule:   "a refusal for a VM of another owner names neither that owner (uid, namespace, name) nor leaks its state",
			Source: "internal/providers/contracts/errors.go: VMNotOwnedReason; ADR-0007 Addendum A, A2 (\"the message never names the other owner\")",
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "secret-vm", Owner: OwnerB, Running: true})
				secrets := []string{OwnerB.UID, OwnerB.Namespace, OwnerB.Name}
				check := func(what string, err error) {
					w := requireCode(e, err, codes.NotFound, what)
					text := w.Message
					for _, r := range w.Reasons {
						text += " " + r.Reason + " " + r.Domain
						for k, v := range r.Metadata {
							text += " " + k + "=" + v
						}
					}
					assert.Empty(e.T, containsAny(text, secrets...), "%s: the refusal names the other owner: %q", what, text)
				}
				_, err := e.Delete(vm.Host, vm.ID, OwnerA)
				check("Delete", err)
				_, err = e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_OFF)
				check("Power", err)
				_, err = e.Reconfigure(vm.Host, vm.ID, OwnerA)
				check("Reconfigure", err)
				resp, derr := e.Describe(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, derr)
				assert.Empty(e.T, containsAny(prototext.Format(resp), secrets...), "Describe of a foreign VM leaks its owner")
			},
		},
		{
			ID:       "owner.vm-not-owned-reason",
			Rule:     "a Provider that adds the VM_NOT_OWNED reason does so for a refusal for another owner's VM, never for a VM that is absent; the manager marks it (contracts.IsVMNotOwned)",
			Source:   "internal/providers/contracts/errors.go: VMNotOwnedReason, IsVMNotOwned",
			Requires: []Feature{FeatureVMNotOwnedReason},
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "not-owned", Owner: OwnerB, Running: true})
				_, err := e.Delete(vm.Host, vm.ID, OwnerA)
				w := requireCode(e, err, codes.NotFound, "Delete of another owner's VM")
				r, ok := w.Reason(contracts.VMNotOwnedReason)
				require.True(e.T, ok, "must carry %s: %s", contracts.VMNotOwnedReason, w)
				assert.Equal(e.T, contracts.ErrorInfoDomain, r.Domain)

				ref := contracts.VMRef{ID: vm.ID, HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)}
				_, err = e.Manager().Delete(e.Ctx(), ref)
				assert.True(e.T, contracts.IsVMNotOwned(err), "manager mapping: %v", err)

				// A VM that is not there at all is a plain NotFound.
				_, err = e.Delete(vm.Host, "no-such-vm", OwnerA)
				w = requireCode(e, err, codes.NotFound, "Delete of an absent VM")
				requireNoReason(e, w, contracts.VMNotOwnedReason, "Delete of an absent VM")
				_, err = e.Manager().Delete(e.Ctx(), contracts.VMRef{ID: "no-such-vm", HostID: vm.Host, Owner: contracts.ObjectIdentity(OwnerA)})
				assert.False(e.T, contracts.IsVMNotOwned(err), "an absent VM is not a VM_NOT_OWNED: %v", err)
			},
		},
	}
}
