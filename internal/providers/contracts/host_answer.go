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

package contracts

import "context"

// SignedAnswer is a per-host agent's signed answer as the Provider relayed
// it: the fields of a provider.v1.SignedBlob. It is not verified: the
// manager verifies it against its own state before it means anything.
type SignedAnswer struct {
	// BodyType names the body's message type.
	BodyType string
	// Body is the serialised answer body.
	Body []byte
	// Signature is the host's signature over the domain-separated body.
	Signature []byte
	// SignerCertificate is the host certificate (DER).
	SignerCertificate []byte
}

// SignedRead is what a read that asked for a signed answer returns beside
// its plain answer: the nonce the client drew for that one request, and the
// signed answer the response (or its error status) carried. Answer is nil
// when the response carried none, or carried one that cannot be a host
// answer (unknown fields, more than one, over the size bound): that is "no
// answer", never a negative one. The nonce is held only by the caller of the
// one request, compared and discarded; it is never stored or re-used.
type SignedRead struct {
	// Nonce is the 16-byte nonce drawn from crypto/rand for this request.
	Nonce []byte
	// Answer is the relayed signed answer, or nil.
	Answer *SignedAnswer
}

// OperationAnswerQuery names one operation for a TaskStatus that asks for a
// signed answer: by op_id, never by a task reference.
type OperationAnswerQuery struct {
	// OpID is the operation's 16-byte op_id.
	OpID []byte
	// OpEnrolmentID is the 16-byte enrolment its grants named.
	OpEnrolmentID []byte
	// HostID is the host the operation was sent to.
	HostID string
	// ContinuitySeq is the store sequence number at which the manager holds a
	// verified chain head (0: none).
	ContinuitySeq uint64
}

// VMAnswerQuery names one VM placement for a Describe that asks for a signed
// answer.
type VMAnswerQuery struct {
	// VM is the VM: its id, host and owner (the owner's UID is the VM UID the
	// answer is about).
	VM VMRef
	// BindingID is the placement's 16-byte binding identifier.
	BindingID []byte
	// ContinuitySeq is as in OperationAnswerQuery.
	ContinuitySeq uint64
}

// HostAnswerReader is an optional capability of a Provider client: reads that
// ask the host's per-host agent for a signed answer (development builds; a
// Provider whose type requires operation identity). Each call draws a fresh
// nonce; a crypto/rand failure fails the call before anything is sent. The
// plain answer beside the signed one is the Provider's unsigned claim, which
// never decides an outcome for such a Provider.
type HostAnswerReader interface {
	// OperationAnswer asks the host about one operation (TaskStatus by op_id).
	// On an error status the SignedRead still carries the status's signed
	// answer, if any.
	OperationAnswer(ctx context.Context, q OperationAnswerQuery) (TaskStatus, SignedRead, error)
	// VMAnswer asks the host about one VM placement (Describe).
	VMAnswer(ctx context.Context, q VMAnswerQuery) (DescribeResponse, SignedRead, error)
	// HostAnswer asks the host about itself (GetHostInfo), without an
	// inventory.
	HostAnswer(ctx context.Context, hostID string, continuitySeq uint64) (HostInfo, SignedRead, error)
}
