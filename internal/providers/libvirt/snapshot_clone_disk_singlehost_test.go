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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// This file pins D9 for the RPCs ADR-0007 Addendum A slice 3 routes: on a
// SINGLE-HOST provider SnapshotCreate / SnapshotDelete / SnapshotRevert, Clone,
// GetDiskInfo, ExportDisk and TaskStatus — driven through the gRPC Server, the
// path production serves — emit exactly the virsh / host command sequence,
// return exactly the response and error, and (Clone) define exactly the domain
// XML they do without slice 3. The expected results live in
// testdata/single_host_snapshot_clone_disk.golden.json, captured by running
// this test file against origin/main — first at 392d79a, then again at df4ec4f
// for #358's single-host changes (disk-dependents guard, linked clones
// refused, the private-directory write path, the varstore dd), and again for
// the ADR-0007 Slice 5 lab fixes to the clone copy (B1: the copy reads the
// source as root through `sudo -n` after verifying its chain, the SSH user's
// historical copy when sudo refuses; B2: a source that is not shut off is
// refused) — with VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN=1; the slice 3 branch
// reproduces it byte for byte. A change to any of them — which would restart
// the ADR-0008 D5 soak window — fails here.

// scdGoldenFile is the golden single-host snapshot / clone / disk results.
const scdGoldenFile = "testdata/single_host_snapshot_clone_disk.golden.json"

// scdDomain is the source domain every scenario acts on.
const scdDomain = "vrgold-web"

// scdDiskPath is its primary disk; scdSeedISO its cloud-init CD-ROM.
const (
	scdDiskPath = "/var/lib/libvirt/images/" + scdDomain + "-disk.qcow2"
	scdSeedISO  = "/tmp/virtrigaud-cloudinit-" + scdDomain + ".abcdefghij/cloud-init.iso"
	scdNVRAM    = "/var/lib/libvirt/qemu/nvram/" + scdDomain + "_VARS.fd"
)

