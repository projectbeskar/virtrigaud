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
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// Time budgets and host-command guards for the long routed calls of a
// CLUSTERED provider — Clone, SnapshotCreate and ExportDisk (security review
// of ADR-0007 Addendum A, slice 3).
//
// The manager bounds each of these calls (Clone and SnapshotCreate 5 minutes,
// ExportDisk 30 minutes). If the provider were still working when that
// deadline passed, the manager would see DeadlineExceeded — which counts
// toward its per-Provider circuit breaker — and its retry could start a second
// qemu-img on the same target while the first still ran on the host
// (cancelling a call only closes the SSH session; the host process lives on).
// So, on a clustered provider:
//
//   - (a) the call runs under a budget that ends routedBudgetMargin before the
//     caller's deadline; a budget that runs out is answered as a
//     VM_OPERATION_FAILED failure, which the manager keeps out of its breaker;
//   - (b) a long host command (the qemu-img that copies a clone's disk or
//     flattens an export) runs under timeout(1), so it is stopped on the host
//     when the budget runs out, not merely abandoned;
//   - (c) the same command runs under flock(1) on a lock that belongs to what
//     it writes (the clone's disk, the source's export), so a retry while an
//     earlier copy still runs is answered "in progress" (retryable) instead of
//     starting a second copy.
//
// The locks are separate files in a VirtRigaud-owned lock directory
// (hostLockDirName under the host staging directory, mode 0700, owned by the
// SSH user), never the disk file itself: on an NFS-backed pool flock(2) is
// emulated as a whole-file POSIX lock that would conflict with qemu's own
// image locks, and an export would otherwise need write access to the source
// disk's directory just to take its lock. Every guarded command runs through
// hostGuardScript, which refuses a lock directory that is a symbolic link or
// not owned by the SSH user, and a lock or target file that is a symbolic
// link.
//
// The commands keep the argv-safe transport (every element shell-quoted). A
// host without flock(1) or timeout(1) (util-linux, coreutils) makes the call
// fail with a clear message — it never runs the copy unguarded. The
// single-host path has no budget and no guard.
//
// A disk copy that reads the source as root (privileged_copy.go) keeps the
// guard as it is: the guard script and flock(1) run as the SSH user, which
// holds the lock outside sudo, and timeout(1) runs inside sudo
// (hostCmdGuard.applyPrivileged), so it can stop and kill the root qemu-img.
//
// virsh snapshot-create-as is bounded by the budget but not wrapped: killing
// the virsh client would not stop libvirtd's snapshot job, and libvirt's
// per-domain job lock already refuses a second concurrent snapshot job. So a
// SnapshotCreate whose budget runs out is answered "outcome unknown, retry"
// (classifySnapshotCreateFailure), never "stopped", and the routed create is
// idempotent per request (existingRoutedSnapshot, routed_snapshot_token.go).

const (
	// routedBudgetMargin is how long before the caller's deadline a routed
	// call's budget ends, so the answer reaches the manager in time.
	routedBudgetMargin = 15 * time.Second

	// defaultRoutedBudget bounds a routed call whose context has no deadline
	// (the manager always sets one; direct callers and tests may not).
	defaultRoutedBudget = 30 * time.Minute

	// hostCommandSlack is how long before the budget ends timeout(1) stops a
	// guarded host command, so its exit status (not the cancelled session)
	// reports the overrun.
	hostCommandSlack = 5 * time.Second

	// hostCommandKillAfter is timeout(1)'s --kill-after: a command that
	// ignores SIGTERM is killed this long after it.
	hostCommandKillAfter = "10s"

	// routedCleanupTimeout bounds the cleanup of a routed call that failed
	// (removing a clone's disk, an export's temporary copy). The cleanup runs
	// even when the call's budget ran out (context.WithoutCancel) and ends
	// within routedBudgetMargin, so the answer still reaches the manager
	// before its deadline.
	routedCleanupTimeout = 8 * time.Second

	// routedCleanupLockWait is how long (flock -w, seconds) the removal of a
	// failed clone's disk waits for the clone's lock: a copy that timeout(1)
	// just stopped may take a few seconds to exit and release it. If the lock
	// is still held after that, another attempt owns the disk and it is left.
	routedCleanupLockWait = "6"

	// flockBusyExit is the exit status flock(1) returns when the lock is held
	// (-E): the same copy is still running.
	flockBusyExit = 75

	// hostGuardRefusedExit is the exit status of hostGuardScript when it
	// refuses to run the command: the lock directory is not a directory owned
	// by the SSH user, or the lock or the target is a symbolic link.
	hostGuardRefusedExit = 79

	// Exit statuses of timeout(1): the command timed out (124), or had to be
	// killed (128+9); a command that could not be found (127) or run (126).
	timeoutExpiredExit = 124
	timeoutKilledExit  = 137
	commandNotFound    = 127
	commandNotRunnable = 126

	// hostLockDirName is the directory, under the host staging directory, that
	// holds the routed guards' lock files. The first guarded command makes it
	// with mode 0700; it must be owned by the SSH user.
	hostLockDirName = "virtrigaud-locks"

	// cloneLockKind and exportLockKind name the lock kinds: a lock file is
	// "<kind>-<domain>.lock" (the clone's target domain, the export's source).
	cloneLockKind  = "clone"
	exportLockKind = "export"
)

