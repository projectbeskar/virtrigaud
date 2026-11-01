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

package clustered

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/resilience"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	// caseTimeout bounds every RPC a case makes.
	caseTimeout = 30 * time.Second

	// UnknownHost is a well-formed host id that no Target fronts.
	UnknownHost = "host-zzz-unknown"
)

var (
	// OwnerA and OwnerB are two distinct VirtualMachine identities.
	OwnerA = Identity{UID: "0b8f5e0e-7a53-4f6b-9a0e-1d2c3b4a5f60", Namespace: "team-a", Name: "web"}
	// OwnerB is the identity of another tenant.
	OwnerB = Identity{UID: "c4d1a9f2-3e6b-4c7d-8e9f-0a1b2c3d4e5f", Namespace: "team-b", Name: "secret-db"}
)

// Env is what a case runs against: the Target, a running gRPC server for its
// Provider, two clients, and helpers that build requests and read answers.
type Env struct {
	// T is the case's test handle.
	T T
	// Target is the adapter under test.
	Target Target

	hosts  []string
	ctx    context.Context
	srv    *grpc.Server
	conn   *grpc.ClientConn
	raw    providerv1.ProviderClient
	mgr    *transportgrpc.Client
	addr   string
	closed bool
}

// newEnv starts the Target's Provider on a loopback gRPC server.
func newEnv(t T, tg Target) *Env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	e := &Env{T: t, Target: tg, ctx: ctx, hosts: tg.Hosts()}
	require.GreaterOrEqual(t, len(e.hosts), 2, "a clustered Target fronts at least two hosts")
	e.startProvider()
	t.Cleanup(e.stopProvider)
	return e
}

// Ctx returns the case's context.
func (e *Env) Ctx() context.Context { return e.ctx }

// Call returns a context bounded by caseTimeout for one RPC.
func (e *Env) Call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, caseTimeout)
}

// Hosts returns the Target's host ids.
func (e *Env) Hosts() []string { return append([]string(nil), e.hosts...) }

// HostA is the first host of the Target.
func (e *Env) HostA() string { return e.hosts[0] }

// HostB is the second host of the Target.
func (e *Env) HostB() string { return e.hosts[1] }

// Raw returns the raw gRPC client: it sees the wire as the Provider sent it.
func (e *Env) Raw() providerv1.ProviderClient { return e.raw }

// Manager returns the manager's own transport client, which maps wire answers
// to contracts errors (contracts.Is*) exactly as the controllers see them.
func (e *Env) Manager() *transportgrpc.Client { return e.mgr }

// ManagerWithBreaker returns a second manager client guarded by a circuit
// breaker that opens after threshold counted failures.
func (e *Env) ManagerWithBreaker(threshold int) (*transportgrpc.Client, *resilience.CircuitBreaker) {
	e.T.Helper()
	cb := resilience.NewCircuitBreaker("conformance", "conformance", e.Target.Name(), &resilience.Config{
		FailureThreshold: threshold,
		ResetTimeout:     time.Hour,
		HalfOpenMaxCalls: 1,
	})
	c, err := transportgrpc.NewClient(e.ctx, e.addr, "conformance", e.Target.Name(), cb, nil)
	require.NoError(e.T, err)
	e.T.Cleanup(func() { _ = c.Close() })
	return c, cb
}

func (e *Env) startProvider() {
	e.T.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(e.T, err, "listen on loopback")
	srv := grpc.NewServer()
	providerv1.RegisterProviderServer(srv, e.Target.Server())
	go func() { _ = srv.Serve(lis) }()
	e.srv = srv
	e.addr = lis.Addr().String()
	e.closed = false

	conn, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(e.T, err)
	e.conn = conn
	e.raw = providerv1.NewProviderClient(conn)
	mgr, err := transportgrpc.NewClient(e.ctx, e.addr, "conformance", e.Target.Name(), nil, nil)
	require.NoError(e.T, err)
	e.mgr = mgr
}

func (e *Env) stopProvider() {
	if e.closed {
		return
	}
	e.closed = true
	if e.mgr != nil {
		_ = e.mgr.Close()
	}
	if e.conn != nil {
		_ = e.conn.Close()
	}
	if e.srv != nil {
		e.srv.Stop()
	}
}

// StopProvider takes the Provider's gRPC server down, as a crashed or
// unreachable Provider pod would be. The clients stay open, so the next call
// meets a dead endpoint. Its state is not touched.
func (e *Env) StopProvider() {
	if e.srv != nil {
		e.srv.Stop()
	}
}

// StartProvider brings the gRPC server of the same service up again, on a new
// address; the clients are replaced.
func (e *Env) StartProvider() {
	e.T.Helper()
	e.stopProvider()
	e.startProvider()
}

// RestartProvider restarts the Provider process (Target.Restart) and serves
// the new service. The old clients are replaced.
func (e *Env) RestartProvider() {
	e.T.Helper()
	e.stopProvider()
	require.NoError(e.T, e.Target.Restart(), "restart the provider")
	e.startProvider()
}

// Seed places a VM on a host out of band.
func (e *Env) Seed(spec SeedSpec) VM {
	e.T.Helper()
	vm, err := e.Target.SeedVM(e.ctx, spec)
	require.NoError(e.T, err, "seed %s on %s", spec.Name, spec.Host)
	return vm
}

// Observe reads the ground truth about vm.
func (e *Env) Observe(vm VM) Observation {
	e.T.Helper()
	o, err := e.Target.Observe(e.ctx, vm)
	require.NoError(e.T, err, "observe %s on %s", vm.Name, vm.Host)
	return o
}

