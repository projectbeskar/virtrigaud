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

package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fakeAgent is a fixed agent state.
type fakeAgent struct {
	mu     sync.Mutex
	view   opstore.HostView
	state  providerv1.AgentState
	reason string
}

func (f *fakeAgent) HostView(context.Context, uint64) (opstore.HostView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view, nil
}

func (f *fakeAgent) TrustState() (providerv1.AgentState, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.reason
}

func (f *fakeAgent) ClockFloor() time.Time { return time.Unix(1700000000, 0) }

func (f *fakeAgent) set(state providerv1.AgentState, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.reason = state, reason
}

type env struct {
	t     *testing.T
	pki   *trusttest.PKI
	creds *server.CredentialStore
	agent *fakeAgent
	addr  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	p := trusttest.New(t)
	dir := t.TempDir()
	p.WriteTrust(t, filepath.Join(dir, "trust"))
	p.WritePKI(t, filepath.Join(dir, "pki"))
	cfg, err := trust.LoadConfig(filepath.Join(dir, "trust"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := trust.LoadIdentity(filepath.Join(dir, "pki"), cfg, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view := opstore.HostView{
		Enrolment: &opstore.ActiveEnrolment{ID: p.Record.EnrolmentID, ProviderUID: p.Record.ProviderUID,
			HostID: trusttest.HostName, InstallID: p.Record.InstallID, StartEpoch: 1},
		Position: journal.Position{Seq: 3},
	}
	view.Position.StoreID[0], view.Position.Head[0] = 7, 8
	e := &env{t: t, pki: p, creds: server.NewCredentialStore(&server.Credentials{Identity: id, Trust: cfg}),
		agent: &fakeAgent{view: view, state: providerv1.AgentState_AGENT_STATE_READY}}
	srv, err := server.New(server.Options{Agent: e.agent, Credentials: e.creds, Version: "test", JournalSchemaVersion: 1, Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = lis.Addr().String()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return e
}

// clientTLS is a Provider's TLS configuration: cert as client certificate,
// and the agent verified against the host identity issuer and the host URI.
func (e *env) clientTLS(cert *tls.Certificate, maxVersion uint16) *tls.Config {
	roots := e.pki.HostCAPool()
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         maxVersion,
		InsecureSkipVerify: true, // #nosec G402 -- verified below against the host identity issuer and URI
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
				return err
			}
			for _, u := range leaf.URIs {
				if u.String() == e.pki.HostURI {
					return nil
				}
			}
			return errors.New("not the expected host")
		},
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg
}

func (e *env) dial(cert *tls.Certificate, maxVersion uint16) *grpc.ClientConn {
	e.t.Helper()
	if maxVersion == 0 {
		maxVersion = tls.VersionTLS13
	}
	cc, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(e.clientTLS(cert, maxVersion))))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func (e *env) provider() *tls.Certificate {
	c := e.pki.ProviderCert(e.t, nil, trusttest.ProviderURI(trusttest.ProviderUID))
	return &c
}

func nonce(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func reasonOf(err error) string {
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == server.ErrorInfoDomain {
			return info.GetReason()
		}
	}
	return ""
}

// verifyAnswer checks a SignedBlob of a HostAnswer against the host identity
// issuer and returns the answer.
func verifyAnswer(t *testing.T, e *env, blob *providerv1.SignedBlob) *providerv1.HostAnswer {
	t.Helper()
	if blob == nil {
		t.Fatal("no signed answer")
	}
	cert, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: e.pki.HostCAPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatal(err)
	}
	m, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(m)
	if !ecdsa.VerifyASN1(cert.PublicKey.(*ecdsa.PublicKey), d[:], blob.GetSignature()) {
		t.Fatal("answer signature does not verify")
	}
	if blob.GetBodyType() != answer.BodyTypeHostAnswer {
		t.Fatalf("body type %q", blob.GetBodyType())
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestErrorInfoDomainIsTheContractDomain(t *testing.T) {
	if server.ErrorInfoDomain != contracts.ErrorInfoDomain || opstore.ReasonProviderMismatch != contracts.ProviderMismatchReason ||
		opstore.ReasonUnattested != contracts.UnattestedReason || opstore.ReasonQuarantined != contracts.QuarantinedReason {
		t.Fatal("agent reason strings differ from the provider contract")
	}
}

// TestHelloSignedAnswer: an allowed Provider gets a host-signed answer that
// echoes its nonce and names the host's enrolment and store position.
func TestHelloSignedAnswer(t *testing.T) {
	e := newEnv(t)
	c := agentv1.NewAgentServiceClient(e.dial(e.provider(), 0))
	resp, err := c.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(5), CallerApiVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	a := verifyAnswer(t, e, resp.GetHostAnswer())
	cx := a.GetContext()
	if !bytes.Equal(cx.GetNonce(), nonce(5)) || cx.GetProviderUid() != trusttest.ProviderUID || cx.GetHostId() != trusttest.HostName ||
		!bytes.Equal(cx.GetEnrolmentId(), e.pki.Record.EnrolmentID) || cx.GetAgentState() != providerv1.AgentState_AGENT_STATE_READY ||
		a.GetAgentApiVersion() != 1 || a.GetMode() != providerv1.AgentMode_AGENT_MODE_IDENTITY {
		t.Fatalf("answer = %v", a)
	}
	st, err := c.AgentStatus(ctx(t), &agentv1.AgentStatusRequest{AnswerNonce: nonce(6)})
	if err != nil {
		t.Fatal(err)
	}
	if a := verifyAnswer(t, e, st.GetHostAnswer()); !bytes.Equal(a.GetContext().GetNonce(), nonce(6)) {
		t.Fatal("status answer does not echo its nonce")
	}
	for _, req := range []*agentv1.HelloRequest{{CallerApiVersion: 1}, {AnswerNonce: nonce(1)[:15], CallerApiVersion: 1}, {AnswerNonce: nonce(1)}} {
		if _, err := c.Hello(ctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("Hello(%v) = %v, want InvalidArgument", req, err)
		}
	}
	if _, err := c.InstallRenewedCertificate(ctx(t), &agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce(1)}); status.Code(err) != codes.Unimplemented {
		t.Errorf("InstallRenewedCertificate = %v", err)
	}
}

// TestUnauthenticatedProvidersRefused: a caller without a client
// certificate, with one from another issuer, with an identity outside the
// allow-list, or offering only TLS 1.2, never completes a call.
func TestUnauthenticatedProvidersRefused(t *testing.T) {
	e := newEnv(t)
	foreign := hostidentitytest.NewCA(t)
	host := tls.Certificate{Certificate: [][]byte{e.creds.Load().Identity.Leaf.Raw}, PrivateKey: e.pki.HostKey}
	for name, tc := range map[string]struct {
		cert *tls.Certificate
		max  uint16
	}{
		"no client certificate": {nil, 0},
		"foreign issuer":        {ptr(e.pki.ProviderCert(t, foreign, trusttest.ProviderURI(trusttest.ProviderUID))), 0},
		"host CA as issuer":     {ptr(e.pki.ProviderCert(t, e.pki.HostCA, trusttest.ProviderURI(trusttest.ProviderUID))), 0},
		"Provider not allowed":  {ptr(e.pki.ProviderCert(t, nil, "spiffe://example.test/virtrigaud/provider/infra/other/"+trusttest.ProviderUID)), 0},
		"other trust domain":    {ptr(e.pki.ProviderCert(t, nil, "spiffe://other.test/virtrigaud/provider/infra/ch/"+trusttest.ProviderUID)), 0},
		"identity without UID":  {ptr(e.pki.ProviderCert(t, nil, trusttest.ProviderIdentity().String())), 0},
		"host certificate":      {&host, 0},
		"TLS 1.2 only":          {e.provider(), tls.VersionTLS12},
	} {
		t.Run(name, func(t *testing.T) {
			c := agentv1.NewAgentServiceClient(e.dial(tc.cert, tc.max))
			_, err := c.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1})
			if err == nil {
				t.Fatal("call completed")
			}
			if code := status.Code(err); code != codes.Unavailable {
				t.Fatalf("code = %v (%v), want a failed handshake", code, err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestProviderMismatch: an allowed Provider identity whose UID is not the
// Provider the host is enrolled with is refused PROVIDER_MISMATCH on every
// method, before any other check.
func TestProviderMismatch(t *testing.T) {
	e := newEnv(t)
	other := e.pki.ProviderCert(t, nil, trusttest.ProviderURI("0b7a1c3e-9999-4222-8333-944455556666"))
	cc := e.dial(&other, 0)
	_, err := agentv1.NewAgentServiceClient(cc).Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1})
	if status.Code(err) != codes.FailedPrecondition || reasonOf(err) != contracts.ProviderMismatchReason {
		t.Fatalf("Hello = %v", err)
	}
	_, err = providerv1.NewProviderClient(cc).Create(ctx(t), &providerv1.CreateRequest{})
	if reasonOf(err) != contracts.ProviderMismatchReason {
		t.Fatalf("Create = %v", err)
	}
	_, err = providerv1.NewProviderClient(cc).GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{})
	if reasonOf(err) != contracts.ProviderMismatchReason {
		t.Fatalf("GetHostInfo = %v", err)
	}
}

// TestMutationsRefused: every mutating method is refused UNATTESTED before
// the agent is ready, QUARANTINED while its store is lost, and Unimplemented
// once ready; reads keep working in every state.
func TestMutationsRefused(t *testing.T) {
	e := newEnv(t)
	cc := e.dial(e.provider(), 0)
	for _, tc := range []struct {
		state  providerv1.AgentState
		reason string
		code   codes.Code
		want   string
	}{
		{providerv1.AgentState_AGENT_STATE_UNATTESTED, "", codes.Unavailable, contracts.UnattestedReason},
		{providerv1.AgentState_AGENT_STATE_QUARANTINED, string(journal.LostRolledBack), codes.FailedPrecondition, contracts.QuarantinedReason},
		{providerv1.AgentState_AGENT_STATE_READY, "", codes.Unimplemented, ""},
	} {
		e.agent.set(tc.state, tc.reason)
		for _, row := range grantverify.Rows() {
			err := cc.Invoke(ctx(t), row.FullMethod, row.NewRequest(), row.NewRequest())
			if status.Code(err) != tc.code || reasonOf(err) != tc.want {
				t.Errorf("%v: %s = %v, want %v %q", tc.state, row.FullMethod, err, tc.code, tc.want)
			}
		}
		resp, err := agentv1.NewAgentServiceClient(cc).Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(2), CallerApiVersion: 1})
		if err != nil {
			t.Fatalf("%v: Hello = %v", tc.state, err)
		}
		a := verifyAnswer(t, e, resp.GetHostAnswer())
		if a.GetContext().GetAgentState() != tc.state || a.GetContext().GetStateReason() != tc.reason {
			t.Fatalf("%v: answer state %v %q", tc.state, a.GetContext().GetAgentState(), a.GetContext().GetStateReason())
		}
	}
	// The other Provider methods are not served.
	pc := providerv1.NewProviderClient(cc)
	for name, call := range map[string]func() error{
		"Describe":   func() error { _, err := pc.Describe(ctx(t), &providerv1.DescribeRequest{}); return err },
		"TaskStatus": func() error { _, err := pc.TaskStatus(ctx(t), &providerv1.TaskStatusRequest{}); return err },
		"ListHosts":  func() error { _, err := pc.ListHosts(ctx(t), &providerv1.ListHostsRequest{}); return err },
		"Validate":   func() error { _, err := pc.Validate(ctx(t), &providerv1.ValidateRequest{}); return err },
	} {
		if err := call(); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want Unimplemented", name, err)
		}
	}
}

