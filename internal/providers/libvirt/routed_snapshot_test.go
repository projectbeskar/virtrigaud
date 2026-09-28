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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum A slice 3 for the
// snapshot family: SnapshotCreate / Delete / Revert on a clustered provider
// are routed to the leased host (withOwnedDomain), OWNER-CHECKED before
// anything is read or changed, and act on the checked domain by its UUID. The
// fake host is scdFakeTool (snapshot_clone_disk_singlehost_test.go), which
// routes virsh on the per-host local URI (qemu:///<host>).

// routedSCD is a clustered provider over host-a and host-b backed by
// scdFakeTool, with the scd fixture that reads its call log.
type routedSCD struct {
	*scdFixture
	reg    *hostconn.ClusterRegistry
	closes map[string]*atomic.Int32
}

// newRoutedSCD seeds hosts (host -> domain name -> dumpxml document; each
// domain is also addressable by its UUID) and returns the clustered provider
// over host-a and host-b.
func newRoutedSCD(t *testing.T, hosts map[string]map[string]string) *routedSCD {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shims")
	}
	dir := t.TempDir()
	for _, host := range []string{"host-a", "host-b", "local"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, host), 0o700))
	}
	for host, domains := range hosts {
		seedSCDHost(t, filepath.Join(dir, host), domains)
	}
	for _, host := range []string{"host-a", "host-b"} {
		if _, ok := hosts[host]; !ok {
			seedSCDHost(t, filepath.Join(dir, host), nil)
		}
	}
	staging := t.TempDir()
	bin, scd := t.TempDir(), t.TempDir()
	for _, tool := range scdTools {
		require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte(scdFakeTool), 0o755)) //nolint:gosec // test shim must be executable
		require.NoError(t, os.WriteFile(filepath.Join(scd, tool), []byte(scdFakeTool), 0o755)) //nolint:gosec // test shim must be executable
	}
	// The routed guards (routed_budget.go) run flock(1) and timeout(1) on the
	// host: fake them too, so a test never locks a real file. The guard script
	// itself runs in a real shell (against the test's own lock directory under
	// staging); mktemp never creates anything outside the test's directories.
	for tool, script := range map[string]string{
		"flock": routedFakeFlock, "timeout": routedFakeTimeout, "sh": routedGuardShell, "mktemp": routedFakeMktemp,
		"virsh": routedFakeVirsh, "qemu-img": routedFakeQemuImg, "sudo": routedFakeSudo,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	}
	t.Setenv("FAKE_SCD_DIR", dir)
	t.Setenv("FAKE_SCD_TOOLS", scd)
	t.Setenv("FAKE_SCD_BIN", bin)
	t.Setenv("FAKE_SCD_STAGING", staging)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	p, reg, closes := routedCluster(t)
	p.hostStagingDir = staging
	return &routedSCD{scdFixture: &scdFixture{t: t, dir: dir, staging: staging, p: p}, reg: reg, closes: closes}
}

// routedFakeFlock is a fake flock(1) for `flock [-n | -w <s>] -E <code> <lock>
// cmd...`: it logs, exits <code> when local/flock-busy exists (the lock is
// held), 127 when local/flock-missing exists (as a shell does for a missing
// command), and otherwise runs cmd without touching <lock>.
const routedFakeFlock = `#!/bin/sh
printf 'local flock %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
if [ -f "$FAKE_SCD_DIR/local/flock-missing" ]; then echo "sh: flock: not found" >&2; exit 127; fi
code=1
while :; do
  case "$1" in
    -n) shift ;;
    -w|-E) [ "$1" = -E ] && code=$2; shift 2 ;;
    *) break ;;
  esac
done
if [ -f "$FAKE_SCD_DIR/local/flock-busy" ]; then exit "$code"; fi
shift
exec "$@"
`

