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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestHalfOpenTransitionCountsAsCall is the regression canary for issue #96.
//
// Before the fix in allowCall, transitioning Open -> HalfOpen reset
// halfOpenCalls to 0 and returned true WITHOUT incrementing the counter.
// That meant if HalfOpenMaxCalls was N, the circuit needed N+1 successful
// half-open calls to close instead of N.
//
// After the fix: the transition itself is counted as the first half-open
// call (via fallthrough), so N successful calls close the circuit.
func TestHalfOpenTransitionCountsAsCall(t *testing.T) {
	cb := NewCircuitBreaker("test", "test", "test", &Config{
		FailureThreshold: 2,
		ResetTimeout:     20 * time.Millisecond,
		HalfOpenMaxCalls: 2,
	})

	ctx := context.Background()
	failFn := func(ctx context.Context) error {
		return contracts.NewRetryableError("forced failure", nil)
	}
	successFn := func(ctx context.Context) error {
		return nil
	}

	// Push 2 failures to open the circuit.
	require.Error(t, cb.Call(ctx, failFn))
	require.Error(t, cb.Call(ctx, failFn))
	assert.Equal(t, StateOpen, cb.GetState(), "circuit should be open after 2 failures with threshold 2")

	// Wait past the reset timeout so allowCall transitions to half-open.
	time.Sleep(40 * time.Millisecond)

	// First successful call: triggers Open -> HalfOpen transition AND
	// is counted as the first half-open call. State stays HalfOpen
	// because halfOpenCalls (1) < HalfOpenMaxCalls (2).
	require.NoError(t, cb.Call(ctx, successFn))
	assert.Equal(t, StateHalfOpen, cb.GetState(),
		"after 1 successful half-open call (1/2), state should remain HalfOpen")

	// Second successful call: halfOpenCalls (2) >= HalfOpenMaxCalls (2),
	// so recordSuccess transitions to Closed.
	require.NoError(t, cb.Call(ctx, successFn))
	assert.Equal(t, StateClosed, cb.GetState(),
		"after 2 successful half-open calls (2/2), circuit should close")
}

// TestOpenStateRejectsCallsBeforeResetTimeout pins the "fast fail when open"
// contract. When the circuit is open and ResetTimeout hasn't elapsed,
// allowCall must return false and the wrapped function must not execute.
func TestOpenStateRejectsCallsBeforeResetTimeout(t *testing.T) {
	cb := NewCircuitBreaker("test", "test", "test", &Config{
		FailureThreshold: 1,
		ResetTimeout:     30 * time.Second, // long enough to not elapse during test
		HalfOpenMaxCalls: 1,
	})

	ctx := context.Background()
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail", nil)
	}))
	assert.Equal(t, StateOpen, cb.GetState())

	// The wrapped fn must NOT execute while the circuit is open.
	executed := false
	err := cb.Call(ctx, func(ctx context.Context) error {
		executed = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, executed, "fn must not execute when circuit is open before reset timeout")
	// The error must be a provider-grade Unavailable so retry loops know
	// fast-fail semantics rather than treating it as a wrapped-fn failure.
	var pe *contracts.ProviderError
	if assert.ErrorAs(t, err, &pe) {
		assert.True(t, pe.IsRetryable(), "circuit-open error should be retryable so callers can back off + retry")
	}
}

// TestHalfOpenFailureReOpensCircuit verifies that a single failure during
// half-open immediately reopens the circuit, regardless of HalfOpenMaxCalls.
func TestHalfOpenFailureReOpensCircuit(t *testing.T) {
	cb := NewCircuitBreaker("test", "test", "test", &Config{
		FailureThreshold: 1,
		ResetTimeout:     20 * time.Millisecond,
		HalfOpenMaxCalls: 3,
	})

	ctx := context.Background()
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail", nil)
	}))
	assert.Equal(t, StateOpen, cb.GetState())

	// Wait for reset timeout to elapse.
	time.Sleep(40 * time.Millisecond)

	// First half-open call fails: must re-open immediately.
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail again", nil)
	}))
	assert.Equal(t, StateOpen, cb.GetState(),
		"a single failure in HalfOpen must re-open the circuit")
}

// TestHalfOpenRejectsAfterMaxCalls verifies that once HalfOpenMaxCalls
// calls have been admitted (transition + subsequent), further calls are
// rejected until the state transitions to Closed.
//
// This is a subtle behavior worth pinning: in the window between
// reaching HalfOpenMaxCalls and recordSuccess transitioning to Closed,
// no additional calls should be admitted. With the #96 fix in place,
// the transition itself counts as the first call, so HalfOpenMaxCalls=2
// means: transition + 1 more = 2 admitted.
func TestHalfOpenRejectsAfterMaxCalls(t *testing.T) {
	cb := NewCircuitBreaker("test", "test", "test", &Config{
		FailureThreshold: 1,
		ResetTimeout:     20 * time.Millisecond,
		// HalfOpenMaxCalls=1 means: only the transition itself is admitted,
		// the next half-open call (before the success triggers Closed) is rejected.
		HalfOpenMaxCalls: 1,
	})

	ctx := context.Background()
	require.Error(t, cb.Call(ctx, func(ctx context.Context) error {
		return contracts.NewRetryableError("fail", nil)
	}))
	assert.Equal(t, StateOpen, cb.GetState())

	time.Sleep(40 * time.Millisecond)

	// First half-open call: admitted (this is the transition itself).
	// With HalfOpenMaxCalls=1, this success closes the circuit on
	// recordSuccess (since halfOpenCalls (1) >= HalfOpenMaxCalls (1)).
	require.NoError(t, cb.Call(ctx, func(ctx context.Context) error {
		return nil
	}))
	assert.Equal(t, StateClosed, cb.GetState(),
		"with HalfOpenMaxCalls=1, one successful call (the transition) closes the circuit")
}
