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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestPinnedToTheAgentAnswer: the Provider's copy of the host answer's
// signature input and its API version are the agent's.
func TestPinnedToTheAgentAnswer(t *testing.T) {
	body := []byte{1, 2, 3}
	want, err := answer.SignatureInput(answer.BodyTypeHostAnswer, body)
	require.NoError(t, err)
	assert.Equal(t, want, signatureInput(bodyTypeHostAnswer, body))
	assert.Equal(t, answer.BodyTypeHostAnswer, bodyTypeHostAnswer)
	assert.Equal(t, answer.DomainSeparator, hostSignedDomain)
	assert.Equal(t, answer.NonceLen, nonceLen)
	assert.Equal(t, answer.MaxBodyBytes, maxAnswerBody)
	assert.Equal(t, answer.AgentAPIVersion, APIVersion, "the Provider speaks the agent's current API version")
}

// TestAgentCertificateRefused: an agent whose key is not pinned, whose
// certificate comes from another CA, names another host or another Provider
// is refused in the handshake: the host is unavailable, and the agent never
// receives a call, not even Hello.
func TestAgentCertificateRefused(t *testing.T) {
	p := newPKI(t)
	cases := map[string]func(t *testing.T) (*fakeAgent, hostsecret.Host){
		"pin mismatch": func(t *testing.T) (*fakeAgent, hostsecret.Host) {
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
			e := a.entry()
			e.Credentials.AgentTLS.KeyPins = []string{"0000000000000000000000000000000000000000000000000000000000000000"}
			return a, e
		},
		"unpinned leaf": func(t *testing.T) (*fakeAgent, hostsecret.Host) {
			// The entry pins the enrolled key; the agent presents another
			// certificate, valid in every other way, for a key nobody pinned.
			pinned := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
			key := hostidentitytest.NewKey(t)
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{key: key, leaf: hostCert(t, p.ca, "host-a", testProviderUID, key)})
			e := a.entry()
			e.Credentials.AgentTLS.KeyPins = pinned.entry().Credentials.AgentTLS.KeyPins
			return a, e
		},
		"foreign CA": func(t *testing.T) (*fakeAgent, hostsecret.Host) {
			key := hostidentitytest.NewKey(t)
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{key: key, leaf: hostCert(t, hostidentitytest.NewCA(t), "host-a", testProviderUID, key)})
			e := a.entry()
			e.Credentials.AgentTLS.CABundle = p.ca.PEM
			return a, e
		},
		"wrong SAN": func(t *testing.T) (*fakeAgent, hostsecret.Host) {
			key := hostidentitytest.NewKey(t)
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{key: key, leaf: hostCert(t, p.ca, "host-b", testProviderUID, key)})
			return a, a.entry()
		},
		"another Provider": func(t *testing.T) (*fakeAgent, hostsecret.Host) {
			key := hostidentitytest.NewKey(t)
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{key: key, leaf: hostCert(t, p.ca, "host-a", "another-provider-uid", key)})
			return a, a.entry()
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			a, e := mk(t)
			prov, err := New(Options{ClientCertificate: p.client, ProviderUID: testProviderUID, Logger: quiet()},
				hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{e}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = prov.Close() })
			client := providerv1.NewProviderClient(served(t, prov))
			_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{Id: "vm-1", TargetHostId: "host-a"})
			requireHostUnavailable(t, err, CauseAgentCertificateRefused)
			assert.Zero(t, a.hellos.Load(), "no Hello reaches a refused agent")
			assert.Empty(t, a.received(), "no call reaches a refused agent")
		})
	}

	t.Run("accepted", func(t *testing.T) {
		a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
		prov := newProvider(t, p, Options{ProviderUID: testProviderUID}, a)
		client := providerv1.NewProviderClient(served(t, prov))
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{Id: "vm-1", TargetHostId: "host-a"})
		require.NoError(t, err)
		assert.Len(t, a.received(), 1)
	})
}

