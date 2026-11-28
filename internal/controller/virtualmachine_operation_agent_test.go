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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	grpcClient "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

// These tests run the VirtualMachine controller against real per-host agent
// processes (test/conformance/clustered/agenttarget) behind the real
// clustered cloudhypervisor Provider, through the real operation-identity
// transport client: every outcome the manager acts on is one the agent
// signed. A gRPC front in front of the Provider's own server plays a lying
// Provider.

const agentHost = "host-a"

// lyingProvider is a front for the Target's Provider: it forwards every call
// the manager makes to the Provider's own gRPC server (whose interceptor
// records the task references FinishTasks settles), and each flag makes it
// lie in one way.
type lyingProvider struct {
	providerv1.UnimplementedProviderServer
	inner providerv1.ProviderClient
	mu    sync.Mutex
	// strip removes every signed answer the agent returned.
	strip bool
	// retired rewrites every TaskStatus as an unsigned RETIRED.
	retired bool
	// dropCreateReply delivers a Create and loses its reply.
	dropCreateReply bool
	// corruptGrant flips a bit of every grant signature.
	corruptGrant bool
	// dropCreate loses the next Create before it reaches the agent.
	dropCreate bool
}

func (l *lyingProvider) flags() (strip, retired, drop, corrupt bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.strip, l.retired, l.dropCreateReply, l.corruptGrant
}

func (l *lyingProvider) set(f func(l *lyingProvider)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l)
}

// stripStatus removes the signed answer details of an error status.
func stripStatus(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	pst := st.Proto()
	var keep []*anypb.Any
	for _, d := range pst.GetDetails() {
		if !strings.HasSuffix(d.GetTypeUrl(), "provider.v1.SignedBlob") {
			keep = append(keep, d)
		}
	}
	pst.Details = keep
	return status.FromProto(pst).Err()
}

func (l *lyingProvider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return l.inner.GetCapabilities(ctx, req)
}

func (l *lyingProvider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	return l.inner.Delete(ctx, req)
}

func (l *lyingProvider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	return l.inner.Power(ctx, req)
}

func (l *lyingProvider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	return l.inner.Reconfigure(ctx, req)
}

func (l *lyingProvider) ListHosts(ctx context.Context, req *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	return l.inner.ListHosts(ctx, req)
}

func (l *lyingProvider) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	strip, _, _, _ := l.flags()
	resp, err := l.inner.GetHostInfo(ctx, req)
	if strip && err == nil {
		resp.HostAnswer = nil
	}
	return resp, err
}

func (l *lyingProvider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	strip, retired, _, _ := l.flags()
	resp, err := l.inner.TaskStatus(ctx, req)
	if retired {
		return &providerv1.TaskStatusResponse{Done: true, Outcome: providerv1.TaskOutcome_TASK_OUTCOME_RETIRED,
			OutcomeReason: "never_received", Error: "not performed: RETIRED{never_received}"}, nil
	}
	if strip {
		if err != nil {
			return nil, stripStatus(err)
		}
		resp.HostAnswer = nil
	}
	return resp, err
}

func (l *lyingProvider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	strip, _, _, _ := l.flags()
	resp, err := l.inner.Describe(ctx, req)
	if strip {
		if err != nil {
			return nil, stripStatus(err)
		}
		resp.HostAnswer = nil
		resp.Exists = false
	}
	return resp, err
}

func (l *lyingProvider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	_, _, drop, corrupt := l.flags()
	l.mu.Lock()
	lose := l.dropCreate
	l.dropCreate = false
	l.mu.Unlock()
	if lose {
		return nil, status.Error(codes.Unavailable, "the request was lost")
	}
	if corrupt && req.GetOperationEnvelope().GetGrant() != nil {
		req = proto.CloneOf(req)
		sig := req.OperationEnvelope.Grant.Signature
		sig[len(sig)-1] ^= 1
	}
	resp, err := l.inner.Create(ctx, req)
	if drop && err == nil {
		l.set(func(l *lyingProvider) { l.dropCreateReply = false })
		return nil, status.Error(codes.Unavailable, "the reply was lost")
	}
	return resp, err
}