// hostGuardMarker is the first line of hostGuardScript: a comment for the
// shell, and what lets a test shim recognize the script.
const hostGuardMarker = "# virtrigaud-host-guard"

// hostGuardScript is the fixed `sh -c` script every guarded host command runs
// through. Every value is a positional parameter, never interpolated into the
// text: $1 the lock directory, $2 the lock file, $3 the host file the command
// writes ("" when it writes none), then the command itself. It makes the lock
// directory (mode 0700) when it is missing and refuses to run the command
// (exit hostGuardRefusedExit) unless the directory is a directory — not a
// symbolic link — owned by the user it runs as; it refuses a lock or a target
// that is a symbolic link. Then it execs the command, argv unchanged.
var hostGuardScript = hostGuardMarker + `
d=$1 l=$2 t=$3
shift 3
mkdir -m 0700 -- "$d" 2>/dev/null
if [ -L "$d" ] || [ ! -d "$d" ] || [ ! -O "$d" ]; then
  echo "virtrigaud: ` + guardMsgLockDir + `" >&2
  exit ` + strconv.Itoa(hostGuardRefusedExit) + `
fi
chmod 0700 -- "$d" || exit ` + strconv.Itoa(hostGuardRefusedExit) + `
if [ -L "$l" ]; then
  echo "virtrigaud: ` + guardMsgLockSymlink + `" >&2
  exit ` + strconv.Itoa(hostGuardRefusedExit) + `
fi
if [ -n "$t" ] && [ -L "$t" ]; then
  echo "virtrigaud: ` + guardMsgTargetSymlink + `" >&2
  exit ` + strconv.Itoa(hostGuardRefusedExit) + `
fi
exec "$@"
`

// withRoutedBudget returns ctx bounded to end routedBudgetMargin before its
// deadline (defaultRoutedBudget from now when it has none).
func withRoutedBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithTimeout(ctx, defaultRoutedBudget)
	}
	return context.WithDeadline(ctx, deadline.Add(-routedBudgetMargin))
}

// hostLock is one guard lock on a host: a file in the lock directory.
type hostLock struct {
	// dir is the lock directory (hostLockDirName under the staging directory).
	dir string
	// name is the lock file's name, "<kind>-<domain>.lock".
	name string
}

// path is the lock file's path on the host.
func (l hostLock) path() string { return filepath.Join(l.dir, l.name) }

// lockDomainRE is the shape a domain name must have to name a lock: the
// alphabet of every domain name this provider makes (DNS-1123 names,
// "<namespace>.<name>", and a shortened name's '_' hash separator), starting
// with a letter or digit. It never contains '/'.
var lockDomainRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// hostLockFor returns the lock of kind for domain. The lock's name derives
// only from the domain name, which must be one this provider makes: anything
// else is refused, never escaped.
func (p *Provider) hostLockFor(kind, domain string) (hostLock, error) {
	if len(domain) > maxDomainNameBytes || !lockDomainRE.MatchString(domain) {
		return hostLock{}, fmt.Errorf("domain name %q cannot name a host lock", domain)
	}
	return hostLock{dir: filepath.Join(p.stagingDir(), hostLockDirName), name: kind + "-" + domain + ".lock"}, nil
}

// guardArgv returns argv run through hostGuardScript and flock(1) on lock with
// flockArgs (-n, or -w <seconds>) and the busy exit status flockBusyExit.
// target is the host file the command writes ("" for none).
func guardArgv(lock hostLock, target string, flockArgs []string, argv []string) []string {
	out := make([]string, 0, len(argv)+len(flockArgs)+11)
	out = append(out, "sh", "-c", hostGuardScript, "sh", lock.dir, lock.path(), target, "flock")
	out = append(out, flockArgs...)
	out = append(out, "-E", strconv.Itoa(flockBusyExit), lock.path())
	return append(out, argv...)
}

