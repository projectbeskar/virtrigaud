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

	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Outcomes of a failed SnapshotRevert (single-host and routed).
//
// The manager sends a revert only while the VM is powered off, and decides
// what to do with a failure by what it proves (internal/controller,
// virtualmachine_snapshot_revert.go): "not performed" (codes.Unavailable with
// VM_OPERATION_FAILED, contracts.IsNothingDone) is sent again after a backoff
// and never counted as an unknown outcome; an unknown outcome (codes.Unavailable
// with VM_OUTCOME_UNKNOWN_RETRY, or a host connection lost after a command
// that changes the host was sent) is sent again a bounded number of times; a
// refusal or a failure is terminal. snapshotRevertOn tags every failure with
// the stage it happened in, and the failure is answered by what the code
// proves:
//
//   - before `virsh snapshot-revert` ran (stageBefore: the existence and
//     state reads, the disk-dependents guard, reading and gating the
//     snapshot's definition): nothing was changed — "not performed". A typed
//     refusal (the revert gate's FailedPrecondition, VM_DISK_IN_USE,
//     VM_DISK_CHECK_FAILED, a host that could not be reached) keeps its
//     answer; a snapshot or domain that does not exist is NotFound.
//   - the revert command (stageCommand) ended on its own with libvirt's
//     refusal before it executes anything, and the domain's disk sources, read
//     again, are as before: a busy domain or one in the wrong state is "not
//     performed"; a revert libvirt does not support for this VM
//     (snapshotConfigRefusalMarkers) is a terminal FailedPrecondition. A
//     snapshot libvirt does not find is NotFound.
//   - the revert command ended on its own with any other error: libvirt
//     reported a failure while it reverted, and the disks may have been
//     changed — a terminal failure (codes.Unknown with VM_OPERATION_FAILED).
//   - the revert command did not end on its own (a dropped connection, a
//     signal, an expired request) or virsh lost libvirtd, a refusal whose
//     disk sources changed or cannot be read again, or a repair of an earlier
//     interrupted ejection that could not put the media back
//     (errMediaNotRestored): the outcome is unknown — retry (routed: a host
//     that stopped answering keeps the answer by how far the call got,
//     hostOpRPCErrorFor).
//
// Wire texts are constant and name no host path or command; the cause is
// logged here.

// Constant wire texts of a failed SnapshotRevert.
const (
	wireRevertNotPerformed = "snapshot revert not performed: a check or command on the libvirt host failed before " +
		"the revert ran; nothing was changed (details are in the provider log)"
	wireRevertNotRun = "snapshot revert not performed: libvirt refused the revert before running it (the VM is busy " +
		"or not in a state it can be reverted in); nothing was changed (details are in the provider log)"
	wireRevertRefusedForConfig = "snapshot revert refused: libvirt does not support reverting this VM to this " +
		"snapshot; nothing was changed (details are in the provider log)"
	wireRevertFailed = "snapshot revert failed: libvirt reported an error while reverting; the VM's disks may " +
		"have been changed (details are in the provider log)"
	wireRevertOutcomeUnknown = "snapshot revert outcome unknown: the libvirt host stopped answering while the " +
		"revert ran, and it may still complete: retry"
	wireRevertSnapshotNotFound = "the snapshot was not found on the libvirt domain; nothing was changed"
	// wireRevertOnHostFmt prefixes a routed revert's answer with its host.
	wireRevertOnHostFmt = "on host %q: %s"
)

// revertSnapshotNotFoundMarker is libvirt's error class for a snapshot it
// does not find (VIR_ERR_NO_DOMAIN_SNAPSHOT), lower case.
const revertSnapshotNotFoundMarker = "domain snapshot not found"

// revertOutcome is what a failed SnapshotRevert proves (see the file comment).
type revertOutcome int

const (
	// revertAsIs: the failure is answered as it is (a typed answer of its own,
	// or the revert's own post-revert checks).
	revertAsIs revertOutcome = iota
	// revertNotPerformed: nothing was changed; sending it again may succeed.
	revertNotPerformed
	// revertNotRun: libvirt refused it before running it; nothing changed.
	revertNotRun
	// revertRefused: libvirt does not support this revert for the VM.
	revertRefused
	// revertFailed: libvirt reported a failure while it reverted.
	revertFailed
	// revertUnknown: the revert may have run, or may still run.
	revertUnknown
	// revertSnapshotNotFound: the snapshot does not exist on the domain.
	revertSnapshotNotFound
	// revertDomainNotFound: the domain does not exist.
	revertDomainNotFound
)

// diskSourcesKey renders the domain's disks as one comparable string: each
// disk's target and source, in document order.
func (d *domainDisksDoc) diskSourcesKey() string {
	var b strings.Builder
	for _, e := range d.Devices.Disks {
		fmt.Fprintf(&b, "%s=%s;", e.target(), e.sourceRef())
	}
	return b.String()
}

