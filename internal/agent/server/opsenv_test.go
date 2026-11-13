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
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// testExec is an in-memory executor: its VMs are names, every effect is
// counted, and a gate can hold every step.
type testExec struct {
	mu      sync.Mutex
	effects map[string]int
	vms     map[string]bool
	running map[string]bool
	gate    chan struct{}
}

func newTestExec() *testExec {
	return &testExec{effects: map[string]int{}, vms: map[string]bool{}, running: map[string]bool{}}
}

func (x *testExec) Plan(k providerv1.OperationKind) ([]executor.Step, error) {
	switch k {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		return []executor.Step{{Name: "define", Once: true}, {Name: "start"}}, nil
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		return []executor.Step{{Name: "destroy", Irreversible: true}}, nil
	case providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
		providerv1.OperationKind_OPERATION_KIND_REBOOT, providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL:
		return []executor.Step{{Name: "power"}}, nil
	case providerv1.OperationKind_OPERATION_KIND_RECONFIGURE:
		return []executor.Step{{Name: "reconfigure"}}, nil
	}
	return nil, executor.ErrUnsupportedKind
}

func (x *testExec) Run(ctx context.Context, op executor.Operation, st executor.Step) error {
	x.mu.Lock()
	gate := x.gate
	x.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("%w: held", executor.ErrNotPerformed)
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.effects[hex.EncodeToString(op.OpID)+"/"+st.Name]++
	switch st.Name {
	case "define":
		x.vms[op.VMUID] = true
	case "start":
		x.running[op.VMUID] = true
	case "destroy":
		delete(x.vms, op.VMUID)
	case "power":
		x.running[op.VMUID] = op.Kind == providerv1.OperationKind_OPERATION_KIND_POWER_ON
	}
	return nil
}

func (x *testExec) Inspect(_ context.Context, op executor.Operation, st executor.Step) (executor.Evidence, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.effects[hex.EncodeToString(op.OpID)+"/"+st.Name] > 0 {
		return executor.EvidencePresent, nil
	}
	return executor.EvidenceAbsent, nil
}

func (x *testExec) Goal(_ context.Context, op executor.Operation, _ executor.Step) (bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.vms[op.VMUID], nil
}

// total is the number of effects of every operation.
func (x *testExec) total() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, c := range x.effects {
		n += c
	}
	return n
}

// opsProxy lets the environment build the server before the store exists.
type opsProxy struct {
	mu    sync.Mutex
	store *opstore.Store
	exec  *testExec
	pool  *runner.Pool
}

func (o *opsProxy) get() (*opstore.Store, *runner.Pool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.store, o.pool
}

func (o *opsProxy) Admit(ctx context.Context, r opstore.Request) (opstore.Decision, error) {
	s, _ := o.get()
	return s.Admit(ctx, r)
}

func (o *opsProxy) AnswerOp(ctx context.Context, ref opstore.OpRef, seq uint64) (opstore.OpAnswerView, error) {
	s, _ := o.get()
	return s.AnswerOp(ctx, ref, seq)
}

func (o *opsProxy) AnswerVM(ctx context.Context, uid string, b []byte, seq uint64) (opstore.VMAnswerView, error) {
	s, _ := o.get()
	return s.AnswerVM(ctx, uid, b, seq)
}

func (o *opsProxy) Supports(k providerv1.OperationKind) bool {
	_, err := o.exec.Plan(k)
	return err == nil
}

func (o *opsProxy) Start(ref opstore.OpRef, payload []byte) {
	_, p := o.get()
	p.Start(ref, payload)
}

func (o *opsProxy) Describe(_ context.Context, uid string) (executor.VMState, bool, error) {
	o.exec.mu.Lock()
	defer o.exec.mu.Unlock()
	return executor.VMState{Exists: o.exec.vms[uid], Running: o.exec.running[uid]}, true, nil
}

// grantSigner is a grant-signing chain under the test PKI's grant anchor.
type grantSigner struct {
	installID []byte
	leafKey   *ecdsa.PrivateKey
	chain     [][]byte
}

var _ operation.Signer = (*grantSigner)(nil)

func (g *grantSigner) InstallID() []byte           { return g.installID }
func (g *grantSigner) AuthorityGeneration() uint64 { return 1 }
func (g *grantSigner) Sign(_ context.Context, body []byte, _ time.Time) (operation.Signature, error) {
	d := sha256.Sum256(body)
	sig, err := g.leafKey.Sign(rand.Reader, d[:], crypto.SHA256)
	if err != nil {
		return operation.Signature{}, err
	}
	if sig, err = grantverify.NormalizeLowS(sig, elliptic.P256()); err != nil {
		return operation.Signature{}, err
	}
	return operation.Signature{Signature: sig, Chain: g.chain}, nil
}

