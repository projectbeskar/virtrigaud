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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// gaugeSample returns the current gauge value for the given metric
// family matching the supplied labels. Returns 0 when no matching
// sample exists (so before/after subtraction yields the delta).
// Local to this file to avoid coupling with the resilience package's
// own gaugeSample helper.
func gaugeSample(t *testing.T, family string, want map[string]string) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatchPairs(m.GetLabel(), want) {
				if g := m.GetGauge(); g != nil {
					return g.GetValue()
				}
			}
		}
	}
	return 0
}

// _ keeps the dto import live for gaugeSample (which calls
// m.GetGauge().GetValue() on *dto.Metric).
var _ = (*dto.Histogram)(nil)

// fakeTasksServer drives the G7.3 lifecycle tests. CreateTaskID is the
// TaskRef ID returned from Create; DoneOnPoll determines whether the
// next TaskStatus poll reports Done=true.
type fakeTasksServer struct {
	providerv1.UnimplementedProviderServer
	CreateTaskID string
	DoneOnPoll   bool
}

func (f *fakeTasksServer) Create(_ context.Context, _ *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	resp := &providerv1.CreateResponse{Id: "vm-test"}
	if f.CreateTaskID != "" {
		resp.Task = &providerv1.TaskRef{Id: f.CreateTaskID}
	}
	return resp, nil
}

func (f *fakeTasksServer) TaskStatus(_ context.Context, _ *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	return &providerv1.TaskStatusResponse{Done: f.DoneOnPoll}, nil
}

// newTestClientForTasks wires up an in-process gRPC client with the
// G7.3 task tracker fully populated. Provider labels are caller-
// supplied so each test pins its own metric sample.
func newTestClientForTasks(t *testing.T, srv providerv1.ProviderServer, providerType, providerName string) *Client {
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

	taskMetrics := metrics.NewTaskMetrics(providerType, providerName)
	taskMetrics.SetInflightTasks(0) // seed the gauge so it appears in /metrics
	return &Client{
		conn:          conn,
		client:        providerv1.NewProviderClient(conn),
		vmOps:         metrics.NewVMOperationMetrics(providerType, providerName),
		tasks:         taskMetrics,
		inflightTasks: make(map[string]struct{}),
	}
}

// TestTrackTask_NilTasksIsSafe pins the nil-safety contract on both
// helpers. Test clients that construct &Client{...} directly may leave
// the tasks field nil; neither helper should panic. G7.3 / #129.
func TestTrackTask_NilTasksIsSafe(t *testing.T) {
	c := &Client{} // tasks left nil on purpose
	// Must not panic on any of these calls.
	c.trackTaskStart("any-id")
	c.trackTaskStart("")
	c.trackTaskDone("any-id")
	c.trackTaskDone("")
}

// TestTrackTask_StartAndDoneCycle pins the success path:
//
//	Start("a") → gauge = 1
//	Start("b") → gauge = 2
//	Done("a")  → gauge = 1
//	Done("b")  → gauge = 0
//
// G7.3 / #129.
func TestTrackTask_StartAndDoneCycle(t *testing.T) {
	const providerType, provider = "g73-cycle", "g73-cycle-provider"
	c := newClientWithTaskMetrics(providerType, provider)
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	c.trackTaskStart("a")
	assert.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"gauge must be 1 after starting one task")

	c.trackTaskStart("b")
	assert.Equal(t, 2.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"gauge must be 2 after starting a second task")

	c.trackTaskDone("a")
	assert.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"gauge must drop to 1 after first done")

	c.trackTaskDone("b")
	assert.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"gauge must drop to 0 after all tasks done")
}

// TestTrackTask_DoubleDoneIsIdempotent pins the double-poll contract:
// calling Done twice for the same taskID must decrement at most once.
// This catches the reconciler-retry-between-observing-done-and-clearing-
// status-ref class of regression. G7.3 / #129.
func TestTrackTask_DoubleDoneIsIdempotent(t *testing.T) {
	const providerType, provider = "g73-double", "g73-double-provider"
	c := newClientWithTaskMetrics(providerType, provider)
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	c.trackTaskStart("a")
	require.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels))

	c.trackTaskDone("a")
	c.trackTaskDone("a") // second done — must be no-op
	c.trackTaskDone("a") // third done — still no-op

	assert.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"repeated Done calls for same taskID must not push gauge below 0")
}

