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
// method (the methods of the operation kind table) is refused UNATTESTED
// until the store's continuity is established and its recovery has run, and
// QUARANTINED while the store has lost its persistence guarantee. Once ready,
// a server with an operation path (Options.Operations, development builds
// only) verifies the call's operation grant and has the operation store admit
// it (see operate); a server without one refuses every mutating method
// Unimplemented. TaskStatus and Describe answer from the operation store,
// with a host-signed OperationAnswer or VMAnswer when the request carries a
// nonce. Every other method is Unimplemented.
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
	"sync"
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
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/attest"
	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
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
	// use, with or without calls in flight; a client that pings more often is
	// disconnected. The Provider pings every 20 seconds, so that it detects
	// an agent that went silent on an open connection.
	KeepaliveMinTime = 15 * time.Second
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
func TLSConfig(creds *CredentialStore) *tls.Config { return tlsConfig(creds, nil, nil) }

// tlsConfig is TLSConfig with a hook that runs when a ClientHello has
// arrived, before anything is signed (a non-nil error ends the handshake),
// and one that receives each refusal of a client certificate that chains to
// the Provider identity issuer but names no allowed Provider (a URI that is
// not a Provider identity with a UID, or a Provider not in the trust
// configuration). In TLS 1.3 that refusal comes after the client finished
// its handshake, so the client sees only a closed connection; the hook is
// what lets the agent say why.
func tlsConfig(creds *CredentialStore, onHello func(net.Conn) error, onRefused func(error)) *tls.Config {
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
					if err != nil && onRefused != nil {
						onRefused(err)
					}
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
	// ClockFloorState returns the clock floor's health; the empty state when
	// the agent has no floor (its store could not give it one).
	ClockFloorState() clockfloor.StateKind
	// UnattestedCause names why a ready store still refuses operations (one
	// of the Cause constants), or "" when nothing does.
	UnattestedCause() string
	// StoreCapacity returns how full the store is (opstore.Store.Capacity);
	// the zero Capacity (not measured) when the agent does not report it, as
	// in a release build or for a store it cannot measure. It must be cheap:
	// it is read for every signed HostAnswer but Hello's, asked for or not.
	StoreCapacity() opstore.Capacity
}

// The causes of an UNATTESTED refusal besides the store's own continuity. A
// refused mutating call carries the reserved UNATTESTED reason (the
// published contract's, exempt from the circuit breaker) and the cause in the
// ErrorInfo metadata under MetadataCause and in the message; a signed host
// answer carries no state reason for them.
const (
	// MetadataCause is the ErrorInfo metadata key that names the cause.
	MetadataCause = "cause"
	// CauseClockFloorAhead: the clock floor is ahead of the host clock by more
	// than its bound (the clock was rolled back, or jumped forward and was
	// corrected). Mutating calls are refused until the clock catches up.
	CauseClockFloorAhead = "clock_floor_ahead"
	// CauseClockFloorUnavailable: the clock floor failed (a clock source
	// failed or it could not be saved) or does not exist.
	CauseClockFloorUnavailable = "clock_floor_unavailable"
	// CauseFreezeIncomplete: a re-enrolment's freeze stopped partway.
	CauseFreezeIncomplete = "freeze_incomplete"
	// CauseRenewalIncomplete: a certificate activation stopped after the
	// signer switched keys and is not finished yet; the credentials may not
	// sign until it is (development builds only).
	CauseRenewalIncomplete = "renewal_activation_incomplete"
	// CauseEnrolmentMismatch: the store's active enrolment is not the one the
	// host certificate names; new operations are refused until the host is
	// enrolled again.
	CauseEnrolmentMismatch = "enrolment_mismatch"
	// CauseAwaitingAttestation: the agent has not been attested against the
	// manager's host ledger since it started (development builds).
	CauseAwaitingAttestation = "awaiting_attestation"
)

// ReasonLedgerBehind is the ErrorInfo reason (in ErrorInfoDomain) of an
// operation refused because the host holds back too many accepted
// operations whose acceptance the manager's host ledger does not cover yet,
// or too many parked request bytes: RESOURCE_EXHAUSTED, definitely not
// performed (development builds only).
const ReasonLedgerBehind = "LEDGER_BEHIND"

