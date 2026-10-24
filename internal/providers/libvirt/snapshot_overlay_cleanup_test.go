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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// overlaySnapFakeVirsh wraps the ops fake virsh (opsFakeVirsh, at
// $FAKE_OPS_VIRSH) with the snapshot subcommands: per host, "snaps" lists the
// snapshot names, "snapxml-<name>" is a snapshot's `snapshot-dumpxml`, and
// snapshot-delete drops the name from "snaps" (libvirt's commit-and-pivot is
// modelled by the domain definition the test writes: the base disk).
const overlaySnapFakeVirsh = `#!/bin/sh
host=local
if [ "$1" = "-c" ]; then host="${2##*/}"; fi
d="$FAKE_VIRSH_DIR/$host"
sub="$1"; [ "$1" = "-c" ] && sub="$3"
case "$sub" in
  snapshot-list|snapshot-dumpxml|snapshot-delete)
    shift 2
    printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
    case "$1" in
      snapshot-list) cat "$d/snaps" 2>/dev/null ;;
      snapshot-dumpxml) if [ -f "$d/snapxml-$3" ]; then cat "$d/snapxml-$3"; else echo "error: snapshot $3 not found" >&2; exit 1; fi ;;
      snapshot-delete) grep -vx "$3" "$d/snaps" > "$d/snaps.new"; mv "$d/snaps.new" "$d/snaps"; echo "Domain snapshot $3 deleted" ;;
    esac ;;
  *) exec "$FAKE_OPS_VIRSH" "$@" ;;
esac
`

// snapOverlayDomainXML is domain name (uuid) with one qcow2 disk at disk.
func snapOverlayDomainXML(name, uuid, disk string) string {
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n  <devices>\n"+
		"    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n      <source file='%s'/>\n"+
		"      <target dev='vda' bus='virtio'/>\n    </disk>\n  </devices>\n</domain>\n", name, uuid, disk)
}

// externalSnapshotXML is `virsh snapshot-dumpxml` of an external disk-only
// snapshot whose vda overlay is overlay, recording the domain as it was then
// (on disk).
func externalSnapshotXML(name, overlay, disk string) string {
	return fmt.Sprintf("<domainsnapshot>\n  <name>%s</name>\n  <state>disk-snapshot</state>\n  <memory snapshot='no'/>\n"+
		"  <disks>\n    <disk name='vda' snapshot='external' type='file'>\n      <driver type='qcow2'/>\n"+
		"      <source file='%s'/>\n    </disk>\n    <disk name='hda' snapshot='no'/>\n  </disks>\n"+
		"  <domain type='kvm'><name>web</name><devices><disk type='file' device='disk'><source file='%s'/></disk></devices></domain>\n"+
		"</domainsnapshot>\n", name, overlay, disk)
}

const (
	overlayDomainUUID = "11111111-2222-4333-8444-555555555555"
	overlayOtherUUID  = "99999999-2222-4333-8444-555555555555"
)

// overlayCase is a fake single host where domain "web" (disk web-disk.qcow2
// in the fixture pool) has external snapshot s1 whose overlay is
// web-disk.s1, a regular file in the pool.
type overlayCase struct {
	fx      *routingFixture
	host    string
	base    string
	overlay string
}