// agentEnv is one VM of a clustered cloudhypervisor Provider whose host
// host-a is a real agent process.
type agentEnv struct {
	tg  *agenttarget.Target
	lie *lyingProvider
	r   *VirtualMachineReconciler
	// provider, cli, trust, ledger and enrol are the manager side of the
	// host (ledger nil without host ledger keys).
	provider *infravirtrigaudiov1beta1.Provider
	cli      *grpcClient.Client
	trust    *staticTrust
	ledger   *HostLedger
	enrol    []byte
	req      ctrl.Request
	host     *agenttarget.Host
	// auth is the lying front of the host authority relay.
	auth *lyingAuthority
}

// agentTLS is a server and client TLS configuration for 127.0.0.1.
func agentTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "agent-test-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13},
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
}

// lyingAuthority forwards the host authority service of the Target's
// Provider, unchanged unless dropMarks is set.
type lyingAuthority struct {
	providerv1.UnimplementedHostAuthorityServer
	inner providerv1.HostAuthorityClient
	// dropMarks loses every ledger mark before it reaches the agent.
	dropMarks atomic.Bool
	// marks counts the ledger marks the agent applied.
	marks atomic.Int32
}

func (l *lyingAuthority) Challenge(ctx context.Context, req *providerv1.ChallengeRequest) (*providerv1.ChallengeResponse, error) {
	return l.inner.Challenge(ctx, req)
}

func (l *lyingAuthority) Attest(ctx context.Context, req *providerv1.AttestRequest) (*providerv1.AuthorityResponse, error) {
	return l.inner.Attest(ctx, req)
}

func (l *lyingAuthority) LedgerMark(ctx context.Context, req *providerv1.LedgerMarkRequest) (*providerv1.AuthorityResponse, error) {
	if l.dropMarks.Load() {
		return nil, status.Error(codes.Unavailable, "the ledger mark was lost")
	}
	resp, err := l.inner.LedgerMark(ctx, req)
	if err == nil {
		l.marks.Add(1)
	}
	return resp, err
}

func (l *lyingAuthority) RetireIfUnknown(ctx context.Context, req *providerv1.RetireIfUnknownRequest) (*providerv1.AuthorityResponse, error) {
	return l.inner.RetireIfUnknown(ctx, req)
}

// newAgentEnv is one VM on a real agent whose attestation and ledger marks
// are the manager's own: the manager holds host ledger keys, attests the
// agent through the Provider's host authority relay (the Target never
// does), signs grant version 2 once it is attested, and raises the agent's
// ledger mark over each operation it accepted.
func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	return newAgentEnvWith(t, true)
}

// newAgentEnvAny is one VM on a real agent whose manager holds no host
// ledger keys (the Target attests the agent itself).
func newAgentEnvAny(t *testing.T) *agentEnv {
	t.Helper()
	return newAgentEnvWith(t, false)
}

