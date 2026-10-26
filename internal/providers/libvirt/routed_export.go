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
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Routed disk export of a CLUSTERED provider (ADR-0007 Addendum A, slice 3).
//
// A clustered VM's disk lives on its bound host, so only the host-side export
// transports are served: s3 (the host's qemu-img flattens the disk, the pod
// streams it to S3 over the host's SSH connection) and nfs (the host's
// qemu-img writes it straight to the export). The pvc backend reads the disk
// from the provider pod and is refused, like on Proxmox. The export runs on
// the VM's bound host (target_host_id), only for a domain whose owner stamp is
// the requester's, on the disk resolved from that domain's own definition.

// requireSSHDiskTransport is the default hostDiskTransportFn: the s3 and nfs
// exports run qemu-img on the host and stream over its SSH connection. The
// refusal names the host id only, never its endpoint.
func requireSSHDiskTransport(c libvirtConn) error {
	if strings.Contains(c.uri(), "ssh://") {
		return nil
	}
	return contracts.NewInvalidSpecError(fmt.Sprintf(
		"disk export needs an ssh:// libvirt connection to host %q (host-side qemu-img and stream)", c.HostID()), nil)
}

// checkHostDiskTransport runs p.hostDiskTransportFn (default
// requireSSHDiskTransport) on c.
func (p *Provider) checkHostDiskTransport(c libvirtConn) error {
	if p.hostDiskTransportFn != nil {
		return p.hostDiskTransportFn(c)
	}
	return requireSSHDiskTransport(c)
}

// exportDiskRouted is the CLUSTERED ExportDisk (ADR-0007 Addendum A, slice 3).
// Every refusal of the request itself (backend, transfer mode, destination)
// happens before any host is leased. Then, on the leased connection of
// req.TargetHostId: the owner check, the transport check, the disk resolved
// from the checked domain's definition (diskInfoOn), and the shared s3 / nfs
// core. Errors are answered by routedExportFailure: a domain this VM does not
// own is NotFound; a failure of the shared core that the single-host export
// proves definite is answered the same way (not performed, or a refusal); a
// host that stopped answering is HOST_UNAVAILABLE before any command ran, "not
// performed" while only reads ran and an unknown outcome once the export
// started (R25); any other failure on the host is VM_OPERATION_FAILED with an
// unknown outcome. None counts toward the manager's circuit breaker, and no
// log line or answer names the staged object's location (R28).
func (p *Provider) exportDiskRouted(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	// Only the host-side transports: the pvc backend (and the legacy empty
	// backend, which means pvc) reads the disk from the pod.
	if err := migration.EnsureS3OrNFSBackend(req.BackendType); err != nil {
		return nil, err
	}
	// Relay-mode enforcement applies to the s3 streaming path; the nfs
	// transport is qemu-img-native (ADR-0006 Slice 4), as on a single host.
	if req.BackendType != migration.BackendNFS {
		if err := migration.EnsureRelayMode(req.TransferMode); err != nil {
			return nil, err
		}
	} else if !strings.HasPrefix(strings.TrimSpace(req.DestinationUrl), "nfs://") {
		// The value is not echoed: it is the tenant's location (R28).
		return nil, status.Error(codes.InvalidArgument, "nfs export destination must be an nfs:// URL")
	}

	// The staged object's locations are redacted from every logged cause
	// (R28).
	locations := exportStagedLocations(req)
	vm := contracts.VMRef{ID: req.VmId, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())}
	// The export is long: it runs under the routed budget, and its qemu-img
	// under flock + timeout (routed_budget.go).
	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	var resp *providerv1.ExportDiskResponse
	err := p.withOwnedDomain(bctx, vm, "disk export", func(bctx context.Context, c libvirtConn, d domainTarget) error {
		if p.routedDiskConnFn != nil {
			c = p.routedDiskConnFn(c)
		}
		if err := p.checkHostDiskTransport(c); err != nil {
			return err
		}
		vp, err := virshOf(c)
		if err != nil {
			return err
		}
		// The domain's own disk and the format its definition opens it in:
		// the export pins it (-f), as root or as the SSH user.
		srcPath, srcFormat, err := exportSourceOn(bctx, vp, d, req.DiskId)
		if err != nil {
			return fmt.Errorf("failed to resolve the source disk: %w", err)
		}
		// One export of a domain at a time, under a lock in the provider's
		// own lock directory (never next to the source disk).
		lock, err := p.hostLockFor(exportLockKind, d.name)
		if err != nil {
			return err
		}
		guard := guardFor(bctx, lock)
		var r *providerv1.ExportDiskResponse
		if req.BackendType == migration.BackendNFS {
			r, err = exportConvertToNFS(bctx, c, req, srcPath, srcFormat, guard)
		} else {
			// The s3 export stages its flattened copy next to the source disk:
			// warn once when others can write that directory (#358's check).
			vp.warnIfDiskDirUnsafe(bctx, filepath.Dir(srcPath))
			r, err = exportFlattenToS3(bctx, c, req, d.name, srcPath, srcFormat, guard)
		}
		if r != nil && r.Task != nil {
			r.Task.Id = encodeHostTaskRef(c.HostID(), r.Task.Id)
		}
		resp = r
		return err
	})
	if err != nil {
		return nil, routedExportFailure(strings.TrimSpace(req.GetTargetHostId()), ctx, bctx, err, locations...)
	}
	return resp, nil
}

