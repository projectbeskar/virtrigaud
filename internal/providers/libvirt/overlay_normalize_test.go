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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the V7 normalisation of a domain's own snapshot overlays
// (overlay_perms.go) after a snapshot or a revert, and the repair of the ones
// an earlier snapshot, revert or release left 0600.

const (
	normPool   = "/var/lib/libvirt/images"
	normDomain = "team-a.web"
	normDisk   = normPool + "/team-a.web-disk.qcow2"
	normS1     = normPool + "/team-a.web-disk.s1"
	normS2     = normPool + "/team-a.web-disk.1792831471"
	normBase   = normPool + "/ubuntu.qcow2"
	// normSafeDir is overlayStatScript's directory lines for a root-owned
	// 0755 pool directory, read by SSH user 1000.
	normSafeDir = "755 0\n1000"
)

// normDirs is the overlayDirsFunc of these tests: the default pool only.
func normDirs(context.Context, hostCommandRunner) []string { return []string{normPool} }

// overlayFakeHost is a hostCommandRunner for normalizeOwnOverlays: it serves
// one domain definition, canonicalizes every path as itself (or as canon
// says), answers overlayStatScript per file, and records every command;
// sudo fails when sudoErr is set.
type overlayFakeHost struct {
	xml     string
	stat    map[string]string
	canon   map[string]string
	sudoErr error
	calls   []string
}

func (f *overlayFakeHost) runVirshCommand(_ context.Context, args ...string) (*VirshResult, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	ok := func(s string) (*VirshResult, error) { return &VirshResult{Stdout: s}, nil }
	if args[0] == "dumpxml" {
		return ok(f.xml)
	}
	if args[0] != "!" {
		return nil, fmt.Errorf("unscripted: %s", line)
	}
	switch args[1] {
	case "realpath":
		var out strings.Builder
		for i, a := range args {
			if a == "--" {
				for _, p := range args[i+1:] {
					if c, found := f.canon[p]; found {
						p = c
					}
					out.WriteString(p + "\x00")
				}
				break
			}
		}
		return ok(out.String())
	case "sh":
		return ok(f.stat[args[5]])
	case "sudo":
		if f.sudoErr != nil {
			return &VirshResult{ExitCode: 1, Stderr: "sudo: a password is required"}, f.sudoErr
		}
		return ok("")
	}
	return nil, fmt.Errorf("unscripted: %s", line)
}

// privileged returns the sudo commands the host saw.
func (f *overlayFakeHost) privileged() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "! sudo ") {
			out = append(out, strings.TrimPrefix(c, "! "))
		}
	}
	return out
}

// normChainXML is team-a.web's definition with chain s2 <- s1 <- disk <-
// base (nested <backingStore>s, as libvirt keeps them for a shut-off domain
// after an offline snapshot and a revert); running adds the domain id.
func normChainXML(running bool) string {
	id := ""
	if running {
		id = " id='7'"
	}
	return "<domain type='kvm'" + id + "><name>" + normDomain + "</name><devices>" +
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='" + normS2 + "'/>" +
		"<backingStore type='file'><format type='qcow2'/><source file='" + normS1 + "'/>" +
		"<backingStore type='file'><format type='qcow2'/><source file='" + normDisk + "'/>" +
		"<backingStore type='file'><format type='qcow2'/><source file='" + normBase + "'/><backingStore/>" +
		"</backingStore></backingStore></backingStore><target dev='vda'/></disk>" +
		"<disk type='file' device='cdrom'><source file='" + normPool + "/team-a.web-disk.iso'/><target dev='hda'/></disk>" +
		"</devices></domain>"
}

func chownCmd(p string) string { return "sudo -n chown -h " + qemuFileOwner + " -- " + p }
func chmodCmd(p string) string { return "sudo -n chmod " + overlayGroupRead + " -- " + p }

// TestNormalizeOwnOverlays_ShutOffDomain: after an offline snapshot or a
// revert, every own overlay of the chain (the new one, and the one an
// earlier snapshot left 0600: the repair) is given to the qemu user and group
// read; the domain's own disk, the base image below it and the CD-ROM are
// never touched.
func TestNormalizeOwnOverlays_ShutOffDomain(t *testing.T) {
	f := &overlayFakeHost{xml: normChainXML(false), stat: map[string]string{
		normS2: "600 root:root\n" + normSafeDir,
		normS1: "600 root:virt\n" + normSafeDir,
	}}
	assert.Zero(t, normalizeOwnOverlays(context.Background(), f, normDomain, normDirs))
	assert.Equal(t, []string{chownCmd(normS2), chmodCmd(normS2), chownCmd(normS1), chmodCmd(normS1)}, f.privileged())
	for _, c := range f.calls {
		assert.NotContains(t, c, normDisk, "the domain's own disk is not an overlay")
		assert.NotContains(t, c, normBase, "nothing below the own disk")
		assert.NotContains(t, c, ".iso", "only disks")
	}
}