// scdFakeTool is a scriptable fake installed under every tool name the
// single-host RPCs run (virsh and the host-shell commands). Every call is
// logged as "<host> <tool> <args>" — the host is the -c URI path for virsh,
// "local" otherwise. Per host, fail-<subcommand or tool> makes it fail, "state"
// holds the domstate answer (default "shut off"), snaps-<domain> the snapshot
// list, and dom-<name>.xml marks a domain as present (`list --all --uuid`
// prints the UUID of every domain seeded under its UUID, as #358's disk
// guards read the host). mktemp is deterministic
// (the X run becomes 0000000000) and creates what it names only inside the
// test's own directories (FAKE_SCD_DIR, FAKE_SCD_STAGING) — never in a real
// host directory such as /var/lib/libvirt/images; sh answers "no such path"
// for every host existence check and runs withUmask's fixed script
// (umaskExecScript) for real, so the command it wraps (qemu-img, sudo dd)
// reaches its fake; `sudo -n <cmd>` (the privileged disk copy and its chain
// read, privileged_copy.go) runs <cmd>'s fake as the test user — or, when
// local/fail-sudo exists, refuses as sudo does without a passwordless rule —
// while any other sudo only logs; every other host tool (mv included: it
// never moves a real file) only logs.
const scdFakeTool = `#!/bin/sh
tool=$(basename "$0")
host=local
if [ "$tool" = virsh ] && [ "$1" = "-c" ]; then host="${2##*/}"; shift 2; fi
printf '%s %s %s\n' "$host" "$tool" "$*" >> "$FAKE_SCD_DIR/calls.log"
d="$FAKE_SCD_DIR/$host"
fail() { if [ -f "$d/fail-$1" ]; then echo "error: scripted failure of $1" >&2; exit 1; fi; }
nodom() { echo "error: failed to get domain '$1'" >&2; exit 1; }
case "$tool" in
virsh)
  case "$1" in
    list)
      if [ "$*" = "list --all --uuid" ]; then
        for f in "$d"/dom-*-*-*-*-*.xml; do [ -f "$f" ] || continue; n="${f##*/dom-}"; echo "${n%.xml}"; done
      else
        cat "$d/list.txt"
      fi ;;
    dumpxml) fail dumpxml; if [ -f "$d/dom-$2.xml" ]; then cat "$d/dom-$2.xml"; else nodom "$2"; fi ;;
    domstate)
      fail domstate
      [ -f "$d/dom-$2.xml" ] || nodom "$2"
      if [ -f "$d/state" ]; then cat "$d/state"; else echo "shut off"; fi ;;
    snapshot-list)
      fail snapshot-list
      [ -f "$d/dom-$2.xml" ] || nodom "$2"
      if [ -f "$d/snaps-$2" ]; then cat "$d/snaps-$2"; fi ;;
    snapshot-create-as) fail snapshot-create-as; [ -f "$d/dom-$2.xml" ] || nodom "$2"; echo "Domain snapshot $3 created" ;;
    snapshot-delete) fail snapshot-delete; [ -f "$d/dom-$2.xml" ] || nodom "$2"; echo "Domain snapshot $3 deleted" ;;
    snapshot-revert) fail snapshot-revert; [ -f "$d/dom-$2.xml" ] || nodom "$2" ;;
    vol-info) fail vol-info; printf 'Name:           %s\nType:           file\nCapacity:       10.00 GiB\nAllocation:     1.00 GiB\n' "$2" ;;
    vol-path) echo "/var/lib/libvirt/images/$2.qcow2" ;;
    pool-info) fail pool-info; printf 'Name:           default\nState:          running\nAutostart:      yes\n' ;;
    pool-dumpxml) printf "<pool type='dir'><name>default</name><target><path>/var/lib/libvirt/images</path></target></pool>\n" ;;
    pool-refresh) exit 0 ;;
    define) fail define; echo "Domain defined from $2" ;;
    *) echo "fake virsh: unsupported: $*" >&2; exit 1 ;;
  esac ;;
mktemp)
  isdir=0; t=""; suf=""
  for a in "$@"; do
    case "$a" in
      -d) isdir=1 ;;
      --suffix=*) suf="${a#--suffix=}" ;;
      *) t="$a" ;;
    esac
  done
  p="${t%XXXXXXXXXX}0000000000$suf"
  mk=0
  case "$p" in "$FAKE_SCD_DIR"/*) mk=1 ;; esac
  if [ -n "$FAKE_SCD_STAGING" ]; then case "$p" in "$FAKE_SCD_STAGING"/*) mk=1 ;; esac; fi
  if [ "$mk" = 1 ]; then if [ "$isdir" = 1 ]; then mkdir -p "$p"; else : > "$p"; fi; fi
  echo "$p" ;;
qemu-img)
  fail qemu-img
  if [ "$1" = info ]; then printf '{"virtual-size": 10737418240, "actual-size": 1073741824, "format": "qcow2"}\n'; fi ;;
sh)
  case "$2" in '` + umaskExecScript + `') exec /bin/sh "$@" ;; esac ;;
sudo)
  if [ "$1" = "-n" ]; then
    if [ -f "$d/fail-sudo" ]; then echo "sudo: a password is required" >&2; exit 1; fi
    shift
    exec "$@"
  fi ;;
id)
  case "$1" in -u) echo ` + scdSSHUID + ` ;; -g) echo ` + scdSSHGID + ` ;; esac ;;
*) exit 0 ;;
esac
`

// scdSSHUID and scdSSHGID are the SSH user's uid and gid the fake id prints.
const (
	scdSSHUID = "1001"
	scdSSHGID = "1002"
)

// scdTools are the names scdFakeTool is installed under.
var scdTools = []string{"virsh", "mktemp", "qemu-img", "sh", "sudo", "cp", "mv", "chmod", "chown", "rm", "restorecon", "stat", "sha256sum", "id"}

// scdFixture is one scenario's fake host: a single-host Provider (with the
// registry seam the Server's snapshot RPCs use) on qemu:///single.
type scdFixture struct {
	t       *testing.T
	dir     string
	staging string
	p       *Provider
}