// newGrantSigner issues a path length 0 issuer and a grant-signing leaf
// under the anchor (cert, key).
func newGrantSigner(t testing.TB, anchor *x509.Certificate, anchorKey crypto.Signer, installID []byte) *grantSigner {
	t.Helper()
	mk := func(tmpl *x509.Certificate, parent *x509.Certificate, pub any, key crypto.Signer) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	now := time.Now()
	ik, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	issuer := mk(&x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "grant issuer"},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}, anchor, &ik.PublicKey, anchorKey)
	lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := mk(&x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "grant leaf"},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature},
		issuer, &lk.PublicKey, ik)
	return &grantSigner{installID: installID, leafKey: lk, chain: [][]byte{leaf.Raw, issuer.Raw}}
}

// opsEnv is an agent server with a real operation store and runners.
type opsEnv struct {
	*env
	ops    *opsProxy
	exec   *testExec
	store  *opstore.Store
	dir    string
	grants *grantSigner
	epoch  operation.Epoch
	logs   *logLines
	connMu sync.Mutex
	conn   *grpc.ClientConn
}

// cc is one shared Provider connection: a new connection per call would meet
// the pre-handshake throttle.
func (e *opsEnv) cc() *grpc.ClientConn {
	e.connMu.Lock()
	defer e.connMu.Unlock()
	if e.conn == nil {
		e.conn = e.dial(e.provider(), 0)
	}
	return e.conn
}

