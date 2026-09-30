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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the ADR-0007 Slice 5 lab fixes to the libvirt copies:
//
//   - B1: a VM with an external (disk-only) snapshot runs on libvirt's overlay,
//     0600 libvirt-qemu, which the SSH user cannot read. A full clone (single
//     host and clustered) and an s3/nfs export read it as root through
//     `sudo -n` — timeout(1) inside sudo, the flock outside it — and run as
//     the SSH user, with the source format still pinned, only when sudo
//     itself refuses; a source whose chain is not safe is not copied at all;
//   - B2: a full clone of a source that is not shut off is refused before
//     anything is copied (FailedPrecondition + VM_SOURCE_RUNNING).
//
// Everything runs against the fixtures' fake host tools: a fake sudo that
// marks what it runs as "root" (FAKE_AS_ROOT), and a fake qemu-img that
// cannot open the overlay unless run through it. Nothing runs the real sudo,
// and nothing touches /var/lib/libvirt.

// scdOverlayPath is libvirt's external-snapshot overlay of the scd source's
// disk (<disk without extension>.<snapshot>): the source's active disk after
// a disk-only snapshot "snap1".
const scdOverlayPath = "/var/lib/libvirt/images/" + scdDomain + "-disk.snap1"

// overlayDomainXML is scdDomainXML for name whose disk is the snapshot
// overlay.
func overlayDomainXML(name string, o scdDomainOpts) string {
	return strings.Replace(scdDomainXML(name, o), "file='"+scdDiskPath+"'", "file='"+scdOverlayPath+"'", 1)
}

// rootOnlyQemuImg is a qemu-img in front of the fixture's: it cannot open
// $ROOT_ONLY_IMAGE (a 0600 libvirt-qemu overlay) unless it runs through the
// fake sudo (FAKE_AS_ROOT=1), and otherwise hands the call on.
const rootOnlyQemuImg = `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "$ROOT_ONLY_IMAGE" ] && [ "$FAKE_AS_ROOT" != 1 ]; then
    printf 'local qemu-img %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
    echo "qemu-img: Could not open '$a': Could not open '$a': Permission denied" >&2
    exit 1
  fi
done
exec "$NEXT_QEMU_IMG" "$@"
`

// rootMarkingSudo is a sudo in front of the fixture's: whatever it runs runs
// "as root" (FAKE_AS_ROOT=1).
const rootMarkingSudo = `#!/bin/sh
FAKE_AS_ROOT=1
export FAKE_AS_ROOT
exec "$NEXT_SUDO" "$@"
`

// installRootOnlyImage makes image readable only through the fake sudo: it
// puts rootOnlyQemuImg and rootMarkingSudo in front of the fixture's fakes
// (which must already be on PATH — never the real sudo).
func installRootOnlyImage(t *testing.T, image string) {
	t.Helper()
	next := map[string]string{}
	for _, tool := range []string{"qemu-img", "sudo"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.Contains(t, p, os.TempDir(), "the fixture's fake %s must come first on PATH, never the real one", tool)
		require.NotContains(t, p, "virtrigaud-host-guard-", "a fixture's fake %s, not the host guard's shim", tool)
		next[tool] = p
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(rootOnlyQemuImg), 0o755)) //nolint:gosec // test shim must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(rootMarkingSudo), 0o755))     //nolint:gosec // test shim must be executable
	t.Setenv("ROOT_ONLY_IMAGE", image)
	t.Setenv("NEXT_QEMU_IMG", next["qemu-img"])
	t.Setenv("NEXT_SUDO", next["sudo"])
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// scdCloneWriteFile is the private-directory file a single-host clone of the
// scd source into team-a/copy writes.
const scdCloneWriteFile = "/var/lib/libvirt/images/" + vmDiskWriteDirPrefix + "0000000000/team-a.copy-disk.qcow2"

// scdFullClone is a single-host full clone of the scd source into team-a/copy.
var scdFullClone = &providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy", TargetVm: scdTargetVM}

