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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the routed time budgets and host-command guards of a
// clustered provider (routed_budget.go): a long routed call answers before the
// manager's deadline with VM_OPERATION_FAILED (kept out of the breaker) instead
// of running into DeadlineExceeded; its qemu-img runs under timeout(1) and
// flock(1); and a retry while an earlier copy still runs is answered "in
// progress" (retryable) instead of starting a second copy.

// cloneTargetDisk is the disk a clone of "web" into team-a/copy writes.
const cloneTargetDisk = "/var/lib/libvirt/images/" + cloneTargetDomain + "-disk.qcow2"

// Locks live in the provider's lock directory under the staging directory
// (normalized to <staging> in the call log), never next to a disk.
const (
	lockDir    = "<staging>/" + hostLockDirName
	cloneLock  = lockDir + "/clone-" + cloneTargetDomain + ".lock"
	exportLock = lockDir + "/export-web.lock"
)

// guardedCall returns the logged `flock ... timeout ... <cmd>` line whose
// command starts with cmd, and the timeout(1) duration it was given.
func guardedCall(t *testing.T, calls []string, lock, cmd string) (string, int) {
	t.Helper()
	prefix := "local flock -n -E 75 " + lock + " timeout --kill-after=10s "
	for _, c := range calls {
		if !strings.HasPrefix(c, prefix) {
			continue
		}
		rest := strings.TrimPrefix(c, prefix)
		secs, after, ok := strings.Cut(rest, "s ")
		require.True(t, ok, c)
		if !strings.HasPrefix(after, cmd) {
			continue
		}
		n, err := strconv.Atoi(secs)
		require.NoError(t, err, c)
		return c, n
	}
	t.Fatalf("no guarded %q under lock %s in %v", cmd, lock, calls)
	return "", 0
}

func TestClustered_Clone_CopyIsGuardedAndBudgeted(t *testing.T) {
	fx := sourceWeb(t, nil)
	// The manager gives Clone 5 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err := NewServer(fx.p).Clone(ctx, routedCloneReq())
	require.NoError(t, err)

	calls := fx.calls()
	_, secs := guardedCall(t, calls, cloneLock, "qemu-img convert -O qcow2 "+scdDiskPath+" "+cloneTargetDisk)
	budget := 5*time.Minute - routedBudgetMargin - hostCommandSlack
	assert.LessOrEqual(t, secs, int(budget/time.Second), "the copy is stopped before the call's budget ends")
	assert.Greater(t, secs, int(budget/time.Second)-10)
	assert.Contains(t, calls, "local guard "+lockDir+" "+cloneLock+" "+cloneTargetDisk,
		"the copy runs through the guard script, which checks the lock directory, the lock and the disk path")
	info, err := os.Stat(filepath.Join(fx.staging, hostLockDirName))
	require.NoError(t, err, "the guard made the lock directory")
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestClustered_Export_CopyIsGuardedUnderTheExportLock(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
	})
	require.NoError(t, err)
	calls := fx.calls()
	guardedCall(t, calls, exportLock, "qemu-img convert -U -f qcow2 -O qcow2 "+scdDiskPath+" nfs://nas/e/web.qcow2")
	assert.Contains(t, calls, "local guard "+lockDir+" "+exportLock+" ", "an nfs export writes no host file: no target to check")
	for _, c := range calls {
		assert.NotContains(t, c, "/var/lib/libvirt/images/.virtrigaud-export-web.lock",
			"the export no longer locks next to the source disk (no write access to its directory is needed)")
	}
}

// TestClustered_GuardRefusesUnsafeLockDirectory: a lock directory that is a
// symbolic link (or not the SSH user's) makes the guard refuse before the copy
// runs: FailedPrecondition, VM_OPERATION_FAILED, no copy and nothing defined.
func TestClustered_GuardRefusesUnsafeLockDirectory(t *testing.T) {
	fx := sourceWeb(t, nil)
	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(fx.staging, hostLockDirName)))

	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assertRoutedOp(t, err, codes.FailedPrecondition, `clone VM on host "host-b" was refused`)
	for _, c := range fx.calls() {
		assert.False(t, strings.HasPrefix(c, "local flock"), "the lock is never taken: %q", c)
		assert.NotContains(t, c, "local qemu-img convert", "nothing is copied: %q", c)
		assert.NotContains(t, c, " define ", "nothing is defined: %q", c)
	}
	entries, err := os.ReadDir(elsewhere)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created through the link")
}

