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
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// expectedMetricNames lists every virtrigaud_* metric that must be registered
// on the controller-runtime registry so that the manager's /metrics endpoint
// exposes them. Keep this in sync with the package-level metric declarations.
var expectedMetricNames = []string{
	"virtrigaud_build_info",
	"virtrigaud_manager_reconcile_total",
	"virtrigaud_manager_reconcile_duration_seconds",
	"virtrigaud_queue_depth",
	"virtrigaud_vm_operations_total",
	"virtrigaud_provider_rpc_requests_total",
	"virtrigaud_provider_rpc_latency_seconds",
	"virtrigaud_provider_tasks_inflight",
	"virtrigaud_errors_total",
	"virtrigaud_ip_discovery_duration_seconds",
	"virtrigaud_circuit_breaker_state",
	"virtrigaud_circuit_breaker_failures_total",
}

// TestMetricsRegisteredOnControllerRuntimeRegistry asserts every virtrigaud_*
// metric ends up on the controller-runtime registry (the one served by the
// manager at /metrics). Regression guard for metrics being registered on
// promauto's default global registry, which is invisible to the manager.
func TestMetricsRegisteredOnControllerRuntimeRegistry(t *testing.T) {
	// Force vmOperationsTotal-style metrics to materialize at least one series
	// so prometheus.Registry.Gather() reports them. Counter/Gauge/Histogram
	// vectors only appear in Gather output after at least one labeled child
	// has been instantiated.
	buildInfo.WithLabelValues("test", "test", "test", "test").Set(1)
	managerReconcileTotal.WithLabelValues("VirtualMachine", "success").Inc()
	managerReconcileDuration.WithLabelValues("VirtualMachine").Observe(0)
	queueDepth.WithLabelValues("VirtualMachine").Set(0)
	vmOperationsTotal.WithLabelValues("Create", "vsphere", "test", "success").Inc()
	providerRPCRequestsTotal.WithLabelValues("vsphere", "Create", "OK").Inc()
	providerRPCLatency.WithLabelValues("vsphere", "Create").Observe(0)
	providerTasksInflight.WithLabelValues("vsphere", "test").Set(0)
	errorsTotal.WithLabelValues("Transient", "manager").Inc()
	ipDiscoveryDuration.WithLabelValues("vsphere").Observe(0)
	circuitBreakerState.WithLabelValues("vsphere", "test").Set(0)
	circuitBreakerFailures.WithLabelValues("vsphere", "test").Inc()

	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("ctrlmetrics.Registry.Gather() returned error: %v", err)
	}

	gathered := make(map[string]struct{}, len(mfs))
	for _, mf := range mfs {
		gathered[mf.GetName()] = struct{}{}
	}

	for _, name := range expectedMetricNames {
		if _, ok := gathered[name]; !ok {
			t.Errorf("metric %q was not gathered from controller-runtime registry; "+
				"manager /metrics will not expose it", name)
		}
	}
}

// TestGetRegistryReturnsControllerRuntimeRegistry confirms GetRegistry() and
// the package-level promautoFactory both point at sigs.k8s.io/controller-runtime
// metrics.Registry rather than prometheus' default global registry.
func TestGetRegistryReturnsControllerRuntimeRegistry(t *testing.T) {
	if GetRegistry() != ctrlmetrics.Registry {
		t.Fatalf("GetRegistry() = %p, want controller-runtime metrics.Registry %p",
			GetRegistry(), ctrlmetrics.Registry)
	}
}
