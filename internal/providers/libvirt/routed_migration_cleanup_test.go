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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin ADR-0007 Addendum B on the shared-pool fixture of
// cluster_disk_guard_test.go (host-a and host-b over ONE scratch storage
// pool, with real host-shell tools — sh, realpath, mktemp, flock, timeout,
// rm — and fake virsh / qemu-img): the routed import's cross-host guard on
// the landing file (B2 step 2), its real guarded write phase, the routed
// DeleteLandedDisk (B4) with the in-use scan of every host, and the routed
// nfs DeleteStagedDisk.

// sharedImportRequest is a routed nfs import of importTarget to host-b.
func sharedImportRequest() *providerv1.ImportDiskRequest {
	return nfsRequest("host-b")
}

// landedOnShared writes the target's landing file in the shared pool.
func landedOnShared(s *sharedPool, content string) string {
	return s.disk(importVolume+qcow2Ext, content)
}

// requireUnchanged asserts the file still holds content.
func requireUnchanged(t *testing.T, path, content string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, content, string(b))
}

// privateDirsIn lists the private write directories left in dir.
func privateDirsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), vmDiskWriteDirPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestRoutedImport_SharedPool_LandingFileUsedOnAnotherHostIsRefused: the
// landing file exists in the shared pool and a domain on host-a uses it; the
// import to host-b scans every host and refuses (AlreadyExists), writing
// nothing and naming neither the other domain nor the host.
func TestRoutedImport_SharedPool_LandingFileUsedOnAnotherHostIsRefused(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "shut off", true: "running"}[running], func(t *testing.T) {
			s := newSharedPool(t)
			s.p.hostDiskTransportFn = anyTransport
			landed := landedOnShared(s, "live-disk")
			s.defineOn("host-a", "legacy-moved", uuidForeign, guardDomainXML("legacy-moved", uuidForeign, landed, ownerForeign, running))

			_, err := NewServer(s.p).ImportDisk(callCtx(t), sharedImportRequest())
			st := status.Convert(err)
			assert.Equal(t, codes.AlreadyExists, st.Code(), "%v", err)
			assert.Contains(t, st.Message(), "in use by another domain on a host of this Provider")
			requireWireSafe(t, st.Message(), s.images, "legacy-moved", uuidForeign, "team-z")
			requireUnchanged(t, landed, "live-disk")
			assert.Empty(t, privateDirsIn(t, s.images), "nothing was written")
			s.requireReadOnly("host-a")
		})
	}
}

// TestRoutedImport_SharedPool_UnreachableHostFailsClosed: the landing file
// exists and a host of the Provider cannot be scanned: the import fails
// closed (HOST_UNAVAILABLE, retryable) and the file is untouched.
func TestRoutedImport_SharedPool_UnreachableHostFailsClosed(t *testing.T) {
	s := newSharedPoolWithDeadHost(t)
	s.p.hostDiskTransportFn = anyTransport
	landed := landedOnShared(s, "leftover")

	_, err := NewServer(s.p).ImportDisk(callCtx(t), sharedImportRequest())
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.True(t, hasHostUnavailableInfo(st), "fail closed, out of the breaker: %v", st)
	requireWireSafe(t, st.Message(), s.images)
	requireUnchanged(t, landed, "leftover")
	assert.Empty(t, privateDirsIn(t, s.images))
}

// TestRoutedImport_SharedPool_PreviousIncarnationElsewhere: a domain on host-a
// stamped for the target's namespace and name under another uid uses its old
// disk in the shared pool: the import is held as VM_PREVIOUS_INCARNATION.
func TestRoutedImport_SharedPool_PreviousIncarnationElsewhere(t *testing.T) {
	s := newSharedPool(t)
	s.p.hostDiskTransportFn = anyTransport
	old := s.disk(importDomain+"-disk.qcow2", "old-disk")
	stale := contracts.ObjectIdentity{UID: "5e1f0c3a-9b8d-4e7f-a6c5-4b3a2d1e0f9a", Namespace: importTarget.Namespace, Name: importTarget.Name}
	s.defineOn("host-a", importDomain, uuidPrevious, guardDomainXML(importDomain, uuidPrevious, old, stale, false))

	_, err := NewServer(s.p).ImportDisk(callCtx(t), sharedImportRequest())
	st := status.Convert(err)
	assert.Equal(t, codes.AlreadyExists, st.Code(), "%v", err)
	assert.True(t, errorInfoReasons(st)[contracts.VMPreviousIncarnationReason], "%v", st)
	requireUnchanged(t, old, "old-disk")
	assert.Empty(t, privateDirsIn(t, s.images))
}

