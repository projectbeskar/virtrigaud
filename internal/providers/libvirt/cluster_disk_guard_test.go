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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin ADR-0007 A6.1 on the clustered provider: the cluster-wide
// disk guard (R3) for Create, Clone and Delete, and the previous-incarnation
// refusal (R2), on the target host and through the guard's fan-out.
//
// The shared-pool fixture is a clustered provider over host-a and host-b whose
// storage pool is ONE scratch directory (createHost's images, never the real
// /var/lib/libvirt): the fake virsh answers per host, and every host-shell
// command (sh, realpath, qemu-img through the fake sudo) runs against the same
// scratch filesystem — an NFS pool mounted at the same path on both hosts.

// UUIDs of the domains the guard tests define.
const (
	uuidPrevious = "bbbbbbbb-0000-4000-8000-000000000001"
	uuidForeign  = "bbbbbbbb-0000-4000-8000-000000000002"
	uuidVMOnB    = "bbbbbbbb-0000-4000-8000-000000000003"
)

// ownerForeign is a VirtualMachine of another tenant.
var ownerForeign = contracts.ObjectIdentity{UID: "bbbbbbbb-1111-4000-8000-00000000000f", Namespace: "team-z", Name: "other"}

// sharedPool is the shared-pool fixture (see the file comment).
type sharedPool struct {
	*createHost
	p *Provider
}

// newSharedPool builds the fixture over host-a and host-b.
func newSharedPool(t *testing.T) *sharedPool {
	t.Helper()
	c := newCreateHost(t)
	c.host("host-a")
	c.host("host-b")
	p, _, _ := routedCluster(t)
	p.imageDirs = []string{c.images}
	p.hostStagingDir = c.staging
	return &sharedPool{createHost: c, p: p}
}

// newSharedPoolWithDeadHost is newSharedPool plus host-c, which is in the
// inventory but can never be dialed (a host that is down).
func newSharedPoolWithDeadHost(t *testing.T) *sharedPool {
	t.Helper()
	c := newCreateHost(t)
	c.host("host-a")
	c.host("host-b")
	conns := map[string]*virshConn{
		"host-a": newClusteredVirshConn("host-a", localHostVP("host-a"), nil),
		"host-b": newClusteredVirshConn("host-b", localHostVP("host-b"), nil),
	}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		if vc, ok := conns[h.ID]; ok {
			return vc, nil
		}
		return nil, fmt.Errorf("dial %s: no route to host", h.ID)
	}
	inv := twoHostInventory()
	inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: "host-c", Endpoint: "qemu+ssh://virt@host-c/system"})
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	p.imageDirs = []string{c.images}
	p.hostStagingDir = c.staging
	return &sharedPool{createHost: c, p: p}
}

// guardDomainXML is a definition with one file-backed disk, stamped with owner
// (none when zero); running adds the domain id and the <backingStore/> a
// running domain's definition lists.
func guardDomainXML(name, uuid, disk string, owner contracts.ObjectIdentity, running bool) string {
	id, bs := "", ""
	if running {
		id, bs = " id='7'", "<backingStore/>"
	}
	return fmt.Sprintf("<domain type='kvm'%s><name>%s</name><uuid>%s</uuid>%s<devices>"+
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='%s'/>%s<target dev='vda' bus='virtio'/></disk>"+
		"</devices></domain>", id, name, uuid, renderOwnerMetadataXML(owner), disk, bs)
}