// HostDown makes a host unreachable for the Provider and restores it when the
// case ends.
func (e *Env) HostDown(host string) {
	e.T.Helper()
	require.NoError(e.T, e.Target.SetHostReachable(host, false))
	e.T.Cleanup(func() { _ = e.Target.SetHostReachable(host, true) })
}

// HostUp makes a host reachable again.
func (e *Env) HostUp(host string) {
	e.T.Helper()
	require.NoError(e.T, e.Target.SetHostReachable(host, true))
}

// Describe sends DescribeRequest{id, target_host_id, owner}.
func (e *Env) Describe(host, id string, owner Identity) (*providerv1.DescribeResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.Describe(ctx, &providerv1.DescribeRequest{Id: id, TargetHostId: host, Owner: owner.Proto()})
}

// Delete sends DeleteRequest{id, target_host_id, owner}.
func (e *Env) Delete(host, id string, owner Identity) (*providerv1.TaskResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.Delete(ctx, &providerv1.DeleteRequest{Id: id, TargetHostId: host, Owner: owner.Proto()})
}

// Power sends PowerRequest{id, op, target_host_id, owner}.
func (e *Env) Power(host, id string, owner Identity, op providerv1.PowerOp) (*providerv1.TaskResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.Power(ctx, &providerv1.PowerRequest{Id: id, Op: op, TargetHostId: host, Owner: owner.Proto()})
}

// Reconfigure sends ReconfigureRequest{id, desired_json, target_host_id,
// owner} with a desired state that asks for more CPU.
func (e *Env) Reconfigure(host, id string, owner Identity) (*providerv1.TaskResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id: id, TargetHostId: host, Owner: owner.Proto(),
		DesiredJson: `{"Name":"` + id + `","Class":{"CPU":4,"MemoryMiB":4096}}`,
	})
}

// TaskStatus sends TaskStatusRequest{task: ref}.
func (e *Env) TaskStatus(ref string) (*providerv1.TaskStatusResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: ref}})
}

// ListVMs sends an unfiltered ListVMsRequest.
func (e *Env) ListVMs() (*providerv1.ListVMsResponse, error) {
	ctx, cancel := e.Call()
	defer cancel()
	return e.raw.ListVMs(ctx, &providerv1.ListVMsRequest{})
}

// Settle completes the task a call returned, if it returned one, so the
// effect of the call can be observed: it finishes the Target's tasks
// (TaskController) and requires TaskStatus to report the task done. A call
// that returned no task has nothing to wait for.
func (e *Env) Settle(resp *providerv1.TaskResponse) {
	e.T.Helper()
	ref := resp.GetTask().GetId()
	if ref == "" {
		return
	}
	ctl, ok := e.Target.(TaskController)
	require.True(e.T, ok, "the call returned a task, so the Target must implement TaskController")
	require.NoError(e.T, ctl.FinishTasks())
	st, err := e.TaskStatus(ref)
	require.NoError(e.T, err, "poll the task the call returned")
	require.True(e.T, st.Done, "the task is done once the host finished it")
}

// CreateRequest returns a CreateRequest for name on host owned by owner, with
// the Target's template (CreateTemplater) filled in. A VM's name is the name
// of its VirtualMachine, so the owner identity carries the same name.
func (e *Env) CreateRequest(name, host string, owner Identity) *providerv1.CreateRequest {
	req := &providerv1.CreateRequest{ClassJson: `{"CPU":2,"MemoryMiB":1024}`}
	if tpl, ok := e.Target.(CreateTemplater); ok {
		if c, ok := proto.Clone(tpl.CreateTemplate()).(*providerv1.CreateRequest); ok {
			req = c
		}
	}
	owner.Name = name
	req.Name, req.TargetHostId, req.Owner = name, host, owner.Proto()
	return req
}

// Reason is one google.rpc.ErrorInfo detail of a wire error.
type Reason struct {
	// Reason is ErrorInfo.reason.
	Reason string
	// Domain is ErrorInfo.domain.
	Domain string
	// Metadata is ErrorInfo.metadata.
	Metadata map[string]string
}

// Wire is a gRPC error as the raw client saw it.
type Wire struct {
	// Code is the gRPC status code (codes.OK for a nil error).
	Code codes.Code
	// Message is the status message.
	Message string
	// Reasons are the ErrorInfo details.
	Reasons []Reason
}

// WireOf decodes err.
func WireOf(err error) Wire {
	st, ok := status.FromError(err)
	if !ok {
		return Wire{Code: codes.Unknown, Message: err.Error()}
	}
	w := Wire{Code: st.Code(), Message: st.Message()}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			w.Reasons = append(w.Reasons, Reason{Reason: info.GetReason(), Domain: info.GetDomain(), Metadata: info.GetMetadata()})
		}
	}
	return w
}

// Reason returns the first ErrorInfo detail with the given reason.
func (w Wire) Reason(reason string) (Reason, bool) {
	for _, r := range w.Reasons {
		if r.Reason == reason {
			return r, true
		}
	}
	return Reason{}, false
}

// String renders the wire error for a failure message.
func (w Wire) String() string {
	return fmt.Sprintf("code=%s reasons=%v message=%q", w.Code, w.Reasons, w.Message)
}

// contextWithTimeout returns a child of the case's context bounded by d.
func contextWithTimeout(e *Env, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, d)
}
