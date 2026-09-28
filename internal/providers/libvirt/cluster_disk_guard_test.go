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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
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

	mu     sync.Mutex
	dialed map[string]int
}

// newSharedPool builds the fixture over host-a and host-b.
func newSharedPool(t *testing.T) *sharedPool {
	t.Helper()
	return newSharedPoolOf(t, []string{"host-a", "host-b"})
}

// newSharedPoolWithDeadHost is newSharedPool plus host-c, which is in the
// inventory but can never be dialed (a host that is down).
func newSharedPoolWithDeadHost(t *testing.T) *sharedPool {
	t.Helper()
	return newSharedPoolOf(t, []string{"host-a", "host-b"}, "host-c")
}

// newSharedPoolOf builds the fixture over the live hosts, plus dead hosts that
// are in the inventory but whose dial always fails. Every dial is counted
// (sharedPool.dials).
func newSharedPoolOf(t *testing.T, live []string, dead ...string) *sharedPool {
	t.Helper()
	c := newCreateHost(t)
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	conns := map[string]*virshConn{}
	for _, h := range live {
		c.host(h)
		conns[h] = newClusteredVirshConn(hostconn.HostID(h), localHostVP(h), nil)
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
	}
	for _, h := range dead {
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
	}
	s := &sharedPool{createHost: c, dialed: map[string]int{}}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		s.mu.Lock()
		s.dialed[h.ID]++
		s.mu.Unlock()
		if vc, ok := conns[h.ID]; ok {
			return vc, nil
		}
		return nil, fmt.Errorf("dial %s: no route to host", h.ID)
	}
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	p.imageDirs = []string{c.images}
	p.hostStagingDir = c.staging
	s.p = p
	return s
}

// dials returns how often host was dialed.
func (s *sharedPool) dials(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialed[host]
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

// TestClusteredCreate_OwnDomainElsewhereIsCountedApart (security review of
// A6.1, item 8): a domain stamped with the requester's OWN UID on another host
// is its own domain (its placement record was lost), not a previous
// incarnation: the create is held the same way, but the answer says so —
// ErrorInfo metadata incarnation=own and its own sentence, naming no host — so
// the manager can point the operator at the pending host and keep a deleted
// VM's finalizer.
func TestClusteredCreate_OwnDomainElsewhereIsCountedApart(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "own-disk")
	s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, ownerTeamA, true))

	_, err := s.createOnB()
	var pi *previousIncarnationError
	require.ErrorAs(t, err, &pi)
	assert.True(t, pi.own)
	st, _ := status.FromError(createRPCError(err))
	assert.Equal(t, codes.AlreadyExists, st.Code())
	var kind string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == contracts.VMPreviousIncarnationReason {
			kind = info.GetMetadata()[contracts.VMPreviousIncarnationKindKey]
		}
	}
	assert.Equal(t, contracts.VMPreviousIncarnationKindOwn, kind)
	assert.Contains(t, st.Message(), "stamped with its own UID")
	assert.Contains(t, st.Message(), "no re-stamp is needed")
	requireWireSafe(t, st.Message(), s.images)
	s.requireNothingWrittenOnB(disk, "own-disk")
}

func TestIncarnationKinds(t *testing.T) {
	for name, tc := range map[string]struct {
		recorded       []contracts.ObjectIdentity
		previous, owns bool
	}{
		"another UID":        {[]contracts.ObjectIdentity{staleTeamAWeb}, true, false},
		"own UID":            {[]contracts.ObjectIdentity{ownerTeamA}, false, true},
		"both stamps":        {[]contracts.ObjectIdentity{ownerTeamA, staleTeamAWeb}, true, true},
		"another namespace":  {[]contracts.ObjectIdentity{ownerTeamB}, false, false},
		"unstamped":          {nil, false, false},
		"another tenant too": {[]contracts.ObjectIdentity{ownerForeign}, false, false},
	} {
		previous, own := incarnationKinds(tc.recorded, ownerTeamA)
		assert.Equal(t, tc.previous, previous, name)
		assert.Equal(t, tc.owns, own, name)
	}
	previous, own := incarnationKinds([]contracts.ObjectIdentity{staleTeamAWeb}, contracts.ObjectIdentity{Namespace: "team-a", Name: "web"})
	assert.True(t, previous, "a requester without a UID never owns one")
	assert.False(t, own)
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
	_, err := s.createFromURLOnB()
	require.NoError(t, err)
	assert.Empty(t, s.virshCalls("host-a"), "no fan-out without a candidate file (and no host-path image to check)")
}

