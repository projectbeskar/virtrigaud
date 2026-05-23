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

package resilience

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dto "github.com/prometheus/client_model/go"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// gaugeSample returns the current value of the gauge sample matching
// the given metric family name and labels. Returns NaN-equivalent
// (a sentinel via the `found` bool) when no matching sample exists.
func gaugeSample(t *testing.T, family string, want map[string]string) (val float64, found bool) {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			if g5LabelsMatch(m.GetLabel(), want) {
				if g := m.GetGauge(); g != nil {
					return g.GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// counterSampleG5 — same shape as elsewhere; local copy to avoid coupling
// the resilience package's tests to controller-package test helpers.
func counterSampleG5(t *testing.T, family string, want map[string]string) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			if g5LabelsMatch(m.GetLabel(), want) {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
			}
		}
	}
	return 0
}

func g5LabelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		ok := false
		for _, lp := range got {
			if lp.GetName() == k && lp.GetValue() == v {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// TestCircuitBreakerMetrics_InitialStateIsClosed verifies that
// constructing a new CircuitBreaker emits the initial SetState(Closed)
// sample. Operators rely on this to know the breaker's starting state
// without waiting for the first call.
func TestCircuitBreakerMetrics_InitialStateIsClosed(t *testing.T) {
	const providerType, provider = "g5-test-init", "g5-init-provider"
	labels := map[string]string{
		"provider_type": providerType,
		"provider":      provider,
	}

	_ = NewCircuitBreaker("init", providerType, provider, &Config{
		FailureThreshold: 3,
		ResetTimeout:     time.Second,
		HalfOpenMaxCalls: 2,
	})

	val, found := gaugeSample(t, "virtrigaud_circuit_breaker_state", labels)
	require.True(t, found, "virtrigaud_circuit_breaker_state should emit a sample on construction")
	assert.Equal(t, float64(metrics.CircuitBreakerClosed), val,
		"initial state must be Closed (gauge value 0)")
}

// TestCircuitBreakerMetrics_FullLifecycle exercises the full
// closed -> open -> half-open -> closed lifecycle and asserts that
// the state gauge reflects each transition and the failures counter
// increments on every counted failure. This is the issue #91
// acceptance criterion in test form.
func TestCircuitBreakerMetrics_FullLifecycle(t *testing.T) {
	const providerType, provider = "g5-test-lifecycle", "g5-lifecycle-provider"
	stateLabels := map[string]string{
		"provider_type": providerType,
		"provider":      provider,
	}
	failureLabels := map[string]string{
		"provider_type": providerType,
		"provider":      provider,
	}

	cb := NewCircuitBreaker("lifecycle", providerType, provider, &Config{
		FailureThreshold: 2,
		ResetTimeout:     20 * time.Millisecond,
		HalfOpenMaxCalls: 2,
	})
	ctx := context.Background()

	failFn := func(ctx context.Context) error {
		return contracts.NewRetryableError("induced", nil)
	}
	successFn := func(ctx context.Context) error { return nil }

	// 1. Initial state: Closed (gauge=0)
	val, _ := gaugeSample(t, "virtrigaud_circuit_breaker_state", stateLabels)
	assert.Equal(t, float64(metrics.CircuitBreakerClosed), val, "initial state should be Closed")

	// 2. First failure: still Closed (1/2 threshold), failures counter += 1
	failuresBefore := counterSampleG5(t, "virtrigaud_circuit_breaker_failures_total", failureLabels)
	require.Error(t, cb.Call(ctx, failFn))
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", stateLabels)
	assert.Equal(t, float64(metrics.CircuitBreakerClosed), val,
		"state should remain Closed after 1 failure (1/2 threshold)")
	failuresAfter := counterSampleG5(t, "virtrigaud_circuit_breaker_failures_total", failureLabels)
	assert.Equal(t, failuresBefore+1, failuresAfter,
		"failures_total should increment by 1 per counted failure")

	// 3. Second failure: transition Closed -> Open (gauge=2)
	require.Error(t, cb.Call(ctx, failFn))
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", stateLabels)
	assert.Equal(t, float64(metrics.CircuitBreakerOpen), val,
		"state should transition to Open after threshold failures")
	failuresAfter2 := counterSampleG5(t, "virtrigaud_circuit_breaker_failures_total", failureLabels)
	assert.Equal(t, failuresBefore+2, failuresAfter2,
		"failures_total should now be +2 from baseline")

	// 4. Wait past ResetTimeout for half-open eligibility
	time.Sleep(40 * time.Millisecond)

	// 5. Successful call: allowCall transitions Open -> HalfOpen (and
	//    per the #96 fix in PR #100, counts this call as the first
	//    half-open call). State gauge briefly = 1 (HalfOpen). With
	//    HalfOpenMaxCalls=2, this one success doesn't close yet.
	require.NoError(t, cb.Call(ctx, successFn))
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", stateLabels)
	assert.Equal(t, float64(metrics.CircuitBreakerHalfOpen), val,
		"state should be HalfOpen after first successful call following reset timeout (1/2 half-open calls)")

	// 6. Second successful call: halfOpenCalls reaches max -> transition
	//    HalfOpen -> Closed (gauge=0)
	require.NoError(t, cb.Call(ctx, successFn))
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", stateLabels)
	assert.Equal(t, float64(metrics.CircuitBreakerClosed), val,
		"state should return to Closed after HalfOpenMaxCalls successes")
}

// TestCircuitBreakerMetrics_FailureInHalfOpenReopens verifies that
// observability captures the half-open -> open re-transition. When a
// half-open call fails, the circuit should re-open immediately and the
// gauge should jump from 1 -> 2 in a single sample read.
func TestCircuitBreakerMetrics_FailureInHalfOpenReopens(t *testing.T) {
	const providerType, provider = "g5-test-reopen", "g5-reopen-provider"
	labels := map[string]string{
		"provider_type": providerType,
		"provider":      provider,
	}

	cb := NewCircuitBreaker("reopen", providerType, provider, &Config{
		FailureThreshold: 1,
		ResetTimeout:     20 * time.Millisecond,
		HalfOpenMaxCalls: 3,
	})
	ctx := context.Background()

	// Fail once to open.
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail", nil)
	}))
	val, _ := gaugeSample(t, "virtrigaud_circuit_breaker_state", labels)
	assert.Equal(t, float64(metrics.CircuitBreakerOpen), val, "should be Open after 1 failure (threshold=1)")

	// Wait past reset timeout.
	time.Sleep(40 * time.Millisecond)

	// Failing call during HalfOpen reopens immediately.
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail again", nil)
	}))
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", labels)
	assert.Equal(t, float64(metrics.CircuitBreakerOpen), val,
		"a failure during HalfOpen must re-set the gauge to Open, not leave it at HalfOpen")
}

// TestCircuitBreakerMetrics_ResetEmitsClosedGauge verifies the explicit
// Reset() method also emits a SetState(Closed) sample. Reset is used by
// the Registry.Reset() pathway during tests and operational recovery.
func TestCircuitBreakerMetrics_ResetEmitsClosedGauge(t *testing.T) {
	const providerType, provider = "g5-test-reset", "g5-reset-provider"
	labels := map[string]string{
		"provider_type": providerType,
		"provider":      provider,
	}

	cb := NewCircuitBreaker("reset-test", providerType, provider, &Config{
		FailureThreshold: 1,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})
	ctx := context.Background()

	// Open the breaker.
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail", nil)
	}))
	val, _ := gaugeSample(t, "virtrigaud_circuit_breaker_state", labels)
	require.Equal(t, float64(metrics.CircuitBreakerOpen), val)

	// Reset explicitly.
	cb.Reset()
	val, _ = gaugeSample(t, "virtrigaud_circuit_breaker_state", labels)
	assert.Equal(t, float64(metrics.CircuitBreakerClosed), val,
		"explicit Reset() must emit SetState(Closed)")
}
