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

package agenttarget

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	clusteredref "github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/cloudhypervisor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
)

// Defaults of a Target.
const (
	// StartEpoch is the epoch counter the hosts are enrolled at.
	StartEpoch = 2
	// DefaultStepDeadline is the hosts' step deadline.
	DefaultStepDeadline = 3 * time.Second
	// settleTimeout bounds FinishTasks.
	settleTimeout = 20 * time.Second
	// pollEvery is how often FinishTasks polls an operation.
	pollEvery = 10 * time.Millisecond
)

// Config configures New.
type Config struct {
	// Hosts are the host ids; empty means host-a and host-b.
	Hosts []string
	// StepDeadline is the hosts' step deadline; zero uses
	// DefaultStepDeadline.
	StepDeadline time.Duration
	// ProviderAPIVersion is the agent API version the Provider speaks; zero
	// uses the Provider's own. A test sets it to stand for another release.
	ProviderAPIVersion uint32
}

// fatalT adapts a clustered.T to the test helpers that need Fatalf.
type fatalT struct{ clustered.T }

func (f fatalT) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	f.FailNow()
}

// Target is a clustered.Target over real per-host agent processes.
type Target struct {
	t          clustered.T
	hosts      []*Host
	byName     map[string]*Host
	signer     *operation.DevSigner
	epoch      operation.Epoch
	hostCA     *hostidentitytest.CA
	placements *clustered.Placements
	deadline   time.Duration

	clientCert tls.Certificate
	apiVersion uint32
	inv        hostsecret.Inventory
	proxies    map[string]*proxy

	mu      sync.Mutex
	prov    *cloudhypervisor.Provider
	tasks   map[string][]string
	scripts map[string]fake.Script
	// seeded maps host/name of a seeded VM to its owner UID.
	seeded map[string]string
}

var (
	_ clustered.Target                  = (*Target)(nil)
	_ clustered.OperationIdentityTarget = (*Target)(nil)
	_ clustered.TaskController          = (*Target)(nil)
	_ clustered.OutcomeScripter         = (*Target)(nil)
	_ clustered.PlacementTracker        = (*Target)(nil)
	_ clustered.HostAnswerTarget        = (*Target)(nil)
	_ clustered.GRPCServerTarget        = (*Target)(nil)
	_ clustered.HostBlackholer          = (*Target)(nil)
	_ clustered.HostRemover             = (*Target)(nil)
)

