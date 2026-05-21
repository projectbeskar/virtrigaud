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

package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

func newTestBreaker(failureThreshold, halfOpenMaxCalls int, resetTimeout time.Duration) *CircuitBreaker {
	return NewCircuitBreaker("t", "test", "test", &Config{
		FailureThreshold: failureThreshold,
		ResetTimeout:     resetTimeout,
		HalfOpenMaxCalls: halfOpenMaxCalls,
	})
}

func failingFn(context.Context) error { return errors.New("boom") }
func successFn(context.Context) error { return nil }

// TestOpensAfterFailureThreshold confirms the basic happy-path transition
// Closed -> Open after FailureThreshold consecutive failures.
func TestOpensAfterFailureThreshold(t *testing.T) {
	cb := newTestBreaker(2, 2, time.Hour)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_ = cb.Call(ctx, failingFn)
	}

	if got := cb.GetState(); got != StateOpen {
		t.Fatalf("after %d failures: state = %s, want %s", 2, got, StateOpen)
	}
}

// TestOpenCallReturnsCircuitOpenError is the regression test for the contract
// of the error returned when the circuit is open:
//   - Type must be contracts.ErrorTypeCircuitOpen (not Unavailable)
//   - Retryable must be false
//   - IsRetryable() must return false
//
// Previously the breaker returned NewUnavailableError, which marks the error
// Retryable=true and is also classified retryable by ProviderError.IsRetryable
// via the Type==Unavailable branch — i.e. callers would retry on an open
// circuit, defeating the purpose of the breaker.
func TestOpenCallReturnsCircuitOpenError(t *testing.T) {
	cb := newTestBreaker(1, 2, time.Hour)
	ctx := context.Background()

	_ = cb.Call(ctx, failingFn)
	if got := cb.GetState(); got != StateOpen {
		t.Fatalf("setup: state = %s, want %s", got, StateOpen)
	}

	err := cb.Call(ctx, successFn)
	if err == nil {
		t.Fatal("Call on open breaker returned nil error")
	}

	var pe *contracts.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *contracts.ProviderError", err, err)
	}
	if pe.Type != contracts.ErrorTypeCircuitOpen {
		t.Errorf("error Type = %q, want %q", pe.Type, contracts.ErrorTypeCircuitOpen)
	}
	if pe.Retryable {
		t.Errorf("error Retryable = true, want false (open circuit must not be retried)")
	}
	if pe.IsRetryable() {
		t.Errorf("IsRetryable() = true, want false (open circuit must not be retried)")
	}
}

// TestHalfOpenFirstProbeCountsAgainstBudget is the regression test for the
// bug where the first probe after the Open -> HalfOpen transition bypassed
// the HalfOpenMaxCalls accounting. With HalfOpenMaxCalls=2 and 2 successful
// probes, the fix transitions back to Closed; the buggy version required a
// third probe before closing.
func TestHalfOpenFirstProbeCountsAgainstBudget(t *testing.T) {
	const resetTimeout = 5 * time.Millisecond
	cb := newTestBreaker(2, 2, resetTimeout)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_ = cb.Call(ctx, failingFn)
	}
	if got := cb.GetState(); got != StateOpen {
		t.Fatalf("setup: state = %s, want %s", got, StateOpen)
	}

	time.Sleep(resetTimeout + 5*time.Millisecond)

	if err := cb.Call(ctx, successFn); err != nil {
		t.Fatalf("probe 1: unexpected error %v", err)
	}
	if got := cb.GetState(); got != StateHalfOpen {
		t.Errorf("after probe 1: state = %s, want %s", got, StateHalfOpen)
	}

	if err := cb.Call(ctx, successFn); err != nil {
		t.Fatalf("probe 2: unexpected error %v", err)
	}

	if got := cb.GetState(); got != StateClosed {
		t.Errorf("after probe 2: state = %s, want %s (fix should have closed the "+
			"breaker; the buggy version stays HalfOpen because the first probe "+
			"did not count against HalfOpenMaxCalls)", got, StateClosed)
	}
}

// TestHalfOpenFailureReopens ensures any failure observed during half-open
// probing puts the breaker back into Open state immediately.
func TestHalfOpenFailureReopens(t *testing.T) {
	const resetTimeout = 5 * time.Millisecond
	cb := newTestBreaker(1, 3, resetTimeout)
	ctx := context.Background()

	_ = cb.Call(ctx, failingFn)
	time.Sleep(resetTimeout + 5*time.Millisecond)

	if err := cb.Call(ctx, failingFn); err == nil {
		t.Fatal("probe expected to surface inner error, got nil")
	}

	if got := cb.GetState(); got != StateOpen {
		t.Errorf("after failing probe: state = %s, want %s", got, StateOpen)
	}
}

// TestReset returns the breaker to Closed regardless of the prior state.
func TestReset(t *testing.T) {
	cb := newTestBreaker(1, 2, time.Hour)
	_ = cb.Call(context.Background(), failingFn)

	if got := cb.GetState(); got != StateOpen {
		t.Fatalf("setup: state = %s, want %s", got, StateOpen)
	}

	cb.Reset()
	if got := cb.GetState(); got != StateClosed {
		t.Errorf("after Reset: state = %s, want %s", got, StateClosed)
	}
	if f := cb.GetFailures(); f != 0 {
		t.Errorf("after Reset: failures = %d, want 0", f)
	}
}