// TestUnsignedAndOuterFieldNeverDialed: a mutating call without an envelope
// is refused IDENTITY_REQUIRED, and one with an outer field beside it
// INVALID_ARGUMENT, before any host is dialed.
func TestUnsignedAndOuterFieldNeverDialed(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))

	_, err := client.Power(ctx(t), &providerv1.PowerRequest{Id: "vm-1", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-a"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
	_, ok := errorInfo(err, contracts.IdentityRequiredReason)
	require.True(t, ok, "IDENTITY_REQUIRED: %v", err)
	_, err = client.Delete(ctx(t), &providerv1.DeleteRequest{Id: "vm-1", TargetHostId: "host-a"})
	_, ok = errorInfo(err, contracts.IdentityRequiredReason)
	require.True(t, ok, "IDENTITY_REQUIRED: %v", err)

	for name, set := range map[string]func(*providerv1.PowerRequest){
		"target_host_id": func(r *providerv1.PowerRequest) { r.TargetHostId = "host-a" },
		"id":             func(r *providerv1.PowerRequest) { r.Id = "vm-1" },
		"unknown field": func(r *providerv1.PowerRequest) {
			r.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 900, protowire.VarintType), 1))
		},
	} {
		req := signedPower(t, "host-a")
		set(req)
		_, err := client.Power(ctx(t), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
	}
	assert.Zero(t, a.lis.accepted.Load(), "nothing was dialed")
	assert.Empty(t, a.received())
}

// TestForwardedByteIdentical: the outer request reaches the agent byte for
// byte as the manager sent it, even in an encoding a re-marshal would not
// reproduce, and a read's answer reaches the manager byte for byte as the
// agent sent it.
func TestForwardedByteIdentical(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cc := served(t, newProvider(t, p, Options{}, a))

	inner, err := proto.Marshal(&providerv1.PowerRequest{Id: "vm-1", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-a"})
	require.NoError(t, err)
	grant, err := proto.Marshal(&providerv1.OperationGrant{Body: []byte("opaque"), Signature: []byte("sig")})
	require.NoError(t, err)
	// The grant before the request (a marshal writes fields in number
	// order), and the request's length as an over-long varint.
	envelope := protowire.AppendTag(nil, 2, protowire.BytesType)
	envelope = protowire.AppendBytes(envelope, grant)
	envelope = protowire.AppendTag(envelope, 1, protowire.BytesType)
	envelope = append(envelope, byte(len(inner))|0x80, 0x00)
	envelope = append(envelope, inner...)
	fn := (&providerv1.PowerRequest{}).ProtoReflect().Descriptor().Fields().ByName("operation_envelope").Number()
	outer := protowire.AppendTag(nil, fn, protowire.BytesType)
	outer = protowire.AppendBytes(outer, envelope)

	var canonical providerv1.PowerRequest
	require.NoError(t, proto.Unmarshal(outer, &canonical))
	again, err := proto.Marshal(&canonical)
	require.NoError(t, err)
	require.NotEqual(t, outer, again, "the test request is one a re-marshal changes")

	out := &emptypb.Empty{}
	require.NoError(t, cc.Invoke(ctx(t), providerv1.Provider_Power_FullMethodName, rawFrame(outer), out))
	got := a.received()
	require.Len(t, got, 1)
	assert.Equal(t, providerv1.Provider_Power_FullMethodName, got[0].method, "forwarded on the same method")
	assert.Equal(t, outer, got[0].raw, "forwarded byte for byte")

	// A read's answer is relayed byte for byte.
	answerBytes := append(protowire.AppendTag(nil, 1, protowire.VarintType), 0x81, 0x00) // exists = true, over-long varint
	a.setRespond(func(string, []byte) (proto.Message, error) { return rawFrame(answerBytes), nil })
	req, err := proto.Marshal(&providerv1.DescribeRequest{Id: "vm-1", TargetHostId: "host-a"})
	require.NoError(t, err)
	out = &emptypb.Empty{}
	require.NoError(t, cc.Invoke(ctx(t), providerv1.Provider_Describe_FullMethodName, rawFrame(req), out))
	assert.Equal(t, answerBytes, frameBytes(out), "the agent's answer is relayed byte for byte")
	got = a.received()
	require.Len(t, got, 2)
	assert.Equal(t, req, got[1].raw)
}

// TestTaskReferences: a mutating call's task reference is host-encoded, and
// a TaskStatus that names it reaches the host with the agent's own
// reference; one that asks for a signed answer is routed by its host and
// forwarded unchanged.
func TestTaskReferences(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a, b)))

	resp, err := client.Power(ctx(t), signedPower(t, "host-b"))
	require.NoError(t, err)
	assert.Equal(t, clustered.EncodeHostTaskRef("host-b", "task-1"), resp.GetTask().GetId())
	assert.Empty(t, a.received(), "routed by the request inside the envelope")

	_, err = client.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: resp.GetTask()})
	require.NoError(t, err)
	got := b.received()
	require.Len(t, got, 2)
	var st providerv1.TaskStatusRequest
	require.NoError(t, proto.Unmarshal(got[1].raw, &st))
	assert.Equal(t, "task-1", st.GetTask().GetId())

	nonce := make([]byte, nonceLen)
	signed := &providerv1.TaskStatusRequest{AnswerNonce: nonce, OpId: make([]byte, 16), OpEnrolmentId: make([]byte, 16), TargetHostId: "host-a"}
	_, err = client.TaskStatus(ctx(t), signed)
	require.NoError(t, err)
	got = a.received()
	require.Len(t, got, 1)
	want, err := proto.Marshal(signed)
	require.NoError(t, err)
	assert.Equal(t, want, got[0].raw)

	_, err = client.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: "not-host-encoded"}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = client.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: clustered.EncodeHostTaskRef("host-zzz", "t")}})
	requireHostUnavailable(t, err, CauseNotFronted)
}

