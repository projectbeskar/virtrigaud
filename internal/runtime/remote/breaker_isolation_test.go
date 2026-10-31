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

package remote

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Fault isolation of the per-Provider circuit breaker (#472). A real
// Resolver, gRPC Client, breaker and interceptor talk to a real loopback gRPC
// provider on which some VMs fail with the error shapes the production
// providers send for a single VM's backend failure, while the other VMs are
// healthy. The VMs reconcile on the VirtualMachine controller's cadence on a
// simulated clock (the breaker's Config.Now), so the breaker's 60 s reset and
// the 2 min healthy poll are exact and the run takes a few seconds.

// Requeue cadence of the VirtualMachine controller.
const (
	isoFailingRequeue = 5 * time.Second   // after a failed Describe
	isoHealthyRequeue = 120 * time.Second // after a healthy Describe
	isoResolveRequeue = 5 * time.Second   // after a failed GetProvider
	isoOpsInterval    = time.Minute       // a healthy VM's lifecycle ops
	isoRunFor         = 20 * time.Minute
)

// isoShape is the error a failing VM's Describe answers with.
type isoShape int

const (
	// isoPlain is a handler's plain error (libvirt single-host Describe /
	// Power / Delete, vSphere fmt.Errorf): grpc-go sends codes.Unknown.
	isoPlain isoShape = iota
	// isoSDKInternal is sdk errors.NewInternal (Proxmox per-VM API error).
	isoSDKInternal
	// isoElsewhere is the Proxmox "exists elsewhere in the cluster"
	// refusal as sent from v0.4.2-rc2: Unavailable + VM_OPERATION_FAILED.
	isoElsewhere
	// isoElsewhereOld is the same refusal as older Proxmox images send it:
	// a plain Unavailable (still counted: residual limitation).
	isoElsewhereOld
	isoShapes = 3 // shapes rotated over failing VMs (not isoElsewhereOld)
)

// isoError returns the shape's provider-side error.
func isoError(shape isoShape, id string) error {
	switch shape {
	case isoPlain:
		return fmt.Errorf("domain %s: internal error: cannot acquire state change lock", id)
	case isoSDKInternal:
		return sdkerrors.NewInternal("failed to describe VM", errors.New("API 500: VM is locked (backup)"))
	case isoElsewhere:
		st, err := status.New(codes.Unavailable, "service unavailable: Proxmox VM 101 is not on node pve1 but exists elsewhere in the cluster").
			WithDetails(&errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
		if err != nil {
			return err
		}
		return st.Err()
	default:
		return sdkerrors.NewUnavailable("Proxmox VM 101 is not on node pve1 but exists elsewhere in the cluster", nil)
	}
}

// isoServer is the provider: VMs in failing answer Describe with their
// shape's error; every other call succeeds.
type isoServer struct {
	providerv1.UnimplementedProviderServer
	failing   map[string]isoShape
	validates atomic.Int64
	rpcs      atomic.Int64
	mu        sync.Mutex
	describes map[string]int64 // Describes that reached the server, per VM
}

func (s *isoServer) count(id string) {
	s.rpcs.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.describes == nil {
		s.describes = map[string]int64{}
	}
	s.describes[id]++
}

func (s *isoServer) reached(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.describes[id]
}

func (s *isoServer) Validate(context.Context, *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	s.validates.Add(1)
	s.rpcs.Add(1)
	return &providerv1.ValidateResponse{Ok: true}, nil
}

func (s *isoServer) Describe(_ context.Context, r *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s.count(r.GetId())
	if shape, ok := s.failing[r.GetId()]; ok {
		return nil, isoError(shape, r.GetId())
	}
	return &providerv1.DescribeResponse{Exists: true, PowerState: "On"}, nil
}

func (s *isoServer) Power(context.Context, *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	s.rpcs.Add(1)
	return &providerv1.TaskResponse{}, nil
}

func (s *isoServer) Reconfigure(context.Context, *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	s.rpcs.Add(1)
	return &providerv1.TaskResponse{}, nil
}

func (s *isoServer) Delete(context.Context, *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	s.rpcs.Add(1)
	return &providerv1.TaskResponse{}, nil
}

func (s *isoServer) Create(_ context.Context, r *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	s.rpcs.Add(1)
	return &providerv1.CreateResponse{Id: "new-" + r.GetName()}, nil
}

func (s *isoServer) Clone(_ context.Context, r *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	s.rpcs.Add(1)
	return &providerv1.CloneResponse{TargetVmId: "clone-" + r.GetTargetName()}, nil
}

// connCountingListener counts the connections the provider accepted: one
// per client dial (connection reuse = 1 for the whole run).
type connCountingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *connCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

// isoEvent is one VM's next reconcile (vm < 0: a lifecycle-ops round).
type isoEvent struct {
	at time.Time
	vm int
}

type isoQueue []isoEvent

func (q isoQueue) Len() int { return len(q) }
func (q isoQueue) Less(i, j int) bool {
	if q[i].at.Equal(q[j].at) {
		return q[i].vm < q[j].vm
	}
	return q[i].at.Before(q[j].at)
}
func (q isoQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *isoQueue) Push(x interface{}) {
	if e, ok := x.(isoEvent); ok {
		*q = append(*q, e)
	}
}
func (q *isoQueue) Pop() interface{} {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]
	return e
}

// isoResult is what one run measured.
type isoResult struct {
	reconciles         int
	healthyReconciles  int
	healthyDescribesOK int   // healthy Describes that succeeded
	healthyReached     int64 // healthy Describes that reached the provider
	healthyRefused     int   // healthy Describes that failed (breaker)
	resolveFailures    int   // GetProvider errors (Provider-wide Ready=False)
	opsAttempted       int
	opsRefused         int // healthy Power/Reconfigure/Delete/Create/Clone refused
	breakerOpenings    int
	notClosedFraction  float64
	transitions        int
	validates          int64
	rpcs               int64
	dials              int64
}

func (r isoResult) String() string {
	return fmt.Sprintf("openings=%d notClosed=%.0f%% healthyDescribeReached=%d healthyRefused=%d resolveFailures=%d "+
		"opsRefused=%d/%d validates=%d (%.4f/reconcile) rpcs=%d dials=%d transitions=%d",
		r.breakerOpenings, 100*r.notClosedFraction, r.healthyReached, r.healthyRefused, r.resolveFailures,
		r.opsRefused, r.opsAttempted, r.validates, float64(r.validates)/float64(r.reconciles), r.rpcs, r.dials, r.transitions)
}

// runIsolation runs failing + healthy VMs on one Provider for isoRunFor of
// simulated time with the default breaker configuration (threshold 10, 60 s
// reset, 3 half-open calls). shape picks each failing VM's error.
func runIsolation(t *testing.T, failing, healthy int, shape func(i int) isoShape) isoResult {
	t.Helper()
	srv := &isoServer{failing: map[string]isoShape{}}
	ids := make([]string, failing+healthy)
	for i := range ids {
		ids[i] = fmt.Sprintf("vm-%03d", i)
		if i < failing {
			srv.failing[ids[i]] = shape(i)
		}
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lis := &connCountingListener{Listener: raw}
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	defer gsrv.Stop()

	now := time.Unix(1_700_000_000, 0)
	cfg := resilience.DefaultConfig()
	cfg.Now = func() time.Time { return now }
	reg := resilience.NewRegistry(cfg)
	r := newValidationResolver(t, reg)
	defer r.CleanupAllClients()
	provider := runningProvider(lis.Addr().String())
	ctx := context.Background()

	q := &isoQueue{}
	n := len(ids)
	for i := range ids {
		heap.Push(q, isoEvent{at: now.Add(time.Duration(i) * isoFailingRequeue / time.Duration(n)), vm: i})
	}
	heap.Push(q, isoEvent{at: now.Add(isoOpsInterval), vm: -1})
	end := now.Add(isoRunFor)

	var res isoResult
	var notClosed time.Duration
	state := resilience.StateClosed
	breakerState := func() resilience.State {
		if cb, ok := reg.Get(circuitBreakerName, string(provider.Spec.Type), provider.Name); ok {
			return cb.GetState()
		}
		return resilience.StateClosed
	}
	observe := func() {
		s := breakerState()
		if s != state {
			res.transitions++
			if s == resilience.StateOpen {
				res.breakerOpenings++
			}
			state = s
		}
	}
	healthyIdx := failing // the VM whose lifecycle ops are driven

	for q.Len() > 0 {
		ev, ok := heap.Pop(q).(isoEvent)
		if !ok || ev.at.After(end) {
			break
		}
		if state != resilience.StateClosed {
			notClosed += ev.at.Sub(now)
		}
		now = ev.at

		if ev.vm < 0 {
			if healthy > 0 {
				res.opsAttempted += 5
				res.opsRefused += isoLifecycleOps(ctx, r, provider, ids[healthyIdx])
				observe()
				healthyIdx = failing + (healthyIdx-failing+1)%healthy
			}
			heap.Push(q, isoEvent{at: now.Add(isoOpsInterval), vm: -1})
			continue
		}

		isHealthy := ev.vm >= failing
		res.reconciles++
		if isHealthy {
			res.healthyReconciles++
		}
		p, err := r.GetProvider(ctx, provider)
		observe()
		if err != nil {
			res.resolveFailures++
			heap.Push(q, isoEvent{at: now.Add(isoResolveRequeue), vm: ev.vm})
			continue
		}
		_, err = p.Describe(ctx, contracts.VMRef{ID: ids[ev.vm]})
		observe()
		next := isoFailingRequeue
		switch {
		case isHealthy && err == nil:
			res.healthyDescribesOK++
			next = isoHealthyRequeue
		case isHealthy:
			res.healthyRefused++
		}
		heap.Push(q, isoEvent{at: now.Add(next), vm: ev.vm})
	}

	for i := failing; i < len(ids); i++ {
		res.healthyReached += srv.reached(ids[i])
	}
	res.notClosedFraction = notClosed.Seconds() / isoRunFor.Seconds()
	res.validates = srv.validates.Load()
	res.rpcs = srv.rpcs.Load()
	res.dials = lis.accepted.Load()
	return res
}

// isoLifecycleOps resolves the Provider and sends Power, Reconfigure, Delete
// and a new Create and Clone for a healthy VM; it returns how many were
// refused (a resolve failure refuses all five).
func isoLifecycleOps(ctx context.Context, r *Resolver, provider *infravirtrigaudiov1beta1.Provider, id string) int {
	p, err := r.GetProvider(ctx, provider)
	if err != nil {
		return 5
	}
	refused := 0
	vm := contracts.VMRef{ID: id}
	if _, err := p.Power(ctx, vm, contracts.PowerOpOn); err != nil {
		refused++
	}
	if _, err := p.Reconfigure(ctx, vm, contracts.CreateRequest{Name: id}); err != nil {
		refused++
	}
	if _, err := p.Delete(ctx, vm); err != nil {
		refused++
	}
	if _, err := p.Create(ctx, contracts.CreateRequest{Name: "new-" + id}); err != nil {
		refused++
	}
	cloner, ok := p.(contracts.Cloner)
	if !ok {
		return refused + 1
	}
	if _, err := cloner.Clone(ctx, contracts.CloneRequest{Source: vm, TargetName: "clone-" + id}); err != nil {
		refused++
	}
	return refused
}

// rotateShapes gives failing VM i the i-th real provider shape in turn.
func rotateShapes(i int) isoShape { return isoShape(i % isoShapes) }

// TestBreakerIsolation_MixedFailingAndHealthyVMs is scenario A of #472:
// VM-scoped backend failures of some VMs must not open the Provider's
// breaker, and healthy VMs keep reaching the provider.
func TestBreakerIsolation_MixedFailingAndHealthyVMs(t *testing.T) {
	scenarios := []struct{ failing, healthy int }{
		{1, 1}, {3, 6}, {40, 6}, {20, 60}, {1, 6}, {10, 60},
	}
	shapes := []struct {
		name  string
		shape func(i int) isoShape
	}{
		// Every failing VM: a handler's plain error (codes.Unknown).
		{"unknown", func(int) isoShape { return isoPlain }},
		// Every failing VM: sdk errors.NewInternal (codes.Internal).
		{"internal", func(int) isoShape { return isoSDKInternal }},
		// Rotated: plain, sdk Internal, Proxmox "exists elsewhere" (rc2 shape).
		{"mixed", rotateShapes},
	}
	var report strings.Builder
	for _, sh := range shapes {
		for _, sc := range scenarios {
			t.Run(fmt.Sprintf("%s/%d_failing_%d_healthy", sh.name, sc.failing, sc.healthy), func(t *testing.T) {
				res := runIsolation(t, sc.failing, sc.healthy, sh.shape)
				fmt.Fprintf(&report, "%-8s %2d/%-2d %s\n", sh.name, sc.failing, sc.healthy, res)

				assert.Zero(t, res.breakerOpenings, "VM-scoped failures must not open the Provider's breaker")
				assert.Zero(t, res.healthyRefused, "every healthy Describe must reach the provider")
				assert.Equal(t, int64(res.healthyDescribesOK), res.healthyReached)
				assert.Zero(t, res.opsRefused, "healthy Power/Reconfigure/Delete and new Create/Clone must not be refused")
				assert.Zero(t, res.resolveFailures, "no Provider-wide resolve failure (Ready=False)")
				// F3 (#467) preserved: the validated client is reused (one
				// connection, Validate only on first use).
				assert.EqualValues(t, 1, res.dials, "the validated client's connection is reused")
				assert.EqualValues(t, 1, res.validates, "Validate runs once, on first use")
			})
		}
	}
	t.Logf("scenario A (failing/healthy, %s simulated, default breaker):\n%s", isoRunFor, report.String())
}

// TestBreakerIsolation_F3ConnectionReuse measures, for 50 healthy VMs and 5
// failing ones over the simulated run, what F3 (#467, validate-once client
// reuse) saves and the fault-isolation fix must keep: Validate RPCs per
// reconcile ~0, one connection for the whole run, no breaker transition, every
// healthy call succeeding.
func TestBreakerIsolation_F3ConnectionReuse(t *testing.T) {
	res := runIsolation(t, 5, 50, func(int) isoShape { return isoPlain })
	t.Logf("F3 (5 failing / 50 healthy, %s simulated): reconciles=%d healthyReconciles=%d healthySuccess=%.1f%% %s",
		isoRunFor, res.reconciles, res.healthyReconciles,
		100*float64(res.healthyDescribesOK+res.opsAttempted-res.opsRefused)/float64(res.healthyReconciles+res.opsAttempted), res)

	assert.EqualValues(t, 1, res.validates, "Validate runs once, on first use")
	assert.Less(t, float64(res.validates)/float64(res.reconciles), 0.01, "Validate per reconcile stays ~0")
	assert.EqualValues(t, 1, res.dials, "one connection for the whole run (no eviction / re-dial)")
	assert.Zero(t, res.transitions, "no breaker transition")
	assert.Equal(t, res.healthyReconciles, res.healthyDescribesOK, "every healthy Describe succeeds")
	assert.Zero(t, res.opsRefused)
}