// TestNormalizeOwnOverlays_AlreadyNormalIsLeftAlone: an overlay that already
// has the mode and owner of a VM disk gets no privileged command, and a
// missing one is skipped.
func TestNormalizeOwnOverlays_AlreadyNormalIsLeftAlone(t *testing.T) {
	f := &overlayFakeHost{xml: normChainXML(false), stat: map[string]string{
		normS2: "640 " + qemuFileOwner + "\n" + normSafeDir,
		normS1: overlayKindAbsent,
	}}
	assert.Zero(t, normalizeOwnOverlays(context.Background(), f, normDomain, normDirs))
	assert.Empty(t, f.privileged())
}

// TestNormalizeOwnOverlays_RunningDomain: a running domain's overlay only gets
// group read, and only when it already belongs to the qemu user — it is never
// given away under the running qemu, and nothing is taken from its owner.
func TestNormalizeOwnOverlays_RunningDomain(t *testing.T) {
	f := &overlayFakeHost{xml: normChainXML(true), stat: map[string]string{
		normS2: "600 " + qemuFileOwner + "\n" + normSafeDir,
		normS1: "600 root:root\n" + normSafeDir,
	}}
	assert.Zero(t, normalizeOwnOverlays(context.Background(), f, normDomain, normDirs))
	assert.Equal(t, []string{chmodCmd(normS2)}, f.privileged(), "no chown while it runs; a root-owned overlay is left")
}

// TestNormalizeOwnOverlays_SafetyRefusals: nothing privileged runs on a
// symbolic link, a non-regular file, a file outside the storage directories,
// a file in a directory another account can write, or a chain that is not
// the domain's own.
func TestNormalizeOwnOverlays_SafetyRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		xml        string
		stat       map[string]string
		canon      map[string]string
		wantFailed int
	}{
		"symlink": {xml: normChainXML(false), stat: map[string]string{
			normS2: overlayKindSymlink, normS1: overlayKindSymlink}, wantFailed: 2},
		"not a regular file": {xml: normChainXML(false), stat: map[string]string{
			normS2: overlayKindOther, normS1: overlayKindOther}, wantFailed: 2},
		"outside the storage directories": {xml: normChainXML(false),
			canon: map[string]string{normS2: "/etc/team-a.web-disk.1792831471", normS1: "/srv/other/team-a.web-disk.s1"},
			stat: map[string]string{
				"/etc/team-a.web-disk.1792831471": "600 root:root\n" + normSafeDir,
				"/srv/other/team-a.web-disk.s1":   "600 root:root\n" + normSafeDir,
			}, wantFailed: 2},
		"group-writable directory": {xml: normChainXML(false), stat: map[string]string{
			normS2: "600 root:root\n775 0\n1000", normS1: "600 root:root\n777 0\n1000"}, wantFailed: 2},
		"another domain's file names": {xml: strings.ReplaceAll(normChainXML(false), "team-a.web-disk.", "team-b.db-disk."),
			wantFailed: 0},
		"a foreign image above the own overlays": {xml: strings.Replace(normChainXML(false), normS2, normBase, 1),
			wantFailed: 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := &overlayFakeHost{xml: tc.xml, stat: tc.stat, canon: tc.canon}
			assert.Equal(t, tc.wantFailed, normalizeOwnOverlays(context.Background(), f, normDomain, normDirs))
			assert.Empty(t, f.privileged(), "nothing privileged runs")
		})
	}
}

// TestNormalizeOwnOverlays_SudoRefusedIsLoggedNotFailed: without a sudo rule
// for chown/chmod the overlay is left as it is and counted; the caller's
// operation is not failed (see the snapshot/revert tests below).
func TestNormalizeOwnOverlays_SudoRefusedIsLoggedNotFailed(t *testing.T) {
	f := &overlayFakeHost{xml: normChainXML(false), sudoErr: errors.New("exit 1"), stat: map[string]string{
		normS2: "600 root:root\n" + normSafeDir,
		normS1: "640 " + qemuFileOwner + "\n" + normSafeDir,
	}}
	assert.Equal(t, 1, normalizeOwnOverlays(context.Background(), f, normDomain, normDirs))
	assert.Equal(t, []string{chownCmd(normS2)}, f.privileged(), "no chmod of a file that could not be given to the qemu user")
}