func TestSingleHost_Clone_SnapshotOverlaySourceIsCopiedAsRoot(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: overlayDomainXML(scdDomain, scdDomainOpts{})})
	installRootOnlyImage(t, scdOverlayPath)

	resp, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
	require.NoError(t, err)
	assert.Equal(t, "team-a.copy", resp.TargetVmId)

	calls := fx.calls()
	assert.Contains(t, calls, "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdOverlayPath,
		"the overlay's chain is read (as root) before root copies it")
	assert.Contains(t, calls, "local sh -c "+createCopyOutputScript+" sh "+vmDiskUmask+" "+scdCloneWriteFile,
		"the SSH user creates the output under the VM-disk umask first")
	assert.Contains(t, calls, "local sh -c "+umaskExecScript+" sh "+vmDiskUmask+
		" sudo -n qemu-img convert -f qcow2 -O qcow2 "+scdOverlayPath+" "+scdCloneWriteFile,
		"the copy reads the overlay as root, its format pinned from the definition")
	assert.Contains(t, calls, "local mv -f -T -- "+scdCloneWriteFile+" /var/lib/libvirt/images/team-a.copy-disk.qcow2")
	assert.Contains(t, calls, "local sudo chown -h libvirt-qemu:kvm -- /var/lib/libvirt/images/team-a.copy-disk.qcow2")
	for _, c := range calls {
		assert.NotContains(t, c, "Permission denied")
		assert.False(t, strings.HasPrefix(c, "local sudo chmod"), "no root chmod: %q", c)
	}
	defined := fx.definedXML("team-a.copy")
	assert.Contains(t, defined, "file='/var/lib/libvirt/images/team-a.copy-disk.qcow2'", "the clone is a standalone disk")
	assert.NotContains(t, defined, scdOverlayPath, "the clone never references the source's overlay")
}

func TestSingleHost_Clone_SudoRefusedCopiesAsTheSSHUserWithTheFormatPinned(t *testing.T) {
	t.Run("readable source: copied as the SSH user", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		fx.script("local", "fail-sudo", "")
		_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
		require.NoError(t, err)
		calls := fx.calls()
		assert.Contains(t, calls, "local sudo -n qemu-img convert -f qcow2 -O qcow2 "+scdDiskPath+" "+scdCloneWriteFile, "sudo is asked first")
		assert.Contains(t, calls, "local qemu-img convert -f qcow2 -O qcow2 "+scdDiskPath+" "+scdCloneWriteFile,
			"then the SSH user copies, the source format still pinned: nothing is probed")
		for _, c := range calls {
			assert.NotContains(t, c, "convert -O qcow2", "no copy probes the source's format: %q", c)
		}
	})
	t.Run("0600 overlay: its chain cannot be read, so it is refused, never copied", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: overlayDomainXML(scdDomain, scdDomainOpts{})})
		installRootOnlyImage(t, scdOverlayPath)
		fx.script("local", "fail-sudo", "")
		_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
		st, ok := status.FromError(err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, codes.FailedPrecondition, st.Code(), "a refusal, never counted by the breaker")
		assert.Contains(t, st.Message(), "cannot be copied safely")
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "qemu-img convert", "nothing is copied: %q", c)
		}
		assert.Empty(t, fx.definedXML("team-a.copy"), "nothing is defined")
	})
}

func TestSingleHost_Clone_RunningSourceIsRefusedBeforeAnyCopy(t *testing.T) {
	for _, state := range []string{"running", "paused", "in shutdown", "pmsuspended"} {
		t.Run(state, func(t *testing.T) {
			fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
			fx.script("single", "state", state+"\n")
			_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, codes.FailedPrecondition, st.Code(), "not counted by the breaker")
			assert.Equal(t, []string{contracts.VMSourceRunningReason}, errorInfoReasonList(st))
			assert.Contains(t, st.Message(), `the clone's source VM is "`+state+`"`)
			assert.Contains(t, st.Message(), "power off the source VM to clone it")
			assert.Equal(t, []string{"single virsh domstate " + scdDomain}, fx.calls(), "nothing else runs")
		})
	}
}