// New starts one agent process per host, a loopback proxy in front of each,
// and the clustered Provider for per-host agents
// (internal/providers/cloudhypervisor) over an inventory of the proxies, and
// returns the Target. The processes stop when t ends.
func New(t clustered.T, cfg Config) (*Target, error) {
	t.Helper()
	ft := fatalT{t}
	if len(cfg.Hosts) == 0 {
		cfg.Hosts = []string{"host-a", "host-b"}
	}
	if cfg.StepDeadline == 0 {
		cfg.StepDeadline = DefaultStepDeadline
	}
	installID, err := operation.NewRandomID()
	if err != nil {
		return nil, err
	}
	grantKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sg, err := operation.NewDevSigner(grantKey, installID, nil)
	if err != nil {
		return nil, err
	}
	holder, err := operation.NewRandomID()
	if err != nil {
		return nil, err
	}
	tg := &Target{t: t, byName: map[string]*Host{}, signer: sg, epoch: operation.Epoch{Counter: StartEpoch, Holder: holder},
		hostCA: hostidentitytest.NewCA(ft), placements: clustered.NewPlacements(), deadline: cfg.StepDeadline,
		apiVersion: cfg.ProviderAPIVersion, inv: hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion},
		proxies: map[string]*proxy{}, tasks: map[string][]string{}, scripts: map[string]fake.Script{}, seeded: map[string]string{}}
	providerCA := hostidentitytest.NewCA(ft)
	clientTLS, err := tg.providerTLS(ft, providerCA)
	if err != nil {
		return nil, err
	}
	trustFiles := map[string][]byte{
		trust.FileHostCA:       tg.hostCA.PEM,
		trust.FileProviderCA:   providerCA.PEM,
		trust.FileGrantAnchors: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sg.Anchor().Raw}),
		trust.FileProviderSANs: []byte(trusttest.ProviderIdentity().String() + "\n"),
	}
	for _, name := range cfg.Hosts {
		hostURI, err := hostidentity.HostURI(trusttest.TrustDomain, trusttest.HostNS, name)
		if err != nil {
			return nil, err
		}
		enrolment, err := operation.NewRandomID()
		if err != nil {
			return nil, err
		}
		rec := hostidentity.Record{EnrolmentID: enrolment, ProviderUID: trusttest.ProviderUID, InstallID: installID, Mode: hostidentity.ModeIdentity}
		key := hostidentitytest.NewKey(ft)
		csr := hostidentitytest.NewCSR(ft, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(hostURI, rec)})
		certPEM := tg.hostCA.Issue(ft, csr, hostidentitytest.IssueOptions{})
		h, err := newHost(filepath.Join(t.TempDir(), name), name, hostURI, enrolment, certPEM, key, trustFiles,
			clientTLS.Clone(), StartEpoch, cfg.StepDeadline)
		if err != nil {
			return nil, err
		}
		tg.hosts = append(tg.hosts, h)
		tg.byName[name] = h
		tg.scripts[name] = fake.Script{CreateStopped: true}
		if err := fake.WriteScript(h.FakeDir(), tg.scripts[name]); err != nil {
			return nil, err
		}
		if err := fake.SetHold(h.FakeDir(), true); err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			_ = h.Stop()
			// No scripted fault outlives its case, on disk or in memory.
			clean := fake.Script{CreateStopped: true}
			tg.mu.Lock()
			tg.scripts[name] = clean
			tg.mu.Unlock()
			_ = fake.WriteScript(h.FakeDir(), clean)
		})
		if err := h.Start(); err != nil {
			return nil, err
		}
		if h.State() != providerv1.AgentState_AGENT_STATE_READY {
			return nil, fmt.Errorf("the agent %s started %s, not ready\n%s", name, h.State(), h.tail())
		}
		if err := tg.front(t, h, hostURI, key); err != nil {
			return nil, err
		}
	}
	tg.clientCert = clientTLS.Certificates[0]
	prov, err := tg.newProvider()
	if err != nil {
		return nil, err
	}
	tg.prov = prov
	t.Cleanup(func() {
		tg.mu.Lock()
		p := tg.prov
		tg.mu.Unlock()
		_ = p.Close()
	})
	return tg, nil
}

// front puts a proxy in front of h's agent and adds the host to the
// Provider's inventory: the proxy's address, the host identity issuer, the
// host's URI and the pin of its key.
func (t *Target) front(ct clustered.T, h *Host, hostURI string, key *ecdsa.PrivateKey) error {
	px, err := newProxy(h.Addr)
	if err != nil {
		return err
	}
	ct.Cleanup(px.Close)
	pin, err := hostidentity.SPKIPin(key.Public())
	if err != nil {
		return err
	}
	t.proxies[h.Name] = px
	t.inv.Hosts = append(t.inv.Hosts, hostsecret.Host{ID: h.Name, Endpoint: hostsecret.AgentEndpointScheme + px.Addr(),
		Credentials: hostsecret.Credentials{AgentTLS: &hostsecret.AgentTLS{CABundle: t.hostCA.PEM, ExpectedSAN: hostURI, KeyPins: []string{pin}}}})
	return nil
}