// newOverlayCase builds the host; extra adds other domains (name -> XML).
func newOverlayCase(t *testing.T, extra map[string]string) *overlayCase {
	t.Helper()
	c := &overlayCase{host: "single"}
	c.base = filepath.Join(fixtureImagesDir, "web-disk.qcow2")
	c.overlay = filepath.Join(fixtureImagesDir, "web-disk.s1")
	domains := map[string]string{"web": snapOverlayDomainXML("web", overlayDomainUUID, c.base)}
	for n, x := range extra {
		domains[n] = x
	}
	c.fx = newOpsFixture(t, map[string]map[string]string{c.host: domains})
	require.NoError(t, os.WriteFile(c.base, []byte("base"), 0o600))
	require.NoError(t, os.WriteFile(c.overlay, []byte("overlay"), 0o600))
	t.Cleanup(func() { _ = os.Remove(c.overlay) })

	// Put the snapshot-aware wrapper in front of the ops fake virsh.
	opsVirsh, err := lookPathIn(os.Getenv("PATH"), "virsh")
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(overlaySnapFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_OPS_VIRSH", opsVirsh)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	c.script("snaps", "s1\n")
	c.script("snapxml-s1", externalSnapshotXML("s1", c.overlay, c.base))
	return c
}

// lookPathIn finds name in the PATH list path.
func lookPathIn(path, name string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not on PATH", name)
}

func (c *overlayCase) script(name, content string) { c.fx.script(c.host, name, content) }

// deleteS1 runs the SnapshotDelete RPC of web's snapshot s1; it must succeed.
func (c *overlayCase) deleteS1(t *testing.T) {
	t.Helper()
	c.deleteSnap(t, "s1")
}

// deleteSnap runs the SnapshotDelete RPC of web's snapshot snap; it must
// succeed.
func (c *overlayCase) deleteSnap(t *testing.T, snap string) {
	t.Helper()
	vp := localHostVP(c.host)
	reg, err := hostconn.NewRegistry(newVirshConn(hostconn.HostID(c.host), vp))
	require.NoError(t, err)
	p := &Provider{virshProvider: vp, registry: reg, hostID: hostconn.HostID(c.host)}
	_, err = NewServer(p).SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: snap})
	require.NoError(t, err, "SnapshotDelete never fails for its overlay cleanup")
	assert.Contains(t, c.fx.calls(), c.host+" snapshot-delete web "+snap)
}

// removed reports whether the cleanup removed the overlay (the sudo shim logs
// the rm instead of running it).
func (c *overlayCase) removed() bool {
	canon, err := filepath.EvalSymlinks(filepath.Dir(c.overlay))
	if err != nil {
		canon = filepath.Dir(c.overlay)
	}
	want := "local sudo rm -f -- " + filepath.Join(canon, filepath.Base(c.overlay))
	for _, call := range c.fx.calls() {
		if call == want {
			return true
		}
	}
	return false
}

// TestSnapshotDelete_RemovesTheLeftOverlay is the v0.4.0-rc1 F2 fix: libvirt
// commits an external snapshot back into its base on delete but leaves the
// overlay <domain>-disk.<snapshot> in the pool; SnapshotDelete removes it
// once it is out of the domain's chain and nobody uses it.
func TestSnapshotDelete_RemovesTheLeftOverlay(t *testing.T) {
	c := newOverlayCase(t, nil)
	c.deleteS1(t)
	assert.True(t, c.removed(), "the overlay libvirt left behind is removed: %v", c.fx.calls())
}

// TestSnapshotDelete_KeepsAnOverlayItCannotProveFree: every condition that
// cannot be established keeps the file — and the RPC still succeeds.
func TestSnapshotDelete_KeepsAnOverlayItCannotProveFree(t *testing.T) {
	cases := map[string]func(t *testing.T) *overlayCase{
		"still in the domain's chain (the delete did not pivot)": func(t *testing.T) *overlayCase {
			c := newOverlayCase(t, nil)
			x := snapOverlayDomainXML("web", overlayDomainUUID, c.overlay)
			c.script("dom-web.xml", x)
			c.script("dom-"+overlayDomainUUID+".xml", x)
			return c
		},
		"referenced by another snapshot": func(t *testing.T) *overlayCase {
			c := newOverlayCase(t, nil)
			c.script("snaps", "s1\ns2\n")
			c.script("snapxml-s2", externalSnapshotXML("s2", filepath.Join(fixtureImagesDir, "web-disk.s2"), c.overlay))
			return c
		},
		"another domain uses it": func(t *testing.T) *overlayCase {
			overlay := filepath.Join(fixtureImagesDir, "web-disk.s1")
			return newOverlayCase(t, map[string]string{"other": snapOverlayDomainXML("other", overlayOtherUUID, overlay)})
		},
		"not the domain's own overlay name": func(t *testing.T) *overlayCase {
			c := newOverlayCase(t, nil)
			foreign := filepath.Join(fixtureImagesDir, "someone-else.s1")
			require.NoError(t, os.WriteFile(foreign, []byte("x"), 0o600))
			t.Cleanup(func() { _ = os.Remove(foreign) })
			c.script("snapxml-s1", externalSnapshotXML("s1", foreign, c.base))
			c.overlay = foreign
			return c
		},
		"a symbolic link": func(t *testing.T) *overlayCase {
			c := newOverlayCase(t, nil)
			require.NoError(t, os.Remove(c.overlay))
			target := filepath.Join(t.TempDir(), "elsewhere")
			require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))
			require.NoError(t, os.Symlink(target, c.overlay))
			return c
		},
		"outside the storage directories": func(t *testing.T) *overlayCase {
			c := newOverlayCase(t, nil)
			outside := filepath.Join(t.TempDir(), "web-disk.s1")
			require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
			c.script("snapxml-s1", externalSnapshotXML("s1", outside, c.base))
			c.overlay = outside
			return c
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := setup(t)
			c.deleteS1(t)
			assert.False(t, c.removed(), "kept: %v", c.fx.calls())
			for _, call := range c.fx.calls() {
				assert.False(t, strings.Contains(call, " rm ") && strings.Contains(call, ".s1"), "no overlay is removed: %q", call)
			}
		})
	}
}

