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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the delete-safety fix: Delete removes only a domain's OWN
// top-level disk files inside the VM storage directories, and refuses —
// before anything is changed — while another domain (a linked clone) uses one
// of them. They run on the full fake host (createHost: real files, real
// realpath/stat/rm, fake virsh/qemu-img, sudo only logs), single-host and
// clustered.

// fixtureQemuImgShim is a qemu-img for the routing/ops fixtures: `info
// --backing-chain` answers $FAKE_VIRSH_DIR/chain-<file name>.json when present
// and the file alone otherwise. Calls go to their own log, so the virsh call
// sequences the fixtures pin are unchanged.
const fixtureQemuImgShim = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_VIRSH_DIR/qemu-img.log"
last=""; chain=""
for a in "$@"; do last="$a"; if [ "$a" = "--backing-chain" ]; then chain=1; fi; done
c="$FAKE_VIRSH_DIR/chain-$(basename "$last").json"
case "$1" in
  info)
    if [ -n "$chain" ] && [ -f "$c" ]; then cat "$c"; exit 0; fi
    if [ -n "$chain" ]; then printf '[{"format":"qcow2","filename":"%s"}]\n' "$last"; exit 0; fi
    printf '{"format":"qcow2","filename":"%s"}\n' "$last" ;;
  *) exit 0 ;;
