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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin #358's disk-safety call-site checklist (disk_dependents.go)
// on the ROUTED paths of a clustered provider: the snapshot family refuses
// while another domain on the leased host depends on the VM's disk, and the
// clustered Clone writes its disk and UEFI varstore on #358's hardened path.

// dependentDomain is a domain on host-b whose disk is a linked-clone overlay
// backed by "web"'s disk. Its name must never reach the requester.
const dependentDomain = "dep-7f3a"

// dependentDisk is dependentDomain's own disk (the overlay).
const dependentDisk = "/var/lib/libvirt/images/" + dependentDomain + "-disk.qcow2"

// webWithADependent seeds host-b with "web" (owned by team A, snapshots s1 and
// s2) and dependentDomain, whose disk is backed by web's disk.
func webWithADependent(t *testing.T) *routedSCD {
	t.Helper()
	dep := strings.Replace(scdDomainXML(dependentDomain, scdDomainOpts{}), scdDiskPath, dependentDisk, 1)
	dep = strings.Replace(dep, routingDomainUUID, "33333333-4444-4555-8666-777777777777", 1)
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {
		"web":           scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}),
		dependentDomain: dep,
	}})
	fx.script("host-b", "snaps-"+routingDomainUUID, "s1\ns2\n")
	fx.script("local", "backing-"+filepath.Base(dependentDisk), scdDiskPath)
	return fx
}

// routedSnapshotCalls are the three routed snapshot calls on "web".
func routedSnapshotCalls() map[string]func(context.Context, *Server) error {
	return map[string]func(context.Context, *Server) error{
		"create": func(ctx context.Context, s *Server) error {
			_, err := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d", RequestToken: "uid-snap-1",
			})
			return err
		},
		"create with memory": func(ctx context.Context, s *Server) error {
			_, err := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "mem", Description: "d", IncludeMemory: true,
			})
			return err
		},
		"delete": func(ctx context.Context, s *Server) error {
			_, err := s.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{
				VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner,
			})
			return err
		},
		"revert": func(ctx context.Context, s *Server) error {
			_, err := s.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
				VmId: "web", SnapshotId: "s2", TargetHostId: "host-b", Owner: teamAOwner,
			})
			return err
		},
	}
}

// TestClustered_Snapshots_RefusedWhileADiskHasDependents: a routed
// SnapshotCreate / SnapshotDelete / SnapshotRevert of a VM whose disk backs
// another domain on the leased host (its linked clone, in any namespace) is
// refused before the snapshot command runs: FailedPrecondition with the
// VM_DISK_IN_USE and VM_OPERATION_FAILED ErrorInfos (a per-VM Conflict the
// manager keeps out of its breaker), naming neither the other domain nor a
// host path.
func TestClustered_Snapshots_RefusedWhileADiskHasDependents(t *testing.T) {
	for name, call := range routedSnapshotCalls() {
		t.Run(name, func(t *testing.T) {
			fx := webWithADependent(t)
			fx.script("host-b", "state", "running\n")
			err := call(context.Background(), NewServer(fx.p))
			requireDiskInUseStatus(t, err, true)
			st, _ := status.FromError(err)
			assert.Contains(t, st.Message(), `libvirt domain "web" refused`)
			assert.Contains(t, st.Message(), "1 other domain(s)")
			assert.NotContains(t, st.Message(), dependentDomain, "another domain (maybe another tenant's) is never named")
			assert.NotContains(t, st.Message(), "/var/lib", "no host path on the wire")
			calls := fx.calls()
			assert.Contains(t, calls, "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+dependentDisk,
				"the other domain's disk chain was read on the leased host")
			for _, c := range calls {
				for _, verb := range scdMutatingSnapshotVerbs {
					assert.NotContains(t, c, verb, "nothing is snapshotted, deleted or reverted: %q", c)
				}
				assert.False(t, strings.HasPrefix(c, "host-a "), "only the leased host is read: %q", c)
			}
		})
	}
}

