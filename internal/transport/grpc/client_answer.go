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
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Compile-time assertion that the client reads signed host answers.
var _ contracts.HostAnswerReader = (*Client)(nil)

// Timeouts of the signed reads: the same as the plain reads.
const (
	taskStatusCallTimeout  = 10 * time.Second
	describeCallTimeout    = 30 * time.Second
	getHostInfoCallTimeout = 30 * time.Second
)

// signedBlobTypeURL is the type URL of a SignedBlob status detail.
const signedBlobTypeURL = "type.googleapis.com/provider.v1.SignedBlob"

// nonceReader is where answer nonces are drawn from (crypto/rand; tests
// replace it to prove a failure sends nothing).
var nonceReader io.Reader = rand.Reader

// drawAnswerNonce draws a fresh 16-byte nonce for one request.
func drawAnswerNonce() ([]byte, error) {
	n := make([]byte, hostidentity.AnswerNonceLen)
	if _, err := io.ReadFull(nonceReader, n); err != nil {
		return nil, fmt.Errorf("draw an answer nonce: %w", err)
	}
	return n, nil
}

// OperationAnswer implements contracts.HostAnswerReader: a TaskStatus that
// names the operation by op_id, op_enrolment_id and target_host_id with a
// fresh nonce, and never a task reference.
func (c *Client) OperationAnswer(ctx context.Context, q contracts.OperationAnswerQuery) (contracts.TaskStatus, contracts.SignedRead, error) {
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.TaskStatus{}, contracts.SignedRead{}, err
	}
	read := contracts.SignedRead{Nonce: nonce}
	ctx, cancel := context.WithTimeout(ctx, taskStatusCallTimeout)
	defer cancel()
	resp, err := c.client.TaskStatus(ctx, &providerv1.TaskStatusRequest{AnswerNonce: nonce, OpId: q.OpID,
		OpEnrolmentId: q.OpEnrolmentID, TargetHostId: q.HostID, ContinuitySeq: q.ContinuitySeq})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return contracts.TaskStatus{}, read, c.mapGRPCError("taskStatus", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return readTaskStatus(resp), read, nil
}

// VMAnswer implements contracts.HostAnswerReader: a Describe of the VM that
// also asks about its placement binding with a fresh nonce.
func (c *Client) VMAnswer(ctx context.Context, q contracts.VMAnswerQuery) (contracts.DescribeResponse, contracts.SignedRead, error) {
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.DescribeResponse{}, contracts.SignedRead{}, err
	}
	read := contracts.SignedRead{Nonce: nonce}
	ctx, cancel := context.WithTimeout(ctx, describeCallTimeout)
	defer cancel()
	resp, err := c.client.Describe(ctx, &providerv1.DescribeRequest{Id: q.VM.ID, TargetHostId: q.VM.HostID, Owner: vmOwner(q.VM),
		AnswerNonce: nonce, BindingId: q.BindingID, ContinuitySeq: q.ContinuitySeq})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return contracts.DescribeResponse{}, read, c.mapGRPCError("describe", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return describeFromProto(resp), read, nil
}

// HostAnswer implements contracts.HostAnswerReader: a GetHostInfo with a
// fresh nonce and no inventory request.
func (c *Client) HostAnswer(ctx context.Context, hostID string, continuitySeq uint64) (contracts.HostInfo, contracts.SignedRead, error) {
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.HostInfo{}, contracts.SignedRead{}, err
	}
	read := contracts.SignedRead{Nonce: nonce}
	ctx, cancel := context.WithTimeout(ctx, getHostInfoCallTimeout)
	defer cancel()
	resp, err := c.client.GetHostInfo(ctx, &providerv1.GetHostInfoRequest{HostId: hostID, AnswerNonce: nonce, ContinuitySeq: continuitySeq})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return contracts.HostInfo{}, read, c.mapGRPCError("getHostInfo", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return hostInfoFromProto(resp), read, nil
}

// answerFromField extracts the signed answer of a response field: nil when
// absent, over its size bound, or carrying unknown fields.
func answerFromField(blob *providerv1.SignedBlob) *contracts.SignedAnswer {
	if blob == nil || proto.Size(blob) > hostidentity.MaxAnswerBlobBytes {
		return nil
	}
	return signedAnswerOf(blob)
}

// answerFromStatus extracts the signed answer of an error status: exactly one
// SignedBlob detail of at most MaxAnswerDetailBytes; none, two or more, an
// oversized one or one with unknown fields is no answer.
func answerFromStatus(err error) *contracts.SignedAnswer {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	var found *providerv1.SignedBlob
	for _, d := range st.Proto().GetDetails() {
		if d.GetTypeUrl() != signedBlobTypeURL {
			continue
		}
		if found != nil || len(d.GetValue()) > hostidentity.MaxAnswerDetailBytes {
			return nil
		}
		b := &providerv1.SignedBlob{}
		if proto.Unmarshal(d.GetValue(), b) != nil {
			return nil
		}
		found = b
	}
	if found == nil {
		return nil
	}
	return signedAnswerOf(found)
}

// signedAnswerOf copies blob, or returns nil when it carries unknown fields.
func signedAnswerOf(blob *providerv1.SignedBlob) *contracts.SignedAnswer {
	if len(blob.ProtoReflect().GetUnknown()) != 0 {
		return nil
	}
	return &contracts.SignedAnswer{
		BodyType:          blob.GetBodyType(),
		Body:              append([]byte(nil), blob.GetBody()...),
		Signature:         append([]byte(nil), blob.GetSignature()...),
		SignerCertificate: append([]byte(nil), blob.GetSignerCertificate()...),
	}
}