// defineOn defines a domain on host (by name and UUID, and in its `list --all`).
func (s *sharedPool) defineOn(host, name, uuid, xml string) {
	s.t.Helper()
	s.define(host, name, uuid, xml)
	f, err := os.OpenFile(filepath.Join(s.root, host, "names"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(s.t, err)
	_, err = f.WriteString(name + "\n")
	require.NoError(s.t, err)
	require.NoError(s.t, f.Close())
}

// disk writes the file name in the shared pool with content and returns its path.
func (s *sharedPool) disk(name, content string) string {
	s.t.Helper()
	p := filepath.Join(s.images, name)
	require.NoError(s.t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// createOnB is team-a/web's create from a base image, landing on host-b.
func (s *sharedPool) createOnB() (contracts.CreateResponse, error) {
	req := s.createReq(ownerTeamA, s.file(s.images, "ubuntu.qcow2"))
	req.TargetHostID = "host-b"
	return s.p.Create(context.Background(), req)
}

// requireNothingWrittenOnB asserts that a refused create on host-b wrote
// nothing: disk unchanged, no copy, no seed, nothing defined.
func (s *sharedPool) requireNothingWrittenOnB(disk, content string) {
	s.t.Helper()
	b, err := os.ReadFile(disk) //nolint:gosec // test reads its own fixture
	require.NoError(s.t, err)
	assert.Equal(s.t, content, string(b), "the existing disk file is untouched")
	assert.NotContains(s.t, s.log("qemu-img"), "convert", "nothing was copied")
	assert.Empty(s.t, s.stagingEntries(), "no cloud-init seed was prepared")
	for _, call := range s.virshCalls("host-b") {
		assert.NotRegexp(s.t, `^(define|vol-create|undefine|destroy) `, call, "nothing was defined on host-b")
	}
}

// requireReadOnly asserts every virsh call on host was a read.
func (s *sharedPool) requireReadOnly(host string) {
	s.t.Helper()
	for _, call := range s.virshCalls(host) {
		assert.Regexp(s.t, `^(list|dumpxml|pool-dumpxml|vol-path) `, call, "%s is only read", host)
	}
}

// requireWireSafe asserts a tenant-visible message names no host, no path and
// no other domain.
func requireWireSafe(t *testing.T, msg string, images string, others ...string) {
	t.Helper()
	for _, h := range []string{"host-a", "host-c"} {
		assert.NotContains(t, msg, h, "no other host is named")
	}
	assert.NotContains(t, msg, images, "no host path")
	for _, o := range others {
		assert.NotContains(t, msg, o, "never another domain or owner")
	}
}

// TestClusteredCreate_SharedPool_PreviousIncarnationElsewhereIsRefused is the
// bug A6.1 closes: team-a/web was orphaned on host-a (its domain still runs
// there, its disk in the shared pool), and a re-created team-a/web (new UID)
// scheduled on host-b must neither overwrite that disk nor create a second
// domain. The guard's fan-out finds host-a's domain stamped for team-a/web and
// answers VM_PREVIOUS_INCARNATION; nothing is written.
func TestClusteredCreate_SharedPool_PreviousIncarnationElsewhereIsRefused(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			s := newSharedPool(t)
			disk := s.disk("team-a.web-disk.qcow2", "original-disk")
			s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, staleTeamAWeb, running))

			resp, err := s.createOnB()
			require.Error(t, err)
			assert.Empty(t, resp.ID)
			assert.True(t, contracts.IsConflict(err), "never retried as if transient: %v", err)
			var pi *previousIncarnationError
			require.ErrorAs(t, err, &pi)

			st, _ := status.FromError(createRPCError(err))
			assert.Equal(t, codes.AlreadyExists, st.Code())
			assert.True(t, errorInfoReasons(st)[contracts.VMPreviousIncarnationReason], "got %v", st)
			requireWireSafe(t, st.Message(), s.images, staleTeamAWeb.UID)

			s.requireNothingWrittenOnB(disk, "original-disk")
			s.requireReadOnly("host-a")
			assert.Contains(t, s.virshCalls("host-a"), "list --all --uuid", "host-a was scanned")
		})
	}
}