// createFromURLOnB is team-a/web's create from an image URL (downloaded, never
// a host path: no image confinement), landing on host-b.
func (s *sharedPool) createFromURLOnB() (contracts.CreateResponse, error) {
	req := s.createReq(ownerTeamA, "")
	req.Image = contracts.VMImage{URL: "https://images.example/ubuntu.qcow2"}
	req.TargetHostID = "host-b"
	return s.p.Create(context.Background(), req)
}

// TestClusteredCreate_BaseImageIsCheckedOnEveryHost (security review of A6.1,
// item 4): the base image a clustered create copies is checked on every host
// of the Provider, not only the landing one. A tenant's VMImage path that
// names a live disk of a domain on another host is refused — with the same
// answer as a path that does not exist or is not allowed, so it cannot probe
// the pool — and nothing is copied; a host that cannot be checked fails the
// create closed.
func TestClusteredCreate_BaseImageIsCheckedOnEveryHost(t *testing.T) {
	notAllowed := "it does not resolve to a file directly inside an allowed image directory"
	t.Run("a live disk on another host is refused like a missing path", func(t *testing.T) {
		s := newSharedPool(t)
		live := s.disk("legacy.qcow2", "someone-elses-data")
		s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, live, ownerForeign, true))

		req := s.createReq(ownerTeamA, live)
		req.TargetHostID = "host-b"
		_, err := s.p.Create(context.Background(), req)
		require.Error(t, err)
		assert.True(t, isInvalidArgument(err), "%v", err)
		st, _ := status.FromError(createRPCError(err))
		assert.Equal(t, codes.InvalidArgument, st.Code())
		assert.Contains(t, st.Message(), notAllowed)
		// The tenant's own path is echoed; nothing about the other host is.
		for _, leak := range []string{"host-a", "legacy-web", ownerForeign.UID, "in use", "existing VM"} {
			assert.NotContains(t, st.Message(), leak)
		}
		assert.NotContains(t, s.log("qemu-img"), "convert", "nothing was copied")

		missing := s.createReq(ownerTeamA, filepath.Join(s.images, "nope.qcow2"))
		missing.TargetHostID = "host-b"
		_, merr := s.p.Create(context.Background(), missing)
		mst, _ := status.FromError(createRPCError(merr))
		assert.Equal(t, st.Code(), mst.Code())
		assert.Equal(t, strings.ReplaceAll(st.Message(), live, "X"), strings.ReplaceAll(mst.Message(), filepath.Join(s.images, "nope.qcow2"), "X"),
			"a live disk elsewhere and a missing file get the same answer")
	})
	t.Run("an unused base image is copied after every host was checked", func(t *testing.T) {
		s := newSharedPool(t)
		_, err := s.createOnB()
		require.NoError(t, err)
		assert.Contains(t, s.virshCalls("host-a"), "list --all --uuid", "the image was checked on host-a")
		s.requireReadOnly("host-a")
	})
	t.Run("a host that cannot be checked fails the create closed", func(t *testing.T) {
		s := newSharedPoolWithDeadHost(t)
		_, err := s.createOnB()
		var ie *clusterGuardIncompleteError
		require.ErrorAs(t, err, &ie)
		assert.True(t, ie.unreachable)
		assert.NotContains(t, s.log("qemu-img"), "convert")
	})
	t.Run("single-host keeps its distinct answers", func(t *testing.T) {
		c := newCreateHost(t)
		_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, filepath.Join(c.images, "nope.qcow2")))
		assert.Contains(t, err.Error(), "it does not exist on the libvirt host", "unchanged on a single host")
	})
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