// newProvider returns a new clustered Provider over the Target's inventory.
func (t *Target) newProvider() (*cloudhypervisor.Provider, error) {
	t.mu.Lock()
	inv := t.inv
	t.mu.Unlock()
	return cloudhypervisor.New(cloudhypervisor.Options{ClientCertificate: t.clientCert, ProviderUID: trusttest.ProviderUID,
		APIVersion: t.apiVersion, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, inv)
}

// Provider returns the Target's current clustered Provider.
func (t *Target) Provider() *cloudhypervisor.Provider {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prov
}

// providerTLS is the client TLS of the Target's own connections to the
// agents: the Provider identity certificate and the host identity issuer as
// the only root. The Provider presents the same certificate.
func (t *Target) providerTLS(ft fatalT, providerCA *hostidentitytest.CA) (*tls.Config, error) {
	key := hostidentitytest.NewKey(ft)
	csr := hostidentitytest.NewCSR(ft, key, hostidentitytest.CSROptions{URIs: []string{trusttest.ProviderURI(trusttest.ProviderUID)}})
	certPEM := providerCA.Issue(ft, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("provider certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(t.hostCA.Cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{block.Bytes}, PrivateKey: key}},
		// The host's certificate is checked against the host identity issuer
		// below; it names no DNS name to verify.
		InsecureSkipVerify: true, // #nosec G402 -- verified in VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no host certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}, nil
}

// Host returns the agent process of host.
func (t *Target) Host(name string) *Host { return t.byName[name] }

// recordTask remembers a task the Provider returned, for FinishTasks.
func (t *Target) recordTask(host, inner string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tasks[host] = append(t.tasks[host], inner)
}

// Name implements clustered.Target.
func (t *Target) Name() string { return "per-host-agent" }

// Capabilities implements clustered.Target.
func (t *Target) Capabilities() clustered.Capabilities {
	why := "the per-host agent target exercises the operation path only"
	return clustered.Capabilities{
		Features: []clustered.Feature{clustered.FeatureOperationIdentity, clustered.FeatureOperationIdentityRequired,
			clustered.FeatureAsyncTasks, clustered.FeatureCreate, clustered.FeatureScriptedOutcomes, clustered.FeatureHostAnswers,
			clustered.FeatureHostInventory, clustered.FeatureHostBlackhole, clustered.FeatureHostRemoval},
		Reasons: map[clustered.Feature]string{
			clustered.FeatureVMNotOwnedReason: why, clustered.FeatureCreateOwnerRebind: why, clustered.FeatureHostFaults: why,
			clustered.FeatureUnreadableStamp: why, clustered.FeatureProviderNotReady: why,
		},
	}
}

// Hosts implements clustered.Target.
func (t *Target) Hosts() []string {
	out := make([]string, 0, len(t.hosts))
	for _, h := range t.hosts {
		out = append(out, h.Name)
	}
	return out
}

// Server implements clustered.Target.
func (t *Target) Server() providerv1.ProviderServer { return t.Provider() }

// NewGRPCServer implements clustered.GRPCServerTarget: the Provider's own
// service description (it reads forwarded requests as raw bytes), and an
// interceptor that records the task references it returns, for FinishTasks.
func (t *Target) NewGRPCServer() *grpc.Server {
	srv := grpc.NewServer(grpc.UnaryInterceptor(t.recordTasks))
	t.Provider().Register(srv)
	return srv
}

// recordTasks records the host-encoded task reference of a mutating call's
// answer.
func (t *Target) recordTasks(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if task, ok := resp.(interface{ GetTask() *providerv1.TaskRef }); ok && err == nil && task.GetTask().GetId() != "" {
		if host, inner, perr := hostTask(task.GetTask().GetId()); perr == nil {
			t.recordTask(host, inner)
		}
	}
	return resp, err
}

// Restart implements clustered.Target: a new Provider process, whose memory
// and connections are new; the hosts keep running.
func (t *Target) Restart() error {
	prov, err := t.newProvider()
	if err != nil {
		return err
	}
	t.mu.Lock()
	old := t.prov
	t.prov = prov
	t.mu.Unlock()
	return old.Close()
}

// SetHostBlackholed implements clustered.HostBlackholer: the host's proxy
// accepts connections and forwards nothing.
func (t *Target) SetHostBlackholed(host string, blackholed bool) error {
	px, ok := t.proxies[host]
	if !ok {
		return fmt.Errorf("no host %q", host)
	}
	px.setBlackhole(blackholed)
	return nil
}

// RemoveHost implements clustered.HostRemover: the host leaves the
// Provider's inventory, as an inventory reload would remove it; its agent
// keeps running.
func (t *Target) RemoveHost(host string) error {
	t.mu.Lock()
	kept := t.inv.Hosts[:0:0]
	for _, h := range t.inv.Hosts {
		if h.ID != host {
			kept = append(kept, h)
		}
	}
	t.inv.Hosts = kept
	inv, prov := t.inv, t.prov
	t.mu.Unlock()
	return prov.Reconcile(inv)
}

// SetHostReachable implements clustered.Target: the host's proxy cuts every
// connection and refuses new ones, or accepts them again.
func (t *Target) SetHostReachable(host string, reachable bool) error {
	px, ok := t.proxies[host]
	if !ok {
		return fmt.Errorf("no host %q", host)
	}
	px.setDown(!reachable)
	return nil
}

// uidRE is the shape of a VM UID, which the agent uses as the VM's id.
var uidRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)

// uidOf is the owner UID of vm: the UID a seeded VM's name maps to, or the
// id itself (a Create answers with the VM's UID).
func (t *Target) uidOf(vm clustered.VM) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if uid, ok := t.seeded[vm.Host+"/"+vm.ID]; ok {
		return uid
	}
	return vm.ID
}

// Observe implements clustered.Target: the fake executor's VM file of the
// VM's owner UID.
func (t *Target) Observe(_ context.Context, vm clustered.VM) (clustered.Observation, error) {
	h := t.byName[vm.Host]
	if h == nil {
		return clustered.Observation{}, fmt.Errorf("no host %q", vm.Host)
	}
	uid := t.uidOf(vm)
	if !uidRE.MatchString(uid) {
		return clustered.Observation{}, nil
	}
	b, err := os.ReadFile(filepath.Join(h.FakeDir(), "vms", uid+".json")) // #nosec G304 -- a UID-shaped name in the test's directory
	if errors.Is(err, os.ErrNotExist) {
		return clustered.Observation{}, nil
	}
	if err != nil {
		return clustered.Observation{}, err
	}
	var v fake.VM
	if err := json.Unmarshal(b, &v); err != nil {
		return clustered.Observation{}, err
	}
	return clustered.Observation{Exists: true, Running: v.Power == fake.PowerOn, OwnerUID: uid}, nil
}

// SeedVM implements clustered.Target: the VM is made through the agent
// itself, by a signed Create (and a signed power operation when its power
// state is not the one asked for), so that the host's store holds its
// placement. Its id is its name. A per-host agent keys a VM by its owner's
// UID: a second VM of the same owner on a host is the first one under
// another name.
func (t *Target) SeedVM(ctx context.Context, spec clustered.SeedSpec) (clustered.VM, error) {
	uid := spec.Owner.UID
	if !uidRE.MatchString(uid) {
		return clustered.VM{}, fmt.Errorf("a per-host agent seeds only VMs with an owner UID, not %q", uid)
	}
	vm := clustered.VM{ID: spec.Name, Host: spec.Host, Name: spec.Name}
	t.mu.Lock()
	t.seeded[spec.Host+"/"+spec.Name] = uid
	t.mu.Unlock()
	o, err := t.Observe(ctx, vm)
	if err != nil {
		return clustered.VM{}, err
	}
	if !o.Exists {
		req := &providerv1.CreateRequest{Name: spec.Name, TargetHostId: spec.Host, Owner: spec.Owner.Proto()}
		if err := t.runSigned(ctx, spec.Host, uid, providerv1.OperationKind_OPERATION_KIND_CREATE, req); err != nil {
			return clustered.VM{}, fmt.Errorf("seed %s: %w", spec.Name, err)
		}
	}
	if spec.Running != o.Running || !o.Exists && spec.Running {
		kind, op := providerv1.OperationKind_OPERATION_KIND_POWER_OFF, providerv1.PowerOp_POWER_OP_OFF
		if spec.Running {
			kind, op = providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.PowerOp_POWER_OP_ON
		}
		req := &providerv1.PowerRequest{Id: spec.Name, Op: op, TargetHostId: spec.Host, Owner: spec.Owner.Proto()}
		if err := t.runSigned(ctx, spec.Host, uid, kind, req); err != nil {
			return clustered.VM{}, fmt.Errorf("seed %s power: %w", spec.Name, err)
		}
	}
	return vm, nil
}

// Mint returns an envelope for one signed operation of kind for req on host:
// the binding and attempt the Target tracks, a fresh op_id and salt.
func (t *Target) Mint(ctx context.Context, host, uid string, kind providerv1.OperationKind, req proto.Message) (*providerv1.OperationEnvelope, operation.Identity, error) {
	h := t.byName[host]
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, operation.Identity{}, err
	}
	binding, attempt := t.placements.NextPlacement(host, uid, kind)
	opID, err := operation.NewRandomID()
	if err != nil {
		return nil, operation.Identity{}, err
	}
	salt, err := operation.NewDigestSalt()
	if err != nil {
		return nil, operation.Identity{}, err
	}
	id := operation.Identity{ProviderUID: trusttest.ProviderUID, HostID: host, EnrolmentID: h.Enrolment, Epoch: t.epoch,
		VMUID: uid, BindingID: binding, OpID: opID, Kind: kind, Attempt: attempt, MintEpoch: t.epoch}
	g, err := operation.MintOperationGrant(ctx, t.signer, operation.MintRequest{Identity: id, DigestSalt: salt, Request: b, IssuedAt: time.Now()})
	if err != nil {
		return nil, operation.Identity{}, err
	}
	return &providerv1.OperationEnvelope{Request: b, Grant: g}, id, nil
}

