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
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	srcEnvelope  = "provider.proto OperationEnvelope; docs/operation-grants-wire-shape.md"
	srcGrantBody = "provider.proto GrantBody and OperationGrant; docs/operation-grants-wire-shape.md"
	srcOpID      = "provider.proto GrantBody.op_id (\"a new attempt always has a new op_id\"); contracts.OpIDConflictReason"
	srcOutcomes  = "provider.proto TaskOutcome and TaskStatusResponse; docs/task-status-outcomes.md"
)

// idCase is an identity.* case: it needs FeatureOperationIdentity and any
// further features.
func idCase(c Case, more ...Feature) Case {
	c.Requires = append([]Feature{FeatureOperationIdentity}, more...)
	return c
}

// seedStopped seeds a stopped VM of OwnerA on host.
func seedStopped(e *Env, host, name string) VM {
	return e.Seed(SeedSpec{Host: host, Name: name, Owner: OwnerA})
}

// requireNotRunning finishes the Target's tasks and requires vm stopped.
func requireNotRunning(e *Env, vm VM, what string) {
	e.T.Helper()
	e.FinishTasks()
	o := e.Observe(vm)
	assert.True(e.T, o.Exists, "%s: the VM still exists", what)
	assert.False(e.T, o.Running, "%s: a refused operation must have had no effect", what)
}