// TestClustered_GuardRefusesSymlinkedLockOrTarget: a lock file, or the clone's
// disk path, that is a symbolic link is refused before the copy.
func TestClustered_GuardRefusesSymlinkedLockOrTarget(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		dir := filepath.Join(fx.staging, hostLockDirName)
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "x"), filepath.Join(dir, "clone-"+cloneTargetDomain+".lock")))
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assertRoutedOp(t, err, codes.FailedPrecondition, "was refused")
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "local qemu-img convert", "nothing is copied: %q", c)
		}
	})
	t.Run("target disk", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		pool := t.TempDir()
		answerPoolPath(t, fx.dir, pool)
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "victim"), filepath.Join(pool, cloneTargetDomain+"-disk.qcow2")))
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assertRoutedOp(t, err, codes.FailedPrecondition, "was refused")
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "local qemu-img convert", "nothing is copied: %q", c)
		}
	})
}

// TestHostGuardRefusalReason: the guard script's messages map to the fixed
// reason labels of the refusal metric (the script is built from the same
// constants).
func TestHostGuardRefusalReason(t *testing.T) {
	for stderr, want := range map[string]string{
		"virtrigaud: " + guardMsgLockDir:       obsmetrics.HostGuardReasonLockDirUnsafe,
		"virtrigaud: " + guardMsgLockSymlink:   obsmetrics.HostGuardReasonLockSymlink,
		"virtrigaud: " + guardMsgTargetSymlink: obsmetrics.HostGuardReasonTargetSymlink,
		"something else":                       obsmetrics.HostGuardReasonUnknown,
	} {
		assert.Equal(t, want, hostGuardRefusalReason(stderr), stderr)
	}
	for _, msg := range []string{guardMsgLockDir, guardMsgLockSymlink, guardMsgTargetSymlink} {
		assert.Contains(t, hostGuardScript, msg, "the script prints the message the reason is read from")
	}
}

// answerPoolPath puts a virsh in front of scdFakeTool that answers
// `pool-dumpxml` with a pool rooted at path (a test directory), and hands
// every other call on.
func answerPoolPath(t *testing.T, fakeDir, path string) {
	t.Helper()
	real, err := exec.LookPath("virsh")
	require.NoError(t, err)
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in pool-dumpxml)\n" +
		"  printf 'local virsh %s\\n' \"$*\" >> \"" + fakeDir + "/calls.log\"\n" +
		"  printf \"<pool type='dir'><name>default</name><target><path>" + path + "</path></target></pool>\\n\"; exit 0 ;;\n" +
		"esac; done\nexec " + real + " \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestWarnIfUnsafeDir: a directory writable by others without the sticky bit
// is warned about once per host and directory; a sticky or private one never.
func TestWarnIfUnsafeDir(t *testing.T) {
	for mode, want := range map[string]bool{"2777": true, "777": true, "1777": false, "3777": false, "755": false, "0711": false} {
		t.Run(mode, func(t *testing.T) {
			h := &statRunner{out: mode + "\n"}
			p := &Provider{}
			assert.Equal(t, want, p.warnIfUnsafeDir(context.Background(), h, "host-a", "/pool"))
			assert.False(t, p.warnIfUnsafeDir(context.Background(), h, "host-a", "/pool"), "once per host and directory")
			assert.Equal(t, 1, h.calls, "the mode is read once")
			assert.Equal(t, want, p.warnIfUnsafeDir(context.Background(), h, "host-b", "/pool"), "another host is checked")
		})
	}
	t.Run("unreadable mode is checked again", func(t *testing.T) {
		h := &statRunner{out: "stat: cannot statx\n"}
		p := &Provider{}
		assert.False(t, p.warnIfUnsafeDir(context.Background(), h, "host-a", "/pool"))
		assert.False(t, p.warnIfUnsafeDir(context.Background(), h, "host-a", "/pool"))
		assert.Equal(t, 2, h.calls)
	})
}

// statRunner answers every host command with out and counts the calls.
type statRunner struct {
	out   string
	calls int
}

func (s *statRunner) runVirshCommand(_ context.Context, args ...string) (*VirshResult, error) {
	s.calls++
	return &VirshResult{Stdout: s.out}, nil
}