func newAgentEnvWith(t *testing.T, managerLedger bool) *agentEnv {
	t.Helper()
	tg, err := agenttarget.New(t, agenttarget.Config{Hosts: []string{agentHost}, ManagerLedger: managerLedger})
	require.NoError(t, err)
	cfg := tg.OperationIdentity()
	// The Provider's own gRPC server (its interceptor records the task
	// references FinishTasks settles), on loopback; the lying front forwards
	// to it.
	providerSrv := tg.NewGRPCServer()
	providerLis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = providerSrv.Serve(providerLis) }()
	t.Cleanup(providerSrv.Stop)
	providerConn, err := grpc.NewClient(providerLis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = providerConn.Close() })
	lie := &lyingProvider{inner: providerv1.NewProviderClient(providerConn)}
	serverTLS, clientTLS := agentTLS(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	providerv1.RegisterProviderServer(srv, lie)
	auth := &lyingAuthority{inner: providerv1.NewHostAuthorityClient(providerConn)}
	providerv1.RegisterHostAuthorityServer(srv, auth)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	ctx := context.Background()
	cli, err := grpcClient.NewOperationIdentityClient(ctx, lis.Addr().String(), string(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor),
		"prov-cluster", nil, &grpcClient.TLSConfig{PrebuiltConfig: clientTLS}, cfg.Signer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	ns := trusttest.HostNS
	vm := clusterVM("vm-id", ns, "prov-cluster")
	vm.UID = "a1b2c3d4-0000-4000-8000-00000000000a"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", ns))
	providerCR.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	providerCR.UID = "0b7a1c3e-1111-4222-8333-944455556666"
	require.Equal(t, cfg.ProviderUID, string(providerCR.UID), "the agents are enrolled with this Provider")
	pool := hostPoolCR("pool-a", ns, providerCR.Name)
	host := readyHost(agentHost, ns, pool.Name, providerCR.Name)
	host.Spec.Identity = "serial-" + agentHost
	host.UID = "5e1f0c2a-2222-4333-8444-955566667777"
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: ownerFilterClient{Client: cli}}, vm, providerCR, pool, host,
		smallVMClass(ns), minimalVMImage(ns))
	r.OperationIntentAuth = operation.IntentAuthenticatorOf(cfg.Signer)

	h := tg.Host(agentHost)
	certPEM, err := os.ReadFile(filepath.Join(h.StateDir(), config.PKIDirName, trust.FileHostCert)) // #nosec G304 -- the test's own file
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pin, err := hostidentity.SPKIPin(leaf.PublicKey)
	require.NoError(t, err)
	uri, err := hostidentity.HostURI(trusttest.TrustDomain, ns, agentHost)
	require.NoError(t, err)
	trustSrc := &staticTrust{trusts: map[string]HostTrust{agentHost: {
		Enrolment: operation.Enrolment{EnrolmentID: cfg.Enrolments[agentHost], Epoch: cfg.Epoch},
		InstallID: cfg.Signer.InstallID(), Pins: []string{pin}, Anchors: tg.HostAnswerRoots(), HostURI: uri, HostName: agentHost,
		HostUID: string(host.UID),
	}}}
	r.HostAnswers = &HostAnswers{Trust: trustSrc}
	_, _, verdict, err := r.HostAnswers.RefreshHostAnswer(ctx, providerCR, cli, agentHost)
	require.NoError(t, err)
	require.Equal(t, answerVerified, verdict, "the real agent's host answer verifies")
	if managerLedger {
		// A ledger object names the floor of the manager that may read it;
		// the test build stands for a release above it.
		prev := buildVersion
		buildVersion = func() string { return "v0.5.0" }
		t.Cleanup(func() { buildVersion = prev })
		keys, err := NewHostLedgerKeys(randomLedgerKey(t), nil, randomLedgerKey(t))
		require.NoError(t, err)
		ledger := NewHostLedger(r.Client, r.Client, trustSrc, keys,
			&HostEnrolmentConfig{Namespace: "virtrigaud-enrolment", InstallID: cfg.Signer.InstallID()}, nil)
		r.HostAnswers.AttachLedger(ledger)
		r.HostAnswers.AttestRetryInterval = time.Millisecond
		// The manager attests the agent, as its Host heartbeat would.
		require.Eventually(t, func() bool {
			r.HostAnswers.DriveHostLedger(ctx, providerCR, cli, agentHost)
			_, err := r.HostAnswers.grantReady(providerCR, agentHost, cfg.Enrolments[agentHost])
			return err == nil
		}, 20*time.Second, 50*time.Millisecond, "the manager attests the agent and signs grant version 2 for it")
		rec, err := ledger.Load(ctx, providerCR, agentHost, r.HostAnswers.ledgerOf(providerCR, agentHost).store)
		require.NoError(t, err)
		require.Equal(t, infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested, rec.AttestationState)
	}
	env := &agentEnv{tg: tg, lie: lie, r: r, req: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}, host: h,
		provider: providerCR, cli: cli, trust: trustSrc, enrol: cfg.Enrolments[agentHost], auth: auth}
	env.ledger = r.HostAnswers.Ledger
	return env
}