// TestNormalizeOwnOverlays_NoDirsNormalisesNothing: a backend without storage
// directories (nil) runs no command at all.
func TestNormalizeOwnOverlays_NoDirsNormalisesNothing(t *testing.T) {
	f := &overlayFakeHost{xml: normChainXML(false)}
	assert.Zero(t, normalizeOwnOverlays(context.Background(), f, normDomain, nil))
	assert.Empty(t, f.calls)
}

// permMediaConn is a mediaConn whose host commands answer overlayStatScript
// with stat and record every command; sudo fails with sudoErr when set.
type permMediaConn struct {
	*mediaConn
	stat    string
	sudoErr error
	runs    []string
}

func (c *permMediaConn) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	c.runs = append(c.runs, strings.Join(argv, " "))
	switch argv[0] {
	case "realpath":
		var out strings.Builder
		for i, a := range argv {
			if a == "--" {
				for _, p := range argv[i+1:] {
					out.WriteString(p + "\x00")
				}
				break
			}
		}
		return &hostconn.Result{Stdout: out.String()}, nil
	case "sh":
		return &hostconn.Result{Stdout: c.stat}, nil
	case "sudo":
		if c.sudoErr != nil {
			return &hostconn.Result{ExitCode: 1, Stderr: "sudo: a password is required"}, c.sudoErr
		}
	}
	return &hostconn.Result{}, nil
}

// v6Dirs is the overlayDirsFunc of the media-host tests: v6Disk's directory.
func v6Dirs(context.Context, hostCommandRunner) []string { return []string{normPool} }

// TestSnapshotRevert_NormalisesTheNewOverlay: libvirt's revert leaves the VM
// on a new 0600 overlay <disk>.<epoch>; the provider gives it the mode and
// owner of a VM disk after the revert. When it cannot (no sudo rule), the
// revert that happened is still answered success.
func TestSnapshotRevert_NormalisesTheNewOverlay(t *testing.T) {
	for _, sudoErr := range []error{nil, errors.New("exit 1")} {
		h := newMediaHost()
		require.NoError(t, createSnap(t, h, "p3", false, v6Roots))
		conn := &permMediaConn{mediaConn: newMediaConn(h), stat: "600 root:root\n" + normSafeDir, sudoErr: sudoErr}
		_, err := snapshotRevertOn(context.Background(), conn, v6Domain, "p3", v6Roots, v6Dirs)
		require.NoError(t, err, "a normalisation failure never fails the revert")

		overlay := overlayName(v6Disk, v6Epoch)
		require.Equal(t, overlay, h.source("vda"))
		assert.Contains(t, conn.runs, "sudo -n chown -h "+qemuFileOwner+" -- "+overlay)
		if sudoErr == nil {
			assert.Contains(t, conn.runs, "sudo -n chmod g+r -- "+overlay)
		}
		assert.Equal(t, "dumpxml "+v6Domain, h.calls[len(h.calls)-1], "the definition is read after the revert")
	}
}

// TestSnapshotCreate_NormalisesOwnOverlaysAfterwards: a snapshot reads the
// domain's definition again once it is taken and normalises its own
// overlays (here: one an earlier revert left 0600, repaired by this
// snapshot); a failure does not fail the snapshot.
func TestSnapshotCreate_NormalisesOwnOverlaysAfterwards(t *testing.T) {
	h := newMediaHost()
	overlay := overlayName(v6Disk, v6Epoch)
	h.disks[0].source = overlay // left by an earlier revert
	for _, sudoErr := range []error{nil, errors.New("exit 1")} {
		conn := &permMediaConn{mediaConn: newMediaConn(h), stat: "600 root:root\n" + normSafeDir, sudoErr: sudoErr}
		name := fmt.Sprintf("s-%t", sudoErr == nil)
		_, err := snapshotCreateWith(context.Background(), conn, v6Domain,
			&providerv1.SnapshotCreateRequest{VmId: v6Domain, NameHint: name, Description: "d"}, snapshotCreateOptions{seedRoots: v6Roots, overlayDirs: v6Dirs})
		require.NoError(t, err, "a normalisation failure never fails the snapshot")
		assert.Contains(t, conn.runs, "sudo -n chown -h "+qemuFileOwner+" -- "+overlay)
		assert.Equal(t, "dumpxml "+v6Domain, h.calls[len(h.calls)-1])
	}
}
