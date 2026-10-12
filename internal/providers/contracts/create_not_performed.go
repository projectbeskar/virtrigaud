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

package contracts

import (
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
)

// CreateNotPerformedReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to a failed Create or Clone that made
// NOTHING: no VM, no partial VM, nothing that a later step could still
// complete (it failed before its first change on the hypervisor, the
// hypervisor refused the request without acting, or the provider removed
// what it had made). It is safe to retry, and safe to treat as a refusal: a
// caller that records "a create may have happened" may clear that record.
//
// It is deliberately distinct from VMOperationFailedReason, which says only
// that a per-VM operation reached its host and failed there — on some
// providers such an operation may still complete. A provider may attach both
// to the same status (VMOperationFailedReason for the circuit breaker,
// when the failure is scoped to one VM); CreateNotPerformedReason then
// decides what was made: nothing. A provider-wide failure (the hypervisor API
// unreachable, no VMID could be allocated) carries CreateNotPerformedReason
// alone, so it counts toward the circuit breaker.
const CreateNotPerformedReason = "CREATE_NOT_PERFORMED"

// ErrCreateNotPerformed marks an error whose Create or Clone made nothing
// (CreateNotPerformedReason). The transport client adds it to the chain of
// a mapped error (MarkCreateNotPerformed); match it with IsCreateNotPerformed.
var ErrCreateNotPerformed = errors.New("the provider made nothing")

// createNotPerformedError is an error marked with ErrCreateNotPerformed; it
// keeps the wrapped error's chain (and so its type and retryability).
type createNotPerformedError struct {
	err error
}

// Error returns the wrapped error's message.
func (e *createNotPerformedError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error and ErrCreateNotPerformed.
func (e *createNotPerformedError) Unwrap() []error { return []error{e.err, ErrCreateNotPerformed} }

// MarkCreateNotPerformed returns err marked with ErrCreateNotPerformed, its
// chain intact (nil for nil).
func MarkCreateNotPerformed(err error) error {
	if err == nil {
		return nil
	}
	return &createNotPerformedError{err: err}
}

// IsCreateNotPerformed reports whether err says that its Create or Clone made
// nothing: it is marked with ErrCreateNotPerformed, or it carries a gRPC
// status with a google.rpc.ErrorInfo{Reason: CreateNotPerformedReason,
// Domain: ErrorInfoDomain}. The provider made nothing; the call is safe to
// retry and to treat as refused.
func IsCreateNotPerformed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCreateNotPerformed) {
		return true
	}
	return HasErrorInfoReason(err, CreateNotPerformedReason)
}

// HasErrorInfoReason reports whether err (or an error in its chain) carries a
// gRPC status with a google.rpc.ErrorInfo of reason in ErrorInfoDomain.
func HasErrorInfoReason(err error, reason string) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == reason && info.GetDomain() == ErrorInfoDomain {
			return true
		}
	}
	return false
}
