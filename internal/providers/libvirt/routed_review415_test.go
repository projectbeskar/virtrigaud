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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the fixes of the #415 security review of the routed import
// and its cleanup (ADR-0007 Addendum B): the provider-wide landing lock across
// hosts, the routed DeleteLandedDisk's removal outcomes, the symlink refusal
// and the routed nfs DeleteStagedDisk's timeout.

// TestRoutedDeleteStagedDisk_NFSTimedOut: the nfs object's qemu-img stopped by
// timeout(1) (124) is a retryable Unavailable whose text names no location.
func TestRoutedDeleteStagedDisk_NFSTimedOut(t *testing.T) {
	p := clusterOf(t, []string{"host-b"})
	host := &stripTimeoutConn{nfsObjectHost: &nfsObjectHost{object: []byte("tenant disk")}, failWith: virshFailure(timeoutExpiredExit, "")}
	p.routedDiskConnFn = func(c libvirtConn) libvirtConn { host.host = c.HostID(); return host }
	uid, gid := int64(3000), int64(3000)
	req := stagedDeleteRequest(t, migration.BackendNFS, migration.StorageOptions{Backend: migration.BackendNFS,
		Server: "nfs.example", Export: "/exports", UID: &uid, GID: &gid}, nil)
	req.TargetHostId = "host-b"
	_, err := NewServer(p).DeleteStagedDisk(context.Background(), req)
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.NotContains(t, st.Message(), "nfs://")
	assert.NotContains(t, st.Message(), "nfs.example")
	assert.Equal(t, []byte("tenant disk"), host.object, "nothing was emptied")
	require.Len(t, host.sent, 1)
	assert.Equal(t, "timeout", host.sent[0][0])
}

// TestRoutedDeleteLandedDisk_RemovalOutcomes: the guarded rm of the landed
// disk — under the import's host lock, the guard refusing a landing name that
// became a symbolic link — and how each failure is answered (retryable or
// refused; the delete is idempotent).
func TestRoutedDeleteLandedDisk_RemovalOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		check func(t *testing.T, err error)
	}{
		"removed": {check: func(t *testing.T, err error) { require.NoError(t, err) }},
		"the rm's session drops: HOST_UNAVAILABLE, retried": {err: virshFailure(-1, "reset"), check: func(t *testing.T, err error) {
			assert.True(t, hasHostUnavailableInfo(status.Convert(err)), "%v", err)
		}},
		"the rm is stopped by timeout(1): retried, out of the breaker": {err: virshFailure(timeoutExpiredExit, ""), check: func(t *testing.T, err error) {
			st := status.Convert(err)
			assert.Equal(t, codes.Unavailable, st.Code())
			assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason))
			assert.Equal(t, landedDiskFailedMessage, st.Message())
		}},
		"the landing name became a symbolic link (guard 79): refused": {err: virshFailure(hostGuardRefusedExit, guardMsgTargetSymlink), check: func(t *testing.T, err error) {
			st := status.Convert(err)
			assert.Equal(t, codes.FailedPrecondition, st.Code())
			assert.Equal(t, landedDiskGuardRefusedMessage, st.Message())
		}},
		"an import command still holds the host lock (75): retried": {err: virshFailure(flockBusyExit, ""), check: func(t *testing.T, err error) {
			st := status.Convert(err)
			assert.Equal(t, codes.Unavailable, st.Code())
			assert.Equal(t, landedDiskImportRunningMessage, st.Message())
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newImportFixtureOf(t, []string{"host-b"})
			if tc.err != nil {
				f.rec.failOn("rm", 0, tc.err)
			}
			resp, err := f.server().DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
			tc.check(t, err)
			if err == nil {
				assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
			}
			var rm *importRecCall
			for _, c := range f.rec.snapshot() {
				if c.key() == "rm" {
					c := c
					rm = &c
				}
			}
			require.NotNil(t, rm)
			assert.True(t, rm.guarded, "under the guard and the import's host lock: %q", rm.argv)
			assert.Equal(t, importLanded, rm.argv[6], "the guard checks the landing name is no symbolic link")
			assert.Equal(t, []string{"rm", "-f", "--", importLanded}, rm.cmd)
		})
	}
}