// TestSnapshotDelete_UnreadableSnapshotIsNotDeleted: a snapshot whose
// definition cannot be read is not deleted (V11): what its delete would merge
// away cannot be checked. The refusal is retryable (VM_DISK_CHECK_FAILED) and
// nothing is sent or removed.
func TestSnapshotDelete_UnreadableSnapshotIsNotDeleted(t *testing.T) {
	c := newOverlayCase(t, nil)
	require.NoError(t, os.Remove(filepath.Join(c.fx.dir, c.host, "snapxml-s1")))
	vp := localHostVP(c.host)
	reg, err := hostconn.NewRegistry(newVirshConn(hostconn.HostID(c.host), vp))
	require.NoError(t, err)
	p := &Provider{virshProvider: vp, registry: reg, hostID: hostconn.HostID(c.host)}
	_, err = NewServer(p).SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: "s1"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, errorInfoReasons(st)[contracts.VMDiskCheckFailedReason], "retryable, kept out of the breaker: %v", st)
	assert.NotContains(t, c.fx.calls(), c.host+" snapshot-delete web s1", "nothing is deleted")
	assert.False(t, c.removed())
}

// TestParseSnapshotOverlays reads only the external overlays of the
// snapshot's own disk list, never the recorded domain definition.
func TestParseSnapshotOverlays(t *testing.T) {
	got, err := parseSnapshotOverlays(externalSnapshotXML("s1", "/pool/web-disk.s1", "/pool/web-disk.qcow2"))
	require.NoError(t, err)
	assert.Equal(t, []string{"/pool/web-disk.s1"}, got)

	files, err := sourceFilesIn(externalSnapshotXML("s1", "/pool/web-disk.s1", "/pool/web-disk.qcow2"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"/pool/web-disk.s1", "/pool/web-disk.qcow2"}, files)
}

// clusteredSnapFakeVirsh wraps the createHost fake virsh ($FAKE_CREATE_VIRSH)
// with the snapshot subcommands, per host under $FAKE_HOST_DIR/<host>:
// "snaps" lists the snapshots, "snapxml-<name>" is a snapshot's XML.
const clusteredSnapFakeVirsh = `#!/bin/sh
uri=""
if [ "$1" = "-c" ]; then uri="$2"; fi
host="${uri##*/}"
d="$FAKE_HOST_DIR/$host"
sub="$1"; [ "$1" = "-c" ] && sub="$3"
case "$sub" in
  snapshot-list|snapshot-dumpxml|snapshot-delete)
    shift 2
    printf '%s\t%s\n' "$host" "$*" >> "$FAKE_HOST_DIR/virsh.log"
    case "$1" in
      snapshot-list) cat "$d/snaps" 2>/dev/null ;;
      snapshot-dumpxml) if [ -f "$d/snapxml-$3" ]; then cat "$d/snapxml-$3"; else echo "error: snapshot $3 not found" >&2; exit 1; fi ;;
      snapshot-delete) grep -vx "$3" "$d/snaps" > "$d/snaps.new"; mv "$d/snaps.new" "$d/snaps"; echo "Domain snapshot $3 deleted" ;;
    esac ;;
  *) exec "$FAKE_CREATE_VIRSH" "$@" ;;
esac
`