// TestRoutedImport_SharedPool_UnusedLeftoverRunsTheGuardedWritePhase: an
// unused leftover at the landing name passes the guard, and the write phase
// runs for real under the host guard, flock(1) and timeout(1): the lock
// directory is made with mode 0700, and when the staged object cannot be read
// the private directory is removed and the leftover is untouched ("not
// performed").
func TestRoutedImport_SharedPool_UnusedLeftoverRunsTheGuardedWritePhase(t *testing.T) {
	s := newSharedPool(t)
	s.p.hostDiskTransportFn = anyTransport
	landed := landedOnShared(s, "leftover")

	_, err := NewServer(s.p).ImportDisk(callCtx(t), sharedImportRequest())
	assertNotPerformedStatus(t, err)
	assert.Contains(t, s.log("qemu-img"), "info -U --output=json -f raw "+importNFSURL,
		"the write phase ran: the bounded copy measured the staged object")
	st, serr := os.Stat(filepath.Join(s.staging, hostLockDirName))
	require.NoError(t, serr, "the guard made the provider's lock directory")
	assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
	requireUnchanged(t, landed, "leftover")
	assert.Empty(t, privateDirsIn(t, s.images), "the private directory is removed")
}

// deleteLandedRequest asks the routed DeleteLandedDisk for importTarget on host.
func deleteLandedRequest(host string) *providerv1.DeleteLandedDiskRequest {
	return &providerv1.DeleteLandedDiskRequest{
		Migration:    &providerv1.ObjectIdentity{Uid: "uid-mig", Namespace: "team-a", Name: "mig"},
		TargetVm:     importTargetWire,
		TargetHostId: host,
	}
}

func TestRoutedDeleteLandedDisk_UnusedIsDeletedOnTheImportHost(t *testing.T) {
	s := newSharedPool(t)
	landed := landedOnShared(s, "landed")
	req := deleteLandedRequest("host-b")
	req.Path = landed

	resp, err := NewServer(s.p).DeleteLandedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	assert.NoFileExists(t, landed)
	s.requireReadOnly("host-a")
	assert.Contains(t, s.virshCalls("host-b"), "pool-dumpxml default", "the landing pool is read on the import's host")

	resp, err = NewServer(s.p).DeleteLandedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome(), "idempotent")
}

// TestRoutedDeleteLandedDisk_InUseOnAnotherHostIsKept: on a shared pool a
// domain on another host may use the landed disk: it is kept.
func TestRoutedDeleteLandedDisk_InUseOnAnotherHostIsKept(t *testing.T) {
	s := newSharedPool(t)
	landed := landedOnShared(s, "landed")
	s.defineOn("host-a", "other", uuidForeign, guardDomainXML("other", uuidForeign, landed, ownerForeign, true))

	_, err := NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
	requireDiskInUseStatus(t, err, true)
	assert.Equal(t, landedDiskInUseClusterMessage, status.Convert(err).Message())
	requireUnchanged(t, landed, "landed")
	s.requireReadOnly("host-a")
}

// TestRoutedDeleteLandedDisk_UnreachableHostHoldsTheCleanup: a host that
// cannot be scanned fails the delete closed, retryably; the disk is kept.
func TestRoutedDeleteLandedDisk_UnreachableHostHoldsTheCleanup(t *testing.T) {
	s := newSharedPoolWithDeadHost(t)
	landed := landedOnShared(s, "landed")

	_, err := NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.True(t, hasHostUnavailableInfo(st), "%v", st)
	requireWireSafe(t, st.Message(), s.images)
	requireUnchanged(t, landed, "landed")
}