// routedFakeVirsh is scdFakeTool's virsh, plus `list --all --uuid` (the
// host-wide in-use check lists every domain by UUID): the UUIDs of the host's
// seeded domains.
const routedFakeVirsh = `#!/bin/sh
case "$*" in *"list --all --uuid"*)
  host=local; if [ "$1" = -c ]; then host="${2##*/}"; fi
  printf '%s virsh list --all --uuid\n' "$host" >> "$FAKE_SCD_DIR/calls.log"
  for f in "$FAKE_SCD_DIR/$host"/dom-*-*-*-*-*.xml; do
    [ -f "$f" ] || continue
    n="${f##*/dom-}"; echo "${n%.xml}"
  done
  exit 0 ;;
esac
exec "$FAKE_SCD_TOOLS/virsh" "$@"
`

// routedFakeQemuImg is scdFakeTool's qemu-img, plus `info --backing-chain`
// (the in-use check walks each disk's chain): a one-link chain, the disk
// itself. It reads nothing on the machine running the test.
const routedFakeQemuImg = `#!/bin/sh
case "$*" in *"--backing-chain"*)
  printf 'local qemu-img %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
  for a in "$@"; do disk="$a"; done
  printf '[{"filename": "%s", "format": "qcow2"}]\n' "$disk"
  exit 0 ;;
esac
exec "$FAKE_SCD_TOOLS/qemu-img" "$@"
`

// routedFakeSudo is scdFakeTool's sudo (it only logs), plus `sudo -n qemu-img
// info ... -- <image>` (the in-use check and the disk-dependents guard read
// each disk's chain one image at a time, #358): a standalone qcow2 image,
// unless local/backing-<image base name> names its backing file (a linked
// clone's overlay), or local/fail-qemu-img-info makes the read fail. It reads
// nothing on the machine running the test.
const routedFakeSudo = `#!/bin/sh
case "$*" in "-n qemu-img info "*)
  printf 'local sudo %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
  if [ -f "$FAKE_SCD_DIR/local/fail-qemu-img-info" ]; then echo "qemu-img: Could not open: scripted failure" >&2; exit 1; fi
  for a in "$@"; do img="$a"; done
  b="$FAKE_SCD_DIR/local/backing-${img##*/}"
  if [ -f "$b" ]; then
    bf=$(cat "$b")
    printf '{"filename": "%s", "format": "qcow2", "backing-filename": "%s", "full-backing-filename": "%s", "backing-filename-format": "qcow2"}\n' "$img" "$bf" "$bf"
  else
    printf '{"filename": "%s", "format": "qcow2"}\n' "$img"
  fi
  exit 0 ;;
esac
exec "$FAKE_SCD_TOOLS/sudo" "$@"
`

// routedGuardShell is sh for the routed fixture. The host guard script
// (hostGuardScript, recognized by hostGuardMarker) is logged as
// "local guard <lock dir> <lock> <target>" and run by the real /bin/sh — its
// directory checks are real, against the test's staging directory. withUmask's
// script (umaskExecScript) is logged as "local umask <mask> <command>" and run
// by the real /bin/sh too, so the command it wraps (qemu-img, sudo dd) reaches
// its fake. The disk/varstore target check (targetKindScript) is logged as
// scdFakeTool logs it, and runs for real only on a path inside the test's own
// directory (FAKE_SCD_DIR): a real host path is never inspected. Every other
// sh call is scdFakeTool's.
const routedGuardShell = `#!/bin/sh
case "$2" in
"` + hostGuardMarker + `"*)
  printf 'local guard %s %s %s\n' "$4" "$5" "$6" >> "$FAKE_SCD_DIR/calls.log"
  exec /bin/sh "$@" ;;
'` + umaskExecScript + `')
  shift 3
  printf 'local umask %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
  exec /bin/sh -c '` + umaskExecScript + `' sh "$@" ;;
'` + targetKindScript + `')
  case "$4" in "$FAKE_SCD_DIR"/*)
    printf 'local sh %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
    exec /bin/sh "$@" ;;
  esac ;;
esac
exec "$FAKE_SCD_TOOLS/sh" "$@"
`

