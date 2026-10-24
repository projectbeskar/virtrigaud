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

package libvirt

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Post-create per-VM routing for a CLUSTERED provider (ADR-0007 Addendum A,
// A1). The operator names the host on every per-VM call (target_host_id); the
// provider never looks a VM's host up by itself. withHostConn is the one place
// a routed call obtains its host:
//
//  1. lease the host's connection from the cluster registry (ConnFor),
//  2. run the call's shared core with that leased connection — the SAME core the
//     single-host path runs on p.virshProvider — so the virsh read, the native
//     read and the shadow read all hit that one host,
//  3. release the lease.
//
// Slice 1 routes Describe and Delete; slice 2 routes Power and Reconfigure;
// slice 3 routes the snapshot family, Clone, GetDiskInfo and ExportDisk (s3 and
// nfs), and host-encodes task references so TaskStatus is routed too
// (routed_tasks.go); slice 4 runs ListVMs across every host (routed_list.go)
// and routes TransferOwner (routed_transfer_owner.go). Every routed call except Create checks
// the domain's owner stamp before it reads or changes anything
// (withOwnedDomain; TransferOwner checks it before it stamps). ImportDisk,
// DeleteLandedDisk and the nfs DeleteStagedDisk are routed to the target
// VirtualMachine's pending host since ADR-0007 Addendum B (routed_import.go,
// routed_migration_cleanup.go); the import runs the name and cross-host disk
// checks instead of an owner check, as a Create does.

// emptyTargetHostMessage is the InvalidArgument message for a routed call on a
// clustered provider that names no host. It never defaults to one (D9).
const emptyTargetHostMessage = "clustered libvirt provider requires target_host_id: " +
	"the operator names the VM's bound host on every per-VM call (ADR-0007 Addendum A)"

// withHostConn leases the connection of host hostID from the cluster registry,
// runs fn against it, and releases the lease (ADR-0007 Addendum A, A1).
//
//   - An empty (or whitespace) hostID is an InvalidSpec error — a clustered
//     provider never defaults to a host.
//   - An unknown, removed or draining host, or a failed lazy dial, is a
//     retryable HOST-scoped unavailability (contracts.ErrorTypeHostUnavailable,
//     on the wire codes.Unavailable + a HOST_UNAVAILABLE ErrorInfo), which the
//     manager keeps out of its per-Provider circuit breaker. A host that starts
//     draining while fn runs is NOT cut off: the held lease keeps its connection
//     open (D3 graceful drain).
//
// fn receives a libvirtConn that is valid only until withHostConn returns,
// unless fn retains it (see hostLease.retain) for work that outlives the call —
// the detached shadow-compare read does exactly that, so it runs on the same
// lease as the virsh read it shadows.
//
// fn also receives the context its host commands must run with: ctx carrying
// the call's hostProgress (host_progress.go), which records whether the host
// answered a command and whether a command that may change it was sent. A
// failure of fn is returned as a hostOpError carrying that record, so the wire
// answer says how far the call got (hostOpRPCError, R25).
func (p *Provider) withHostConn(ctx context.Context, hostID string, fn func(ctx context.Context, c libvirtConn) error) error {
	id := strings.TrimSpace(hostID)
	if id == "" {
		return contracts.NewInvalidSpecError(emptyTargetHostMessage, nil)
	}
	if p.clusterReg == nil {
		return contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil)
	}
	lease, err := p.clusterReg.ConnFor(ctx, hostconn.HostID(id))
	if err != nil {
		return &routedHostUnavailableError{host: hostconn.HostID(id),
			pe: contracts.NewHostUnavailableError(fmt.Sprintf("connect to host %q", id), err)}
	}
	vc, err := virshConnFrom(lease)
	if err != nil {
		_ = lease.Close()
		return &routedHostUnavailableError{host: hostconn.HostID(id),
			pe: contracts.NewHostUnavailableError(fmt.Sprintf("resolve connection of host %q", id), err)}
	}
	h := newHostLease(vc, lease)
	defer h.release()
	hctx, progress := withHostProgress(ctx)
	if err := fn(hctx, h); err != nil {
		return &hostOpError{host: hostconn.HostID(id), err: err, progress: progress}
	}
	return nil
}

