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

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/obs/health"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

func TestObservabilityIntegration(t *testing.T) {
	// Setup logging
	logConfig := logging.DefaultConfig()
	logConfig.Level = "debug"
	logConfig.Format = "json"
	err := logging.Setup(logConfig)
	require.NoError(t, err)

	// Setup metrics
	metrics.SetupMetrics("test", "abc123", "test-component")

	// Test structured logging with correlation
	ctx := context.Background()
	ctx = logging.WithVM(ctx, "test-ns", "test-vm")
	ctx = logging.WithProvider(ctx, "test-ns", "test-provider")
	ctx = logging.WithProviderType(ctx, "vsphere")
	ctx = logging.WithTaskRef(ctx, "task-12345")

	logger := logging.FromContext(ctx)
	logger.Info("Test log entry with correlation")

	// Test secret redaction
	sensitiveData := "password=secret123 and api_key=abcdef"
	redacted := logging.RedactString(sensitiveData)
	assert.Contains(t, redacted, "[REDACTED]")
	assert.NotContains(t, redacted, "secret123")

	t.Logf("Original: %s", sensitiveData)
	t.Logf("Redacted: %s", redacted)
}

func TestMetricsIntegration(t *testing.T) {
	// Test reconcile metrics
	reconcileMetrics := metrics.NewReconcileMetrics("VirtualMachine")

	// Simulate a successful reconcile
	timer := metrics.NewReconcileTimer("VirtualMachine")
	time.Sleep(10 * time.Millisecond) // Simulate work
	timer.Finish(metrics.OutcomeSuccess)

	reconcileMetrics.SetQueueDepth(5)

	// Test VM operation metrics
	vmMetrics := metrics.NewVMOperationMetrics("vsphere", "prod")
	vmMetrics.RecordOperation(metrics.OpCreate, metrics.OutcomeSuccess)
	vmMetrics.RecordOperation(metrics.OpCreate, metrics.OutcomeError)

	// Test RPC metrics
	rpcTimer := metrics.NewRPCTimer("vsphere", "Create")
	time.Sleep(5 * time.Millisecond) // Simulate RPC
	rpcTimer.Finish("OK")

	// Test task metrics
	taskMetrics := metrics.NewTaskMetrics("vsphere", "prod")
	taskMetrics.SetInflightTasks(10)

	// Test error recording
	metrics.RecordError("ValidationError", metrics.ComponentManager)

	// Test IP discovery
	metrics.RecordIPDiscovery("vsphere", 2*time.Second)

	// Verify metrics are recorded (check registry)
	metricFamilies, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)

	metricNames := make(map[string]bool)
	for _, family := range metricFamilies {
		metricNames[*family.Name] = true
	}

	// Verify key metrics exist
	expectedMetrics := []string{
		"virtrigaud_build_info",
		"virtrigaud_manager_reconcile_total",
		"virtrigaud_vm_operations_total",
		"virtrigaud_provider_rpc_requests_total",
		"virtrigaud_errors_total",
	}

	for _, metric := range expectedMetrics {
		assert.True(t, metricNames[metric], "Missing metric: %s", metric)
	}

	t.Logf("Found %d metric families", len(metricFamilies))
}

func TestHealthSystem(t *testing.T) {
	checker := health.NewHealthChecker()

	// Register test health checks
	checker.RegisterCheck("test-check", func(ctx context.Context) error {
		return nil // Always healthy
	})

	checker.RegisterCheck("failing-check", func(ctx context.Context) error {
		return assert.AnError // Always fails
	})

	// Test individual check
	ctx := context.Background()
	result := checker.RunCheck(ctx, "test-check")
	assert.Equal(t, health.StatusHealthy, result.Status)

	result = checker.RunCheck(ctx, "failing-check")
	assert.Equal(t, health.StatusUnhealthy, result.Status)

	// Test overall status
	status := checker.GetOverallStatus(ctx)
	assert.Equal(t, health.StatusUnhealthy, status.Status) // One failing check
	assert.Equal(t, 1, status.Summary[health.StatusHealthy])
	assert.Equal(t, 1, status.Summary[health.StatusUnhealthy])

	// Test removing failing check
	checker.UnregisterCheck("failing-check")
	status = checker.GetOverallStatus(ctx)
	assert.Equal(t, health.StatusHealthy, status.Status)
}