// ReasonStoreFull is the ErrorInfo reason (in ErrorInfoDomain) of an
// operation refused because the host's operation store is above its size
// threshold: RESOURCE_EXHAUSTED, definitely not performed. A re-key of the
// Host reclaims the space (development builds only serve operations).
const ReasonStoreFull = "STORE_FULL"

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
	// LogProviderRefusals logs, rate-limited, each Provider client
	// certificate refused after the TLS handshake (development builds).
	LogProviderRefusals bool
	// Now is the clock for answers; nil uses time.Now.
	Now func() time.Time
	// Inventory supplies the host inventory for a GetHostInfo that asks for
	// one; nil answers without an inventory.
	Inventory InventorySource
	// InventoryMaxBytes lowers the encoded size bound of an inventory in an
	// answer; zero uses the standard bound. Only tests set it.
	InventoryMaxBytes int
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
	// HelloAnswersPerSecond and HelloAnswerBurst limit the signed answers to
	// Hello for one Provider identity, separately from AnswersPerSecond, so
	// that reads at the answer limit cannot starve the Hello a Provider
	// makes on each new connection. Zero fields take
	// DefaultHelloAnswersPerSecond and DefaultHelloAnswerBurst.
	HelloAnswersPerSecond float64
	// HelloAnswerBurst is the burst of the per-identity Hello answer limit.
	HelloAnswerBurst int
	// ReceiptsPerSecond and ReceiptBurst limit the operation receipts
	// signed for one Provider identity, separately from AnswersPerSecond.
	// A delivery above the limit is answered without a receipt (the
	// manager asks for the operation's state instead). Zero fields take
	// DefaultReceiptsPerSecond and DefaultReceiptBurst.
	ReceiptsPerSecond float64
	// ReceiptBurst is the burst of the per-identity receipt limit.
	ReceiptBurst int
	// Operations is the agent's operation path. Nil refuses every mutating
	// method once the agent is ready (Unimplemented): a release build never
	// sets it.
	Operations Operations
	// Freezes applies freeze confirmations relayed in AgentStatus. Nil
	// ignores them: a release build never sets it.
	Freezes FreezeConfirmer
	// Renewals is the host's certificate renewal. Nil serves none
	// (InstallRenewedCertificate is Unimplemented and no answer carries a
	// renewal request): a release build never sets it.
	Renewals Renewals
	// Authority is the agent's attestation against the manager's host
	// ledger. Nil serves no HostAuthority service (every method is
	// Unimplemented) and reports the agent as awaiting attestation in every
	// ledger state: a release build never sets it. When set, every mutating
	// method is refused UNATTESTED until the agent is attested, and every
	// operation accepted while attested is answered with a signed receipt.
	Authority Authority
	// ChallengeClock replaces CLOCK_MONOTONIC_RAW for the validity of
	// attestation challenges. Only tests set it.
	ChallengeClock attest.Clock
}

// Authority is the agent's attestation against the manager's host ledger,
// as the server serves it (the operation store, opstore.Store).
type Authority interface {
	// Attestation returns the current attestation session; ok is false
	// while the agent awaits its first attestation.
	Attestation() (a opstore.Attestation, ok bool)
	// Attest applies a verified attestation (opstore.Store.Attest).
	Attest(ctx context.Context, r opstore.AttestRequest) (opstore.AttestResult, error)
	// LedgerMark applies a verified ledger mark (opstore.Store.LedgerMark).
	LedgerMark(ctx context.Context, r opstore.LedgerMarkRequest) (opstore.LedgerMarkResult, error)
	// RetireIfUnknown applies a verified retire-if-unknown
	// (opstore.Store.RetireIfUnknown).
	RetireIfUnknown(ctx context.Context, r opstore.RetireRequest) (opstore.RetireResult, error)
	// Backlog is what the ledger gate holds back (opstore.Store.Backlog).
	Backlog() opstore.LedgerBacklog
}

// Renewals is the host's certificate renewal, as the server serves it.
type Renewals interface {
	// PendingRequest returns the host's pending renewal request for a
	// GetHostInfo that asked for renewal version requested (at least 1),
	// signed with the current key; nil when there is none.
	PendingRequest(ctx context.Context, requested uint32) *providerv1.SignedBlob
	// StagedSHA256 returns the SHA-256 of the staged renewed certificate,
	// nil when none is staged.
	StagedSHA256() []byte
	// Install stages a renewed certificate (activate false) or puts the
	// staged one in use (activate true); the error is a gRPC status.
	Install(ctx context.Context, certificate []byte, activate bool) error
}