// TestClustered_Snapshots_DependentsCheckFailedIsRetryable: when the guard
// cannot read a disk chain on the leased host, the snapshot call is not made
// and the answer is Unavailable with VM_DISK_CHECK_FAILED and
// VM_OPERATION_FAILED (retried, never counted by the breaker).
func TestClustered_Snapshots_DependentsCheckFailedIsRetryable(t *testing.T) {
	for name, call := range routedSnapshotCalls() {
		t.Run(name, func(t *testing.T) {
			fx := webWithADependent(t)
			fx.script("host-b", "state", "running\n")
			fx.script("local", "fail-qemu-img-info", "")
			err := call(context.Background(), NewServer(fx.p))
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, codes.Unavailable, st.Code(), "got %v", err)
			reasons := errorInfoReasons(st)
			assert.True(t, reasons[contracts.VMDiskCheckFailedReason], "got %v", err)
			assert.True(t, reasons[contracts.VMOperationFailedReason], "kept out of the manager's breaker")
			assert.NotContains(t, st.Message(), "scripted failure", "the cause stays in the provider log")
			for _, c := range fx.calls() {
				for _, verb := range scdMutatingSnapshotVerbs {
					assert.NotContains(t, c, verb, "nothing is changed: %q", c)
				}
			}
		})
	}
}

// uefiWebOnB seeds host-b with a UEFI "web" owned by team A.
func uefiWebOnB(t *testing.T) *routedSCD {
	t.Helper()
	return newRoutedSCD(t, map[string]map[string]string{"host-b": {
		"web": scdDomainXML("web", scdDomainOpts{owner: ownerTeamA, uefi: true}),
	}})
}

// cloneNVRAM is the clone's own varstore, next to the source's.
const cloneNVRAM = "/var/lib/libvirt/qemu/nvram/" + cloneTargetDomain + "_VARS.fd"

// TestClustered_Clone_WritesOnTheHardenedPath pins the clustered clone's disk
// and varstore writes to #358's single-host helpers: the disk is copied with
// the VM-disk umask into a private directory, renamed onto its name with
// `mv -f -T` and given to the qemu user with `chown -h` — never chmod'ed, never
// `cp -f`'ed as root — and the varstore is unlinked, then created by `dd` with
// O_NOFOLLOW on both ends and O_EXCL on the target under a 0600 umask, and
// `chown -h`'ed. Both targets are checked before anything is written.
func TestClustered_Clone_WritesOnTheHardenedPath(t *testing.T) {
	fx := uefiWebOnB(t)
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)
	calls := fx.calls()

	idx := func(want string) int {
		t.Helper()
		for i, c := range calls {
			if c == want {
				return i
			}
		}
		t.Fatalf("no %q in %v", want, calls)
		return -1
	}
	diskCheck := idx("local sh -c " + targetKindScript + " sh " + cloneTargetDisk)
	nvramCheck := idx("local sh -c " + targetKindScript + " sh " + cloneNVRAM)
	writeDir := idx("local mktemp -d /var/lib/libvirt/images/" + vmDiskWriteDirPrefix + mktempTemplateSuffix)
	// The copy runs as root (privileged_copy.go) into a file the SSH user
	// created first, under the VM-disk umask, in the private directory.
	created := idx("local sh -c " + createCopyOutputScript + " sh " + vmDiskUmask + " " + cloneWriteFile)
	copyAt := idx("local " + cloneCopyCmd)
	assert.Less(t, writeDir, created)
	assert.Less(t, created, copyAt, "root writes into the file the SSH user created; it never creates it")
	publish := idx("local mv -f -T -- " + cloneWriteFile + " " + cloneTargetDisk)
	diskChown := idx("local sudo chown -h " + qemuFileOwner + " -- " + cloneTargetDisk)
	dirRemoved := idx("local rm -rf -- " + cloneWriteDir)
	nvramUnlink := idx("local sudo rm -f -- " + cloneNVRAM)
	nvramCopy := idx("local umask " + clonedNVRAMUmask + " sudo dd if=" + scdNVRAM + " of=" + cloneNVRAM +
		" iflag=nofollow oflag=nofollow conv=excl status=none")
	nvramChown := idx("local sudo chown -h " + qemuFileOwner + " -- " + cloneNVRAM)
	define := idx("local virsh define <staging>/" + cloneTargetDomain + domainXMLStagingInfix + "0000000000")

	assert.Less(t, diskCheck, writeDir, "the disk's name is checked before anything is written")
	assert.Less(t, nvramCheck, writeDir, "the varstore path is checked before anything is written (ensureNVRAMTargetFree)")
	assert.Less(t, writeDir, copyAt)
	assert.Less(t, copyAt, publish, "the copy is renamed onto the name only once complete")
	assert.Less(t, publish, diskChown)
	assert.Less(t, diskChown, dirRemoved, "the private directory is removed")
	assert.Less(t, nvramUnlink, nvramCopy, "a stale varstore is unlinked, never truncated")
	assert.Less(t, nvramCopy, nvramChown)
	assert.Less(t, nvramChown, define)
	_, _ = guardedCall(t, calls, cloneLock, cloneCopyAsRoot, cloneCopyCmd)

	for _, c := range calls {
		assert.NotContains(t, c, "chmod 777", "nothing is made world-writable: %q", c)
		assert.False(t, strings.HasPrefix(c, "local sudo chmod"), "no file is chmod'ed as root: %q", c)
		assert.NotContains(t, c, "cp -f", "nothing is copied over a name as root: %q", c)
		if strings.Contains(c, "chown") {
			assert.Contains(t, c, "chown -h", "ownership never follows a symbolic link: %q", c)
		}
	}

	defined := fx.definedXML(cloneTargetDomain)
	assert.Contains(t, defined, "<nvram>"+cloneNVRAM+"</nvram>", "the clone boots from its own varstore")
}

