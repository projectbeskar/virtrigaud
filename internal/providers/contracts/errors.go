/*
Copyright 2025.

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
	"fmt"
)

// ErrorType represents the category of error
type ErrorType string

const (
	// ErrorTypeNotFound indicates resource not found
	ErrorTypeNotFound ErrorType = "NotFound"
	// ErrorTypeInvalidSpec indicates invalid specification
	ErrorTypeInvalidSpec ErrorType = "InvalidSpec"
	// ErrorTypeRetryable indicates a transient error
	ErrorTypeRetryable ErrorType = "Retryable"
	// ErrorTypeUnauthorized indicates authentication/authorization failure
	ErrorTypeUnauthorized ErrorType = "Unauthorized"
	// ErrorTypeNotSupported indicates unsupported operation
	ErrorTypeNotSupported ErrorType = "NotSupported"
	// ErrorTypeRateLimit indicates rate limiting
	ErrorTypeRateLimit ErrorType = "RateLimit"
	// ErrorTypeUnavailable indicates service unavailable
	ErrorTypeUnavailable ErrorType = "Unavailable"
	// ErrorTypeTimeout indicates operation timeout
	ErrorTypeTimeout ErrorType = "Timeout"
	// ErrorTypeQuotaExceeded indicates quota exceeded
	ErrorTypeQuotaExceeded ErrorType = "QuotaExceeded"
	// ErrorTypeConflict indicates resource conflict
	ErrorTypeConflict ErrorType = "Conflict"
	// ErrorTypeHostUnavailable indicates that ONE host of a clustered provider
	// is unknown, being drained, or unreachable (ADR-0007 Addendum A, A1). It is
	// retryable, and it is deliberately distinct from ErrorTypeUnavailable: the
	// provider itself is healthy, so it must not count toward the per-Provider
	// circuit breaker (one dead host must not fast-fail every VM on every other
	// host of the provider).
	ErrorTypeHostUnavailable ErrorType = "HostUnavailable"
)

// HostUnavailableReason is the google.rpc.ErrorInfo reason a clustered
// provider attaches to a codes.Unavailable status when the unavailability is
// scoped to one host (ADR-0007 Addendum A). The manager maps such a status to
// ErrorTypeHostUnavailable and keeps it out of its circuit breaker.
const HostUnavailableReason = "HOST_UNAVAILABLE"

// ErrorInfoDomain is the google.rpc.ErrorInfo domain of every reason a
// VirtRigaud provider attaches to a gRPC status (HostUnavailableReason,
// VMOperationFailedReason).
const ErrorInfoDomain = "provider.virtrigaud.io"

// HostUnavailableErrorDomain is the google.rpc.ErrorInfo domain of
// HostUnavailableReason.
const HostUnavailableErrorDomain = ErrorInfoDomain

// VMOperationFailedReason is the google.rpc.ErrorInfo reason a clustered
// provider attaches to a failed per-VM call that REACHED the VM's host and
// failed there — the host answered, but the operation on that one VM did not
// succeed (e.g. a blockresize beyond what the host can give, or a domain that
// refuses to start). The provider is healthy, and such a failure can be
// triggered repeatedly by one tenant's spec, so the manager keeps it out of its
// per-Provider circuit breaker (ADR-0007 Addendum A, slice 2): otherwise one
// tenant's failing VM could open the breaker for every VM, on every host, of
// every tenant of the Provider. The status keeps its historical code and
// message; only this detail is added.
const VMOperationFailedReason = "VM_OPERATION_FAILED"

// ProviderError represents a categorized error from a provider
type ProviderError struct {
	// Type categorizes the error
	Type ErrorType
	// Message describes the error
	Message string
	// Cause contains the underlying error
	Cause error
	// Retryable indicates if the operation should be retried
	Retryable bool
}

// Error implements the error interface
func (e *ProviderError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Type, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// Unwrap returns the underlying error
func (e *ProviderError) Unwrap() error {
	return e.Cause
}

// IsRetryable returns true if the error is retryable
func (e *ProviderError) IsRetryable() bool {
	return e.Retryable || e.Type == ErrorTypeRetryable ||
		e.Type == ErrorTypeUnavailable || e.Type == ErrorTypeTimeout ||
		e.Type == ErrorTypeRateLimit || e.Type == ErrorTypeHostUnavailable
}

// IsNotFound reports whether err is, or wraps, a provider NotFound error. The
// transport client maps a gRPC codes.NotFound into a *ProviderError of this
// type (see mapGRPCError), so controllers can treat an already-absent resource
// as success rather than a failure.
func IsNotFound(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeNotFound
}

// IsNotSupported reports whether err is, or wraps, a provider NotSupported
// error. The transport client maps a gRPC codes.Unimplemented into a
// *ProviderError of this type (see mapGRPCError), so a controller can tell a
// provider that does not implement an RPC (e.g. a non-clustered provider asked
// for GetHostInfo, ADR-0007 P1) apart from a transient failure — and surface a
// config-level condition rather than retrying on a tight loop forever.
func IsNotSupported(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeNotSupported
}

// IsConflict reports whether err is, or wraps, a provider Conflict error. The
// transport client maps a gRPC codes.AlreadyExists into a *ProviderError of this
// type (see mapGRPCError) — e.g. a libvirt Create whose requested domain name is
// already taken by a domain NOT owned by the requesting VirtualMachine. It is
// non-retryable: retrying cannot resolve the conflict without operator action.
func IsConflict(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeConflict
}

// IsInvalidSpec reports whether err is, or wraps, a provider InvalidSpec error.
// The transport client maps a gRPC codes.InvalidArgument into a *ProviderError
// of this type (see mapGRPCError). It is non-retryable: the same request will
// be rejected again until the spec changes (for example a libvirt image path
// outside the provider's allowed image directories).
func IsInvalidSpec(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeInvalidSpec
}

// IsRetryable reports whether err is, or wraps, a provider error of a
// transient class (see ProviderError.IsRetryable). The transport client maps a
// gRPC Unavailable or DeadlineExceeded — including a circuit-breaker
// fast-fail — into such an error (see mapGRPCError), so a controller can tell
// "the provider or the host it routes to is unreachable right now" apart from
// a request the provider rejected. A plain (uncategorized) error is not
// retryable by this definition.
func IsRetryable(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.IsRetryable()
}

// IsHostUnavailable reports whether err is, or wraps, a host-scoped
// unavailability of a clustered provider (ErrorTypeHostUnavailable).
func IsHostUnavailable(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeHostUnavailable
}

// NewHostUnavailableError creates a retryable error scoped to one host of a
// clustered provider (see ErrorTypeHostUnavailable).
func NewHostUnavailableError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeHostUnavailable,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewNotFoundError creates a not found error
func NewNotFoundError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeNotFound,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewInvalidSpecError creates an invalid spec error
func NewInvalidSpecError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeInvalidSpec,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewRetryableError creates a retryable error
func NewRetryableError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeRetryable,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewUnauthorizedError creates an unauthorized error
func NewUnauthorizedError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeUnauthorized,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewNotSupportedError creates a not supported error
func NewNotSupportedError(message string) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeNotSupported,
		Message:   message,
		Retryable: false,
	}
}

// NewRateLimitError creates a rate limit error
func NewRateLimitError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeRateLimit,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewUnavailableError creates an unavailable error
func NewUnavailableError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeUnavailable,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewTimeoutError creates a timeout error
func NewTimeoutError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeTimeout,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
}

// NewQuotaExceededError creates a quota exceeded error
func NewQuotaExceededError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeQuotaExceeded,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}

// NewConflictError creates a conflict error
func NewConflictError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeConflict,
		Message:   message,
		Cause:     cause,
		Retryable: false,
	}
}
