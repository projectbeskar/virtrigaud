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

package mock

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSignedAnswerFields_Ignored: the mock answers TaskStatus, Describe and
// GetHostInfo with the signed host answer request fields set exactly as
// without them, and never sets host_answer.
func TestSignedAnswerFields_Ignored(t *testing.T) {
	ctx := context.Background()
	p, _ := newImageProvider(t, time.Hour)

	prep, err := p.ImagePrepare(ctx, identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	running := prep.GetTask().GetId()
	finished, err := p.ImagePrepare(ctx, identityReq("team-a", "debian", imgUID2, digestA, provUID))
	require.NoError(t, err)
	require.True(t, p.FinishTask(finished.GetTask().GetId(), "download failed"))
	for _, id := range []string{running, finished.GetTask().GetId(), "no-such-task"} {
		req := &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: id}}
		plain, plainErr := p.TaskStatus(ctx, req)
		signed, signedErr := p.TaskStatus(ctx, signedAnswerFieldsTaskStatus(req))
		assertSameAnswer(t, plain, plainErr, signed, signedErr)
		assert.Nil(t, signed.GetHostAnswer(), id)
	}

	vm, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web"})
	require.NoError(t, err)
	for _, id := range []string{vm.GetId(), "vm-missing"} {
		req := &providerv1.DescribeRequest{Id: id}
		plain, plainErr := p.Describe(ctx, req)
		signed, signedErr := p.Describe(ctx, signedAnswerFieldsDescribe(req))
		assertSameAnswer(t, plain, plainErr, signed, signedErr)
		assert.Nil(t, signed.GetHostAnswer(), id)
	}

	req := &providerv1.GetHostInfoRequest{HostId: "host-a"}
	plain, plainErr := p.GetHostInfo(ctx, req)
	signed, signedErr := p.GetHostInfo(ctx, signedAnswerFieldsGetHostInfo(req))
	assertSameAnswer(t, plain, plainErr, signed, signedErr)
	assert.Nil(t, signed.GetHostAnswer())
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
