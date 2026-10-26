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
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stagedDiskLocations returns the texts that locate disk, for redactLocations
// (R28): its nfs:// URL and the texts in it, its object key and, for s3, its
// bucket name and endpoint (migration.StagedDiskLocations). An error from the
// S3 client names the bucket, and the key alone would leave it in the log.
func stagedDiskLocations(disk migration.StagedDisk) []string {
	return migration.StagedDiskLocations(disk)
}

// DeleteStagedDisk removes a VMMigration's staging object (ADR-0011). The
// object is derived from the migration identity (migration.ResolveStagedDisk;
// the request carries no location). s3 objects are removed from the provider
// pod, the S3 client of the relay path. nfs objects are reached exactly as
// the NFS import reads them (nfs.go): the host's qemu-img over libnfs, as the
// SSH user, with the migration's uid/gid in the URL. libnfs through qemu-img
// cannot unlink, so the object is emptied (truncated to zero bytes) instead
// (migration.EmptyNFSStagedDisk). Credentials are never logged, and the
// location is not logged at all (this provider logs with log.Printf, which has
// no debug level).
//
// A clustered provider removes s3 objects from the pod as well, and empties an
// nfs object with the qemu-img of req.target_host_id, the host its routed
// import was sent to (deleteStagedDiskRouted, ADR-0007 Addendum B).
func (s *Server) DeleteStagedDisk(ctx context.Context, req *providerv1.DeleteStagedDiskRequest) (*providerv1.DeleteStagedDiskResponse, error) {
	disk, err := migration.ResolveStagedDisk(req)
	if err != nil {
		return nil, err
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()

	var outcome providerv1.StagedDiskDeleteOutcome
	switch {
	case s.clusteredProvider():
		outcome, err = s.provider.deleteStagedDiskRouted(ctx, req, disk)
	case disk.Backend == migration.BackendS3:
		outcome, err = migration.DeleteS3StagedDisk(ctx, disk, req.GetCredentials())
	default: // single-host migration.BackendNFS: ResolveStagedDisk accepts nothing else.
		outcome, err = s.emptyNFSStagedDisk(ctx, disk)
	}
	if err != nil {
		// The status message names no location; the cause (stderr, host)
		// goes to the DEBUG line only, with the staged object's location
		// (nfs URL, s3 key) replaced (#415).
		log.Printf("WARN Could not delete the staged disk of migration %s (backend %s): %s", migrationRef, disk.Backend,
			status.Convert(err).Message())
		log.Printf("DEBUG Staged disk delete failure cause for migration %s: %v", migrationRef,
			redactLocations(err, stagedDiskLocations(disk)...))
		return nil, err
	}
	log.Printf("INFO Removed the staged disk of migration %s (backend %s): %s", migrationRef, disk.Backend, outcome)
	return &providerv1.DeleteStagedDiskResponse{Outcome: outcome}, nil
}

// emptyNFSStagedDisk empties the staged nfs object through the single host's
// qemu-img (libnfs), over the same ssh:// connection the NFS import uses.
func (s *Server) emptyNFSStagedDisk(ctx context.Context, disk migration.StagedDisk) (providerv1.StagedDiskDeleteOutcome, error) {
	unspecified := providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED
	if s.provider == nil {
		return unspecified, &migration.StagedDiskError{Code: codes.Unavailable, Msg: "DeleteStagedDisk: libvirt provider not initialized"}
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return unspecified, &migration.StagedDiskError{Code: codes.Unavailable,
			Msg: "DeleteStagedDisk: the libvirt host cannot be reached", Cause: err}
	}
	if !strings.Contains(conn.uri(), "ssh://") {
		return unspecified, &migration.StagedDiskError{Code: codes.Unimplemented,
			Msg: "DeleteStagedDisk: an nfs staged object is reached by the host's qemu-img, which needs an ssh:// libvirt transport"}
	}
	ctx = withNFSLogRedactions(ctx, disk.NFSURL)
	run := func(ctx context.Context, args ...string) (string, string, error) {
		res, err := conn.RunHost(ctx, append([]string{"qemu-img"}, args...)...)
		if res == nil {
			return "", "", err
		}
		return res.Stdout, res.Stderr, err
	}
	return migration.EmptyNFSStagedDisk(ctx, run, disk.NFSURL)
}