func TestCircuitBreakerIntegration(t *testing.T) {
	config := &resilience.Config{
		FailureThreshold: 3,
		ResetTimeout:     100 * time.Millisecond,
		HalfOpenMaxCalls: 2,
	}

	cb := resilience.NewCircuitBreaker("test", "test-provider", "test", config)

	// Test successful calls
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		err := cb.Call(ctx, func(ctx context.Context) error {
			return nil // Success
		})
		assert.NoError(t, err)
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState())

	// Test failures to open circuit
	for i := 0; i < 3; i++ {
		err := cb.Call(ctx, func(ctx context.Context) error {
			return contracts.NewRetryableError("test failure", nil)
		})
		assert.Error(t, err)
	}
	assert.Equal(t, resilience.StateOpen, cb.GetState())

	// Test fast-fail while open
	err := cb.Call(ctx, func(ctx context.Context) error {
		t.Error("Should not execute when circuit is open")
		return nil
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker")

	// Wait for reset timeout
	time.Sleep(150 * time.Millisecond)

	// Test half-open state
	err = cb.Call(ctx, func(ctx context.Context) error {
		return nil // Success
	})
	assert.NoError(t, err)
	assert.Equal(t, resilience.StateHalfOpen, cb.GetState())

	// Another success should close the circuit
	err = cb.Call(ctx, func(ctx context.Context) error {
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, resilience.StateClosed, cb.GetState())
}

func TestRetryIntegration(t *testing.T) {
	config := &resilience.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Multiplier:  2.0,
		Jitter:      false,
	}

	// Test successful retry
	attempt := 0
	ctx := context.Background()
	err := resilience.Retry(ctx, config, func(ctx context.Context, attemptNum int) error {
		attempt = attemptNum
		if attemptNum < 2 {
			return contracts.NewRetryableError("transient error", nil)
		}
		return nil // Success on third attempt
	})

	assert.NoError(t, err)
	assert.Equal(t, 2, attempt) // Should succeed on attempt 2 (0-indexed)

	// Test non-retryable error
	attempt = 0
	err = resilience.Retry(ctx, config, func(ctx context.Context, attemptNum int) error {
		attempt = attemptNum
		return contracts.NewNotFoundError("not found", nil) // Non-retryable
	})

	assert.Error(t, err)
	assert.Equal(t, 0, attempt) // Should not retry

	// Test max attempts exceeded
	attempt = 0
	err = resilience.Retry(ctx, config, func(ctx context.Context, attemptNum int) error {
		attempt = attemptNum
		return contracts.NewRetryableError("persistent error", nil)
	})

	assert.Error(t, err)
	assert.Equal(t, 2, attempt) // Should try 3 times (0, 1, 2)
}

func TestCombinedResiliencePolicy(t *testing.T) {
	// Create circuit breaker
	cbConfig := &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     50 * time.Millisecond,
		HalfOpenMaxCalls: 1,
	}
	cb := resilience.NewCircuitBreaker("test", "test-provider", "test", cbConfig)

	// Create retry config
	retryConfig := &resilience.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    50 * time.Millisecond,
		Multiplier:  2.0,
		Jitter:      false,
	}

	// Create combined policy
	policy := resilience.NewPolicy("test-policy", retryConfig, cb)

	// Test successful operation
	ctx := context.Background()
	callCount := 0
	err := policy.Execute(ctx, func(ctx context.Context) error {
		callCount++
		if callCount == 1 {
			return contracts.NewRetryableError("first failure", nil)
		}
		return nil // Success on second call
	})

	assert.NoError(t, err)
	assert.Equal(t, 2, callCount)
	assert.Equal(t, resilience.StateClosed, cb.GetState())

	// Test circuit breaker protection
	callCount = 0
	for i := 0; i < 3; i++ {
		err = policy.Execute(ctx, func(ctx context.Context) error {
			callCount++
			return contracts.NewRetryableError("persistent failure", nil)
		})
		assert.Error(t, err)
	}

	// Circuit should be open now
	assert.Equal(t, resilience.StateOpen, cb.GetState())

	// Next call should be fast-failed by circuit breaker
	oldCallCount := callCount
	err = policy.Execute(ctx, func(ctx context.Context) error {
		callCount++
		t.Error("Should not execute when circuit is open")
		return nil
	})
	assert.Error(t, err)
	assert.Equal(t, oldCallCount, callCount) // No additional calls
}

