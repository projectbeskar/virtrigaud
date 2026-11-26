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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// authorityMethods are the host authority methods, each with a request for
// host.
func authorityMethods(host string) map[string]proto.Message {
	grant := &providerv1.OperationGrant{Body: []byte("opaque"), Signature: []byte("sig"), SignerCertChain: [][]byte{[]byte("leaf")}}
	nonce := make([]byte, nonceLen)
	return map[string]proto.Message{
		providerv1.HostAuthority_Challenge_FullMethodName: &providerv1.ChallengeRequest{TargetHostId: host, AnswerNonce: nonce,
			Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST},
		providerv1.HostAuthority_Attest_FullMethodName:          &providerv1.AttestRequest{TargetHostId: host, AnswerNonce: nonce, Grant: grant},
		providerv1.HostAuthority_LedgerMark_FullMethodName:      &providerv1.LedgerMarkRequest{TargetHostId: host, AnswerNonce: nonce, Grant: grant},
		providerv1.HostAuthority_RetireIfUnknown_FullMethodName: &providerv1.RetireIfUnknownRequest{TargetHostId: host, AnswerNonce: nonce, Grant: grant},
	}
}

// newAuthorityAgent is a fake agent of the first API version that serves
// the host authority service.
func newAuthorityAgent(t *testing.T, p *pki, name string) *fakeAgent {
	t.Helper()
	a := newFakeAgent(t, p, name, fakeAgentOptions{})
	a.apiVersion.Store(HostAuthorityAgentAPIVersion)
	return a
}

// invokeRaw sends the exact bytes req on method and returns the exact bytes
// of the answer.
func invokeRaw(t *testing.T, cc *grpc.ClientConn, method string, req []byte) ([]byte, error) {
	t.Helper()
	out := &emptypb.Empty{}
	if err := cc.Invoke(ctx(t), method, rawFrame(req), out); err != nil {
		return nil, err
	}
	return frameBytes(out), nil
}

// nonCanonical is the encoding of m with its fields in reverse order and an
// unknown field appended: bytes a decode and re-encode would change.
func nonCanonical(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	var fields [][]byte
	for len(b) > 0 {
		_, _, n := protowire.ConsumeField(b)
		require.Positive(t, n)
		fields = append(fields, b[:n])
		b = b[n:]
	}
	var out []byte
	for i := len(fields) - 1; i >= 0; i-- {
		out = append(out, fields[i]...)
	}
	out = protowire.AppendTag(out, 999, protowire.BytesType)
	return protowire.AppendBytes(out, []byte("unknown to this build"))
}

// TestHostAuthority_ServiceDescRaw: every method of the host authority
// service is routed and reads its request as raw bytes; Register serves both
// services.
func TestHostAuthority_ServiceDescRaw(t *testing.T) {
	d := AuthorityServiceDesc()
	assert.Equal(t, providerv1.HostAuthority_ServiceDesc.ServiceName, d.ServiceName)
	require.Len(t, d.Methods, len(authorityRoutes))
	for _, m := range d.Methods {
		r, ok := authorityRoutes["/"+d.ServiceName+"/"+m.MethodName]
		require.True(t, ok, "%s routed", m.MethodName)
		assert.True(t, r.authority)
		assert.False(t, r.mutating, "no operation envelope")
	}
	assert.Len(t, authorityRoutes, len(authorityMethods("h")))
}