// guardedHostCommand wraps argv — a long host command writing target ("" when
// it writes no host file) — in hostGuardScript, flock(1) on lock
// (non-blocking: a held lock exits flockBusyExit) and timeout(1) set to end
// hostCommandSlack before ctx's deadline. A budget already too short to run
// anything is a budget failure.
func guardedHostCommand(ctx context.Context, lock hostLock, target string, argv ...string) ([]string, error) {
	timeout, err := hostCommandTimeout(ctx)
	if err != nil {
		return nil, err
	}
	return guardArgv(lock, target, []string{"-n"}, append(timeout, argv...)), nil
}

// hostCommandTimeout returns the timeout(1) argv prefix of a guarded host
// command: stopped hostCommandSlack before ctx's deadline (defaultRoutedBudget
// when it has none), killed hostCommandKillAfter later. A budget already too
// short to run anything is a budget failure.
func hostCommandTimeout(ctx context.Context) ([]string, error) {
	secs := int64(defaultRoutedBudget / time.Second)
	if deadline, ok := ctx.Deadline(); ok {
		secs = int64((time.Until(deadline) - hostCommandSlack) / time.Second)
	}
	if secs < 1 {
		return nil, &routedOpError{code: codes.Unknown, wire: "the operation's time budget ran out before it could start",
			cause: context.DeadlineExceeded}
	}
	return []string{"timeout", "--kill-after=" + hostCommandKillAfter, strconv.FormatInt(secs, 10) + "s"}, nil
}

// hostCmdGuard is the guard of a long host command on a clustered host:
// flock(1) on lock and timeout(1) within ctx's budget (guardedHostCommand). A
// nil guard is the single-host path: apply leaves argv unchanged (its
// historical commands), and applyPrivileged adds sudo but no guard and no
// timeout.
type hostCmdGuard struct {
	// ctx bounds the command (its deadline sets timeout(1)).
	ctx context.Context
	// lock is the flock(1) lock the command runs under.
	lock hostLock
}

// guardFor returns the hostCmdGuard that runs a command under flock(1) on lock
// and timeout(1) within ctx's budget.
func guardFor(ctx context.Context, lock hostLock) *hostCmdGuard {
	return &hostCmdGuard{ctx: ctx, lock: lock}
}

// apply returns argv wrapped by g — the guard script, flock(1), timeout(1),
// then argv — or argv itself when g is nil. target is the host file the
// command writes ("" for none).
func (g *hostCmdGuard) apply(target string, argv ...string) ([]string, error) {
	if g == nil {
		return argv, nil
	}
	return guardedHostCommand(g.ctx, g.lock, target, argv...)
}

// applyPrivileged returns argv run as root through passwordless sudo under
// umask ("" for none), wrapped by g:
//
//	sh -c <hostGuardScript> … flock -n -E 75 <lock> sh -c <umaskExecScript> sh <umask> sudo -n timeout --kill-after=10s <N>s <argv>
//
// The guard script and flock(1) run as the SSH user, which holds the lock
// outside sudo; timeout(1) runs INSIDE sudo, so it can stop (and kill) the
// root command when the budget runs out. A nil g — the single-host path — is
// `sh -c <umaskExecScript> sh <umask> sudo -n <argv>`: no guard, no timeout.
func (g *hostCmdGuard) applyPrivileged(target, umask string, argv ...string) ([]string, error) {
	if g == nil {
		return privilegedArgv(umask, nil, argv), nil
	}
	timeout, err := hostCommandTimeout(g.ctx)
	if err != nil {
		return nil, err
	}
	return guardArgv(g.lock, target, []string{"-n"}, privilegedArgv(umask, timeout, argv)), nil
}

// privilegedArgv returns `sudo -n <timeout...> <argv...>`, run under umask
// (withUmask) unless umask is "". sudo keeps the caller's umask (it applies
// the union of it and its own), and -n makes it refuse at once, never prompt,
// when no passwordless rule allows the command.
func privilegedArgv(umask string, timeout, argv []string) []string {
	cmd := make([]string, 0, 2+len(timeout)+len(argv))
	cmd = append(cmd, "sudo", sudoNonInteractive)
	cmd = append(cmd, timeout...)
	cmd = append(cmd, argv...)
	if umask == "" {
		return cmd
	}
	return withUmask(umask, cmd...)
}

