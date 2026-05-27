/*
Copyright 2025.

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

package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Client wraps a gRPC provider client and implements the contracts.Provider interface
type Client struct {
	conn   *grpc.ClientConn
	client providerv1.ProviderClient
	// vmOps records per-VM-operation counters (G7.1 / #124). Always
	// non-nil for production clients constructed via NewClient — the
	// constructor initialises it from (providerType, providerName) even
	// when those are empty strings, so recordVMOp can call into it
	// without a nil check. May be nil in tests that bypass NewClient
	// (e.g. construct &Client{...} directly); recordVMOp is nil-safe to
	// support that path.
	vmOps *metrics.VMOperationMetrics

	// tasks owns the virtrigaud_provider_tasks_inflight gauge for this
	// Client (G7.3 / #129). Always non-nil for production clients via
	// NewClient. trackTaskStart / trackTaskDone are nil-safe so test
	// clients that bypass NewClient don't panic.
	tasks *metrics.TaskMetrics
	// inflightTasksMu guards inflightTasks.
	inflightTasksMu sync.Mutex
	// inflightTasks is the set of TaskRef IDs this Client returned from
	// a task-creating RPC and has NOT yet observed Done=true on. The
	// map-based set is what makes the gauge correct under double-poll
	// (reconciler retries between observing Done and clearing
	// vm.Status.LastTaskRef) and post-restart (the new manager's map
	// starts empty; trackTaskDone on an unknown ID no-ops, which means
	// the gauge measures "tasks THIS instance is tracking", not "tasks
	// the provider thinks are in-flight").
	inflightTasks map[string]struct{}
}

// NewClient creates a new gRPC provider client.
//
// providerType is the value of the Provider CR's spec.type field (e.g.
// "vsphere", "libvirt", "proxmox", "mock") and is used as a metric label
// on every RPC call made through this client. Passing an empty string is
// permitted (the label will be empty) but discouraged in production —
// makes per-provider-type alerts impossible.
//
// providerName is the Provider CR's metadata.name (e.g. "vsphere-prod",
// "libvirt-lab"). Used as the `provider` label on
// virtrigaud_vm_operations_total samples emitted by this client (G7.1 /
// #124). Empty string is permitted (label will be empty); discouraged
// in production for the same reason as providerType.
//
// cb is an optional CircuitBreaker wrapping every outbound RPC. When
// non-nil, infrastructure-class gRPC errors (Unavailable, DeadlineExceeded,
// Internal, Unknown) count toward the breaker's failure threshold; once
// the threshold trips, subsequent RPCs short-circuit with a synthesized
// Unavailable status until ResetTimeout elapses (G6 / #111). Pass nil to
// disable circuit-breaker protection — useful in unit tests that exercise
// real gRPC failure semantics without the breaker interposing.
func NewClient(ctx context.Context, endpoint string, providerType string, providerName string, cb *resilience.CircuitBreaker, tlsConfig *TLSConfig) (*Client, error) {
	// Connection timeout is handled by grpc.NewClient internally
	_ = ctx // Context available for future timeout implementation

	var opts []grpc.DialOption

	if tlsConfig != nil {
		creds, err := buildTLSCredentials(tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Build the unary interceptor chain.
	//
	// Order is important and deliberate:
	//   1. providerRPCMetricsInterceptor — records EVERY RPC (including
	//      circuit-breaker rejections, which show up as code=Unavailable).
	//      This means dashboards see "the breaker fast-failed this RPC"
	//      as a normal Unavailable RPC, not a silent drop.
	//   2. providerCircuitBreakerInterceptor — wraps the actual invoker.
	//      When the breaker is open, returns Unavailable BEFORE invoker
	//      runs, so step 1 still observes it.
	unaryInterceptors := []grpc.UnaryClientInterceptor{
		// G4 (#90): record per-RPC latency + status code into the
		// virtrigaud_provider_rpc_* metric families.
		providerRPCMetricsInterceptor(providerType),
	}
	if cb != nil {
		// G6 (#111): wrap RPCs with circuit-breaker fast-fail. Infra
		// errors count toward the threshold; business errors (NotFound,
		// InvalidArgument, ...) pass through without tripping.
		unaryInterceptors = append(unaryInterceptors, providerCircuitBreakerInterceptor(cb))
	}

	// Add retry and timeout configurations
	opts = append(opts,
		// grpc.WithBlock() removed as it's deprecated in newer gRPC versions
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
		),
		grpc.WithChainUnaryInterceptor(unaryInterceptors...),
	)

	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to provider at %s: %w", endpoint, err)
	}

	client := providerv1.NewProviderClient(conn)
	// G7.3 (#129): per-Provider task-inflight tracker. Seed the gauge to
	// 0 here so the virtrigaud_provider_tasks_inflight family appears on
	// /metrics from boot (Prometheus gauges with labels don't show until
	// first write), giving operators a stable label set to dashboard
	// against even before the first async task fires.
	taskMetrics := metrics.NewTaskMetrics(providerType, providerName)
	taskMetrics.SetInflightTasks(0)
	return &Client{
		conn:   conn,
		client: client,
		// G7.1 (#124): per-VM-operation counter, labelled by
		// providerType + providerName so dashboards can answer
		// "how often does Create fail on the vsphere-prod Provider?"
		// independently of the gRPC-method-level G4 view.
		vmOps:         metrics.NewVMOperationMetrics(providerType, providerName),
		tasks:         taskMetrics,
		inflightTasks: make(map[string]struct{}),
	}, nil
}

// providerRPCMetricsInterceptor returns a UnaryClientInterceptor that
// records every outbound RPC call's latency and gRPC status code to
// virtrigaud_provider_rpc_requests_total{provider_type,method,code} and
// virtrigaud_provider_rpc_latency_seconds{provider_type,method}.
//
// `method` is the proto-RPC short name (e.g. "Validate", "Create",
// "Describe"), extracted from the full gRPC method path. `code` is the
// stringified gRPC status code ("OK", "Unavailable", "DeadlineExceeded",
// etc.). On nil error, code is "OK" by gRPC convention.
//
// The interceptor must never fail (panic, etc.) — if it did, every
// provider RPC would break. We keep it intentionally minimal.
func providerRPCMetricsInterceptor(providerType string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		fullMethod string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		shortMethod := shortRPCMethod(fullMethod)
		timer := metrics.NewRPCTimer(providerType, shortMethod)
		err := invoker(ctx, fullMethod, req, reply, cc, opts...)
		timer.Finish(grpcCodeString(err))
		return err
	}
}

// shortRPCMethod extracts the RPC method name from a full gRPC method
// path. gRPC formats the path as "/<package>.<Service>/<Method>"
// (e.g. "/provider.v1.Provider/Validate" -> "Validate"). Falls back to
// the input unchanged if the format is unexpected, keeping the metric
// label stable rather than silently dropping data.
func shortRPCMethod(fullMethod string) string {
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 && idx < len(fullMethod)-1 {
		return fullMethod[idx+1:]
	}
	return fullMethod
}

// grpcCodeString returns the stringified gRPC status code for the given
// error. nil error -> "OK". Non-gRPC errors -> "Unknown" (matching gRPC's
// own convention). The string is the canonical short form from
// codes.Code.String() so metric labels match the gRPC reference docs.
func grpcCodeString(err error) string {
	return status.Code(err).String()
}

// providerCircuitBreakerInterceptor returns a UnaryClientInterceptor that
// wraps every outbound RPC with the supplied CircuitBreaker. Implements
// G6 / #111.
//
// Behaviour:
//   - When the breaker is Closed or HalfOpen, the invoker runs normally.
//     If it returns an infrastructure-class error (see isInfraFailure),
//     that counts as a failure toward the breaker's threshold. Business
//     errors (NotFound, InvalidArgument, ...) are returned to the caller
//     unchanged AND do not trip the breaker — those are signs the
//     provider is healthy and the request was bad, not the other way.
//   - When the breaker is Open, the invoker does NOT run; the
//     interceptor synthesises a codes.Unavailable status so the rest of
//     the stack (the G4 metrics interceptor, c.mapGRPCError, callers
//     doing `errors.Is(err, contracts.RetryableError)`) all treat it
//     uniformly with any other "provider down" signal.
//
// The interceptor MUST NOT panic. If it did, every provider RPC would
// break. We deliberately keep it small and free of allocation-heavy work.
func providerCircuitBreakerInterceptor(cb *resilience.CircuitBreaker) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		fullMethod string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		var invokerErr error
		cbErr := cb.Call(ctx, func(ctx context.Context) error {
			invokerErr = invoker(ctx, fullMethod, req, reply, cc, opts...)
			// Only infra failures count toward the breaker's threshold.
			// Business errors are returned out-of-band via invokerErr.
			if isInfraFailure(invokerErr) {
				return invokerErr
			}
			return nil
		})
		// Circuit is open and rejected the call before the invoker ran:
		// cb.Call returned an error but invokerErr is still its zero value.
		// Synthesise a canonical gRPC Unavailable so downstream code paths
		// (metrics interceptor, mapGRPCError, retry loops) behave as if the
		// provider itself returned Unavailable.
		if cbErr != nil && invokerErr == nil {
			return status.Errorf(codes.Unavailable, "circuit breaker open: %v", cbErr)
		}
		return invokerErr
	}
}

// isInfraFailure reports whether the given error is an infrastructure-
// class gRPC failure that should count toward a CircuitBreaker's failure
// threshold. Treats the following codes as infra failures:
//
//   - Unavailable        — provider pod down, network partition
//   - DeadlineExceeded   — provider hung past the call timeout
//   - Internal           — provider crashed mid-call
//   - Unknown            — non-gRPC error from the transport layer
//
// Codes deliberately NOT counted as infra failures:
//
//   - OK                 — obviously a success
//   - Canceled           — caller gave up, not the provider failing
//   - NotFound,
//     InvalidArgument,
//     AlreadyExists,
//     FailedPrecondition,
//     PermissionDenied,
//     Unauthenticated    — application/business errors: the provider is
//     healthy, the request was bad
//   - ResourceExhausted  — rate-limit signal: caller should back off this
//     one call, not stop talking to the provider
//   - Aborted, OutOfRange,
//     Unimplemented      — protocol-level issues unrelated to provider
//     health
//
// The classification is opinionated; rationale is documented in the G6
// PR (#111) and CHANGELOG entry.
func isInfraFailure(err error) bool {
	if err == nil {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable,
		codes.DeadlineExceeded,
		codes.Internal,
		codes.Unknown:
		return true
	default:
		return false
	}
}

// recordVMOp records a virtrigaud_vm_operations_total sample for the
// given operation. G7.1 / #124.
//
// Designed to be called via `defer c.recordVMOp(metrics.OpCreate,
// &retErr)` from each VM-operation method, with a named `retErr` return
// value. The pointer-to-error indirection is required so the deferred
// call evaluates the FINAL return value of the enclosing function, not
// the value of err at defer-time (which would always be nil, since the
// defer runs before any explicit `return ...` evaluates).
//
// Nil-safe on c.vmOps (tests that construct &Client{...} directly
// bypass NewClient and may not have it set). Production clients via
// NewClient always have it initialised.
//
// Outcome derivation matches G1-G3's named-return reconcile pattern:
//   - retErr == nil  → metrics.OutcomeSuccess
//   - retErr != nil  → metrics.OutcomeError
//
// The provider/provider_type labels come from the values passed to
// NewVMOperationMetrics at NewClient construction time.
func (c *Client) recordVMOp(op string, retErr *error) {
	if c.vmOps == nil {
		return
	}
	outcome := metrics.OutcomeSuccess
	if retErr != nil && *retErr != nil {
		outcome = metrics.OutcomeError
	}
	c.vmOps.RecordOperation(op, outcome)
}

// trackTaskStart adds taskID to the inflight set and pushes the new
// gauge value to virtrigaud_provider_tasks_inflight{provider_type,
// provider}. G7.3 / #129.
//
// Called by every task-creating RPC right after a non-nil Task field
// is observed in the response. taskID = "" or c.tasks = nil are no-ops
// so the helper is safe under both the empty-TaskRef edge case and the
// &Client{...}-direct-construction test path.
//
// Re-adding an ID already in the set is a no-op for the gauge (set
// semantics) — this defends against pathological provider-server
// behaviour returning the same TaskRef from two RPCs.
func (c *Client) trackTaskStart(taskID string) {
	if c.tasks == nil || taskID == "" {
		return
	}
	c.inflightTasksMu.Lock()
	if c.inflightTasks == nil {
		c.inflightTasks = make(map[string]struct{})
	}
	if _, already := c.inflightTasks[taskID]; already {
		c.inflightTasksMu.Unlock()
		return
	}
	c.inflightTasks[taskID] = struct{}{}
	n := float64(len(c.inflightTasks))
	c.inflightTasksMu.Unlock()
	c.tasks.SetInflightTasks(n)
}

// trackTaskDone removes taskID from the inflight set and pushes the
// new gauge value. G7.3 / #129.
//
// Idempotent on unknown IDs (no-op): handles two real cases observed
// in production-style flows:
//  1. The reconciler crashes between observing Done=true and clearing
//     vm.Status.LastTaskRef — next reconcile polls again, gets Done=true
//     again, calls trackTaskDone again. Without this guard the gauge
//     would decrement to -1.
//  2. A new manager instance (post-restart) polls TaskStatus for a
//     vm.Status.LastTaskRef recorded by the previous instance. The new
//     instance's inflightTasks map starts empty, so the ID is unknown,
//     and the gauge stays at 0 — matching the documented semantic that
//     this gauge measures "tasks THIS manager instance is tracking",
//     not "tasks the provider believes are in-flight".
func (c *Client) trackTaskDone(taskID string) {
	if c.tasks == nil || taskID == "" {
		return
	}
	c.inflightTasksMu.Lock()
	if _, ok := c.inflightTasks[taskID]; !ok {
		c.inflightTasksMu.Unlock()
		return
	}
	delete(c.inflightTasks, taskID)
	n := float64(len(c.inflightTasks))
	c.inflightTasksMu.Unlock()
	c.tasks.SetInflightTasks(n)
}

// Close closes the gRPC connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// Validate implements contracts.Provider
func (c *Client) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.client.Validate(ctx, &providerv1.ValidateRequest{})
	if err != nil {
		return c.mapGRPCError("validate", err)
	}

	if !resp.Ok {
		return fmt.Errorf("provider validation failed: %s", resp.Message)
	}

	return nil
}

// Create implements contracts.Provider.
//
// Records virtrigaud_vm_operations_total{operation="Create",...} via
// deferred recordVMOp using the named retErr return value (G7.1 / #124).
func (c *Client) Create(ctx context.Context, req contracts.CreateRequest) (result contracts.CreateResponse, retErr error) {
	defer c.recordVMOp(metrics.OpCreate, &retErr)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	grpcReq, err := c.convertCreateRequest(req)
	if err != nil {
		return contracts.CreateResponse{}, fmt.Errorf("failed to convert create request: %w", err)
	}

	resp, err := c.client.Create(ctx, grpcReq)
	if err != nil {
		return contracts.CreateResponse{}, c.mapGRPCError("create", err)
	}

	result = contracts.CreateResponse{
		ID: resp.Id,
	}

	if resp.Task != nil {
		result.TaskRef = resp.Task.Id
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
	}

	return result, nil
}

// Delete implements contracts.Provider.
//
// Records virtrigaud_vm_operations_total{operation="Delete",...} via
// deferred recordVMOp using the named retErr return value (G7.1 / #124).
func (c *Client) Delete(ctx context.Context, id string) (taskRef string, retErr error) {
	defer c.recordVMOp(metrics.OpDelete, &retErr)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	resp, err := c.client.Delete(ctx, &providerv1.DeleteRequest{Id: id})
	if err != nil {
		return "", c.mapGRPCError("delete", err)
	}

	if resp.Task != nil {
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
		return resp.Task.Id, nil
	}

	return "", nil
}

// Power implements contracts.Provider.
//
// Records virtrigaud_vm_operations_total{operation="Power",...} via
// deferred recordVMOp using the named retErr return value (G7.1 / #124).
func (c *Client) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, retErr error) {
	defer c.recordVMOp(metrics.OpPower, &retErr)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	grpcOp, err := c.convertPowerOp(op)
	if err != nil {
		return "", fmt.Errorf("invalid power operation: %w", err)
	}

	powerReq := &providerv1.PowerRequest{
		Id: id,
		Op: grpcOp,
	}

	// Set default graceful timeout for graceful shutdown operations
	if op == contracts.PowerOpShutdownGraceful {
		powerReq.GracefulTimeoutSeconds = 60 // 60 seconds default
	}

	resp, err := c.client.Power(ctx, powerReq)
	if err != nil {
		return "", c.mapGRPCError("power", err)
	}

	if resp.Task != nil {
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
		return resp.Task.Id, nil
	}

	return "", nil
}

// Reconfigure implements contracts.Provider.
//
// Records virtrigaud_vm_operations_total{operation="Reconfigure",...}
// via deferred recordVMOp using the named retErr return value (G7.1 /
// #124).
func (c *Client) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, retErr error) {
	defer c.recordVMOp(metrics.OpReconfigure, &retErr)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return "", fmt.Errorf("failed to marshal desired configuration: %w", err)
	}

	resp, err := c.client.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          id,
		DesiredJson: string(desiredJSON),
	})
	if err != nil {
		return "", c.mapGRPCError("reconfigure", err)
	}

	if resp.Task != nil {
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
		return resp.Task.Id, nil
	}

	return "", nil
}

// Describe implements contracts.Provider.
//
// Records virtrigaud_vm_operations_total{operation="Describe",...} via
// deferred recordVMOp using the named retErr return value (G7.1 / #124).
func (c *Client) Describe(ctx context.Context, id string) (result contracts.DescribeResponse, retErr error) {
	defer c.recordVMOp(metrics.OpDescribe, &retErr)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.client.Describe(ctx, &providerv1.DescribeRequest{Id: id})
	if err != nil {
		return contracts.DescribeResponse{}, c.mapGRPCError("describe", err)
	}

	// Parse provider raw data
	var providerRaw map[string]string
	if resp.ProviderRawJson != "" {
		// First unmarshal to map[string]any, then convert to map[string]string
		var rawData map[string]any
		if err := json.Unmarshal([]byte(resp.ProviderRawJson), &rawData); err != nil {
			// Log error but don't fail the entire operation
			providerRaw = map[string]string{"parseError": err.Error()}
		} else {
			// Convert map[string]any to map[string]string
			providerRaw = make(map[string]string)
			for k, v := range rawData {
				providerRaw[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	return contracts.DescribeResponse{
		Exists:      resp.Exists,
		PowerState:  resp.PowerState,
		IPs:         resp.Ips,
		ConsoleURL:  resp.ConsoleUrl,
		ProviderRaw: providerRaw,
	}, nil
}

// IsTaskComplete implements contracts.Provider
func (c *Client) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := c.client.TaskStatus(ctx, &providerv1.TaskStatusRequest{
		Task: &providerv1.TaskRef{Id: taskRef},
	})
	if err != nil {
		return false, c.mapGRPCError("taskStatus", err)
	}

	if resp.Error != "" {
		// Terminal failure also counts as task-done — decrement the
		// inflight gauge before returning (G7.3 / #129).
		c.trackTaskDone(taskRef)
		return true, fmt.Errorf("task failed: %s", resp.Error)
	}

	if resp.Done {
		c.trackTaskDone(taskRef) // G7.3 (#129)
	}
	return resp.Done, nil
}

// TaskStatus checks the status of an async task
func (c *Client) TaskStatus(ctx context.Context, taskRef string) (contracts.TaskStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := c.client.TaskStatus(ctx, &providerv1.TaskStatusRequest{
		Task: &providerv1.TaskRef{Id: taskRef},
	})
	if err != nil {
		return contracts.TaskStatus{}, c.mapGRPCError("taskStatus", err)
	}

	// Both terminal-success (Done=true) and terminal-failure (Error != "")
	// flip the task out of the inflight set (G7.3 / #129). Idempotent on
	// unknown taskRef so double-poll / post-restart polls are safe.
	if resp.Done || resp.Error != "" {
		c.trackTaskDone(taskRef)
	}

	return contracts.TaskStatus{
		IsCompleted: resp.Done,
		Error:       resp.Error,
		Message:     "", // Message field not in proto
	}, nil
}

// SnapshotCreate creates a VM snapshot
func (c *Client) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	grpcReq := &providerv1.SnapshotCreateRequest{
		VmId:          req.VmId,
		NameHint:      req.NameHint,
		Description:   req.Description,
		IncludeMemory: req.IncludeMemory,
		// Note: Quiesce not in proto yet, would need to add to provider.proto
	}

	resp, err := c.client.SnapshotCreate(ctx, grpcReq)
	if err != nil {
		return contracts.SnapshotCreateResponse{}, c.mapGRPCError("snapshotCreate", err)
	}

	result := contracts.SnapshotCreateResponse{
		SnapshotId: resp.SnapshotId,
	}

	if resp.Task != nil {
		result.Task = &contracts.TaskRef{
			ID: resp.Task.Id,
		}
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
	}

	return result, nil
}

// SnapshotDelete deletes a VM snapshot
func (c *Client) SnapshotDelete(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	grpcReq := &providerv1.SnapshotDeleteRequest{
		VmId:       vmId,
		SnapshotId: snapshotId,
	}

	resp, err := c.client.SnapshotDelete(ctx, grpcReq)
	if err != nil {
		return "", c.mapGRPCError("snapshotDelete", err)
	}

	if resp.Task != nil {
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
		return resp.Task.Id, nil
	}

	return "", nil
}

// SnapshotRevert reverts a VM to a snapshot
func (c *Client) SnapshotRevert(ctx context.Context, vmId string, snapshotId string) (taskRef string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	grpcReq := &providerv1.SnapshotRevertRequest{
		VmId:       vmId,
		SnapshotId: snapshotId,
	}

	resp, err := c.client.SnapshotRevert(ctx, grpcReq)
	if err != nil {
		return "", c.mapGRPCError("snapshotRevert", err)
	}

	if resp.Task != nil {
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
		return resp.Task.Id, nil
	}

	return "", nil
}

// ExportDisk exports a VM disk for migration
func (c *Client) ExportDisk(ctx context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute) // Long timeout for disk export
	defer cancel()

	grpcReq := &providerv1.ExportDiskRequest{
		VmId:           req.VmId,
		DiskId:         req.DiskId,
		SnapshotId:     req.SnapshotId,
		DestinationUrl: req.DestinationURL,
		Format:         req.Format,
		Compress:       req.Compress,
		Credentials:    req.Credentials,
	}

	resp, err := c.client.ExportDisk(ctx, grpcReq)
	if err != nil {
		return contracts.ExportDiskResponse{}, c.mapGRPCError("exportDisk", err)
	}

	result := contracts.ExportDiskResponse{
		ExportId:           resp.ExportId,
		EstimatedSizeBytes: resp.EstimatedSizeBytes,
		Checksum:           resp.Checksum,
	}

	if resp.Task != nil {
		result.TaskRef = resp.Task.Id
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
	}

	return result, nil
}

// ImportDisk imports a disk from an external source
func (c *Client) ImportDisk(ctx context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute) // Long timeout for disk import
	defer cancel()

	grpcReq := &providerv1.ImportDiskRequest{
		SourceUrl:        req.SourceURL,
		StorageHint:      req.StorageHint,
		Format:           req.Format,
		TargetName:       req.TargetName,
		VerifyChecksum:   req.VerifyChecksum,
		ExpectedChecksum: req.ExpectedChecksum,
		Credentials:      req.Credentials,
	}

	resp, err := c.client.ImportDisk(ctx, grpcReq)
	if err != nil {
		return contracts.ImportDiskResponse{}, c.mapGRPCError("importDisk", err)
	}

	result := contracts.ImportDiskResponse{
		DiskId:          resp.DiskId,
		Path:            resp.Path,
		ActualSizeBytes: resp.ActualSizeBytes,
		Checksum:        resp.Checksum,
	}

	if resp.Task != nil {
		result.TaskRef = resp.Task.Id
		c.trackTaskStart(resp.Task.Id) // G7.3 (#129)
	}

	return result, nil
}

// GetDiskInfo retrieves detailed information about a VM disk
func (c *Client) GetDiskInfo(ctx context.Context, req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	grpcReq := &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	}

	resp, err := c.client.GetDiskInfo(ctx, grpcReq)
	if err != nil {
		return contracts.GetDiskInfoResponse{}, c.mapGRPCError("getDiskInfo", err)
	}

	result := contracts.GetDiskInfoResponse{
		DiskId:           resp.DiskId,
		Format:           resp.Format,
		VirtualSizeBytes: resp.VirtualSizeBytes,
		ActualSizeBytes:  resp.ActualSizeBytes,
		Path:             resp.Path,
		IsBootable:       resp.IsBootable,
		Snapshots:        resp.Snapshots,
		BackingFile:      resp.BackingFile,
		Metadata:         resp.Metadata,
	}

	return result, nil
}

// ListVMs implements contracts.Provider
func (c *Client) ListVMs(ctx context.Context) ([]contracts.VMInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	resp, err := c.client.ListVMs(ctx, &providerv1.ListVMsRequest{})
	if err != nil {
		return nil, c.mapGRPCError("listVMs", err)
	}

	// Convert proto VMInfo to contracts VMInfo
	var vmInfos []contracts.VMInfo
	for _, protoVM := range resp.Vms {
		// Convert disks
		var disks []contracts.DiskInfo
		for _, protoDisk := range protoVM.Disks {
			disks = append(disks, contracts.DiskInfo{
				ID:      protoDisk.Id,
				Path:    protoDisk.Path,
				SizeGiB: protoDisk.SizeGib,
				Format:  protoDisk.Format,
			})
		}

		// Convert networks
		var networks []contracts.NetworkInfo
		for _, protoNet := range protoVM.Networks {
			networks = append(networks, contracts.NetworkInfo{
				Name:      protoNet.Name,
				MAC:       protoNet.Mac,
				IPAddress: protoNet.IpAddress,
			})
		}

		vmInfo := contracts.VMInfo{
			ID:          protoVM.Id,
			Name:        protoVM.Name,
			PowerState:  protoVM.PowerState,
			IPs:         protoVM.Ips,
			CPU:         protoVM.Cpu,
			MemoryMiB:   protoVM.MemoryMib,
			Disks:       disks,
			Networks:    networks,
			ProviderRaw: protoVM.ProviderRaw,
		}

		vmInfos = append(vmInfos, vmInfo)
	}

	return vmInfos, nil
}

// convertCreateRequest converts contracts.CreateRequest to gRPC format
func (c *Client) convertCreateRequest(req contracts.CreateRequest) (*providerv1.CreateRequest, error) {
	grpcReq := &providerv1.CreateRequest{
		Name: req.Name,
		Tags: req.Tags,
	}

	// Convert UserData
	if req.UserData != nil {
		grpcReq.UserData = []byte(req.UserData.CloudInitData)

		// Convert MetaData
		if req.MetaData != nil {
			grpcReq.MetaData = []byte(req.MetaData.MetaDataYAML)
		}
	}

	// Convert each component to JSON
	if classData, err := json.Marshal(req.Class); err == nil {
		grpcReq.ClassJson = string(classData)
	}

	if imageData, err := json.Marshal(req.Image); err == nil {
		grpcReq.ImageJson = string(imageData)
	}

	if networksData, err := json.Marshal(req.Networks); err == nil {
		grpcReq.NetworksJson = string(networksData)
	}

	if disksData, err := json.Marshal(req.Disks); err == nil {
		grpcReq.DisksJson = string(disksData)
		if len(req.Disks) > 0 {
			fmt.Printf("gRPC Client: DisksJson being sent to provider: disk_count=%d disks_json=%s\n",
				len(req.Disks), string(disksData))
		}
	}

	if req.Placement != nil {
		if placementData, err := json.Marshal(req.Placement); err == nil {
			grpcReq.PlacementJson = string(placementData)
		}
	}

	return grpcReq, nil
}

// convertPowerOp converts contracts.PowerOp to gRPC format
func (c *Client) convertPowerOp(op contracts.PowerOp) (providerv1.PowerOp, error) {
	switch op {
	case contracts.PowerOpOn:
		return providerv1.PowerOp_POWER_OP_ON, nil
	case contracts.PowerOpOff:
		return providerv1.PowerOp_POWER_OP_OFF, nil
	case contracts.PowerOpReboot:
		return providerv1.PowerOp_POWER_OP_REBOOT, nil
	case contracts.PowerOpShutdownGraceful:
		return providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL, nil
	default:
		return providerv1.PowerOp_POWER_OP_UNSPECIFIED, fmt.Errorf("unsupported power operation: %s", op)
	}
}

// mapGRPCError converts gRPC errors to contracts errors where possible
func (c *Client) mapGRPCError(operation string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s failed: %w", operation, err)
	}

	switch st.Code() {
	case codes.NotFound:
		return contracts.NewNotFoundError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	case codes.InvalidArgument:
		return contracts.NewInvalidSpecError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	case codes.Unavailable, codes.DeadlineExceeded:
		return contracts.NewRetryableError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	default:
		return fmt.Errorf("%s failed: %s", operation, st.Message())
	}
}

// TLSConfig represents TLS configuration for gRPC clients.
//
// Two construction styles are supported, in priority order:
//
//  1. PrebuiltConfig — a fully assembled *tls.Config produced upstream
//     (e.g. by Resolver.buildTLSConfig loading cert/key/ca PEM material
//     from a Kubernetes Secret). When non-nil this field wins; all
//     file-path fields below are ignored. This is the path used by the
//     v0.3.7 mTLS wiring (ADR-0003 / umbrella #156).
//  2. CertFile/KeyFile/CAFile — paths to PEM files on the local
//     filesystem. Kept for the local-dev / on-disk-cert workflow.
//
// Insecure short-circuits both paths and produces a TLS config that
// skips peer verification — dev-only escape hatch, never set in
// production.
type TLSConfig struct {
	// PrebuiltConfig, when non-nil, is used as-is to build gRPC
	// transport credentials. The caller is responsible for setting
	// MinVersion, Certificates, RootCAs, ServerName, and any other
	// fields it cares about.
	PrebuiltConfig *tls.Config

	CertFile string
	KeyFile  string
	CAFile   string
	Insecure bool
}

// buildTLSCredentials builds gRPC transport credentials from TLS config.
//
// When config.PrebuiltConfig is non-nil it is honored verbatim — the
// file-path fields are ignored. Otherwise the old on-disk loading path
// is used, preserving backwards compatibility for any caller still
// passing file paths.
func buildTLSCredentials(config *TLSConfig) (credentials.TransportCredentials, error) {
	if config.PrebuiltConfig != nil {
		return credentials.NewTLS(config.PrebuiltConfig), nil
	}

	if config.Insecure {
		return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}), nil
	}

	tlsConfig := &tls.Config{}

	// Load client certificate if provided
	if config.CertFile != "" && config.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided
	if config.CAFile != "" {
		caCert, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return credentials.NewTLS(tlsConfig), nil
}