// SendDirect sends env on kind's method straight to host's agent and returns
// the agent's task reference.
func (t *Target) SendDirect(ctx context.Context, host string, kind providerv1.OperationKind, env *providerv1.OperationEnvelope) (string, error) {
	row, ok := grantverify.RowFor(kind)
	if !ok {
		return "", fmt.Errorf("kind %s", kind)
	}
	req := row.NewRequest()
	var resp proto.Message = &providerv1.TaskResponse{}
	if kind == providerv1.OperationKind_OPERATION_KIND_CREATE {
		resp = &providerv1.CreateResponse{}
	}
	r := req.ProtoReflect()
	r.Set(r.Descriptor().Fields().ByName("operation_envelope"), protoreflect.ValueOfMessage(env.ProtoReflect()))
	if err := t.byName[host].Conn().Invoke(ctx, row.FullMethod, req, resp); err != nil {
		return "", err
	}
	if c, ok := resp.(*providerv1.CreateResponse); ok {
		return c.GetTask().GetId(), nil
	}
	if r, ok := resp.(*providerv1.TaskResponse); ok {
		return r.GetTask().GetId(), nil
	}
	return "", errors.New("unexpected response type")
}

// runSigned runs one signed operation straight on host's agent, to its end.
func (t *Target) runSigned(ctx context.Context, host, uid string, kind providerv1.OperationKind, req proto.Message) error {
	env, _, err := t.Mint(ctx, host, uid, kind, req)
	if err != nil {
		return err
	}
	task, err := t.SendDirect(ctx, host, kind, env)
	if err != nil {
		return err
	}
	return t.settle(ctx, host, []string{task})
}