// TestHelloVersionWindow: an agent of the Provider's API version or the one
// before it is routed; any other is refused AgentIncompatible and receives
// no call. Hello is made once per connection.
func TestHelloVersionWindow(t *testing.T) {
	p := newPKI(t)
	for _, tc := range []struct {
		name     string
		provider uint32
		routed   bool
	}{
		{"same version", answer.AgentAPIVersion, true},
		{"agent one version behind", answer.AgentAPIVersion + 1, true},
		{"agent two versions behind", answer.AgentAPIVersion + 2, false},
		{"agent newer", answer.AgentAPIVersion - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
			client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{APIVersion: tc.provider}, a)))
			for i := 0; i < 3; i++ {
				_, err := client.Power(ctx(t), signedPower(t, "host-a"))
				if tc.routed {
					require.NoError(t, err)
				} else {
					requireHostUnavailable(t, err, CauseAgentIncompatible)
				}
			}
			assert.EqualValues(t, 1, a.hellos.Load(), "one Hello per connection")
			if tc.routed {
				assert.Len(t, a.received(), 3)
			} else {
				assert.Empty(t, a.received(), "an incompatible agent receives no call")
			}
		})
	}

	t.Run("new connection, new Hello", func(t *testing.T) {
		a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
		client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))
		_, err := client.Power(ctx(t), signedPower(t, "host-a"))
		require.NoError(t, err)
		// The agent restarts as a newer, unsupported release.
		a.apiVersion.Store(answer.AgentAPIVersion + 1)
		a.lis.dropAll()
		require.Eventually(t, func() bool {
			_, err := client.Power(ctx(t), signedPower(t, "host-a"))
			info, ok := errorInfo(err, contracts.HostUnavailableReason)
			return ok && info.GetMetadata()[MetadataCause] == CauseAgentIncompatible
		}, testTimeout, 10*time.Millisecond, "the new connection's Hello refuses the agent")
		assert.GreaterOrEqual(t, a.hellos.Load(), int32(2))
	})
}

