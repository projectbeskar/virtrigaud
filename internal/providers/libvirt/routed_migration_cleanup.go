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
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Routed migration cleanup of a CLUSTERED provider (ADR-0007 Addendum B, B4;
// R11 slice 2).
//
// DeleteLandedDisk removes the disk a routed ImportDisk landed, on the host the
// import was sent to (DeleteLandedDiskRequest.target_host_id, the host the
// manager recorded before the import). It applies the single-host checks — the
// file derived from the target VM, the recorded path, a regular file — but its
// in-use check is A6.1's CROSS-HOST scan, not pathInUseOnHost: on a shared pool
// a domain on another host may use the file. A host that cannot be scanned
// fails the delete closed (retryable: HOST_UNAVAILABLE or VM_DISK_CHECK_FAILED),
// and the disk is kept. It runs under the routed import's landing lock, and its
// rm under the import's host lock (flock -n), so it never races an import of
// the same name in this provider, nor a host-side import command left over from
// a cancelled call.
//
// DeleteStagedDisk removes s3 objects from the provider pod, as on a single
// host. An nfs object is emptied by the qemu-img of the host the import was
// sent to (DeleteStagedDiskRequest.target_host_id), under timeout(1); an
// unknown, removed or draining host is HOST_UNAVAILABLE and the manager holds
// the cleanup.

const (
	// guardOpLandedDiskDelete names the routed landed-disk delete in the
	// cluster-wide guard's answers.
	guardOpLandedDiskDelete = "landed disk delete"

	// landedDiskNoHostMessage refuses a clustered DeleteLandedDisk without the
	// recorded import host.
	landedDiskNoHostMessage = "DeleteLandedDisk: a clustered libvirt provider requires target_host_id, the host the " +
		"import landed the disk on (ADR-0007 Addendum B)"
	// landedDiskInUseClusterMessage is the routed refusal of a landed disk a
	// domain on any host of the Provider uses. It names neither the domain
	// nor the host.
	landedDiskInUseClusterMessage = "DeleteLandedDisk: a libvirt domain on a host of this Provider uses the " +
		"migration's landed disk; it is kept"
	// landedDiskImportRunningMessage answers a delete while a host-side import
	// command of the same landing name still runs (the host lock is held).
	landedDiskImportRunningMessage = "DeleteLandedDisk: an import of this landing disk is still running on the host; " +
		"will retry"
	// landedDiskGuardRefusedMessage answers a delete the host guard refused.
	landedDiskGuardRefusedMessage = "DeleteLandedDisk: the host refused the removal (the provider's lock directory is " +
		"not a directory owned by its SSH user, or the landing name is a symbolic link); it is not removed"
	// landedDiskHostDownFmt is the HOST_UNAVAILABLE text of a routed delete.
	landedDiskHostDownFmt = "DeleteLandedDisk: host %q is unreachable; will retry"
	// landedDiskRemovalUnknownMessage is the retryable answer of a routed
	// delete whose host stopped answering after the removal was sent: the
	// disk may be gone. Retried (the removal is idempotent).
	landedDiskRemovalUnknownMessage = "DeleteLandedDisk: the host stopped answering while the landed disk was being " +
		"removed, so it is not known whether it was removed; will retry"

	// stagedDiskNoHostMessage refuses a clustered nfs DeleteStagedDisk
	// without the recorded import host.
	stagedDiskNoHostMessage = "DeleteStagedDisk: a clustered libvirt provider empties an nfs staged object with the " +
		"qemu-img of the host the import was sent to: target_host_id is required (ADR-0007 Addendum B)"
	// stagedDiskNeedsSSHMessage refuses an nfs delete on a host without an
	// ssh:// transport.
	stagedDiskNeedsSSHMessage = "DeleteStagedDisk: an nfs staged object is reached by the host's qemu-img, which " +
		"needs an ssh:// libvirt transport"
	// stagedDiskHostFailedMessage is the retryable answer of an nfs delete
	// that failed on the host for another reason.
	stagedDiskHostFailedMessage = "DeleteStagedDisk: the nfs staged object could not be emptied on the host; the " +
		"cause is in the provider log"
	// stagedDiskHostDownFmt is the HOST_UNAVAILABLE text of a routed nfs delete.
	stagedDiskHostDownFmt = "DeleteStagedDisk: host %q is unreachable; will retry"
)

// deleteLandedDiskRouted is the CLUSTERED DeleteLandedDisk (see the file
// comment). It returns wire errors.
func (p *Provider) deleteLandedDiskRouted(ctx context.Context, req *providerv1.DeleteLandedDiskRequest) (*providerv1.DeleteLandedDiskResponse, error) {
	host := hostconn.HostID(strings.TrimSpace(req.GetTargetHostId()))
	if host == "" {
		return nil, status.Error(codes.InvalidArgument, landedDiskNoHostMessage)
	}
	target := ownerFromProto(req.GetTargetVm())
	if !hasNamingIdentity(target) {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	volumeName, err := importedDiskVolumeName(target, "")
	if err != nil || volumeName == "" {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	domain, err := domainNameFor(target, "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()

	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	// The routed import's landing lock: an import of the same name in flight
	// in this provider is answered retryably, never with NOT_FOUND.
	unlock, err := landingLocks.lockUnlessImporting(bctx, routedLandingKey(volumeName))
	if stderrors.Is(err, errLandingImportInFlight) {
		// Retryable, and kept out of the manager's circuit breaker.
		return nil, vmOperationStatus(codes.Unavailable, landedDiskImportInFlightMessage)
	}
	if err != nil {
		return nil, routedLandedDiskFailed(host, nil, "wait for other work on the landing disk", err)
	}
	defer unlock()

	var outcome providerv1.LandedDiskDeleteOutcome
	err = p.withHostConn(bctx, string(host), func(bctx context.Context, c libvirtConn) error {
		if p.routedDiskConnFn != nil {
			c = p.routedDiskConnFn(c)
		}
		o, err := p.deleteLandedOnHost(bctx, c, req.GetPath(), volumeName, domain, migrationRef)
		outcome = o
		return err
	})
	if err != nil {
		return nil, routedLandedDiskError(host, err)
	}
	return &providerv1.DeleteLandedDiskResponse{Outcome: outcome}, nil
}

// deleteLandedOnHost removes the landed disk volumeName of domain on the
// landing host behind c, after the cross-host in-use scan. path is the
// recorded landing path ("" when none was recorded).
func (p *Provider) deleteLandedOnHost(ctx context.Context, c libvirtConn, path, volumeName, domain, migrationRef string) (providerv1.LandedDiskDeleteOutcome, error) {
	unspecified := providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_UNSPECIFIED
	host := c.HostID()
	h := hostConnRunner{conn: c}
	poolPath, err := defaultPoolPath(ctx, c)
	if err != nil {
		return unspecified, routedLandedDiskFailed(host, hostProgressFrom(ctx), "resolve the default pool's directory", err)
	}
	landed := filepath.Join(poolPath, volumeName+qcow2Ext)
	if path != "" && path != landed {
		return unspecified, status.Error(codes.InvalidArgument, landedDiskPathMismatchMessage)
	}
	kind, err := hostDiskKind(ctx, h, landed)
	if err != nil {
		return unspecified, routedLandedDiskFailed(host, hostProgressFrom(ctx), "check the landed disk", err)
	}
	switch kind {
	case diskKindAbsent:
		log.Printf("INFO The landed disk of migration %s is already gone on host %s: %s", migrationRef, host, landed)
		return providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND, nil
	case diskKindSymlink:
		return unspecified, status.Error(codes.InvalidArgument, landedDiskSymlinkMessage)
	case diskKindFile:
	default:
		return unspecified, status.Error(codes.InvalidArgument, landedDiskNotAFileMessage)
	}
	inUse, err := p.landedDiskUsedOnAnyHost(ctx, host, h, landed, domain)
	if err != nil {
		return unspecified, err
	}
	if inUse {
		log.Printf("WARN Not deleting the landed disk of migration %s on host %s: a domain on a host of the Provider uses %s",
			migrationRef, host, landed)
		return unspecified, diskInUseStatus(landedDiskInUseClusterMessage, true).Err()
	}
	// The rm runs under the import's host lock (flock -n: an import command
	// left over from a cancelled call is "still running") and the guard,
	// which refuses a landing name that became a symbolic link.
	lock, err := p.hostLockFor(importLockKind, domain)
	if err != nil {
		return unspecified, routedLandedDiskFailed(host, hostProgressFrom(ctx), "name the host lock", err)
	}
	argv, err := guardedHostCommand(ctx, lock, landed, "rm", "-f", "--", landed)
	if err != nil {
		return unspecified, routedLandedDiskFailed(host, hostProgressFrom(ctx), "bound the removal", err)
	}
	if _, err := runHost(ctx, h, argv...); err != nil {
		switch guardExitCode(err) {
		case flockBusyExit:
			return unspecified, vmOperationStatus(codes.Unavailable, landedDiskImportRunningMessage)
		case hostGuardRefusedExit:
			reportHostGuardRefusal("landed disk delete", string(host), err)
			return unspecified, status.Error(codes.FailedPrecondition, landedDiskGuardRefusedMessage)
		}
		return unspecified, routedLandedDiskFailed(host, hostProgressFrom(ctx), "remove the landed disk", err)
	}
	if _, err := c.Virsh(ctx, "pool-refresh", defaultStoragePool); err != nil {
		log.Printf("WARN pool-refresh on host %s after removing a landed disk failed (harmless): %v", host, err)
	}
	log.Printf("INFO Removed the landed disk of migration %s on host %s: %s", migrationRef, host, landed)
	return providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, nil
}

// landedDiskUsedOnAnyHost reports whether a domain on ANY host of the
// Provider — the landing host, scanned over its call's connection h, included
// — uses the landed disk file landed (as the landing host names it, and as it
// resolves there). It fails closed: a host that cannot be scanned is a
// clusterGuardIncompleteError (HOST_UNAVAILABLE or VM_DISK_CHECK_FAILED,
// retryable), and the disk is kept.
func (p *Provider) landedDiskUsedOnAnyHost(ctx context.Context, host hostconn.HostID, h hostCommandRunner, landed, domain string) (bool, error) {
	canon, err := canonicalizeOnHost(ctx, h, []string{landed})
	if err != nil {
		return false, routedLandedDiskFailed(host, hostProgressFrom(ctx), "resolve the landed disk", err)
	}
	files := []string{landed}
	if canon[0] != landed {
		files = append(files, canon[0])
	}
	res, err := p.scanClusterDiskUse(ctx, clusterScan{files: files, target: host, conn: h,
		// The first use, or the first host that cannot be checked, decides.
		stopWhen:    func(r hostDiskScan) bool { return r.users > 0 },
		stopOnError: true,
	})
	if res.users > 0 {
		return true, nil
	}
	if err != nil {
		var ie *clusterGuardIncompleteError
		if stderrors.As(err, &ie) {
			return false, ie.with(guardOpLandedDiskDelete, domain)
		}
		return false, routedLandedDiskFailed(host, hostProgressFrom(ctx), "scan the Provider's hosts", err)
	}
	return false, nil
}

// routedLandedDiskFailed logs a failed host step of a routed landed-disk
// delete and returns its retryable answer. progress is what the call ran on
// the host so far (host_progress.go). A host that could not be reached is
// HOST_UNAVAILABLE only while nothing of the call ran on it; once the host
// answered, or the removal itself was sent, it is the constant
// landedDiskFailedMessage or landedDiskRemovalUnknownMessage, both
// codes.Unavailable with VM_OPERATION_FAILED (kept out of the manager's
// circuit breaker, and retried: the removal is idempotent, and a retry that
// finds the disk gone is NOT_FOUND). Any other failure is
// landedDiskFailedMessage. The cause goes to the provider log only.
func routedLandedDiskFailed(host hostconn.HostID, progress *hostProgress, step string, err error) error {
	log.Printf("ERROR DeleteLandedDisk on host %s: %s: %v", host, step, err)
	if isHostTransportFailure(err) {
		switch progress.stage() {
		case hostDownNothingRan:
			return hostUnavailableStatus(contracts.NewHostUnavailableError(fmt.Sprintf(landedDiskHostDownFmt, host), nil))
		case hostDownWriteSent:
			return vmOperationStatus(codes.Unavailable, landedDiskRemovalUnknownMessage)
		}
	}
	return vmOperationStatus(codes.Unavailable, landedDiskFailedMessage)
}

// routedLandedDiskError is the wire form of a routed landed-disk delete that
// failed: the guard's own answers and every status the steps produced keep
// their form; a host that could not be leased is HOST_UNAVAILABLE.
func routedLandedDiskError(host hostconn.HostID, err error) error {
	if st := clusterGuardStatus(err); st != nil {
		return st.Err()
	}
	if st := innerStatus(err); st != nil {
		return st.Err()
	}
	if contracts.IsHostUnavailable(err) {
		return hostUnavailableStatus(contracts.NewHostUnavailableError(fmt.Sprintf(landedDiskHostDownFmt, host), err))
	}
	var ho *hostOpError
	if stderrors.As(err, &ho) {
		return routedLandedDiskFailed(host, ho.progress, "delete the landed disk", err)
	}
	return routedLandedDiskFailed(host, nil, "delete the landed disk", err)
}

// deleteStagedDiskRouted is the CLUSTERED DeleteStagedDisk of the resolved
// staged object disk (see the file comment).
func (p *Provider) deleteStagedDiskRouted(ctx context.Context, req *providerv1.DeleteStagedDiskRequest, disk migration.StagedDisk) (providerv1.StagedDiskDeleteOutcome, error) {
	unspecified := providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED
	if disk.Backend == migration.BackendS3 {
		// The pod is the S3 client: no host is involved.
		return migration.DeleteS3StagedDisk(ctx, disk, req.GetCredentials())
	}
	host := strings.TrimSpace(req.GetTargetHostId())
	if host == "" {
		return unspecified, &migration.StagedDiskError{Code: codes.InvalidArgument, Msg: stagedDiskNoHostMessage}
	}
	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	var outcome providerv1.StagedDiskDeleteOutcome
	err := p.withHostConn(bctx, host, func(bctx context.Context, c libvirtConn) error {
		if p.routedDiskConnFn != nil {
			c = p.routedDiskConnFn(c)
		}
		if err := p.checkHostDiskTransport(c); err != nil {
			return &migration.StagedDiskError{Code: codes.Unimplemented, Msg: stagedDiskNeedsSSHMessage, Cause: err}
		}
		// Every qemu-img runs under timeout(1) within the routed budget.
		run := func(ctx context.Context, args ...string) (string, string, error) {
			timeout, err := hostCommandTimeout(ctx)
			if err != nil {
				return "", "", err
			}
			argv := append(append(timeout, "qemu-img"), args...)
			res, err := c.RunHost(ctx, argv...)
			if res == nil {
				return "", "", err
			}
			return res.Stdout, res.Stderr, err
		}
		o, err := migration.EmptyNFSStagedDisk(bctx, run, disk.NFSURL)
		outcome = o
		return err
	})
	if err == nil {
		return outcome, nil
	}
	var se *migration.StagedDiskError
	if stderrors.As(err, &se) {
		return unspecified, se
	}
	if contracts.IsHostUnavailable(err) || isHostTransportFailure(err) {
		// The cause names the staged object's nfs:// URL: logged redacted,
		// never passed on (#415).
		log.Printf("WARN DeleteStagedDisk on host %s: the host could not be reached: %v", host,
			redactLocations(err, disk.NFSURL, disk.Key))
		return unspecified, hostUnavailableStatus(contracts.NewHostUnavailableError(fmt.Sprintf(stagedDiskHostDownFmt, host), nil))
	}
	return unspecified, &migration.StagedDiskError{Code: codes.Unavailable, Msg: stagedDiskHostFailedMessage, Cause: err}
}