// routedExportOp names the routed export in its answers and log lines.
const routedExportOp = "export disk"

// routedExportFailure answers a failed routed export on host. parent is the
// call's context, budget the routed budget; locations are the staged object's
// (exportStagedLocations), replaced in every logged cause (R28: the S3
// client's error names the bucket, key and endpoint, qemu-img's the nfs
// server, export, path and uid/gid).
//
// The routed guards' answers come first (classifyRoutedFailure: the budget ran
// out or timeout(1) stopped the command — unknown; the same export still runs
// — retry; the lock directory refused; a host tool missing), then every
// categorized answer (routedTypedStatus: the owner check's NotFound, a refused
// source chain, a host that could not be leased). A failure of the shared s3 /
// nfs core is then answered as the single-host export answers it
// (singleHostExportFailure), because the core proves the same facts on a
// clustered host:
//
//   - stageInvalid (the request's s3 configuration is unusable): InvalidArgument.
//   - stageBefore (nothing was published: a check, a step in the export's
//     private directory, or an S3 upload the service refused or failed before
//     the client read the source to its end, so it never sent the completing
//     call — SignatureDoesNotMatch, AccessDenied): "not performed".
//   - stageCommand (the nfs export's in-place convert, the s3 export's
//     flatten) that ended on its own with an exit status while the call was
//     alive (hostCommandEnded) — an nfs mount the server refused
//     (MNT3ERR_ACCES), a qemu-img error: "failed, nothing exported", which
//     the manager reads as "not performed" as well.
//
// A host that stopped answering (isHostTransportFailure) is never read that
// way: it is answered by how far the call got on the host (R25,
// hostOpRPCErrorFor) — unknown once a command that may change the host was
// sent. So is a stageCommand whose command did not end on its own (its
// session dropped, it was killed, the call was cancelled): it may still be
// writing, and its outcome stays unknown, as is any failure the core did not
// tag (the S3 upload's completing call may have taken effect).
func routedExportFailure(host string, parent, budget context.Context, err error, locations ...string) error {
	err = classifyRoutedFailure(routedExportOp, host, parent, budget, err)
	cause := redactLocations(err, locations...)
	if st := routedTypedStatus(routedExportOp, cause); st != nil {
		return st
	}
	if definite := routedExportDefiniteFailure(budget, err, cause); definite != nil {
		return definite
	}
	return routedHostFailureStatus(hostOpWrite, routedExportOp, cause)
}

// routedExportDefiniteFailure is the single-host classification of a failure
// of the shared export core that routedExportFailure accepts (see there), or
// nil. err is the failure, cause its redacted form (logged).
func routedExportDefiniteFailure(ctx context.Context, err, cause error) error {
	stage, _, tagged := stageOf(err)
	if !tagged || isHostTransportFailure(err) {
		return nil
	}
	switch stage {
	case stageInvalid:
		return refusal(codes.InvalidArgument, wireExportRefused, cause)
	case stageBefore:
		return nothingDone(wireExportNotPerformed, cause)
	case stageCommand:
		if hostCommandEnded(ctx, err) {
			return notPerformed(wireExportFailed, cause)
		}
	}
	return nil
}

// exportStagedLocations returns the texts that locate req's staged object, for
// redactLocations (R28): for nfs, its nfs:// URL and the texts in it
// (nfsURLLocations); for s3, its s3:// URL, its object key, its bucket and
// its endpoint (migration.StagedDiskLocations).
func exportStagedLocations(req *providerv1.ExportDiskRequest) []string {
	dest := strings.TrimSpace(req.GetDestinationUrl())
	if req.GetBackendType() == migration.BackendNFS {
		return nfsURLLocations(dest)
	}
	// An unparsable options document has no bucket or endpoint to redact; the
	// core refuses it (stageInvalid).
	opts, _ := migration.ParseStorageOptions(req.GetStorageOptionsJson())
	disk := migration.StagedDisk{Backend: migration.BackendS3, Options: opts}
	if rest, ok := strings.CutPrefix(dest, "s3://"); ok {
		if bucket, key, found := strings.Cut(rest, "/"); found && bucket == strings.TrimSpace(opts.Bucket) {
			disk.Key = key
		} else {
			disk.Key = rest
		}
	}
	return append([]string{dest}, stagedDiskLocations(disk)...)
}