func TestClustered_Clone_SnapshotOverlaySourceIsCopiedAsRoot(t *testing.T) {
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": overlayDomainXML("web", scdDomainOpts{owner: ownerTeamA})}})
	fx.script("local", "backing-"+filepath.Base(scdOverlayPath), scdDiskPath)
	installRootOnlyImage(t, scdOverlayPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, err := NewServer(fx.p).Clone(ctx, routedCloneReq())
	require.NoError(t, err)

	calls := fx.calls()
	assert.Contains(t, calls, "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdOverlayPath)
	assert.Contains(t, calls, "local sudo -n qemu-img info -U -f qcow2 --output=json -- "+scdDiskPath,
		"the chain is followed to the base, in the format the overlay's header names")
	line, secs := guardedCall(t, calls, cloneLock, cloneCopyAsRoot, "qemu-img convert -f qcow2 -O qcow2 "+scdOverlayPath+" "+cloneWriteFile)
	assert.Greater(t, secs, 0)
	// The argv, in order: the SSH user's guard and flock hold the lock, then
	// the umask shell, then sudo -n, and timeout(1) INSIDE sudo.
	flock := strings.Index(line, "flock -n -E 75 ")
	sudo := strings.Index(line, "sudo -n ")
	timeout := strings.Index(line, "timeout --kill-after=10s ")
	convert := strings.Index(line, "qemu-img convert")
	assert.True(t, flock >= 0 && flock < sudo && sudo < timeout && timeout < convert, "flock outside sudo, timeout inside: %q", line)
	assert.Contains(t, calls, "local "+"timeout --kill-after=10s "+line[timeout+len("timeout --kill-after=10s "):],
		"timeout(1) runs as root, from sudo")
	assert.Contains(t, calls, "local mv -f -T -- "+cloneWriteFile+" "+cloneTargetDisk)
	for _, c := range calls {
		assert.NotContains(t, c, "Permission denied")
	}
	defined := fx.definedXML(cloneTargetDomain)
	assert.Contains(t, defined, "file='"+cloneTargetDisk+"'")
	assert.NotContains(t, defined, scdOverlayPath)
}

// TestClustered_Clone_SudoRefusedFailsTheOverlayCopyOutsideTheBreaker: sudo
// allows the chain read but refuses the copy; the SSH user's copy (format
// pinned) cannot open the 0600 overlay and fails, outside the breaker.
func TestClustered_Clone_SudoRefusedFailsTheOverlayCopyOutsideTheBreaker(t *testing.T) {
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": overlayDomainXML("web", scdDomainOpts{owner: ownerTeamA})}})
	installRootOnlyImage(t, scdOverlayPath)
	fx.script("local", "fail-sudo", "")

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assertRoutedOp(t, err, codes.Unknown, `failed to clone VM on host "host-b"`)
	st, _ := status.FromError(err)
	assert.NotContains(t, st.Message(), "Permission denied", "the cause stays in the provider log")
	_, _ = guardedCall(t, fx.calls(), cloneLock, "", cloneCopyAsSSHUserCmdFor(scdOverlayPath))
}

// cloneCopyAsSSHUserCmdFor is cloneCopyAsSSHUserCmd for source src.
func cloneCopyAsSSHUserCmdFor(src string) string {
	return strings.Replace(cloneCopyAsSSHUserCmd, scdDiskPath, src, 1)
}

func TestClustered_Clone_RunningSourceIsRefusedBeforeAnyCopy(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("host-b", "state", "running\n")
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	st, ok := status.FromError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.ElementsMatch(t, []string{contracts.VMSourceRunningReason, contracts.VMOperationFailedReason}, errorInfoReasonList(st),
		"marked for the manager, and kept out of its breaker")
	assert.Contains(t, st.Message(), "power off the source VM to clone it")
	assert.NotContains(t, st.Message(), "/var/lib", "no host path on the wire")
	assertNoCopy(t, fx.calls())
	assert.Contains(t, fx.calls(), "host-b virsh domstate "+routingDomainUUID, "the owner-checked source is addressed by its UUID")
}

// TestClustered_Clone_DoneCloneIsReportedWhateverTheSourceState: a retry of a
// clone an earlier attempt completed (its answer lost) is reported as done
// even when the source was powered on since.
func TestClustered_Clone_DoneCloneIsReportedWhateverTheSourceState(t *testing.T) {
	done := strings.Replace(scdDomainXML(cloneTargetDomain, scdDomainOpts{owner: cloneTarget}), routingDomainUUID,
		"99999999-2222-4333-8444-555555555555", 1)
	fx := sourceWeb(t, map[string]string{cloneTargetDomain: done})
	fx.script("host-b", "state", "running\n")
	resp, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)
	assert.Equal(t, cloneTargetDomain, resp.TargetVmId)
	assertNoCopy(t, fx.calls())
}

func TestClustered_Export_SnapshotOverlaySourceIsReadAsRoot(t *testing.T) {
	overlayWeb := func(t *testing.T) *routedSCD {
		fx := ownedWebOnB(t, overlayDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.p.hostDiskTransportFn = anyTransport
		fx.script("local", "backing-"+filepath.Base(scdOverlayPath), scdDiskPath)
		installRootOnlyImage(t, scdOverlayPath)
		return fx
	}
	t.Run("nfs", func(t *testing.T) {
		fx := overlayWeb(t)
		_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
		})
		require.NoError(t, err)
		calls := fx.calls()
		guardedCall(t, calls, exportLock, nfsExportAsRoot,
			"qemu-img convert -U -f qcow2 -O qcow2 "+scdOverlayPath+" nfs://nas/e/web.qcow2?uid="+scdSSHUID+"&gid="+scdSSHGID)
		for _, c := range calls {
			assert.NotContains(t, c, "qemu-img convert -U -f qcow2 -O qcow2 "+scdOverlayPath+" nfs://nas/e/web.qcow2 ",
				"never the SSH user's convert of the overlay")
		}
	})
	t.Run("s3", func(t *testing.T) {
		fx := overlayWeb(t)
		_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "s3",
			DestinationUrl:     "s3://bucket/web.qcow2",
			StorageOptionsJson: `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`,
			Credentials:        map[string]string{"accessKeyID": "a", "secretAccessKey": "s"},
		})
		// The local fakes have no SSH stream: the export fails after the
		// flatten, which ran as root and succeeded.
		require.Error(t, err)
		calls := fx.calls()
		guardedCall(t, calls, exportLock, "sh -c "+umaskExecScript+" sh "+exportStageUmask+" sudo -n ",
			"qemu-img convert -U -f qcow2 -O qcow2 "+scdOverlayPath+" "+s3ExportTemp)
		assert.Contains(t, calls, "local qemu-img convert -U -f qcow2 -O qcow2 "+scdOverlayPath+" "+s3ExportTemp)
		for _, c := range calls {
			assert.NotContains(t, c, "Permission denied", "the overlay was read as root: %q", c)
		}
		assert.Contains(t, calls, "local rm -rf -- "+s3ExportStageDir, "the staging directory is removed")
	})
}