// TestClustered_RetryWhileCopyRunsIsInProgress: the lock of the clone's disk
// (or of the source's export) is held by an earlier copy still running: the
// retry is answered retryable "in progress" — no second copy, nothing
// defined — and stays out of the breaker (VM_OPERATION_FAILED).
func TestClustered_RetryWhileCopyRunsIsInProgress(t *testing.T) {
	t.Run("clone", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		fx.script("local", "flock-busy", "")
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assertRoutedOp(t, err, codes.Unavailable, `clone VM on host "host-b": the same copy is still running`)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "local qemu-img", "no second copy starts: %q", c)
			assert.NotContains(t, c, " define ", "nothing is defined: %q", c)
		}
	})
	t.Run("export", func(t *testing.T) {
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.p.hostDiskTransportFn = anyTransport
		fx.script("local", "flock-busy", "")
		_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
		})
		assertRoutedOp(t, err, codes.Unavailable, `export disk on host "host-b": the same copy is still running`)
	})
}

// TestClustered_CopyStoppedByTimeoutIsAFailure: timeout(1) stopped the copy at
// the end of the budget: a failure (not a retry loop), out of the breaker.
func TestClustered_CopyStoppedByTimeoutIsAFailure(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("local", "timeout-expire", "")
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assertRoutedOp(t, err, codes.Unknown, `clone VM on host "host-b" did not finish within its time budget and was stopped`)
}

// TestClustered_MissingHostToolFailsClearly: a host without flock(1) (or
// timeout(1)) never runs the copy unguarded; the call fails naming the tools.
func TestClustered_MissingHostToolFailsClearly(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("local", "flock-missing", "")
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assertRoutedOp(t, err, codes.Unknown, "a required host command is missing")
	for _, c := range fx.calls() {
		assert.False(t, strings.HasPrefix(c, "local qemu-img"), "the copy never runs unguarded: %q", c)
	}
}

// TestClustered_BudgetEndsBeforeTheCallersDeadline: a call whose deadline is
// closer than the margin is answered at once, VM_OPERATION_FAILED — it never
// runs into the caller's DeadlineExceeded, which the manager's breaker counts.
func TestClustered_BudgetEndsBeforeTheCallersDeadline(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	s := NewServer(fx.p)
	calls := map[string]func(ctx context.Context) error{
		"clone VM": func(ctx context.Context) error {
			_, err := s.Clone(ctx, routedCloneReq())
			return err
		},
		"create snapshot": func(ctx context.Context) error {
			_, err := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "x"})
			return err
		},
		"export disk": func(ctx context.Context) error {
			_, err := s.ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
				BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2"})
			return err
		},
	}
	for op, call := range calls {
		t.Run(op, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), routedBudgetMargin/2)
			defer cancel()
			err := call(ctx)
			if op == "create snapshot" {
				// Closing the session does not stop libvirtd's snapshot job:
				// the outcome is unknown, and the answer says retry.
				assertRoutedOp(t, err, codes.Unavailable, `create snapshot on host "host-b" did not answer within its time budget; its outcome is unknown`)
			} else {
				assertRoutedOp(t, err, codes.Unknown, op+` on host "host-b" did not finish within its time budget`)
			}
			assert.NoError(t, ctx.Err(), "answered before the caller's deadline")
		})
	}
}

// assertRoutedOp checks a routed wire error: code, VM_OPERATION_FAILED, and a
// categorized message containing want.
func assertRoutedOp(t *testing.T, err error, code codes.Code, want string) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, code, st.Code(), "got %v", err)
	assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "kept out of the manager's breaker")
	assert.Contains(t, st.Message(), want)
	assert.NotContains(t, st.Message(), "/var/lib", "no host path on the wire")
}