// classifyRevertFailure tells what a failed snapshotRevertOn of domain on
// conn proves (see the file comment).
func classifyRevertFailure(ctx context.Context, conn libvirtConn, domain string, err error) revertOutcome {
	if stderrors.Is(err, errMediaNotRestored) {
		return revertUnknown
	}
	stage, _, ok := stageOf(err)
	if !ok {
		return revertAsIs
	}
	switch stage {
	case stageBefore:
		if virshNoDomain(err) {
			return revertDomainNotFound
		}
		return revertNotPerformed
	case stageCommand:
		if !hostCommandEnded(ctx, err) || virshConnectionLost(err) {
			return revertUnknown
		}
		if virshNoDomain(err) {
			return revertDomainNotFound
		}
		if firstErrorLineHasAny(err, revertSnapshotNotFoundMarker) {
			return revertSnapshotNotFound
		}
		if !firstErrorLineHasAny(err, snapshotRefusalMarkers...) && !snapshotRefusedForConfig(err) {
			return revertFailed
		}
		// A refusal text: proven only when the domain's disks are still the
		// ones it had before the revert.
		after, readErr := readDomainDisks(ctx, hostConnRunner{conn: conn}, domain)
		if readErr != nil || after.diskSourcesKey() != stageDetailOf(err) {
			log.Printf("WARN libvirt snapshot revert outcome unknown: the domain's disk sources are not readable or "+
				"changed (read error: %v) after: %v", readErr, err)
			return revertUnknown
		}
		if snapshotRefusedForConfig(err) {
			return revertRefused
		}
		return revertNotRun
	}
	return revertAsIs
}

// singleHostRevertFailure answers a failed single-host SnapshotRevert with
// the outcome the provider can prove (classifyRevertFailure).
func singleHostRevertFailure(ctx context.Context, conn libvirtConn, domain string, err error) error {
	switch classifyRevertFailure(ctx, conn, domain, err) {
	case revertNotPerformed:
		return nothingDone(wireRevertNotPerformed, err)
	case revertNotRun:
		return notPerformed(wireRevertNotRun, err)
	case revertRefused:
		log.Printf("ERROR libvirt snapshot revert refused: %v", err)
		return vmOperationStatus(codes.FailedPrecondition, wireRevertRefusedForConfig)
	case revertFailed:
		log.Printf("ERROR libvirt snapshot revert failed: %v", err)
		return vmOperationStatus(codes.Unknown, wireRevertFailed)
	case revertUnknown:
		log.Printf("WARN libvirt snapshot revert outcome unknown: %v", err)
		return outcomeUnknownRetryStatus(wireRevertOutcomeUnknown)
	case revertSnapshotNotFound:
		return refusal(codes.NotFound, wireRevertSnapshotNotFound, err)
	case revertDomainNotFound:
		return refusal(codes.NotFound, wireDomainNotFound, err)
	}
	if typed := definiteStatusError(err); typed != nil {
		return typed
	}
	return err
}

// routedRevertFailure is singleHostRevertFailure for a routed revert on host:
// it returns the error routedRPCError renders. A host that stopped answering
// (isHostTransportFailure) and a typed answer are returned unchanged, so the
// routed classification by how far the call got (hostOpRPCErrorFor) and the
// typed refusals keep their answers.
func routedRevertFailure(ctx context.Context, conn libvirtConn, domain string, err error) error {
	if isHostTransportFailure(err) || definiteStatusError(err) != nil {
		return err
	}
	host := conn.HostID()
	switch classifyRevertFailure(ctx, conn, domain, err) {
	case revertNotPerformed:
		return &routedOpError{code: codes.Unavailable, cause: err, wire: fmt.Sprintf(
			wireRevertOnHostFmt, host, wireRevertNotPerformed)}
	case revertNotRun:
		return &routedOpError{code: codes.Unavailable, cause: err, wire: fmt.Sprintf(
			wireRevertOnHostFmt, host, wireRevertNotRun)}
	case revertRefused:
		return &routedOpError{code: codes.FailedPrecondition, cause: err, wire: fmt.Sprintf(
			wireRevertOnHostFmt, host, wireRevertRefusedForConfig)}
	case revertUnknown:
		return &routedOpError{code: codes.Unavailable, cause: err, outcomeUnknown: true, wire: fmt.Sprintf(
			wireRevertOnHostFmt, host, wireRevertOutcomeUnknown)}
	case revertSnapshotNotFound:
		log.Printf("WARN revert to snapshot on host %s: %v", host, err)
		return contracts.NewNotFoundError(wireRevertSnapshotNotFound, nil)
	case revertDomainNotFound:
		log.Printf("WARN revert to snapshot on host %s: %v", host, err)
		return contracts.NewNotFoundError(wireDomainNotFound, nil)
	}
	// revertFailed and revertAsIs: the operation failed on the host
	// (VM_OPERATION_FAILED, codes.Unknown), as before.
	return err
}
