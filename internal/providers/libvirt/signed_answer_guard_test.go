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

package libvirt

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin that the libvirt provider, single-host and clustered,
// neither reads the signed host answer request fields nor sets host_answer.

func TestSignedAnswerFields_ClusteredTaskStatusIgnored(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		ref  *providerv1.TaskRef
		done bool
	}{
		"issued, done":    {&providerv1.TaskRef{Id: clustered.EncodeHostTaskRef("host-a", "job-1")}, true},
		"issued, running": {&providerv1.TaskRef{Id: clustered.EncodeHostTaskRef("host-a", "job-1")}, false},
		"unissued":        {&providerv1.TaskRef{Id: clustered.EncodeHostTaskRef("host-b", "job-secret-1")}, false},
		"forged":          {&providerv1.TaskRef{Id: "host-task/v1/host-zzz/t"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := taskCluster(t)
			s := NewServer(&doneTaskBackend{Provider: p, done: tc.done})
			req := &providerv1.TaskStatusRequest{Task: tc.ref}
			plain, plainErr := s.TaskStatus(ctx, req)
			signed, signedErr := s.TaskStatus(ctx, signedAnswerFieldsTaskStatus(req))
			assertSameAnswer(t, plain, plainErr, signed, signedErr)
			assert.Nil(t, signed.GetHostAnswer())
		})
	}
}

// TestSignedAnswerFields_ClusteredTaskStatusRoutesByTaskOnly: a request with
// task unset and target_host_id set is answered exactly as today's request
// without a task reference, and nothing is dialed: the provider routes by
// the task reference only.
func TestSignedAnswerFields_ClusteredTaskStatusRoutesByTaskOnly(t *testing.T) {
	ctx := context.Background()
	p, dialed := taskCluster(t)
	s := NewServer(p)
	plain, plainErr := s.TaskStatus(ctx, &providerv1.TaskStatusRequest{})
	signed, signedErr := s.TaskStatus(ctx, signedAnswerFieldsTaskStatus(&providerv1.TaskStatusRequest{}))
	require.Error(t, plainErr)
	assert.Equal(t, codes.InvalidArgument, status.Code(plainErr))
	assertSameAnswer(t, plain, plainErr, signed, signedErr)
	assert.Empty(t, dialed(), "target_host_id never routes a TaskStatus")
	assert.Zero(t, p.virshProvider.unroutableHits.Load())
}

func TestSignedAnswerFields_SingleHostTaskStatusIgnored(t *testing.T) {
	ctx := context.Background()
	s := NewServer(&Provider{virshProvider: localHostVP("single")})
	for _, ref := range []*providerv1.TaskRef{{Id: "task-123"}, {Id: ""}} {
		req := &providerv1.TaskStatusRequest{Task: ref}
		plain, plainErr := s.TaskStatus(ctx, req)
		signed, signedErr := s.TaskStatus(ctx, signedAnswerFieldsTaskStatus(req))
		assertSameAnswer(t, plain, plainErr, signed, signedErr)
		assert.Nil(t, signed.GetHostAnswer())
	}
}

func TestSignedAnswerFields_ClusteredDescribeIgnored(t *testing.T) {
	ctx := context.Background()
	newRoutingFixture(t, map[string]map[string]string{
		"host-a": {},
		"host-b": {"db": routingDomainXML("db", ownerTeamA)},
	})
	p, _, _ := routedCluster(t)
	s := NewServer(p)
	owner := &providerv1.ObjectIdentity{Uid: ownerTeamA.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name}
	for _, req := range []*providerv1.DescribeRequest{
		{Id: "db", TargetHostId: "host-b", Owner: owner},
		{Id: "gone", TargetHostId: "host-a", Owner: owner},
		{Id: "db", TargetHostId: "host-zzz", Owner: owner},
	} {
		plain, plainErr := s.Describe(ctx, req)
		if req.GetId() == "db" && req.GetTargetHostId() == "host-b" {
			require.NoError(t, plainErr)
			require.True(t, plain.GetExists())
		}
		signed, signedErr := s.Describe(ctx, signedAnswerFieldsDescribe(req))
		assertSameAnswer(t, plain, plainErr, signed, signedErr)
		assert.Nil(t, signed.GetHostAnswer(), req.GetId())
	}
}