// scdDomainOpts shapes the source domain's XML.
type scdDomainOpts struct {
	owner contracts.ObjectIdentity
	uefi  bool
}

// scdDomainXML is a `virsh dumpxml` document for name: one file-backed disk,
// the cloud-init CD-ROM, one NIC, and optionally an owner stamp and a UEFI
// varstore.
func scdDomainXML(name string, o scdDomainOpts) string {
	osXML := "  <os>\n    <type arch='x86_64' machine='pc-q35-8.2'>hvm</type>\n  </os>\n"
	if o.uefi {
		osXML = "  <os>\n    <type arch='x86_64' machine='pc-q35-8.2'>hvm</type>\n" +
			"    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader>\n" +
			"    <nvram>" + scdNVRAM + "</nvram>\n  </os>\n"
	}
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n%s"+
		"  <memory unit='KiB'>2097152</memory>\n  <currentMemory unit='KiB'>2097152</currentMemory>\n"+
		"  <vcpu placement='static'>2</vcpu>\n%s"+
		"  <devices>\n"+
		"    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n"+
		"      <source file='%s'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n"+
		"    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n"+
		"      <source file='%s'/>\n      <target dev='sda' bus='sata'/>\n      <readonly/>\n    </disk>\n"+
		"    <interface type='network'>\n      <mac address='52:54:00:12:34:56'/>\n"+
		"      <source network='default'/>\n      <model type='virtio'/>\n    </interface>\n"+
		"  </devices>\n</domain>\n",
		name, routingDomainUUID, renderOwnerMetadataXML(o.owner), osXML, scdDiskPath, scdSeedISO)
}

// newSCDFixture installs scdFakeTool and seeds host "single" with domains
// (name -> dumpxml document; each is also addressable by its UUID).
func newSCDFixture(t *testing.T, domains map[string]string) *scdFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shims")
	}
	dir := t.TempDir()
	for _, host := range []string{"single", "local"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, host), 0o700))
	}
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
		require.NoError(t, os.WriteFile(filepath.Join(dir, "single", "dom-"+name+".xml"), []byte(xml), 0o600))
		if d, err := parseDomainLibvirtxml(xml); err == nil && strings.TrimSpace(d.UUID) != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "single", "dom-"+strings.TrimSpace(d.UUID)+".xml"), []byte(xml), 0o600))
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "single", "list.txt"), []byte(list.String()), 0o600))

	bin := t.TempDir()
	for _, tool := range scdTools {
		require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte(scdFakeTool), 0o755)) //nolint:gosec // test shim must be executable
	}
	staging := t.TempDir()
	t.Setenv("FAKE_SCD_DIR", dir)
	t.Setenv("FAKE_SCD_STAGING", staging)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	vp := localHostVP("single")
	reg, err := hostconn.NewRegistry(newVirshConn("single", vp))
	require.NoError(t, err)
	return &scdFixture{
		t:       t,
		dir:     dir,
		staging: staging,
		p:       &Provider{virshProvider: vp, registry: reg, hostID: "single", hostStagingDir: staging},
	}
}

// script writes a per-host behavior file (fail-<cmd>, state, snaps-<domain>).
func (f *scdFixture) script(host, name, content string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, host, name), []byte(content), 0o600))
}

// calls returns the logged invocations, normalized.
func (f *scdFixture) calls() []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.log")) //nolint:gosec // test reads its own fixture log
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(f.t, err)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, f.normalize(l))
		}
	}
	return out
}

// definedXML returns the (normalized) domain XML a clone staged for define
// under domain, or "" when none was staged.
func (f *scdFixture) definedXML(domain string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.staging, domain+domainXMLStagingInfix+"0000000000")) //nolint:gosec // test reads its fixture
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(f.t, err)
	out := scdUUIDRE.ReplaceAllString(string(b), "<uuid>UUID</uuid>")
	out = scdMACRE.ReplaceAllString(out, "<mac address='MAC'/>")
	return f.normalize(out)
}

