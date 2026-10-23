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

// OutcomeUnknownRetryReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a clustered provider attaches to the codes.Unavailable
// status of a call that may have changed its host and whose outcome it cannot
// tell (R25): a command that may change the host was sent and the host stopped
// answering, or the call's time budget ran out while the host's job may still
// complete. The call is safe to repeat (idempotent per request), so it is
// retryable — but it is NOT "nothing was done": the manager must not clear a
// marker, re-place, or count it as a definite outcome on it
// (contracts.IsNothingDone is false). The provider always sends it together
// with VMOperationFailedReason, so a manager that predates this reason maps
// the answer as before (retryable, kept out of the circuit breaker).
const OutcomeUnknownRetryReason = "VM_OUTCOME_UNKNOWN_RETRY"

// HostStorageMisconfiguredReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a clustered provider attaches to the
// codes.FailedPrecondition status of an operation it did not perform because
// the storage of the ONE host it was routed to is not set up as VirtRigaud
// requires (e.g. a libvirt host whose "default" storage pool is not at the
// required directory; ADR-0007 Addendum B, #415). Nothing was changed. It is
// scoped to that host: the manager may place the work on another host, but
// retrying on the same host fails the same way until an administrator fixes
// it. It is sent together with VMOperationFailedReason, which keeps it out of
// the per-Provider circuit breaker.
const HostStorageMisconfiguredReason = "HOST_STORAGE_MISCONFIGURED"

// IsHostStorageMisconfigured reports whether err is a provider answer carrying
// HostStorageMisconfiguredReason.
func IsHostStorageMisconfigured(err error) bool {
	return HasErrorInfoReason(err, HostStorageMisconfiguredReason)
}

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

// VMSourceRunningReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.FailedPrecondition status
// of a Clone it refused, before copying anything, because the source VM is not
// powered off. A libvirt full clone copies the source's disk chain with
// qemu-img, which cannot take the image lock a running QEMU holds (and a copy
// of a disk that is being written is not a consistent clone), so the libvirt
// provider requires the source to be shut off. The refusal holds until the
// source is powered off; it is not a failure of the clone. The manager maps it
// to a retryable error marked ErrVMSourceRunning, keeps the VMClone Pending
// (SourceMustBePoweredOff) and retries with a backoff; FailedPrecondition keeps
// it out of the per-Provider circuit breaker.
const VMSourceRunningReason = "VM_SOURCE_RUNNING"

// ReconfigureNeedsPowerOffReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.FailedPrecondition status
// of a Reconfigure it refused, before changing anything, because the VM is
// not powered off and the hypervisor cannot apply the requested change to it
// now, nor stage it for its next boot — on vSphere, CPU or memory hot-add (or
// CPU hot-remove, for fewer vCPUs) is not enabled on the VM, the memory is
// above its hot-plug limit, memory is never removed from a running VM, a
// shared-bus or multi-writer disk is grown only while the VM is off, and a
// suspended VM takes no change at all. The change is applied by
// a Reconfigure once the VM is powered off. The manager maps it to a
// retryable error marked ErrReconfigureNeedsPowerOff, records it as
// Reconfiguring=False/ResizePendingPowerOff and asks again on its backoff;
// FailedPrecondition keeps it out of the per-Provider circuit breaker. A
// provider that can stage the change for the next boot (libvirt) answers
// restart_required instead.
const ReconfigureNeedsPowerOffReason = "RECONFIGURE_NEEDS_POWER_OFF"

// DiskSizeExceedsLimitReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.FailedPrecondition status
// of an ImportDisk it refused because the disk it was about to land has a
// virtual size above ImportDiskRequest.MaxVirtualSizeBytes. Nothing was
// landed. The manager maps it to a Conflict marked ErrDiskSizeExceedsLimit and
// holds the migration (DiskSizeExceedsLimit) instead of failing it.
const DiskSizeExceedsLimitReason = "DISK_SIZE_EXCEEDS_LIMIT"

// DiskSizeUnverifiableReason is DiskSizeExceedsLimitReason's counterpart for
// an ImportDisk refused because MaxVirtualSizeBytes was set and the provider
// could not read the disk's virtual size (fail closed). Nothing was landed.
// The manager holds the migration (DiskSizeLimitUnverifiable).
const DiskSizeUnverifiableReason = "DISK_SIZE_UNVERIFIABLE"

// ErrDiskSizeExceedsLimit marks (in an error's chain) a provider's refusal
// carrying DiskSizeExceedsLimitReason.
var ErrDiskSizeExceedsLimit = errors.New("the disk is above the disk-size limit")

// ErrDiskSizeUnverifiable marks (in an error's chain) a provider's refusal
// carrying DiskSizeUnverifiableReason.
var ErrDiskSizeUnverifiable = errors.New("the disk's size could not be checked against the disk-size limit")

// IsDiskSizeExceedsLimit reports whether err is a provider's
// DiskSizeExceedsLimitReason refusal.
func IsDiskSizeExceedsLimit(err error) bool {
	return errors.Is(err, ErrDiskSizeExceedsLimit)
}

// IsDiskSizeUnverifiable reports whether err is a provider's
// DiskSizeUnverifiableReason refusal.
func IsDiskSizeUnverifiable(err error) bool {
	return errors.Is(err, ErrDiskSizeUnverifiable)
}

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

// VMIsImageArtifactReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.FailedPrecondition status
// of a per-VM operation (Delete, Power, Reconfigure, HardwareUpgrade, snapshot
// create/delete/revert) it refused because the object addressed is not a VM
// but shared image state: a hypervisor template, or an object carrying a
// VirtRigaud prepared-image stamp (an image, or an image import in flight).
// Performing it would destroy or change the image for every VM cloned from
// it. Nothing was changed. The refusal is not retryable as such (it holds
// until an operator detaches the VirtualMachine, e.g. with orphan-on-delete);
// the manager maps it to ErrorTypeConflict, and FailedPrecondition keeps it
// out of the per-Provider circuit breaker.
const VMIsImageArtifactReason = "VM_IS_IMAGE_ARTIFACT"

// ErrVMIsImageArtifact marks (in an error's chain) a provider's refusal
// carrying VMIsImageArtifactReason. The transport client wraps it into the
// Conflict it maps that refusal to.
var ErrVMIsImageArtifact = errors.New("the object is a template or an image artifact, not a VM")

// IsVMIsImageArtifact reports whether err is a provider's
// VMIsImageArtifactReason refusal (a Conflict that says so), as opposed to any
// other Conflict.
func IsVMIsImageArtifact(err error) bool {
	return IsConflict(err) && errors.Is(err, ErrVMIsImageArtifact)
}

// VMNotOwnedReason is the google.rpc.ErrorInfo reason (in ErrorInfoDomain) a
// provider attaches to the codes.NotFound status of an owner-checked per-VM
// operation when a VM exists at the requested id but is not the requesting
// VirtualMachine's: it is stamped with another owner, or it is unstamped and
// not named like the owner (e.g. a Proxmox VMID reused by an unrelated VM).
// The VM was not touched. For a Delete the requester's own VM is treated as
// gone, but — unlike a plain NotFound — something is known to still run at
// that id, so the manager records it (a VMLeftOnHypervisor Warning) instead
// of releasing silently: the requester may be a re-created object (for
// example after a backup restore, under a new UID) whose VM is still running.
const VMNotOwnedReason = "VM_NOT_OWNED"

// ErrVMNotOwned marks (in an error's chain) a provider's NotFound carrying
// VMNotOwnedReason. The transport client wraps it into the NotFound it maps
// that answer to.
var ErrVMNotOwned = errors.New("a VM exists at that id but is not the requester's")

// IsVMNotOwned reports whether err is a provider's VMNotOwnedReason answer (a
// NotFound that says so), as opposed to a plain NotFound.
func IsVMNotOwned(err error) bool {
	return IsNotFound(err) && errors.Is(err, ErrVMNotOwned)
}

// OwnerStampUnreadableReason is the google.rpc.ErrorInfo reason (in
// ErrorInfoDomain) a provider attaches to the codes.FailedPrecondition status
// of an owner-checked per-VM operation it refused because the VM's owner
// stamp cannot be read or records more than one owner: who owns the VM cannot
// be told, so nothing was changed. The refusal holds until an administrator
// fixes the stamp or removes the VM by hand; the manager maps it to a
// non-retryable Conflict, and FailedPrecondition keeps it out of the circuit
// breaker.
const OwnerStampUnreadableReason = "OWNER_STAMP_UNREADABLE"

// ErrOwnerStampUnreadable marks (in an error's chain) a provider's refusal
// carrying OwnerStampUnreadableReason.
var ErrOwnerStampUnreadable = errors.New("the VM's owner stamp cannot be read or records more than one owner")

// IsOwnerStampUnreadable reports whether err is a provider's
// OwnerStampUnreadableReason refusal (a Conflict that says so).
func IsOwnerStampUnreadable(err error) bool {
	return IsConflict(err) && errors.Is(err, ErrOwnerStampUnreadable)
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

// ErrOutcomeUnknownRetry marks (in an error's chain) a provider's answer
// carrying OutcomeUnknownRetryReason: the call may have changed the host, its
// outcome is unknown, and it is safe to send again. The transport client wraps
// it into the retryable error it maps that answer to. It is deliberately not
// ErrOperationNotPerformed.
var ErrOutcomeUnknownRetry = errors.New("the provider cannot tell whether the call changed its host; it is safe to retry")

// IsOutcomeUnknownRetry reports whether err is a provider's
// OutcomeUnknownRetryReason answer (a retryable error that says so). Such an
// answer is retried, and proves nothing about what the call did.
func IsOutcomeUnknownRetry(err error) bool {
	return IsRetryable(err) && errors.Is(err, ErrOutcomeUnknownRetry)
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

// ErrVMSourceRunning marks (in an error's chain) a provider's refusal carrying
// VMSourceRunningReason: the clone's source VM must be powered off before it
// is cloned. The transport client wraps it into the retryable error it maps
// that refusal to.
var ErrVMSourceRunning = errors.New("the clone's source VM must be powered off")

// IsVMSourceRunning reports whether err is a provider's VMSourceRunningReason
// refusal (a retryable error that says so): the caller keeps the clone
// pending until the source VM is powered off, and never fails it for this.
func IsVMSourceRunning(err error) bool {
	return IsRetryable(err) && errors.Is(err, ErrVMSourceRunning)
}

// ErrReconfigureNeedsPowerOff marks (in an error's chain) a provider's
// refusal carrying ReconfigureNeedsPowerOffReason: the VM's change
// can be applied only while it is powered off. The transport client wraps it
// into the retryable error it maps that refusal to.
var ErrReconfigureNeedsPowerOff = errors.New("the VM's change can be applied only while it is powered off")

// IsReconfigureNeedsPowerOff reports whether err is a provider's
// ReconfigureNeedsPowerOffReason refusal (a retryable error that says so):
// nothing was changed, and the change is applied once the VM is powered off.
func IsReconfigureNeedsPowerOff(err error) bool {
	return IsRetryable(err) && errors.Is(err, ErrReconfigureNeedsPowerOff)
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
