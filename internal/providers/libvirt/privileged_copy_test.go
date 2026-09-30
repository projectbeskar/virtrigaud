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
//     `sudo -n` — timeout(1) inside sudo, the flock outside it — and fall back
//     to the historical SSH-user command when sudo refuses;
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

func TestSingleHost_Clone_SudoRefusedRunsTheHistoricalCopy(t *testing.T) {
	t.Run("readable source: copied as the SSH user, as before", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		fx.script("local", "fail-sudo", "")
		_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
		require.NoError(t, err)
		assert.Contains(t, fx.calls(), "local qemu-img convert -O qcow2 "+scdDiskPath+" "+scdCloneWriteFile)
	})
	t.Run("0600 overlay: fails as before", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: overlayDomainXML(scdDomain, scdDomainOpts{})})
		installRootOnlyImage(t, scdOverlayPath)
		fx.script("local", "fail-sudo", "")
		_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Permission denied")
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

func TestCloneSourceFormat(t *testing.T) {
	xml := scdDomainXML(scdDomain, scdDomainOpts{})
	assert.Equal(t, "qcow2", cloneSourceFormat(xml, scdDiskPath))
	assert.Equal(t, "raw", cloneSourceFormat(strings.Replace(xml, "<driver name='qemu' type='qcow2'/>", "<driver name='qemu'/>", 1), scdDiskPath),
		"libvirt opens a file disk without a format as raw; so does the copy")
	assert.Equal(t, "raw", cloneSourceFormat(xml, scdSeedISO), "the CD-ROM's own driver")
	assert.Empty(t, cloneSourceFormat(xml, "/var/lib/libvirt/images/other.qcow2"), "not a disk of the domain")
	assert.Empty(t, cloneSourceFormat("<not-xml", scdDiskPath))
}

func TestPrivilegedCopyRefusal(t *testing.T) {
	fx := newRoutedSCD(t, nil)
	h := localHostVP("host-b")
	ctx := context.Background()

	assert.Empty(t, privilegedCopyRefusal(ctx, h, scdDiskPath, "qcow2"), "a standalone qcow2 in its named format")
	assert.Contains(t, privilegedCopyRefusal(ctx, h, scdDiskPath, "vmdk"), "not one a copy opens as root")
	assert.Contains(t, privilegedCopyRefusal(ctx, h, scdDiskPath, ""), "not one a copy opens as root")

	fx.script("local", "fail-qemu-img-info", "")
	assert.Contains(t, privilegedCopyRefusal(ctx, h, scdDiskPath, "qcow2"), "could not be verified")
}

func TestPrivilegedCopyRefusal_UnnamedBackingFormat(t *testing.T) {
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

	reason := privilegedCopyRefusal(context.Background(), localHostVP("host-b"), scdOverlayPath, "qcow2")
	assert.Contains(t, reason, "without its format")
}

func TestNFSURLWithHostIdentity(t *testing.T) {
	_ = newRoutedSCD(t, nil) // the fake id: uid scdSSHUID, gid scdSSHGID
	h := localHostVP("host-b")
	ctx := context.Background()

	got, err := nfsURLWithHostIdentity(ctx, h, "nfs://nas/e/web.qcow2")
	require.NoError(t, err)
	assert.Equal(t, "nfs://nas/e/web.qcow2?uid="+scdSSHUID+"&gid="+scdSSHGID, got)

	got, err = nfsURLWithHostIdentity(ctx, h, "nfs://nas/e/web.qcow2?uid=7")
	require.NoError(t, err)
	assert.Equal(t, "nfs://nas/e/web.qcow2?uid=7&gid="+scdSSHGID, got, "an identity the URL names is kept")

	got, err = nfsURLWithHostIdentity(ctx, h, "nfs://nas/e/web.qcow2?gid=8&uid=7")
	require.NoError(t, err)
	assert.Equal(t, "nfs://nas/e/web.qcow2?gid=8&uid=7", got, "nothing to add")

	bad := `#!/bin/sh
echo "uid=1000(x)"
`
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "id"), []byte(bad), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = nfsURLWithHostIdentity(ctx, h, "nfs://nas/e/web.qcow2")
	assert.Error(t, err, "an unexpected id output is never put in the URL")
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