// TestTrackTask_UnknownDoneIsNoop pins the post-restart contract: a
// new manager instance polls TaskStatus for a vm.Status.LastTaskRef
// recorded by a previous instance. The new instance's set is empty;
// Done on an unknown ID must NOT push the gauge negative. G7.3 / #129.
func TestTrackTask_UnknownDoneIsNoop(t *testing.T) {
	const providerType, provider = "g73-unknown", "g73-unknown-provider"
	c := newClientWithTaskMetrics(providerType, provider)
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	require.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"precondition: seeded gauge must be 0")

	c.trackTaskDone("never-seen-this-id")
	c.trackTaskDone("nor-this-one")

	assert.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"Done on unknown IDs must NOT push the gauge negative")
}

// TestTrackTask_StartIdempotentOnSameID pins the defensive
// re-start-with-same-ID path: should not double-count, gauge stays
// at +1 not +2. G7.3 / #129.
func TestTrackTask_StartIdempotentOnSameID(t *testing.T) {
	const providerType, provider = "g73-restart", "g73-restart-provider"
	c := newClientWithTaskMetrics(providerType, provider)
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	c.trackTaskStart("a")
	c.trackTaskStart("a") // repeated — should not double-count
	c.trackTaskStart("a") // still should not double-count

	assert.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"repeated Start with same ID must keep gauge at 1, not 3")
}

// TestTrackTask_ConcurrentStartDoneIsRaceFree pushes the mutex hard:
// many goroutines start and complete tasks concurrently. End state
// must be 0 with no panic and no race detection failure (run with
// `go test -race`). G7.3 / #129.
func TestTrackTask_ConcurrentStartDoneIsRaceFree(t *testing.T) {
	const providerType, provider = "g73-concurrent", "g73-concurrent-provider"
	c := newClientWithTaskMetrics(providerType, provider)
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	const N = 100
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for i := 0; i < N; i++ {
		id := "task-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/(26*26))%26))
		go func(id string) {
			defer wg.Done()
			c.trackTaskStart(id)
		}(id)
		go func(id string) {
			defer wg.Done()
			c.trackTaskDone(id)
		}(id)
	}
	wg.Wait()

	// The Start/Done pairs interleave non-deterministically. After all
	// goroutines settle, the gauge MAY be > 0 if some Done calls
	// happened before their Start (no-op on unknown ID), so we drain by
	// running Done again for each id sequentially.
	for i := 0; i < N; i++ {
		id := "task-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/(26*26))%26))
		c.trackTaskDone(id)
	}
	assert.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"after sequential drain, gauge must settle at 0")
}

// TestClient_TaskLifecycle_EndToEnd exercises the integration path:
// Create returns Task → gauge goes to 1; IsTaskComplete returns Done=true
// → gauge drops to 0. This is the canary that catches if either side of
// the Inc/Dec wiring is dropped in a future refactor. G7.3 / #129.
func TestClient_TaskLifecycle_EndToEnd(t *testing.T) {
	const providerType, provider = "g73-e2e", "g73-e2e-provider"
	labels := map[string]string{"provider_type": providerType, "provider": provider}

	srv := &fakeTasksServer{CreateTaskID: "lifecycle-task-1", DoneOnPoll: false}
	cli := newTestClientForTasks(t, srv, providerType, provider)
	ctx := context.Background()

	require.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"precondition: seeded gauge must be 0")

	// Create with Task → gauge increments to 1.
	resp, err := cli.Create(ctx, contracts.CreateRequest{Name: "vm-test"})
	require.NoError(t, err)
	require.Equal(t, "lifecycle-task-1", resp.TaskRef, "fake server must echo the task id")
	assert.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"Create returning a Task must push gauge to 1")

	// Poll: not done yet → gauge unchanged.
	done, err := cli.IsTaskComplete(ctx, "lifecycle-task-1")
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, 1.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"polling a not-yet-done task must leave gauge unchanged")

	// Flip the server to Done; next poll terminates the task.
	srv.DoneOnPoll = true
	done, err = cli.IsTaskComplete(ctx, "lifecycle-task-1")
	require.NoError(t, err)
	require.True(t, done)
	assert.Equal(t, 0.0, gaugeSample(t, "virtrigaud_provider_tasks_inflight", labels),
		"poll observing Done=true must decrement gauge to 0")
}

// newClientWithTaskMetrics builds a minimal Client with only the
// fields needed to exercise trackTaskStart / trackTaskDone (no real
// gRPC connection — these tests pin the in-process state machine,
// not the network path). Helper local to this file so the unit-level
// tests stay loosely coupled from the integration-style helpers.
func newClientWithTaskMetrics(providerType, provider string) *Client {
	tm := metrics.NewTaskMetrics(providerType, provider)
	tm.SetInflightTasks(0) // seed the gauge so gaugeSample finds the label set
	return &Client{
		tasks:         tm,
		inflightTasks: make(map[string]struct{}),
	}
}