func TestGuardedHostCommand(t *testing.T) {
	lock := hostLock{dir: "/s/" + hostLockDirName, name: "clone-ns.vm.lock"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	argv, err := guardedHostCommand(ctx, lock, "/pool/t", "qemu-img", "convert", "a b", "c")
	require.NoError(t, err)
	require.Len(t, argv, 19)
	assert.Equal(t, []string{"sh", "-c", hostGuardScript, "sh", lock.dir, lock.path(), "/pool/t"}, argv[:7],
		"the guard script's values are positional parameters")
	assert.Equal(t, []string{"flock", "-n", "-E", "75", lock.path(), "timeout", "--kill-after=10s"}, argv[7:14])
	assert.Equal(t, []string{"qemu-img", "convert", "a b", "c"}, argv[15:], "the command's argv is kept as is (the transport quotes it)")

	short, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_, err = guardedHostCommand(short, lock, "", "true")
	var roe *routedOpError
	require.ErrorAs(t, err, &roe, "a budget too short to run anything is a budget failure")

	var none hostCmdGuard
	same, err := none.apply("/pool/t", "qemu-img", "info")
	require.NoError(t, err)
	assert.Equal(t, []string{"qemu-img", "info"}, same, "a nil guard (single-host) leaves the command unchanged")
}

// TestHostLockFor: a lock's name derives only from a domain name this provider
// makes; anything else (a path, an option, an upper-case or empty name) is
// refused, never escaped.
func TestHostLockFor(t *testing.T) {
	p := &Provider{hostStagingDir: "/stage"}
	lock, err := p.hostLockFor(cloneLockKind, "team-a.web")
	require.NoError(t, err)
	assert.Equal(t, "/stage/"+hostLockDirName+"/clone-team-a.web.lock", lock.path())
	_, err = p.hostLockFor(exportLockKind, "team-a.long_0123456789abcdef")
	require.NoError(t, err, "a shortened name's hash separator is allowed")
	for _, bad := range []string{"", "../x", "a/b", "-rf", ".hidden", "Web", "a b", strings.Repeat("a", maxDomainNameBytes+1)} {
		_, err := p.hostLockFor(cloneLockKind, bad)
		assert.Error(t, err, "%q", bad)
	}
}

// TestHostGuardScript runs the guard script in a real shell: it makes a
// missing lock directory 0700 and runs the command; it refuses (exit
// hostGuardRefusedExit, the command not run) a lock directory that is a
// symbolic link, and a lock or target that is a symbolic link.
func TestHostGuardScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	// run runs the script as guardArgv does, with `touch <marker>` in place of
	// flock(1) and the command: the test checks the guard, not the lock.
	run := func(dir, lock, target string) (int, string) {
		marker := filepath.Join(t.TempDir(), "ran")
		cmd := exec.Command("sh", "-c", hostGuardScript, "sh", dir, lock, target, "touch", marker) //nolint:gosec // test runs the fixed guard script
		out, _ := cmd.CombinedOutput()
		ran := "not run"
		if _, err := os.Stat(marker); err == nil {
			ran = "ran"
		}
		return cmd.ProcessState.ExitCode(), ran + " " + string(out)
	}

	base := t.TempDir()
	dir := filepath.Join(base, hostLockDirName)
	code, out := run(dir, filepath.Join(dir, "clone-x.lock"), filepath.Join(base, "disk.qcow2"))
	assert.Equal(t, 0, code, out)
	assert.True(t, strings.HasPrefix(out, "ran"), out)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "a missing lock directory is made 0700")

	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // test loosens its own directory to see it tightened
	code, out = run(dir, filepath.Join(dir, "clone-x.lock"), "")
	assert.Equal(t, 0, code, out)
	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "an existing lock directory of this user is tightened to 0700")

	linkDir := filepath.Join(base, "linked-locks")
	require.NoError(t, os.Symlink(t.TempDir(), linkDir))
	code, out = run(linkDir, filepath.Join(linkDir, "clone-x.lock"), "")
	assert.Equal(t, hostGuardRefusedExit, code, out)
	assert.True(t, strings.HasPrefix(out, "not run"), out)

	require.NoError(t, os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(dir, "clone-y.lock")))
	code, out = run(dir, filepath.Join(dir, "clone-y.lock"), "")
	assert.Equal(t, hostGuardRefusedExit, code, out)
	assert.Contains(t, out, "the lock is a symbolic link")

	target := filepath.Join(base, "linked-disk.qcow2")
	require.NoError(t, os.Symlink(filepath.Join(base, "victim"), target))
	code, out = run(dir, filepath.Join(dir, "clone-x.lock"), target)
	assert.Equal(t, hostGuardRefusedExit, code, out)
	assert.Contains(t, out, "the target is a symbolic link")
	_, err = os.Lstat(filepath.Join(base, "victim"))
	assert.True(t, os.IsNotExist(err), "nothing is written through the link")
}
