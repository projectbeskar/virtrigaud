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

package grpc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// answerServer records the signed reads it receives and answers from scripts.
type answerServer struct {
	providerv1.UnimplementedProviderServer
	mu        sync.Mutex
	tasks     []*providerv1.TaskStatusRequest
	describes []*providerv1.DescribeRequest
	hosts     []*providerv1.GetHostInfoRequest
	blob      *providerv1.SignedBlob
	err       func() error
}

func (s *answerServer) TaskStatus(_ context.Context, r *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, proto.CloneOf(r))
	if s.err != nil {
		return nil, s.err()
	}
	return &providerv1.TaskStatusResponse{Done: true, Outcome: providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, HostAnswer: s.blob}, nil
}

func (s *answerServer) Describe(_ context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.describes = append(s.describes, proto.CloneOf(r))
	return &providerv1.DescribeResponse{Exists: false, HostAnswer: s.blob}, nil
}

func (s *answerServer) GetHostInfo(_ context.Context, r *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = append(s.hosts, proto.CloneOf(r))
	return &providerv1.HostInfo{Id: r.GetHostId(), HostAnswer: s.blob}, nil
}

func testBlob() *providerv1.SignedBlob {
	return &providerv1.SignedBlob{Body: []byte{1, 2}, Signature: []byte{3}, SignerCertificate: []byte{4}, BodyType: "provider.v1.OperationAnswer"}
}

func statusWith(details ...proto.Message) error {
	st := status.New(codes.NotFound, "no record")
	pst := st.Proto()
	for _, d := range details {
		a, _ := anypb.New(d)
		pst.Details = append(pst.Details, a)
	}
	return status.FromProto(pst).Err()
}

// The signed TaskStatus names the operation by op_id with a fresh nonce per
// request and never a task reference; the answer is relayed with the nonce.
func TestClient_OperationAnswer(t *testing.T) {
	srv := &answerServer{blob: testBlob()}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "cloudhypervisor")
	q := contracts.OperationAnswerQuery{OpID: bytes.Repeat([]byte{1}, 16), OpEnrolmentID: bytes.Repeat([]byte{2}, 16),
		HostID: "host-a", ContinuitySeq: 9}
	st, read, err := c.OperationAnswer(context.Background(), q)
	require.NoError(t, err)
	assert.Equal(t, contracts.TaskOutcomeSucceeded, st.Outcome)
	require.NotNil(t, read.Answer)
	assert.Equal(t, testBlob().GetBody(), read.Answer.Body)
	_, read2, err := c.OperationAnswer(context.Background(), q)
	require.NoError(t, err)

	require.Len(t, srv.tasks, 2)
	r := srv.tasks[0]
	assert.Nil(t, r.GetTask(), "a signed poll never carries a task reference")
	assert.Equal(t, q.OpID, r.GetOpId())
	assert.Equal(t, q.OpEnrolmentID, r.GetOpEnrolmentId())
	assert.Equal(t, "host-a", r.GetTargetHostId())
	assert.Equal(t, uint64(9), r.GetContinuitySeq())
	assert.Len(t, r.GetAnswerNonce(), 16)
	assert.Equal(t, read.Nonce, r.GetAnswerNonce())
	assert.NotEqual(t, read.Nonce, read2.Nonce, "every request draws a fresh nonce")
}

// A signed answer in an error status is extracted only when it is the one
// SignedBlob detail; the error is mapped as for a plain read.
func TestClient_OperationAnswerFromStatus(t *testing.T) {
	info := &errdetails.ErrorInfo{Reason: contracts.UnknownToAgentReason, Domain: contracts.ErrorInfoDomain}
	withUnknown := testBlob()
	withUnknown.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	big := testBlob()
	big.Body = make([]byte, 17<<10)
	cases := map[string]struct {
		err  error
		want bool
	}{
		"one detail":              {statusWith(info, testBlob()), true},
		"no detail":               {statusWith(info), false},
		"two details":             {statusWith(info, testBlob(), testBlob()), false},
		"detail with unknowns":    {statusWith(info, withUnknown), false},
		"detail over 16 KiB":      {statusWith(info, big), false},
		"plain transport failure": {status.Error(codes.Unavailable, "down"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := &answerServer{err: func() error { return tc.err }}
			dialer, cleanup := startBufconnServer(t, srv)
			defer cleanup()
			c := newTestClient(t, dialer, "cloudhypervisor")
			_, read, err := c.OperationAnswer(context.Background(), contracts.OperationAnswerQuery{OpID: make([]byte, 16),
				OpEnrolmentID: make([]byte, 16), HostID: "host-a"})
			require.Error(t, err)
			assert.Equal(t, tc.want, read.Answer != nil)
			assert.Len(t, read.Nonce, 16)
		})
	}
}

// A response field with unknown fields is no answer.
func TestClient_AnswerFieldWithUnknownsIsNoAnswer(t *testing.T) {
	b := testBlob()
	b.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	srv := &answerServer{blob: b}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "cloudhypervisor")
	_, read, err := c.HostAnswer(context.Background(), "host-a", 0)
	require.NoError(t, err)
	assert.Nil(t, read.Answer)
	require.Len(t, srv.hosts, 1)
	assert.Len(t, srv.hosts[0].GetAnswerNonce(), 16)
	assert.Zero(t, srv.hosts[0].GetInventoryVersion(), "no inventory is asked for")
}

// The signed Describe carries the owner, the binding and a nonce.
func TestClient_VMAnswer(t *testing.T) {
	srv := &answerServer{blob: testBlob()}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "cloudhypervisor")
	binding := bytes.Repeat([]byte{5}, 16)
	resp, read, err := c.VMAnswer(context.Background(), contracts.VMAnswerQuery{VM: opTestVM, BindingID: binding})
	require.NoError(t, err)
	assert.False(t, resp.Exists)
	require.NotNil(t, read.Answer)
	require.Len(t, srv.describes, 1)
	r := srv.describes[0]
	assert.Equal(t, binding, r.GetBindingId())
	assert.Equal(t, opTestVM.Owner.UID, r.GetOwner().GetUid())
	assert.Equal(t, read.Nonce, r.GetAnswerNonce())
}

// A nonce that cannot be drawn sends nothing.
func TestClient_NonceFailureSendsNothing(t *testing.T) {
	srv := &answerServer{blob: testBlob()}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "cloudhypervisor")
	old := nonceReader
	nonceReader = failingReader{}
	defer func() { nonceReader = old }()
	_, _, err := c.OperationAnswer(context.Background(), contracts.OperationAnswerQuery{HostID: "host-a"})
	require.Error(t, err)
	_, _, err = c.HostAnswer(context.Background(), "host-a", 0)
	require.Error(t, err)
	assert.Empty(t, srv.tasks)
	assert.Empty(t, srv.hosts)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