// TestClusteredCreate_SharedPool_ForeignUserIsPlainAlreadyExists: the file is
// used by a domain on host-a that is not a previous incarnation of the VM
// (another tenant's, or unstamped): refused as a plain AlreadyExists — the
// manager's slice 2 exclusion — and nothing is written.
func TestClusteredCreate_SharedPool_ForeignUserIsPlainAlreadyExists(t *testing.T) {
	for name, owner := range map[string]contracts.ObjectIdentity{"another tenant's": ownerForeign, "unstamped": {}} {
		t.Run(name, func(t *testing.T) {
			s := newSharedPool(t)
			disk := s.disk("team-a.web-disk.qcow2", "original-disk")
			s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, disk, owner, true))

			_, err := s.createOnB()
			require.Error(t, err)
			assert.True(t, contracts.IsConflict(err), "%v", err)
			assert.False(t, contracts.IsVMPreviousIncarnation(err))
			var pi *previousIncarnationError
			assert.False(t, stderrors.As(err, &pi), "not a previous incarnation")

			st, _ := status.FromError(createRPCError(err))
			assert.Equal(t, codes.AlreadyExists, st.Code())
			assert.Empty(t, st.Details(), "a plain AlreadyExists: the slice 2 exclusion applies")
			assert.Contains(t, st.Message(), "in use by another domain on a host of this Provider")
			requireWireSafe(t, st.Message(), s.images, "legacy-web", ownerForeign.UID)
			s.requireNothingWrittenOnB(disk, "original-disk")
		})
	}
}

// TestClusteredCreate_SharedPool_UnusedLeftoverIsOverwritten: a file that no
// domain on any host uses is a leftover of an earlier failed attempt and is
// replaced, as on a single host — but only after every host was scanned.
func TestClusteredCreate_SharedPool_UnusedLeftoverIsOverwritten(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "leftover")
	s.defineOn("host-a", "other-vm", uuidForeign, guardDomainXML("other-vm", uuidForeign, s.disk("other-vm-disk.qcow2", "x"), ownerForeign, false))

	resp, err := s.createOnB()
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	assert.Contains(t, s.virshCalls("host-a"), "list --all --uuid", "every host was scanned first")
	s.requireReadOnly("host-a")
	b, err := os.ReadFile(disk) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, "converted\n", string(b), "the leftover was replaced by the new disk")
}

// TestClusteredCreate_NoFileScansNoOtherHost: the common case — nothing where
// the disk goes — is unchanged: no other host is contacted.
func TestClusteredCreate_NoFileScansNoOtherHost(t *testing.T) {
	s := newSharedPool(t)
	_, err := s.createOnB()
	require.NoError(t, err)
	assert.Empty(t, s.virshCalls("host-a"), "no fan-out without a candidate file")
}

// TestClusteredCreate_UnreachableHostFailsClosed: with a file where the disk
// goes and a host of the Provider that cannot be reached, the create fails
// closed — HOST_UNAVAILABLE (retried, never counted by the breaker), naming
// no host — and nothing is written. A host that answers but cannot be scanned
// is VM_DISK_CHECK_FAILED, just as closed.
func TestClusteredCreate_UnreachableHostFailsClosed(t *testing.T) {
	t.Run("host unreachable", func(t *testing.T) {
		s := newSharedPoolWithDeadHost(t)
		disk := s.disk("team-a.web-disk.qcow2", "original-disk")

		_, err := s.createOnB()
		require.Error(t, err)
		assert.True(t, contracts.IsRetryable(err), "%v", err)
		assert.False(t, contracts.IsConflict(err))

		st, _ := status.FromError(createRPCError(err))
		assert.Equal(t, codes.Unavailable, st.Code())
		assert.True(t, hasHostUnavailableInfo(st), "host-scoped: kept out of the circuit breaker (%v)", st)
		assert.Contains(t, st.Message(), "could not be reached")
		requireWireSafe(t, st.Message(), s.images)
		s.requireNothingWrittenOnB(disk, "original-disk")
	})
	t.Run("host cannot be scanned", func(t *testing.T) {
		s := newSharedPool(t)
		disk := s.disk("team-a.web-disk.qcow2", "original-disk")
		s.domain("host-a", uuidForeign, "") // listed, but its definition cannot be read

		_, err := s.createOnB()
		require.Error(t, err)
		st, _ := status.FromError(createRPCError(err))
		assert.Equal(t, codes.Unavailable, st.Code())
		reasons := errorInfoReasons(st)
		assert.True(t, reasons[contracts.VMDiskCheckFailedReason], "got %v", st)
		assert.True(t, reasons[contracts.VMOperationFailedReason], "kept out of the circuit breaker")
		assert.False(t, hasHostUnavailableInfo(st))
		requireWireSafe(t, st.Message(), s.images, uuidForeign)
		s.requireNothingWrittenOnB(disk, "original-disk")
	})
	t.Run("a use already found is definitive", func(t *testing.T) {
		s := newSharedPoolWithDeadHost(t)
		disk := s.disk("team-a.web-disk.qcow2", "original-disk")
		s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, staleTeamAWeb, true))

		_, err := s.createOnB()
		assert.True(t, contracts.IsVMPreviousIncarnation(asWire(createRPCError(err))),
			"the previous incarnation found on a reachable host is the answer, whatever the dead host holds: %v", err)
		s.requireNothingWrittenOnB(disk, "original-disk")
	})
}

