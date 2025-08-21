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

package metrics

import (
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// Build information
	buildInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_build_info",
			Help: "Build information for virtrigaud components",
		},
		[]string{"version", "git_sha", "go_version", "component"},
	)

	// Manager metrics
	managerReconcileTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_manager_reconcile_total",
			Help: "Total number of reconcile operations by kind and outcome",
		},
		[]string{"kind", "outcome"},
	)

	managerReconcileDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "virtrigaud_manager_reconcile_duration_seconds",
			Help:    "Duration of reconcile operations by kind",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~32s
		},
		[]string{"kind"},
	)

	queueDepth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_queue_depth",
			Help: "Current depth of work queue by kind",
		},
		[]string{"kind"},
	)

	// VM operation metrics
	vmOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_vm_operations_total",
			Help: "Total number of VM operations by operation, provider type, provider, and outcome",
		},
		[]string{"operation", "provider_type", "provider", "outcome"},
	)

	// Provider RPC metrics
	providerRPCRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_provider_rpc_requests_total",
			Help: "Total number of provider RPC requests by provider type, method, and code",
		},
		[]string{"provider_type", "method", "code"},
	)

	providerRPCLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "virtrigaud_provider_rpc_latency_seconds",
			Help:    "Latency of provider RPC requests by provider type and method",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~32s
		},
		[]string{"provider_type", "method"},
	)

	// Provider task metrics
	providerTasksInflight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_provider_tasks_inflight",
			Help: "Number of inflight tasks by provider type and provider",
		},
		[]string{"provider_type", "provider"},
	)

	// Error metrics
	errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_errors_total",
			Help: "Total number of errors by reason and component",
		},
		[]string{"reason", "component"},
	)

	// IP discovery metrics
	ipDiscoveryDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "virtrigaud_ip_discovery_duration_seconds",
			Help:    "Duration of IP discovery operations by provider type",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 100ms to ~100s
		},
		[]string{"provider_type"},
	)

	// Circuit breaker metrics
	circuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=half-open, 2=open)",
		},
		[]string{"provider_type", "provider"},
	)

	circuitBreakerFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "virtrigaud_circuit_breaker_failures_total",
			Help: "Total number of circuit breaker failures",
		},
		[]string{"provider_type", "provider"},
	)
)

// Outcomes for reconcile operations
const (
	OutcomeSuccess = "success"
	OutcomeError   = "error"
	OutcomeRequeue = "requeue"
)

// VM Operations
const (
	OpCreate      = "Create"
	OpDelete      = "Delete"
	OpPower       = "Power"
	OpDescribe    = "Describe"
	OpReconfigure = "Reconfigure"
)

// Components
const (
	ComponentManager  = "manager"
	ComponentProvider = "provider"
)

// Circuit breaker states
const (
	CircuitBreakerClosed   = 0
	CircuitBreakerHalfOpen = 1
	CircuitBreakerOpen     = 2
)

// SetupMetrics initializes metrics with build information
func SetupMetrics(version, gitSHA, component string) {
	buildInfo.WithLabelValues(version, gitSHA, runtime.Version(), component).Set(1)
}

// ReconcileMetrics provides metrics for reconcile operations
type ReconcileMetrics struct {
	kind string
}

// NewReconcileMetrics creates metrics for a specific resource kind
func NewReconcileMetrics(kind string) *ReconcileMetrics {
	return &ReconcileMetrics{kind: kind}
}

// RecordReconcile records a reconcile operation with its outcome and duration
func (m *ReconcileMetrics) RecordReconcile(outcome string, duration time.Duration) {
	managerReconcileTotal.WithLabelValues(m.kind, outcome).Inc()
	managerReconcileDuration.WithLabelValues(m.kind).Observe(duration.Seconds())
}

// SetQueueDepth sets the current queue depth
func (m *ReconcileMetrics) SetQueueDepth(depth float64) {
	queueDepth.WithLabelValues(m.kind).Set(depth)
}

// VMOperationMetrics provides metrics for VM operations
type VMOperationMetrics struct {
	providerType string
	provider     string
}

// NewVMOperationMetrics creates metrics for VM operations
func NewVMOperationMetrics(providerType, provider string) *VMOperationMetrics {
	return &VMOperationMetrics{
		providerType: providerType,
		provider:     provider,
	}
}