// routedFakeMktemp is scdFakeTool's mktemp for a template under the test's own
// directories, and otherwise only logs and prints the path it would make: a
// routed test never creates a file in a real host directory such as
// /var/lib/libvirt/images.
const routedFakeMktemp = `#!/bin/sh
for a in "$@"; do t="$a"; done
case "$t" in
"$FAKE_SCD_DIR"/*|"$FAKE_SCD_STAGING"/*) exec "$FAKE_SCD_TOOLS/mktemp" "$@" ;;
esac
printf 'local mktemp %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
suf=""
for a in "$@"; do case "$a" in --suffix=*) suf="${a#--suffix=}" ;; esac; done
echo "${t%XXXXXXXXXX}0000000000$suf"
`

// routedFakeTimeout is a fake timeout(1) for `timeout --kill-after=<d> <d> cmd...`:
// it logs, exits 124 when local/timeout-expire exists, and otherwise runs cmd.
const routedFakeTimeout = `#!/bin/sh
printf 'local timeout %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
if [ -f "$FAKE_SCD_DIR/local/timeout-expire" ]; then exit 124; fi
shift 2
exec "$@"
`

// seedSCDHost writes one fake host's domain list and definitions.
func seedSCDHost(t *testing.T, hd string, domains map[string]string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(hd, 0o700))
	names := make([]string, 0, len(domains))
	for name := range domains {
		names = append(names, name)
	}
	sort.Strings(names)
	var list strings.Builder
	list.WriteString(" Id   Name                 State\n------------------------------------\n")
	for _, name := range names {
		fmt.Fprintf(&list, " -    %-20s shut off\n", name)
		xml := domains[name]
		require.NoError(t, os.WriteFile(filepath.Join(hd, "dom-"+name+".xml"), []byte(xml), 0o600))
		if d, err := parseDomainLibvirtxml(xml); err == nil && strings.TrimSpace(d.UUID) != "" {
			require.NoError(t, os.WriteFile(filepath.Join(hd, "dom-"+strings.TrimSpace(d.UUID)+".xml"), []byte(xml), 0o600))
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(hd, "list.txt"), []byte(list.String()), 0o600))
}

// ownedWebOnB seeds host-b with "web", owned by ownerTeamA, with snapshots s1
// and s2.
func ownedWebOnB(t *testing.T, xml string) *routedSCD {
	t.Helper()
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": xml}})
	fx.script("host-b", "snaps-"+routingDomainUUID, "s1\ns2\n")
	return fx
}

// teamAOwner is ownerTeamA on the wire.
var teamAOwner = &providerv1.ObjectIdentity{Uid: ownerTeamA.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name}

// snapshotOwnerCheck is the read-only ownership check every routed snapshot
// call starts with.
var snapshotOwnerCheck = []string{"host-b virsh list --all", "host-b virsh dumpxml web"}

// snapshotDependentsGuard is #358's disk-dependents guard, run on the leased
// host with the owner-checked UUID right before a snapshot is created, deleted
// or reverted: the VM's own definition, then every domain on the host (only
// itself here, so no disk chain is read).
var snapshotDependentsGuard = []string{"host-b virsh dumpxml " + routingDomainUUID, "host-b virsh list --all --uuid"}

// scdMutatingSnapshotVerbs change a domain's snapshots or state.
var scdMutatingSnapshotVerbs = []string{"snapshot-create-as", "snapshot-delete", "snapshot-revert"}