// TestClusteredSnapshotDelete_OverlayCleanupIsGuardedClusterWide: on a
// clustered provider the left overlay is removed from the shared pool only
// when no domain on ANY host of the Provider uses it.
func TestClusteredSnapshotDelete_OverlayCleanupIsGuardedClusterWide(t *testing.T) {
	for name, usedElsewhere := range map[string]bool{"free": false, "used on host-a": true} {
		t.Run(name, func(t *testing.T) {
			s := newSharedPool(t)
			base := s.disk("team-a.web-disk.qcow2", "base")
			overlay := s.disk("team-a.web-disk.s1", "overlay")
			s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, base, ownerTeamA, false))
			if usedElsewhere {
				s.defineOn("host-a", "legacy-web", uuidForeign, guardDomainXML("legacy-web", uuidForeign, overlay, ownerForeign, false))
			}
			hb := filepath.Join(s.root, "host-b")
			require.NoError(t, os.WriteFile(filepath.Join(hb, "snaps"), []byte("s1\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(hb, "snapxml-s1"), []byte(externalSnapshotXML("s1", overlay, base)), 0o600))

			createVirsh, err := lookPathIn(os.Getenv("PATH"), "virsh")
			require.NoError(t, err)
			bin := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(clusteredSnapFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
			t.Setenv("FAKE_CREATE_VIRSH", createVirsh)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			_, err = NewServer(s.p).SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{
				VmId: "team-a.web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner,
			})
			require.NoError(t, err)

			sudo, _ := os.ReadFile(filepath.Join(s.root, "sudo.log")) //nolint:gosec // test reads its own log
			canonDir, err := filepath.EvalSymlinks(filepath.Dir(overlay))
			require.NoError(t, err)
			rm := "rm -f -- " + filepath.Join(canonDir, filepath.Base(overlay))
			if usedElsewhere {
				assert.NotContains(t, string(sudo), rm, "a file a domain on another host uses is never removed")
			} else {
				assert.Contains(t, string(sudo), rm, "the left overlay is removed")
			}
		})
	}
}

// TestSnapshotDelete_SnapshotNamedLikeTheBaseKeepsTheBase: a snapshot named
// "qcow2" makes the own-overlay name <domain>-disk.qcow2 — the live base disk.
// Even if the snapshot names that file, it is in the domain's current chain,
// so it is never removed.
func TestSnapshotDelete_SnapshotNamedLikeTheBaseKeepsTheBase(t *testing.T) {
	c := newOverlayCase(t, nil)
	c.script("snaps", "qcow2\n")
	c.script("snapxml-qcow2", externalSnapshotXML("qcow2", c.base, c.base))
	c.deleteSnap(t, "qcow2")
	for _, call := range c.fx.calls() {
		assert.NotContains(t, call, " rm ", "the live base disk is never removed: %q", call)
	}
	assert.FileExists(t, c.base)
}

// TestClusteredSnapshotDelete_UncheckableHostKeepsTheOverlay: a host of the
// Provider that cannot be scanned (it is down) leaves the cluster-wide answer
// unknown, so the overlay is kept — and the SnapshotDelete still succeeds.
func TestClusteredSnapshotDelete_UncheckableHostKeepsTheOverlay(t *testing.T) {
	s := newSharedPoolWithDeadHost(t)
	base := s.disk("team-a.web-disk.qcow2", "base")
	overlay := s.disk("team-a.web-disk.s1", "overlay")
	s.defineOn("host-b", "team-a.web", uuidVMOnB, guardDomainXML("team-a.web", uuidVMOnB, base, ownerTeamA, false))
	hb := filepath.Join(s.root, "host-b")
	require.NoError(t, os.WriteFile(filepath.Join(hb, "snaps"), []byte("s1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(hb, "snapxml-s1"), []byte(externalSnapshotXML("s1", overlay, base)), 0o600))

	createVirsh, err := lookPathIn(os.Getenv("PATH"), "virsh")
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(clusteredSnapFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_CREATE_VIRSH", createVirsh)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = NewServer(s.p).SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{
		VmId: "team-a.web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner,
	})
	require.NoError(t, err, "the overlay cleanup never fails the delete")
	assert.Positive(t, s.dials("host-c"), "the dead host was asked")
	sudo, _ := os.ReadFile(filepath.Join(s.root, "sudo.log")) //nolint:gosec // test reads its own log
	assert.NotContains(t, string(sudo), "rm -f", "a host that cannot be checked keeps the overlay")
	assert.FileExists(t, overlay)
}