var (
	// scdUnixRE matches a unix-seconds timestamp (snapshot names, export ids).
	scdUnixRE = regexp.MustCompile(`\b1[0-9]{9}\b`)
	// scdRFC3339RE matches an RFC 3339 timestamp (default descriptions).
	scdRFC3339RE = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})`)
	// scdUUIDRE and scdMACRE match the fresh identity a clone generates.
	scdUUIDRE = regexp.MustCompile(`<uuid>[^<]*</uuid>`)
	scdMACRE  = regexp.MustCompile(`<mac address='[^']*'/>`)
)

// normalize replaces the per-run parts of s (scratch directories, times).
func (f *scdFixture) normalize(s string) string {
	s = strings.ReplaceAll(s, f.staging, "<staging>")
	s = strings.ReplaceAll(s, f.dir, "<fake>")
	s = scdUnixRE.ReplaceAllString(s, "<unix>")
	return scdRFC3339RE.ReplaceAllString(s, "<time>")
}

// scdResult is what a scenario produced.
type scdResult struct {
	Calls   []string `json:"calls"`
	Resp    string   `json:"resp"`
	Err     string   `json:"err"`
	Defined string   `json:"defined,omitempty"`
}

// scdScenario is one single-host call whose results are pinned.
type scdScenario struct {
	name    string
	domains map[string]string // nil: the default source domain
	setup   func(fx *scdFixture)
	run     func(ctx context.Context, s *Server) (any, error)
	// defined names the domain whose staged define XML is recorded.
	defined string
}

func snapCreate(req *providerv1.SnapshotCreateRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.SnapshotCreate(ctx, req) }
}

func snapDelete(req *providerv1.SnapshotDeleteRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.SnapshotDelete(ctx, req) }
}

func snapRevert(req *providerv1.SnapshotRevertRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.SnapshotRevert(ctx, req) }
}

func cloneCall(req *providerv1.CloneRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.Clone(ctx, req) }
}

func diskInfoCall(req *providerv1.GetDiskInfoRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.GetDiskInfo(ctx, req) }
}

func exportCall(req *providerv1.ExportDiskRequest) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) { return s.ExportDisk(ctx, req) }
}

func taskStatusCall(ref string) func(context.Context, *Server) (any, error) {
	return func(ctx context.Context, s *Server) (any, error) {
		return s.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: ref}})
	}
}

// scdTargetVM is the clone target's identity (no UID: it does not exist yet).
var scdTargetVM = &providerv1.ObjectIdentity{Namespace: "team-a", Name: "copy"}

