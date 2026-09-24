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
// Slice 1 routes Describe and Delete; slice 2 routes Power and Reconfigure.
// Every routed call except Create checks the domain's owner stamp before it
// reads or changes anything. The other per-VM RPCs are refused with an honest
// Unimplemented until their slice lands (see notRoutedYet), and the clustered
// GetCapabilities hides them.

// The ADR-0007 delivery step (Addendum A, A5 slice, or main-ADR phase) that
// routes each RPC a clustered provider refuses today. They only appear in the
// refusal message.
const (
	sliceRoutedSnapshotCloneDisk = "Addendum A slice 3"
	sliceRoutedListVMs           = "Addendum A slice 4"
	// sliceRoutedImport: import is not per-VM; routing it into a clustered
	// provider needs a target-host design (phase P3).
	sliceRoutedImport = "phase P3"
)

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
func (p *Provider) withHostConn(ctx context.Context, hostID string, fn func(libvirtConn) error) error {
	id := strings.TrimSpace(hostID)
	if id == "" {
		return contracts.NewInvalidSpecError(emptyTargetHostMessage, nil)
	}
	if p.clusterReg == nil {
		return contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil)
	}
	lease, err := p.clusterReg.ConnFor(ctx, hostconn.HostID(id))
	if err != nil {
		return contracts.NewHostUnavailableError(fmt.Sprintf("connect to host %q", id), err)
	}
	vc, err := virshConnFrom(lease)
	if err != nil {
		_ = lease.Close()
		return contracts.NewHostUnavailableError(fmt.Sprintf("resolve connection of host %q", id), err)
	}
	h := newHostLease(vc, lease)
	defer h.release()
	if err := fn(h); err != nil {
		return &hostOpError{host: hostconn.HostID(id), err: err}
	}
	return nil
}

// hostOpError marks an error as having arisen while a routed call ran on a
// leased host — i.e. AFTER the provider itself resolved and leased the host,
// so the failure is scoped to that host or to the one VM on it, not to the
// provider (see routedRPCError). It is transparent: Error is the wrapped
// error's text and Unwrap exposes it, so errors.As/Is and every message are
// unchanged.
type hostOpError struct {
	host hostconn.HostID
	err  error
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
//   - a virsh error saying it cannot connect to the host's libvirtd.
//
// A cancelled or expired call context is not a host failure.
func isHostTransportFailure(err error) bool {
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

// notRoutedYet is the honest refusal of a per-VM (or host-scoped) RPC that a
// clustered provider does not route to a host yet: codes.Unimplemented, which
// the manager maps to a non-retryable NotSupported and backs off on. It never
// reaches any host — least of all the unroutable placeholder.
func notRoutedYet(rpc, routedIn string) error {
	return status.Errorf(codes.Unimplemented,
		"%s is not yet routed to a host on a clustered libvirt provider (ADR-0007 %s); "+
			"topology: cluster is experimental", rpc, routedIn)
}

// hostUnavailableStatus renders a host-scoped unavailability as
// codes.Unavailable carrying a google.rpc.ErrorInfo{Reason: HOST_UNAVAILABLE},
// which the manager maps to contracts.ErrorTypeHostUnavailable and keeps out of
// its circuit breaker. The message names the host only (never credentials).
func hostUnavailableStatus(pe *contracts.ProviderError) error {
	st := status.New(codes.Unavailable, pe.Error())
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.HostUnavailableReason,
		Domain: contracts.HostUnavailableErrorDomain,
	})
	if err != nil {
		// Unreachable in practice (ErrorInfo always marshals); a plain
		// Unavailable is still a correct, if breaker-counted, answer.
		return st.Err()
	}
	return withInfo.Err()
}

// vmOperationFailedStatus renders a failed per-VM operation that reached its
// host as codes.Unknown with message msg — exactly the code and message the
// historical wrapped error produced — plus a google.rpc.ErrorInfo{Reason:
// VM_OPERATION_FAILED}, which the manager keeps out of its circuit breaker
// (ADR-0007 Addendum A, slice 2).
func vmOperationFailedStatus(msg string) error {
	st := status.New(codes.Unknown, msg)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.VMOperationFailedReason,
		Domain: contracts.ErrorInfoDomain,
	})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// hostOpRPCError is the wire form of an error that arose on a leased host
// (hostOpError) and is not one of the categorized classes: a host that could
// not be reached is a host-scoped Unavailable (HOST_UNAVAILABLE); anything
// else is a failure of the operation on that one VM (VM_OPERATION_FAILED,
// historical code and message). Neither counts toward the manager's
// per-Provider circuit breaker.
func hostOpRPCError(op string, ho *hostOpError, err error) error {
	if isHostTransportFailure(err) {
		return hostUnavailableStatus(contracts.NewHostUnavailableError(
			fmt.Sprintf("%s: host %q is unreachable", op, ho.host), err))
	}
	return vmOperationFailedStatus(fmt.Sprintf("failed to %s: %v", op, err))
}

// routedRPCError converts an error from a ROUTED call on a clustered provider
// to its gRPC status (ADR-0007 Addendum A):
//
//   - InvalidSpec -> InvalidArgument (no target host, an unsupported power
//     operation);
//   - HostUnavailable -> Unavailable with a HOST_UNAVAILABLE ErrorInfo (an
//     unknown, draining or unreachable host; retryable, host-scoped);
//   - NotFound -> NotFound (a delete, power operation or reconfigure of a
//     domain this VM does not own, reported as absent and never touched);
//   - any other failure that arose on the leased host (hostOpError): a host
//     that could not be reached (isHostTransportFailure) is HOST_UNAVAILABLE,
//     and anything else is VM_OPERATION_FAILED with the historical code and
//     message. Neither is counted by the manager's circuit breaker, so neither
//     a dead host nor one tenant's failing VM can open it for the whole
//     Provider;
//   - a provider-level Unavailable (the provider itself is not ready, e.g. its
//     host registry is not initialized) -> a plain Unavailable, which the
//     breaker DOES count; anything else keeps the historical wrapped form.
//
// Only the categorized message crosses the wire. It is never used on the
// single-host path, whose wire errors are unchanged.
func routedRPCError(op string, err error) error {
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		switch pe.Type {
		case contracts.ErrorTypeInvalidSpec:
			return status.Error(codes.InvalidArgument, pe.Message)
		case contracts.ErrorTypeHostUnavailable:
			return hostUnavailableStatus(pe)
		case contracts.ErrorTypeNotFound:
			return status.Error(codes.NotFound, pe.Message)
		}
	}
	var ho *hostOpError
	if stderrors.As(err, &ho) {
		return hostOpRPCError(op, ho, err)
	}
	if pe != nil && pe.Type == contracts.ErrorTypeUnavailable {
		return status.Error(codes.Unavailable, pe.Message)
	}
	return fmt.Errorf("failed to %s: %w", op, err)
}
