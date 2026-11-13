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

// Package server is the per-host agent's gRPC server: mutually authenticated
// TLS 1.3 with the Provider that routes to the host, the provider.v1 Provider
// methods the agent serves for its host and the internal agent.v1 service.
//
// # Peer authentication
//
// A connection is accepted only from a client certificate that chains to the
// pinned Provider identity issuer, carries exactly one URI identity, and
// names a Provider in the trust configuration's allow-list
// (spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>/<uid>);
// anything else fails the handshake. Every call then verifies the client
// certificate again against the current issuer and clock (a reload or an
// expiry cuts off an established connection, PermissionDenied), and compares
// the Provider UID of that identity with the Provider the host is enrolled
// with, refusing a different one with PROVIDER_MISMATCH before any other
// check. Connections are closed after MaxConnectionAge. The
// server speaks TLS 1.3 only, so the host key never signs a TLS 1.2 key
// exchange chosen by an unauthenticated client.
//
// # What it serves
//
// GetHostInfo, agent.v1 Hello and AgentStatus answer with a host-signed
// HostAnswer when the request carries a nonce and the store's active
// enrolment is the one the host certificate names. Every mutating Provider
// method (the methods of the operation kind table) is refused: UNATTESTED
// until the store's continuity is established and its recovery has run,
// QUARANTINED while the store has lost its persistence guarantee, and
// Unimplemented once ready, because this agent does not execute operations
// yet. Every other method is Unimplemented.
//
// It imports no Kubernetes package.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ErrorInfoDomain is the google.rpc.ErrorInfo domain of every reason the
// agent answers with; it equals the provider contract's domain.
const ErrorInfoDomain = "provider.virtrigaud.io"

// Connection limits of the server.
const (
	// ConnectionTimeout bounds a connection's TLS handshake.
	ConnectionTimeout = 10 * time.Second
	// MaxConcurrentStreams bounds the calls in flight on one connection.
	MaxConcurrentStreams = 64
	// MaxConnectionAge closes every connection after this age, so that
	// each one is authenticated again with a new handshake against the
	// current credentials.
	MaxConnectionAge = 30 * time.Minute
	// MaxConnectionAgeGrace is how long running calls may finish after
	// MaxConnectionAge.
	MaxConnectionAgeGrace = time.Minute
	// KeepaliveMinTime is the shortest keepalive ping interval a client may
	// use; a client that pings more often is disconnected.
	KeepaliveMinTime = 30 * time.Second
)

// Credentials is what the TLS server and the peer check use: the host's
// identity and the trust configuration. It is replaced as a whole on reload.
type Credentials struct {
	// Identity is the host's identity.
	Identity *trust.Identity
	// Trust is the trust configuration.
	Trust *trust.Config
}

// CredentialStore holds the current Credentials. It is safe for concurrent
// use; a handshake or call reads one consistent value.
type CredentialStore struct{ p atomic.Pointer[Credentials] }

// NewCredentialStore returns a store holding c.
func NewCredentialStore(c *Credentials) *CredentialStore {
	s := &CredentialStore{}
	s.p.Store(c)
	return s
}

// Load returns the current Credentials.
func (s *CredentialStore) Load() *Credentials { return s.p.Load() }

// Store replaces the Credentials. New handshakes and calls use them;
// established connections keep their peer, which every call re-checks
// against the new Credentials.
func (s *CredentialStore) Store(c *Credentials) { s.p.Store(c) }

// TLSConfig returns the server TLS configuration: TLS 1.3 only, the host
// certificate, and a required client certificate that chains to the pinned
// Provider identity issuer and names an allowed Provider. Each handshake
// reads the current Credentials.
func TLSConfig(creds *CredentialStore) *tls.Config { return tlsConfig(creds, nil) }

// tlsConfig is TLSConfig with a hook that runs when a ClientHello has
// arrived, before anything is signed: a non-nil error ends the handshake.
func tlsConfig(creds *CredentialStore, onHello func(net.Conn) error) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if onHello != nil {
				if err := onHello(hello.Conn); err != nil {
					return nil, err
				}
			}
			c := creds.Load()
			if c == nil {
				return nil, errors.New("no server credentials")
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				MaxVersion:   tls.VersionTLS13,
				NextProtos:   []string{"h2"},
				Certificates: []tls.Certificate{c.Identity.TLS},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    c.Trust.ProviderCAPool,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return errors.New("no client certificate")
					}
					_, _, err := c.Trust.AuthorizeProvider(cs.PeerCertificates[0])
					return err
				},
			}, nil
		},
	}
}

// Agent is what the server reads about the agent.
type Agent interface {
	// HostView reads the store's view (opstore.Store.HostView).
	HostView(ctx context.Context, continuitySeq uint64) (opstore.HostView, error)
	// TrustState returns the agent's trust state and its reason
	// (answer.StoreState).
	TrustState() (providerv1.AgentState, string)
	// ClockFloor returns the host's clock floor; zero when it has none.
	ClockFloor() time.Time
}

