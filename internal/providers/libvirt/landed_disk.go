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
	"encoding/xml"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Landed-disk delete (rc2 R7).
//
// A migration whose import landed "<default pool>/<domain>-migrated.qcow2"
// (importedDiskVolumeName over the target VM: the name the target VM's Create
// attaches in place) and that then failed for good, or was deleted, before a
// target VM attached it, asks this provider to remove it (DeleteLandedDisk).
// The file is derived from the request's target VM, exactly as ImportDisk
// names it, in the default pool's directory (the manager never sends a storage
// hint); a recorded path, when sent, must be exactly that file. It is removed
// only when it is a regular file — a symbolic link there is refused — and no
// domain on the host uses it as a disk, backing file or shared directory
// (pathInUseOnHost, the same fail-closed scan ImportDisk runs before it lands
// over the name). Nothing else is ever removed.

const (
	// landedDiskInvalidMessage refuses a request without a valid target VM
	// identity.
	landedDiskInvalidMessage = "DeleteLandedDisk: the target VirtualMachine's namespace and name do not name a libvirt " +
		"landing disk"
	// landedDiskPathMismatchMessage refuses a recorded path that is not the
	// target VM's landing disk. It never echoes the path.
	landedDiskPathMismatchMessage = "DeleteLandedDisk: the recorded landing path is not the target VirtualMachine's " +
		"landing disk (<default pool>/<namespace>.<vm name>-migrated.qcow2)"
	// landedDiskInUseMessage is the refusal of a landed disk a domain uses.
	// It never names the domain.
	landedDiskInUseMessage = "DeleteLandedDisk: a libvirt domain on the host uses the migration's landed disk; it is kept"
	// landedDiskSymlinkMessage refuses a landing name that is a symbolic
	// link: VirtRigaud never wrote one there, and never removes one.
	landedDiskSymlinkMessage = "DeleteLandedDisk: the landing name is a symbolic link on the host; it is not removed"
	// landedDiskFailedMessage is the retryable answer of a host step that
	// failed; the cause goes to the provider log only.
	landedDiskFailedMessage = "DeleteLandedDisk: the landed disk could not be checked or removed on the host; the " +
		"cause is in the provider log; will retry"
)

// DeleteLandedDisk removes the disk a migration's ImportDisk landed for
// req.target_vm (see above). A disk that does not exist is NOT_FOUND; one a
// domain uses is refused with VM_DISK_IN_USE and kept. A clustered provider
// answers Unimplemented and does not report the capability: its ImportDisk is
// not routed, so it never lands a disk.
func (s *Server) DeleteLandedDisk(ctx context.Context, req *providerv1.DeleteLandedDiskRequest) (*providerv1.DeleteLandedDiskResponse, error) {
	if s.clusteredProvider() {
		return nil, status.Error(codes.Unimplemented,
			"DeleteLandedDisk is not served by a clustered libvirt provider (it is never a migration target)")
	}
	target := ownerFromProto(req.GetTargetVm())
	if !hasNamingIdentity(target) {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	volumeName, err := importedDiskVolumeName(target, "")
	if err != nil || volumeName == "" {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	if s.provider == nil {
		return nil, status.Error(codes.Unavailable, "DeleteLandedDisk: libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, landedDiskFailed("connect to the host", err)
	}
	poolPath, err := defaultPoolPath(ctx, conn)
	if err != nil {
		return nil, landedDiskFailed("resolve the default pool's directory", err)
	}
	landed := filepath.Join(poolPath, volumeName+qcow2Ext)
	if p := req.GetPath(); p != "" && p != landed {
		return nil, status.Error(codes.InvalidArgument, landedDiskPathMismatchMessage)
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()

	h := hostConnRunner{conn: conn}
	exists, err := checkWriteTarget(ctx, h, importedDiskSubject(volumeName), landed)
	if err != nil {
		// A symbolic link at the name is refused for good (never followed,
		// never removed); a failed check is retryable. Both keep the name as
		// it is.
		if contracts.IsConflict(err) {
			return nil, status.Error(codes.InvalidArgument, landedDiskSymlinkMessage)
		}
		return nil, landedDiskFailed("check the landed disk", err)
	}
	if !exists {
		log.Printf("INFO The landed disk of migration %s is already gone: %s", migrationRef, landed)
		return &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND}, nil
	}
	inUse, err := pathInUseOnHost(ctx, h, landed)
	if err != nil {
		return nil, landedDiskFailed("check whether a domain uses the landed disk", err)
	}
	if inUse {
		log.Printf("WARN Not deleting the landed disk of migration %s: a domain on the host uses %s", migrationRef, landed)
		return nil, diskInUseStatus(landedDiskInUseMessage, false).Err()
	}
	// unlink(2) never follows a symbolic link; the name was checked to be no
	// link just above.
	if _, err := runHost(ctx, h, "rm", "-f", "--", landed); err != nil {
		return nil, landedDiskFailed("remove the landed disk", err)
	}
	if _, err := conn.Virsh(ctx, "pool-refresh", defaultStoragePool); err != nil {
		log.Printf("WARN pool-refresh after removing a landed disk failed (harmless): %v", err)
	}
	log.Printf("INFO Removed the landed disk of migration %s: %s", migrationRef, landed)
	return &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED}, nil
}

// defaultPoolPath is the directory of the default storage pool on the host
// behind conn, the pool ImportDisk lands migration disks in.
func defaultPoolPath(ctx context.Context, conn libvirtConn) (string, error) {
	res, err := conn.Virsh(ctx, "pool-dumpxml", defaultStoragePool)
	if err != nil {
		return "", fmt.Errorf("pool-dumpxml %s: %w", defaultStoragePool, err)
	}
	var pool poolTargetXML
	if err := xml.Unmarshal([]byte(res.Stdout), &pool); err != nil {
		return "", fmt.Errorf("parse pool-dumpxml %s: %w", defaultStoragePool, err)
	}
	dir := strings.TrimRight(strings.TrimSpace(pool.Target.Path), "/")
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("pool %s has no usable directory", defaultStoragePool)
	}
	return dir, nil
}

// landedDiskFailed logs a failed landed-disk host step and returns the
// constant, retryable error the manager sees.
func landedDiskFailed(step string, err error) error {
	log.Printf("ERROR DeleteLandedDisk: %s: %v", step, err)
	return status.Error(codes.Unavailable, landedDiskFailedMessage)
}
