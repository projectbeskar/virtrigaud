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

package server_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ledgerVersions are ledger_version values a manager might send.
var ledgerVersions = []uint32{1, 2, 1 << 31}

// TestLedgerVersion_GetHostInfo: a GetHostInfo that asks for a ledger version
// is answered with the ledger state and the echo, also by a server without
// an Authority (it reports the agent awaiting attestation); one that does not
// ask gets neither; ledger_version without a nonce is refused.
func TestLedgerVersion_GetHostInfo(t *testing.T) {
	e := newEnv(t)
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	ask := func(t *testing.T, req *providerv1.GetHostInfoRequest) *providerv1.HostAnswer {
		t.Helper()
		info, err := pc.GetHostInfo(ctx(t), req)
		require.NoError(t, err)
		return verifyAnswer(t, e, info.GetHostAnswer())
	}
	plain := ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(7)})
	assert.Nil(t, plain.GetLedger())
	assert.Zero(t, plain.GetContext().GetLedgerVersionEcho())
	assert.Equal(t, answer.AgentAPIVersion, plain.GetAgentApiVersion())
	assert.Equal(t, []uint32{answer.GrantVersion}, plain.GetGrantVersions())
	for _, v := range ledgerVersions {
		with := ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(7), LedgerVersion: v})
		require.NotNil(t, with.GetLedger())
		assert.Equal(t, v, with.GetContext().GetLedgerVersionEcho())
		assert.Equal(t, v, with.GetLedger().GetRequestedVersion())
		assert.Equal(t, min(v, answer.LedgerVersion), with.GetLedger().GetVersion())
		assert.Equal(t, providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION, with.GetLedger().GetState())
		assert.Empty(t, with.GetContext().GetAttestationId())

		_, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{LedgerVersion: v})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "ledger_version without answer_nonce")
	}
}

// TestLedger_HostAuthorityNotServedWithoutAuthority: a server without an
// Authority (a release build) registers no HostAuthority service.
func TestLedger_HostAuthorityNotServedWithoutAuthority(t *testing.T) {
	e := newEnv(t)
	ha := providerv1.NewHostAuthorityClient(e.dial(e.provider(), 0))
	_, err := ha.Challenge(ctx(t), &providerv1.ChallengeRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(1),
		Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "Challenge: %v", err)
	_, err = ha.Attest(ctx(t), &providerv1.AttestRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(2)})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "Attest: %v", err)
	_, err = ha.LedgerMark(ctx(t), &providerv1.LedgerMarkRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(3)})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "LedgerMark: %v", err)
	_, err = ha.RetireIfUnknown(ctx(t), &providerv1.RetireIfUnknownRequest{TargetHostId: trusttest.HostName, AnswerNonce: nonce(4)})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "RetireIfUnknown: %v", err)
}

// TestLedgerVersion_TaskStatusAndDescribe: a signed TaskStatus or Describe
// that asks for a ledger version echoes it and carries the acceptance
// position; a plain one that carries it is refused; and the accept response
// of an agent outside the ledger protocol carries no operation receipt.
func TestLedgerVersion_TaskStatusAndDescribe(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))

	created := &providerv1.CreateResponse{}
	req := &providerv1.CreateRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)}}
	require.NoError(t, e.cc().Invoke(ctx(t), providerv1.Provider_Create_FullMethodName, req, created))
	assert.Nil(t, created.GetOperationReceipt(), "an accept response carries no receipt")
	assert.Empty(t, created.ProtoReflect().GetUnknown())
	task := created.GetTask().GetId()
	require.NotEmpty(t, task)
	e.waitDone(task)

	c := e.client()
	for _, v := range ledgerVersions {
		resp, err := c.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{AnswerNonce: nonce(8), OpId: o.id.OpID,
			OpEnrolmentId: o.id.EnrolmentID, TargetHostId: trusttest.HostName, LedgerVersion: v})
		require.NoError(t, err)
		oa := &providerv1.OperationAnswer{}
		verifyBlob(t, e, resp.GetHostAnswer(), answer.BodyTypeOperationAnswer, oa)
		assert.Equal(t, v, oa.GetContext().GetLedgerVersionEcho())
		assert.NotZero(t, oa.GetAcceptedSeq(), "the acceptance position, from the record")
		assert.Empty(t, oa.GetAcceptedHead(), "no head once the operation left ACCEPTED")
		assert.False(t, oa.GetAwaitingRequest())

		_, err = c.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}, LedgerVersion: v})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "a plain TaskStatus with ledger_version")

		d, err := c.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), TargetHostId: trusttest.HostName,
			AnswerNonce: nonce(9), BindingId: o.id.BindingID, LedgerVersion: v})
		require.NoError(t, err)
		va := &providerv1.VMAnswer{}
		verifyBlob(t, e, d.GetHostAnswer(), answer.BodyTypeVMAnswer, va)
		assert.Equal(t, v, va.GetContext().GetLedgerVersionEcho())

		_, err = c.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), LedgerVersion: v})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "a plain Describe with ledger_version")
	}
}
