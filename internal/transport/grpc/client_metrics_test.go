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
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestShortRPCMethod covers the path-stripping helper that turns gRPC's
// full method path "/<pkg>.<Service>/<Method>" into just "<Method>" for
// use as a metric label. Includes a few edge cases to pin behavior.
func TestShortRPCMethod(t *testing.T) {
	tests := []struct {
		name string
		full string
		want string
	}{
		{"standard gRPC method path", "/provider.v1.Provider/Validate", "Validate"},
		{"another method", "/provider.v1.Provider/SnapshotCreate", "SnapshotCreate"},
		{"deeper package path", "/foo.bar.baz.Service/Method", "Method"},
		{"no slash returns input unchanged", "BareName", "BareName"},
		{"trailing slash falls back to full input", "/provider.v1.Provider/", "/provider.v1.Provider/"},
		{"empty string returns empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shortRPCMethod(tt.full))
		})
	}
}

// TestGrpcCodeString verifies the err -> code-string mapping used by
// the metric label. Pins canonical gRPC strings so dashboards keep working.
func TestGrpcCodeString(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error is OK", nil, codes.OK.String()},
		{"unavailable", status.Error(codes.Unavailable, "down"), codes.Unavailable.String()},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "timeout"), codes.DeadlineExceeded.String()},
		{"not found", status.Error(codes.NotFound, "missing"), codes.NotFound.String()},
		{"non-grpc error becomes Unknown", errors.New("raw"), codes.Unknown.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, grpcCodeString(tt.err))
		})
	}
}

// counterSampleByLabels returns the current value of a counter sample
// matching the given metric family name and labels. Helper for this file
// only — controller-package tests have their own copy under a different
// import path; intentionally not exported because metrics_test cross-file
// helpers shouldn't leak into production code.
func counterSampleByLabels(t *testing.T, family string, want map[string]string) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatchPairs(m.GetLabel(), want) {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
			}
		}
	}
	return 0
}

func labelsMatchPairs(got []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, lp := range got {
			if lp.GetName() == k && lp.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fakeProviderServer implements providerv1.ProviderServer for the
// interceptor integration tests. ValidateFn is configurable so tests
// can simulate success and error responses.
type fakeProviderServer struct {
	providerv1.UnimplementedProviderServer
	ValidateFn func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error)
}

func (f *fakeProviderServer) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	if f.ValidateFn != nil {
		return f.ValidateFn(ctx, req)
	}
	return &providerv1.ValidateResponse{Ok: true}, nil
}

// startBufconnServer brings up an in-process gRPC server backed by
// google.golang.org/grpc/test/bufconn. Returns a dialer + cleanup func.
// Avoids touching real TCP for the integration tests.
func startBufconnServer(t *testing.T, srv providerv1.ProviderServer) (func(context.Context, string) (net.Conn, error), func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	cleanup := func() {
		gsrv.Stop()
		_ = lis.Close()
	}
	return dialer, cleanup
}

// newTestClient builds a *Client wired to the in-process bufconn server,
// with the production interceptor in place. Used by integration tests
// to verify the interceptor emits the right metric samples.
func newTestClient(t *testing.T, dialer func(context.Context, string) (net.Conn, error), providerType string) *Client {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(providerRPCMetricsInterceptor(providerType)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	// vmOps initialised with providerType + empty providerName — keeps
	// recordVMOp's defer in production methods nil-safe for this test
	// path (G7.1 / #124). Tests asserting on vm_operations_total labels
	// use a non-empty providerName per-call.
	return &Client{
		conn:   conn,
		client: providerv1.NewProviderClient(conn),
		vmOps:  metrics.NewVMOperationMetrics(providerType, ""),
	}
}

// TestProviderRPCMetricsInterceptor_SuccessfulCall verifies that a
// normal RPC records virtrigaud_provider_rpc_requests_total{provider_type,
// method,code="OK"} += 1 and observes the latency histogram.
func TestProviderRPCMetricsInterceptor_SuccessfulCall(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			return &providerv1.ValidateResponse{Ok: true}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-success")

	wantLabels := map[string]string{
		"provider_type": "test-success",
		"method":        "Validate",
		"code":          codes.OK.String(),
	}
	before := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)

	err := cli.Validate(context.Background())
	require.NoError(t, err)

	after := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)
	assert.Equal(t, before+1, after,
		"successful Validate RPC should increment requests_total{code=OK} by 1")
}

// TestProviderRPCMetricsInterceptor_ErrorPropagatesCode verifies that
// gRPC errors are recorded with their canonical code string. Important
// for operators alerting on `code=~"Unavailable|DeadlineExceeded|..."`.
func TestProviderRPCMetricsInterceptor_ErrorPropagatesCode(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			return nil, status.Error(codes.Unavailable, "provider is down")
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-error")

	wantLabels := map[string]string{
		"provider_type": "test-error",
		"method":        "Validate",
		"code":          codes.Unavailable.String(),
	}
	before := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)

	err := cli.Validate(context.Background())
	require.Error(t, err)

	after := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)
	assert.Equal(t, before+1, after,
		"Unavailable RPC should increment requests_total{code=Unavailable} by 1")
}

// TestProviderRPCMetricsInterceptor_LatencyHistogramFires — smoke that
// the latency histogram receives at least one observation per RPC call.
func TestProviderRPCMetricsInterceptor_LatencyHistogramFires(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-latency")

	err := cli.Validate(context.Background())
	require.NoError(t, err)

	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)

	var sampleCount uint64
	for _, f := range families {
		if f.GetName() != "virtrigaud_provider_rpc_latency_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatchPairs(m.GetLabel(), map[string]string{
				"provider_type": "test-latency",
				"method":        "Validate",
			}) {
				if h := m.GetHistogram(); h != nil {
					sampleCount += h.GetSampleCount()
				}
			}
		}
	}
	assert.Greater(t, sampleCount, uint64(0),
		"virtrigaud_provider_rpc_latency_seconds{provider_type=test-latency,method=Validate} should have at least one observation")
}

// TestProviderRPCMetricsInterceptor_DeadlineExceeded — pins the deadline-
// exceeded case, which is what happens in production when a provider
// goes silent and the client's ctx times out before the server responds.
func TestProviderRPCMetricsInterceptor_DeadlineExceeded(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ValidateFn: func(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
			// Sleep longer than the client's deadline.
			select {
			case <-time.After(2 * time.Second):
				return &providerv1.ValidateResponse{Ok: true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-deadline")

	wantLabels := map[string]string{
		"provider_type": "test-deadline",
		"method":        "Validate",
		"code":          codes.DeadlineExceeded.String(),
	}
	before := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)

	// 50ms deadline — much shorter than the server's 2s sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := cli.Validate(ctx)
	require.Error(t, err)

	after := counterSampleByLabels(t, "virtrigaud_provider_rpc_requests_total", wantLabels)
	assert.Equal(t, before+1, after,
		"deadline-exceeded RPC should increment requests_total{code=DeadlineExceeded} by 1")
}