// TestHostAuthority_RelayedByteForByte: each method's request reaches the
// addressed host's agent byte for byte, a non-canonical encoding with an
// unknown field included, and the agent's answer frame comes back byte for
// byte; the other host receives nothing.
func TestHostAuthority_RelayedByteForByte(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	b := newAuthorityAgent(t, p, "host-b")
	answer := nonCanonical(t, &providerv1.AuthorityResponse{HostAnswer: &providerv1.SignedBlob{Body: []byte{1, 2, 3},
		Signature: []byte{4}, SignerCertificate: []byte{5}, BodyType: "provider.v1.AuthorityAnswer"}})
	b.setRespond(func(string, []byte) (proto.Message, error) { return rawFrame(answer), nil })
	cc := served(t, newProvider(t, p, Options{}, a, b))

	var sent [][]byte
	for method, req := range authorityMethods("host-b") {
		raw := nonCanonical(t, req)
		sent = append(sent, raw)
		got, err := invokeRaw(t, cc, method, raw)
		require.NoError(t, err, method)
		assert.Equal(t, answer, got, "%s: the answer is relayed byte for byte", method)
	}
	recv := b.received()
	require.Len(t, recv, len(sent))
	for i := range sent {
		assert.Equal(t, sent[i], recv[i].raw, "request %d (%s) forwarded byte for byte", i, recv[i].method)
	}
	assert.Empty(t, a.received(), "only the addressed host is called")
}

// TestHostAuthority_TypedClient: the generated client gets the agent's
// signed answer unchanged.
func TestHostAuthority_TypedClient(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	blob := &providerv1.SignedBlob{Body: []byte{9}, Signature: []byte{8}, SignerCertificate: []byte{7}, BodyType: "provider.v1.ChallengeAnswer"}
	a.setRespond(func(method string, _ []byte) (proto.Message, error) {
		if method == providerv1.HostAuthority_Challenge_FullMethodName {
			return &providerv1.ChallengeResponse{HostAnswer: blob}, nil
		}
		return &providerv1.AuthorityResponse{HostAnswer: blob}, nil
	})
	ha := providerv1.NewHostAuthorityClient(served(t, newProvider(t, p, Options{}, a)))
	reqs := authorityMethods("host-a")

	c, err := ha.Challenge(ctx(t), reqs[providerv1.HostAuthority_Challenge_FullMethodName].(*providerv1.ChallengeRequest))
	require.NoError(t, err)
	assert.True(t, proto.Equal(blob, c.GetHostAnswer()))
	r, err := ha.Attest(ctx(t), reqs[providerv1.HostAuthority_Attest_FullMethodName].(*providerv1.AttestRequest))
	require.NoError(t, err)
	assert.True(t, proto.Equal(blob, r.GetHostAnswer()))
	r, err = ha.LedgerMark(ctx(t), reqs[providerv1.HostAuthority_LedgerMark_FullMethodName].(*providerv1.LedgerMarkRequest))
	require.NoError(t, err)
	assert.True(t, proto.Equal(blob, r.GetHostAnswer()))
	r, err = ha.RetireIfUnknown(ctx(t), reqs[providerv1.HostAuthority_RetireIfUnknown_FullMethodName].(*providerv1.RetireIfUnknownRequest))
	require.NoError(t, err)
	assert.True(t, proto.Equal(blob, r.GetHostAnswer()))

	// The Provider's own methods, called directly, behave the same.
	prov := newProvider(t, p, Options{}, a)
	r, err = prov.Attest(ctx(t), reqs[providerv1.HostAuthority_Attest_FullMethodName].(*providerv1.AttestRequest))
	require.NoError(t, err)
	assert.True(t, proto.Equal(blob, r.GetHostAnswer()))
}