// TestRoutedDeleteLandedDisk_Refusals: no host, an unknown host, a path that
// is not the landing file, and a symbolic link at the landing name.
func TestRoutedDeleteLandedDisk_Refusals(t *testing.T) {
	s := newSharedPool(t)
	landed := landedOnShared(s, "landed")

	_, err := NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest(""))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, landedDiskNoHostMessage, status.Convert(err).Message())

	_, err = NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest("host-x"))
	assert.True(t, hasHostUnavailableInfo(status.Convert(err)), "%v", err)

	req := deleteLandedRequest("host-b")
	req.Path = filepath.Join(s.images, "other-migrated.qcow2")
	_, err = NewServer(s.p).DeleteLandedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.NotContains(t, status.Convert(err).Message(), s.images, "never echoes the path")

	_, err = NewServer(s.p).DeleteLandedDisk(context.Background(), &providerv1.DeleteLandedDiskRequest{TargetHostId: "host-b"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a target VM is required")
	requireUnchanged(t, landed, "landed")

	require.NoError(t, os.Remove(landed))
	require.NoError(t, os.Symlink(filepath.Join(s.outside, "precious"), landed))
	_, err = NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, lerr := os.Lstat(landed)
	assert.NoError(t, lerr, "a link is never removed")
}

// TestRoutedDeleteLandedDisk_ImportCommandStillRunning: the removal runs under
// the import's host lock; while a host-side import command (left over from a
// cancelled call) holds it, the delete is answered "still running" and the
// disk is kept.
func TestRoutedDeleteLandedDisk_ImportCommandStillRunning(t *testing.T) {
	s := newSharedPool(t)
	landed := landedOnShared(s, "landed")
	dir := filepath.Join(s.staging, hostLockDirName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	f, err := os.OpenFile(filepath.Join(dir, importLockKind+"-"+importDomain+".lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test lock file
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	_, err = NewServer(s.p).DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.Equal(t, landedDiskImportRunningMessage, st.Message())
	requireUnchanged(t, landed, "landed")
}

// TestRoutedImport_ClusteredCreateTakesTheLandingLock: the clustered Create
// that attaches the imported disk in place takes the landing lock of its
// host (B2 step 3): while an import of that name holds it, the create waits
// and gives up retryably, having written nothing.
func TestRoutedImport_ClusteredCreateTakesTheLandingLock(t *testing.T) {
	s := newSharedPool(t)
	landed := s.disk("team-a.web-migrated.qcow2", "imported")
	unlock, err := landingLocks.lockForImport(context.Background(), routedLandingKey("host-b", "team-a.web-migrated"))
	require.NoError(t, err)
	defer unlock()

	req := s.createReq(ownerTeamA, landed)
	req.Image.ImportedDisk = true
	req.TargetHostID = "host-b"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = s.p.Create(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gave up waiting for other work on the migration landing disk")
	requireUnchanged(t, landed, "imported")
	for _, call := range s.virshCalls("host-b") {
		assert.NotRegexp(t, `^define `, call, "nothing was defined")
	}
}

// stripTimeoutConn passes a routed nfs DeleteStagedDisk's commands to an
// nfsObjectHost without their timeout(1) prefix, and records them as sent.
type stripTimeoutConn struct {
	*nfsObjectHost
	host hostconn.HostID
	sent [][]string
}

func (c *stripTimeoutConn) HostID() hostconn.HostID { return c.host }

func (c *stripTimeoutConn) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	c.sent = append(c.sent, argv)
	if len(argv) > 3 && argv[0] == "timeout" {
		argv = argv[3:]
	}
	return c.nfsObjectHost.RunHost(ctx, argv...)
}

func TestRoutedDeleteStagedDisk_NFSEmptiedOnTheImportHost(t *testing.T) {
	p := clusterOf(t, []string{"host-a", "host-b"})
	host := &stripTimeoutConn{nfsObjectHost: &nfsObjectHost{object: []byte("tenant disk")}}
	p.routedDiskConnFn = func(c libvirtConn) libvirtConn { host.host = c.HostID(); return host }
	uid, gid := int64(3000), int64(3000)
	req := stagedDeleteRequest(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", UID: &uid, GID: &gid}, nil)
	req.TargetHostId = "host-b"

	resp, err := NewServer(p).DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, resp.GetOutcome())
	assert.Equal(t, hostconn.HostID("host-b"), host.host, "the import's host")
	require.Len(t, host.sent, 2)
	for _, argv := range host.sent {
		assert.Equal(t, []string{"timeout", "--kill-after=" + hostCommandKillAfter}, argv[:2], "every qemu-img under timeout(1): %q", argv)
		assert.Equal(t, "qemu-img", argv[3])
	}
	assert.Empty(t, host.object, "the object's data is gone")

	// Refusals: no host (InvalidArgument), an unknown host (HOST_UNAVAILABLE).
	req.TargetHostId = ""
	_, err = NewServer(p).DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, stagedDiskNoHostMessage, status.Convert(err).Message())
	req.TargetHostId = "host-x"
	_, err = NewServer(p).DeleteStagedDisk(context.Background(), req)
	assert.True(t, hasHostUnavailableInfo(status.Convert(err)), "%v", err)
}

// TestRoutedDeleteStagedDisk_S3FromThePod: a clustered provider removes an s3
// object from the pod, as on a single host; no host is involved.
func TestRoutedDeleteStagedDisk_S3FromThePod(t *testing.T) {
	p := clusterOf(t, []string{"host-a"})
	p.routedDiskConnFn = func(libvirtConn) libvirtConn {
		t.Error("an s3 delete never leases a host")
		return nil
	}
	srv := s3fake.New("stage")
	t.Cleanup(srv.Close)
	req := stagedDeleteRequest(t, migration.BackendS3, migration.StorageOptions{Backend: migration.BackendS3, Bucket: "stage",
		Endpoint: srv.Endpoint(), UsePathStyle: true},
		map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"})
	key, err := migration.StagedObjectKey(migration.BackendS3, "", "", "team-a", "web", migration.StagingStageExport)
	require.NoError(t, err)
	srv.Put(key, []byte("tenant disk"))

	resp, err := NewServer(p).DeleteStagedDisk(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	assert.Empty(t, srv.Keys())
}