// withOwnedDomain is the routing + ownership gate of a routed per-VM call
// after Create (ADR-0007 Addendum A, slices 2 and 3): it leases vm.HostID's
// connection (withHostConn), checks the owner stamp of domain vm.ID against
// vm.Owner BEFORE anything else is read or changed (ownedDomainTarget, which
// fails closed and answers NotFound for a domain this VM does not own), and
// runs fn with the leased connection and the checked domain, addressed by its
// UUID. op names the call for the refusal message and the operator log.
func (p *Provider) withOwnedDomain(ctx context.Context, vm contracts.VMRef, op string, fn func(ctx context.Context, c libvirtConn, d domainTarget) error) error {
	return p.withHostConn(ctx, vm.HostID, func(hctx context.Context, c libvirtConn) error {
		vp, err := virshOf(c)
		if err != nil {
			return err
		}
		d, err := ownedDomainTarget(hctx, vp, c.HostID(), vm.ID, vm.Owner, op)
		if err != nil {
			return err
		}
		return fn(hctx, c, d)
	})
}

// routedHostUnavailableError is the host-scoped unavailability of the very
// host a routed call leases (withHostConn: an unknown, removed or draining
// host, or one whose connection cannot be resolved). It is transparent — Error
// and Unwrap are pe's, so contracts.IsHostUnavailable and every message are
// unchanged — and only tells routedRPCError which host to name in the
// answer's ErrorInfo (hostUnavailableStatusFor).
type routedHostUnavailableError struct {
	host hostconn.HostID
	pe   *contracts.ProviderError
}

// Error returns pe's message.
func (e *routedHostUnavailableError) Error() string { return e.pe.Error() }

// Unwrap exposes pe.
func (e *routedHostUnavailableError) Unwrap() error { return e.pe }

// hostOpError marks an error as having arisen while a routed call ran on a
// leased host — i.e. AFTER the provider itself resolved and leased the host,
// so the failure is scoped to that host or to the one VM on it, not to the
// provider (see routedRPCError). It is transparent: Error is the wrapped
// error's text and Unwrap exposes it, so errors.As/Is and every message are
// unchanged.
type hostOpError struct {
	host hostconn.HostID
	err  error
	// progress records how far the call got on the host before it failed
	// (host_progress.go). nil when the error was not made by a tracked call;
	// hostOpRPCError then assumes the worst (a command that changes the host
	// may have run).
	progress *hostProgress
}