// singleHostSCDScenarios covers every branch of the single-host snapshot,
// clone, disk-info, export and task-status RPCs a fake host can drive.
func singleHostSCDScenarios() []scdScenario {
	running := func(fx *scdFixture) { fx.script("single", "state", "running\n") }
	withSnap := func(fx *scdFixture) { fx.script("single", "snaps-"+scdDomain, "s1\ns2\n") }
	failing := func(host, what string, more ...func(*scdFixture)) func(*scdFixture) {
		return func(fx *scdFixture) {
			for _, m := range more {
				m(fx)
			}
			fx.script(host, "fail-"+what, "")
		}
	}
	stamped := map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{owner: ownerTeamA})}
	uefi := map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{uefi: true})}
	targetTaken := map[string]string{
		scdDomain:     scdDomainXML(scdDomain, scdDomainOpts{}),
		"team-a.copy": scdDomainXML("team-a.copy", scdDomainOpts{}),
	}
	fullClone := &providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy", TargetVm: scdTargetVM}

	return []scdScenario{
		// SnapshotCreate
		{name: "snapshot-create-disk-only", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "pre-upgrade", Description: "before the upgrade"})},
		{name: "snapshot-create-memory-running", setup: running, run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "mem", Description: "d", IncludeMemory: true})},
		{name: "snapshot-create-memory-stopped-downgrades", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "mem", Description: "d", IncludeMemory: true})},
		{name: "snapshot-create-default-name-and-description", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain})},
		{name: "snapshot-create-sanitized-name", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "-my snap/1!", Description: "d"})},
		{name: "snapshot-create-ignores-target-host", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "h", Description: "d", TargetHostId: "host-ignored"})},
		{name: "snapshot-create-domstate-fails", setup: failing("single", "domstate"), run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "x", Description: "d"})},
		{name: "snapshot-create-fails", setup: failing("single", "snapshot-create-as"), run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "x", Description: "d"})},
		{name: "snapshot-create-missing-domain", run: snapCreate(&providerv1.SnapshotCreateRequest{VmId: "gone", NameHint: "x", Description: "d"})},
		// SnapshotDelete
		{name: "snapshot-delete-existing", setup: withSnap, run: snapDelete(&providerv1.SnapshotDeleteRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-delete-absent", setup: withSnap, run: snapDelete(&providerv1.SnapshotDeleteRequest{VmId: scdDomain, SnapshotId: "s9"})},
		{name: "snapshot-delete-list-fails", setup: failing("single", "snapshot-list"), run: snapDelete(&providerv1.SnapshotDeleteRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-delete-fails", setup: failing("single", "snapshot-delete", withSnap), run: snapDelete(&providerv1.SnapshotDeleteRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-delete-ignores-target-host", setup: withSnap, run: snapDelete(&providerv1.SnapshotDeleteRequest{VmId: scdDomain, SnapshotId: "s2", TargetHostId: "host-ignored"})},
		// SnapshotRevert
		{name: "snapshot-revert-running", setup: func(fx *scdFixture) { withSnap(fx); running(fx) }, run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-revert-stopped", setup: withSnap, run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-revert-absent", setup: withSnap, run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s9"})},
		{name: "snapshot-revert-list-fails", setup: failing("single", "snapshot-list"), run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-revert-domstate-fails", setup: failing("single", "domstate", withSnap), run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})},
		{name: "snapshot-revert-fails", setup: failing("single", "snapshot-revert", withSnap), run: snapRevert(&providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})},
		// Clone
		{name: "clone-full", run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-linked", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy", TargetVm: scdTargetVM, Linked: true}), defined: "team-a.copy"},
		{name: "clone-legacy-target-name", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy"}), defined: "copy"},
		{name: "clone-ignores-source-host", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, SourceHostId: "host-ignored", TargetName: "copy", TargetVm: scdTargetVM}), defined: "team-a.copy"},
		{name: "clone-strips-source-owner-stamp", domains: stamped, run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-uefi-nvram", domains: uefi, run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-class-override", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy", TargetVm: scdTargetVM,
			ClassJson: `{"cpu":4,"memory":"4Gi","performanceProfile":{"cpuHotAddEnabled":true}}`}), defined: "team-a.copy"},
		{name: "clone-customize-json-not-applied", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "copy", TargetVm: scdTargetVM,
			CustomizeJson: `{"hostname":"x"}`}), defined: "team-a.copy"},
		{name: "clone-target-exists", domains: targetTaken, run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-source-missing", run: cloneCall(&providerv1.CloneRequest{SourceVmId: "gone", TargetName: "copy", TargetVm: scdTargetVM}), defined: "team-a.copy"},
		{name: "clone-mismatched-target-name", run: cloneCall(&providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: "other", TargetVm: scdTargetVM}), defined: "team-a.copy"},
		{name: "clone-copy-fails", setup: failing("local", "qemu-img"), run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-define-fails", setup: failing("local", "define"), run: cloneCall(fullClone), defined: "team-a.copy"},
		// A full clone requires a powered-off source (B2): refused before any
		// copy; and a host whose sudo refuses the privileged copy (B1) runs
		// the historical copy as the SSH user.
		{name: "clone-source-running", setup: running, run: cloneCall(fullClone), defined: "team-a.copy"},
		{name: "clone-copy-sudo-refused", setup: failing("local", "sudo"), run: cloneCall(fullClone), defined: "team-a.copy"},
		// GetDiskInfo
		{name: "diskinfo-primary", setup: withSnap, run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain})},
		{name: "diskinfo-explicit-path", run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain, DiskId: "/var/lib/libvirt/images/other.qcow2"})},
		{name: "diskinfo-non-path-disk-id", run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain, DiskId: "vda", SnapshotId: "s1"})},
		{name: "diskinfo-qemu-img-fails", setup: failing("local", "qemu-img"), run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain})},
		{name: "diskinfo-snapshot-list-fails", setup: failing("single", "snapshot-list"), run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain})},
		{name: "diskinfo-missing-domain", run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: "gone"})},
		{name: "diskinfo-ignores-target-host", run: diskInfoCall(&providerv1.GetDiskInfoRequest{VmId: scdDomain, TargetHostId: "host-ignored"})},
		// ExportDisk
		{name: "export-unknown-backend", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: "ftp"})},
		{name: "export-s3-direct-mode", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: "s3", TransferMode: "direct"})},
		{name: "export-s3-needs-ssh", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: "s3", DestinationUrl: "s3://b/k.qcow2"})},
		{name: "export-nfs-needs-ssh", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: "nfs", DestinationUrl: "nfs://h/e/k.qcow2"})},
		{name: "export-pvc-unsupported-format", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, Format: "vmdk", DestinationUrl: "pvc://vrgold-pvc/k.vmdk"})},
		{name: "export-pvc-no-conversion", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, Format: "qcow2", DestinationUrl: "pvc://vrgold-pvc/k.qcow2"})},
		{name: "export-pvc-compressed", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, Format: "qcow2", Compress: true, DestinationUrl: "pvc://vrgold-pvc/k.qcow2"})},
		{name: "export-pvc-ignores-target-host", run: exportCall(&providerv1.ExportDiskRequest{VmId: scdDomain, TargetHostId: "host-ignored", DestinationUrl: "pvc://vrgold-pvc/k.qcow2"})},
		// TaskStatus
		{name: "task-status-any-ref", run: taskStatusCall("task-123")},
		{name: "task-status-host-looking-ref", run: taskStatusCall("host-task/v1/host-a/task-123")},
		{name: "task-status-empty-ref", run: taskStatusCall("")},
	}
}