func TestClustered_Snapshots_RoutedOwnerCheckedByUUID(t *testing.T) {
	uuid := routingDomainUUID
	webXML := scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		resp, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d",
		})
		require.NoError(t, err)
		assert.Equal(t, "pre", resp.SnapshotId)
		assert.Nil(t, resp.Task, "libvirt snapshots are synchronous")
		assert.Equal(t, append(append(append(append([]string{}, snapshotOwnerCheck...),
			"host-b virsh snapshot-list "+uuid+" --name",
			"host-b virsh domstate "+uuid),
			snapshotDependentsGuard...),
			"host-b virsh snapshot-create-as "+uuid+" pre --description d --atomic --disk-only"), fx.calls(),
			"the name is looked up first (idempotent create), the disk-dependents guard runs, then the snapshot is made")
	})

	t.Run("create of a name this request already made is the earlier attempt's snapshot", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML) // host-b's "web" has snapshots s1 and s2
		answerSnapshotDescription(t, "d "+snapshotRequestTokenPrefix+"uid-snap-1"+snapshotRequestTokenSuffix)
		resp, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "s2", Description: "d", RequestToken: "uid-snap-1",
		})
		require.NoError(t, err)
		assert.Equal(t, "s2", resp.SnapshotId)
		assert.Equal(t, append(append([]string{}, snapshotOwnerCheck...),
			"host-b virsh snapshot-list "+uuid+" --name",
			"host-b virsh snapshot-dumpxml "+uuid+" s2"), fx.calls(), "no second snapshot is made")
	})

	t.Run("create when the name check fails makes nothing", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		fx.script("host-b", "fail-snapshot-list", "")
		_, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d",
		})
		require.Error(t, err)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "snapshot-create-as", "neither answer is assumed: %q", c)
		}
	})

	t.Run("create with memory on a running VM", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		fx.script("host-b", "state", "running\n")
		_, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "mem", Description: "d", IncludeMemory: true,
		})
		require.NoError(t, err)
		assert.Contains(t, fx.calls(), "host-b virsh snapshot-create-as "+uuid+" mem --description d --atomic")
	})

	t.Run("delete", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		resp, err := NewServer(fx.p).SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{
			VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner,
		})
		require.NoError(t, err)
		assert.Nil(t, resp.Task)
		assert.Equal(t, append(append(append(append([]string{}, snapshotOwnerCheck...),
			"host-b virsh snapshot-list "+uuid+" --name"),
			snapshotDependentsGuard...),
			"host-b virsh snapshot-delete "+uuid+" s1"), fx.calls())
	})

	t.Run("revert", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		fx.script("host-b", "state", "running\n")
		_, err := NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
			VmId: "web", SnapshotId: "s2", TargetHostId: "host-b", Owner: teamAOwner,
		})
		require.NoError(t, err)
		assert.Equal(t, append(append(append(append([]string{}, snapshotOwnerCheck...),
			"host-b virsh snapshot-list "+uuid+" --name",
			"host-b virsh domstate "+uuid),
			snapshotDependentsGuard...),
			"host-b virsh snapshot-revert "+uuid+" s2 --force --running"), fx.calls())
	})

	t.Run("lease released and the other host never dialed", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		_, err := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "x", Description: "d",
		})
		require.NoError(t, err)
		assert.Zero(t, fx.closes["host-b"].Load())
		fx.reg.Evict("host-b")
		assert.EqualValues(t, 1, fx.closes["host-b"].Load(), "a leaked lease would keep the connection open")
		assert.Zero(t, fx.closes["host-a"].Load())
		assert.Zero(t, fx.p.virshProvider.unroutableHits.Load(), "nothing reached the single-host placeholder")
	})
}