// TestClusteredCreate_BlankDiskIsGuardedToo: a VM without an image gets a blank
// volume (vol-create, which never replaces a file); on a clustered provider an
// existing file there is still checked across the hosts, so a previous
// incarnation is answered as such instead of a vol-create failure retried
// forever.
func TestClusteredCreate_BlankDiskIsGuardedToo(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "original-disk")
	s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, staleTeamAWeb, false))

	req := s.createReq(ownerTeamA, "")
	req.Image = contracts.VMImage{}
	req.TargetHostID = "host-b"
	_, err := s.p.Create(context.Background(), req)
	var pi *previousIncarnationError
	require.ErrorAs(t, err, &pi)
	s.requireNothingWrittenOnB(disk, "original-disk")
}

// TestClusteredCreate_EveryDiskNameOfTheVMIsProbed (security review of A6.1):
// a previous incarnation's disk may carry any name the VM's disk could have
// had — blank ("<domain>-disk", no extension), from an image
// ("<domain>-disk.qcow2"), imported ("<domain>-migrated.qcow2"), or the
// legacy bare name's — whatever kind of disk the new create makes. Each is
// found, and the create is held with nothing written.
func TestClusteredCreate_EveryDiskNameOfTheVMIsProbed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		oldDisk  string // the previous incarnation's disk, in the shared pool
		newBlank bool   // the new create makes a blank volume (else: from an image)
	}{
		{"blank incarnation, create from an image", "team-a.web-disk", false},
		{"image incarnation, blank create", "team-a.web-disk.qcow2", true},
		{"imported incarnation, create from an image", "team-a.web-migrated.qcow2", false},
		{"imported incarnation, blank create", "team-a.web-migrated.qcow2", true},
		{"legacy blank incarnation, create from an image", "web-disk", false},
		{"legacy image incarnation, blank create", "web-disk.qcow2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSharedPool(t)
			disk := s.disk(tc.oldDisk, "original-disk")
			s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, staleTeamAWeb, true))

			req := s.createReq(ownerTeamA, s.file(s.images, "ubuntu.qcow2"))
			if tc.newBlank {
				req.Image = contracts.VMImage{}
			}
			req.TargetHostID = "host-b"
			_, err := s.p.Create(context.Background(), req)
			var pi *previousIncarnationError
			require.ErrorAs(t, err, &pi, "the incarnation's %s is found", tc.oldDisk)
			s.requireNothingWrittenOnB(disk, "original-disk")
			for _, made := range []string{"team-a.web-disk", "team-a.web-disk.qcow2"} {
				if made != tc.oldDisk {
					assert.NoFileExists(t, filepath.Join(s.images, made), "no disk was made")
				}
			}
		})
	}
}

// TestClusteredCreate_AnotherNamesForeignUseRefusesNothing: a foreign domain
// that uses a file under ANOTHER of the VM's disk names (not the one this
// create writes) triggers the scan but refuses nothing — that file is not
// written — and the create proceeds.
func TestClusteredCreate_AnotherNamesForeignUseRefusesNothing(t *testing.T) {
	s := newSharedPool(t)
	blank := s.disk("team-a.web-disk", "someone-elses")
	s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, blank, ownerForeign, true))

	resp, err := s.createOnB()
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	assert.Contains(t, s.virshCalls("host-a"), "list --all --uuid", "the existing name triggered the scan")
	b, err := os.ReadFile(blank) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, "someone-elses", string(b), "the other name's file is never touched")
}

