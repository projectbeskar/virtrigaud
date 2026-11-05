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
	"errors"
	"fmt"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// How a routed call answers a host that stops answering (R25).
//
// The manager reads HOST_UNAVAILABLE as a DEFINITE "nothing was done" (it
// re-places, re-sends and clears markers on it), so it may be answered only
// when no command of the call ran on the host. Once a command that may change
// the host has been sent, a dropped connection says nothing about what that
// command did. A provider records how far each routed call got on its host
// (HostDownStage) and classifies its own failures as "the host stopped
// answering" or not; HostDownStatus then picks the answer from the kind of RPC
// and the stage.

// OpKind says how a routed RPC answers a host that stops answering
// (HostDownStatus).
type OpKind int

const (
	// OpWrite is an RPC that changes the host. A transport failure is
	// HOST_UNAVAILABLE only while no command of the call was answered and none
	// that may change the host was sent, "not performed" while only reads ran,
	// and an unknown outcome (codes.Unknown) once a command that may change the
	// host was sent.
	OpWrite OpKind = iota
	// OpRead is an RPC that only reads (Describe, GetDiskInfo, TaskStatus):
	// it cannot half-do anything, so a host that stops answering at any point
	// is HOST_UNAVAILABLE.
	OpRead
	// OpWriteRetry is an OpWrite whose unknown outcome is answered "retry"
	// (codes.Unavailable with VM_OPERATION_FAILED, the text saying the outcome
	// is unknown) instead of codes.Unknown: the RPC is idempotent per request
	// and the manager retries it on the same host — a SnapshotCreate (adopted
	// by request token) and a Clone (stamped, and its copy under flock). A
	// codes.Unknown would fail the clone and remove its target.
	OpWriteRetry
)

// HostDownStage says how far a call got on its host when the host stopped
// answering.
type HostDownStage int

const (
	// HostDownNothingRan: no command of the call was answered and none that may
	// change the host was sent — the lease, the dial, or the very first command
	// found the connection dead. Nothing was done.
	HostDownNothingRan HostDownStage = iota
	// HostDownReadsOnly: the host answered at least one command, and no
	// command that may change it was sent. Nothing was done.
	HostDownReadsOnly
	// HostDownWriteSent: a command that may change the host was sent. It may
	// have completed, or still be running: the outcome is unknown.
	HostDownWriteSent
)

// Wire texts of a transport failure of a routed write RPC. They name the
// operation and the host only.
const (
	// WireHostNotPerformedFmt is the "not performed" answer: the host stopped
	// answering after only reads ran.
	WireHostNotPerformedFmt = "failed to %s on host %q: the host stopped answering before the operation changed " +
		"anything; nothing was done"
	// WireHostUnknownFmt is the unknown-outcome answer of an OpWrite.
	WireHostUnknownFmt = "failed to %s on host %q: the host stopped answering while the operation ran; " +
		"its outcome is unknown"
	// WireHostUnknownRetryFmt is the unknown-outcome answer of an OpWriteRetry.
	WireHostUnknownRetryFmt = "failed to %s on host %q: the host stopped answering while the operation ran; " +
		"its outcome is unknown and it may still complete on the host: retry"
)

// HostDownStatus is the wire answer of a routed RPC of kind, operation op, on
// host, whose host stopped answering after the call got to stage. failClosed
// forces HOST_UNAVAILABLE whatever ran before: a guard that runs before
// anything is changed, and says so, fails closed.
//
//   - no command of the call was answered and none that may change the host
//     was sent, or the RPC only reads (OpRead), or failClosed:
//     HOST_UNAVAILABLE naming host;
//   - only reads ran: "not performed" (codes.Unavailable, VM_OPERATION_FAILED,
//     HOST_CONNECTION_LOST);
//   - a command that may change the host was sent (it may have completed, or
//     still be running): an unknown outcome — codes.Unknown with
//     VM_OPERATION_FAILED and HOST_CONNECTION_LOST, or, for OpWriteRetry, the
//     "retry" form of it (OutcomeUnknownRetryStatus).
//
// None counts toward the manager's per-Provider circuit breaker. Only the
// operation and host cross the wire.
func HostDownStatus(kind OpKind, op string, host HostID, stage HostDownStage, failClosed bool) error {
	switch {
	case kind == OpRead || failClosed || stage == HostDownNothingRan:
		return HostUnavailableStatusFor(contracts.NewHostUnavailableError(
			fmt.Sprintf("%s: host %q is unreachable", op, host), nil), host)
	case stage == HostDownReadsOnly:
		return HostLostStatus(codes.Unavailable, fmt.Sprintf(WireHostNotPerformedFmt, op, host))
	case kind == OpWriteRetry:
		log.Printf("WARN %s on host %s: outcome unknown (a command that may change the host was sent)", op, host)
		return OutcomeUnknownRetryStatus(fmt.Sprintf(WireHostUnknownRetryFmt, op, host), contracts.HostConnectionLostReason)
	default:
		log.Printf("WARN %s on host %s: outcome unknown (a command that may change the host was sent)", op, host)
		return HostLostStatus(codes.Unknown, fmt.Sprintf(WireHostUnknownFmt, op, host))
	}
}