// TestClustered_Snapshots_OwnerMismatchIsNotFoundAndNothingChanges is the
// slice 3 ownership rule for the snapshot family: a domain whose stamp is
// another tenant's, missing or unreadable — or a request without an owner — is
// answered NotFound (the manager treats it as absent) and none of its
// snapshots is created, deleted or reverted; only the read-only ownership
// check runs.
func TestClustered_Snapshots_OwnerMismatchIsNotFoundAndNothingChanges(t *testing.T) {
	cases := map[string]struct {
		xml   string
		owner *providerv1.ObjectIdentity
	}{
		"owned by another tenant": {scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}),
			&providerv1.ObjectIdentity{Uid: ownerTeamB.UID, Namespace: ownerTeamB.Namespace, Name: ownerTeamB.Name}},
		"unstamped (legacy / foreign)": {scdDomainXML("web", scdDomainOpts{}), teamAOwner},
		"request without owner":        {scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}), nil},
		"unreadable stamp":             {"<domain><name>web", teamAOwner},
		"absent":                       {"", teamAOwner},
	}
	calls := map[string]func(s *Server, owner *providerv1.ObjectIdentity) error{
		"create": func(s *Server, o *providerv1.ObjectIdentity) error {
			_, err := s.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{VmId: "web", TargetHostId: "host-b", Owner: o, NameHint: "x"})
			return err
		},
		"delete": func(s *Server, o *providerv1.ObjectIdentity) error {
			_, err := s.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: o})
			return err
		},
		"revert": func(s *Server, o *providerv1.ObjectIdentity) error {
			_, err := s.SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: o})
			return err
		},
	}
	for caseName, tc := range cases {
		for callName, call := range calls {
			t.Run(caseName+"/"+callName, func(t *testing.T) {
				domains := map[string]string{}
				if tc.xml != "" {
					domains["web"] = tc.xml
				}
				fx := newRoutedSCD(t, map[string]map[string]string{"host-b": domains})
				fx.script("host-b", "snaps-web", "s1\n")
				err := call(NewServer(fx.p), tc.owner)
				require.Error(t, err)
				assert.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
				assert.NotContains(t, err.Error(), ownerTeamA.UID, "the message never discloses the other owner")
				for _, c := range fx.calls() {
					for _, verb := range append([]string{"snapshot-list", "domstate"}, scdMutatingSnapshotVerbs...) {
						assert.NotContains(t, c, " "+verb+" ", "a domain this VM does not own is never read or changed: %q", c)
					}
				}
			})
		}
	}
}

func TestClustered_Snapshots_RoutingRefusals(t *testing.T) {
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})}})
	s := NewServer(fx.p)
	ctx := context.Background()

	// No host: never defaults to one.
	_, err := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "web", Owner: teamAOwner, NameHint: "x"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: "s1", TargetHostId: "  ", Owner: teamAOwner})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// An unknown host is the retryable, host-scoped Unavailable.
	_, err = s.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: "web", SnapshotId: "s1", TargetHostId: "host-zzz", Owner: teamAOwner})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st))

	// A snapshot id this provider could not have created (here: one virsh
	// would read as an option) is refused before any host is touched.
	for _, id := range []string{"--children", "", "a b", strings.Repeat("s", 65)} {
		_, err = s.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: id, TargetHostId: "host-b", Owner: teamAOwner})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "delete %q", id)
		_, err = s.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: "web", SnapshotId: id, TargetHostId: "host-b", Owner: teamAOwner})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "revert %q", id)
	}
	assert.Empty(t, fx.calls(), "no host was touched by a refused request")
	assert.Zero(t, fx.p.virshProvider.unroutableHits.Load())
}

// TestClustered_Snapshots_FailureIsVMScoped: a snapshot command the host
// rejects is a per-VM failure (VM_OPERATION_FAILED), which the manager keeps
// out of its per-Provider circuit breaker.
func TestClustered_Snapshots_FailureIsVMScoped(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.script("host-b", "fail-snapshot-create-as", "")
	_, err := NewServer(fx.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "x", Description: "d",
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st))
	assert.Contains(t, st.Message(), "failed to create snapshot")
}

// TestClustered_SnapshotCreate_WhileAnEarlierJobRunsIsRetryable: a retry that
// reaches the host while the earlier snapshot job still holds the domain's job
// lock is answered retryable (the outcome is still unknown), not failed; the
// retry after that finds the snapshot by name.
func TestClustered_SnapshotCreate_WhileAnEarlierJobRunsIsRetryable(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	script := "#!/bin/sh\ncase \"$*\" in *snapshot-create-as*)\n" +
		"  echo 'error: Timed out during operation: " + libvirtJobBusyMarker +
		" (held by monitor=remoteDispatchDomainSnapshotCreateXML)' >&2; exit 1 ;;\nesac\n" +
		"exec \"$FAKE_SCD_BIN/virsh\" \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := NewServer(fx.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre",
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code(), "got %v", err)
	assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "kept out of the breaker")
	assert.Contains(t, st.Message(), "another operation on this VM is still running")
	assert.NotContains(t, st.Message(), "remoteDispatch", "the cause stays in the provider log")
}
