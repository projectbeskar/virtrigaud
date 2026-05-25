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

package grpc

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fakeVMOpsServer implements enough of providerv1.ProviderServer to drive
// the five VM-operation methods (Create / Delete / Power / Describe /
// Reconfigure) exercised by G7.1 wiring. fail toggles every handler's
// response between success and an Unavailable error so a single bufconn
// server can produce both metric outcomes without re-instantiating.
type fakeVMOpsServer struct {
	providerv1.UnimplementedProviderServer
	fail bool
}

func (f *fakeVMOpsServer) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	if f.fail {
		return nil, status.Error(codes.Unavailable, "induced create failure")
	}
	return &providerv1.CreateResponse{Id: "test-vm-id"}, nil
}

func (f *fakeVMOpsServer) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	if f.fail {
		return nil, status.Error(codes.Unavailable, "induced delete failure")
	}
	return &providerv1.TaskResponse{}, nil
}

func (f *fakeVMOpsServer) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	if f.fail {
		return nil, status.Error(codes.Unavailable, "induced power failure")
	}
	return &providerv1.TaskResponse{}, nil
}

func (f *fakeVMOpsServer) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if f.fail {
		return nil, status.Error(codes.Unavailable, "induced describe failure")
	}
	return &providerv1.DescribeResponse{Exists: true, PowerState: "On"}, nil
}

func (f *fakeVMOpsServer) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	if f.fail {
		return nil, status.Error(codes.Unavailable, "induced reconfigure failure")
	}
	return &providerv1.TaskResponse{}, nil
}

// newTestClientForVMOps brings up an in-process gRPC server backed by
// bufconn and returns a Client wired to it. vmOps is initialised with
// the supplied provider labels so vm_operations_total samples carry the
// right { provider_type, provider } pair for the assertions.
//
// This intentionally does NOT chain the CircuitBreaker interceptor —
// these tests cover the G7.1 wiring path without entangling it with G6
// semantics.
func newTestClientForVMOps(t *testing.T, srv providerv1.ProviderServer, providerType, providerName string) *Client {
	t.Helper()
	dialer, cleanup := startBufconnServer(t, srv)
	t.Cleanup(cleanup)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dialer(ctx, "") }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(providerRPCMetricsInterceptor(providerType)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{
		conn:   conn,
		client: providerv1.NewProviderClient(conn),
		vmOps:  metrics.NewVMOperationMetrics(providerType, providerName),
	}
}

// TestRecordVMOp_NilVMOpsIsSafe pins the nil-safety contract on
// recordVMOp. Tests that construct &Client{...} directly (bypassing
// NewClient) may leave vmOps unset; the helper must not panic.
//
// G7.1 / #124.
func TestRecordVMOp_NilVMOpsIsSafe(t *testing.T) {
	c := &Client{} // vmOps left nil on purpose
	var nilErr error
	// Must not panic. Outcome derivation is irrelevant when vmOps is nil.
	c.recordVMOp(metrics.OpCreate, &nilErr)

	err := assert.AnError
	c.recordVMOp(metrics.OpDelete, &err)
}

// TestRecordVMOp_OutcomeDerivation pins the success vs error contract.
// A nil *error gives Success; a non-nil *error containing a real error
// gives Error; a non-nil *error containing nil gives Success.
//
// G7.1 / #124.
func TestRecordVMOp_OutcomeDerivation(t *testing.T) {
	const providerType, provider = "g71-derivation", "g71-derivation-provider"
	c := &Client{vmOps: metrics.NewVMOperationMetrics(providerType, provider)}

	wantSuccess := map[string]string{
		"operation": metrics.OpCreate, "provider_type": providerType,
		"provider": provider, "outcome": metrics.OutcomeSuccess,
	}
	wantError := map[string]string{
		"operation": metrics.OpDelete, "provider_type": providerType,
		"provider": provider, "outcome": metrics.OutcomeError,
	}

	successBefore := counterSampleByLabels(t, "virtrigaud_vm_operations_total", wantSuccess)
	errorBefore := counterSampleByLabels(t, "virtrigaud_vm_operations_total", wantError)

	// Nil retErr pointer → treated as success (defensive path).
	c.recordVMOp(metrics.OpCreate, nil)
	// Non-nil pointer to nil error → success.
	var nilErr error
	c.recordVMOp(metrics.OpCreate, &nilErr)
	// Non-nil pointer to real error → error.
	realErr := assert.AnError
	c.recordVMOp(metrics.OpDelete, &realErr)

	successAfter := counterSampleByLabels(t, "virtrigaud_vm_operations_total", wantSuccess)
	errorAfter := counterSampleByLabels(t, "virtrigaud_vm_operations_total", wantError)

	assert.Equal(t, successBefore+2, successAfter,
		"two nil-error calls must each increment the success sample")
	assert.Equal(t, errorBefore+1, errorAfter,
		"one real-error call must increment the error sample exactly once")
}