// WaitTask polls the agent's TaskStatus for task until it is no longer
// running, and returns the answer.
func (t *Target) WaitTask(ctx context.Context, host, task string, limit time.Duration) (*providerv1.TaskStatusResponse, error) {
	deadline := time.Now().Add(limit)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		st, err := t.byName[host].Client().TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
		if err != nil && status.Code(err) != codes.Unavailable {
			return nil, err
		}
		if err == nil && st.GetOutcome() != providerv1.TaskOutcome_TASK_OUTCOME_RUNNING {
			return st, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("task %s on %s still running after %s", task, host, limit)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// settle releases host's held steps until tasks are no longer running.
func (t *Target) settle(ctx context.Context, host string, tasks []string) error {
	h := t.byName[host]
	if err := fake.SetHold(h.FakeDir(), false); err != nil {
		return err
	}
	defer func() { _ = fake.SetHold(h.FakeDir(), true) }()
	for _, task := range tasks {
		if _, err := ParseTask(task); err != nil {
			continue
		}
		if _, err := t.WaitTask(ctx, host, task, settleTimeout); err != nil && status.Code(err) != codes.NotFound {
			// NotFound: the host holds no record of the task (its store was
			// rolled back); nothing runs.
			return fmt.Errorf("settle %s on %s: %w\n%s", task, host, err, h.tail())
		}
	}
	return nil
}

// ParseTask parses an agent's task reference.
func ParseTask(task string) (string, error) {
	if _, err := server.ParseTaskRef(task); err != nil {
		return "", err
	}
	return task, nil
}

// FinishTasks implements clustered.TaskController: every operation the
// Provider started runs to its end.
func (t *Target) FinishTasks() error {
	t.mu.Lock()
	tasks := map[string][]string{}
	for h, ts := range t.tasks {
		tasks[h] = append([]string(nil), ts...)
	}
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	for _, h := range t.hosts {
		if !h.Running() {
			continue
		}
		if err := t.settle(ctx, h.Name, tasks[h.Name]); err != nil {
			return err
		}
	}
	return nil
}

// OperationIdentity implements clustered.OperationIdentityTarget.
func (t *Target) OperationIdentity() clustered.OperationIdentityConfig {
	enrolments := map[string][]byte{}
	for _, h := range t.hosts {
		enrolments[h.Name] = append([]byte(nil), h.Enrolment...)
	}
	return clustered.OperationIdentityConfig{Signer: t.signer, ProviderUID: trusttest.ProviderUID, Enrolments: enrolments,
		Epoch: t.epoch, EnrolmentStartEpoch: StartEpoch}
}

// NextPlacement implements clustered.PlacementTracker.
func (t *Target) NextPlacement(host, vmUID string, kind providerv1.OperationKind) ([]byte, uint64) {
	return t.placements.NextPlacement(host, vmUID, kind)
}

// addFault appends a fault to host's script.
func (t *Target) addFault(host string, f fake.ScriptedFault) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.byName[host]
	if h == nil {
		return fmt.Errorf("no host %q", host)
	}
	s := t.scripts[host]
	s.Faults = append(s.Faults, f)
	t.scripts[host] = s
	return fake.WriteScript(h.FakeDir(), s)
}

// ErrCannotScript is returned by ScriptOutcome for an outcome the agent
// cannot be made to reach from a power operation.
var ErrCannotScript = errors.New("the per-host agent target cannot script this outcome")

// ScriptOutcome implements clustered.OutcomeScripter for the outcomes the
// agent can reach: UNKNOWN (a power step's effect is in doubt and its
// inspection fails), FAILED_CLEAN (a power step reports it did nothing) and
// FAILED_DIRTY (a Create's start step reports it did nothing after the VM's
// definition, which cannot be undone). RETIRED arises only from a
// re-enrolment, which the agent process cannot do yet.
func (t *Target) ScriptOutcome(host string, outcome providerv1.TaskOutcome, _ string) error {
	switch outcome {
	case providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN:
		return t.addFault(host, fake.ScriptedFault{Step: "power", Fault: fake.FaultProbeError})
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN:
		return t.addFault(host, fake.ScriptedFault{Step: "power", Fault: fake.FaultNotPerformed})
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY:
		// A Create's definition of the VM cannot be undone: its start then
		// failing is a dirty failure.
		return t.addFault(host, fake.ScriptedFault{Step: "start", Fault: fake.FaultNotPerformed})
	}
	return fmt.Errorf("%w: %s", ErrCannotScript, outcome)
}

// HostAnswerRoots implements clustered.HostAnswerTarget.
func (t *Target) HostAnswerRoots() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(t.hostCA.Cert)
	return p
}

