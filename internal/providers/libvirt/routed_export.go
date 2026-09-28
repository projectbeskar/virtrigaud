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
// core. Errors are mapped by routedRPCError: a domain this VM does not own is
// NotFound; a failure on the host is VM_OPERATION_FAILED or HOST_UNAVAILABLE,
// neither of which counts toward the manager's circuit breaker.
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
		return nil, status.Errorf(codes.InvalidArgument, "nfs export destination must be an nfs:// URL, got %q", req.DestinationUrl)
	}

	vm := contracts.VMRef{ID: req.VmId, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())}
	// The export is long: it runs under the routed budget, and its qemu-img
	// under flock + timeout (routed_budget.go).
	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	var resp *providerv1.ExportDiskResponse
	err := p.withOwnedDomain(bctx, vm, "disk export", func(c libvirtConn, d domainTarget) error {
		if err := p.checkHostDiskTransport(c); err != nil {
			return err
		}
		vp, err := virshOf(c)
		if err != nil {
			return err
		}
		info, err := diskInfoOn(bctx, vp, d, contracts.GetDiskInfoRequest{VM: vm, DiskId: req.DiskId, SnapshotId: req.SnapshotId})
		if err != nil {
			return fmt.Errorf("failed to resolve source disk info: %w", err)
		}
		if info.Path == "" {
			return fmt.Errorf("source disk %q has no resolvable host path", req.DiskId)
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
			r, err = exportConvertToNFS(bctx, c, req, info.Path, guard)
		} else {
			// The s3 export stages its flattened copy next to the source disk.
			p.warnIfUnsafeDir(bctx, vp, c.HostID(), filepath.Dir(info.Path))
			r, err = exportFlattenToS3(bctx, c, req, d.name, info.Path, guard)
		}
		if r != nil && r.Task != nil {
			r.Task.Id = encodeHostTaskRef(c.HostID(), r.Task.Id)
		}
		resp = r
		return err
	})
	if err != nil {
		return nil, routedRPCError("export disk",
			classifyRoutedFailure("export disk", strings.TrimSpace(req.GetTargetHostId()), ctx, bctx, err))
	}
	return resp, nil
}
