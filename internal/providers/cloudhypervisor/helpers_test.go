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
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	testTrustDomain = "cluster.test"
	testHostNS      = "hosts"
	testProviderUID = "7d0c0f7e-2a8e-4e62-9a55-0c1f6f1f2a01"
	testTimeout     = 10 * time.Second
)

// pki is the test's host identity issuer and the Provider's client
// certificate.
type pki struct {
	ca     *hostidentitytest.CA
	client tls.Certificate
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	ca := hostidentitytest.NewCA(t)
	other := hostidentitytest.NewCA(t)
	key := hostidentitytest.NewKey(t)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: []string{"spiffe://" + testTrustDomain + "/virtrigaud/provider/infra/ch/" + testProviderUID}})
	certPEM := other.Issue(t, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	return &pki{ca: ca, client: tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key}}
}

// hostCert issues a host certificate for host from ca, naming providerUID in
// its enrolment record.
func hostCert(t *testing.T, ca *hostidentitytest.CA, host, providerUID string, key *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	uri, err := hostidentity.HostURI(testTrustDomain, testHostNS, host)
	require.NoError(t, err)
	rec := hostidentity.Record{EnrolmentID: make([]byte, hostidentity.IDLen), ProviderUID: providerUID,
		InstallID: make([]byte, hostidentity.IDLen), Mode: hostidentity.ModeIdentity}
	rec.EnrolmentID[0], rec.InstallID[0] = 1, 2
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(uri, rec)})
	block, _ := pem.Decode(ca.Issue(t, csr, hostidentitytest.IssueOptions{}))
	require.NotNil(t, block)
	c, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return c
}

// captured is one call a fake agent received.
type captured struct {
	method string
	raw    []byte
}

// countingListener tracks the connections it accepted that are still open.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
	open     atomic.Int32
	mu       sync.Mutex
	conns    map[*countedConn]struct{}
}

type countedConn struct {
	net.Conn
	l    *countingListener
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() {
		c.l.open.Add(-1)
		c.l.mu.Lock()
		delete(c.l.conns, c)
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	l.open.Add(1)
	cc := &countedConn{Conn: c, l: l}
	l.mu.Lock()
	l.conns[cc] = struct{}{}
	l.mu.Unlock()
	return cc, nil
}

// dropAll closes every open connection, as a restarted agent would.
func (l *countingListener) dropAll() {
	l.mu.Lock()
	conns := make([]*countedConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// fakeAgent is a TLS gRPC server that presents a host certificate, answers
// Hello with a signed host answer and records the exact bytes of every
// provider.v1 call it receives.
type fakeAgent struct {
	t          *testing.T
	name       string
	hostURI    string
	key        *ecdsa.PrivateKey
	leaf       *x509.Certificate
	ca         *hostidentitytest.CA
	lis        *countingListener
	srv        *grpc.Server
	apiVersion atomic.Uint32
	hellos     atomic.Int32

	mu      sync.Mutex
	calls   []captured
	respond func(method string, raw []byte) (proto.Message, error)
}

type fakeAgentOptions struct {
	// leaf replaces the certificate the agent presents (key is its key).
	leaf *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newFakeAgent(t *testing.T, p *pki, name string, o fakeAgentOptions) *fakeAgent {
	t.Helper()
	uri, err := hostidentity.HostURI(testTrustDomain, testHostNS, name)
	require.NoError(t, err)
	key, leaf := o.key, o.leaf
	if leaf == nil {
		key = hostidentitytest.NewKey(t)
		leaf = hostCert(t, p.ca, name, testProviderUID, key)
	}
	f := &fakeAgent{t: t, name: name, hostURI: uri, key: key, leaf: leaf, ca: p.ca}
	f.apiVersion.Store(answer.AgentAPIVersion)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.lis = &countingListener{Listener: l, conns: map[*countedConn]struct{}{}}
	f.srv = grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}},
		ClientAuth:   tls.RequireAnyClientCert,
	})), grpc.UnknownServiceHandler(f.unknown))
	agentv1.RegisterAgentServiceServer(f.srv, &helloServer{f: f})
	go func() { _ = f.srv.Serve(f.lis) }()
	t.Cleanup(f.srv.Stop)
	return f
}