// blockingRecHost returns an importRecHost whose call key blocks until
// release is closed; entered is closed when it arrives.
func blockingRecHost(key string) (rec *importRecHost, entered, release chan struct{}) {
	rec = newImportRecHost()
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	rec.hooks[key] = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	return rec, entered, release
}

// TestRoutedLandingLock_DeleteThroughAnotherHostWaitsForTheImport: an import
// of a landing name runs through host-b; a DeleteLandedDisk of the same name
// through host-a — the same file on a shared pool — is answered "import in
// flight" and removes nothing.
func TestRoutedLandingLock_DeleteThroughAnotherHostWaitsForTheImport(t *testing.T) {
	f := newImportFixtureOf(t, []string{"host-a", "host-b"})
	recB, entered, release := blockingRecHost("qemu-img convert")
	recA := newImportRecHost()
	f.p.routedDiskConnFn = func(c libvirtConn) libvirtConn {
		if c.HostID() == "host-b" {
			return recB.wrap(c)
		}
		return recA.wrap(c)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
		done <- err
	}()
	<-entered

	_, err := f.server().DeleteLandedDisk(context.Background(), deleteLandedRequest("host-a"))
	assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	assert.Equal(t, landedDiskImportInFlightMessage, status.Convert(err).Message())
	assert.Empty(t, recA.snapshot(), "nothing ran on host-a")

	close(release)
	require.NoError(t, <-done)
}

// TestRoutedLandingLock_CreateThroughAnotherHostWaitsForTheImport: on the
// shared pool, an import of team-a/moved's landing disk runs through host-a;
// the in-place Create of team-a/moved through host-b waits for it and gives
// up retryably, defining nothing.
func TestRoutedLandingLock_CreateThroughAnotherHostWaitsForTheImport(t *testing.T) {
	s := newSharedPool(t)
	s.p.hostDiskTransportFn = anyTransport
	recA, entered, release := blockingRecHost("qemu-img convert")
	recA.pool = s.images
	s.p.routedDiskConnFn = func(c libvirtConn) libvirtConn { return recA.wrap(c) }
	done := make(chan error, 1)
	go func() {
		_, err := NewServer(s.p).ImportDisk(callCtx(t), nfsRequest("host-a"))
		done <- err
	}()
	<-entered

	landed := s.disk(importVolume+qcow2Ext, "imported")
	req := s.createReq(importTarget, landed)
	req.Image.ImportedDisk = true
	req.TargetHostID = "host-b"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.p.Create(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gave up waiting for other work on the migration landing disk")
	for _, call := range s.virshCalls("host-b") {
		assert.NotRegexp(t, `^define `, call, "nothing was defined")
	}
	close(release)
	require.NoError(t, <-done)
}

// TestRoutedImport_SymlinkAtLandingNameIsAGuardRefusal: a symbolic link at the
// landing name is a host-guard refusal (FailedPrecondition +
// VM_OPERATION_FAILED), never the plain AlreadyExists of a name conflict;
// nothing is written. Create, Clone and the single-host import keep their
// Conflict.
func TestRoutedImport_SymlinkAtLandingNameIsAGuardRefusal(t *testing.T) {
	f := newImportFixture(t)
	f.rec.target = targetKindSymlink
	_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	st := status.Convert(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code(), "%v", err)
	assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason))
	assert.Equal(t, fmt.Sprintf(wireRoutedImportLandingSymlinkFmt, "host-b"), st.Message())
	assert.False(t, f.rec.wrote())

	_, cerr := checkWriteTarget(context.Background(), hostConnRunner{conn: f.rec}, "x", importLanded)
	assert.True(t, contracts.IsConflict(cerr), "%v", cerr)
	assert.Equal(t, codes.AlreadyExists, status.Code(definiteStatusError(cerr)))
}