func TestSignedAnswerFields_SingleHostDescribeIgnored(t *testing.T) {
	ctx := context.Background()
	newRoutingFixture(t, map[string]map[string]string{
		"single": {"web": routingDomainXML("web", contracts.ObjectIdentity{})},
	})
	s := NewServer(&Provider{virshProvider: localHostVP("single")})
	for _, id := range []string{"web", "gone"} {
		req := &providerv1.DescribeRequest{Id: id}
		plain, plainErr := s.Describe(ctx, req)
		signed, signedErr := s.Describe(ctx, signedAnswerFieldsDescribe(req))
		assertSameAnswer(t, plain, plainErr, signed, signedErr)
		assert.Nil(t, signed.GetHostAnswer(), id)
	}
}

func TestSignedAnswerFields_GetHostInfoIgnored(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]*Server{
		"single-host": {},
		"clustered":   serverWithClusteredHostA(t),
	} {
		for _, host := range []string{"host-a", "host-nope"} {
			req := &providerv1.GetHostInfoRequest{HostId: host}
			plain, plainErr := s.GetHostInfo(ctx, req)
			if name == "clustered" && host == "host-a" {
				require.NoError(t, plainErr)
			}
			signed, signedErr := s.GetHostInfo(ctx, signedAnswerFieldsGetHostInfo(req))
			assertSameAnswer(t, plain, plainErr, signed, signedErr)
			assert.Nil(t, signed.GetHostAnswer(), name+"/"+host)
		}
	}
}

// The helpers below pin that this provider neither reads the signed host
// answer request fields nor sets a host_answer: a request carrying them is
// answered exactly as the same request without them.

// signedAnswerFieldsTaskStatus returns a copy of req with every signed
// answer request field of TaskStatusRequest set to a valid-looking value.
func signedAnswerFieldsTaskStatus(req *providerv1.TaskStatusRequest) *providerv1.TaskStatusRequest {
	out, _ := proto.Clone(req).(*providerv1.TaskStatusRequest)
	if out == nil {
		out = &providerv1.TaskStatusRequest{}
	}
	out.AnswerNonce = bytes.Repeat([]byte{0x5a}, 16)
	out.OpId = bytes.Repeat([]byte{0x5b}, 16)
	out.OpEnrolmentId = bytes.Repeat([]byte{0x5c}, 16)
	out.TargetHostId = "host-a"
	out.ContinuitySeq = 7
	return out
}

// signedAnswerFieldsDescribe returns a copy of req with every signed answer
// request field of DescribeRequest set to a valid-looking value.
func signedAnswerFieldsDescribe(req *providerv1.DescribeRequest) *providerv1.DescribeRequest {
	out, _ := proto.Clone(req).(*providerv1.DescribeRequest)
	if out == nil {
		out = &providerv1.DescribeRequest{}
	}
	out.AnswerNonce = bytes.Repeat([]byte{0x5a}, 16)
	out.BindingId = bytes.Repeat([]byte{0x5d}, 16)
	out.ContinuitySeq = 7
	return out
}

// signedAnswerFieldsGetHostInfo returns a copy of req with every signed
// answer request field of GetHostInfoRequest set to a valid-looking value.
func signedAnswerFieldsGetHostInfo(req *providerv1.GetHostInfoRequest) *providerv1.GetHostInfoRequest {
	out, _ := proto.Clone(req).(*providerv1.GetHostInfoRequest)
	if out == nil {
		out = &providerv1.GetHostInfoRequest{}
	}
	out.AnswerNonce = bytes.Repeat([]byte{0x5a}, 16)
	out.ContinuitySeq = 7
	return out
}

// assertSameAnswer fails unless the answer to the request with the signed
// answer fields equals the answer without them: the same message and the
// same gRPC status, details included.
func assertSameAnswer[M proto.Message](t *testing.T, plain M, plainErr error, signed M, signedErr error) {
	t.Helper()
	assert.True(t, proto.Equal(status.Convert(plainErr).Proto(), status.Convert(signedErr).Proto()),
		"status changed: plain %v, with signed answer fields %v", plainErr, signedErr)
	assert.True(t, proto.Equal(plain, signed),
		"answer changed: plain %v, with signed answer fields %v", plain, signed)
}