// step reconciles once and lets every operation the agent started finish.
func (e *agentEnv) step(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := e.r.Reconcile(context.Background(), e.req)
		require.NoError(t, err)
		require.NoError(t, e.tg.FinishTasks())
	}
}

// vm reads the VM (nil once it is gone).
func (e *agentEnv) vm(t *testing.T) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := e.r.Get(context.Background(), e.req.NamespacedName, vm); err != nil {
		require.True(t, client.IgnoreNotFound(err) == nil, "get VM: %v", err)
		return nil
	}
	return vm
}

// createOps counts the distinct Create op_ids the agent ran a step of.
func (e *agentEnv) createOps(t *testing.T) int {
	t.Helper()
	effects, err := e.host.Effects()
	require.NoError(t, err)
	ops := map[string]bool{}
	for _, ef := range effects {
		if ef.Step == "define" { // the Create's step on the development executor
			ops[ef.OpID] = true
		}
	}
	return len(ops)
}

func (e *agentEnv) waitCreated(t *testing.T) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	skew := time.Duration(0)
	defer func() { e.r.clock = nil }()
	for i := 0; i < 20; i++ {
		e.step(t, 1)
		vm := e.vm(t)
		if vm != nil && vm.Status.ID != "" {
			return vm
		}
		if vm != nil && readyReason(vm) == k8s.ReasonOperationRetryBackoff {
			// A not-performed attempt (for example one the host retired
			// because it never received it) is followed by a new attempt
			// after its backoff: the manager's clock moves past it.
			skew += blockedRetryMax
			at := skew
			e.r.clock = func() time.Time { return time.Now().Add(at) }
		}
	}
	vm := e.vm(t)
	t.Fatalf("the Create did not succeed: %+v (ready %s)", vm.Status.OperationAttempts, readyReason(vm))
	return nil
}

func (e *agentEnv) markDeleted(t *testing.T) {
	t.Helper()
	require.NoError(t, e.r.Delete(context.Background(), e.vm(t)))
}

// A manager without host ledger keys signs no operation for an agent that
// reports grant versions [2] (the host ledger protocol): it never signs
// version 2 without a ledger, so the agent executes nothing and the VM
// waits.
func TestAgentIdentity_ManagerSignsNothingForALedgerAgent(t *testing.T) {
	e := newAgentEnvAny(t)
	e.step(t, 8)
	assert.Zero(t, e.createOps(t), "no Create executed on an agent the manager cannot sign for")
	vm := e.vm(t)
	require.NotNil(t, vm)
	assert.Empty(t, vm.Status.ID, "the VM was not created")
	for _, a := range vm.Status.OperationAttempts {
		assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, a.Outcome)
	}
}

// A Create and its Delete run to their end on the agent's signed answers:
// exactly one Create and one Delete are executed, and the finalizer is
// released only once the agent signed the Delete.
func TestAgentIdentity_CreateAndDeleteOnSignedAnswers(t *testing.T) {
	e := newAgentEnv(t)
	vm := e.waitCreated(t)
	c := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, c)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, c.Outcome)
	assert.Equal(t, 1, e.createOps(t), "exactly one Create executed")
	e.markDeleted(t)
	for i := 0; i < 20 && e.vm(t) != nil; i++ {
		e.step(t, 1)
	}
	assert.Nil(t, e.vm(t), "released on the agent's signed Delete")
	assert.Equal(t, 1, e.createOps(t))
}

