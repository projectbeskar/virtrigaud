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

package clustered

import (
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// requireCode fails the case unless err is a gRPC error with code want, and
// returns its decoded form.
func requireCode(e *Env, err error, want codes.Code, what string) Wire {
	e.T.Helper()
	require.Error(e.T, err, "%s: must be refused", what)
	w := WireOf(err)
	require.Equal(e.T, want, w.Code, "%s: %s", what, w)
	return w
}

// requireHostUnavailable fails the case unless err is the retryable,
// host-scoped answer of a clustered provider: codes.Unavailable carrying a
// HOST_UNAVAILABLE ErrorInfo of the provider domain. When host is not empty
// the ErrorInfo must name it (contracts.HostUnavailableHostKey).
func requireHostUnavailable(e *Env, err error, host, what string) Wire {
	e.T.Helper()
	w := requireCode(e, err, codes.Unavailable, what)
	r, ok := w.Reason(contracts.HostUnavailableReason)
	require.True(e.T, ok, "%s: Unavailable without a %s ErrorInfo is a Provider-level failure, not a host-scoped one: %s",
		what, contracts.HostUnavailableReason, w)
	assert.Equal(e.T, contracts.HostUnavailableErrorDomain, r.Domain, "%s: ErrorInfo domain", what)
	if host != "" {
		assert.Equal(e.T, host, r.Metadata[contracts.HostUnavailableHostKey],
			"%s: the ErrorInfo names the host the call was routed to", what)
	}
	return w
}

// requireNoReason fails the case if w carries the ErrorInfo reason.
func requireNoReason(e *Env, w Wire, reason, what string) {
	e.T.Helper()
	_, ok := w.Reason(reason)
	assert.False(e.T, ok, "%s: must not carry %s: %s", what, reason, w)
}

// requireUnchanged fails the case unless vm is, on its host, exactly as before.
func requireUnchanged(e *Env, vm VM, before Observation, what string) {
	e.T.Helper()
	assert.Equal(e.T, before, e.Observe(vm), "%s: the VM on %s must not have been touched", what, vm.Host)
}

// containsAny reports which of needles s contains.
func containsAny(s string, needles ...string) []string {
	var hit []string
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			hit = append(hit, n)
		}
	}
	return hit
}
