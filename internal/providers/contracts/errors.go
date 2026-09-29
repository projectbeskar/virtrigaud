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
	// ErrorTypeInProgress indicates that what the request asks for is still
	// being produced by an earlier request — a prepared-image artifact still
	// being imported for the same VMImage (ADR-0009 D4). It is retryable, and
	// like ErrorTypeHostUnavailable it says nothing about the provider's
	// health: the provider answered.
	ErrorTypeInProgress ErrorType = "InProgress"
)

// HostUnavailableReason is the google.rpc.ErrorInfo reason a clustered
// provider attaches to a codes.Unavailable status when the unavailability is
// scoped to one host (ADR-0007 Addendum A). The manager maps such a status to
// ErrorTypeHostUnavailable and keeps it out of its circuit breaker.
const HostUnavailableReason = "HOST_UNAVAILABLE"

// ErrorInfoDomain is the google.rpc.ErrorInfo domain of every reason a
// VirtRigaud provider attaches to a gRPC status (HostUnavailableReason,
// VMOperationFailedReason, ImageArtifactInProgressReason).
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

// ImageArtifactInProgressReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.Unavailable status of an
// ImagePrepare whose artifact is still being prepared for the same VMImage by
// another request (ADR-0009 D4, imageartifact.InProgressError). The manager
// maps such a status to ErrorTypeInProgress and keeps it out of its
// per-Provider circuit breaker: a long import through one Provider must not
// open the breaker of another Provider that shares its image location.
const ImageArtifactInProgressReason = "IMAGE_ARTIFACT_IN_PROGRESS"

// ImageSourceUnavailableReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.Unavailable status of an
// ImagePrepare that failed because of the image's source or content — the
// source server failed, refused or broke off the download, the download could
// not be staged, or the hypervisor refused to import this image's content —
// and not because the provider or its hypervisor endpoint is unreachable
// (imageartifact.SourceUnavailableError). It stays retryable, but the manager
// keeps it out of its per-Provider circuit breaker on ImagePrepare: one
// tenant's bad VMImage, retried, must not open the breaker for every tenant of
// the Provider.
const ImageSourceUnavailableReason = "IMAGE_SOURCE_UNAVAILABLE"

// VMDiskInUseReason is the google.rpc.ErrorInfo reason (in ErrorInfoDomain) a
// provider attaches to the codes.FailedPrecondition status of a per-VM
// operation it refused because another VM on the same host depends on this
// VM's disk — typically a linked clone whose backing file it is. Deleting the
// VM, reverting it to a snapshot, or creating or deleting one of its snapshots
// would remove or rewrite that file underneath the other VM. The refusal is not
// retryable as such (it holds until the dependent VMs are gone); the manager
// maps it to ErrorTypeConflict, and FailedPrecondition keeps it out of the
// per-Provider circuit breaker.
const VMDiskInUseReason = "VM_DISK_IN_USE"

// VMDiskCheckFailedReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.Unavailable status of a
// per-VM operation it did not perform because it could not verify that no
// other VM depends on this VM's disk — the check reads every VM's disk chain
// on the host, and one of them could not be read (e.g. a disk the provider's
// host account may not read, an inactive storage pool). It is retryable, but
// the provider is healthy and one unreadable disk on a host must not stop
// every VM of the Provider, so the manager keeps it out of its per-Provider
// circuit breaker.
const VMDiskCheckFailedReason = "VM_DISK_CHECK_FAILED"

// VMPreviousIncarnationReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a clustered provider attaches to the codes.AlreadyExists
// status of a Create or Clone it refused because a domain VirtRigaud created
// for the SAME namespace and name — a previous incarnation of the requesting
// VirtualMachine, stamped with another UID (orphaned with orphan-on-delete,
// left by a force-delete, or restored from a backup with a new UID) — exists
// on a host of the Provider (ADR-0007 A6, R2 and R3). Nothing was created.
//
// Unlike a plain AlreadyExists (a foreign or unstamped same-named domain),
// it must NOT make the manager exclude the host and re-schedule: moving on to
// another host is exactly how a second domain for the same namespace and name
// is made (A6 decision 2). The manager keeps the pending host and holds the VM
// (RestorePending) until an administrator re-attaches or removes the previous
// incarnation. It maps to ErrorTypeConflict, which never counts toward the
// per-Provider circuit breaker.
const VMPreviousIncarnationReason = "VM_PREVIOUS_INCARNATION"

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
		e.Type == ErrorTypeRateLimit || e.Type == ErrorTypeHostUnavailable ||
		e.Type == ErrorTypeInProgress
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