// A Create sent at grant version 2 comes back with the agent's receipt: the
// attempt is Accepted with the receipt recorded and bound to it, prepared
// under the ledger its intent records, the ledger covers the receipt, and
// the agent runs the operation exactly once.
func TestAgentIdentity_ReceiptAcceptedAndLedgered(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, a.Phase, "the receipt moved the attempt to Accepted")
	require.NotNil(t, a.Receipt)
	assert.True(t, e.r.receiptBound(vm, a), "the recorded receipt is bound to the attempt")
	assert.Positive(t, a.LedgerGeneration)
	provider := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, e.r.Get(ctx, client.ObjectKey{Namespace: trusttest.HostNS, Name: "prov-cluster"}, provider))
	p, found, err := e.r.loadIntent(ctx, vm, a.OpID, provider.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	rec, err := e.r.HostAnswers.Ledger.Load(ctx, provider, agentHost, [16]byte(p.LedgerStoreID))
	require.NoError(t, err)
	assert.True(t, ledgerContinuous(p, rec), "the intent records the ledger it was prepared under")
	assert.EqualValues(t, rec.Generation, a.LedgerGeneration)
	assert.GreaterOrEqual(t, int64(rec.Position.Seq), a.Receipt.Seq, "the ledger covers the receipt")
	assert.True(t, receiptLedgered(rec, a.Receipt))
	e.waitCreated(t)
	assert.Equal(t, 1, e.createOps(t))
}

// driveReady drives the host's ledger once and reports whether grant
// version 2 may be signed for it.
func (e *agentEnv) driveReady() bool {
	e.r.HostAnswers.DriveHostLedger(context.Background(), e.provider, e.cli, agentHost)
	_, err := e.r.HostAnswers.grantReady(e.provider, agentHost, e.enrol)
	return err == nil
}

// driveReady is paced by its callers below the agent's signed answer rate
// (ten a second).
//
// A ledger that is archived never makes its host ready for grant version 2,
// even right after a heartbeat raise (the raise is throttled and returns a
// read): the ready check reads the ledger bound and not archived, whatever
// the throttle or the token buckets say.
func TestAgentIdentity_LedgerArchivedStoreNotReady(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	store := e.r.HostAnswers.ledgerOf(e.provider, agentHost).store
	lh, err := e.ledger.resolve(ctx, e.provider, agentHost)
	require.NoError(t, err)
	_, err = e.ledger.archive(ctx, lh, store, [16]byte{0xff})
	require.NoError(t, err)
	require.Never(t, e.driveReady, time.Second, 300*time.Millisecond, "an archived ledger never makes the host ready")
	_, ok := e.r.HostAnswers.sendVersion(e.provider, agentHost)
	assert.False(t, ok, "nothing is sent to the host")
	_, err = e.ledger.LoadBound(ctx, e.provider, agentHost, store)
	require.ErrorIs(t, err, ErrHostLedgerArchived)
}

// A Host replaced by a new object of the same name (the agent keeps its
// session): the ledger bound to the earlier object never makes the new one
// ready, and moves to it only once the rebind conditions hold (the earlier
// object gone and quiet for the answer lifetime); the agent is then
// attested again and the new Host becomes ready.
func TestAgentIdentity_LedgerReplacedHostRebinds(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	store := e.r.HostAnswers.ledgerOf(e.provider, agentHost).store
	old := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, e.r.Get(ctx, client.ObjectKey{Namespace: trusttest.HostNS, Name: agentHost}, old))
	oldUID := string(old.UID)
	old.Finalizers = nil
	require.NoError(t, e.r.Update(ctx, old))
	require.NoError(t, e.r.Delete(ctx, old))
	replacement := old.DeepCopy()
	replacement.ResourceVersion, replacement.DeletionTimestamp = "", nil
	replacement.UID = "5e1f0c2a-2222-4333-8444-955566668888"
	require.NoError(t, e.r.Create(ctx, replacement))
	// The pins Secret of a new Host object starts afresh (no ledger index).
	e.trust.mu.Lock()
	tr := e.trust.trusts[agentHost]
	tr.HostUID = string(replacement.UID)
	e.trust.trusts[agentHost] = tr
	delete(e.trust.stores, agentHost)
	e.trust.mu.Unlock()

	require.Never(t, e.driveReady, time.Second, 300*time.Millisecond,
		"a ledger bound to the earlier Host object never makes the replacement ready")
	rec, err := e.ledger.Load(ctx, e.provider, agentHost, store)
	require.NoError(t, err)
	assert.Equal(t, oldUID, rec.HostUID, "no rebind while the earlier object was released less than the answer lifetime ago")

	e.ledger.Now = func() time.Time { return time.Now().Add(2 * hostAnswerTTL) }
	require.Eventually(t, e.driveReady, 20*time.Second, 300*time.Millisecond, "the replacement is attested and ready after the rebind")
	rec, err = e.ledger.LoadBound(ctx, e.provider, agentHost, store)
	require.NoError(t, err)
	assert.Equal(t, string(replacement.UID), rec.HostUID)
	assert.Equal(t, infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested, rec.AttestationState)
}