// FreezeConfirmer applies a relayed freeze confirmation (the operation store,
// opstore.Store.ConfirmFrozen): it discards the frozen predecessor
// enrolments among ids, when the confirmation was made for the active
// enrolment forEnrolment, and returns one outcome for each id, in order.
type FreezeConfirmer interface {
	ConfirmFrozen(ctx context.Context, forEnrolment []byte, ids [][]byte) ([]opstore.FreezeOutcome, error)
}

// InventorySource gives the host inventory a signed answer carries.
type InventorySource interface {
	// Inventory returns the inventory for a request that asked for inventory
	// version requested (at least 1), from the latest snapshot; it does no
	// file system access.
	Inventory(requested uint32) *providerv1.HostInventory
}

// Defaults of the per-identity answer limit, below the signer's answer budget
// (50 per second) so that one identity cannot use all of it.
const (
	// DefaultAnswersPerSecond is the sustained signed answers per second for
	// one Provider identity.
	DefaultAnswersPerSecond = 10.0
	// DefaultAnswerBurst is the burst of signed answers for one identity.
	DefaultAnswerBurst = 20
	// DefaultHelloAnswersPerSecond is the sustained signed Hello answers per
	// second for one Provider identity: a Provider makes one per new
	// connection.
	DefaultHelloAnswersPerSecond = 1.0
	// DefaultHelloAnswerBurst is the burst of signed Hello answers for one
	// identity.
	DefaultHelloAnswerBurst = 5
	// DefaultReceiptsPerSecond is the sustained operation receipts per
	// second for one Provider identity; with the answer and Hello limits it
	// stays below the signer's budget.
	DefaultReceiptsPerSecond = 20.0
	// DefaultReceiptBurst is the burst of operation receipts for one
	// identity.
	DefaultReceiptBurst = 40
)

// Server is the agent's gRPC server.
type Server struct {
	opts      Options
	grpc      *grpc.Server
	mutating  map[string]bool
	throttle  *throttle
	answers   *sourceLimiter
	hellos    *sourceLimiter
	receipts  *sourceLimiter
	noAnswer  logGate
	limited   logGate
	refusedPr logGate
	providerS *providerService
	agentS    *agentService
	vc        verifierCache
	// challenges are the outstanding attestation challenges.
	challenges *attest.Challenges
	// attestMu serialises the attestations, so that a challenge's purpose,
	// its consumption and the store's attestation state are judged
	// together.
	attestMu sync.Mutex
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
	if opts.HelloAnswersPerSecond <= 0 {
		opts.HelloAnswersPerSecond = DefaultHelloAnswersPerSecond
	}
	if opts.HelloAnswerBurst <= 0 {
		opts.HelloAnswerBurst = DefaultHelloAnswerBurst
	}
	s.hellos = newSourceLimiter(opts.HelloAnswersPerSecond, opts.HelloAnswerBurst)
	if opts.ReceiptsPerSecond <= 0 {
		opts.ReceiptsPerSecond = DefaultReceiptsPerSecond
	}
	if opts.ReceiptBurst <= 0 {
		opts.ReceiptBurst = DefaultReceiptBurst
	}
	s.receipts = newSourceLimiter(opts.ReceiptsPerSecond, opts.ReceiptBurst)
	s.limited = logGate{every: refusalLogEvery}
	s.refusedPr = logGate{every: refusalLogEvery}
	s.opts = opts
	var onRefused func(error)
	if opts.LogProviderRefusals {
		onRefused = s.providerRefused
	}
	so := []grpc.ServerOption{
		grpc.Creds(s.throttle.creds(credentials.NewTLS(tlsConfig(opts.Credentials, s.throttle.acquire, onRefused)))),
		grpc.ConnectionTimeout(ConnectionTimeout),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: MaxConnectionAge, MaxConnectionAgeGrace: MaxConnectionAgeGrace}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: KeepaliveMinTime, PermitWithoutStream: true}),
		grpc.ChainUnaryInterceptor(s.unaryGate),
		grpc.ChainStreamInterceptor(s.streamGate),
		grpc.MaxRecvMsgSize(4 << 20),
	}
	s.grpc = grpc.NewServer(so...)
	s.providerS = &providerService{s: s}
	s.agentS = &agentService{s: s}
	providerv1.RegisterProviderServer(s.grpc, s.providerS)
	agentv1.RegisterAgentServiceServer(s.grpc, s.agentS)
	if opts.Authority != nil {
		s.challenges = attest.New(opts.ChallengeClock)
		providerv1.RegisterHostAuthorityServer(s.grpc, &authorityService{s: s})
	}
	return s, nil
}