// TestOneHostDownOthersUnaffected: a host whose agent is down is
// unavailable, host-scoped, and its breaker opens without touching another
// host's; answers with an agent-contract reason never open a breaker.
func TestOneHostDownOthersUnaffected(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	prov := newProvider(t, p, Options{Breaker: &resilience.Config{FailureThreshold: 2, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}}, a, b)
	client := providerv1.NewProviderClient(served(t, prov))

	b.srv.Stop()
	for i := 0; i < 2; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
		requireHostUnavailable(t, err, CauseAgentUnreachable)
	}
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseBreakerOpen)
	for i := 0; i < 5; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err, "the other host is unaffected")
	}
	hosts, err := client.ListHosts(ctx(t), &providerv1.ListHostsRequest{})
	require.NoError(t, err)
	health := map[string]providerv1.HostHealth{}
	for _, h := range hosts.GetHosts() {
		health[h.GetId()] = h.GetHealth()
	}
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_READY, health["host-a"])
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_NOT_READY, health["host-b"])

	// The agent's own Unavailable with an agent-contract reason is exempt.
	unattested := func(string, []byte) (proto.Message, error) {
		return nil, refusalErr(codes.Unavailable, contracts.UnattestedReason)
	}
	a.setRespond(unattested)
	for i := 0; i < 5; i++ {
		_, err := client.Power(ctx(t), signedPower(t, "host-a"))
		_, ok := errorInfo(err, contracts.UnattestedReason)
		require.True(t, ok, "the agent's answer is relayed unchanged: %v", err)
	}
	// A definite not-performed answer is exempt too.
	a.setRespond(func(string, []byte) (proto.Message, error) {
		return nil, refusalErr(codes.FailedPrecondition, contracts.GrantMismatchReason)
	})
	_, err = client.Power(ctx(t), signedPower(t, "host-a"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	e, _ := prov.pool.get("host-a")
	assert.Equal(t, resilience.StateClosed, e.breaker.GetState(), "agent answers never open the breaker")
}

// TestConnectionLostMidCall: a mutating call that was sent and got no answer
// may have been performed (OUTCOME_UNKNOWN_RETRY); a read is host-scoped.
func TestConnectionLostMidCall(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	a.setRespond(func(string, []byte) (proto.Message, error) {
		a.lis.dropAll()
		return &emptypb.Empty{}, nil
	})
	_, err = client.Power(ctx(t), signedPower(t, "host-a"))
	require.Error(t, err)
	_, ok := errorInfo(err, contracts.OutcomeUnknownRetryReason)
	assert.True(t, ok, "a sent mutation without an answer has an unknown outcome: %v", err)
	_, ok = errorInfo(err, contracts.HostUnavailableReason)
	assert.False(t, ok, "never 'not performed': %v", err)

	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	requireHostUnavailable(t, err, CauseAgentUnreachable)
}

// TestReloadClosesRemovedHosts: a reload that removes a host, or changes its
// pins, closes its connection; another host keeps its connection.
func TestReloadClosesRemovedHosts(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	prov := newProvider(t, p, Options{}, a, b)
	client := providerv1.NewProviderClient(served(t, prov))
	for _, h := range []string{"host-a", "host-b"} {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: h})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, b.lis.open.Load())

	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{a.entry()}}))
	require.Eventually(t, func() bool { return b.lis.open.Load() == 0 }, testTimeout, 10*time.Millisecond,
		"the removed host's connection is closed")
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseNotFronted)
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, a.lis.accepted.Load(), "the remaining host kept its connection")

	// A change of a host's pins replaces its connection.
	changed := a.entry()
	changed.Credentials.AgentTLS.KeyPins = append(changed.Credentials.AgentTLS.KeyPins, "11"+changed.Credentials.AgentTLS.KeyPins[0][2:])
	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{changed}}))
	require.Eventually(t, func() bool { return a.lis.open.Load() == 0 }, testTimeout, 10*time.Millisecond)
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, a.lis.accepted.Load())

	// A label change keeps it.
	relabelled := changed
	relabelled.Labels = map[string]string{"zone": "b"}
	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{relabelled}}))
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, a.lis.accepted.Load())
	hosts, err := client.ListHosts(ctx(t), &providerv1.ListHostsRequest{})
	require.NoError(t, err)
	require.Len(t, hosts.GetHosts(), 1)
	assert.Equal(t, "b", hosts.GetHosts()[0].GetLabels()["zone"])
}

// TestInventoryEntriesRefused: an entry without agent TLS material, with a
// duplicated id or an SSH endpoint is not routed.
func TestInventoryEntriesRefused(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	ssh := hostsecret.Host{ID: "host-ssh", Endpoint: "qemu+ssh://virt@host-ssh/system", Credentials: hostsecret.Credentials{SSHPrivateKey: []byte("k")}}
	prov, err := New(Options{ClientCertificate: p.client, Logger: quiet()}, hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion,
		Hosts: []hostsecret.Host{a.entry(), ssh, a.entry()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	assert.Empty(t, prov.pool.ids(), "a duplicated id is routed to neither entry")
	assert.Equal(t, []string{"host-a", "host-ssh"}, prov.pool.unroutable)
}

// TestCapabilities: identity supported and required; nothing else claimed.
func TestCapabilities(t *testing.T) {
	p := newPKI(t)
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{})))
	caps, err := client.GetCapabilities(ctx(t), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOperationIdentity())
	assert.True(t, caps.GetRequiresOperationIdentity())
	assert.True(t, caps.GetSupportsClustering())
	assert.False(t, caps.GetSupportsSnapshots())
	_, err = client.SnapshotCreate(ctx(t), &providerv1.SnapshotCreateRequest{})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// refusalErr is a status with one ErrorInfo of reason in the contract's
// domain.
func refusalErr(code codes.Code, reason string) error {
	st, err := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain})
	if err != nil {
		return status.Error(code, "refused")
	}
	return st.Err()
}