// Error returns the wrapped error's message, unchanged.
func (e *hostOpError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped error.
func (e *hostOpError) Unwrap() error { return e.err }

// hostDownStderrMarkers are virsh error texts that mean the host's libvirtd
// itself cannot be reached, whatever the command was.
var hostDownStderrMarkers = []string{"failed to connect to the hypervisor"}

// isHostTransportFailure reports whether err — from a call run on a leased
// host — means the HOST could not be reached, rather than that the host ran a
// command and rejected it:
//
//   - a *VirshError with no exit status (ExitCode < 0): the command never
//     completed on the host — the SSH dial, handshake (host key, credentials)
//     or session failed, or the connection dropped mid-command. This also
//     covers an already-dialed connection to a host that has since died (the
//     registry reuses it without a health probe);
//   - a virsh error saying it cannot connect to the host's libvirtd;
//   - a wait on the host — a command slot, the SSH dial and handshake, an SSH
//     session — that ran out before the call's deadline
//     (hostNotAnsweringError, host_answer.go).
//
// A cancelled or expired call context is not a host failure.
func isHostTransportFailure(err error) bool {
	// A wait on the host that ran out before the call's deadline
	// (host_answer.go): the host did not answer in the call's time.
	if isHostNotAnswering(err) {
		return true
	}
	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ve *VirshError
	if !stderrors.As(err, &ve) {
		return false
	}
	if ve.ExitCode < 0 {
		return true
	}
	stderr := strings.ToLower(ve.Stderr)
	for _, m := range hostDownStderrMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// hostLease is the libvirtConn a routed core runs on: the host's *virshConn,
// reference-counted over the registry lease it was borrowed with. The lease is
// released when the last reference goes — the base reference withHostConn
// holds, plus any taken by retain for work that outlives the call. Close is a
// no-op: a core never closes the shared connection.
type hostLease struct {
	*virshConn
	lease hostconn.Conn
	refs  atomic.Int32
}

// Compile-time proof a hostLease is the libvirtConn a routed core takes.
var _ libvirtConn = (*hostLease)(nil)

// newHostLease wraps vc, borrowed through lease, with one reference held.
func newHostLease(vc *virshConn, lease hostconn.Conn) *hostLease {
	h := &hostLease{virshConn: vc, lease: lease}
	h.refs.Store(1)
	return h
}

// retain takes one more reference on the lease and returns its (idempotent)
// release. It must be called while the caller still holds a reference.
func (h *hostLease) retain() func() {
	h.refs.Add(1)
	var once sync.Once
	return func() { once.Do(h.release) }
}

// release drops one reference; the last one returns the registry lease.
func (h *hostLease) release() {
	if h.refs.Add(-1) == 0 {
		_ = h.lease.Close()
	}
}

// Close deliberately does nothing: the shared host connection belongs to the
// registry, and the lease's lifetime is the reference count above.
func (h *hostLease) Close() error { return nil }

// Unwrap returns the underlying *virshConn (see virshConnFrom).
func (h *hostLease) Unwrap() hostconn.Conn { return h.virshConn }

// retainConn keeps c usable past the call that received it and returns the
// matching release. For a routed hostLease that is a real reference on the
// registry lease; the single-host connection lives as long as the provider, so
// there is nothing to hold.
func retainConn(c libvirtConn) func() {
	if h, ok := c.(interface{ retain() func() }); ok {
		return h.retain()
	}
	return func() {}
}

// singleHostConn is the libvirtConn view of the single-host provider's one
// connection, p.virshProvider — the handle every single-host call has always
// run on. It is created per call, never registered and never closed (closing
// it would tear down the provider's persistent SSH client).
func (p *Provider) singleHostConn() libvirtConn {
	return newVirshConn(p.hostID, p.virshProvider)
}

// virshOf narrows a connection to the VirshProvider of its host, which the
// virsh-based cores still drive. ADR-0008 PR 5 moves those cores onto the
// connection itself; their signatures already take the connection, so the
// routing does not change.
func virshOf(c libvirtConn) (*VirshProvider, error) {
	vc, err := virshConnFrom(c)
	if err != nil {
		return nil, err
	}
	if vc.virsh == nil {
		return nil, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	return vc.virsh, nil
}

// Routed wire errors are SANITIZED (security review of slice 3): they land in
// VirtualMachine, VMSnapshot, VMClone and VMMigration conditions and events,
// readable by tenants, so they carry only a categorized message — the
// operation and the host id — never a cause's text (an SSH dial address,
// virsh or qemu-img stderr, a host path). The cause is logged by the provider,
// for the operator. The single-host path never goes through these and keeps
// its historical errors.

// hostUnavailableStatus renders a host-scoped unavailability as
// codes.Unavailable carrying a google.rpc.ErrorInfo{Reason: HOST_UNAVAILABLE},
// which the manager maps to contracts.ErrorTypeHostUnavailable and keeps out of
// its circuit breaker. Only pe.Message (which names the host id) crosses the
// wire; the cause is logged.
func hostUnavailableStatus(pe *contracts.ProviderError) error {
	return hostUnavailableStatusFor(pe, "")
}

// hostUnavailableStatusFor is hostUnavailableStatus for an unavailability of
// the host the call was routed to, host (its target_host_id): the ErrorInfo
// names it under contracts.HostUnavailableHostKey, so the manager can say in
// the VM's status that its own host could not be reached. An empty host sets
// no metadata (the host that could not be reached may be another one).
func hostUnavailableStatusFor(pe *contracts.ProviderError, host hostconn.HostID) error {
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

// vmOperationStatus renders a failed per-VM operation that reached its host as
// code (codes.Unknown for a failure, codes.Unavailable for one the caller
// should retry, e.g. the same copy still running) with the categorized message
// msg, plus a google.rpc.ErrorInfo{Reason: VM_OPERATION_FAILED}, which the
// manager keeps out of its circuit breaker whatever the code (ADR-0007
// Addendum A, slice 2).
func vmOperationStatus(code codes.Code, msg string) error {
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

// outcomeUnknownRetryStatus renders a call that may have changed its host, whose
// outcome is unknown and that is safe to repeat: codes.Unavailable with a
// google.rpc.ErrorInfo{Reason: OUTCOME_UNKNOWN_RETRY} that the manager maps to a
// retryable error that is NOT "nothing done" (contracts.IsOutcomeUnknownRetry),
// plus VM_OPERATION_FAILED. The second reason is for a manager that predates
// the first: it maps the answer as it always did (retryable, out of its circuit
// breaker, and every consumer of these calls retries a retryable answer
// first), never as a definite failure.
func outcomeUnknownRetryStatus(msg string, extra ...string) error {
	return statusWithReasons(codes.Unavailable, msg, append([]string{contracts.OutcomeUnknownRetryReason, contracts.VMOperationFailedReason}, extra...)...).Err()
}

// hostLostStatus renders the answer of a routed call whose host stopped
// answering part-way: code and msg as hostOpRPCErrorFor chose them, with
// VM_OPERATION_FAILED and HOST_CONNECTION_LOST, the reason the manager paces
// its retries by (contracts.IsHostConnectionLost).
func hostLostStatus(code codes.Code, msg string) error {
	return statusWithReasons(code, msg, contracts.VMOperationFailedReason, contracts.HostConnectionLostReason).Err()
}

// routedOpError is a failure of a routed operation that carries its own
// categorized, tenant-safe wire message and code: a time budget that ran out,
// a copy of the same disk already running, a host tool missing
// (routed_budget.go). routedRPCError puts wire (never the cause) on the wire.
type routedOpError struct {
	// code is the wire code: codes.Unknown (the operation failed) or
	// codes.Unavailable (retry: the same operation is still in progress).
	code codes.Code
	// wire is the categorized message sent to the manager.
	wire string
	// cause is logged, never sent.
	cause error
	// outcomeUnknown marks a codes.Unavailable answer for a call that may have
	// changed the host and whose outcome is unknown (the routed budget ran out
	// while the host's job may still complete; the request's snapshot is still
	// being taken): it is sent with OutcomeUnknownRetryReason, so that the
	// manager retries it but never reads it as "nothing was done" (R25).
	outcomeUnknown bool
}

// Error returns the wire message and the cause, for the provider's log.
func (e *routedOpError) Error() string {
	if e.cause != nil {
		return e.wire + ": " + e.cause.Error()
	}
	return e.wire
}

// Unwrap exposes the cause.
func (e *routedOpError) Unwrap() error { return e.cause }

// hostOpKind says how a routed RPC answers a host that stops answering
// (hostOpRPCErrorFor).
type hostOpKind int

const (
	// hostOpWrite is an RPC that changes the host. A transport failure is
	// HOST_UNAVAILABLE only while no command of the call was answered and none
	// that may change the host was sent, "not performed" while only reads ran,
	// and an unknown outcome (codes.Unknown) once a command that may change the
	// host was sent.
	hostOpWrite hostOpKind = iota
	// hostOpRead is an RPC that only reads (Describe, GetDiskInfo, TaskStatus):
	// it cannot half-do anything, so a host that stops answering at any point
	// is HOST_UNAVAILABLE.
	hostOpRead
	// hostOpWriteRetry is a hostOpWrite whose unknown outcome is answered
	// "retry" (codes.Unavailable with VM_OPERATION_FAILED, the text saying the
	// outcome is unknown) instead of codes.Unknown: the RPC is idempotent per
	// request and the manager retries it on the same host — a SnapshotCreate
	// (adopted by request token) and a Clone (stamped, and its copy under
	// flock). A codes.Unknown would fail the clone and remove its target.
	hostOpWriteRetry
)

// Wire texts of a transport failure of a routed write RPC. They name the
// operation and the host only.
const (
	wireHostNotPerformedFmt = "failed to %s on host %q: the host stopped answering before the operation changed " +
		"anything; nothing was done"
	wireHostUnknownFmt = "failed to %s on host %q: the host stopped answering while the operation ran; " +
		"its outcome is unknown"
	wireHostUnknownRetryFmt = "failed to %s on host %q: the host stopped answering while the operation ran; " +
		"its outcome is unknown and it may still complete on the host: retry"
)

// hostOpRPCError is hostOpRPCErrorFor for an RPC that changes the host.
func hostOpRPCError(op string, ho *hostOpError, err error) error {
	return hostOpRPCErrorFor(hostOpWrite, op, ho, err)
}

// hostOpRPCErrorFor is the wire form of an error that arose on a leased host
// (hostOpError) and is not one of the categorized classes. A failure of the
// operation on that one VM (the host answered and the operation failed) is
// VM_OPERATION_FAILED, codes.Unknown. A host that stopped answering
// (isHostTransportFailure) is answered by how far the call got on it
// (hostProgress, R25), because the manager reads HOST_UNAVAILABLE as definite
// ("nothing was done"):
//
//   - no command of the call was answered and none that may change the host
//     was sent (the lease, the dial, or the very first command found the
//     connection dead), or the RPC only reads (kind hostOpRead):
//     HOST_UNAVAILABLE;
//   - only reads ran: "not performed" (codes.Unavailable, VM_OPERATION_FAILED);
//   - a command that may change the host was sent (it may have completed, or
//     still be running): an unknown outcome — codes.Unknown with
//     VM_OPERATION_FAILED, or, for kind hostOpWriteRetry, the "retry" form of
//     it.
//
// None counts toward the manager's per-Provider circuit breaker. Only
// "<op> ... host <id>" crosses the wire — for a failure of the operation
// itself followed by the provider's own requester-facing account of it when it
// wrote one (requesterFacingMessage) — and the error is logged.
func hostOpRPCErrorFor(kind hostOpKind, op string, ho *hostOpError, err error) error {
	log.Printf("WARN %s on host %s failed: %v", op, ho.host, err)
	if isHostTransportFailure(err) {
		stage := ho.progress.stage()
		// The dependency guard runs before anything is changed and says so
		// (guardHostUnreachableError: "nothing was changed"): its host failure
		// is the fail-closed HOST_UNAVAILABLE whatever the call read before.
		var gh *guardHostUnreachableError
		switch {
		case kind == hostOpRead || stderrors.As(err, &gh) || stage == hostDownNothingRan:
			return hostUnavailableStatusFor(contracts.NewHostUnavailableError(
				fmt.Sprintf("%s: host %q is unreachable", op, ho.host), nil), ho.host)
		case stage == hostDownReadsOnly:
			return hostLostStatus(codes.Unavailable, fmt.Sprintf(wireHostNotPerformedFmt, op, ho.host))
		case kind == hostOpWriteRetry:
			log.Printf("WARN %s on host %s: outcome unknown (a command that may change the host was sent)", op, ho.host)
			return outcomeUnknownRetryStatus(fmt.Sprintf(wireHostUnknownRetryFmt, op, ho.host), contracts.HostConnectionLostReason)
		default:
			log.Printf("WARN %s on host %s: outcome unknown (a command that may change the host was sent)", op, ho.host)
			return hostLostStatus(codes.Unknown, fmt.Sprintf(wireHostUnknownFmt, op, ho.host))
		}
	}
	msg := fmt.Sprintf("failed to %s on host %q", op, ho.host)
	if detail := requesterFacingMessage(err); detail != "" {
		msg += ": " + detail
	}
	return vmOperationStatus(codes.Unknown, msg)
}

// requesterFacingMessage returns the message of the outermost provider error
// in err's chain when the provider composed it for the requester: a
// ProviderError with no cause, or whose cause is kept out of its text
// (providerLogOnly, #357's honest Reconfigure) — "could not grow the VM's disk
// to 20 GiB", "the VM is \"paused\" (active, but not running); ... nothing was
// changed". Such a message names the outcome, never raw host output. A
// provider error that carries any other cause may quote virsh or qemu-img
// output (disk paths, the connection URI): "" is returned and only the
// operation and host reach the wire.
func requesterFacingMessage(err error) string {
	var pe *contracts.ProviderError
	if !stderrors.As(err, &pe) {
		return ""
	}
	if pe.Cause == nil {
		return pe.Message
	}
	if _, ok := pe.Cause.(*providerLogOnly); ok {
		return pe.Message
	}
	return ""
}

// routedRPCError converts an error from a ROUTED call on a clustered provider
// to its gRPC status (ADR-0007 Addendum A):
//
//   - InvalidSpec -> InvalidArgument (no target host, an unsupported power
//     operation);
//   - HostUnavailable -> Unavailable with a HOST_UNAVAILABLE ErrorInfo (an
//     unknown, draining or unreachable host; retryable, host-scoped);
//   - NotFound -> NotFound (a delete, power operation, reconfigure, snapshot,
//     clone, export or disk read of a domain this VM does not own, reported as
//     absent and never touched; a task reference naming a host this provider
//     does not front);
//   - Conflict -> AlreadyExists (a clone whose target domain name is taken on
//     the host by a domain the target VirtualMachine does not own);
//   - a routedOpError (a time budget that ran out, the same copy in progress,
//     a host tool missing) -> its own code and categorized message with a
//     VM_OPERATION_FAILED ErrorInfo;
//   - any other failure that arose on the leased host (hostOpError): a host
//     that stopped answering (isHostTransportFailure) is answered by how far
//     the call got (hostOpRPCErrorFor), and anything else is
//     VM_OPERATION_FAILED (codes.Unknown). None is
//     counted by the manager's circuit breaker, so neither a dead host nor one
//     tenant's failing VM can open it for the whole Provider;
//   - a provider-level Unavailable (the provider itself is not ready, e.g. its
//     host registry is not initialized) -> a plain Unavailable, which the
//     breaker DOES count; anything else is a plain "failed to <op>".
//
// A refusal by the disk dependency guard (diskDependentsError: another domain
// on the host uses this VM's disk, e.g. its linked clone) is FailedPrecondition
// with the VM_DISK_IN_USE and VM_OPERATION_FAILED ErrorInfos: a per-VM answer
// that the manager maps to a Conflict and never counts toward its breaker. A
// guard that could not run (diskCheckFailedError) is Unavailable with the
// VM_DISK_CHECK_FAILED and VM_OPERATION_FAILED ErrorInfos: retried, not
// counted. A guard that could not reach the host (guardHostUnreachableError)
// is a host failure like any other: HOST_UNAVAILABLE (hostOpRPCError). A clone
// refused because its source is not shut off (sourceRunningError) is
// FailedPrecondition with the VM_SOURCE_RUNNING and VM_OPERATION_FAILED
// ErrorInfos: the manager keeps the clone pending, never counts it toward its
// breaker. A copy whose source chain is not safe to copy (copyRefusedError)
// is FailedPrecondition with VM_OPERATION_FAILED and a message that names no
// host path: terminal, never counted.
//
// The cluster-wide guards of ADR-0007 A6 (slice A6.1) answer first
// (clusterGuardStatus): a Clone that finds a previous incarnation of its
// target VirtualMachine is AlreadyExists + VM_PREVIOUS_INCARNATION, and a
// Clone or Delete whose disk guard could not check every host of the Provider
// is Unavailable + HOST_UNAVAILABLE (a host could not be reached) or +
// VM_DISK_CHECK_FAILED and VM_OPERATION_FAILED (a host could not be scanned);
// none of them names another host. A Delete refused because a domain on
// another host uses its disk is the disk-dependents refusal below.
//
// A host that stops answering (isHostTransportFailure) is answered by how far
// the call got on it, not always HOST_UNAVAILABLE (R25): see hostOpRPCErrorFor.
// routedRPCError is for an RPC that changes the host; routedReadRPCError for
// one that only reads, and routedIdempotentRPCError for one the manager retries
// by design (SnapshotCreate, Clone).
//
// Only the categorized message crosses the wire; causes are logged. It is
// never used on the single-host path, whose wire errors are unchanged.
func routedRPCError(op string, err error) error {
	return routedRPCErrorFor(hostOpWrite, op, err)
}

// routedReadRPCError is routedRPCError for an RPC that only reads (Describe,
// GetDiskInfo, TaskStatus, ListVMs): a host that stops answering is always
// HOST_UNAVAILABLE, because a read cannot half-do anything.
func routedReadRPCError(op string, err error) error {
	return routedRPCErrorFor(hostOpRead, op, err)
}

// routedIdempotentRPCError is routedRPCError for an RPC that is idempotent per
// request and that the manager retries on the same host (SnapshotCreate,
// Clone): its unknown outcome is answered "retry" (hostOpWriteRetry).
func routedIdempotentRPCError(op string, err error) error {
	return routedRPCErrorFor(hostOpWriteRetry, op, err)
}

// routedRPCErrorFor is routedRPCError for an RPC of kind.
func routedRPCErrorFor(kind hostOpKind, op string, err error) error {
	if st := clusterGuardStatus(err); st != nil {
		return st.Err()
	}
	var de *diskDependentsError
	if stderrors.As(err, &de) {
		return diskInUseStatus(fmt.Sprintf("failed to %s: %v", op, de), true).Err()
	}
	var dc *diskCheckFailedError
	if stderrors.As(err, &dc) {
		return diskCheckFailedStatus(fmt.Sprintf("failed to %s: %v", op, dc), true).Err()
	}
	var sr *sourceRunningError
	if stderrors.As(err, &sr) {
		return sourceRunningStatus(fmt.Sprintf("failed to %s: %v", op, sr), true).Err()
	}
	// A revert refused because the snapshot records a read-only disk as
	// external, or one that libvirt made but that rewrote a read-only disk
	// (snapshot_readonly_disks.go): a definite, terminal answer.
	var ru *snapshotRevertUnsafeError
	if stderrors.As(err, &ru) {
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf("failed to %s: %s", op, ru.Error()))
	}
	var rb *snapshotRevertBrokeDisksError
	if stderrors.As(err, &rb) {
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf("failed to %s: %s", op, rb.Error()))
	}
	var rm *snapshotRevertMediaError
	if stderrors.As(err, &rm) {
		log.Printf("WARN %s: %v: %v", op, rm, rm.cause)
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf("failed to %s: %s", op, rm.Error()))
	}
	var cr *copyRefusedError
	if stderrors.As(err, &cr) {
		log.Printf("WARN %s refused: %v", op, cr)
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf("failed to %s: %s", op, cr.Error()))
	}
	var roe *routedOpError
	if stderrors.As(err, &roe) {
		log.Printf("WARN %s failed: %v", op, err)
		if roe.outcomeUnknown {
			return outcomeUnknownRetryStatus(roe.wire)
		}
		return vmOperationStatus(roe.code, roe.wire)
	}
	var rh *routedHostUnavailableError
	if stderrors.As(err, &rh) {
		return hostUnavailableStatusFor(rh.pe, rh.host)
	}
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		switch pe.Type {
		case contracts.ErrorTypeInvalidSpec:
			return status.Error(codes.InvalidArgument, pe.Message)
		case contracts.ErrorTypeHostUnavailable:
			return hostUnavailableStatus(pe)
		case contracts.ErrorTypeNotFound:
			return status.Error(codes.NotFound, pe.Message)
		case contracts.ErrorTypeConflict:
			return status.Error(codes.AlreadyExists, pe.Message)
		}
	}
	var ho *hostOpError
	if stderrors.As(err, &ho) {
		return hostOpRPCErrorFor(kind, op, ho, err)
	}
	log.Printf("WARN %s failed: %v", op, err)
	if pe != nil && pe.Type == contracts.ErrorTypeUnavailable {
		return status.Error(codes.Unavailable, pe.Message)
	}
	return status.Errorf(codes.Unknown, "failed to %s", op)
}