// TestClustered_Clone_RefusesSymlinkedTargets: a symbolic link at the clone's
// disk name, or at its varstore path, is refused (AlreadyExists: the name is
// not usable on this host) before anything is written.
func TestClustered_Clone_RefusesSymlinkedTargets(t *testing.T) {
	t.Run("disk", func(t *testing.T) {
		fx := uefiWebOnB(t)
		pool := filepath.Join(fx.dir, "pool")
		require.NoError(t, os.Mkdir(pool, 0o700))
		answerPoolPath(t, fx.dir, pool)
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "victim"), filepath.Join(pool, cloneTargetDomain+"-disk.qcow2")))

		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assert.Equal(t, codes.AlreadyExists, status.Code(err), "got %v", err)
		assert.Contains(t, err.Error(), "is a symbolic link on the host")
		assertNothingWritten(t, fx.calls())
	})
	t.Run("varstore", func(t *testing.T) {
		fx := uefiWebOnB(t)
		nvramDir := filepath.Join(fx.dir, "nvram")
		require.NoError(t, os.Mkdir(nvramDir, 0o700))
		src := filepath.Join(nvramDir, "web_VARS.fd")
		web := strings.Replace(scdDomainXML("web", scdDomainOpts{owner: ownerTeamA, uefi: true}), scdNVRAM, src, 1)
		seedSCDHost(t, filepath.Join(fx.dir, "host-b"), map[string]string{"web": web})
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "victim"), filepath.Join(nvramDir, cloneTargetDomain+"_VARS.fd")))

		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assert.Equal(t, codes.AlreadyExists, status.Code(err), "got %v", err)
		assert.Contains(t, err.Error(), "UEFI varstore path")
		assert.NotContains(t, err.Error(), fx.dir, "no host path on the wire")
		assertNothingWritten(t, fx.calls())
	})
}

// assertNothingWritten fails if a clone wrote, renamed, chowned or defined
// anything.
func assertNothingWritten(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		for _, verb := range []string{"mktemp -d", "qemu-img convert", "local mv ", " dd ", "chown", " define "} {
			assert.NotContains(t, c, verb, "nothing may be written: %q", c)
		}
	}
}
