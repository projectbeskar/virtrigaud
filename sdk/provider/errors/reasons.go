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

package errors

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrorInfoDomain is the google.rpc.ErrorInfo domain of every reason a
// VirtRigaud provider attaches to a gRPC status. The manager ignores a
// reason in any other domain.
const ErrorInfoDomain = "provider.virtrigaud.io"

// Part A ErrorInfo reasons (ADR-0016), for the answers of a per-host agent.
// Every one of them is breaker-exempt: the manager keeps a status carrying
// any of them (in ErrorInfoDomain) out of its per-Provider circuit breaker,
// whatever the status code, because the provider answered and the fact is
// about one host, VM or operation. Each comment says whether the reason is a
// definite not-performed: the request was refused before it was accepted, so
// it did nothing. A reason that is not one proves nothing about what the
// operation did.
const (
	// ReasonUnknownToAgent (codes.NotFound): the host has no record of the
	// task or operation asked about, for example a task reference it never
	// issued. Breaker-exempt. NOT a definite not-performed: a task answered
	// with it is never done and never "not performed".
	ReasonUnknownToAgent = "UNKNOWN_TO_AGENT"
	// ReasonVMBusy: the host runs another, non-terminal operation for the
	// same VM and refused this one. Breaker-exempt. NOT a definite
	// not-performed: the request may re-deliver an attempt already accepted.
	ReasonVMBusy = "VM_BUSY"
	// ReasonProviderMismatch: the request came from a Provider other than
	// the one the host is enrolled with and was refused before any other
	// check. Breaker-exempt. A definite not-performed of this request.
	ReasonProviderMismatch = "PROVIDER_MISMATCH"
	// ReasonUnattested: the host's agent has not yet proved the continuity of
	// its store and refuses mutations. Breaker-exempt. NOT a definite
	// not-performed.
	ReasonUnattested = "UNATTESTED"
	// ReasonAgentLost: the host's agent lost or cannot trust its store and
	// refuses mutations until it is re-enrolled. Breaker-exempt. NOT a
	// definite not-performed.
	ReasonAgentLost = "AGENT_LOST"
	// ReasonDigestMismatch: the request bytes the host received do not match
	// the authorised digest; refused. Breaker-exempt. A definite
	// not-performed of this request.
	ReasonDigestMismatch = "DIGEST_MISMATCH"
	// ReasonCancelRefused: a cancellation was refused because the operation
	// it names passed an irreversible step (or its outcome is unknown); the
	// operation goes on. Breaker-exempt. A definite not-performed of the
	// cancellation only, never of the operation it names.
	ReasonCancelRefused = "CANCEL_REFUSED"
	// ReasonDeadlineExceededNotPerformed: the host refused to accept the
	// operation because its acceptance deadline had passed; it never runs.
	// Breaker-exempt whatever the code. A definite not-performed. A plain
	// DeadlineExceeded without this reason is never one.
	ReasonDeadlineExceededNotPerformed = "DEADLINE_EXCEEDED_NOT_PERFORMED"
)

// WithReason returns a gRPC status error of code and message carrying a
// google.rpc.ErrorInfo of reason in ErrorInfoDomain, with metadata (may be
// nil). Metadata must never hold a secret, an endpoint, a URI or an address.
// If the detail cannot be attached the plain status is returned.
func WithReason(code codes.Code, message, reason string, metadata map[string]string) error {
	st := status.New(code, message)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorInfoDomain, Metadata: metadata})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}
