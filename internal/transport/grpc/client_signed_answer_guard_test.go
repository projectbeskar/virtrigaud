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
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin that the manager's transport neither sets the signed host
// answer request fields nor reads a host_answer: the requests it sends carry
// none of them, and an answer that carries one (in a response field or as an
// extra status detail) is read exactly as the same answer without it.

// signedAnswerGuardServer answers TaskStatus, Describe and GetHostInfo with
// fixed answers, each with a SignedBlob of arbitrary bytes when withAnswer is
// set, and records every request.
type signedAnswerGuardServer struct {
	providerv1.UnimplementedProviderServer
	withAnswer     bool
	unknownToAgent bool

	mu        sync.Mutex
	taskReqs  []*providerv1.TaskStatusRequest
	descReqs  []*providerv1.DescribeRequest
	hostsReqs []*providerv1.GetHostInfoRequest
}

func arbitrarySignedBlob() *providerv1.SignedBlob {
	return &providerv1.SignedBlob{
		Body:              []byte{0xff, 0x00, 0x12, 0x06, 0x76},
		Signature:         []byte{0x30, 0x44, 0x02},
		SignerCertificate: []byte{0x30, 0x82, 0x01},
		BodyType:          "provider.v1.OperationAnswer",
	}
}

func (s *signedAnswerGuardServer) answer() *providerv1.SignedBlob {
	if !s.withAnswer {
		return nil
	}
	return arbitrarySignedBlob()
}

func (s *signedAnswerGuardServer) TaskStatus(_ context.Context, r *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	s.mu.Lock()
	s.taskReqs = append(s.taskReqs, r)
	s.mu.Unlock()
	if s.unknownToAgent {
		st := status.New(codes.NotFound, "host \"host-a\" has no record of the task")
		details := []protoadapt.MessageV1{&errdetails.ErrorInfo{Reason: contracts.UnknownToAgentReason, Domain: contracts.ErrorInfoDomain}}
		if s.withAnswer {
			details = append(details, arbitrarySignedBlob())
		}
		withDetails, err := st.WithDetails(details...)
		if err != nil {
			return nil, err
		}
		return nil, withDetails.Err()
	}
	return &providerv1.TaskStatusResponse{
		Done:       true,
		Outcome:    providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED,
		HostAnswer: s.answer(),
	}, nil
}

func (s *signedAnswerGuardServer) Describe(_ context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.mu.Lock()
	s.descReqs = append(s.descReqs, r)
	s.mu.Unlock()
	return &providerv1.DescribeResponse{
		Exists:     true,
		PowerState: "On",
		Vcpus:      2,
		HostAnswer: s.answer(),
	}, nil
}

func (s *signedAnswerGuardServer) GetHostInfo(_ context.Context, r *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	s.mu.Lock()
	s.hostsReqs = append(s.hostsReqs, r)
	s.mu.Unlock()
	return &providerv1.HostInfo{
		Id:             r.GetHostId(),
		AllocatableCpu: 16,
		Health:         providerv1.HostHealth_HOST_HEALTH_READY,
		HostAnswer:     s.answer(),
	}, nil
}