// classifyRoutedFailure turns the failure of a budgeted, guarded routed call
// (op on host) into its categorized wire error:
//
//   - the budget ran out while the caller still waits, or timeout(1) stopped
//     the command: a failure (codes.Unknown) saying the operation was stopped;
//   - flock(1) found the lock held: retryable (codes.Unavailable) — the same
//     copy is still running from an earlier attempt;
//   - the guard refused an unsafe lock directory, lock or target: a failure
//     (codes.FailedPrecondition) until the host is fixed;
//   - a guard or qemu-img could not be found or run on the host: a failure
//     naming the host tools it needs.
//
// Anything else is returned unchanged. All of them are VM_OPERATION_FAILED on
// the wire (routedRPCError), so none counts toward the manager's breaker.
func classifyRoutedFailure(op, host string, parent, budget context.Context, err error) error {
	if err == nil {
		return nil
	}
	stopped := func() error {
		return &routedOpError{code: codes.Unknown, cause: err, wire: fmt.Sprintf(
			"%s on host %q did not finish within its time budget and was stopped", op, host)}
	}
	if budgetRanOut(parent, budget) {
		return stopped()
	}
	switch guardExitCode(err) {
	case flockBusyExit:
		return &routedOpError{code: codes.Unavailable, cause: err, wire: fmt.Sprintf(
			"%s on host %q: the same copy is still running from an earlier attempt; retry later", op, host)}
	case hostGuardRefusedExit:
		reportHostGuardRefusal(op, host, err)
		return &routedOpError{code: codes.FailedPrecondition, cause: err, wire: fmt.Sprintf(
			"%s on host %q was refused: the provider's lock directory on the host is not a directory owned by "+
				"its SSH user, or a lock or target file is a symbolic link", op, host)}
	case timeoutExpiredExit, timeoutKilledExit:
		return stopped()
	case commandNotFound, commandNotRunnable:
		return &routedOpError{code: codes.Unknown, cause: err, wire: fmt.Sprintf(
			"%s on host %q: a required host command is missing or not runnable (flock from util-linux, timeout from coreutils, qemu-img)", op, host)}
	}
	return err
}

// Guard messages (hostGuardScript's stderr) that identify why it refused.
const (
	guardMsgLockDir       = "the lock directory is not a directory owned by this user"
	guardMsgLockSymlink   = "the lock is a symbolic link"
	guardMsgTargetSymlink = "the target is a symbolic link"
)

// hostGuardRefusalReason maps the guard script's stderr to the (fixed) reason
// label of the refusal metric.
func hostGuardRefusalReason(stderr string) string {
	switch {
	case strings.Contains(stderr, guardMsgLockDir):
		return obsmetrics.HostGuardReasonLockDirUnsafe
	case strings.Contains(stderr, guardMsgLockSymlink):
		return obsmetrics.HostGuardReasonLockSymlink
	case strings.Contains(stderr, guardMsgTargetSymlink):
		return obsmetrics.HostGuardReasonTargetSymlink
	}
	return obsmetrics.HostGuardReasonUnknown
}

// reportHostGuardRefusal counts a host-guard refusal
// (virtrigaud_provider_host_guard_refusals_total) and logs it as an ERROR for
// administrators: a host path VirtRigaud writes to was tampered with or
// pre-created by another local user. The log names the host and the reason,
// never a tenant.
func reportHostGuardRefusal(op, host string, err error) {
	reason := obsmetrics.HostGuardReasonUnknown
	var ve *VirshError
	if stderrors.As(err, &ve) {
		reason = hostGuardRefusalReason(ve.Stderr)
	}
	obsmetrics.RecordHostGuardRefusal(libvirtProviderType, reason)
	log.Printf("ERROR ALERT host guard refused %s on host %q (%s): check the provider's lock directory %s "+
		"and the pool directories on that host", op, host, reason, hostLockDirName)
}

// budgetRanOut reports whether the routed budget ran out while the caller
// (parent) still waits for the answer.
func budgetRanOut(parent, budget context.Context) bool {
	return stderrors.Is(budget.Err(), context.DeadlineExceeded) && parent.Err() == nil
}

// guardExitCode returns the exit status of a failed host command in err, or
// -1 when err is not a command's exit.
func guardExitCode(err error) int {
	var ve *VirshError
	if stderrors.As(err, &ve) {
		return ve.ExitCode
	}
	return -1
}
