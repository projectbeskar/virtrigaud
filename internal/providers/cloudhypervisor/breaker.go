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
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// Defaults of a host's circuit breaker.
const (
	// DefaultBreakerThreshold is how many counted failures in a row open a
	// host's breaker.
	DefaultBreakerThreshold = 5
	// DefaultBreakerReset is how long an open breaker refuses calls before
	// it lets one through again.
	DefaultBreakerReset = 30 * time.Second
	// defaultBreakerHalfOpenCalls is how many calls a half-open breaker
	// lets through.
	defaultBreakerHalfOpenCalls = 1
)

// errBreakerOpen is returned by guard when the host's breaker refused the
// call: nothing was sent.
var errBreakerOpen = errors.New("the host's circuit breaker is open")

// newHostBreaker returns the breaker of one host. Its metrics are labelled
// with the Provider type and the host id.
func newHostBreaker(host string, cfg *resilience.Config) *resilience.CircuitBreaker {
	c := resilience.Config{FailureThreshold: DefaultBreakerThreshold, ResetTimeout: DefaultBreakerReset,
		HalfOpenMaxCalls: defaultBreakerHalfOpenCalls}
	if cfg != nil {
		c = *cfg
	}
	return resilience.NewCircuitBreaker("host/"+host, ProviderType, host, &c)
}

// countsTowardHostBreaker reports whether err, the result of one call to a
// host's agent, says that the agent cannot be reached, and so counts toward
// the host's breaker. answered says whether the agent's gRPC server answered
// the call (callState.answered); callerDone says whether the caller had given
// up (its context ended) when the call failed.
//
//   - No answer (refused or reset connection, a TLS handshake the agent's
//     certificate failed, the transport closing mid-call, our own deadline):
//     counts, unless the caller gave up.
//   - An answer: counts only when the agent answered Unavailable without a
//     reason of the agent contract (contracts.PartAReasons: UNATTESTED,
//     VM_BUSY, UNKNOWN_TO_AGENT, the grant refusals, ...). Every other
//     answer, a definite not-performed included, is about one VM or one
//     operation, and the agent demonstrably works.
func countsTowardHostBreaker(err error, answered, callerDone bool) bool {
	if err == nil {
		return false
	}
	if !answered {
		return !callerDone
	}
	return status.Code(err) == codes.Unavailable && !contracts.HasPartAReason(err)
}

// guard runs call through b. call returns whether the agent answered and
// the call's error; callerDone reports whether the caller had given up. guard
// returns errBreakerOpen when b refused the call, and the call's own error
// otherwise; only a counted failure is recorded as one. The expiry of the
// Provider's own deadline in ctx counts; the caller's does not.
func guard(ctx context.Context, b *resilience.CircuitBreaker, call func(context.Context) (answered bool, err error), callerDone func() bool) error {
	ran := false
	var callErr error
	_ = b.Call(ctx, func(ctx context.Context) error {
		ran = true
		var answered bool
		answered, callErr = call(ctx)
		if errors.Is(callErr, errHelloStale) {
			// Inside the breaker only when it is half-open: never let a call
			// that sent nothing close it.
			return callErr
		}
		if countsTowardHostBreaker(callErr, answered, callerDone()) {
			return callErr
		}
		return nil
	})
	if !ran {
		return errBreakerOpen
	}
	return callErr
}