// TestClusteredCreate_BlankVolumeFileIsTheOneGuarded: a blank create writes
// <pool>/<domain>-disk (no extension); a foreign domain using THAT file refuses
// the create (plain AlreadyExists) before vol-create runs.
func TestClusteredCreate_BlankVolumeFileIsTheOneGuarded(t *testing.T) {
	s := newSharedPool(t)
	blank := s.disk("team-a.web-disk", "someone-elses")
	s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, blank, ownerForeign, true))

	req := s.createReq(ownerTeamA, "")
	req.Image = contracts.VMImage{}
	req.TargetHostID = "host-b"
	_, err := s.p.Create(context.Background(), req)
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "%v", err)
	assert.False(t, contracts.IsVMPreviousIncarnation(err))
	s.requireNothingWrittenOnB(blank, "someone-elses")
}

// TestClusteredCreate_PathsComparedCanonicallyOnEachHost: host-b reaches the
// pool through a symbolic link (its pool path is an alias), host-a's domain
// names the file by its canonical path. The guard resolves the candidate on
// every host, so the use is found.
func TestClusteredCreate_PathsComparedCanonicallyOnEachHost(t *testing.T) {
	s := newSharedPool(t)
	alias := filepath.Join(s.base, "pool-alias")
	require.NoError(t, os.Symlink(s.images, alias))
	require.NoError(t, os.WriteFile(filepath.Join(s.root, "host-b", "pooldir"), []byte(alias), 0o600))
	disk := s.disk("team-a.web-disk.qcow2", "original-disk")
	s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, disk, ownerForeign, false))

	_, err := s.createOnB()
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "the canonical path matches host-a's reference: %v", err)
	s.requireNothingWrittenOnB(disk, "original-disk")
}

// TestClusteredDelete_DiskUsedOnAnotherHostIsRefused: team-a/web on host-b,
// and a stale definition on host-a running on the same shared disk. The
// delete on host-b is refused before anything is changed (VM_DISK_IN_USE +
// VM_OPERATION_FAILED: the manager keeps the finalizer and never counts it),
// and the file is kept.
func TestClusteredDelete_DiskUsedOnAnotherHostIsRefused(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			s := newSharedPool(t)
			disk := s.disk("team-a.web-disk.qcow2", "live-disk")
			s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
			s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, disk, contracts.ObjectIdentity{}, running))

			_, err := NewServer(s.p).Delete(context.Background(), &providerv1.DeleteRequest{
				Id: "team-a.web", TargetHostId: "host-b", Owner: teamAOwner,
			})
			requireDiskInUseStatus(t, err, true)
			msg := status.Convert(err).Message()
			assert.Contains(t, msg, `delete of libvirt domain "team-a.web" refused`)
			assert.Contains(t, msg, "other hosts of this Provider")
			requireWireSafe(t, msg, s.images, "legacy-web", uuidForeign)

			s.requireUntouched("host-b", "team-a.web")
			assert.FileExists(t, disk, "the shared disk is kept")
			s.requireReadOnly("host-a")
		})
	}
}

// TestClusteredDelete_AliasPathOnAnotherHost: host-a's domain names the shared
// disk through a symbolic link; the canonical comparison on host-a finds it.
func TestClusteredDelete_AliasPathOnAnotherHost(t *testing.T) {
	s := newSharedPool(t)
	alias := filepath.Join(s.base, "pool-alias")
	require.NoError(t, os.Symlink(s.images, alias))
	disk := s.disk("team-a.web-disk.qcow2", "live-disk")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
	s.defineOn("host-a", "legacy-web", uuidForeign,
		guardDomainXML("legacy-web", uuidForeign, filepath.Join(alias, "team-a.web-disk.qcow2"), contracts.ObjectIdentity{}, true))

	_, err := s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
	var de *diskDependentsError
	require.ErrorAs(t, err, &de)
	assert.True(t, de.onOtherHosts)
	s.requireUntouched("host-b", "team-a.web")
}

