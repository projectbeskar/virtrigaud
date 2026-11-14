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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
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
// processes (test/conformance/clustered/agenttarget) behind the in-test
// router standing in for the clustered Provider, through the real
// operation-identity transport client: every outcome the manager acts on is
// one the agent signed. A wrapper around the router plays a lying Provider.

const agentHost = "host-a"

// lyingProvider wraps the router: each flag makes it lie in one way. It also
// forwards GetHostInfo to the host's agent, which the in-test router does
// not route.
type lyingProvider struct {
	providerv1.ProviderServer
	tg *agenttarget.Target
	mu sync.Mutex
	// strip removes every signed answer the agent returned.
	strip bool
	// retired rewrites every TaskStatus as an unsigned RETIRED.
	retired bool
	// dropCreateReply delivers a Create and loses its reply.
	dropCreateReply bool
	// corruptGrant flips a bit of every grant signature.
	corruptGrant bool
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

func (l *lyingProvider) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	h := l.tg.Host(req.GetHostId())
	if h == nil {
		return nil, status.Error(codes.NotFound, "no such host")
	}
	strip, _, _, _ := l.flags()
	resp, err := h.Client().GetHostInfo(ctx, req)
	if strip && err == nil {
		resp.HostAnswer = nil
	}
	return resp, err
}

func (l *lyingProvider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	strip, retired, _, _ := l.flags()
	resp, err := l.ProviderServer.TaskStatus(ctx, req)
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
	resp, err := l.ProviderServer.Describe(ctx, req)
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
	if corrupt && req.GetOperationEnvelope().GetGrant() != nil {
		req = proto.CloneOf(req)
		sig := req.OperationEnvelope.Grant.Signature
		sig[len(sig)-1] ^= 1
	}
	resp, err := l.ProviderServer.Create(ctx, req)
	if drop && err == nil {
		l.set(func(l *lyingProvider) { l.dropCreateReply = false })
		return nil, status.Error(codes.Unavailable, "the reply was lost")
	}
	return resp, err
}

// agentEnv is one VM of a clustered cloudhypervisor Provider whose host
// host-a is a real agent process.
type agentEnv struct {
	tg   *agenttarget.Target
	lie  *lyingProvider
	r    *VirtualMachineReconciler
	req  ctrl.Request
	host *agenttarget.Host
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

func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	tg, err := agenttarget.New(t, agenttarget.Config{Hosts: []string{agentHost}})
	require.NoError(t, err)
	cfg := tg.OperationIdentity()
	lie := &lyingProvider{ProviderServer: tg.Server(), tg: tg}
	serverTLS, clientTLS := agentTLS(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	providerv1.RegisterProviderServer(srv, lie)
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
	}}}
	r.HostAnswers = &HostAnswers{Trust: trustSrc}
	_, _, verdict, err := r.HostAnswers.RefreshHostAnswer(ctx, providerCR, cli, agentHost)
	require.NoError(t, err)
	require.Equal(t, answerVerified, verdict, "the real agent's host answer verifies")
	return &agentEnv{tg: tg, lie: lie, r: r, req: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}, host: h}
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
	for i := 0; i < 20; i++ {
		e.step(t, 1)
		if vm := e.vm(t); vm != nil && vm.Status.ID != "" {
			return vm
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