func TestGetHostInfo(t *testing.T) {
	e := newEnv(t)
	pc := providerv1.NewProviderClient(e.dial(e.provider(), 0))
	info, err := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{HostId: trusttest.HostName})
	if err != nil || info.GetId() != trusttest.HostName || info.GetHealth() != providerv1.HostHealth_HOST_HEALTH_READY || info.GetHostAnswer() != nil {
		t.Fatalf("GetHostInfo without nonce = %v, %v", info, err)
	}
	info, err = pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{AnswerNonce: nonce(3), ContinuitySeq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a := verifyAnswer(t, e, info.GetHostAnswer()); !bytes.Equal(a.GetContext().GetNonce(), nonce(3)) {
		t.Fatal("nonce not echoed")
	}
	for name, req := range map[string]*providerv1.GetHostInfoRequest{
		"other host":                 {HostId: "host-b"},
		"short nonce":                {AnswerNonce: nonce(1)[:8]},
		"continuity without a nonce": {ContinuitySeq: 1},
	} {
		if _, err := pc.GetHostInfo(ctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.agent.set(providerv1.AgentState_AGENT_STATE_UNATTESTED, "")
	if info, _ := pc.GetHostInfo(ctx(t), &providerv1.GetHostInfoRequest{}); info.GetHealth() != providerv1.HostHealth_HOST_HEALTH_NOT_READY {
		t.Fatalf("health while unattested = %v", info.GetHealth())
	}
}

// TestNoAnswerWithoutEnrolment: with no active enrolment the reads answer,
// unsigned.
func TestNoAnswerWithoutEnrolment(t *testing.T) {
	e := newEnv(t)
	e.agent.mu.Lock()
	e.agent.view.Enrolment = nil
	e.agent.mu.Unlock()
	resp, err := agentv1.NewAgentServiceClient(e.dial(e.provider(), 0)).Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1})
	if err != nil || resp.GetHostAnswer() != nil {
		t.Fatalf("Hello = %v, %v; want an unsigned answer", resp, err)
	}
}

// TestCredentialReload: a reload that removes the Provider from the
// allow-list refuses its next call on an established connection.
func TestCredentialReload(t *testing.T) {
	e := newEnv(t)
	c := agentv1.NewAgentServiceClient(e.dial(e.provider(), 0))
	if _, err := c.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1}); err != nil {
		t.Fatal(err)
	}
	old := e.creds.Load()
	cfg := *old.Trust
	cfg.Providers = []trust.ProviderIdentity{{TrustDomain: trusttest.TrustDomain, Namespace: "infra", Name: "other"}}
	e.creds.Store(&server.Credentials{Identity: old.Identity, Trust: &cfg})
	if _, err := c.Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("after reload: %v", err)
	}
}
