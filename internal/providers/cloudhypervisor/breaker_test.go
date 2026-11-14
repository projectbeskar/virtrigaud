//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// TestCountsTowardHostBreaker: only a host the Provider cannot reach counts;
// every answer of the agent with a reason of its contract, and every other
// answered code but a bare Unavailable, is exempt.
func TestCountsTowardHostBreaker(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "x")
	assert.False(t, countsTowardHostBreaker(nil, false, false))
	assert.True(t, countsTowardHostBreaker(unavailable, false, false), "no answer")
	assert.True(t, countsTowardHostBreaker(status.Error(codes.DeadlineExceeded, "x"), false, false), "no answer")
	assert.False(t, countsTowardHostBreaker(unavailable, false, true), "the caller gave up")
	assert.True(t, countsTowardHostBreaker(unavailable, true, false), "a bare answered Unavailable")
	for _, reason := range contracts.PartAReasons() {
		for _, code := range []codes.Code{codes.Unavailable, codes.FailedPrecondition, codes.NotFound, codes.Aborted} {
			assert.False(t, countsTowardHostBreaker(refusalErr(code, reason), true, false), "%s %s", code, reason)
		}
	}
	for _, code := range []codes.Code{codes.Internal, codes.Unknown, codes.NotFound, codes.InvalidArgument, codes.Unimplemented} {
		assert.False(t, countsTowardHostBreaker(status.Error(code, "x"), true, false), "answered %s", code)
	}
}

// TestStaleHelloNeverClosesTheBreaker: inside a half-open breaker a call
// found to hold a stale Hello sent nothing, so it cannot close the breaker.
func TestStaleHelloNeverClosesTheBreaker(t *testing.T) {
	now := time.Now()
	b := newHostBreaker("host-a", &resilience.Config{FailureThreshold: 1, ResetTimeout: time.Second, HalfOpenMaxCalls: 1,
		Now: func() time.Time { return now }})
	unreachable := func(context.Context) (bool, error) { return false, status.Error(codes.Unavailable, "down") }
	_ = guard(context.Background(), b, unreachable, func() bool { return false })
	require.Equal(t, resilience.StateOpen, b.GetState())
	now = now.Add(2 * time.Second)
	err := guard(context.Background(), b, func(context.Context) (bool, error) { return false, errHelloStale }, func() bool { return false })
	require.ErrorIs(t, err, errHelloStale)
	assert.NotEqual(t, resilience.StateClosed, b.GetState())
}