// RecordOperation records a VM operation with its outcome
func (m *VMOperationMetrics) RecordOperation(operation, outcome string) {
	vmOperationsTotal.WithLabelValues(operation, m.providerType, m.provider, outcome).Inc()
}

// ProviderRPCMetrics provides metrics for provider RPC calls
type ProviderRPCMetrics struct {
	providerType string
}

// NewProviderRPCMetrics creates metrics for provider RPC calls
func NewProviderRPCMetrics(providerType string) *ProviderRPCMetrics {
	return &ProviderRPCMetrics{providerType: providerType}
}

// RecordRPC records an RPC call with its method, status code, and duration
func (m *ProviderRPCMetrics) RecordRPC(method, code string, duration time.Duration) {
	providerRPCRequestsTotal.WithLabelValues(m.providerType, method, code).Inc()
	providerRPCLatency.WithLabelValues(m.providerType, method).Observe(duration.Seconds())
}

// TaskMetrics provides metrics for provider tasks
type TaskMetrics struct {
	providerType string
	provider     string
}

// NewTaskMetrics creates metrics for provider tasks
func NewTaskMetrics(providerType, provider string) *TaskMetrics {
	return &TaskMetrics{
		providerType: providerType,
		provider:     provider,
	}
}

// SetInflightTasks sets the number of inflight tasks
func (m *TaskMetrics) SetInflightTasks(count float64) {
	providerTasksInflight.WithLabelValues(m.providerType, m.provider).Set(count)
}

// RecordError records an error with its reason and component
func RecordError(reason, component string) {
	errorsTotal.WithLabelValues(reason, component).Inc()
}

// RecordIPDiscovery records IP discovery duration
func RecordIPDiscovery(providerType string, duration time.Duration) {
	ipDiscoveryDuration.WithLabelValues(providerType).Observe(duration.Seconds())
}

// CircuitBreakerMetrics provides metrics for circuit breakers
type CircuitBreakerMetrics struct {
	providerType string
	provider     string
}

// NewCircuitBreakerMetrics creates metrics for circuit breakers
func NewCircuitBreakerMetrics(providerType, provider string) *CircuitBreakerMetrics {
	return &CircuitBreakerMetrics{
		providerType: providerType,
		provider:     provider,
	}
}

// SetState sets the circuit breaker state
func (m *CircuitBreakerMetrics) SetState(state int) {
	circuitBreakerState.WithLabelValues(m.providerType, m.provider).Set(float64(state))
}

// RecordFailure records a circuit breaker failure
func (m *CircuitBreakerMetrics) RecordFailure() {
	circuitBreakerFailures.WithLabelValues(m.providerType, m.provider).Inc()
}

// Timer is a helper for measuring operation duration
type Timer struct {
	start time.Time
}

// NewTimer creates a new timer
func NewTimer() *Timer {
	return &Timer{start: time.Now()}
}

// Duration returns the elapsed time since the timer was created
func (t *Timer) Duration() time.Duration {
	return time.Since(t.start)
}

// ReconcileTimer is a helper for measuring reconcile operations
type ReconcileTimer struct {
	metrics *ReconcileMetrics
	timer   *Timer
}

// NewReconcileTimer creates a timer for reconcile operations
func NewReconcileTimer(kind string) *ReconcileTimer {
	return &ReconcileTimer{
		metrics: NewReconcileMetrics(kind),
		timer:   NewTimer(),
	}
}

// Finish records the reconcile operation with the given outcome
func (rt *ReconcileTimer) Finish(outcome string) {
	rt.metrics.RecordReconcile(outcome, rt.timer.Duration())
}

// RPCTimer is a helper for measuring RPC operations
type RPCTimer struct {
	metrics *ProviderRPCMetrics
	method  string
	timer   *Timer
}

// NewRPCTimer creates a timer for RPC operations
func NewRPCTimer(providerType, method string) *RPCTimer {
	return &RPCTimer{
		metrics: NewProviderRPCMetrics(providerType),
		method:  method,
		timer:   NewTimer(),
	}
}

// Finish records the RPC operation with the given status code
func (rt *RPCTimer) Finish(code string) {
	rt.metrics.RecordRPC(rt.method, code, rt.timer.Duration())
}

// Init registers all metrics with the controller-runtime metrics registry
func Init() {
	// Metrics are automatically registered via promauto
	// This function is for any additional setup if needed
}

// GetRegistry returns the Prometheus registry used by controller-runtime
func GetRegistry() prometheus.Gatherer {
	return metrics.Registry
}