// resign mints a grant for o, changes its body and signs it again with the
// same authority: an authentic grant that says something other than o.
func (e *opsEnv) resign(o *op, mutate func(*providerv1.GrantBody)) *providerv1.OperationGrant {
	e.t.Helper()
	g := e.grant(o, time.Now(), 0)
	body := &providerv1.GrantBody{}
	if err := proto.Unmarshal(g.GetBody(), body); err != nil {
		e.t.Fatal(err)
	}
	mutate(body)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	sig, err := e.grants.Sign(context.Background(), b, time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	return &providerv1.OperationGrant{Body: b, Signature: sig.Signature, SignerCertChain: sig.Chain}
}

// logLines captures log output.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(prefix, args string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, prefix+" "+args)
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

const startEpoch = 2

func newOpsEnv(t testing.TB, mods ...withOptions) *opsEnv {
	t.Helper()
	x := newTestExec()
	proxy := &opsProxy{exec: x}
	logs := &logLines{}
	all := append([]withOptions{func(o *server.Options) {
		o.Operations = proxy
		o.Log = funcrLogger(logs)
	}}, mods...)
	e := newEnvWith(t, server.Throttle{}, all...)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: dir}, Executor: x,
		StepDeadlines: map[providerv1.OperationKind]time.Duration{providerv1.OperationKind_OPERATION_KIND_POWER_OFF: 300 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Enrol(ctx, opstore.Enrolment{ID: e.pki.Record.EnrolmentID, ProviderUID: e.pki.Record.ProviderUID,
		HostID: trusttest.HostName, InstallID: e.pki.Record.InstallID, StartEpoch: startEpoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	poolCtx, stop := context.WithCancel(ctx)
	pool := runner.New(poolCtx, st, logr.Discard())
	t.Cleanup(func() {
		stop()
		pool.Wait()
		_ = st.Close()
	})
	proxy.mu.Lock()
	proxy.store, proxy.pool = st, pool
	proxy.mu.Unlock()
	holder := make([]byte, 16)
	_, _ = rand.Read(holder)
	return &opsEnv{env: e, ops: proxy, exec: x, store: st, dir: dir, logs: logs,
		grants: newGrantSigner(t, e.pki.GrantCA.Cert, e.pki.GrantCA.Key, e.pki.Record.InstallID),
		epoch:  operation.Epoch{Counter: startEpoch, Holder: holder}}
}

// op is one signed operation attempt.
type op struct {
	id      operation.Identity
	salt    []byte
	request []byte
	method  string
}

func randID() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// newOp prepares attempt of kind for req on the host, for vm with binding.
func (e *opsEnv) newOp(kind providerv1.OperationKind, vm string, binding []byte, attempt uint64, req proto.Message) *op {
	e.t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		e.t.Fatal(err)
	}
	row, _ := grantverify.RowFor(kind)
	salt := make([]byte, 32)
	_, _ = rand.Read(salt)
	return &op{
		id: operation.Identity{ProviderUID: e.pki.Record.ProviderUID, HostID: trusttest.HostName, EnrolmentID: e.pki.Record.EnrolmentID,
			Epoch: e.epoch, VMUID: vm, BindingID: binding, OpID: randID(), Kind: kind, Attempt: attempt, MintEpoch: e.epoch},
		salt: salt, request: b, method: row.FullMethod,
	}
}

// grant mints a grant for o, issued at issued with lifetime.
func (e *opsEnv) grant(o *op, issued time.Time, lifetime time.Duration) *providerv1.OperationGrant {
	e.t.Helper()
	g, err := operation.MintOperationGrant(context.Background(), e.grants, operation.MintRequest{
		Identity: o.id, DigestSalt: o.salt, Request: o.request, IssuedAt: issued, Lifetime: lifetime})
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

// send sends o with a fresh grant on its method.
func (e *opsEnv) send(o *op) (string, error) {
	e.t.Helper()
	return e.sendEnvelope(o.method, &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)}, nil)
}

// sendEnvelope sends env on method, with outer changing the outer request,
// and returns the task reference.
func (e *opsEnv) sendEnvelope(method string, env *providerv1.OperationEnvelope, outer func(proto.Message)) (string, error) {
	e.t.Helper()
	var req, resp proto.Message
	switch method {
	case providerv1.Provider_Create_FullMethodName:
		req, resp = &providerv1.CreateRequest{}, &providerv1.CreateResponse{}
	case providerv1.Provider_Delete_FullMethodName:
		req, resp = &providerv1.DeleteRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Power_FullMethodName:
		req, resp = &providerv1.PowerRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Reconfigure_FullMethodName:
		req, resp = &providerv1.ReconfigureRequest{}, &providerv1.TaskResponse{}
	default:
		e.t.Fatalf("method %s", method)
	}
	r := req.ProtoReflect()
	r.Set(r.Descriptor().Fields().ByName("operation_envelope"), protoreflect.ValueOfMessage(env.ProtoReflect()))
	if outer != nil {
		outer(req)
	}
	if err := e.cc().Invoke(ctx(e.t), method, req, resp); err != nil {
		return "", err
	}
	switch a := resp.(type) {
	case *providerv1.CreateResponse:
		return a.GetTask().GetId(), nil
	case *providerv1.TaskResponse:
		return a.GetTask().GetId(), nil
	}
	return "", nil
}

// client returns a Provider client.
func (e *opsEnv) client() providerv1.ProviderClient {
	return providerv1.NewProviderClient(e.cc())
}

// waitDone polls TaskStatus for task until it is no longer running.
func (e *opsEnv) waitDone(task string) *providerv1.TaskStatusResponse {
	e.t.Helper()
	c := e.client()
	deadline := time.Now().Add(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := c.TaskStatus(ctx(e.t), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
		if err != nil {
			e.t.Fatalf("TaskStatus: %v", err)
		}
		if st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_RUNNING {
			return st
		}
		if time.Now().After(deadline) {
			e.t.Fatal("the task did not end")
		}
		<-tick.C
	}
}

// ownerOf is an owner identity for uid.
func ownerOf(uid string) *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: uid, Namespace: "ns", Name: "vm"}
}

// createReq is a Create of uid on the host, with userData.
func createReq(uid string, userData []byte) *providerv1.CreateRequest {
	return &providerv1.CreateRequest{Name: "vm", TargetHostId: trusttest.HostName, Owner: ownerOf(uid), UserData: userData}
}

// powerReq is a Power of uid on the host.
func powerReq(uid string, p providerv1.PowerOp) *providerv1.PowerRequest {
	return &providerv1.PowerRequest{Id: uid, Op: p, TargetHostId: trusttest.HostName, Owner: ownerOf(uid)}
}

// deleteReq is a Delete of uid on the host.
func deleteReq(uid string) *providerv1.DeleteRequest {
	return &providerv1.DeleteRequest{Id: uid, TargetHostId: trusttest.HostName, Owner: ownerOf(uid)}
}

// funcrLogger logs into l.
func funcrLogger(l *logLines) logr.Logger {
	return funcr.New(l.add, funcr.Options{Verbosity: 1})
}
