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
	"bytes"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	srcHostAnswers = "proto/provider/v1/host_answer.proto (OperationAnswer, VMAnswer, AnswerContext); docs/operation-grants-wire-shape.md (host answers)"
	srcAttempt     = "provider.proto GrantBody.attempt; contracts.OpIDConflictReason (\"or the attempt does not increase\")"
)

// answerCase is an identity.* case about host answers.
func answerCase(c Case, more ...Feature) Case {
	return idCase(c, append([]Feature{FeatureHostAnswers}, more...)...)
}

// requireSigned fails the case unless s carries a verified answer.
func requireSigned(e *Env, s SignedOpStatus, what string) *providerv1.OperationAnswer {
	e.T.Helper()
	require.Empty(e.T, s.Invalid, "%s: the signed answer must verify", what)
	require.NotNil(e.T, s.Answer, "%s: a request with answer_nonce gets a signed answer (call error: %v)", what, s.Err)
	return s.Answer
}

// operationState is the state a verified answer names.
func operationState(a *providerv1.OperationAnswer) providerv1.OperationRecordState { return a.GetState() }

func agentIdentityCases() []Case {
	return []Case{
		answerCase(Case{
			ID:     "identity.tombstone-after-restart",
			Rule:   "a host's record of a finished op_id survives a crash of its agent: a re-delivery after the restart is answered as the first one was and never performed again, and a signed TaskStatus names it as tombstoned",
			Source: srcOpID + "; " + srcHostAnswers + " (OperationAnswer.tombstoned)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-tomb")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_DELETE, vm.Host, deleteReq(vm))
				first, err := e.SendSigned(op)
				require.NoError(e.T, err, "the first delivery is accepted")
				e.SettleSigned(first, "the first delivery")
				vm = seedStopped(e, vm.Host, vm.Name)
				require.NoError(e.T, e.HostAnswers().CrashHost(vm.Host), "crash the host's agent")
				again, err := e.SendSigned(op)
				require.NoError(e.T, err, "a re-delivery after the crash is answered, not refused")
				assert.Equal(e.T, first, again, "the re-delivery gets the answer the first delivery got")
				e.FinishTasks()
				assert.True(e.T, e.Observe(vm).Exists, "the re-delivered Delete must not delete the VM made after it")
				a := requireSigned(e, e.SignedTaskStatus(op, e.NewNonce()), "signed TaskStatus after the crash")
				assert.Equal(e.T, providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, operationState(a), "state")
				assert.True(e.T, a.GetTombstoned(), "a finished operation is answered from its tombstone")
			},
		}),
		answerCase(Case{
			ID:     "identity.taskstatus-bound",
			Rule:   "a signed TaskStatus answers for exactly the op_id asked about (its kind, attempt, VM and binding), and an op_id the host never saw is NO_RECORD, NotFound with UNKNOWN_TO_AGENT, never done",
			Source: srcHostAnswers + " (OperationAnswer fields 2-11); docs/task-status-outcomes.md (UNKNOWN_TO_AGENT)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-bound")
				on := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				a, err := e.SendSigned(on)
				require.NoError(e.T, err)
				e.SettleSigned(a, "POWER_ON")
				off := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF))
				a, err = e.SendSigned(off)
				require.NoError(e.T, err)
				e.SettleSigned(a, "POWER_OFF")
				for _, op := range []*SignedOp{on, off} {
					ans := requireSigned(e, e.SignedTaskStatus(op, e.NewNonce()), op.Identity.Kind.String())
					assert.Equal(e.T, op.Identity.OpID, ans.GetOpId(), "%s: the answer names the op_id asked about", op.Identity.Kind)
					assert.Equal(e.T, op.Identity.EnrolmentID, ans.GetOpEnrolmentId(), "%s: and its enrolment", op.Identity.Kind)
					assert.Equal(e.T, int32(op.Identity.Kind), ans.GetKind(), "%s: kind", op.Identity.Kind)
					assert.Equal(e.T, op.Identity.Attempt, ans.GetAttempt(), "%s: attempt", op.Identity.Kind)
					assert.Equal(e.T, op.Identity.VMUID, ans.GetVmUid(), "%s: VM", op.Identity.Kind)
					assert.Equal(e.T, op.Identity.BindingID, ans.GetBindingId(), "%s: binding", op.Identity.Kind)
					assert.Equal(e.T, providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, ans.GetState(), "%s: state", op.Identity.Kind)
				}
				never := e.NewOp(providerv1.OperationKind_OPERATION_KIND_REBOOT, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_REBOOT))
				s := e.SignedTaskStatus(never, e.NewNonce())
				requireRefusedWith(e, s.Err, contracts.UnknownToAgentReason, "an op_id the host never saw")
				assert.Equal(e.T, codes.NotFound, WireOf(s.Err).Code, "an op_id the host never saw is NotFound")
				ans := requireSigned(e, s, "an op_id the host never saw")
				assert.Equal(e.T, providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD, ans.GetState(), "state")
				assert.Equal(e.T, never.Identity.OpID, ans.GetOpId(), "the answer names the op_id asked about")
				assert.Zero(e.T, ans.GetKind(), "NO_RECORD carries no identity fields")
			},
		}),
		answerCase(Case{
			ID:     "identity.deleted-answer",
			Rule:   "a signed Describe answers DELETED, naming the Delete, only after that placement's Delete completed; a placement the host never had is NO_RECORD, never DELETED",
			Source: srcHostAnswers + " (VMAnswer.presence, delete_op_id)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-deleted")
				del := e.NewOp(providerv1.OperationKind_OPERATION_KIND_DELETE, vm.Host, deleteReq(vm))
				binding := del.Identity.BindingID
				resp, va, err := e.SignedDescribe(vm.Host, OwnerA.UID, binding, e.NewNonce())
				require.NoError(e.T, err, "signed Describe of the placement")
				assert.True(e.T, resp.GetExists(), "the placement exists before its Delete")
				assert.NotEqual(e.T, providerv1.VMPresence_VM_PRESENCE_DELETED, va.GetPresence(), "a placement that exists is not DELETED")
				a, err := e.SendSigned(del)
				require.NoError(e.T, err)
				e.SettleSigned(a, "Delete")
				resp, va, err = e.SignedDescribe(vm.Host, OwnerA.UID, binding, e.NewNonce())
				require.NoError(e.T, err, "signed Describe after the Delete")
				assert.False(e.T, resp.GetExists(), "the placement is gone")
				assert.Equal(e.T, providerv1.VMPresence_VM_PRESENCE_DELETED, va.GetPresence(), "DELETED after the Delete completed")
				assert.Equal(e.T, del.Identity.OpID, va.GetDeleteOpId(), "DELETED names the Delete")
				resp, va, err = e.SignedDescribe(vm.Host, OwnerB.UID, e.NewNonce(), e.NewNonce())
				require.NoError(e.T, err, "signed Describe of a placement the host never had")
				assert.False(e.T, resp.GetExists(), "a placement the host never had does not exist")
				assert.Equal(e.T, providerv1.VMPresence_VM_PRESENCE_NO_RECORD, va.GetPresence(), "a placement the host never had is NO_RECORD, never DELETED")
			},
		}),
		answerCase(Case{
			ID:     "identity.answer-fresh",
			Rule:   "every signed answer echoes the nonce of the request it answers, so an answer replayed for another request is detected",
			Source: srcHostAnswers + " (AnswerContext.nonce)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-fresh")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				a, err := e.SendSigned(op)
				require.NoError(e.T, err)
				e.SettleSigned(a, "POWER_ON")
				n1, n2 := e.NewNonce(), e.NewNonce()
				first := e.SignedTaskStatus(op, n1)
				second := e.SignedTaskStatus(op, n2)
				requireSigned(e, first, "the first request")
				requireSigned(e, second, "the second request")
				assert.False(e.T, bytes.Equal(first.Blob.GetBody(), second.Blob.GetBody()), "two requests never get the same answer body")
				replayed := &providerv1.OperationAnswer{}
				assert.Error(e.T, e.VerifyHostAnswer(first.Blob, first.Blob.GetBodyType(), vm.Host, n2, replayed),
					"the first answer, replayed for the second request, is detected")
			},
		}),
		answerCase(Case{
			ID:     "identity.attempt-regression",
			Rule:   "a new op_id whose attempt is not above an attempt the host already accepted for the VM and kind is refused with OP_ID_CONFLICT, and nothing is performed",
			Source: srcAttempt,
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-attempt")
				high := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				high.Identity.Attempt += 2
				a, err := e.SendSigned(high)
				require.NoError(e.T, err, "attempt n+1 is accepted")
				e.SettleSigned(a, "attempt n+1")
				off := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF))
				a, err = e.SendSigned(off)
				require.NoError(e.T, err)
				e.SettleSigned(a, "POWER_OFF")
				low := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				low.Identity.Attempt = high.Identity.Attempt - 1
				_, err = e.SendSigned(low)
				requireRefusedWith(e, err, contracts.OpIDConflictReason, "attempt n arriving after attempt n+1")
				requireNotRunning(e, vm, "a regressed attempt")
			},
		}),
		answerCase(Case{
			ID:     "identity.inner-unknown-field",
			Rule:   "an authentic grant over request bytes that carry a field the host does not know is refused with GRANT_MISMATCH, and nothing is performed",
			Source: srcEnvelope + " (\"a decoded request message that carries a field it does not know\"); contracts.GrantMismatchReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-inner-unknown")
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				// An authentic grant over the request with an unknown field: a
				// minter refuses to sign such bytes, so the grant is re-signed.
				withUnknown := protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), op.Request...), 999, protowire.VarintType), 1)
				g := e.Resign(op, func(b *providerv1.GrantBody) { b.IntentDigest = operation.IntentDigest(b.GetDigestSalt(), withUnknown) })
				_, err := e.SendEnvelope(providerv1.Provider_Power_FullMethodName, &providerv1.OperationEnvelope{Request: withUnknown, Grant: g}, nil)
				requireRefusedWith(e, err, contracts.GrantMismatchReason, "request bytes with an unknown field")
				requireNotRunning(e, vm, "a request with an unknown field")
			},
		}),
		answerCase(Case{
			ID:     "identity.store-quarantined",
			Rule:   "a host whose operation store was rolled back refuses every mutation with QUARANTINED, never as definitely not performed, performs nothing, and still answers signed reads with its quarantine",
			Source: srcHostAnswers + " (AgentState QUARANTINED, state_reason); contracts.QuarantinedReason",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-quarantine")
				h := e.HostAnswers()
				require.NoError(e.T, h.SnapshotHost(vm.Host), "keep a copy of the host's store")
				on := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				a, err := e.SendSigned(on)
				require.NoError(e.T, err)
				e.SettleSigned(a, "POWER_ON")
				require.True(e.T, e.Observe(vm).Running, "the VM runs")
				require.NoError(e.T, h.RollBackHost(vm.Host), "roll the host's store back")
				off := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_OFF))
				_, err = e.SendSigned(off)
				w := requireRefusedWith(e, err, contracts.QuarantinedReason, "a mutation on a rolled-back store")
				for _, r := range w.Reasons {
					assert.False(e.T, contracts.IsDefiniteNotPerformedReason(r.Reason), "a quarantine is never definite: %s", r.Reason)
				}
				e.FinishTasks()
				assert.True(e.T, e.Observe(vm).Running, "a quarantined host performs nothing")
				s := e.SignedTaskStatus(on, e.NewNonce())
				var ans *providerv1.OperationAnswer
				if assert.Empty(e.T, s.Invalid, "the signed answer must verify") && assert.NotNil(e.T, s.Answer, "a quarantined host still answers signed reads (call error: %v)", s.Err) {
					ans = s.Answer
					assert.Equal(e.T, providerv1.AgentState_AGENT_STATE_QUARANTINED, ans.GetContext().GetAgentState(), "the answer carries the quarantine")
					assert.NotEmpty(e.T, ans.GetContext().GetStateReason(), "and its reason")
				}
			},
		}),
		answerCase(Case{
			ID:     "identity.no-running-forever",
			Rule:   "an operation whose step never ends on its own leaves the running state by the host's step deadline: it ends, or its outcome is unknown, and it is never reported running past it",
			Source: srcOutcomes + "; " + srcHostAnswers + " (OperationRecordState)",
			Run: func(e *Env) {
				vm := seedStopped(e, e.HostA(), "id-stall")
				h := e.HostAnswers()
				require.NoError(e.T, h.StallNextStep(vm.Host))
				op := e.NewOp(providerv1.OperationKind_OPERATION_KIND_POWER_ON, vm.Host, powerReq(vm, providerv1.PowerOp_POWER_OP_ON))
				a, err := e.SendSigned(op)
				require.NoError(e.T, err, "the operation is accepted")
				require.NotEmpty(e.T, a.TaskRef, "an operation that stalls returns a task")
				limit := time.Now().Add(2*h.StepDeadline() + 10*time.Second)
				tick := time.NewTicker(50 * time.Millisecond)
				defer tick.Stop()
				for {
					st, err := e.TaskStatus(a.TaskRef)
					require.NoError(e.T, err, "poll the stalled operation")
					if st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_RUNNING || time.Now().After(limit) {
						break
					}
					<-tick.C
				}
				last := requireSigned(e, e.SignedTaskStatus(op, e.NewNonce()), "signed TaskStatus of a stalled operation").GetState()
				assert.Contains(e.T, []providerv1.OperationRecordState{
					providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
					providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY,
					providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN,
				}, last, "a stalled operation ends or its outcome is unknown by the step deadline, never running past it nor completed")
			},
		}),
	}
}