// TestClusterScan_UnroutableHostsFailClosed (security review of A6.1, item
// 2a): a host the Provider fronts but the operator could not render (an
// id-only tombstone) exists and may use the shared disk, so the guard fails
// closed on it — HOST_UNAVAILABLE, never dialed, nothing written or removed —
// while a create with nothing where its disk goes is unaffected.
func TestClusterScan_UnroutableHostsFailClosed(t *testing.T) {
	withTombstone := func(t *testing.T) *sharedPool {
		t.Helper()
		s := newSharedPool(t)
		inv := twoHostInventory()
		inv.UnroutableHostIDs = []string{"host-t"}
		require.NoError(t, s.p.clusterReg.Reconcile(inv))
		require.Equal(t, []hostconn.HostID{"host-t"}, s.p.clusterReg.UnroutableHosts())
		return s
	}
	t.Run("delete", func(t *testing.T) {
		s := withTombstone(t)
		disk := s.disk("team-a.web-disk.qcow2", "live-disk")
		s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
		_, err := NewServer(s.p).Delete(context.Background(), &providerv1.DeleteRequest{
			Id: "team-a.web", TargetHostId: "host-b", Owner: teamAOwner,
		})
		st, _ := status.FromError(err)
		assert.Equal(t, codes.Unavailable, st.Code(), "got %v", err)
		assert.True(t, hasHostUnavailableInfo(st))
		assert.NotContains(t, st.Message(), "host-t", "the tombstoned host is not named")
		s.requireUntouched("host-b", "team-a.web")
		assert.Zero(t, s.dials("host-t"), "a tombstone is never dialed")
	})
	t.Run("create over an existing file", func(t *testing.T) {
		s := withTombstone(t)
		disk := s.disk("team-a.web-disk.qcow2", "original-disk")
		_, err := s.createOnB()
		var ie *clusterGuardIncompleteError
		require.ErrorAs(t, err, &ie)
		assert.True(t, ie.unreachable)
		s.requireNothingWrittenOnB(disk, "original-disk")
	})
	t.Run("create from a host-path image", func(t *testing.T) {
		s := withTombstone(t)
		_, err := s.createOnB()
		var ie *clusterGuardIncompleteError
		require.ErrorAs(t, err, &ie, "the base image is checked on every host, the tombstone included")
	})
	t.Run("create from a URL with nothing there", func(t *testing.T) {
		s := withTombstone(t)
		_, err := s.createFromURLOnB()
		require.NoError(t, err, "no candidate file and no host-path image: nothing is scanned")
	})
}

// ─── cost bounds (security review of A6.1, item 3) ──────────────────────────

// TestClusterScan_ShortCircuitsOnceDecided: once a host's answer decides the
// operation, the hosts not yet scanned are never contacted — a Delete on its
// first use (or failure), a Create on its first previous incarnation. With
// one host at a time, host-a decides and host-c is never read.
func TestClusterScan_ShortCircuitsOnceDecided(t *testing.T) {
	t.Run("delete: the first use refuses it", func(t *testing.T) {
		s := newSharedPoolOf(t, []string{"host-a", "host-b", "host-c"})
		s.p.listHostConcurrency = 1
		disk := s.disk("team-a.web-disk.qcow2", "live-disk")
		s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
		s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, disk, contracts.ObjectIdentity{}, true))

		_, err := s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		var de *diskDependentsError
		require.ErrorAs(t, err, &de)
		assert.Empty(t, s.virshCalls("host-c"), "decided on host-a: host-c is never scanned")
		s.requireUntouched("host-b", "team-a.web")
	})
	t.Run("delete: the first failure refuses it", func(t *testing.T) {
		s := newSharedPoolOf(t, []string{"host-b", "host-c"}, "host-a")
		s.p.listHostConcurrency = 1
		disk := s.disk("team-a.web-disk.qcow2", "live-disk")
		s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))

		_, err := s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		var ie *clusterGuardIncompleteError
		require.ErrorAs(t, err, &ie)
		assert.True(t, ie.unreachable)
		assert.Empty(t, s.virshCalls("host-c"), "decided on host-a's failure: host-c is never scanned")
	})
	t.Run("create: the first previous incarnation holds it", func(t *testing.T) {
		s := newSharedPoolOf(t, []string{"host-a", "host-b", "host-c"})
		s.p.listHostConcurrency = 1
		disk := s.disk("team-a.web-disk.qcow2", "original-disk")
		s.defineOn("host-a", "team-a.web", uuidPrevious, guardDomainXML("team-a.web", uuidPrevious, disk, staleTeamAWeb, true))

		_, err := s.createOnB()
		var pi *previousIncarnationError
		require.ErrorAs(t, err, &pi)
		assert.Empty(t, s.virshCalls("host-c"), "decided on host-a: host-c is never scanned")
		s.requireNothingWrittenOnB(disk, "original-disk")
	})
}

// TestClusterScan_KnownUnreachableHostIsNotDialedAgain: a host whose dial just
// failed is failed at once by the next scans (fail closed, HOST_UNAVAILABLE)
// without being dialed again, so a dead host does not cost every retry its
// dial timeout.
func TestClusterScan_KnownUnreachableHostIsNotDialedAgain(t *testing.T) {
	s := newSharedPoolWithDeadHost(t)
	disk := s.disk("team-a.web-disk.qcow2", "live-disk")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))

	for i := 0; i < 3; i++ {
		_, err := s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		var ie *clusterGuardIncompleteError
		require.ErrorAs(t, err, &ie, "attempt %d", i)
		assert.True(t, ie.unreachable, "attempt %d", i)
	}
	assert.Equal(t, 1, s.dials("host-c"), "only the first scan dialed the dead host")
	s.requireUntouched("host-b", "team-a.web")
}