// TestHostAuthority_WrongHost: a request without a host, one that does not
// decode, and one for a host the Provider does not front are refused without
// dialing; an unknown host id is never echoed.
func TestHostAuthority_WrongHost(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	cc := served(t, newProvider(t, p, Options{}, a))

	for method, req := range authorityMethods("host-unknown") {
		raw, err := proto.Marshal(req)
		require.NoError(t, err)
		_, err = invokeRaw(t, cc, method, raw)
		requireHostUnavailable(t, err, CauseNotFronted)
		info, _ := errorInfo(err, contracts.HostUnavailableReason)
		assert.NotContains(t, info.GetMetadata(), contracts.HostUnavailableHostKey, method)
		assert.NotContains(t, status.Convert(err).Message(), "host-unknown", method)
	}
	for method, req := range authorityMethods("  ") {
		raw, err := proto.Marshal(req)
		require.NoError(t, err)
		_, err = invokeRaw(t, cc, method, raw)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s without a host: %v", method, err)

		// target_host_id that is not UTF-8: well-formed bytes that do not
		// decode as the method's request.
		bad := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte{0xff, 0xfe})
		_, err = invokeRaw(t, cc, method, bad)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s that does not decode: %v", method, err)

		// Malformed wire bytes are refused by gRPC before the Provider.
		_, err = invokeRaw(t, cc, method, []byte{0xff, 0xff, 0xff})
		assert.Error(t, err, method)
	}
	assert.Empty(t, a.received(), "nothing reaches an agent")
}

// TestHostAuthority_OldAgent: an agent of the API version before the host
// authority service is still routed for the Provider service, but its host
// authority calls are answered Unimplemented by the Provider, naming the
// host, without reaching the agent or touching its breaker.
func TestHostAuthority_OldAgent(t *testing.T) {
	p := newPKI(t)
	old := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	old.apiVersion.Store(HostAuthorityAgentAPIVersion - 1)
	prov := newProvider(t, p, Options{Breaker: &resilience.Config{FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}}, old)
	cc := served(t, prov)

	_, err := providerv1.NewProviderClient(cc).Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err, "the older agent is routed for the Provider service")
	before := len(old.received())
	for method, req := range authorityMethods("host-a") {
		raw, err := proto.Marshal(req)
		require.NoError(t, err)
		_, err = invokeRaw(t, cc, method, raw)
		require.Equal(t, codes.Unimplemented, status.Code(err), "%s: %v", method, err)
		info, ok := errorInfo(err, contracts.HostAuthorityUnsupportedReason)
		require.True(t, ok, "%s: HOST_AUTHORITY_UNSUPPORTED: %v", method, err)
		assert.Equal(t, contracts.ErrorInfoDomain, info.GetDomain())
		assert.Equal(t, "host-a", info.GetMetadata()[contracts.HostUnavailableHostKey])
		assert.Equal(t, strconv.Itoa(int(HostAuthorityAgentAPIVersion-1)), info.GetMetadata()[metadataAgentAPIVersion])
	}
	assert.Len(t, old.received(), before, "the older agent receives no host authority call")
	e, _ := prov.pool.get("host-a")
	assert.Equal(t, resilience.StateClosed, e.breaker.GetState())
	assert.EqualValues(t, 1, old.hellos.Load(), "no new Hello")
}

// TestHostAuthority_AgentAnswersRelayed: an agent's own answers are relayed
// unchanged, an Unimplemented from an agent that does not serve the service
// included, unless the manager would count them toward its Provider-wide
// breaker: those become OUTCOME_UNKNOWN_RETRY. None opens the host's breaker
// except an agent that answers Unavailable without a reason.
func TestHostAuthority_AgentAnswersRelayed(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	prov := newProvider(t, p, Options{}, a)
	cc := served(t, prov)
	raw, err := proto.Marshal(authorityMethods("host-a")[providerv1.HostAuthority_LedgerMark_FullMethodName])
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		answer    error
		code      codes.Code
		reason    string
		rewritten bool
	}{
		{"not served by the agent", status.Error(codes.Unimplemented, "unknown service provider.v1.HostAuthority"), codes.Unimplemented, "", false},
		{"agent-contract reason", refusalErr(codes.Unavailable, contracts.UnattestedReason), codes.Unavailable, contracts.UnattestedReason, false},
		{"business refusal", refusalErr(codes.FailedPrecondition, contracts.GrantMismatchReason), codes.FailedPrecondition, contracts.GrantMismatchReason, false},
		{"rate limited", status.Error(codes.ResourceExhausted, "slow down"), codes.ResourceExhausted, "", false},
		{"internal", status.Error(codes.Internal, "failed"), codes.Internal, "", false},
		{"bare unavailable", status.Error(codes.Unavailable, "signer away"), codes.Unavailable, contracts.OutcomeUnknownRetryReason, true},
		{"bare deadline", status.Error(codes.DeadlineExceeded, "too slow"), codes.Unavailable, contracts.OutcomeUnknownRetryReason, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a.setRespond(func(string, []byte) (proto.Message, error) { return nil, tc.answer })
			_, err := invokeRaw(t, cc, providerv1.HostAuthority_LedgerMark_FullMethodName, raw)
			require.Equal(t, tc.code, status.Code(err), "%v", err)
			if tc.reason != "" {
				_, ok := errorInfo(err, tc.reason)
				assert.True(t, ok, "%s: %v", tc.reason, err)
			}
			if tc.rewritten {
				_, ok := errorInfo(err, contracts.HostUnavailableReason)
				assert.False(t, ok, "never 'not performed': %v", err)
				assert.Contains(t, status.Convert(err).Message(), "host-a")
			} else {
				assert.Equal(t, status.Convert(tc.answer).Message(), status.Convert(err).Message(), "relayed unchanged")
			}
		})
	}
}