// TestObservabilityEndToEnd demonstrates the complete observability flow
func TestObservabilityEndToEnd(t *testing.T) {
	t.Log("=== Starting End-to-End Observability Test ===")

	// Initialize all observability components
	logConfig := logging.DefaultConfig()
	require.NoError(t, logging.Setup(logConfig))

	metrics.SetupMetrics("v0.1.0", "test-sha", "integration-test")

	healthChecker := health.NewHealthChecker()
	healthChecker.RegisterCheck("integration-test", func(ctx context.Context) error {
		return nil
	})

	// Create resilience components
	cbRegistry := resilience.NewRegistry(nil)
	cb := cbRegistry.GetOrCreate("test-operation", "vsphere", "prod")

	// Simulate a complete operation flow
	ctx := context.Background()
	ctx = logging.WithVM(ctx, "test-namespace", "web-server-1")
	ctx = logging.WithProvider(ctx, "test-namespace", "vsphere-prod")
	ctx = logging.WithProviderType(ctx, "vsphere")
	ctx = logging.WithCorrelationID(ctx, "req-12345")

	logger := logging.FromContext(ctx)
	logger.Info("Starting VM operation")

	// Record metrics
	reconcileTimer := metrics.NewReconcileTimer("VirtualMachine")
	vmMetrics := metrics.NewVMOperationMetrics("vsphere", "prod")

	// Simulate operation with resilience
	policy := resilience.NewPolicyBuilder("vm-create").
		WithRetry(resilience.DefaultRetryConfig()).
		WithCircuitBreaker(cb).
		Build()

	operationCount := 0
	err := policy.Execute(ctx, func(ctx context.Context) error {
		operationCount++
		logger.Info("Executing provider operation", "attempt", operationCount)

		// Simulate RPC call
		rpcTimer := metrics.NewRPCTimer("vsphere", "Create")
		time.Sleep(5 * time.Millisecond)
		rpcTimer.Finish("OK")

		if operationCount == 1 {
			// Fail first attempt
			return contracts.NewRetryableError("simulated failure", nil)
		}

		return nil // Success on second attempt
	})

	// Record results
	if err != nil {
		reconcileTimer.Finish(metrics.OutcomeError)
		vmMetrics.RecordOperation(metrics.OpCreate, metrics.OutcomeError)
		metrics.RecordError("ProviderError", metrics.ComponentManager)
		logger.Error(err, "VM operation failed")
	} else {
		reconcileTimer.Finish(metrics.OutcomeSuccess)
		vmMetrics.RecordOperation(metrics.OpCreate, metrics.OutcomeSuccess)
		metrics.RecordIPDiscovery("vsphere", 1*time.Second)
		logger.Info("VM operation completed successfully")
	}

	// Verify operation succeeded
	assert.NoError(t, err)
	assert.Equal(t, 2, operationCount) // Should retry once

	// Verify health check
	healthStatus := healthChecker.GetOverallStatus(ctx)
	assert.Equal(t, health.StatusHealthy, healthStatus.Status)

	// Verify circuit breaker state
	assert.Equal(t, resilience.StateClosed, cb.GetState())

	logger.Info("End-to-end observability test completed successfully")
	t.Log("=== End-to-End Observability Test Completed ===")
}
