//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// APIVersion is the version of the agent.v1 service this Provider speaks. It
// accepts an agent of this version and of the version before it.
const APIVersion uint32 = 2

// The signed host answer, as a per-host agent signs it (internal/agent/answer;
// a test pins the two together). The Provider verifies only the answer to its
// own Hello, to learn the agent's API version from the host itself.
const (
	// hostSignedDomain starts the signature input of every host answer.
	hostSignedDomain = "virtrigaud.host-signed.v1\x00"
	// bodyTypeHostAnswer is the body type of an answer about the host.
	bodyTypeHostAnswer = "provider.v1.HostAnswer"
	// maxAnswerBody bounds the body of a host answer.
	maxAnswerBody = 64 << 10
	// nonceLen is the length of an answer nonce.
	nonceLen = 16
)

// compatible reports whether an agent of API version agent may be routed to
// by a Provider of version provider: the same version, or the one before it.
func compatible(agent, provider uint32) bool {
	return agent == provider || (provider > 1 && agent == provider-1)
}

// signatureInput is the exact bytes a host signs for body of bodyType.
func signatureInput(bodyType string, body []byte) []byte {
	m := make([]byte, 0, len(hostSignedDomain)+2+len(bodyType)+4+len(body))
	m = append(m, hostSignedDomain...)
	m = binary.BigEndian.AppendUint16(m, uint16(len(bodyType))) // #nosec G115 -- the body type is the constant bodyTypeHostAnswer
	m = append(m, bodyType...)
	m = binary.BigEndian.AppendUint32(m, uint32(len(body))) // #nosec G115 -- bounded by maxAnswerBody
	return append(m, body...)
}

// verifyHostAnswer verifies the answer to a Hello sent with nonce to host
// over a connection whose agent presented leaf: a HostAnswer, signed by the
// key of leaf itself (the certificate the TLS handshake verified), that
// echoes nonce and names host.
func verifyHostAnswer(blob *providerv1.SignedBlob, leaf *x509.Certificate, nonce []byte, host string) (*providerv1.HostAnswer, error) {
	switch {
	case blob == nil:
		return nil, errors.New("the agent gave no signed answer")
	case blob.GetBodyType() != bodyTypeHostAnswer:
		return nil, errors.New("the signed answer is not a host answer")
	case len(blob.GetBody()) > maxAnswerBody:
		return nil, errors.New("the signed answer is too large")
	case !bytes.Equal(blob.GetSignerCertificate(), leaf.Raw):
		return nil, errors.New("the answer is not signed with the connection's certificate")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	digest := sha256.Sum256(signatureInput(blob.GetBodyType(), blob.GetBody()))
	if !ok || !ecdsa.VerifyASN1(pub, digest[:], blob.GetSignature()) {
		return nil, errors.New("the answer's signature does not verify")
	}
	ha := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(blob.GetBody(), ha); err != nil {
		return nil, errors.New("the host answer does not decode")
	}
	if !bytes.Equal(ha.GetContext().GetNonce(), nonce) {
		return nil, errors.New("the host answer does not echo the Hello's nonce")
	}
	if ha.GetContext().GetHostId() != host {
		return nil, errors.New("the host answer names another host")
	}
	return ha, nil
}

// helloed returns nil when the host's agent passed its Hello on the current
// connection, making the Hello first when none was made on it. An
// incompatible agent stays refused until the connection is replaced; a
// Hello that could not be made or verified is tried again on the next call.
func (p *pool) helloed(ctx context.Context, e *hostEntry) error {
	if made, err := e.currentHello(); made {
		return err
	}
	e.helloMu.Lock()
	defer e.helloMu.Unlock()
	if made, err := e.currentHello(); made {
		return err
	}
	e.mu.Lock()
	epoch := e.epoch
	e.mu.Unlock()
	version, err := p.hello(ctx, e)
	if err != nil && !errors.Is(err, errIncompatible) {
		return err
	}
	outcome := error(nil)
	if err != nil {
		outcome = hostUnavailable(e.id, true, CauseAgentIncompatible)
	}
	e.mu.Lock()
	e.hello, e.helloEpoch, e.helloDone, e.apiVersion = outcome, epoch, true, version
	e.mu.Unlock()
	return outcome
}

// errIncompatible is returned by hello for an agent outside the version
// window.
var errIncompatible = errors.New("the agent's API version is not supported")

// hello calls Hello on e's connection and returns the agent's API version.
// It returns errIncompatible for an agent that is too old or too new
// (including one that does not serve Hello), and a host-scoped refusal when
// the Hello could not be made or its answer not verified.
func (p *pool) hello(ctx context.Context, e *hostEntry) (uint32, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return 0, status.Error(codes.Internal, "draw a nonce")
	}
	var pr peer.Peer
	resp := &agentv1.HelloResponse{}
	cs, err := p.invoke(ctx, e, agentv1.AgentService_Hello_FullMethodName,
		&agentv1.HelloRequest{AnswerNonce: nonce, CallerApiVersion: p.opts.APIVersion}, resp, grpc.Peer(&pr))
	if err != nil {
		if cs.answered.Load() && status.Code(err) == codes.Unimplemented {
			p.log.Warn("agent does not serve Hello; not routed", "host", e.id)
			return 0, errIncompatible
		}
		if cs.answered.Load() {
			p.log.Warn("agent refused the Hello; not routed", "host", e.id, "code", status.Code(err).String())
			return 0, hostUnavailable(e.id, true, CauseAgentUnverified)
		}
		return 0, mapCallError(ctx, e.id, false, cs, err)
	}
	info, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return 0, hostUnavailable(e.id, true, CauseAgentUnverified)
	}
	ha, err := verifyHostAnswer(resp.GetHostAnswer(), info.State.PeerCertificates[0], nonce, e.id)
	if err != nil {
		p.log.Warn("agent's Hello answer not verified; not routed", "host", e.id, "error", err.Error())
		return 0, hostUnavailable(e.id, true, CauseAgentUnverified)
	}
	v := ha.GetAgentApiVersion()
	if !compatible(v, p.opts.APIVersion) {
		p.log.Warn("agent API version outside the supported window; not routed", "host", e.id,
			"agentAPIVersion", v, "providerAPIVersion", p.opts.APIVersion)
		return v, fmt.Errorf("%w: agent %d, Provider %d", errIncompatible, v, p.opts.APIVersion)
	}
	return v, nil
}