func TestDefinitionDiskFormat(t *testing.T) {
	xml := scdDomainXML(scdDomain, scdDomainOpts{})
	assert.Equal(t, "qcow2", definitionDiskFormat(xml, scdDiskPath))
	assert.Equal(t, "raw", definitionDiskFormat(strings.Replace(xml, "<driver name='qemu' type='qcow2'/>", "<driver name='qemu'/>", 1), scdDiskPath),
		"libvirt opens a file disk without a format as raw; so does the copy")
	assert.Equal(t, "raw", definitionDiskFormat(xml, scdSeedISO), "the CD-ROM's own driver")
	assert.Empty(t, definitionDiskFormat(xml, "/var/lib/libvirt/images/other.qcow2"), "not a disk of the domain")
	assert.Empty(t, definitionDiskFormat("<not-xml", scdDiskPath))
}

// refusedCopy asserts err is a copyRefusedError whose reason contains want.
func refusedCopy(t *testing.T, err error, want string) {
	t.Helper()
	var cr *copyRefusedError
	require.ErrorAs(t, err, &cr)
	assert.Contains(t, cr.reason, want)
}

func TestCheckCopySource(t *testing.T) {
	fx := newRoutedSCD(t, nil)
	h := localHostVP("host-b")
	ctx := context.Background()

	assert.NoError(t, checkCopySource(ctx, h, scdDiskPath, "qcow2"), "a standalone qcow2 in its named format")
	refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "vmdk"), "is not one a copy opens")
	refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, ""), "is not one a copy opens")

	fx.script("local", "fail-qemu-img-info", "")
	refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "qcow2"), "could not be read and verified")
}

