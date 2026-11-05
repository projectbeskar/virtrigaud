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
	"log"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The wire answers of a routed call on a clustered provider (ADR-0007
// Addendum A). They go into VirtualMachine, VMSnapshot, VMClone and
// VMMigration conditions and events, readable by tenants, so they carry only a
// categorized message — the operation and the host id — never a cause's text
// (a dial address, hypervisor tool stderr, a host path). The cause is logged
// by the provider, for the operator.

// StatusWithReasons is a gRPC status with one VirtRigaud ErrorInfo
// (contracts.ErrorInfoDomain) per reason.
func StatusWithReasons(code codes.Code, msg string, reasons ...string) *status.Status {
	st := status.New(code, msg)
	withInfo := st
	for _, r := range reasons {
		next, err := withInfo.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		if err != nil {
			// Unreachable in practice (ErrorInfo always marshals); the bare
			// status is still a correct answer.
			return st
		}
		withInfo = next
	}
	return withInfo
}

// HostUnavailableStatus renders a host-scoped unavailability as
// codes.Unavailable carrying a google.rpc.ErrorInfo{Reason: HOST_UNAVAILABLE},
// which the manager maps to contracts.ErrorTypeHostUnavailable and keeps out of
// its circuit breaker. Only pe.Message (which names the host id) crosses the
// wire; the cause is logged.
func HostUnavailableStatus(pe *contracts.ProviderError) error {
	return HostUnavailableStatusFor(pe, "")
}

// HostUnavailableStatusFor is HostUnavailableStatus for an unavailability of
// the host the call was routed to, host (its target_host_id): the ErrorInfo
// names it under contracts.HostUnavailableHostKey, so the manager can say in
// the VM's status that its own host could not be reached. An empty host sets
// no metadata (the host that could not be reached may be another one).
func HostUnavailableStatusFor(pe *contracts.ProviderError, host HostID) error {
	if pe.Cause != nil {
		log.Printf("WARN %s: %v", pe.Message, pe.Cause)
	}
	st := status.New(codes.Unavailable, pe.Message)
	info := &errdetails.ErrorInfo{
		Reason: contracts.HostUnavailableReason,
		Domain: contracts.HostUnavailableErrorDomain,
	}
	if host != "" {
		info.Metadata = map[string]string{contracts.HostUnavailableHostKey: string(host)}
	}
	withInfo, err := st.WithDetails(info)
	if err != nil {
		// Unreachable in practice (ErrorInfo always marshals); a plain
		// Unavailable is still a correct, if breaker-counted, answer.
		return st.Err()
	}
	return withInfo.Err()
}

// UnknownToAgentStatus renders a task reference the host has no record of
// as codes.NotFound with a google.rpc.ErrorInfo{Reason: UNKNOWN_TO_AGENT,
// Domain: provider.virtrigaud.io} (ADR-0016 A7): never done, never "not
// performed", and out of the manager's circuit breaker.
func UnknownToAgentStatus(msg string) error {
	st := status.New(codes.NotFound, msg)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.UnknownToAgentReason,
		Domain: contracts.ErrorInfoDomain,
	})
	if err != nil {
		// Unreachable in practice (ErrorInfo always marshals); a plain
		// NotFound is still never done.
		return st.Err()
	}
	return withInfo.Err()
}

// VMOperationStatus renders a failed per-VM operation that reached its host as
// code (codes.Unknown for a failure, codes.Unavailable for one the caller
// should retry, e.g. the same copy still running) with the categorized message
// msg, plus a google.rpc.ErrorInfo{Reason: VM_OPERATION_FAILED}, which the
// manager keeps out of its circuit breaker whatever the code (ADR-0007
// Addendum A, slice 2).
func VMOperationStatus(code codes.Code, msg string) error {
	st := status.New(code, msg)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.VMOperationFailedReason,
		Domain: contracts.ErrorInfoDomain,
	})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// OutcomeUnknownRetryStatus renders a call that may have changed its host,
// whose outcome is unknown and that is safe to repeat: codes.Unavailable with a
// google.rpc.ErrorInfo{Reason: OUTCOME_UNKNOWN_RETRY} that the manager maps to
// a retryable error that is NOT "nothing done" (contracts.IsOutcomeUnknownRetry),
// plus VM_OPERATION_FAILED, plus any extra reasons. The second reason is for a
// manager that predates the first: it maps the answer as it always did
// (retryable, out of its circuit breaker, and every consumer of these calls
// retries a retryable answer first), never as a definite failure.
func OutcomeUnknownRetryStatus(msg string, extra ...string) error {
	return StatusWithReasons(codes.Unavailable, msg, append([]string{contracts.OutcomeUnknownRetryReason, contracts.VMOperationFailedReason}, extra...)...).Err()
}

// HostLostStatus renders the answer of a routed call whose host stopped
// answering part-way: code and msg as HostDownStatus chose them, with
// VM_OPERATION_FAILED and HOST_CONNECTION_LOST, the reason the manager paces
// its retries by (contracts.IsHostConnectionLost).
func HostLostStatus(code codes.Code, msg string) error {
	return StatusWithReasons(code, msg, contracts.VMOperationFailedReason, contracts.HostConnectionLostReason).Err()
}
