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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCircuitBreaker_InjectedClock drives the full Closed -> Open ->
// HalfOpen -> Closed cycle on Config.Now, without sleeping: ResetTimeout is
// measured on the injected clock, not the wall clock.
func TestCircuitBreaker_InjectedClock(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cb := NewCircuitBreaker("clock", "clock-type", "clock-provider", &Config{
		FailureThreshold: 2,
		ResetTimeout:     time.Minute,
		HalfOpenMaxCalls: 2,
		Now:              func() time.Time { return now },
	})
	ctx := context.Background()
	fail := func(context.Context) error { return errors.New("down") }
	ok := func(context.Context) error { return nil }

	require.Error(t, cb.Call(ctx, fail))
	require.Error(t, cb.Call(ctx, fail))
	require.Equal(t, StateOpen, cb.GetState())

	// Still open one tick before ResetTimeout on the injected clock, however
	// much wall time passes.
	now = now.Add(time.Minute)
	ran := false
	require.Error(t, cb.Call(ctx, func(context.Context) error { ran = true; return nil }))
	assert.False(t, ran, "the breaker must refuse until ResetTimeout elapsed on its clock")

	now = now.Add(time.Second)
	require.NoError(t, cb.Call(ctx, ok))
	assert.Equal(t, StateHalfOpen, cb.GetState())
	require.NoError(t, cb.Call(ctx, ok))
	assert.Equal(t, StateClosed, cb.GetState())
}