// A Create whose reply (and receipt) is lost: the agent's verified answer
// that it accepted the operation, in the ledger's adopted session, is
// recorded on the marker as its receipt (Accepted, tagged) before the
// ledger and the mark are raised over it, so a re-send then sets
// expect_known; the agent runs the Create exactly once.
func TestAgentIdentity_LostReceiptRecordedFromAnswer(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	e.lie.set(func(l *lyingProvider) { l.dropCreateReply = true })
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, a.Phase, "the accepted answer stands for the lost receipt")
	require.NotNil(t, a.Receipt)
	assert.True(t, e.r.receiptBound(vm, a))
	p, found, err := e.r.loadIntent(ctx, vm, a.OpID, e.provider.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	assert.EqualValues(t, p.LedgerGeneration, a.LedgerGeneration, "the display generation mirrors the intent's")
	ia := identityAttempt{providerCR: e.provider}
	assert.True(t, e.r.expectKnown(ctx, vm, ia, a, p), "a re-send expects the agent to know the operation")
	e.waitCreated(t)
	assert.Equal(t, 1, e.createOps(t))
}

// A receipt is recorded only for the host's current store, bound to this
// Host object, not archived, in the ledger's adopted session.
func TestAgentIdentity_ReceiptOnlyOnTheBoundCurrentLedger(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	ia := identityAttempt{providerCR: e.provider, reader: e.cli}
	st := e.r.HostAnswers.ledgerOf(e.provider, agentHost)
	p := contracts.PreparedOperation{LedgerGeneration: st.generation}
	good := hostidentity.VerifiedReceipt{StoreID: st.store, Seq: 1, AttestationID: st.session}
	require.NotNil(t, e.r.receiptOnLedger(ctx, ia, agentHost, good, p))
	otherSession := good
	otherSession.AttestationID = [16]byte{1}
	assert.Nil(t, e.r.receiptOnLedger(ctx, ia, agentHost, otherSession, p), "another session")
	otherStore := good
	otherStore.StoreID = [16]byte{2}
	assert.Nil(t, e.r.receiptOnLedger(ctx, ia, agentHost, otherStore, p), "another store")
	lh, err := e.ledger.resolve(ctx, e.provider, agentHost)
	require.NoError(t, err)
	_, err = e.ledger.archive(ctx, lh, st.store, [16]byte{0xff})
	require.NoError(t, err)
	assert.Nil(t, e.r.receiptOnLedger(ctx, ia, agentHost, good, p), "an archived ledger")
}

// For a host the host ledger drives, an operation answer from a session
// other than the ledger's adopted one (the agent restarted) is no answer;
// once the agent is attested again, its answers count.
func TestAgentIdentity_AnswerOutsideAdoptedSessionIsNoAnswer(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	p, found, err := e.r.loadIntent(ctx, vm, a.OpID, e.provider.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, e.host.Stop())
	_, _ = e.host.Wait(10 * time.Second)
	require.NoError(t, e.host.Restart())
	ia := identityAttempt{providerCR: e.provider, reader: e.cli}
	poll := e.r.pollSigned(ctx, vm, ia, refOf(p))
	assert.Equal(t, answerNone, poll.verdict, "the restarted agent's session is not the adopted one")
	require.Eventually(t, func() bool {
		return e.r.pollSigned(ctx, vm, ia, refOf(p)).verdict == answerVerified
	}, 20*time.Second, 300*time.Millisecond, "answers count again once the agent is attested again")
}