// TestClient_VMOperations_RecordOnSuccess verifies the five VM-
// operation methods each increment
// virtrigaud_vm_operations_total{operation=<Op>, outcome="success"} by
// exactly one when the underlying gRPC call returns success.
//
// G7.1 / #124. Pinned per-operation so a regression that drops the
// defer on (say) Reconfigure but not the others is caught.
func TestClient_VMOperations_RecordOnSuccess(t *testing.T) {
	const providerType, provider = "g71-success", "g71-success-provider"
	cli := newTestClientForVMOps(t, &fakeVMOpsServer{fail: false}, providerType, provider)
	ctx := context.Background()

	tests := []struct {
		name string
		op   string
		fn   func() error
	}{
		{"Create", metrics.OpCreate, func() error {
			_, e := cli.Create(ctx, contracts.CreateRequest{Name: "vm-1"})
			return e
		}},
		{"Delete", metrics.OpDelete, func() error {
			_, e := cli.Delete(ctx, "vm-1")
			return e
		}},
		{"Power", metrics.OpPower, func() error {
			_, e := cli.Power(ctx, "vm-1", contracts.PowerOpOn)
			return e
		}},
		{"Describe", metrics.OpDescribe, func() error {
			_, e := cli.Describe(ctx, "vm-1")
			return e
		}},
		{"Reconfigure", metrics.OpReconfigure, func() error {
			_, e := cli.Reconfigure(ctx, "vm-1", contracts.CreateRequest{Name: "vm-1"})
			return e
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := map[string]string{
				"operation":     tt.op,
				"provider_type": providerType,
				"provider":      provider,
				"outcome":       metrics.OutcomeSuccess,
			}
			before := counterSampleByLabels(t, "virtrigaud_vm_operations_total", want)
			require.NoError(t, tt.fn(), "%s call must succeed against the fake server", tt.name)
			after := counterSampleByLabels(t, "virtrigaud_vm_operations_total", want)
			assert.Equal(t, before+1, after,
				"%s success must increment virtrigaud_vm_operations_total{operation=%s, outcome=success} by 1",
				tt.name, tt.op)
		})
	}
}

// TestClient_VMOperations_RecordOnError verifies the five VM-operation
// methods each increment
// virtrigaud_vm_operations_total{operation=<Op>, outcome="error"} by
// exactly one when the underlying gRPC call returns an error.
//
// G7.1 / #124. Catches the regression class where outcome derivation
// inverts (success becomes error or vice-versa).
func TestClient_VMOperations_RecordOnError(t *testing.T) {
	const providerType, provider = "g71-error", "g71-error-provider"
	cli := newTestClientForVMOps(t, &fakeVMOpsServer{fail: true}, providerType, provider)
	ctx := context.Background()

	tests := []struct {
		name string
		op   string
		fn   func() error
	}{
		{"Create", metrics.OpCreate, func() error {
			_, e := cli.Create(ctx, contracts.CreateRequest{Name: "vm-1"})
			return e
		}},
		{"Delete", metrics.OpDelete, func() error {
			_, e := cli.Delete(ctx, "vm-1")
			return e
		}},
		{"Power", metrics.OpPower, func() error {
			_, e := cli.Power(ctx, "vm-1", contracts.PowerOpOn)
			return e
		}},
		{"Describe", metrics.OpDescribe, func() error {
			_, e := cli.Describe(ctx, "vm-1")
			return e
		}},
		{"Reconfigure", metrics.OpReconfigure, func() error {
			_, e := cli.Reconfigure(ctx, "vm-1", contracts.CreateRequest{Name: "vm-1"})
			return e
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := map[string]string{
				"operation":     tt.op,
				"provider_type": providerType,
				"provider":      provider,
				"outcome":       metrics.OutcomeError,
			}
			before := counterSampleByLabels(t, "virtrigaud_vm_operations_total", want)
			require.Error(t, tt.fn(), "%s call must surface the induced Unavailable error", tt.name)
			after := counterSampleByLabels(t, "virtrigaud_vm_operations_total", want)
			assert.Equal(t, before+1, after,
				"%s error must increment virtrigaud_vm_operations_total{operation=%s, outcome=error} by 1",
				tt.name, tt.op)
		})
	}
}
