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
	"fmt"
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A libvirt full clone requires a powered-off source (ADR-0007 Slice 5 lab,
// B2; maintainer decision). The copy reads the source's disk chain with
// qemu-img convert, which cannot take the image lock a running QEMU holds
// ("Failed to get shared "write" lock"), and a copy of a disk a running guest
// is writing is not a consistent clone either — forcing it with -U would copy
// a crash-inconsistent disk. So, on both the single-host and the clustered
// path, the source's state is read before anything is copied, and a source
// that is not shut off is refused with FailedPrecondition and the
// VM_SOURCE_RUNNING ErrorInfo: nothing was copied, the refusal holds until the
// source is powered off, and it never counts toward the manager's circuit
// breaker. The manager keeps the VMClone Pending (SourceMustBePoweredOff) and
// retries; the clone proceeds once the source is off.

// knownDomainStates are the states `virsh domstate` prints; only these are
// quoted back to the requester.
var knownDomainStates = map[string]bool{
	domStateRunning: true, domStateIdle: true, domStateBlocked: true, domStateCrashed: true, domStatePaused: true,
	domStatePMSusp: true, domStateInShut: true, domStateNoState: true, domStateSuspended: true,
}

// sourceRunningError refuses a clone because its source domain is not shut
// off. Its message is safe for the requester: it names the state only.
type sourceRunningError struct {
	// state is the source's state as `virsh domstate` printed it.
	state string
}

// Error is the refusal, safe for the VMClone's status.
func (e *sourceRunningError) Error() string {
	state := strings.ToLower(strings.TrimSpace(e.state))
	if !knownDomainStates[state] {
		state = "not shut off"
	}
	return fmt.Sprintf("the clone's source VM is %q: power off the source VM to clone it "+
		"(a libvirt full clone copies the disk of a powered-off VM); nothing was copied", state)
}

// GRPCStatus renders the refusal as codes.FailedPrecondition carrying a
// google.rpc.ErrorInfo{Reason: VM_SOURCE_RUNNING}. status.FromError finds it
// through the single-host Clone handler's "failed to clone VM: %w" wrapping.
func (e *sourceRunningError) GRPCStatus() *status.Status {
	return sourceRunningStatus(e.Error(), false)
}

// sourceRunningStatus is the wire form of a clone refused because its source
// is not shut off, with message msg. routed adds the VM_OPERATION_FAILED
// ErrorInfo a clustered provider attaches to a per-VM answer from its host.
func sourceRunningStatus(msg string, routed bool) *status.Status {
	reasons := []string{contracts.VMSourceRunningReason}
	if routed {
		reasons = append(reasons, contracts.VMOperationFailedReason)
	}
	return statusWithReasons(codes.FailedPrecondition, msg, reasons...)
}

// cloneSourceStateError returns the refusal of a clone whose source is in
// state (as `virsh domstate` prints it), or nil when it is shut off.
func cloneSourceStateError(state string) error {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case domStateShutOff, domStateShutoff:
		return nil
	}
	return &sourceRunningError{state: state}
}

// refuseRunningCloneSource reads the state of the source domain handle (a
// name, or the owner-checked UUID of a clustered source) on vp's host and
// refuses the clone (sourceRunningError) unless it is shut off. A state that
// cannot be read is a retryable failure; nothing is copied either way.
func refuseRunningCloneSource(ctx context.Context, vp *VirshProvider, handle, name string) error {
	state, err := vp.getDomainState(ctx, handle)
	if err != nil {
		return contracts.NewRetryableError(fmt.Sprintf("could not read the power state of clone source %q", name), err)
	}
	if err := cloneSourceStateError(state); err != nil {
		log.Printf("WARN Refusing to clone %s: it is %q, and a libvirt full clone requires a powered-off source", name, state)
		return err
	}
	return nil
}