// CrashHost implements clustered.HostAnswerTarget: SIGKILL, then a new
// process on the same state.
func (t *Target) CrashHost(host string) error {
	h := t.byName[host]
	if h == nil {
		return fmt.Errorf("no host %q", host)
	}
	if err := h.Kill(); err != nil {
		return err
	}
	return h.Restart()
}

// SnapshotHost implements clustered.HostAnswerTarget: the agent is stopped,
// its store copied, and the agent started again.
func (t *Target) SnapshotHost(host string) error {
	h := t.byName[host]
	if h == nil {
		return fmt.Errorf("no host %q", host)
	}
	if err := h.Stop(); err != nil {
		return err
	}
	if err := h.Snapshot(); err != nil {
		return err
	}
	return h.Restart()
}

// RollBackHost implements clustered.HostAnswerTarget: the agent is stopped,
// the copy put back without its anchor log, and the agent started again.
func (t *Target) RollBackHost(host string) error {
	h := t.byName[host]
	if h == nil {
		return fmt.Errorf("no host %q", host)
	}
	if err := h.Stop(); err != nil {
		return err
	}
	if err := h.RollBack(); err != nil {
		return err
	}
	return h.Restart()
}

// StallNextStep implements clustered.HostAnswerTarget.
func (t *Target) StallNextStep(host string) error {
	return t.addFault(host, fake.ScriptedFault{Fault: fake.FaultStall})
}

// StepDeadline implements clustered.HostAnswerTarget.
func (t *Target) StepDeadline() time.Duration { return t.deadline }

// hostTask splits a host-encoded task reference the Provider returned.
func hostTask(ref string) (string, string, error) {
	host, inner, err := clusteredref.ParseHostTaskRef(ref)
	return string(host), inner, err
}