func TestCheckCopySource_UnnamedBackingFormat(t *testing.T) {
	// A header that names its backing file without its format would make
	// qemu-img probe the backing file as root: refused.
	info := `#!/bin/sh
printf 'local sudo %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
for a in "$@"; do img="$a"; done
if [ "$img" = "` + scdOverlayPath + `" ]; then
  printf '{"filename": "%s", "format": "qcow2", "backing-filename": "%s", "full-backing-filename": "%s"}\n' "$img" "` + scdDiskPath + `" "` + scdDiskPath + `"
else
  printf '{"filename": "%s", "format": "raw"}\n' "$img"
fi
`
	newRoutedSCD(t, nil) // the fixture's fake host tools, then this sudo in front
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(info), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	refusedCopy(t, checkCopySource(context.Background(), localHostVP("host-b"), scdOverlayPath, "qcow2"), "without its format")
}

func TestNFSURLForRoot(t *testing.T) {
	_ = newRoutedSCD(t, nil) // the fake id: uid scdSSHUID, gid scdSSHGID
	h := localHostVP("host-b")
	ctx := context.Background()
	const dest = "nfs://nas/e/web.qcow2"
	want := dest + "?uid=" + scdSSHUID + "&gid=" + scdSSHGID

	for _, in := range []string{
		dest,
		dest + "?uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		dest + "?gid=" + scdSSHGID + "&uid=" + scdSSHUID,
		dest + "?uid=" + scdSSHUID,
		dest + "?nfsport=2050&mountport=2050&debug=9",
		dest + "?uid=" + scdSSHUID + "&gid=" + scdSSHGID + "&readahead=131072",
		dest + "?uid=" + scdSSHUID + "&uid=" + scdSSHUID,
	} {
		got, err := nfsURLForRoot(ctx, h, in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, "exactly the SSH user's identity and nothing else: %s", in)
	}

	for _, in := range []string{
		dest + "?uid=0",
		dest + "?gid=0",
		dest + "?uid=0&gid=0",
		dest + "?uid=7",
		dest + "?uid=" + scdSSHUID + "&gid=8",
		dest + "?uid=" + scdSSHUID + "&uid=0",
		dest + "?uid=" + scdSSHUID + "#frag",
		dest + "#frag",
		"nfs://nas",
		"nfs:///e/web.qcow2",
		"file:///etc/shadow",
		"nfs://nas/e/web qcow2",
		dest + "?uid=%zz",
	} {
		_, err := nfsURLForRoot(ctx, h, in)
		assert.Error(t, err, "never written as root: %s", in)
	}

	bad := `#!/bin/sh
echo "uid=1000(x)"
`
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "id"), []byte(bad), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := nfsURLForRoot(ctx, h, dest)
	assert.Error(t, err, "an unexpected id output is never put in the URL")
}