// TestHostAuthority_UnreachableHostIsolated: an unreachable host is
// host-scoped and opens only its own breaker; another host keeps being
// served, on both services.
func TestHostAuthority_UnreachableHostIsolated(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	b := newAuthorityAgent(t, p, "host-b")
	prov := newProvider(t, p, Options{ConnectTimeout: 300 * time.Millisecond,
		Breaker: &resilience.Config{FailureThreshold: 2, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}}, a, b)
	cc := served(t, prov)
	attest := func(host string) error {
		raw, err := proto.Marshal(authorityMethods(host)[providerv1.HostAuthority_Attest_FullMethodName])
		require.NoError(t, err)
		_, err = invokeRaw(t, cc, providerv1.HostAuthority_Attest_FullMethodName, raw)
		return err
	}

	b.srv.Stop()
	for i := 0; i < 2; i++ {
		requireHostUnavailable(t, attest("host-b"), CauseAgentUnreachable)
	}
	err := attest("host-b")
	requireHostUnavailable(t, err, CauseBreakerOpen)
	info, _ := errorInfo(err, contracts.HostUnavailableReason)
	assert.Equal(t, "host-b", info.GetMetadata()[contracts.HostUnavailableHostKey])
	for i := 0; i < 5; i++ {
		require.NoError(t, attest("host-a"), "the other host is unaffected")
		_, err := providerv1.NewProviderClient(cc).Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	ea, _ := prov.pool.get("host-a")
	assert.Equal(t, resilience.StateClosed, ea.breaker.GetState())
}

// TestHostAuthority_ConnectionLostMidCall: a host authority call that was
// sent and got no answer may have changed the agent's state: it is
// OUTCOME_UNKNOWN_RETRY, never HOST_UNAVAILABLE.
func TestHostAuthority_ConnectionLostMidCall(t *testing.T) {
	p := newPKI(t)
	a := newAuthorityAgent(t, p, "host-a")
	cc := served(t, newProvider(t, p, Options{}, a))
	_, err := providerv1.NewProviderClient(cc).Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	a.setRespond(func(string, []byte) (proto.Message, error) {
		a.lis.dropAll()
		return &emptypb.Empty{}, nil
	})
	for method, req := range authorityMethods("host-a") {
		raw, err := proto.Marshal(req)
		require.NoError(t, err)
		_, err = invokeRaw(t, cc, method, raw)
		require.Error(t, err, method)
		_, ok := errorInfo(err, contracts.OutcomeUnknownRetryReason)
		assert.True(t, ok, "%s: a sent call without an answer has an unknown outcome: %v", method, err)
		_, ok = errorInfo(err, contracts.HostUnavailableReason)
		assert.False(t, ok, "%s: never 'not performed': %v", method, err)
	}
}