// TestClusteredDelete_UnreachableHostFailsClosed: a clustered delete with files
// to remove fails closed while a host of the Provider cannot be checked:
// HOST_UNAVAILABLE (kept out of the circuit breaker), the domain intact, the
// disk kept.
func TestClusteredDelete_UnreachableHostFailsClosed(t *testing.T) {
	s := newSharedPoolWithDeadHost(t)
	disk := s.disk("team-a.web-disk.qcow2", "live-disk")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))

	_, err := NewServer(s.p).Delete(context.Background(), &providerv1.DeleteRequest{
		Id: "team-a.web", TargetHostId: "host-b", Owner: teamAOwner,
	})
	st, ok := status.FromError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st), "host-scoped: kept out of the circuit breaker (%v)", st)
	assert.Contains(t, st.Message(), `delete of libvirt domain "team-a.web" not performed`)
	requireWireSafe(t, st.Message(), s.images)
	s.requireUntouched("host-b", "team-a.web")
	assert.FileExists(t, disk)
}

// TestClusteredDelete_NoOtherUseDeletes: with every other host checked and no
// use found, the clustered delete proceeds as before.
func TestClusteredDelete_NoOtherUseDeletes(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "live-disk")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
	s.defineOn("host-a", "other-vm", uuidForeign, guardDomainXML("other-vm", uuidForeign, s.disk("other-vm-disk.qcow2", "x"), ownerForeign, true))

	_, err := s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
	require.NoError(t, err)
	assert.Equal(t, []string{disk}, s.removals())
	s.requireReadOnly("host-a")
}

// TestClusterScan_HostOverTheDomainBoundFailsClosed: a host with more domains
// than a scan reads (clusteredListMaxDomainsPerHost) is not read partially:
// the create fails closed (VM_DISK_CHECK_FAILED) before any definition is read.
func TestClusterScan_HostOverTheDomainBoundFailsClosed(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "original-disk")
	var b strings.Builder
	for i := 0; i <= clusteredListMaxDomainsPerHost; i++ {
		fmt.Fprintf(&b, "cccccccc-0000-4000-8000-%012d\n", i)
	}
	require.NoError(t, os.WriteFile(filepath.Join(s.root, "host-a", "uuids"), []byte(b.String()), 0o600))

	_, err := s.createOnB()
	st, _ := status.FromError(createRPCError(err))
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason], "got %v", st)
	for _, call := range s.virshCalls("host-a") {
		assert.NotContains(t, call, "dumpxml", "no definition is read on a host over the bound")
	}
	s.requireNothingWrittenOnB(disk, "original-disk")
}

// ─── the clustered Clone (routed SCD fixture) ────────────────────────────────

// cloneDiskInPool points host-b's pool at a directory of the fixture and puts
// a file where the clone's disk goes; it returns that path.
func cloneDiskInPool(t *testing.T, fx *routedSCD) string {
	t.Helper()
	pool := filepath.Join(fx.dir, "pool")
	require.NoError(t, os.Mkdir(pool, 0o700))
	answerPoolPath(t, fx.dir, pool)
	target := filepath.Join(pool, cloneTargetDomain+"-disk.qcow2")
	require.NoError(t, os.WriteFile(target, []byte("original-disk"), 0o600))
	return target
}

// previousCopy is a previous incarnation of the clone's target VirtualMachine
// (team-a/copy under another UID).
var previousCopy = contracts.ObjectIdentity{UID: "dddddddd-0000-4000-8000-000000000001", Namespace: cloneTarget.Namespace, Name: cloneTarget.Name}