// providerRefused logs the refusal of a Provider client certificate, at most
// once per refusalLogEvery with the number held back since the last line.
// The reason names the rule and the Provider identity, never a key.
func (s *Server) providerRefused(err error) {
	if ok, n := s.refusedPr.allow(s.opts.Now()); ok {
		s.opts.Log.Error(err, "refused a Provider client certificate after the TLS handshake; the Provider sees a closed connection",
			"suppressedSinceLast", n)
	}
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
	return refusalWith(code, reason, msg, nil)
}

// refusalWith is refusal with ErrorInfo metadata.
func refusalWith(code codes.Code, reason, msg string, metadata map[string]string) error {
	st := status.New(code, msg)
	if withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorInfoDomain, Metadata: metadata}); err == nil {
		return withInfo.Err()
	}
	return st.Err()
}

// verifyNow is the time a client certificate's validity is judged at: the
// later of the host clock and the agent's clock floor while the floor is
// healthy, so that rolling the host clock back does not make an expired
// certificate valid again, as it does not for an expired grant key. While the
// floor is ahead of the clock beyond its bound, failed or missing, the floor
// is not used (a floor far ahead would refuse every call, GetHostInfo
// included, and the manager could no longer see why); certificates are
// judged at the host clock, and gate refuses every mutating call instead.
func (s *Server) verifyNow() time.Time {
	now := s.opts.Now()
	if s.opts.Agent.ClockFloorState() != clockfloor.StateOK {
		return now
	}
	if floor := s.opts.Agent.ClockFloor(); floor.After(now) {
		return floor
	}
	return now
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
		CurrentTime: s.verifyNow(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
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
	cause := s.opts.Agent.UnattestedCause()
	if cause == "" && state != providerv1.AgentState_AGENT_STATE_QUARANTINED && !s.attested() &&
		!guard.Off(guard.MutationsAfterAttestation) {
		cause = CauseAwaitingAttestation
	}
	switch {
	case state == providerv1.AgentState_AGENT_STATE_QUARANTINED:
		err = refusal(codes.FailedPrecondition, opstore.ReasonQuarantined, "the host's operation store is quarantined ("+reason+")")
	case cause != "":
		err = refusalWith(codes.Unavailable, opstore.ReasonUnattested, "the host refuses operations: "+cause,
			map[string]string{MetadataCause: cause})
	case state == providerv1.AgentState_AGENT_STATE_READY && s.opts.Operations != nil:
		return nil
	case state == providerv1.AgentState_AGENT_STATE_READY:
		err = status.Error(codes.Unimplemented, "this agent executes no operations")
	default:
		err = refusal(codes.Unavailable, opstore.ReasonUnattested, "the host's operation store has not established its continuity")
	}
	s.opts.Log.Info("mutation refused", "method", method, "state", state.String(), "cause", cause)
	return err
}

func (s *Server) unaryGate(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.gate(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	if s.mutating[info.FullMethod] {
		// gate passes a mutating method only to the operation path.
		return s.operate(ctx, info.FullMethod, req)
	}
	return handler(ctx, req)
}

func (s *Server) streamGate(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.gate(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	if s.mutating[info.FullMethod] {
		return status.Error(codes.Unimplemented, "no operation is carried on a stream")
	}
	return handler(srv, ss)
}

// hostAnswer signs a HostAnswer for nonce, or returns nil when none can be
// made (no nonce, no matching enrolment, a store that cannot be read). It
// carries no inventory and no renewal request.
func (s *Server) hostAnswer(ctx context.Context, nonce []byte, continuitySeq uint64) *providerv1.SignedBlob {
	return s.hostAnswerWithInventory(ctx, nonce, continuitySeq, 0, 0, 0, 0)
}

// attested reports whether the agent is attested; true when the server has
// no Authority (it does not take part in the ledger protocol).
func (s *Server) attested() bool {
	if s.opts.Authority == nil {
		return true
	}
	_, ok := s.opts.Authority.Attestation()
	return ok
}

// helloAnswer is hostAnswer for Hello, under the separate Hello answer limit.
func (s *Server) helloAnswer(ctx context.Context, nonce []byte) *providerv1.SignedBlob {
	return s.signAnswerUnder(ctx, s.hellos, nonce, 0, 0, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, 0)
		if err != nil {
			return nil, fmt.Errorf("the store cannot be read: %w", err)
		}
		h.View = view
		return answer.HostAnswer(h)
	})
}

// hostAnswerWithInventory is hostAnswer for a request that asked for the host
// inventory in version inventoryVersion, for the pending renewal request in
// version renewalVersion, for the store's capacity in version
// storeCapacityVersion and for the host ledger state in version
// ledgerVersion (0: it asked for none). See signWith for what happens
// when the inventory cannot be signed. The capacity is read just after the
// store's view (Agent.StoreCapacity: two counters, not a scan); the answer
// carries it only when the request asked for it (answer.HostAnswer).
func (s *Server) hostAnswerWithInventory(ctx context.Context, nonce []byte, continuitySeq uint64,
	inventoryVersion, renewalVersion, storeCapacityVersion, ledgerVersion uint32) *providerv1.SignedBlob {
	return s.signAnswerUnder(ctx, s.answers, nonce, inventoryVersion, renewalVersion, func(h answer.Host) (proto.Message, error) {
		view, err := s.opts.Agent.HostView(ctx, continuitySeq)
		if err != nil {
			return nil, fmt.Errorf("the store cannot be read: %w", err)
		}
		h.View = view
		h.StoreCapacityRequested, h.StoreCapacity = storeCapacityVersion, s.opts.Agent.StoreCapacity()
		h.LedgerRequested = ledgerVersion
		return answer.HostAnswer(h)
	})
}

// signAnswer makes a signed answer for nonce with build, or returns nil when
// none can be made: no nonce, the caller is above its answer rate, build
// refuses (no matching enrolment, a store that cannot be read, a field that
// breaks the answer rules), or the signer fails. It is the one signing path of
// every host answer (HostAnswer, OperationAnswer, VMAnswer). build receives
// the host's identity, trust state, clock and, when inventoryVersion is not 0
// (a GetHostInfo that asked for it), the host inventory; it reads the store
// itself, in one read.
func (s *Server) signAnswer(ctx context.Context, nonce []byte, inventoryVersion uint32, build func(answer.Host) (proto.Message, error)) *providerv1.SignedBlob {
	return s.signAnswerUnder(ctx, s.answers, nonce, inventoryVersion, 0, build)
}

// signAnswerUnder is signAnswer under the per-identity limit lim; a request
// that asked for renewal version renewalVersion (a GetHostInfo) also gets
// the pending renewal request, when there is one.
func (s *Server) signAnswerUnder(ctx context.Context, lim *sourceLimiter, nonce []byte, inventoryVersion, renewalVersion uint32,
	build func(answer.Host) (proto.Message, error)) *providerv1.SignedBlob {
	if len(nonce) == 0 {
		return nil
	}
	id := peerIdentity(ctx)
	if id == "" || !lim.allow(id, s.opts.Now()) {
		if ok, n := s.limited.allow(s.opts.Now()); ok {
			s.opts.Log.Info("no signed answer: the Provider is above its answer rate", "suppressedSinceLast", n)
		}
		return nil
	}
	c := s.opts.Credentials.Load()
	state, reason := s.opts.Agent.TrustState()
	h := answer.Host{
		Nonce: nonce, HostID: c.Identity.HostID(), Record: c.Identity.Record,
		State: state, StateReason: reason, AgentVersion: s.opts.Version,
		JournalSchemaVersion: s.opts.JournalSchemaVersion, ClockFloor: s.opts.Agent.ClockFloor(), Now: s.opts.Now(),
		InventoryRequested: inventoryVersion, InventoryMaxBytes: s.opts.InventoryMaxBytes,
	}
	if inventoryVersion != 0 && s.opts.Inventory != nil {
		h.Inventory = s.opts.Inventory.Inventory(inventoryVersion)
	}
	if s.opts.Authority != nil {
		h.Attestation, h.Attested = s.opts.Authority.Attestation()
		h.Backlog = s.opts.Authority.Backlog()
	}
	if s.opts.Renewals != nil {
		h.StagedCertificateSHA256 = s.opts.Renewals.StagedSHA256()
		if renewalVersion != 0 {
			h.RenewalRequested = renewalVersion
			h.PendingRenewal = s.opts.Renewals.PendingRequest(ctx, renewalVersion)
		}
	}
	return s.signWith(c, h, build)
}

// signWith builds the answer for h with build and signs it. An inventory that
// cannot be signed is replaced, never signed as it is: by an OVER_LIMIT
// inventory that lists nothing when the size is the problem, by an
// UNREADABLE (probe_failed) one when it breaks a field rule, and by none when
// that replacement fails too, so the rest of the answer is still delivered.
func (s *Server) signWith(c *Credentials, h answer.Host, build func(answer.Host) (proto.Message, error)) *providerv1.SignedBlob {
	for attempt := 0; attempt < 3; attempt++ {
		body, err := build(h)
		if err == nil {
			var blob *providerv1.SignedBlob
			if blob, err = answer.Sign(c.Identity.Signer, c.Identity.Leaf.Raw, body); err == nil {
				return blob
			}
		}
		switch {
		case errors.Is(err, answer.ErrNoAnswer):
			s.opts.Log.V(1).Info("no signed answer", "why", err.Error())
			return nil
		case h.Inventory != nil && (errors.Is(err, answer.ErrInvalidInventory) || errors.Is(err, inventory.ErrTooLarge)):
			s.logNoAnswer(err, "the host inventory cannot be signed; replacing it")
			switch {
			case attempt >= 1:
				h.Inventory = nil
			case errors.Is(err, inventory.ErrTooLarge):
				h.Inventory = inventory.OverLimit(h.InventoryRequested, inventory.ReasonInventoryTooLarge)
			default:
				h.Inventory = inventory.Unreadable(h.InventoryRequested, inventory.ReasonProbeFailed)
			}
		default:
			// A signer that is away or refusing fails every answer; a store
			// that cannot be read, or a field that breaks the answer rules,
			// gives no answer either.
			s.logNoAnswer(err, "no signed answer")
			return nil
		}
	}
	return nil
}

// peerIdentity is the key of the verified caller for the answer limit: the
// URI names of its client certificate (the Provider identity); empty when it
// has none, which fails closed (no signed answer).
func peerIdentity(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return ""
	}
	key := ""
	for _, u := range info.State.PeerCertificates[0].URIs {
		key += u.String() + " "
	}
	return key
}

