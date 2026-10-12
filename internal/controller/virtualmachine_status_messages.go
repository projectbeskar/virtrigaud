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

package controller

import (
	"context"
	stderrors "errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A VirtualMachine's status and events are read by its namespace's users,
// who need not be the administrators of its Provider: a clustered Provider in
// another namespace serves consumers (ADR-0007 A6, threat 5). What the
// manager could not do to reach that Provider — the resolver's errors name the
// TLS Secret's namespace and name, the provider Service's ClusterIP and port
// and the runtime phase; a gRPC transport error names the dial target — goes
// to the manager log only. Status and events say it with the constants below.

// providerResolveFailedMessage is the Ready message of a VirtualMachine whose
// Provider exists but has no usable client (getProviderInstance failed: its
// runtime is not Running or has no endpoint, or the TLS configuration, the
// gRPC client or its Validate call failed). It is constant, so a repeated
// failure leaves the status unchanged; the cause is in the manager log.
var providerResolveFailedMessage = fmt.Sprintf("The manager cannot reach this VirtualMachine's Provider (its provider "+
	"runtime is not running or not ready, or a connection to it cannot be set up); retried every %s. The manager "+
	"log has the cause", providerErrorRetryInterval)

// providerNotReachedDetail replaces, in a VirtualMachine condition, the text
// of a provider call that failed in transport (isProviderTransportFailure).
const providerNotReachedDetail = "the provider could not be reached or did not answer in time (the manager log has the cause)"

// providerFailureDetail renders a failed provider call's err for a
// VirtualMachine condition: providerNotReachedDetail when the provider was
// not reached or did not answer (isProviderTransportFailure), otherwise the
// provider's own answer made safe for status (sanitizeProviderDetail: its
// categorized message, URL userinfo removed, one line, bounded).
func providerFailureDetail(err error) string {
	if isProviderTransportFailure(err) {
		return providerNotReachedDetail
	}
	return sanitizeProviderDetail(err)
}

// isProviderTransportFailure reports whether err says that a provider call did
// not get the provider's answer: a gRPC Unavailable, DeadlineExceeded or
// Canceled (a refused or broken connection, an open circuit breaker, a
// deadline), a context deadline or cancellation, or a provider error
// categorized Unavailable or Timeout. Their text is the transport's (it names
// the dial target) or the backend's, never something the request asked for.
//
// An answer the provider marked is not one: a clustered provider's
// HOST_UNAVAILABLE or IN_PROGRESS, VM_DISK_CHECK_FAILED and
// VM_SOURCE_RUNNING travel as Unavailable but are the provider's decision,
// which the controller reports as such.
func isProviderTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if contracts.IsHostUnavailable(err) || contracts.IsInProgress(err) ||
		contracts.IsVMDiskCheckFailed(err) || contracts.IsVMSourceRunning(err) {
		return false
	}
	if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return true
		}
	}
	var pe *contracts.ProviderError
	return stderrors.As(err, &pe) && (pe.Type == contracts.ErrorTypeUnavailable || pe.Type == contracts.ErrorTypeTimeout)
}