esac
`

// fixtureSudoShim is a sudo for the routing/ops fixtures: `sudo -n qemu-img`
// (the disk in-use check's chain read) runs the qemu-img shim without being
// logged, so the pinned call sequences are unchanged; anything else is logged
// as "local sudo <args>" and not run.
const fixtureSudoShim = "#!/bin/sh\n" +
	"if [ \"$1\" = \"-n\" ] && [ \"$2\" = \"qemu-img\" ]; then shift 2; exec qemu-img \"$@\"; fi\n" +
	"printf 'local %s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$FAKE_VIRSH_DIR/calls.log\"\n"

// installQemuImgShim puts fixtureQemuImgShim and fixtureSudoShim on PATH (in
// bin).
func installQemuImgShim(t *testing.T, bin string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(fixtureQemuImgShim), 0o755)) //nolint:gosec // test shim must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(fixtureSudoShim), 0o755))        //nolint:gosec // test shim must be executable
}

// writeFixtureUUIDs writes hd/uuids.txt — what `virsh list --all --uuid`
// prints — from the UUIDs of domains' definitions.
func writeFixtureUUIDs(t *testing.T, hd string, domains map[string]string) {
	t.Helper()
	seen := map[string]bool{}
	var b strings.Builder
	for _, x := range domains {
		d, err := parseDomainDisks(x)
		if err != nil || d.UUID == "" || seen[d.UUID] {
			continue
		}
		seen[d.UUID] = true
		b.WriteString(d.UUID + "\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(hd, "uuids.txt"), []byte(b.String()), 0o600))
}

// liveBackingStore turns the recorded definition of domain name on host into
// the shape `virsh dumpxml` shows while it RUNS: its disk's backing file listed
// in a nested <backingStore>, as in the security review's repro.
func (c *createHost) liveBackingStore(host, name, disk, backing string) {
	c.t.Helper()
	dir := filepath.Join(c.root, host)
	x, err := os.ReadFile(filepath.Join(dir, "dom-"+name+".xml")) //nolint:gosec // test reads its own fixture
	require.NoError(c.t, err)
	d, err := parseDomainDisks(string(x))
	require.NoError(c.t, err)
	src := fmt.Sprintf("<source file='%s'/>", disk)
	require.Contains(c.t, string(x), src)
	live := strings.Replace(string(x), src, src+fmt.Sprintf(
		"<backingStore type='file' index='3'><format type='qcow2'/><source file='%s'/><backingStore/></backingStore>", backing), 1)
	for _, key := range []string{name, d.UUID} {
		require.NoError(c.t, os.WriteFile(filepath.Join(dir, "dom-"+key+".xml"), []byte(live), 0o600))
	}
}

// removals returns the files the provider removed with sudo on the fake host.
func (c *createHost) removals() []string {
	var out []string
	for _, l := range splitLines(c.log("sudo")) {
		if strings.HasPrefix(l, "rm -f -- ") {
			out = append(out, strings.TrimPrefix(l, "rm -f -- "))
		}
	}
	return out
}

// virshCalls returns the virsh subcommands (with arguments) run on host.
func (c *createHost) virshCalls(host string) []string {
	var out []string
	for _, l := range splitLines(c.log("virsh")) {
		if h, args, ok := strings.Cut(l, "\t"); ok && h == host {
			out = append(out, args)
		}
	}
	return out
}

// requireUntouched asserts no destroy/undefine of domain ran and nothing was
// removed since the logs were last truncated.
func (c *createHost) requireUntouched(host, domain string) {
	c.t.Helper()
	for _, call := range c.virshCalls(host) {
		assert.NotRegexp(c.t, `^(destroy|undefine) `, call, "the refused domain must not be half-deleted")
	}
	assert.Empty(c.t, c.removals(), "nothing may be removed by a refused delete")
	_, err := os.Stat(filepath.Join(c.root, host, "dom-"+domain+".xml"))
	assert.NoError(c.t, err, "the domain is still defined")
}

// resetLogs truncates the fake host's command logs.
func (c *createHost) resetLogs() {
	for _, tool := range []string{"virsh", "sudo", "qemu-img"} {
		_ = os.Remove(filepath.Join(c.root, tool+".log"))
	}
}

// linkedPair creates team-a/web on the fake host and clones it into
// team-b/copy — a full clone through Clone, or a linked clone the way an
// earlier release made one (legacyLinkedClone: Clone refuses linked clones
// now, but existing ones must stay protected). It returns both disk paths.
func (c *createHost) linkedPair(linked bool) (src, clone string) {
	c.t.Helper()
	ctx := context.Background()
	base := c.file(c.images, "ubuntu.qcow2")
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(c.t, err)
	src = filepath.Join(c.images, "team-a.web-disk.qcow2")
	clone = filepath.Join(c.images, "team-b.copy-disk.qcow2")
	if linked {
		c.legacyLinkedClone(src, clone)
	} else {
		resp, err := c.p.Clone(ctx, cloneReq())
		require.NoError(c.t, err)
		require.Equal(c.t, "team-b.copy", resp.TargetVmID)
	}
	require.FileExists(c.t, src)
	require.FileExists(c.t, clone)
	c.resetLogs()
	return src, clone
}

// legacyLinkedClone makes team-b.copy a linked clone of team-a.web with the
// steps Clone took before linked clones were disabled: a qcow2 overlay backed
// by the source disk, and the source's definition rewritten to use it.
func (c *createHost) legacyLinkedClone(src, clone string) {
	c.t.Helper()
	ctx := context.Background()
	require.NoError(c.t, createLinkedOverlay(ctx, c.vp, src, "qcow2", clone))
	res, err := c.vp.runVirshCommand(ctx, "dumpxml", "team-a.web")
	require.NoError(c.t, err)
	x, _, _, err := rewriteDomainXMLForClone(res.Stdout, "team-b.copy", src, clone)
	require.NoError(c.t, err)
	require.NoError(c.t, c.p.defineDomainFromXML(ctx, c.vp, "team-b.copy", x))
}

// requireDependentsRefusal asserts err is the dependency guard's refusal of op
// on domain, with a message that names no path and no other domain.
func requireDependentsRefusal(t *testing.T, err error, op, domain string, c *createHost) {
	t.Helper()
	var de *diskDependentsError
	require.True(t, stderrors.As(err, &de), "want a disk-dependents refusal, got: %v", err)
	assert.Equal(t, op, de.op)
	assert.Equal(t, 1, de.dependents)
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", domain))
	assert.Contains(t, err.Error(), "delete the linked clones first")
	assert.NotContains(t, err.Error(), c.images, "no host path in the tenant-visible message")
	assert.NotContains(t, err.Error(), "team-b.copy", "never names the other domain")
}

func TestDelete_SingleHost_LinkedClone(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			c := newCreateHost(t)
			src, clone := c.linkedPair(true)
			if running {
				c.liveBackingStore("h1", "team-b.copy", clone, src)
			}
			ctx := context.Background()

			// The source cannot be deleted while its linked clone exists.
			_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
			requireDependentsRefusal(t, err, guardOpDelete, "team-a.web", c)
			c.requireUntouched("h1", "team-a.web")
			assert.FileExists(t, src)

			// Deleting the clone removes its overlay only — never the source
			// disk its <backingStore> (running) or qemu-img chain names — and
			// keeps the source's cloud-init seed, which the clone's CD-ROM
			// references.
			_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-b.copy"})
			require.NoError(t, err)
			assert.Equal(t, []string{clone}, c.removals())
			assert.Len(t, c.stagingEntries(), 1, "the source's seed directory is kept")

			// The clone is gone: the source is deletable again.
			c.resetLogs()
			_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
			require.NoError(t, err)
			assert.Equal(t, []string{src}, c.removals())
			assert.Empty(t, c.stagingEntries(), "the source's own seed directory is removed with it")
		})
	}
}

// TestDomainDiskPaths_RunningLinkedClone pins the disk list every other reader
// shares (clone source resolution, disk info and its allow-list): a running
// linked clone's disks are its own overlay only, never the source disk its
// <backingStore> names.
func TestDomainDiskPaths_RunningLinkedClone(t *testing.T) {
	c := newCreateHost(t)
	src, clone := c.linkedPair(true)
	c.liveBackingStore("h1", "team-b.copy", clone, src)
	disks, err := domainDiskPaths(context.Background(), c.vp, "team-b.copy")
	require.NoError(t, err)
	assert.Equal(t, []string{clone}, disks)
}

func TestDelete_SingleHost_FullCloneIsIndependent(t *testing.T) {
	for _, first := range []string{"team-a.web", "team-b.copy"} {
		t.Run("delete "+first+" first", func(t *testing.T) {
			c := newCreateHost(t)
			src, clone := c.linkedPair(false)
			own := map[string]string{"team-a.web": src, "team-b.copy": clone}
			second := "team-a.web"
			if first == second {
				second = "team-b.copy"
			}
			ctx := context.Background()

			_, err := c.p.Delete(ctx, contracts.VMRef{ID: first})
			require.NoError(t, err, "a full clone and its source never depend on each other")
			assert.Equal(t, []string{own[first]}, c.removals(), "only its own disk")

			c.resetLogs()
			_, err = c.p.Delete(ctx, contracts.VMRef{ID: second})
			require.NoError(t, err)
			assert.Equal(t, []string{own[second]}, c.removals())
		})
	}
}

// TestDelete_SingleHost_NothingOutsideThePoolIsRemoved: a domain whose
// definition names files outside the VM storage directories — directly, or
// through a symlink in the pool — has only its in-pool disk removed; media are
// never removed.
func TestDelete_SingleHost_NothingOutsideThePoolIsRemoved(t *testing.T) {
	c := newCreateHost(t)
	own := c.file(c.images, "web-disk.qcow2")
	outside := c.file(c.outside, "data.qcow2")
	escape := filepath.Join(c.images, "escape.qcow2")
	require.NoError(t, os.Symlink(c.file(c.outside, "secret.qcow2"), escape))
	sub := filepath.Join(c.images, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	nested := c.file(sub, "nested.qcow2")
	iso := c.file(c.images, "installer.iso")

	x := fmt.Sprintf("<domain type='kvm'><name>web</name><uuid>%s</uuid><devices>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='/etc/shadow'/></disk>"+
		"<disk type='file' device='cdrom'><source file='%s'/></disk>"+
		"</devices></domain>", uuidA, own, outside, escape, nested, iso)
	c.define("h1", "web", uuidA, x)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("web\n"), 0o600))

	_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "web"})
	require.NoError(t, err)
	assert.Equal(t, []string{own}, c.removals(), "only the disk directly inside the pool directory")
}

// TestDelete_SingleHost_SymlinkedDiskIsNotFollowed: a disk the definition
// names through a symlink — even one inside the pool pointing at a file inside
// the pool — is not a regular file of this domain: neither the link nor its
// target (possibly another VM's disk) is removed. A directory is not removed
// either; the domain's own regular disk still is.
func TestDelete_SingleHost_SymlinkedDiskIsNotFollowed(t *testing.T) {
	c := newCreateHost(t)
	own := c.file(c.images, "web-disk.qcow2")
	target := c.file(c.images, "other-vm-disk.qcow2")
	link := filepath.Join(c.images, "web-data.qcow2")
	require.NoError(t, os.Symlink(target, link))
	dir := filepath.Join(c.images, "not-a-disk.qcow2")
	require.NoError(t, os.MkdirAll(dir, 0o750))

	x := fmt.Sprintf("<domain type='kvm'><name>web</name><uuid>%s</uuid><devices>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"<disk type='file' device='disk'><source file='%s'/></disk>"+
		"</devices></domain>", uuidA, own, link, dir)
	c.define("h1", "web", uuidA, x)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("web\n"), 0o600))

	_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "web"})
	require.NoError(t, err)
	assert.Equal(t, []string{own}, c.removals(), "never the symlink's target, the symlink or a directory")
	assert.FileExists(t, target)
}

// chainSidecar writes the qemu-img --backing-chain answer for files[0]: each
// file backed by the next.
func (c *createHost) chainSidecar(files ...string) {
	c.t.Helper()
	var links []string
	for i, f := range files {
		if i+1 < len(files) {
			links = append(links, fmt.Sprintf(`{"filename":%q,"format":"qcow2","backing-filename":%q,"full-backing-filename":%q}`,
				f, files[i+1], files[i+1]))
		} else {
			links = append(links, fmt.Sprintf(`{"filename":%q,"format":"qcow2"}`, f))
		}
	}
	require.NoError(c.t, os.WriteFile(files[0]+".chain.json", []byte("["+strings.Join(links, ",")+"]"), 0o600))
}

// TestDelete_SingleHost_RemovesOwnSnapshotChain: a VM whose disk is an
// external-snapshot overlay (<vm>-disk.snap1 over <vm>-disk.qcow2 over a base
// image) is deleted with its own chain files; the base image is never removed,
// and neither is its pre-snapshot disk while a linked clone still reads it.
func TestDelete_SingleHost_RemovesOwnSnapshotChain(t *testing.T) {
	for _, withClone := range []bool{false, true} {
		t.Run(fmt.Sprintf("linked clone of the pre-snapshot disk=%v", withClone), func(t *testing.T) {
			c := newCreateHost(t)
			top := c.file(c.images, "team-a.web-disk.snap1")
			own := c.file(c.images, "team-a.web-disk.qcow2")
			base := c.file(c.images, "golden.qcow2")
			c.chainSidecar(top, own, base)
			c.define("h1", "team-a.web", uuidSource, depDomainXML("team-a.web", uuidSource, top, ""))
			names := "team-a.web\n"
			if withClone {
				clone := c.file(c.images, "team-b.copy-disk.qcow2")
				c.chainSidecar(clone, own, base)
				c.define("h1", "team-b.copy", uuidClone, depDomainXML("team-b.copy", uuidClone, clone, ""))
				names += "team-b.copy\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte(names), 0o600))

			_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web"})
			require.NoError(t, err)
			want := []string{top, own}
			if withClone {
				want = []string{top}
			}
			assert.Equal(t, want, c.removals(), "its own overlay and pre-snapshot disk; never the base image")
		})
	}
}

// TestPowerOn_WarnsWhenLinkedClonesDependOnTheDisk: starting the source of an
// existing linked clone is not refused, but the provider counts the domains
// depending on its disk right after the start and Describe reports the count
// (contracts.ProviderRawLinkedCloneDependentsKey) for the manager's warning; a
// VM nothing depends on reports 0; a VM never started reports nothing.
func TestPowerOn_WarnsWhenLinkedClonesDependOnTheDisk(t *testing.T) {
	c := newCreateHost(t)
	c.p.hostStagingDir = t.TempDir()
	c.linkedPair(true)
	ctx := context.Background()

	_, err := c.p.Power(ctx, contracts.VMRef{ID: "team-a.web"}, contracts.PowerOpOn)
	require.NoError(t, err, "the start is never refused")
	assert.Contains(t, c.virshCalls("h1"), "start team-a.web")
	n, ok := c.p.linkedDeps.get(c.p.hostID, "team-a.web")
	require.True(t, ok)
	assert.Equal(t, 1, n)

	raw := map[string]string{}
	c.p.reportLinkedCloneDependents(c.p.hostID, "team-a.web", raw)
	assert.Equal(t, "1", raw[contracts.ProviderRawLinkedCloneDependentsKey])

	_, err = c.p.Power(ctx, contracts.VMRef{ID: "team-b.copy"}, contracts.PowerOpReboot)
	require.NoError(t, err)
	raw = map[string]string{}
	c.p.reportLinkedCloneDependents(c.p.hostID, "team-b.copy", raw)
	assert.Equal(t, "0", raw[contracts.ProviderRawLinkedCloneDependentsKey], "nothing depends on the clone")

	raw = map[string]string{}
	c.p.reportLinkedCloneDependents(c.p.hostID, "never-started", raw)
	assert.NotContains(t, raw, contracts.ProviderRawLinkedCloneDependentsKey)
}

// TestDeleteDiskFile_RechecksRightBeforeRemoving: a path that stopped being a
// regular file between the plan and the removal (replaced by a symlink) is
// not removed.
func TestDeleteDiskFile_RechecksRightBeforeRemoving(t *testing.T) {
	c := newCreateHost(t)
	p := filepath.Join(c.images, "swapped.qcow2")
	require.NoError(t, os.Symlink(c.file(c.outside, "victim.qcow2"), p))
	require.Error(t, deleteDiskFile(context.Background(), c.vp, p))
	require.NoError(t, deleteDiskFile(context.Background(), c.vp, filepath.Join(c.images, "gone.qcow2")), "already gone")
	assert.Empty(t, c.removals())
}

func TestDelete_SingleHost_UnverifiableDependentsFailClosed(t *testing.T) {
	c := newCreateHost(t)
	_, clone := c.linkedPair(true)
	// The clone's chain can no longer be read: the source must not be deleted
	// on a guess.
	require.NoError(t, os.WriteFile(clone+".chainfail", nil, 0o600))
	require.NoError(t, os.WriteFile(clone+".info.json", []byte("{"), 0o600))

	_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web"})
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "a failed check is retryable: %v", err)
	assert.NotContains(t, err.Error(), c.images)
	c.requireUntouched("h1", "team-a.web")
}

// TestDelete_SingleHost_UnreadableDefinitionLeavesDomainIntact: a domain that
// is listed but whose definition cannot be read is not undefined — that would
// strand its disks — the delete fails retryably and changes nothing.
func TestDelete_SingleHost_UnreadableDefinitionLeavesDomainIntact(t *testing.T) {
	c := newCreateHost(t)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("web\n"), 0o600)) // listed, no definition

	_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "web"})
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "%v", err)
	for _, call := range c.virshCalls("h1") {
		assert.NotRegexp(t, `^(destroy|undefine) `, call, "the domain must not be torn down")
	}
	assert.Empty(t, c.removals())
}

func TestRemoveOrphanedDisks_KeepsBackingFileOfSurvivingClone(t *testing.T) {
	c := newCreateHost(t)
	src, _ := c.linkedPair(true)
	stray := c.file(c.images, "stray-disk.qcow2")

	c.p.removeOrphanedDisks(context.Background(), c.vp, "team-a.web", []string{src, stray, c.file(c.outside, "x-disk.qcow2")})
	assert.Equal(t, []string{stray}, c.removals(),
		"the source's disk is still the backing file of its clone; the outside file is not in the pool")
}

// clusteredCreateHost is createHost's fake host serving a clustered provider's
// host-a (the fake virsh routes on the -c URI), with a source VM and a linked
// clone of it, both owner-stamped.
func clusteredLinkedPair(t *testing.T, running bool) (*createHost, *Provider, string, string) {
	t.Helper()
	c := newCreateHost(t)
	c.host("host-a")
	p, _, _ := routedCluster(t)
	p.imageDirs = []string{c.images} // never the real /var/lib/libvirt/images
	src := c.file(c.images, "team-a.web-disk.qcow2")
	clone := c.file(c.images, "team-b.copy-disk.qcow2")
	c.overlay(clone, src)
	ownerCopy := contracts.ObjectIdentity{UID: "5d7c2b1a-0000-4000-8000-00000000c0b1", Namespace: "team-b", Name: "copy"}
	backing := ""
	if running {
		backing = src
	}
	for _, d := range []struct {
		name, uuid, disk, backing string
		owner                     contracts.ObjectIdentity
	}{
		{"team-a.web", uuidSource, src, "", ownerTeamA},
		{"team-b.copy", uuidClone, clone, backing, ownerCopy},
	} {
		x := strings.Replace(depDomainXML(d.name, d.uuid, d.disk, d.backing), "<devices>", renderOwnerMetadataXML(d.owner)+"<devices>", 1)
		c.define("host-a", d.name, d.uuid, x)
		f, err := os.OpenFile(filepath.Join(c.root, "host-a", "names"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(d.name + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	return c, p, src, clone
}

// TestServer_SnapshotOps_RefusedWhileLinkedCloneExists pins the snapshot side
// of the dependency guard on a single-host provider: SnapshotCreate,
// SnapshotDelete and SnapshotRevert of a VM whose disk backs a linked clone are
// refused (FailedPrecondition + VM_DISK_IN_USE) before any snapshot command
// runs, the clone's own snapshots are unaffected, and the source's are allowed
// again once the clone is gone.
func TestServer_SnapshotOps_RefusedWhileLinkedCloneExists(t *testing.T) {
	c := newCreateHost(t)
	c.linkedPair(true)
	s := c.withRegistry()
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "snapshots"), []byte("snap1\n"), 0o600))
	ctx := context.Background()

	ops := map[string]func(vm string) error{
		"snapshot-create-as": func(vm string) error {
			_, err := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: vm, NameHint: "snap2"})
			return err
		},
		"snapshot-delete": func(vm string) error {
			_, err := s.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: vm, SnapshotId: "snap1"})
			return err
		},
		"snapshot-revert": func(vm string) error {
			_, err := s.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: vm, SnapshotId: "snap1"})
			return err
		},
	}
	ran := func(sub, vm string) bool {
		for _, call := range c.virshCalls("h1") {
			if strings.HasPrefix(call, sub+" "+vm+" ") {
				return true
			}
		}
		return false
	}
	for sub, op := range ops {
		c.resetLogs()
		err := op("team-a.web")
		requireDiskInUseStatus(t, err, false)
		assert.Contains(t, err.Error(), "delete the linked clones first", sub)
		assert.False(t, ran(sub, "team-a.web"), "%s must not reach the source", sub)

		require.NoError(t, op("team-b.copy"), "%s of the clone itself is allowed", sub)
		assert.True(t, ran(sub, "team-b.copy"), sub)
	}

	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-b.copy"})
	require.NoError(t, err)
	for sub, op := range ops {
		c.resetLogs()
		require.NoError(t, op("team-a.web"), "%s of the source is allowed once its clone is gone", sub)
		assert.True(t, ran(sub, "team-a.web"), sub)
	}
}

// TestServer_Delete_DependentsRefusalWireForm pins the wire form of a refused
// delete: FailedPrecondition + VM_DISK_IN_USE on a single-host provider (the
// historical "failed to delete VM" prefix kept), and additionally
// VM_OPERATION_FAILED on the routed clustered path — neither counts toward the
// manager's circuit breaker, which maps both to a Conflict.
func TestServer_Delete_DependentsRefusalWireForm(t *testing.T) {
	t.Run("single-host", func(t *testing.T) {
		c := newCreateHost(t)
		c.linkedPair(true)
		_, err := NewServer(c.p).Delete(context.Background(), &providerv1.DeleteRequest{Id: "team-a.web"})
		requireDiskInUseStatus(t, err, false)
		assert.Contains(t, status.Convert(err).Message(), "failed to delete VM: delete of libvirt domain \"team-a.web\" refused")
		c.requireUntouched("h1", "team-a.web")
	})
	t.Run("clustered", func(t *testing.T) {
		c, p, _, _ := clusteredLinkedPair(t, true)
		_, err := NewServer(p).Delete(context.Background(), &providerv1.DeleteRequest{
			Id: "team-a.web", TargetHostId: "host-a",
			Owner: &providerv1.ObjectIdentity{Uid: ownerTeamA.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name},
		})
		requireDiskInUseStatus(t, err, true)
		assert.Contains(t, status.Convert(err).Message(), "failed to delete VM: delete of libvirt domain \"team-a.web\" refused")
		assert.NotContains(t, status.Convert(err).Message(), c.images)
		c.requireUntouched("host-a", "team-a.web")
	})
}

func TestDelete_Clustered_LinkedClone(t *testing.T) {
	ownerCopy := contracts.ObjectIdentity{UID: "5d7c2b1a-0000-4000-8000-00000000c0b1", Namespace: "team-b", Name: "copy"}
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			c, p, src, clone := clusteredLinkedPair(t, running)
			ctx := context.Background()

			_, err := p.Delete(ctx, contracts.VMRef{ID: "team-a.web", HostID: "host-a", Owner: ownerTeamA})
			requireDependentsRefusal(t, err, guardOpDelete, "team-a.web", c)
			c.requireUntouched("host-a", "team-a.web")

			_, err = p.Delete(ctx, contracts.VMRef{ID: "team-b.copy", HostID: "host-a", Owner: ownerCopy})
			require.NoError(t, err)
			assert.Equal(t, []string{clone}, c.removals(), "the clone's overlay only, never its backing file")

			c.resetLogs()
			_, err = p.Delete(ctx, contracts.VMRef{ID: "team-a.web", HostID: "host-a", Owner: ownerTeamA})
			require.NoError(t, err)
			assert.Equal(t, []string{src}, c.removals())
			assert.Empty(t, c.virshCalls("host-b"), "only the leased host is touched")
		})
	}
}