// A lying Provider that strips the agent's answers and reports an unsigned
// RETIRED never makes the manager mint a second attempt.
func TestAgentIdentity_LyingProviderNoSecondAttempt(t *testing.T) {
	e := newAgentEnv(t)
	e.lie.set(func(l *lyingProvider) { l.retired, l.strip = true, true })
	now := time.Now()
	e.r.clock = func() time.Time { return now }
	for i := 0; i < 6; i++ {
		e.step(t, 1)
		now = now.Add(blockedRetryMax)
	}
	vm := e.vm(t)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.Equal(t, int64(1), a.Attempt, "no second attempt")
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase, "never closed on an unsigned outcome")
	assert.LessOrEqual(t, e.createOps(t), 1, "at most one Create executed")

	// Once the Provider relays the agent's answers again, the attempt ends
	// on the agent's signed COMPLETED.
	e.lie.set(func(l *lyingProvider) { l.retired, l.strip = false, false })
	e.r.clock = nil
	vm = e.waitCreated(t)
	assert.Equal(t, int64(1), findAttempt(vm, contracts.OperationKindCreate).Attempt)
	assert.Equal(t, 1, e.createOps(t))
}

// A Delete is never released on an unsigned NotFound or with the agent's
// answers stripped; it is once they are relayed.
func TestAgentIdentity_DeleteReleasedOnlyOnSignedAnswer(t *testing.T) {
	e := newAgentEnv(t)
	e.waitCreated(t)
	e.lie.set(func(l *lyingProvider) { l.strip = true })
	e.markDeleted(t)
	e.step(t, 5)
	require.NotNil(t, e.vm(t), "no release without a signed answer")
	e.lie.set(func(l *lyingProvider) { l.strip = false })
	for i := 0; i < 10 && e.vm(t) != nil; i++ {
		e.step(t, 1)
	}
	assert.Nil(t, e.vm(t))
}

// A re-delivery refused with GRANT_UNAUTHENTICATED (a grant that does not
// verify) never ends the attempt: the agent is asked about the op_id, and
// its signed COMPLETED closes it Succeeded, with exactly one Create
// executed.
func TestAgentIdentity_RedeliveryRefusalNeverRejects(t *testing.T) {
	e := newAgentEnv(t)
	e.lie.set(func(l *lyingProvider) { l.dropCreateReply = true })
	e.step(t, 1)
	e.lie.set(func(l *lyingProvider) { l.corruptGrant = true })
	e.step(t, 3)
	a := findAttempt(e.vm(t), contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected, a.Outcome, "never closed Rejected")
	assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase)
	e.lie.set(func(l *lyingProvider) { l.corruptGrant = false })
	vm := e.waitCreated(t)
	c := findAttempt(vm, contracts.OperationKindCreate)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, c.Outcome)
	assert.Equal(t, int64(1), c.Attempt)
	assert.Equal(t, 1, e.createOps(t), "exactly one Create executed")
}