// Options configures New.
type Options struct {
	// Agent is the agent the server answers for. Required.
	Agent Agent
	// Credentials are the TLS credentials. Required.
	Credentials *CredentialStore
	// Version is the agent's build version, reported in host answers.
	Version string
	// JournalSchemaVersion is the store's schema version.
	JournalSchemaVersion uint32
	// Log receives one line per refused call and per answer that could not
	// be signed; never request bytes.
	Log logr.Logger
	// Now is the clock for answers; nil uses time.Now.
	Now func() time.Time
	// Throttle sets the pre-handshake limits (see Throttle); zero fields
	// take their defaults.
	Throttle Throttle
	// AnswersPerSecond and AnswerBurst limit the signed answers made for one
	// Provider identity (the verified client certificate's URI), so that one
	// Provider cannot use up the signer's answer budget; a read above the
	// limit is answered without a signed answer. Zero fields take
	// DefaultAnswersPerSecond and DefaultAnswerBurst.
	AnswersPerSecond float64
	// AnswerBurst is the burst of the per-identity answer limit.
	AnswerBurst int
}

// Defaults of the per-identity answer limit, below the signer's answer budget
// (50 per second) so that one identity cannot use all of it.
const (
	// DefaultAnswersPerSecond is the sustained signed answers per second for
	// one Provider identity.
	DefaultAnswersPerSecond = 10.0
	// DefaultAnswerBurst is the burst of signed answers for one identity.
	DefaultAnswerBurst = 20
)

// Server is the agent's gRPC server.
type Server struct {
	opts      Options
	grpc      *grpc.Server
	mutating  map[string]bool
	throttle  *throttle
	answers   *sourceLimiter
	noAnswer  logGate
	limited   logGate
	providerS *providerService
	agentS    *agentService
}

// New returns a server for opts.
func New(opts Options) (*Server, error) {
	if opts.Agent == nil || opts.Credentials == nil || opts.Credentials.Load() == nil {
		return nil, errors.New("agent server: agent and credentials are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, mutating: MutatingMethods(), noAnswer: logGate{every: refusalLogEvery}}
	s.throttle = newThrottle(opts.Throttle, opts.Log, time.Now)
	if opts.AnswersPerSecond <= 0 {
		opts.AnswersPerSecond = DefaultAnswersPerSecond
	}
	if opts.AnswerBurst <= 0 {
		opts.AnswerBurst = DefaultAnswerBurst
	}
	s.answers = newSourceLimiter(opts.AnswersPerSecond, opts.AnswerBurst)
	s.limited = logGate{every: refusalLogEvery}
	s.opts = opts
	so := []grpc.ServerOption{
		grpc.Creds(s.throttle.creds(credentials.NewTLS(tlsConfig(opts.Credentials, s.throttle.acquire)))),
		grpc.ConnectionTimeout(ConnectionTimeout),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: MaxConnectionAge, MaxConnectionAgeGrace: MaxConnectionAgeGrace}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: KeepaliveMinTime}),
		grpc.ChainUnaryInterceptor(s.unaryGate),
		grpc.ChainStreamInterceptor(s.streamGate),
		grpc.MaxRecvMsgSize(4 << 20),
	}
	s.grpc = grpc.NewServer(so...)
	s.providerS = &providerService{s: s}
	s.agentS = &agentService{s: s}
	providerv1.RegisterProviderServer(s.grpc, s.providerS)
	agentv1.RegisterAgentServiceServer(s.grpc, s.agentS)
	return s, nil
}

// Serve serves on lis until Stop or GracefulStop. Connections pass the
// pre-handshake throttle first (see Throttle).
func (s *Server) Serve(lis net.Listener) error {
	if err := s.grpc.Serve(s.throttle.listener(lis)); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("agent server: %w", err)
	}
	return nil
}

// Shutdown stops accepting calls and waits for running calls to finish until
// ctx ends, then closes every connection.
func (s *Server) Shutdown(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
}

// MutatingMethods returns the full names of the Provider methods that carry
// an operation: every method of the operation kind table.
func MutatingMethods() map[string]bool {
	out := map[string]bool{}
	for _, r := range grantverify.Rows() {
		out[r.FullMethod] = true
	}
	return out
}

// refusal is a gRPC status with a google.rpc.ErrorInfo reason.
func refusal(code codes.Code, reason, msg string) error {
	st := status.New(code, msg)
	if withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorInfoDomain}); err == nil {
		return withInfo.Err()
	}
	return st.Err()
}