// HostOpFailedStatus is the wire answer of a routed operation op that reached
// host and failed there (the host answered, and the operation failed on that
// one VM): VM_OPERATION_FAILED, codes.Unknown, with the message "failed to
// <op> on host <host>" followed by detail when the provider composed a
// requester-facing account of the failure (never raw host output).
func HostOpFailedStatus(op string, host HostID, detail string) error {
	msg := fmt.Sprintf("failed to %s on host %q", op, host)
	if detail != "" {
		msg += ": " + detail
	}
	return VMOperationStatus(codes.Unknown, msg)
}

// HostUnavailableError is the host-scoped unavailability of the very host a
// routed call leases (an unknown, removed or draining host, or one whose
// connection cannot be resolved). It is transparent — Error and Unwrap are
// PE's, so contracts.IsHostUnavailable and every message are unchanged — and
// only tells TypedStatus which host to name in the answer's ErrorInfo
// (HostUnavailableStatusFor).
type HostUnavailableError struct {
	// Host is the host the call was routed to.
	Host HostID
	// PE is the HOST_UNAVAILABLE provider error; its message crosses the wire.
	PE *contracts.ProviderError
}

// Error returns PE's message.
func (e *HostUnavailableError) Error() string { return e.PE.Error() }

// Unwrap exposes PE.
func (e *HostUnavailableError) Unwrap() error { return e.PE }

// OpError is a failure of a routed operation that carries its own
// categorized, tenant-safe wire message and code: a time budget that ran out,
// a copy of the same disk already running, a host tool missing. The
// provider's error mapping puts Wire (never the cause) on the wire, as
// VMOperationStatus(Code, Wire) or, when OutcomeUnknown,
// OutcomeUnknownRetryStatus(Wire).
type OpError struct {
	// Code is the wire code: codes.Unknown (the operation failed) or
	// codes.Unavailable (retry: the same operation is still in progress).
	Code codes.Code
	// Wire is the categorized message sent to the manager.
	Wire string
	// Cause is logged, never sent.
	Cause error
	// OutcomeUnknown marks a codes.Unavailable answer for a call that may have
	// changed the host and whose outcome is unknown (the routed budget ran out
	// while the host's job may still complete; the request's snapshot is still
	// being taken): it is sent with OutcomeUnknownRetryReason, so that the
	// manager retries it but never reads it as "nothing was done" (R25).
	OutcomeUnknown bool
}

// Error returns the wire message and the cause, for the provider's log.
func (e *OpError) Error() string {
	if e.Cause != nil {
		return e.Wire + ": " + e.Cause.Error()
	}
	return e.Wire
}

// Unwrap exposes the cause.
func (e *OpError) Unwrap() error { return e.Cause }

// TypedStatus returns the categorized answer of err from a routed call for
// the hypervisor-neutral classes, or nil when err is none of them:
//
//   - a HostUnavailableError -> HOST_UNAVAILABLE naming its host;
//   - an UnknownHostTaskError -> NotFound + UNKNOWN_TO_AGENT;
//   - a typed ProviderError: InvalidSpec -> InvalidArgument, HostUnavailable
//     -> HOST_UNAVAILABLE (no host metadata), NotFound -> NotFound, Conflict
//     -> AlreadyExists.
//
// A provider checks its own typed refusals (and an OpError, whose log line is
// the provider's) first.
func TypedStatus(err error) error {
	var rh *HostUnavailableError
	if errors.As(err, &rh) {
		return HostUnavailableStatusFor(rh.PE, rh.Host)
	}
	var ut *UnknownHostTaskError
	if errors.As(err, &ut) {
		return UnknownToAgentStatus(ut.Error())
	}
	var pe *contracts.ProviderError
	if errors.As(err, &pe) {
		switch pe.Type {
		case contracts.ErrorTypeInvalidSpec:
			return status.Error(codes.InvalidArgument, pe.Message)
		case contracts.ErrorTypeHostUnavailable:
			return HostUnavailableStatus(pe)
		case contracts.ErrorTypeNotFound:
			return status.Error(codes.NotFound, pe.Message)
		case contracts.ErrorTypeConflict:
			return status.Error(codes.AlreadyExists, pe.Message)
		}
	}
	return nil
}

// ProviderFailureStatus is the answer of a routed call's failure that is
// neither categorized nor scoped to its host: a provider-level Unavailable
// (the provider itself is not ready, e.g. its host registry is not
// initialized) is a plain Unavailable, which the manager's circuit breaker
// counts; anything else is a plain "failed to <op>".
func ProviderFailureStatus(op string, err error) error {
	var pe *contracts.ProviderError
	if errors.As(err, &pe) && pe.Type == contracts.ErrorTypeUnavailable {
		return status.Error(codes.Unavailable, pe.Message)
	}
	return status.Errorf(codes.Unknown, "failed to %s", op)
}
