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
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Placements tracks, per host and VM, the placement binding and the highest
// attempt used per kind, for a Target that implements PlacementTracker. It is
// safe for concurrent use.
type Placements struct {
	mu       sync.Mutex
	bindings map[string][]byte
	attempts map[string]uint64
}

// NewPlacements returns an empty tracker.
func NewPlacements() *Placements {
	return &Placements{bindings: map[string][]byte{}, attempts: map[string]uint64{}}
}

// Binding returns the binding of vmUID on host, drawing one when it has none.
func (p *Placements) Binding(host, vmUID string) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bindingLocked(host, vmUID, false)
}

// Placed reports whether vmUID has a placement on host.
func (p *Placements) Placed(host, vmUID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.bindings[host+"/"+vmUID]
	return ok
}

// Forget drops the placement of vmUID on host (its Delete completed); the
// attempts are kept.
func (p *Placements) Forget(host, vmUID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.bindings, host+"/"+vmUID)
}

func (p *Placements) bindingLocked(host, vmUID string, fresh bool) []byte {
	k := host + "/" + vmUID
	if b, ok := p.bindings[k]; ok && !fresh {
		return append([]byte(nil), b...)
	}
	b, err := operation.NewRandomID()
	if err != nil {
		// crypto/rand does not fail on supported platforms; a zero binding
		// would only make a case fail visibly.
		b = make([]byte, 16)
	}
	p.bindings[k] = b
	return append([]byte(nil), b...)
}

// NextPlacement implements PlacementTracker: a Create draws a new binding,
// any other kind uses the VM's binding; the attempt is one above the highest
// one used for the VM and kind on the host.
func (p *Placements) NextPlacement(host, vmUID string, kind providerv1.OperationKind) ([]byte, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.bindingLocked(host, vmUID, kind == providerv1.OperationKind_OPERATION_KIND_CREATE)
	k := fmt.Sprintf("%s/%s/%d", host, vmUID, kind)
	p.attempts[k]++
	return b, p.attempts[k]
}

// HostAnswers returns the Target's host-answer controls. Only a case that
// needs FeatureHostAnswers calls it; the probe made sure the Target
// implements HostAnswerTarget.
func (e *Env) HostAnswers() HostAnswerTarget {
	e.T.Helper()
	h, ok := e.Target.(HostAnswerTarget)
	require.True(e.T, ok, "the target offers host answers, so it implements HostAnswerTarget")
	return h
}

// NewNonce draws a fresh 16-byte answer nonce.
func (e *Env) NewNonce() []byte {
	e.T.Helper()
	n := make([]byte, answer.NonceLen)
	_, err := rand.Read(n)
	require.NoError(e.T, err)
	return n
}

// SignedOpStatus is a TaskStatus answer to a request that carried a nonce.
type SignedOpStatus struct {
	// Resp is the response, nil when the call failed.
	Resp *providerv1.TaskStatusResponse
	// Err is the call's error.
	Err error
	// Blob is the signed answer, from the response or from the error's
	// status details; nil when there is none.
	Blob *providerv1.SignedBlob
	// Answer is the verified answer body; nil when there is no valid one.
	Answer *providerv1.OperationAnswer
	// Invalid says why Blob did not verify, "" when it did or there is none.
	Invalid string
}

// SignedTaskStatus asks op's host about op by op_id with nonce, and verifies
// the signed answer it carries (VerifyHostAnswer).
func (e *Env) SignedTaskStatus(op *SignedOp, nonce []byte) SignedOpStatus {
	e.T.Helper()
	ctx, cancel := e.Call()
	defer cancel()
	resp, err := e.raw.TaskStatus(ctx, &providerv1.TaskStatusRequest{AnswerNonce: nonce, OpId: op.Identity.OpID,
		OpEnrolmentId: op.Identity.EnrolmentID, TargetHostId: op.Identity.HostID})
	out := SignedOpStatus{Resp: resp, Err: err}
	if err == nil {
		out.Blob = resp.GetHostAnswer()
	} else {
		out.Blob = detailBlob(err)
	}
	if out.Blob != nil {
		a := &providerv1.OperationAnswer{}
		if verr := e.VerifyHostAnswer(out.Blob, answer.BodyTypeOperationAnswer, op.Identity.HostID, nonce, a); verr != nil {
			out.Invalid = verr.Error()
		} else {
			out.Answer = a
		}
	}
	return out
}

// SignedDescribe asks host about the placement binding of the VM uid with
// nonce, and verifies the signed answer (VerifyHostAnswer).
func (e *Env) SignedDescribe(host, uid string, binding, nonce []byte) (*providerv1.DescribeResponse, *providerv1.VMAnswer, error) {
	e.T.Helper()
	ctx, cancel := e.Call()
	defer cancel()
	resp, err := e.raw.Describe(ctx, &providerv1.DescribeRequest{Id: uid, TargetHostId: host,
		Owner: &providerv1.ObjectIdentity{Uid: uid, Namespace: "ns", Name: "vm"}, AnswerNonce: nonce, BindingId: binding})
	if err != nil {
		return nil, nil, err
	}
	a := &providerv1.VMAnswer{}
	if verr := e.VerifyHostAnswer(resp.GetHostAnswer(), answer.BodyTypeVMAnswer, host, nonce, a); verr != nil {
		return resp, nil, verr
	}
	return resp, a, nil
}

// detailBlob returns the SignedBlob status detail of err, if exactly one.
func detailBlob(err error) *providerv1.SignedBlob {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	var found *providerv1.SignedBlob
	for _, d := range st.Details() {
		if b, ok := d.(*providerv1.SignedBlob); ok {
			if found != nil {
				return nil
			}
			found = b
		}
	}
	return found
}

// errNoAnswer is returned by VerifyHostAnswer for a missing answer.
var errNoAnswer = errors.New("no signed answer")

// VerifyHostAnswer verifies blob as the manager would for a request it sent
// to host with nonce: the body type is the one expected; the signer
// certificate chains to the Target's host answer roots; the signature over
// the domain-separated body verifies; the body decodes as bodyType with no
// unknown field; its context echoes nonce and names host. It decodes the
// body into out.
func (e *Env) VerifyHostAnswer(blob *providerv1.SignedBlob, bodyType, host string, nonce []byte, out proto.Message) error {
	e.T.Helper()
	if blob == nil {
		return errNoAnswer
	}
	if blob.GetBodyType() != bodyType {
		return fmt.Errorf("body type %q, want %q", blob.GetBodyType(), bodyType)
	}
	cert, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		return fmt.Errorf("signer certificate: %w", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: e.HostAnswers().HostAnswerRoots(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("signer certificate does not chain to the host roots: %w", err)
	}
	in, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		return err
	}
	d := sha256.Sum256(in)
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(pub, d[:], blob.GetSignature()) {
		return errors.New("the signature does not verify")
	}
	if err := proto.Unmarshal(blob.GetBody(), out); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if len(out.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("the body carries unknown fields")
	}
	ac, ok := out.(interface {
		GetContext() *providerv1.AnswerContext
	})
	if !ok {
		return errors.New("the body has no answer context")
	}
	if !bytes.Equal(ac.GetContext().GetNonce(), nonce) {
		return errors.New("the answer does not echo this request's nonce")
	}
	if ac.GetContext().GetHostId() != host {
		return fmt.Errorf("the answer names host %q, not %q", ac.GetContext().GetHostId(), host)
	}
	return nil
}