// TestClustered_Export_NFSIdentity: an nfs export runs as root only with
// exactly the SSH user's uid and gid; a destination naming another identity
// (a VMMigration's spec.storage.nfs.uid/gid, uid 0 included) is written by
// the SSH user's own qemu-img, with the source format pinned, as before.
func TestClustered_Export_NFSIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		dest, rootURL, sshURL string
	}{
		"no identity named": {
			dest:    "nfs://nas/e/web.qcow2",
			rootURL: "nfs://nas/e/web.qcow2?uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		},
		"other libnfs options dropped": {
			dest:    "nfs://nas/e/web.qcow2?nfsport=2050&debug=9",
			rootURL: "nfs://nas/e/web.qcow2?uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		},
		"the SSH user's identity": {
			dest:    "nfs://nas/e/web.qcow2?gid=" + scdSSHGID + "&uid=" + scdSSHUID,
			rootURL: "nfs://nas/e/web.qcow2?uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		},
		"uid 0": {
			dest:   "nfs://nas/e/web.qcow2?uid=0&gid=0",
			sshURL: "nfs://nas/e/web.qcow2?uid=0&gid=0",
		},
		"another identity": {
			dest:   "nfs://nas/e/web.qcow2?gid=2000&uid=2000",
			sshURL: "nfs://nas/e/web.qcow2?gid=2000&uid=2000",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
			fx.p.hostDiskTransportFn = anyTransport
			_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: tc.dest,
			})
			require.NoError(t, err)
			calls := fx.calls()
			const convert = "qemu-img convert -U -f qcow2 -O qcow2 " + scdDiskPath + " "
			if tc.rootURL != "" {
				guardedCall(t, calls, exportLock, nfsExportAsRoot, convert+tc.rootURL)
				return
			}
			assert.Contains(t, calls, "local "+convert+tc.sshURL, "the SSH user's own qemu-img presents the named identity")
			for _, c := range calls {
				assert.False(t, strings.Contains(c, "sudo -n") && strings.Contains(c, "convert"), "never a root convert: %q", c)
			}
		})
	}
}

func TestApplyPrivileged(t *testing.T) {
	var single *hostCmdGuard
	argv, err := single.applyPrivileged("/pool/t", vmDiskUmask, "qemu-img", "convert", "a b", "c")
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", umaskExecScript, "sh", vmDiskUmask, "sudo", "-n", "qemu-img", "convert", "a b", "c"}, argv,
		"single host: the umask, then sudo -n; no guard and no timeout")

	lock := hostLock{dir: "/s/" + hostLockDirName, name: "clone-ns.vm.lock"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	argv, err = guardFor(ctx, lock).applyPrivileged("/pool/t", vmDiskUmask, "qemu-img", "convert", "a b", "c")
	require.NoError(t, err)
	assert.Equal(t, []string{"sh", "-c", hostGuardScript, "sh", lock.dir, lock.path(), "/pool/t"}, argv[:7])
	assert.Equal(t, []string{"flock", "-n", "-E", "75", lock.path()}, argv[7:12], "the SSH user takes the lock")
	assert.Equal(t, []string{"sh", "-c", umaskExecScript, "sh", vmDiskUmask, "sudo", "-n", "timeout", "--kill-after=10s"}, argv[12:21],
		"then sudo -n, with timeout(1) inside it")
	assert.Equal(t, []string{"qemu-img", "convert", "a b", "c"}, argv[22:])

	argv, err = guardFor(ctx, lock).applyPrivileged("", "", "qemu-img", "convert")
	require.NoError(t, err)
	assert.Equal(t, []string{"sudo", "-n", "timeout", "--kill-after=10s"}, argv[12:16], "no umask: sudo right after the lock")

	short, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_, err = guardFor(short, lock).applyPrivileged("", "", "true")
	var roe *routedOpError
	require.ErrorAs(t, err, &roe, "a budget too short to run anything is a budget failure")
}