func TestClusteredClone_PreviousIncarnationOfTheTarget(t *testing.T) {
	t.Run("on another host, using the disk", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		target := cloneDiskInPool(t, fx)
		seedSCDHost(t, filepath.Join(fx.dir, "host-a"), map[string]string{
			cloneTargetDomain: guardDomainXML(cloneTargetDomain, uuidPrevious, target, previousCopy, true),
		})

		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		st, ok := status.FromError(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, codes.AlreadyExists, st.Code(), "got %v", err)
		assert.True(t, errorInfoReasons(st)[contracts.VMPreviousIncarnationReason], "got %v", st)
		assert.Contains(t, st.Message(), "clone of libvirt domain \""+cloneTargetDomain+"\" refused")
		requireWireSafe(t, st.Message(), fx.dir, previousCopy.UID)
		assertNothingWritten(t, fx.calls())
		assert.Contains(t, fx.calls(), "host-a virsh list --all --uuid", "host-a was scanned")
		b, rerr := os.ReadFile(target) //nolint:gosec // test reads its own fixture
		require.NoError(t, rerr)
		assert.Equal(t, "original-disk", string(b))
	})
	t.Run("on the landing host, by name", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{
			cloneTargetDomain: guardDomainXML(cloneTargetDomain, uuidPrevious, "/var/lib/libvirt/images/x.qcow2", previousCopy, false),
		})
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		st, _ := status.FromError(err)
		assert.Equal(t, codes.AlreadyExists, st.Code(), "got %v", err)
		assert.True(t, errorInfoReasons(st)[contracts.VMPreviousIncarnationReason], "got %v", st)
		assertNothingWritten(t, fx.calls())
		for _, c := range fx.calls() {
			assert.False(t, strings.HasPrefix(c, "host-a "), "decided by the name check alone: %q", c)
		}
	})
	t.Run("a foreign same-named domain keeps the slice 2 answer", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{
			cloneTargetDomain: guardDomainXML(cloneTargetDomain, uuidForeign, "/var/lib/libvirt/images/x.qcow2", ownerForeign, false),
		})
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		st, _ := status.FromError(err)
		assert.Equal(t, codes.AlreadyExists, st.Code(), "got %v", err)
		assert.Empty(t, st.Details(), "a plain AlreadyExists: the host is excluded for the target")
	})
}

// TestClusteredClone_BlankIncarnationOfTheTargetIsFound: the clone writes
// "<domain>-disk.qcow2", but a previous incarnation of its target made from a
// blank volume ("<domain>-disk") is found too, and the clone is held.
func TestClusteredClone_BlankIncarnationOfTheTargetIsFound(t *testing.T) {
	fx := sourceWeb(t, nil)
	pool := filepath.Join(fx.dir, "pool")
	require.NoError(t, os.Mkdir(pool, 0o700))
	answerPoolPath(t, fx.dir, pool)
	oldBlank := filepath.Join(pool, cloneTargetDomain+"-disk")
	require.NoError(t, os.WriteFile(oldBlank, []byte("original-disk"), 0o600))
	seedSCDHost(t, filepath.Join(fx.dir, "host-a"), map[string]string{
		cloneTargetDomain: guardDomainXML(cloneTargetDomain, uuidPrevious, oldBlank, previousCopy, true),
	})

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	st, _ := status.FromError(err)
	assert.Equal(t, codes.AlreadyExists, st.Code(), "got %v", err)
	assert.True(t, errorInfoReasons(st)[contracts.VMPreviousIncarnationReason], "got %v", st)
	assertNothingWritten(t, fx.calls())
}

func TestClusteredClone_DiskUsedOnAnotherHostIsRefused(t *testing.T) {
	fx := sourceWeb(t, nil)
	target := cloneDiskInPool(t, fx)
	seedSCDHost(t, filepath.Join(fx.dir, "host-a"), map[string]string{
		"legacy-copy": guardDomainXML("legacy-copy", uuidForeign, target, ownerForeign, true),
	})

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	st, _ := status.FromError(err)
	assert.Equal(t, codes.AlreadyExists, st.Code(), "got %v", err)
	assert.Empty(t, st.Details())
	assert.Contains(t, st.Message(), "in use by another domain on a host of this Provider")
	requireWireSafe(t, st.Message(), fx.dir, "legacy-copy", ownerForeign.UID)
	assertNothingWritten(t, fx.calls())
}

// ─── units ───────────────────────────────────────────────────────────────────