// checkPeer authenticates the caller of one call: an allowed Provider
// identity (checked again against the current trust configuration) whose UID
// is the Provider the host is enrolled with.
func (s *Server) checkPeer(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "the caller presented no client certificate")
	}
	c := s.opts.Credentials.Load()
	leaf := info.State.PeerCertificates[0]
	// The handshake verified the chain against the credentials of that
	// moment; a reload may since have replaced the Provider identity issuer,
	// and the certificate may since have expired. Verify again, now.
	inter := x509.NewCertPool()
	for _, ic := range info.State.PeerCertificates[1:] {
		inter.AddCert(ic)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: c.Trust.ProviderCAPool, Intermediates: inter,
		CurrentTime: s.opts.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return status.Error(codes.PermissionDenied, "the caller's certificate is no longer valid for this agent")
	}
	_, uid, err := c.Trust.AuthorizeProvider(leaf)
	if err != nil {
		return status.Error(codes.PermissionDenied, "the caller is not an allowed Provider")
	}
	if uid != c.Identity.Record.ProviderUID {
		return refusal(codes.FailedPrecondition, opstore.ReasonProviderMismatch,
			"the caller is not the Provider this host is enrolled with")
	}
	return nil
}

// gate applies the peer check and, for a mutating method, the readiness gate.
func (s *Server) gate(ctx context.Context, method string) error {
	if err := s.checkPeer(ctx); err != nil {
		s.opts.Log.V(1).Info("call refused", "method", method, "reason", status.Code(err).String())
		return err
	}
	if !s.mutating[method] {
		return nil
	}
	state, reason := s.opts.Agent.TrustState()
	var err error
	switch state {
	case providerv1.AgentState_AGENT_STATE_READY:
		err = status.Error(codes.Unimplemented, "this agent executes no operations")
	case providerv1.AgentState_AGENT_STATE_QUARANTINED:
		err = refusal(codes.FailedPrecondition, opstore.ReasonQuarantined, "the host's operation store is quarantined ("+reason+")")
	default:
		err = refusal(codes.Unavailable, opstore.ReasonUnattested, "the host's operation store has not established its continuity")
	}
	s.opts.Log.Info("mutation refused", "method", method, "state", state.String())
	return err
}

func (s *Server) unaryGate(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.gate(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) streamGate(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.gate(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	return handler(srv, ss)
}

// hostAnswer signs a HostAnswer for nonce, or returns nil when none can be
// made (no nonce, no matching enrolment, a store that cannot be read).
func (s *Server) hostAnswer(ctx context.Context, nonce []byte, continuitySeq uint64) *providerv1.SignedBlob {
	if len(nonce) == 0 {
		return nil
	}
	if !s.answers.allow(peerIdentity(ctx), s.opts.Now()) {
		if ok, n := s.limited.allow(s.opts.Now()); ok {
			s.opts.Log.Info("no signed answer: the Provider is above its answer rate", "suppressedSinceLast", n)
		}
		return nil
	}
	c := s.opts.Credentials.Load()
	view, err := s.opts.Agent.HostView(ctx, continuitySeq)
	if err != nil {
		s.logNoAnswer(err, "no signed answer: the store cannot be read")
		return nil
	}
	state, reason := s.opts.Agent.TrustState()
	body, err := answer.HostAnswer(answer.Host{
		Nonce: nonce, View: view, HostID: c.Identity.HostID(), Record: c.Identity.Record,
		State: state, StateReason: reason, AgentVersion: s.opts.Version,
		JournalSchemaVersion: s.opts.JournalSchemaVersion, ClockFloor: s.opts.Agent.ClockFloor(), Now: s.opts.Now(),
	})
	if err != nil {
		if errors.Is(err, answer.ErrNoAnswer) {
			s.opts.Log.V(1).Info("no signed answer", "why", err.Error())
		} else {
			s.logNoAnswer(err, "no signed answer")
		}
		return nil
	}
	blob, err := answer.Sign(c.Identity.Signer, c.Identity.Leaf.Raw, body)
	if err != nil {
		// A signer that is away or refusing fails every answer.
		s.logNoAnswer(err, "no signed answer")
		return nil
	}
	return blob
}

// peerIdentity is the key of the verified caller for the answer limit: the
// URI names of its client certificate (the Provider identity), or its serial
// number.
func peerIdentity(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return ""
	}
	leaf := info.State.PeerCertificates[0]
	key := ""
	for _, u := range leaf.URIs {
		key += u.String() + " "
	}
	if key == "" {
		key = leaf.SerialNumber.String()
	}
	return key
}

// logNoAnswer logs why an answer could not be made, one line per interval
// with the count held back: a signer that is away fails every answer, and a
// log line per request would let any caller fill the journal.
func (s *Server) logNoAnswer(err error, msg string) {
	if ok, n := s.noAnswer.allow(s.opts.Now()); ok {
		s.opts.Log.Error(err, msg, "suppressedSinceLast", n)
	}
}

// checkNonce refuses a nonce that is set but not exactly 16 bytes.
func checkNonce(nonce []byte, required bool) error {
	if len(nonce) == 0 && !required {
		return nil
	}
	if len(nonce) != answer.NonceLen {
		return status.Errorf(codes.InvalidArgument, "answer_nonce must be exactly %d bytes", answer.NonceLen)
	}
	return nil
}