// Each field of a Create marker in flight edited by a tenant leads to at
// most one Create executed on the agent: a new op_id needs a grant the
// manager signs only for a new attempt, and a new attempt follows only a
// signed not-performed outcome.
func TestAgentIdentity_AdversarialMarkerEdits(t *testing.T) {
	edits := map[string]func(a *infravirtrigaudiov1beta1.OperationAttempt){
		"op_id":   func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.OpID = strings.Repeat("ab", 16) },
		"attempt": func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Attempt++ },
		"binding": func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.BindingID = strings.Repeat("cd", 16) },
		"host":    func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.HostID = "host-b" },
		"phase back": func(a *infravirtrigaudiov1beta1.OperationAttempt) {
			a.Phase = infravirtrigaudiov1beta1.OperationAttemptPhasePrepared
		},
		"phase forward to the reserved Accepted": func(a *infravirtrigaudiov1beta1.OperationAttempt) {
			a.Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
			a.Receipt = &infravirtrigaudiov1beta1.OperationAttemptReceipt{
				StoreID: strings.Repeat("1", 32), Seq: 1, Head: strings.Repeat("2", 64),
				AttestationID: strings.Repeat("3", 32), ReceiptTag: strings.Repeat("4", 64),
			}
		},
		"task tag":    func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.TaskTag = strings.Repeat("00", 32) },
		"task ref":    func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.TaskRef = "forged-task" },
		"provider id": func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.ID = "forged-id" },
		"not performed": func(a *infravirtrigaudiov1beta1.OperationAttempt) {
			a.Phase, a.Outcome = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedClean
			a.LastTransitionTime.Time = time.Now().Add(-time.Hour)
		},
		"rejected": func(a *infravirtrigaudiov1beta1.OperationAttempt) {
			a.Phase, a.Outcome = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected
			a.LastTransitionTime.Time = time.Now().Add(-time.Hour)
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			e := newAgentEnv(t)
			_, err := e.r.Reconcile(context.Background(), e.req)
			require.NoError(t, err)
			vm := e.vm(t)
			a := findAttempt(vm, contracts.OperationKindCreate)
			require.NotNil(t, a, "the Create is in flight")
			edit(a)
			vm.Status.OperationAttempts = withAttempt(vm, *a)
			require.NoError(t, e.r.Status().Update(context.Background(), vm))
			e.step(t, 6)
			assert.LessOrEqual(t, e.createOps(t), 1, "at most one Create executed on the agent")
		})
	}
}

// copyTree copies the regular files of from into to (the agent's state,
// its operation store and the anchor that would let it see a rollback).
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(path) // #nosec G304 -- the test's own files
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(filepath.Join(to, rel), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the test's own files
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}))
}

// The agent's store restored together with its anchor (a joint restore the
// agent cannot see) to a point before a position the manager verified holds
// the host: nothing it answers is acted on and nothing new is sent to it.
func TestAgentIdentity_StoreRestoredWithAnchorHoldsHost(t *testing.T) {
	e := newAgentEnv(t)
	snapshot := t.TempDir()
	require.NoError(t, e.host.Stop())
	_, _ = e.host.Wait(10 * time.Second)
	copyTree(t, e.host.StateDir(), snapshot)
	require.NoError(t, e.host.Restart())

	e.waitCreated(t)
	require.Equal(t, 1, e.createOps(t))

	require.NoError(t, e.host.Stop())
	_, _ = e.host.Wait(10 * time.Second)
	require.NoError(t, os.RemoveAll(e.host.StateDir()))
	copyTree(t, snapshot, e.host.StateDir())
	require.NoError(t, e.host.Restart())

	provider := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, e.r.Get(context.Background(), client.ObjectKey{Namespace: trusttest.HostNS, Name: "prov-cluster"}, provider))
	res, ok := e.r.RemoteResolver.(*stubResolver)
	require.True(t, ok)
	reader, ok := res.provider.(contracts.HostAnswerReader)
	require.True(t, ok)
	_, _, verdict, err := e.r.HostAnswers.RefreshHostAnswer(context.Background(), provider, reader, agentHost)
	require.NoError(t, err)
	assert.Equal(t, answerRegressed, verdict, "the restored store is below the position the manager verified")

	e.markDeleted(t)
	e.step(t, 3)
	vm := e.vm(t)
	require.NotNil(t, vm, "nothing a regressed host answers releases the VM")
	assert.Equal(t, k8s.ReasonOperationHostStoreRegressed, readyReason(vm))
	// The restored state holds the executor's log of before the Create (the
	// snapshot), so any line in it now is an operation the regressed host
	// ran.
	effects, err := e.host.Effects()
	require.NoError(t, err)
	assert.Empty(t, effects, "nothing is executed on a regressed host")
}
