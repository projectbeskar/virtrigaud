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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatheredNames returns the set of metric family names currently registered
// in the registry returned by GetRegistry().
func gatheredNames(t *testing.T) map[string]bool {
	t.Helper()
	families, err := GetRegistry().Gather()
	require.NoError(t, err, "GetRegistry().Gather() should not error")
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	return names
}

// TestMetricsRegisteredInControllerRuntimeRegistry verifies that every
// virtrigaud_* metric is registered in controller-runtime's Registry (which is
// the one served at /metrics) rather than promauto's default global registry.
// This is the canary test for the regression where metrics were created but
// never exposed.
func TestMetricsRegisteredInControllerRuntimeRegistry(t *testing.T) {
	// Touch buildInfo so the metric family appears in the registry's gather output.
	// Metric vectors only emit a family entry once they have at least one observation.
	SetupMetrics("test-version", "test-sha", "test-component")

	// Exercise each metric vector with one observation so all 12 families appear.
	NewReconcileMetrics("VirtualMachine").RecordReconcile(OutcomeSuccess, 5*time.Millisecond)
	NewReconcileMetrics("VirtualMachine").SetQueueDepth(1)
	NewVMOperationMetrics("test", "p1").RecordOperation(OpCreate, OutcomeSuccess)
	NewProviderRPCMetrics("test").RecordRPC("Validate", "OK", 2*time.Millisecond)
	NewTaskMetrics("test", "p1").SetInflightTasks(0)
	RecordError("UnitTest", ComponentManager)
	RecordIPDiscovery("test", 100*time.Millisecond)
	cb := NewCircuitBreakerMetrics("test", "p1")
	cb.SetState(CircuitBreakerClosed)
	cb.RecordFailure()

	names := gatheredNames(t)

	expected := []string{
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

	for _, name := range expected {
		assert.True(t, names[name], "expected metric %q to be registered in controller-runtime's Registry; this is the v0.3.x metrics-not-exposed regression", name)
	}
}

// TestSetupMetricsEmitsBuildInfo verifies SetupMetrics writes a build_info
// sample with the expected labels. This is the canary for release smoke tests.
func TestSetupMetricsEmitsBuildInfo(t *testing.T) {
	SetupMetrics("v0.3.3-rc2", "abc1234", "manager")

	families, err := GetRegistry().Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() != "virtrigaud_build_info" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["version"] == "v0.3.3-rc2" &&
				labels["git_sha"] == "abc1234" &&
				labels["component"] == "manager" {
				found = true
				assert.Equal(t, float64(1), m.GetGauge().GetValue(), "build_info gauge should be set to 1")
			}
		}
	}
	assert.True(t, found, "virtrigaud_build_info should have a sample with the labels passed to SetupMetrics")
}