func identityCases() []Case {
	return []Case{
		idCase(Case{
			ID:     "identity.capabilities",
			Rule:   "a Provider whose hosts verify operation grants reports supports_operation_identity, and reports requires_operation_identity exactly when it refuses a mutating call that carries no grant",
			Source: "provider.proto GetCapabilitiesResponse fields 31 and 32; docs/operation-grants-wire-shape.md (Capability gates)",
			Run: func(e *Env) {
				ctx, cancel := e.Call()
				defer cancel()
				caps, err := e.Raw().GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
				require.NoError(e.T, err)
				assert.True(e.T, caps.GetSupportsOperationIdentity(), "a Provider that verifies grants reports supports_operation_identity")
				required := e.Target.Capabilities().Has(FeatureOperationIdentityRequired)
				assert.Equal(e.T, required, caps.GetRequiresOperationIdentity(), "requires_operation_identity matches whether unsigned calls are refused")
				mgr, err := e.Manager().GetCapabilities(e.Ctx())
				require.NoError(e.T, err)
				assert.True(e.T, mgr.SupportsOperationIdentity, "the manager reads supports_operation_identity")
			},
		}),
		idCase(Case{
			ID:     "identity.unsigned-refused",
			Rule:   "a Provider that reports requires_operation_identity refuses a mutating call with no operation_envelope with IDENTITY_REQUIRED, and the VM is untouched",
			Source: "provider.proto GetCapabilitiesResponse.requires_operation_identity; contracts.IdentityRequiredReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-unsigned")
				before := e.Observe(vm)
				_, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				requireRefusedWith(e, err, contracts.IdentityRequiredReason, "unsigned Power")
				_, err = e.Delete(vm.Host, vm.ID, OwnerA)
				requireRefusedWith(e, err, contracts.IdentityRequiredReason, "unsigned Delete")
				e.FinishTasks()
				requireUnchanged(e, vm, before, "unsigned calls")
			},
		}, FeatureOperationIdentityRequired),
		idCase(Case{
			ID:     "identity.signed-power",
			Rule:   "a correctly signed Power is routed by the request inside the envelope, performed on that host, and its task reports TASK_OUTCOME_SUCCEEDED once the host finished it",
			Source: srcEnvelope + " (\"It routes only from its decoded copy of request\"); " + srcOutcomes,
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostB(), "id-power")
				other := seedStopped(e, e.HostA(), "id-power")
				a, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON)))
				require.NoError(e.T, err, "a signed POWER_ON is accepted")
				e.SettleSigned(a, "signed POWER_ON")
				assert.True(e.T, e.Observe(vm).Running, "the VM on the host the request names runs")
				assert.False(e.T, e.Observe(other).Running, "the VM of the same name on the other host is untouched")
				a, err = e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF)))
				require.NoError(e.T, err, "a signed POWER_OFF is accepted")
				e.SettleSigned(a, "signed POWER_OFF")
				assert.False(e.T, e.Observe(vm).Running, "the VM is off again")
			},
		}),
		idCase(Case{
			ID:     "identity.signed-create-delete",
			Rule:   "a correctly signed Create makes the VM on the host its request names, stamped with its owner, and a correctly signed Delete removes it",
			Source: srcEnvelope + "; provider.proto CreateRequest.owner",
			Run: func(e *Env) {
				b := e.HostB()
				req := e.CreateRequest("id-create", b, OwnerA)
				a, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_CREATE, b, req))
				require.NoError(e.T, err, "a signed Create is accepted")
				require.NotEmpty(e.T, a.ID, "Create returns the VM's id")
				e.SettleSigned(a, "signed Create")
				vm := VM{ID: a.ID, Host: b, Name: "id-create"}
				o := e.Observe(vm)
				require.True(e.T, o.Exists, "the VM is on the host the request names")
				assert.Equal(e.T, OwnerA.UID, o.OwnerUID, "the VM carries its owner stamp")
				assert.False(e.T, e.Observe(VM{ID: a.ID, Host: e.HostA(), Name: "id-create"}).Exists, "and not on the other host")
				del := deleteReq(vm)
				del.Owner = req.GetOwner()
				a, err = e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_DELETE, b, del))
				require.NoError(e.T, err, "a signed Delete is accepted")
				e.SettleSigned(a, "signed Delete")
				assert.False(e.T, e.Observe(vm).Exists, "the VM is gone")
			},
		}, FeatureCreate),
		idCase(Case{
			ID:     "identity.forged-grant-refused",
			Rule:   "a grant whose signature or certificate chain does not verify to the anchor the host pinned is refused with GRANT_UNAUTHENTICATED, and nothing is performed",
			Source: srcGrantBody + " (\"A verifier checks the signature before it decodes body\"; the chain is exactly a leaf and its issuer); contracts.GrantUnauthenticatedReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-forged")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				power := providerv1.Provider_Power_FullMethodName

				// The body changed after it was signed (a higher attempt).
				g := e.Grant(op)
				body := &providerv1.GrantBody{}
				require.NoError(e.T, proto.Unmarshal(g.GetBody(), body))
				body.Attempt++
				changed, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
				require.NoError(e.T, err)
				g.Body = changed
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: op.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantUnauthenticatedReason, "a body changed after signing")

				// Another grant's signature.
				g = e.Grant(op)
				g.Signature = e.Grant(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))).GetSignature()
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: op.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantUnauthenticatedReason, "another grant's signature")

				// The leaf alone, without its issuer.
				g = e.Grant(op)
				g.SignerCertChain = g.GetSignerCertChain()[:1]
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: op.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantUnauthenticatedReason, "a chain without the issuer")

				// No grant at all.
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: op.Request}, nil)
				requireRefusedWith(e, err, contracts.GrantUnauthenticatedReason, "an envelope with no grant")

				requireNotRunning(e, vm, "forged grants")
			},
		}),
		idCase(Case{
			ID:     "identity.wrong-kind-refused",
			Rule:   "an authentic grant whose operation kind does not fit the call (another PowerRequest.op, or another RPC) is refused with GRANT_MISMATCH, and nothing is performed",
			Source: "provider.proto OperationKind (each kind names exactly one RPC, its request type and, for Power, the op); contracts.GrantMismatchReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-kind")
				on := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				power := providerv1.Provider_Power_FullMethodName

				// A POWER_OFF grant over a POWER_ON request.
				g := e.Resign(on, func(b *providerv1.GrantBody) { b.Kind = providerv1.OperationKind_OPERATION_KIND_POWER_OFF })
				_, err := e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: on.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantMismatchReason, "a POWER_OFF grant on a POWER_ON request")

				// A REBOOT grant over a POWER_ON request.
				g = e.Resign(on, func(b *providerv1.GrantBody) { b.Kind = providerv1.OperationKind_OPERATION_KIND_REBOOT })
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: on.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantMismatchReason, "a REBOOT grant on a POWER_ON request")

				// A DELETE grant sent on Power.
				g = e.Resign(on, func(b *providerv1.GrantBody) { b.Kind = providerv1.OperationKind_OPERATION_KIND_DELETE })
				_, err = e.SendEnvelope(power, &providerv1.OperationEnvelope{Request: on.Request, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantMismatchReason, "a DELETE grant on Power")

				requireNotRunning(e, vm, "wrong-kind grants")
			},
		}),
		idCase(Case{
			ID:     "identity.outer-fields-refused",
			Rule:   "a request that sets any field beside operation_envelope is refused with INVALID_ARGUMENT, and nothing is performed",
			Source: srcEnvelope + " (\"refuses a request that has operation_envelope and any other field set (INVALID_ARGUMENT; the request is not performed)\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-outer")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				for name, set := range map[string]func(*providerv1.PowerRequest){
					"target_host_id": func(r *providerv1.PowerRequest) { r.TargetHostId = vm.Host },
					"id and op":      func(r *providerv1.PowerRequest) { r.Id, r.Op = vm.ID, providerv1.PowerOp_POWER_OP_ON },
					"owner":          func(r *providerv1.PowerRequest) { r.Owner = OwnerA.Proto() },
				} {
					set := func(m proto.Message) {
						r, ok := m.(*providerv1.PowerRequest)
						require.True(e.T, ok, "the outer request of Power is a PowerRequest")
						set(r)
					}
					_, err := e.SendEnvelope(providerv1.Provider_Power_FullMethodName, e.Envelope(op), set)
					requireCode(e, err, codes.InvalidArgument, "outer "+name+" beside the envelope")
				}
				requireNotRunning(e, vm, "outer fields beside the envelope")
			},
		}),
		idCase(Case{
			ID:     "identity.digest-bound",
			Rule:   "request bytes that are not the ones the grant's intent_digest covers are refused with GRANT_MISMATCH, and neither request is performed",
			Source: "provider.proto GrantBody.intent_digest (\"SHA-256(digest_salt || OperationEnvelope.request), over the exact request bytes\"); contracts.GrantMismatchReason",
			Run: func(e *Env) {
				signed := seedStopped(e, e.HostA(), "id-digest-signed")
				swapped := seedStopped(e, e.HostA(), "id-digest-swapped")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, signed.Host, powerReq(signed, providerv1.PowerOp_POWER_OP_ON))
				other, err := proto.MarshalOptions{Deterministic: true}.Marshal(powerReq(swapped, providerv1.PowerOp_POWER_OP_ON))
				require.NoError(e.T, err)
				_, err = e.SendEnvelope(providerv1.Provider_Power_FullMethodName,
					&providerv1.OperationEnvelope{Request: other, Grant: e.Grant(op)}, nil)
				requireRefusedWith(e, err, contracts.GrantMismatchReason, "a grant over other request bytes")
				requireNotRunning(e, swapped, "the swapped-in request")
				requireNotRunning(e, signed, "the request the grant covers")
			},
		}),
		idCase(Case{
			ID:     "identity.binding-refused",
			Rule:   "an authentic grant for another Provider, another enrolment or another host than the one the request names is refused with GRANT_MISMATCH, and nothing is performed",
			Source: srcGrantBody + " (provider_uid, host_id, enrolment_id); contracts.GrantMismatchReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-binding")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				other := e.OperationIdentity().Enrolments[e.HostB()]
				for name, mutate := range map[string]func(*providerv1.GrantBody){
					"another Provider":   func(b *providerv1.GrantBody) { b.ProviderUid = OwnerB.UID },
					"another enrolment":  func(b *providerv1.GrantBody) { b.EnrolmentId = append([]byte(nil), other...) },
					"another host's id":  func(b *providerv1.GrantBody) { b.HostId, b.EnrolmentId = e.HostB(), append([]byte(nil), other...) },
					"another host alone": func(b *providerv1.GrantBody) { b.HostId = e.HostB() },
				} {
					_, err := e.SendEnvelope(providerv1.Provider_Power_FullMethodName,
						&providerv1.OperationEnvelope{Request: op.Request, Grant: e.Resign(op, mutate)}, nil)
					requireRefusedWith(e, err, contracts.GrantMismatchReason, "a grant for "+name)
				}
				requireNotRunning(e, vm, "grants bound elsewhere")
			},
		}),
		idCase(Case{
			ID:     "identity.duplicate-op-id-once",
			Rule:   "a re-delivery of an op_id the host accepted, even after the operation is over, is answered as the first delivery was and never performed again: a re-sent Delete does not delete a VM made after it",
			Source: srcOpID + "; docs/task-status-outcomes.md (VM_BUSY: \"the request may re-deliver an attempt already accepted\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-dup")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_DELETE, vm.Host, deleteReq(vm))
				first, err := e.SendSigned(op)
				require.NoError(e.T, err, "the first delivery is accepted")
				e.SettleSigned(first, "the first delivery")
				require.False(e.T, e.Observe(vm).Exists, "the Delete happened")
				vm = seedStopped(e, vm.Host, vm.Name) // a new VM of the same name, made later
				again, err := e.SendSigned(op)
				require.NoError(e.T, err, "a re-delivery of the same attempt is answered, not refused")
				assert.Equal(e.T, first, again, "the re-delivery gets the answer the first delivery got")
				e.FinishTasks()
				assert.True(e.T, e.Observe(vm).Exists, "the re-delivered Delete must not delete the VM made after it")
			},
		}),
		idCase(Case{
			ID:     "identity.duplicate-after-restart",
			Rule:   "the host's record of an accepted op_id survives a restart of the Provider: a re-delivery after the restart is never performed again",
			Source: srcOpID + "; ADR-0007 (state lives on the hosts, never only in the Provider process)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-dup-restart")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_DELETE, vm.Host, deleteReq(vm))
				first, err := e.SendSigned(op)
				require.NoError(e.T, err, "the first delivery is accepted")
				e.SettleSigned(first, "the first delivery")
				vm = seedStopped(e, vm.Host, vm.Name)
				e.RestartProvider()
				again, err := e.SendSigned(op)
				require.NoError(e.T, err, "a re-delivery after the restart is answered, not refused")
				assert.Equal(e.T, first, again, "the re-delivery gets the answer the first delivery got")
				e.FinishTasks()
				assert.True(e.T, e.Observe(vm).Exists, "the re-delivered Delete must not delete the VM made after it")
			},
		}),
		idCase(Case{
			ID:     "identity.op-id-conflict",
			Rule:   "a known op_id that arrives with other identity fields (another attempt, another intent_digest, another VM) is refused with OP_ID_CONFLICT, and nothing is performed",
			Source: srcOpID,
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-conflict")
				bystander := seedStopped(e, e.HostA(), "id-conflict-other")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF))
				a, err := e.SendSigned(op)
				require.NoError(e.T, err, "the first delivery is accepted")
				e.SettleSigned(a, "the first delivery")

				attempt := *op
				attempt.Identity.Attempt = 2
				_, err = e.SendSigned(&attempt)
				requireRefusedWith(e, err, contracts.OpIDConflictReason, "the op_id with another attempt")

				salt, err := operation.NewDigestSalt()
				require.NoError(e.T, err)
				resalted := *op
				resalted.Salt = salt
				_, err = e.SendSigned(&resalted)
				requireRefusedWith(e, err, contracts.OpIDConflictReason, "the op_id with another intent_digest")

				onOther := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, bystander.Host, powerReq(bystander, providerv1.PowerOp_POWER_OP_ON))
				onOther.Identity.OpID = op.Identity.OpID
				_, err = e.SendSigned(onOther)
				requireRefusedWith(e, err, contracts.OpIDConflictReason, "the op_id for another VM and kind")
				requireNotRunning(e, bystander, "a conflicting op_id")
			},
		}),
		idCase(Case{
			ID:     "identity.vm-busy",
			Rule:   "while an operation on a VM is not over, a new operation (a new op_id) for the same VM is refused with VM_BUSY; once the first is over the same attempt is accepted",
			Source: "contracts.VMBusyReason (\"the host runs another, non-terminal operation for the same VM ... Retryable later with the same attempt\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-busy")
				first, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON)))
				require.NoError(e.T, err, "the first operation is accepted")
				require.NotEmpty(e.T, first.TaskRef, "an asynchronous Power returns a task")
				second := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF))
				_, err = e.SendSigned(second)
				requireRefusedWith(e, err, contracts.VMBusyReason, "a second operation while the first runs")
				e.SettleSigned(first, "the first operation")
				require.True(e.T, e.Observe(vm).Running, "the first operation had its effect")
				a, err := e.SendSigned(second)
				require.NoError(e.T, err, "the same attempt is accepted once the first operation is over")
				e.SettleSigned(a, "the second operation")
				assert.False(e.T, e.Observe(vm).Running, "the second operation had its effect")
			},
		}, FeatureAsyncTasks),
		idCase(Case{
			ID:     "identity.mint-epoch-first-delivery",
			Rule:   "the first delivery of an op_id minted before the host's enrolment started is refused, and nothing is performed: the host cannot know what an earlier enrolment did with it",
			Source: "provider.proto GrantBody.mint_epoch; ADR-0016 Part A (the first-delivery rule for mint_epoch)",
			Level:  Recommended,
			Run: func(e *Env) {
				start := e.OperationIdentity().EnrolmentStartEpoch
				if start <= 1 {
					e.T.Skipf("the target's hosts were enrolled at epoch %d: no counter is below it", start)
				}
				vm := seedStopped(e, e.HostA(), "id-mint")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				op.Identity.MintEpoch.Counter = start - 1
				_, err := e.SendSigned(op)
				require.Error(e.T, err, "an op_id minted before the enrolment must be refused on its first delivery")
				requireNotRunning(e, vm, "an op_id minted before the enrolment")
			},
		}),
		idCase(Case{
			ID:     "identity.outcome-retired",
			Rule:   "a task that never started reports TASK_OUTCOME_RETIRED with done=true and a \"not performed\" error, the manager reads it as not performed, and it had no effect",
			Source: srcOutcomes + " (RETIRED: \"done=true, error=not performed: RETIRED{<outcome_reason>}\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-retired")
				e.ScriptOutcome(vm.Host, providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, "never_received")
				a, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON)))
				require.NoError(e.T, err)
				require.NotEmpty(e.T, a.TaskRef)
				e.FinishTasks()
				st, err := e.TaskStatus(a.TaskRef)
				require.NoError(e.T, err)
				assert.Equal(e.T, providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, st.GetOutcome(), "typed outcome")
				assert.True(e.T, st.GetDone(), "a retired task is over")
				assert.True(e.T, strings.HasPrefix(st.GetError(), "not performed"), "a retired task's error says it was not performed: %q", st.GetError())
				mgr, err := e.Manager().TaskStatus(e.Ctx(), a.TaskRef)
				require.NoError(e.T, err)
				assert.Equal(e.T, contracts.TaskOutcomeRetired, mgr.Outcome, "the manager reads RETIRED")
				assert.NotEmpty(e.T, mgr.Error, "the manager never takes a retired task for a success")
				requireNotRunning(e, vm, "a retired operation")
			},
		}, FeatureScriptedOutcomes, FeatureAsyncTasks),
		idCase(Case{
			ID:     "identity.outcome-unknown",
			Rule:   "a task whose outcome the host cannot tell reports TASK_OUTCOME_UNKNOWN and is never done, and the VM stays busy for a new operation",
			Source: srcOutcomes + " (UNKNOWN: \"Not terminal: the VM is held, and the task is never read as done\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-unknown")
				e.ScriptOutcome(vm.Host, providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN, "host_lost_record")
				a, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON)))
				require.NoError(e.T, err)
				require.NotEmpty(e.T, a.TaskRef)
				e.FinishTasks()
				st, err := e.TaskStatus(a.TaskRef)
				require.NoError(e.T, err)
				assert.Equal(e.T, providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN, st.GetOutcome(), "typed outcome")
				assert.False(e.T, st.GetDone(), "an unknown outcome is never done")
				mgr, err := e.Manager().TaskStatus(e.Ctx(), a.TaskRef)
				require.NoError(e.T, err)
				assert.False(e.T, mgr.IsCompleted, "the manager never reads an unknown outcome as done")
				_, err = e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF)))
				requireRefusedWith(e, err, contracts.VMBusyReason, "a new operation while the outcome is unknown")
			},
		}, FeatureScriptedOutcomes, FeatureAsyncTasks),
		idCase(Case{
			ID:     "identity.outcome-failed-dirty",
			Rule:   "a task that failed after an irreversible step reports TASK_OUTCOME_FAILED_DIRTY with done=true and an error, and the manager reads it as a failure, never as not performed",
			Source: srcOutcomes + " (FAILED_DIRTY: \"something may have happened, and the VM is held\")",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-dirty")
				e.ScriptOutcome(vm.Host, providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, "disk_half_written")
				a, err := e.SendSigned(e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON)))
				require.NoError(e.T, err)
				require.NotEmpty(e.T, a.TaskRef)
				e.FinishTasks()
				st, err := e.TaskStatus(a.TaskRef)
				require.NoError(e.T, err)
				assert.Equal(e.T, providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, st.GetOutcome(), "typed outcome")
				assert.True(e.T, st.GetDone(), "a dirty failure is over")
				assert.NotEmpty(e.T, st.GetError(), "a dirty failure carries an error")
				mgr, err := e.Manager().TaskStatus(e.Ctx(), a.TaskRef)
				require.NoError(e.T, err)
				assert.Equal(e.T, contracts.TaskOutcomeFailedDirty, mgr.Outcome, "the manager reads FAILED_DIRTY")
				assert.True(e.T, mgr.IsCompleted && mgr.Error != "", "the manager reads a failure")
				assert.False(e.T, strings.HasPrefix(mgr.Error, "not performed"), "a dirty failure is never read as not performed: %q", mgr.Error)
			},
		}, FeatureScriptedOutcomes, FeatureAsyncTasks),
	}
}