// TestSudoRefused_OnlySudosOwnAnswer: a copy falls back to the SSH user only
// when sudo itself refused — its exit status and every stderr line are
// sudo's (or the shell's "sudo: not found"). A qemu-img failure — even one
// whose quoted file name carries a newline and a forged sudo line — is not.
func TestSudoRefused_OnlySudosOwnAnswer(t *testing.T) {
	refused := map[string]*VirshResult{
		"password required": {ExitCode: 1, Stderr: "sudo: a password is required\n"},
		"not allowed": {ExitCode: 1, Stderr: "Sorry, user virtrigaud is not allowed to execute " +
			"'/usr/bin/qemu-img convert -f qcow2 -O qcow2 /p/a /p/b' as root on dome.\n"},
		"may not run sudo":        {ExitCode: 1, Stderr: "Sorry, user virtrigaud may not run sudo on dome.\n"},
		"not in sudoers":          {ExitCode: 1, Stderr: "virtrigaud is not in the sudoers file.\n"},
		"not in sudoers reported": {ExitCode: 1, Stderr: "virtrigaud is not in the sudoers file.  This incident will be reported.\n"},
		"tty required":            {ExitCode: 1, Stderr: "sudo: sorry, you must have a tty to run sudo\n"},
		"not installed (dash)":    {ExitCode: 127, Stderr: "sh: 1: exec: sudo: not found\n"},
		"not installed (bash)":    {ExitCode: 127, Stderr: "bash: line 1: sudo: command not found\n"},
		"not installed (sh)":      {ExitCode: 127, Stderr: "sh: sudo: not found\n"},
	}
	for name, res := range refused {
		assert.True(t, sudoRefused(res), name)
	}
	notRefused := map[string]*VirshResult{
		"nil":             nil,
		"success":         {ExitCode: 0},
		"qemu-img failed": {ExitCode: 1, Stderr: "qemu-img: Could not open '/p/a': Permission denied\n"},
		"forged sudo line in a file name": {ExitCode: 1, Stderr: "qemu-img: Could not open '/p/a\n" +
			"sudo: a password is required\n': No such file or directory\n"},
		"sudo line after qemu-img output": {ExitCode: 1, Stderr: "qemu-img: error while writing\nsudo: a password is required\n"},
		"sudo line with trailing text":    {ExitCode: 1, Stderr: "sudo: a password is required'\n"},
		"not allowed, file name newline": {ExitCode: 1, Stderr: "Sorry, user u is not allowed to execute '/usr/bin/qemu-img info /p/a\n" +
			"b' as root on h.\n"},
		"sudo line, another status":   {ExitCode: 2, Stderr: "sudo: a password is required\n"},
		"no stderr":                   {ExitCode: 1},
		"command not found, not sudo": {ExitCode: 127, Stderr: "sh: 1: flock: not found\n"},
		"timeout missing under sudo":  {ExitCode: 1, Stderr: "sudo: timeout: command not found\n"},
	}
	for name, res := range notRefused {
		assert.False(t, sudoRefused(res), name)
	}
}

// TestSingleHost_Clone_UnsafeChainIsRefusedNotCopiedAsTheSSHUser: a chain the
// check refuses (a backing file named without its format) is never copied —
// not as root, and not as the SSH user either.
func TestSingleHost_Clone_UnsafeChainIsRefusedNotCopiedAsTheSSHUser(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	info := `#!/bin/sh
printf 'local qemu-img %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
for a in "$@"; do img="$a"; done
case "$1" in info)
  if [ "$img" = "` + scdDiskPath + `" ]; then
    printf '{"filename": "%s", "format": "qcow2", "backing-filename": "/p/base", "full-backing-filename": "/p/base"}\n' "$img"
  else
    printf '{"filename": "%s", "format": "raw"}\n' "$img"
  fi ;;
esac
`
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(info), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_SCD_BIN", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
	st, ok := status.FromError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Contains(t, st.Message(), "without its format")
	for _, c := range fx.calls() {
		assert.NotContains(t, c, "qemu-img convert", "nothing is copied, by anyone: %q", c)
	}
}

// errorInfoReasonList returns the ErrorInfo reasons st carries, sorted.
func errorInfoReasonList(st *status.Status) []string {
	var out []string
	for r := range errorInfoReasons(st) {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func TestCloneSourceStateError(t *testing.T) {
	for _, s := range []string{"shut off", "shutoff", " Shut Off \n"} {
		assert.NoError(t, cloneSourceStateError(s), "%q", s)
	}
	for _, s := range []string{"running", "paused", "idle", "crashed", "", "garbage /var/lib/x"} {
		err := cloneSourceStateError(s)
		require.Error(t, err, "%q", s)
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.FailedPrecondition, st.Code())
		assert.Equal(t, []string{contracts.VMSourceRunningReason}, errorInfoReasonList(st))
		assert.NotContains(t, st.Message(), "/var/lib", "only a known state is quoted")
	}
	routed := routedRPCError("clone VM", &hostOpError{host: "host-b", err: cloneSourceStateError("running")})
	st, _ := status.FromError(routed)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.ElementsMatch(t, []string{contracts.VMSourceRunningReason, contracts.VMOperationFailedReason}, errorInfoReasonList(st))
}