// TestSingleHost_SnapshotCloneDisk_ResultsUnchanged is the single-host
// equivalence proof for slice 3: every scenario's virsh/host command sequence,
// response, error and (Clone) defined domain XML equal what origin/main
// produced before the slice.
func TestSingleHost_SnapshotCloneDisk_ResultsUnchanged(t *testing.T) {
	got := map[string]scdResult{}
	for _, sc := range singleHostSCDScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			domains := sc.domains
			if domains == nil {
				domains = map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})}
			}
			fx := newSCDFixture(t, domains)
			if sc.setup != nil {
				sc.setup(fx)
			}
			require.False(t, fx.p.clustered())
			res := scdResult{}
			resp, err := sc.run(context.Background(), NewServer(fx.p))
			if err != nil {
				res.Err = fx.normalize(err.Error())
			}
			if resp != nil {
				b, merr := json.Marshal(resp)
				require.NoError(t, merr)
				res.Resp = fx.normalize(string(b))
			}
			res.Calls = fx.calls()
			if sc.defined != "" {
				res.Defined = fx.definedXML(sc.defined)
			}
			got[sc.name] = res
		})
	}

	if os.Getenv(callSeqUpdateEnv) == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(scdGoldenFile), 0o750))
		require.NoError(t, os.WriteFile(scdGoldenFile, append(b, '\n'), 0o600))
		t.Logf("rewrote %s", scdGoldenFile)
		return
	}

	raw, err := os.ReadFile(scdGoldenFile)
	require.NoError(t, err)
	want := map[string]scdResult{}
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Len(t, got, len(want), "every golden scenario is still exercised")
	for name, w := range want {
		g, ok := got[name]
		require.True(t, ok, "scenario %s not run", name)
		assert.Equal(t, w.Calls, g.Calls, "%s: the single-host command sequence changed", name)
		assert.Equal(t, w.Resp, g.Resp, "%s: the single-host response changed", name)
		assert.Equal(t, w.Err, g.Err, "%s: the single-host error changed", name)
		assert.Equal(t, w.Defined, g.Defined, "%s: the single-host defined domain XML changed", name)
	}
}