// signedAnswerGuardResults is what the manager's client makes of one round of
// TaskStatus, IsTaskComplete, Describe and GetHostInfo.
type signedAnswerGuardResults struct {
	task     contracts.TaskStatus
	taskErr  string
	done     bool
	doneErr  string
	describe contracts.DescribeResponse
	descErr  string
	host     contracts.HostInfo
	hostErr  string
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func runSignedAnswerGuard(t *testing.T, srv *signedAnswerGuardServer) signedAnswerGuardResults {
	t.Helper()
	c := newTestClientForTasks(t, srv, "guard", "guard-provider")
	ctx := context.Background()
	var r signedAnswerGuardResults
	var err error
	r.task, err = c.TaskStatus(ctx, "host-task/v1/host-a/job-1")
	r.taskErr = errText(err)
	r.done, err = c.IsTaskComplete(ctx, "host-task/v1/host-a/job-1")
	r.doneErr = errText(err)
	r.describe, err = c.Describe(ctx, contracts.VMRef{
		ID:     "vm-1",
		HostID: "host-a",
		Owner:  contracts.ObjectIdentity{UID: "0b8f5e0e-7a53-4f6b-9a0e-1d2c3b4a5f60", Namespace: "team-a", Name: "web"},
	})
	r.descErr = errText(err)
	r.host, err = c.GetHostInfo(ctx, "host-a")
	r.hostErr = errText(err)
	return r
}

// TestSignedAnswerFields_ManagerSendsNone: no request the manager sends sets
// a signed host answer request field.
func TestSignedAnswerFields_ManagerSendsNone(t *testing.T) {
	srv := &signedAnswerGuardServer{}
	runSignedAnswerGuard(t, srv)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.NotEmpty(t, srv.taskReqs)
	require.NotEmpty(t, srv.descReqs)
	require.NotEmpty(t, srv.hostsReqs)
	for _, r := range srv.taskReqs {
		assert.Empty(t, r.GetAnswerNonce())
		assert.Empty(t, r.GetOpId())
		assert.Empty(t, r.GetOpEnrolmentId())
		assert.Empty(t, r.GetTargetHostId())
		assert.Zero(t, r.GetContinuitySeq())
		assert.Zero(t, r.GetLedgerVersion(), "the manager never asks for the reserved ledger fields")
		assert.NotNil(t, r.GetTask(), "the manager polls by task reference")
	}
	for _, r := range srv.descReqs {
		assert.Empty(t, r.GetAnswerNonce())
		assert.Empty(t, r.GetBindingId())
		assert.Zero(t, r.GetContinuitySeq())
		assert.Zero(t, r.GetLedgerVersion(), "the manager never asks for the reserved ledger fields")
	}
	for _, r := range srv.hostsReqs {
		assert.Empty(t, r.GetAnswerNonce())
		assert.Zero(t, r.GetContinuitySeq())
		assert.Zero(t, r.GetLedgerVersion(), "the manager never asks for the reserved ledger fields")
	}
}

// TestSignedAnswerFields_ManagerIgnoresHostAnswer: a host_answer in a
// response changes nothing the client returns.
func TestSignedAnswerFields_ManagerIgnoresHostAnswer(t *testing.T) {
	plain := runSignedAnswerGuard(t, &signedAnswerGuardServer{})
	signed := runSignedAnswerGuard(t, &signedAnswerGuardServer{withAnswer: true})
	assert.Equal(t, plain, signed)
	assert.True(t, plain.done)
	assert.True(t, plain.describe.Exists)
	assert.Equal(t, "host-a", plain.host.ID)
}

// receiptServer answers Create and Delete with a task, and with an
// operation_receipt of arbitrary bytes when withReceipt is set.
type receiptServer struct {
	providerv1.UnimplementedProviderServer
	withReceipt bool
}

func (s *receiptServer) receipt() *providerv1.SignedBlob {
	if !s.withReceipt {
		return nil
	}
	b := arbitrarySignedBlob()
	b.BodyType = "provider.v1.OperationReceipt"
	return b
}

func (s *receiptServer) Create(_ context.Context, r *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	return &providerv1.CreateResponse{Id: r.GetName(), Task: &providerv1.TaskRef{Id: "task-create"}, OperationReceipt: s.receipt()}, nil
}

func (s *receiptServer) Delete(_ context.Context, _ *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	return &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-delete"}, OperationReceipt: s.receipt()}, nil
}

// TestSignedAnswerFields_ManagerIgnoresOperationReceipt: the reserved
// operation_receipt on an accept response changes nothing the client returns
// (the manager predates the host ledger protocol and never reads it).
func TestSignedAnswerFields_ManagerIgnoresOperationReceipt(t *testing.T) {
	run := func(srv *receiptServer) (contracts.CreateResponse, string) {
		c := newTestClientForVMOps(t, srv, "libvirt", "receipt-guard")
		created, err := c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: testOwner})
		require.NoError(t, err)
		deleted, err := c.Delete(context.Background(), contracts.VMRef{ID: "web", Owner: testOwner})
		require.NoError(t, err)
		return created, deleted
	}
	plainCreate, plainDelete := run(&receiptServer{})
	withCreate, withDelete := run(&receiptServer{withReceipt: true})
	assert.Equal(t, plainCreate, withCreate)
	assert.Equal(t, plainDelete, withDelete)
	assert.Equal(t, "task-create", withCreate.TaskRef)
	assert.Equal(t, "task-delete", withDelete)
}

// TestSignedAnswerFields_ManagerIgnoresSignedBlobDetail: a NotFound
// UNKNOWN_TO_AGENT status with an extra SignedBlob detail is read exactly as
// the same status without it.
func TestSignedAnswerFields_ManagerIgnoresSignedBlobDetail(t *testing.T) {
	plain := runSignedAnswerGuard(t, &signedAnswerGuardServer{unknownToAgent: true})
	signed := runSignedAnswerGuard(t, &signedAnswerGuardServer{unknownToAgent: true, withAnswer: true})
	assert.Equal(t, plain, signed)
	assert.False(t, signed.done)
	assert.NotEmpty(t, signed.doneErr)

	c := newTestClientForTasks(t, &signedAnswerGuardServer{unknownToAgent: true, withAnswer: true}, "guard", "guard-provider")
	done, err := c.IsTaskComplete(context.Background(), "host-task/v1/host-a/job-1")
	assert.False(t, done)
	assert.True(t, contracts.IsUnknownToAgent(err), "still marked UNKNOWN_TO_AGENT: %v", err)
	assert.False(t, contracts.IsNotPerformed(err))
}