// TestClusterScan_BusyProviderFailsClosed: while the provider's scan slots are
// all taken, a scan that gets none within its budget fails closed as busy
// (VM_DISK_CHECK_FAILED: retried, never counted by the breaker) and nothing is
// written.
func TestClusterScan_BusyProviderFailsClosed(t *testing.T) {
	s := newSharedPool(t)
	disk := s.disk("team-a.web-disk.qcow2", "original-disk")
	require.NoError(t, s.p.guardSemaphore().Acquire(context.Background(), clusterGuardConcurrency))
	defer s.p.guardSemaphore().Release(clusterGuardConcurrency)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := s.createReq(ownerTeamA, s.file(s.images, "ubuntu.qcow2"))
	req.TargetHostID = "host-b"
	_, err := s.p.Create(ctx, req)
	var ie *clusterGuardIncompleteError
	require.ErrorAs(t, err, &ie)
	assert.True(t, ie.busy)
	st, _ := status.FromError(createRPCError(err))
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason], "got %v", st)
	assert.Contains(t, st.Message(), "busy")
	assert.Empty(t, s.virshCalls("host-a"), "no host was scanned")
	s.requireNothingWrittenOnB(disk, "original-disk")
}

// TestClustered_DomainLockSerializesCheckAndAct (security review of A6.1,
// item 5): a clustered Create or Delete of a domain whose lock another
// operation holds (an earlier attempt still running in the provider) waits
// for it — within its budget — instead of checking and acting next to it; one
// that gets no lock in time is not performed (Unavailable +
// VM_OPERATION_FAILED: retried, never counted by the breaker).
func TestClustered_DomainLockSerializesCheckAndAct(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		s := newSharedPool(t)
		unlock, err := s.p.lockDomain(context.Background(), "team-a.web", "test")
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req := s.createReq(ownerTeamA, s.file(s.images, "ubuntu.qcow2"))
		req.TargetHostID = "host-b"
		_, err = s.p.Create(ctx, req)
		var be *domainBusyError
		require.ErrorAs(t, err, &be)
		st, _ := status.FromError(createRPCError(err))
		assert.Equal(t, codes.Unavailable, st.Code())
		assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st))
		assert.Empty(t, s.virshCalls("host-b"), "nothing was checked or written while the lock was held")

		unlock()
		_, err = s.p.Create(context.Background(), req)
		require.NoError(t, err, "once released, the create runs")
	})
	t.Run("delete", func(t *testing.T) {
		s := newSharedPool(t)
		disk := s.disk("team-a.web-disk.qcow2", "live-disk")
		s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
		unlock, err := s.p.lockDomain(context.Background(), "team-a.web", "test")
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err = s.p.Delete(ctx, contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		var be *domainBusyError
		require.ErrorAs(t, err, &be)
		s.requireUntouched("host-b", "team-a.web")

		unlock()
		_, err = s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		require.NoError(t, err)
		assert.Equal(t, []string{disk}, s.removals())
	})
	// A6.1 fix verification, N1: the finalizer's Delete of a VM whose Create
	// is still running (it addresses the VM by its bare name, no status.id
	// yet) takes the domain's lock BEFORE its owner check. It waits for the
	// Create, then finds and removes the domain the Create defined — instead
	// of finding nothing yet, answering NotFound (which releases the
	// finalizer) and leaving that domain orphaned.
	t.Run("finalizer delete racing a create", func(t *testing.T) {
		s := newSharedPool(t)
		disk := filepath.Join(s.images, "team-a.web-disk.qcow2")
		unlockCreate, err := s.p.lockDomain(context.Background(), "team-a.web", "test")
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, derr := s.p.Delete(ctx, contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA})
			done <- derr
		}()
		require.Eventually(t, func() bool { return s.p.domainLocks.refs("team-a.web") == 2 }, 10*time.Second, time.Millisecond,
			"the Delete waits for the Create's lock")
		assert.Empty(t, s.virshCalls("host-b"), "nothing was looked up while the Create held the lock")

		// The Create completes: its disk is written and its domain defined.
		require.NoError(t, os.WriteFile(disk, []byte("new-disk"), 0o600))
		s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, disk, ownerTeamA, false))
		unlockCreate()

		require.NoError(t, <-done, "the Delete found the domain the Create defined")
		assert.Equal(t, []string{disk}, s.removals(), "and removed it with its disk")
		assert.Zero(t, s.p.domainLocks.refs("team-a.web"))
	})
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