// ErrVMDiskInUse marks (in an error's chain) a provider's refusal carrying
// VMDiskInUseReason: another VM on the host depends on this VM's disk. The
// transport client wraps it into the Conflict it maps that refusal to.
var ErrVMDiskInUse = errors.New("another VM depends on this VM's disk")

// IsVMDiskInUse reports whether err is a provider's VMDiskInUseReason refusal
// (a Conflict that says so), as opposed to any other Conflict.
func IsVMDiskInUse(err error) bool {
	return IsConflict(err) && errors.Is(err, ErrVMDiskInUse)
}

// ErrVMDiskCheckFailed marks (in an error's chain) a provider's answer
// carrying VMDiskCheckFailedReason: the operation was not performed because
// the provider could not verify that no other VM uses this VM's disk (on a
// clustered provider: on every host of the Provider). The transport client
// wraps it into the retryable error it maps that answer to.
var ErrVMDiskCheckFailed = errors.New("the provider could not verify that no other VM uses this VM's disk")

// IsVMDiskCheckFailed reports whether err is a provider's
// VMDiskCheckFailedReason answer (a retryable error that says so).
func IsVMDiskCheckFailed(err error) bool {
	return IsRetryable(err) && errors.Is(err, ErrVMDiskCheckFailed)
}

// ErrVMPreviousIncarnation marks (in an error's chain) a provider's refusal
// carrying VMPreviousIncarnationReason: a previous incarnation of the
// requesting VirtualMachine exists on a host of the clustered Provider. The
// transport client wraps it into the Conflict it maps that refusal to.
var ErrVMPreviousIncarnation = errors.New("a previous incarnation of this VirtualMachine exists on a host of the Provider")

// VMPreviousIncarnationKindKey is the google.rpc.ErrorInfo metadata key of a
// VMPreviousIncarnationReason status that says which kind of domain was
// found; its only value today is VMPreviousIncarnationKindOwn. Absent means a
// previous incarnation under another UID.
const VMPreviousIncarnationKindKey = "incarnation"

// VMPreviousIncarnationKindOwn marks a VMPreviousIncarnationReason status
// whose domain is stamped with the requester's OWN UID, on another host of the
// Provider: the VirtualMachine's own domain, whose placement record was lost.
// Nothing has to be re-stamped; the VM's pending host must point at that host,
// and deleting the VM must not release it while the domain runs elsewhere.
const VMPreviousIncarnationKindOwn = "own"

// ErrVMOwnDomainElsewhere marks (in an error's chain, next to
// ErrVMPreviousIncarnation) a VMPreviousIncarnationReason refusal of kind
// VMPreviousIncarnationKindOwn.
var ErrVMOwnDomainElsewhere = errors.New("this VirtualMachine's own domain exists on another host of the Provider")

// IsVMOwnDomainElsewhere reports whether err is a provider's previous
// incarnation refusal of kind VMPreviousIncarnationKindOwn.
func IsVMOwnDomainElsewhere(err error) bool {
	return IsVMPreviousIncarnation(err) && errors.Is(err, ErrVMOwnDomainElsewhere)
}

// IsVMPreviousIncarnation reports whether err is a provider's
// VMPreviousIncarnationReason refusal (a Conflict that says so), as opposed
// to any other Conflict (ADR-0007 A6, R2): the caller must hold the VM on its
// pending host, never exclude the host.
func IsVMPreviousIncarnation(err error) bool {
	return IsConflict(err) && errors.Is(err, ErrVMPreviousIncarnation)
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

// IsInProgress reports whether err is, or wraps, an ErrorTypeInProgress error:
// what the request asks for is still being produced by an earlier request.
func IsInProgress(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Type == ErrorTypeInProgress
}

// NewInProgressError creates a retryable error for a request whose target is
// still being produced by an earlier request (see ErrorTypeInProgress).
func NewInProgressError(message string, cause error) *ProviderError {
	return &ProviderError{
		Type:      ErrorTypeInProgress,
		Message:   message,
		Cause:     cause,
		Retryable: true,
	}
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
