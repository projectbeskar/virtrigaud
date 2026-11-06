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

// Operation-grant ErrorInfo reasons (ADR-0016), for the answers of a
// per-host agent that verifies operation grants (see OperationEnvelope in
// provider.proto). No built-in provider emits them yet. Like the Part A
// reasons, every one of them is breaker-exempt, and each comment says whether
// the reason is a definite not-performed. A reason that is not one proves
// nothing about what the operation did.
const (
	// ReasonGrantUnauthenticated: the operation grant's signature, certificate
	// chain, certificate validity or authority generation did not verify, and the
	// request was refused. Breaker-exempt. A definite not-performed of this
	// request.
	ReasonGrantUnauthenticated = "GRANT_UNAUTHENTICATED"
	// ReasonGrantMismatch: the operation grant is authentic but does not fit
	// this call (installation, Provider, host, enrolment, grant kind, operation
	// kind, RPC, request type, grant version, or a field that the grant version
	// does not define), and the request was refused. Breaker-exempt. A definite
	// not-performed of this request.
	ReasonGrantMismatch = "GRANT_MISMATCH"
	// ReasonNonceConsumed: the grant's single-use nonce was already used, and this
	// delivery was refused. Breaker-exempt. A definite not-performed of this
	// delivery only; it says nothing about the earlier use of the nonce.
	ReasonNonceConsumed = "NONCE_CONSUMED"
	// ReasonRequestConsumed: the approved request that the grant executes was
	// already executed under this enrolment, and this delivery was refused.
	// Breaker-exempt. A definite not-performed of this delivery only; it says
	// nothing about the earlier execution.
	ReasonRequestConsumed = "REQUEST_CONSUMED"
	// ReasonIdentityRequired: the Provider or host requires an operation grant and
	// the call carried none. Breaker-exempt. NOT a definite not-performed.
	ReasonIdentityRequired = "IDENTITY_REQUIRED"
	// ReasonFencingRequired: the Provider or host requires an operation grant
	// issued under fencing. Breaker-exempt. NOT a definite not-performed.
	ReasonFencingRequired = "FENCING_REQUIRED"
	// ReasonStaleEpoch: the grant's epoch is older than the epoch the host has
	// activated. Breaker-exempt. NOT a definite not-performed.
	ReasonStaleEpoch = "STALE_EPOCH"
	// ReasonStaleIssued: the grant was issued below the floor that the host has
	// recorded for the Provider. Breaker-exempt. NOT a definite not-performed.
	ReasonStaleIssued = "STALE_ISSUED"
	// ReasonEpochNotActivated: the grant's epoch has not been activated on the
	// host. Breaker-exempt. NOT a definite not-performed.
	ReasonEpochNotActivated = "EPOCH_NOT_ACTIVATED"
	// ReasonJumpTooLarge: an activation would raise the host's epoch by more than
	// its bound. Breaker-exempt. NOT a definite not-performed.
	ReasonJumpTooLarge = "JUMP_TOO_LARGE"
	// ReasonQuarantined: the host is quarantined for this Provider and refuses
	// mutations. Breaker-exempt. NOT a definite not-performed.
	ReasonQuarantined = "QUARANTINED"
	// ReasonOpIDConflict: the host knows the grant's op_id with different identity
	// fields. Breaker-exempt. NOT a definite not-performed.
	ReasonOpIDConflict = "OP_ID_CONFLICT"
	// ReasonBadNonce: the nonce is not one the host issued, or it has expired.
	// Breaker-exempt. NOT a definite not-performed.
	ReasonBadNonce = "BAD_NONCE"
	// ReasonVMBindingStale: the grant's binding is not the VM's current placement
	// on the host. Breaker-exempt. NOT a definite not-performed.
	ReasonVMBindingStale = "VM_BINDING_STALE"
	// ReasonClockUncorroborated: the host refused an attestation because it could
	// not corroborate its clock. Breaker-exempt. NOT a definite not-performed.
	ReasonClockUncorroborated = "CLOCK_UNCORROBORATED"
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