// logNoAnswer logs why an answer could not be made, one line per interval
// with the count held back: a signer that is away fails every answer, and a
// log line per request would let any caller fill the journal.
func (s *Server) logNoAnswer(err error, msg string) {
	if ok, n := s.noAnswer.allow(s.opts.Now()); ok {
		// A class, never the error's text: it can name a certificate subject, a
		// component or a path.
		s.opts.Log.Error(nil, msg, "class", errorClass(err), "suppressedSinceLast", n)
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

// errorClass names the kind of an answer failure for a log line.
func errorClass(err error) string {
	switch {
	case errors.Is(err, answer.ErrNoAnswer):
		return "no_answer"
	case errors.Is(err, inventory.ErrTooLarge):
		return "inventory_too_large"
	case errors.Is(err, answer.ErrInvalidInventory):
		return "invalid_inventory"
	case errors.Is(err, signer.ErrRateLimited):
		return "signer_rate_limited"
	case errors.Is(err, signer.ErrPeer):
		return "signer_peer_refused"
	case errors.Is(err, signer.ErrUnavailable):
		return "signer_unavailable"
	case errors.Is(err, signer.ErrRefused):
		return "signer_refused"
	case errors.Is(err, answer.ErrSign):
		return "sign"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	}
	return inventory.ErrorClass(err)
}