// entry is the agent's inventory entry: its CA, host URI and key pin.
func (f *fakeAgent) entry() hostsecret.Host {
	f.t.Helper()
	pin, err := hostidentity.SPKIPin(f.key.Public())
	require.NoError(f.t, err)
	return hostsecret.Host{ID: f.name, Endpoint: "grpc://" + f.lis.Addr().String(), Labels: map[string]string{"zone": "a"},
		Credentials: hostsecret.Credentials{AgentTLS: &hostsecret.AgentTLS{CABundle: f.ca.PEM, ExpectedSAN: f.hostURI, KeyPins: []string{pin}}}}
}

func (f *fakeAgent) received() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.calls...)
}

func (f *fakeAgent) setRespond(r func(method string, raw []byte) (proto.Message, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = r
}

func (f *fakeAgent) unknown(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	in := &emptypb.Empty{}
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	f.mu.Lock()
	f.calls = append(f.calls, captured{method: method, raw: append([]byte(nil), frameBytes(in)...)})
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		respond = defaultRespond
	}
	resp, err := respond(method, frameBytes(in))
	if err != nil {
		return err
	}
	return stream.SendMsg(resp)
}

// defaultRespond answers a mutating call with a task and anything else with
// an empty message.
func defaultRespond(method string, _ []byte) (proto.Message, error) {
	switch method {
	case providerv1.Provider_Create_FullMethodName:
		return &providerv1.CreateResponse{Id: "vm-1", Task: &providerv1.TaskRef{Id: "task-1"}}, nil
	case providerv1.Provider_Delete_FullMethodName, providerv1.Provider_Power_FullMethodName, providerv1.Provider_Reconfigure_FullMethodName:
		return &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-1"}}, nil
	}
	return &emptypb.Empty{}, nil
}

type helloServer struct {
	agentv1.UnimplementedAgentServiceServer
	f *fakeAgent
}

func (h *helloServer) Hello(_ context.Context, req *agentv1.HelloRequest) (*agentv1.HelloResponse, error) {
	h.f.hellos.Add(1)
	ha := &providerv1.HostAnswer{Context: &providerv1.AnswerContext{Nonce: req.GetAnswerNonce(), HostId: h.f.name},
		AgentApiVersion: h.f.apiVersion.Load()}
	blob, err := answer.Sign(h.f.key, h.f.leaf.Raw, ha)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.HelloResponse{HostAnswer: blob}, nil
}

// quiet discards logs.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// served starts p on a loopback gRPC server and returns a client connection
// to it.
func served(t *testing.T, p *Provider) *grpc.ClientConn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	p.Register(srv)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// newProvider returns a Provider over agents, closed when t ends.
func newProvider(t *testing.T, p *pki, o Options, agents ...*fakeAgent) *Provider {
	t.Helper()
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	for _, a := range agents {
		inv.Hosts = append(inv.Hosts, a.entry())
	}
	o.ClientCertificate = p.client
	if o.Logger == nil {
		o.Logger = quiet()
	}
	prov, err := New(o, inv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	return prov
}

// ctx is a test call context.
func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return c
}

// errorInfo returns err's ErrorInfo of reason, if any.
func errorInfo(err error, reason string) (*errdetails.ErrorInfo, bool) {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == reason {
			return info, true
		}
	}
	return nil, false
}

// requireHostUnavailable requires err to be the host-scoped refusal with
// cause.
func requireHostUnavailable(t *testing.T, err error, cause string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	info, ok := errorInfo(err, contracts.HostUnavailableReason)
	require.True(t, ok, "HOST_UNAVAILABLE: %v", err)
	require.Equal(t, cause, info.GetMetadata()[MetadataCause], "%v", err)
}

// signedPower is a PowerRequest that carries only an envelope around an
// inner request for host; the grant is opaque to the Provider.
func signedPower(t *testing.T, host string) *providerv1.PowerRequest {
	t.Helper()
	inner, err := proto.Marshal(&providerv1.PowerRequest{Id: "vm-1", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: host})
	require.NoError(t, err)
	return &providerv1.PowerRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: inner,
		Grant: &providerv1.OperationGrant{Body: []byte("opaque"), Signature: []byte("sig")}}}
}
