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
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The host ledger protocol is reserved, not implemented: this agent behaves
// as an agent that predates it. A request's ledger_version is ignored (the
// answer is the one the same request without it gets: no ledger field, no
// echo), and no response carries an operation receipt.

// ledgerVersions are ledger_version values a newer manager might send.
var ledgerVersions = []uint32{1, 2, 1 << 31}

// sameAnswer fails unless a and b are equal apart from the context's
// agent_time (the host's clock when each was made).
func sameAnswer(t *testing.T, a, b proto.Message, ctxOf func(proto.Message) *providerv1.AnswerContext) {
	t.Helper()
	a, b = proto.Clone(a), proto.Clone(b)
	ctxOf(a).AgentTime, ctxOf(b).AgentTime = nil, nil
	if !proto.Equal(a, b) {
		t.Fatalf("the answer to a request with ledger_version differs:\n got %v\nwant %v", b, a)
	}
}

// TestLedgerVersion_GetHostInfoIgnored: GetHostInfo with a ledger_version is
// answered as without it, with or without a nonce.
func TestLedgerVersion_GetHostInfoIgnored(t *testing.T) {
	e := newEnv(t)
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	ask := func(t *testing.T, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, *providerv1.HostAnswer) {
		t.Helper()
		info, err := pc.GetHostInfo(ctx(t), req)
		require.NoError(t, err)
		return info, verifyAnswer(t, e, info.GetHostAnswer())
	}
	hostCtx := func(m proto.Message) *providerv1.AnswerContext { return m.(*providerv1.HostAnswer).GetContext() }
	for _, v := range ledgerVersions {
		_, plain := ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(7), StoreCapacityVersion: 1})
		_, with := ask(t, &providerv1.GetHostInfoRequest{AnswerNonce: nonce(7), StoreCapacityVersion: 1, LedgerVersion: v})
		require.NoError(t, hostidentity.CheckNoLedgerFields(with))
		assert.Nil(t, with.GetLedger())
		assert.Zero(t, with.GetContext().GetLedgerVersionEcho())
		assert.Empty(t, with.GetContext().GetAttestationId())
		assert.Equal(t, answer.AgentAPIVersion, with.GetAgentApiVersion())
		assert.Equal(t, []uint32{answer.GrantVersion}, with.GetGrantVersions())
		sameAnswer(t, plain, with, hostCtx)

		// Without a nonce it is not refused either (unlike the
		// implemented versions): an agent that predates it never sees it.
		info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{LedgerVersion: v})
		require.NoError(t, err)
		assert.Nil(t, info.GetHostAnswer())
	}
}

// TestLedgerReserved_HostAuthorityNotServed: the agent registers no
// HostAuthority service, so each of its methods is Unimplemented.
func TestLedgerReserved_HostAuthorityNotServed(t *testing.T) {
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

// TestLedgerVersion_TaskStatusAndDescribeIgnored: TaskStatus and Describe
// with a ledger_version are answered as without it, signed or plain; and the
// accept response of a mutation carries no operation receipt.
func TestLedgerVersion_TaskStatusAndDescribeIgnored(t *testing.T) {
	e := newOpsEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))

	created := &providerv1.CreateResponse{}
	req := &providerv1.CreateRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)}}
	require.NoError(t, e.cc().Invoke(ctx(t), providerv1.Provider_Create_FullMethodName, req, created))
	assert.Nil(t, created.GetOperationReceipt(), "an accept response never carries a receipt")
	assert.Empty(t, created.ProtoReflect().GetUnknown())
	task := created.GetTask().GetId()
	require.NotEmpty(t, task)
	e.waitDone(task)

	c := e.client()
	opCtx := func(m proto.Message) *providerv1.AnswerContext { return m.(*providerv1.OperationAnswer).GetContext() }
	vmCtx := func(m proto.Message) *providerv1.AnswerContext { return m.(*providerv1.VMAnswer).GetContext() }
	for _, v := range ledgerVersions {
		signed := func(lv uint32) *providerv1.OperationAnswer {
			resp, err := c.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{AnswerNonce: nonce(8), OpId: o.id.OpID,
				OpEnrolmentId: o.id.EnrolmentID, TargetHostId: trusttest.HostName, LedgerVersion: lv})
			require.NoError(t, err)
			a := &providerv1.OperationAnswer{}
			verifyBlob(t, e, resp.GetHostAnswer(), answer.BodyTypeOperationAnswer, a)
			return a
		}
		plainOp, withOp := signed(0), signed(v)
		require.NoError(t, hostidentity.CheckNoLedgerFields(withOp))
		assert.Zero(t, withOp.GetAcceptedSeq())
		assert.Empty(t, withOp.GetAcceptedHead())
		assert.False(t, withOp.GetAwaitingRequest())
		sameAnswer(t, plainOp, withOp, opCtx)

		plainStatus, err := c.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
		require.NoError(t, err)
		withStatus, err := c.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}, LedgerVersion: v})
		require.NoError(t, err, "a plain TaskStatus with ledger_version is answered as without it")
		assert.True(t, proto.Equal(plainStatus, withStatus))

		describe := func(lv uint32) *providerv1.VMAnswer {
			resp, err := c.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), TargetHostId: trusttest.HostName,
				AnswerNonce: nonce(9), BindingId: o.id.BindingID, LedgerVersion: lv})
			require.NoError(t, err)
			a := &providerv1.VMAnswer{}
			verifyBlob(t, e, resp.GetHostAnswer(), answer.BodyTypeVMAnswer, a)
			return a
		}
		plainVM, withVM := describe(0), describe(v)
		require.NoError(t, hostidentity.CheckNoLedgerFields(withVM))
		sameAnswer(t, plainVM, withVM, vmCtx)

		plainD, err := c.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1")})
		require.NoError(t, err)
		withD, err := c.Describe(ctx(t), &providerv1.DescribeRequest{Owner: ownerOf("vm-1"), LedgerVersion: v})
		require.NoError(t, err, "a plain Describe with ledger_version is answered as without it")
		assert.True(t, proto.Equal(plainD, withD))
	}
}