// TestScanHostDiskUse_HostLocalDirsAndSeedDir (security review of A6.1, item
// 7): a candidate in the host-local NVRAM directory is compared on the
// operation's own host only — on another host the same path is another file —
// and the seed directory is resolved again on each scanned host, so a domain
// there that names it by its resolved path is found.
func TestScanHostDiskUse_HostLocalDirsAndSeedDir(t *testing.T) {
	const uuid = "cccccccc-0000-4000-8000-000000000001"
	const nvram = "/var/lib/libvirt/qemu/nvram/team-a.copy_VARS.fd"
	const iso = "/tmp/stage/seed/cloud-init.iso"
	domXML := "<domain id='3'><uuid>" + uuid + "</uuid><os><nvram>" + nvram + "</nvram></os><devices>" +
		"<disk type='file' device='cdrom'><source file='" + iso + "'/><backingStore/></disk></devices></domain>"
	h := &scriptedHost{answers: map[string]*VirshResult{
		"list --all --uuid":                                  {Stdout: uuid + "\n"},
		"dumpxml " + uuid:                                    {Stdout: domXML},
		"! realpath -m -z -- " + nvram + " " + iso:           {Stdout: nvram + "\x00" + iso + "\x00"},
		"! realpath -m -z -- " + nvram + " /stage-link/seed": {Stdout: nvram + "\x00/tmp/stage/seed\x00"},
	}}
	s := clusterScan{files: []string{nvram}, seedDir: "/stage-link/seed"}

	remote, err := scanHostDiskUse(context.Background(), h, s, true)
	require.NoError(t, err)
	assert.Equal(t, []bool{false}, remote.used, "another host's NVRAM directory is another file")
	assert.Zero(t, remote.users)
	assert.True(t, remote.seedUsed, "the seed directory resolved on the scanned host matches its domain's CD-ROM")

	local, err := scanHostDiskUse(context.Background(), h, s, false)
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, local.used, "on the operation's own host the NVRAM path is compared")
	assert.Equal(t, 1, local.users)
}

func TestStampsNameOwner(t *testing.T) {
	assert.True(t, stampsNameOwner([]contracts.ObjectIdentity{staleTeamAWeb}, ownerTeamA), "same namespace and name, another UID")
	assert.True(t, stampsNameOwner([]contracts.ObjectIdentity{ownerTeamA}, ownerTeamA), "the requester's own UID counts too")
	assert.True(t, stampsNameOwner([]contracts.ObjectIdentity{ownerForeign, staleTeamAWeb}, ownerTeamA), "any of several stamps")
	assert.False(t, stampsNameOwner([]contracts.ObjectIdentity{ownerTeamB}, ownerTeamA), "same name, another namespace")
	assert.False(t, stampsNameOwner(nil, ownerTeamA), "unstamped")
	assert.False(t, stampsNameOwner([]contracts.ObjectIdentity{{}}, contracts.ObjectIdentity{UID: "u"}), "an owner without a name matches nothing")
}

func TestGuardHosts(t *testing.T) {
	reg := []hostconn.HostID{"host-a", "host-b"}
	assert.Equal(t, []hostconn.HostID{"host-a", "host-b"}, guardHosts(reg, clusterScan{target: "host-b"}))
	assert.Equal(t, []hostconn.HostID{"host-a"}, guardHosts(reg, clusterScan{target: "host-b", skipTarget: true}), "a Delete skips its own host")
	assert.Equal(t, []hostconn.HostID{"host-a", "host-b", "host-x"}, guardHosts(reg, clusterScan{target: "host-x"}),
		"a landing host that left the registry mid-call is still scanned (over the call's own connection)")
}

// asWire maps a provider wire error back through the manager's reading of
// VM_PREVIOUS_INCARNATION, for a compact assertion.
func asWire(err error) error {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.AlreadyExists || !errorInfoReasons(st)[contracts.VMPreviousIncarnationReason] {
		return err
	}
	return contracts.NewConflictError(st.Message(), fmt.Errorf("%w: %w", contracts.ErrVMPreviousIncarnation, err))
}
