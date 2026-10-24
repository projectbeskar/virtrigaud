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
	"slices"
	"sort"
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

// These tests pin V11 (v0.4.0-rc8 lab validation, libvirt 10): a memory
// (internal) snapshot taken on the overlay of a disk-only (external) snapshot
// is stored inside that overlay, and libvirt's delete of the external snapshot
// commits the overlay away with it. They run on the in-memory mediaHost with
// its V11 model on (newV11Host): image files with their backing files and the
// internal snapshots stored in them, the snapshot tree, and libvirt 10's
// snapshot delete (qemu_snapshot.c: the merged file is the first child's
// recorded source, or the domain's current one; it is block-committed into its
// backing file and removed; only external snapshots' recorded sources are
// updated; an internal snapshot of a running domain whose disk is not the file
// it was taken on is refused).

// v11Image is one image file of the V11 model.
type v11Image struct {
	format  string
	backing string
	// tags are the internal snapshots stored in the file.
	tags []string
}

// v11Overlay is the overlay snapshot "live" puts over ns.vm's disk.
var v11Overlay = overlayName(v6Disk, "live")

// newV11Host is a RUNNING ns.vm (vda on its own qcow2 disk, the seed CD-ROM
// hda) with the V11 model on.
func newV11Host() *mediaHost {
	h := newMediaHost()
	h.state = "running"
	h.files = map[string]*v11Image{v6Disk: {format: "qcow2"}}
	h.hostStderr = map[string]string{}
	return h
}

// v11Virsh is the V11 model's virsh: snapshot-list, snapshot-create-as,
// snapshot-dumpxml and snapshot-delete; anything else is mediaHost's.
func (h *mediaHost) v11Virsh(args []string, line string) (*hostconn.Result, bool, error) {
	ok := func(s string) (*hostconn.Result, bool, error) { return &hostconn.Result{Stdout: s}, true, nil }
	switch args[0] {
	case "snapshot-list":
		return ok(strings.Join(h.order, "\n") + "\n")
	case "snapshot-create-as":
		name := args[2]
		diskOnly := strings.Contains(line, "--disk-only")
		excluded := map[string]bool{}
		for i, a := range args {
			if a == "--diskspec" && i+1 < len(args) {
				excluded[strings.TrimSuffix(args[i+1], diskspecNoSnapshot)] = true
			}
		}
		s := &mediaSnapshot{disks: append([]mediaDisk{}, h.disks...), marker: h.marker, internal: !diskOnly,
			parent: h.current, overlays: map[string]string{}}
		for i, d := range h.disks {
			if d.readonly || d.source == "" || excluded[d.target] {
				continue
			}
			if diskOnly {
				o := overlayName(d.source, name)
				h.files[o] = &v11Image{format: "qcow2", backing: d.source}
				h.disks[i].source, h.disks[i].driver = o, "qcow2"
				s.overlays[d.target] = o
				continue
			}
			if f := h.files[d.source]; f != nil {
				f.tags = append(f.tags, name)
			}
		}
		h.snaps[name] = s
		h.order = append(h.order, name)
		h.current = name
		return ok("Domain snapshot " + name + " created")
	case "snapshot-dumpxml":
		s, found := h.snaps[args[2]]
		if !found {
			return nil, true, exited("snapshot not found")
		}
		if s.xml != "" {
			return ok(s.xml)
		}
		return ok(h.v11SnapshotXML(args[2], s))
	case "snapshot-delete":
		res, err := h.v11Delete(args[2], slices.Contains(args, snapshotDeleteMetadataOnly))
		return res, true, err
	}
	return nil, false, nil
}

// v11SnapshotXML renders snapshot name as `virsh snapshot-dumpxml` does: its
// parent, memory and disk modes (an external disk with its overlay), and the
// definition it recorded.
func (h *mediaHost) v11SnapshotXML(name string, s *mediaSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<domainsnapshot><name>%s</name>", name)
	if s.parent != "" {
		fmt.Fprintf(&b, "<parent><name>%s</name></parent>", s.parent)
	}
	memory := "no"
	if s.internal {
		memory = snapshotInternalMode
	}
	fmt.Fprintf(&b, "<memory snapshot='%s'/><disks>", memory)
	for _, d := range s.disks {
		switch {
		case s.internal && !d.readonly && d.source != "":
			fmt.Fprintf(&b, "<disk name='%s' snapshot='internal'/>", d.target)
		case s.overlays[d.target] != "":
			fmt.Fprintf(&b, "<disk name='%s' snapshot='external' type='file'><driver type='qcow2'/><source file='%s'/></disk>",
				d.target, s.overlays[d.target])
		default:
			fmt.Fprintf(&b, "<disk name='%s' snapshot='no'/>", d.target)
		}
	}
	b.WriteString("</disks>" + renderMediaDomain(s.disks, s.marker) + "</domainsnapshot>")
	return b.String()
}

// storedSource is the source snapshot s recorded for target.
func (s *mediaSnapshot) storedSource(target string) string {
	for _, d := range s.disks {
		if d.target == target {
			return d.source
		}
	}
	return ""
}

// covers reports whether snapshot s includes target (libvirt's
// qemuSnapshotFindParentSnapForDisk: a disk not 'no').
func (s *mediaSnapshot) covers(target string) bool {
	if s.internal {
		for _, d := range s.disks {
			if d.target == target && !d.readonly && d.source != "" {
				return true
			}
		}
		return false
	}
	return s.overlays[target] != ""
}

// v11Delete is libvirt 10's snapshot-delete of name (see the file comment).
func (h *mediaHost) v11Delete(name string, metadataOnly bool) (*hostconn.Result, error) {
	s, found := h.snaps[name]
	if !found {
		return nil, exited("Domain snapshot not found")
	}
	if !metadataOnly && s.internal {
		for _, d := range s.disks {
			if d.readonly || d.source == "" {
				continue
			}
			if h.state == "running" && h.source(d.target) != d.source {
				return nil, exited(fmt.Sprintf("Operation not supported: disk image '%s' for internal snapshot '%s' is not "+
					"the same as disk image currently used by VM", d.target, name))
			}
		}
		for _, d := range s.disks {
			if f := h.files[d.source]; f != nil && !d.readonly {
				f.tags = slices.DeleteFunc(f.tags, func(t string) bool { return t == name })
			}
		}
	}
	if !metadataOnly && !s.internal {
		var children []string
		for _, n := range h.order {
			if h.snaps[n].parent == name {
				children = append(children, n)
			}
		}
		for target := range s.overlays {
			for p := h.snaps[s.parent]; p != nil; p = h.snaps[p.parent] {
				if p.covers(target) {
					if p.internal {
						return nil, exited("Operation not supported: deleting external snapshot that has internal snapshot as parent not supported")
					}
					break
				}
			}
		}
		for target, overlay := range s.overlays {
			var merged string
			switch {
			case len(children) > 0:
				merged = h.snaps[children[0]].storedSource(target)
			case h.current == name:
				merged = h.source(target)
			default:
				delete(h.files, s.storedSource(target)) // a leaf that is not current: unlinked
				continue
			}
			if merged == "" {
				merged = overlay
			}
			img := h.files[merged]
			if img == nil {
				return nil, exited("failed to find disk source in backing chain")
			}
			base := img.backing
			delete(h.files, merged) // committed into base with VIR_DOMAIN_BLOCK_COMMIT_DELETE
			if i := h.diskIdx(target); i >= 0 && h.disks[i].source == merged {
				h.disks[i].source = base
			}
			for _, f := range h.files {
				if f.backing == merged {
					f.backing = base
				}
			}
			for _, o := range h.snaps {
				if o.internal {
					continue // libvirt updates external snapshots' recorded sources only
				}
				for i := range o.disks {
					if o.disks[i].target == target && o.disks[i].source == merged {
						o.disks[i].source = base
					}
				}
			}
		}
	}
	for _, o := range h.snaps {
		if o.parent == name {
			o.parent = s.parent
		}
	}
	if h.current == name {
		h.current = s.parent
	}
	delete(h.snaps, name)
	h.order = slices.DeleteFunc(h.order, func(n string) bool { return n == name })
	return &hostconn.Result{Stdout: "Domain snapshot " + name + " deleted"}, nil
}

// RunHost is mediaConn's host shell: with the V11 model on it answers the
// file-kind check and `qemu-img info` (directly or through `sudo -n`) from
// the model's files; anything else is fakeSeamConn's.
func (c *mediaConn) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	h := c.h
	if h.files == nil {
		return c.fakeSeamConn.RunHost(ctx, argv...)
	}
	line := strings.Join(argv, " ")
	h.calls = append(h.calls, "! "+line)
	for prefix, err := range h.fail {
		if strings.HasPrefix("! "+line, prefix) {
			return nil, err
		}
	}
	for prefix, stderr := range h.hostStderr {
		if strings.HasPrefix("! "+line, prefix) {
			return &hostconn.Result{ExitCode: 1, Stderr: stderr}, exited(stderr)
		}
	}
	path := argv[len(argv)-1]
	switch {
	case len(argv) > 2 && argv[0] == "sh" && argv[2] == backingKindScript:
		if h.files[path] != nil {
			return &hostconn.Result{Stdout: backingKindFile + "\n"}, nil
		}
		return &hostconn.Result{}, nil
	case slices.Contains(argv, "qemu-img") && slices.Contains(argv, "info"):
		img := h.files[path]
		if img == nil {
			msg := fmt.Sprintf("qemu-img: Could not open '%s': Could not open '%s': %s", path, path, qemuImgMissingFile)
			return &hostconn.Result{ExitCode: 1, Stderr: msg}, exited(msg)
		}
		info := map[string]any{"format": img.format, "filename": path}
		if img.backing != "" {
			info["backing-filename"], info["full-backing-filename"] = img.backing, img.backing
			info["backing-filename-format"] = h.files[img.backing].format
		}
		var snaps []map[string]string
		for _, t := range img.tags {
			snaps = append(snaps, map[string]string{"name": t})
		}
		if snaps != nil {
			info["snapshots"] = snaps
		}
		out, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}
		return &hostconn.Result{Stdout: string(out)}, nil
	}
	return c.fakeSeamConn.RunHost(ctx, argv...)
}

// v11Delete runs the SnapshotDelete core on h (the overlay cleanup is
// recorded, never run).
func v11DeleteSnap(h *mediaHost, name string) error {
	_, err := snapshotDeleteOn(context.Background(), &fakeSeamProvider{}, newMediaConn(h), v6Domain, name)
	return err
}

// v11Lab is the lab's sequence up to the deletes: disk-only snapshot "live"
// of the running VM, then memory snapshot "mem" on its overlay.
func v11Lab(t *testing.T) *mediaHost {
	t.Helper()
	h := newV11Host()
	require.NoError(t, createSnap(t, h, "live", false, v6Roots))
	require.Equal(t, v11Overlay, h.source("vda"), "vda moved onto the overlay")
	require.NoError(t, createSnap(t, h, "mem", true, v6Roots))
	require.Equal(t, []string{"mem"}, h.files[v11Overlay].tags, "the memory snapshot is stored inside the overlay")
	h.calls = nil
	return h
}

// requireDeleteRefused asserts a definite V11 refusal naming want, with
// nothing sent and no host path.
func requireDeleteRefused(t *testing.T, h *mediaHost, err error, want ...string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status: %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code(), "%v", err)
	assert.Contains(t, st.Message(), "delete refused, nothing changed: delete snapshot")
	for _, w := range want {
		assert.Contains(t, st.Message(), fmt.Sprintf("%q", w))
	}
	assert.NotContains(t, st.Message(), "/", "no host path crosses the wire")
	assert.False(t, h.called("snapshot-delete"), "nothing is deleted: %v", h.calls)
}

// TestV11Model_ReproducesTheLab: the model, driven with the bare virsh
// commands rc8 sent, loses the memory snapshot exactly as the lab did.
func TestV11Model_ReproducesTheLab(t *testing.T) {
	h := v11Lab(t)
	_, err := h.virsh([]string{"snapshot-delete", v6Domain, "live"})
	require.NoError(t, err, "libvirt reports success")
	assert.Nil(t, h.files[v11Overlay], "the overlay was committed away, and the memory snapshot in it")
	assert.Equal(t, v6Disk, h.source("vda"))
	_, err = h.virsh([]string{"snapshot-delete", v6Domain, "mem"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not the same as disk image currently used by VM")
}

// TestSnapshotDelete_V11_LabSequence: deleting the disk-only snapshot first
// is refused before anything changes; deleting the memory snapshot first
// works, and then the disk-only one.
func TestSnapshotDelete_V11_LabSequence(t *testing.T) {
	h := v11Lab(t)

	err := v11DeleteSnap(h, "live")
	requireDeleteRefused(t, h, err, "mem", "live")
	assert.Equal(t, []string{"mem"}, h.files[v11Overlay].tags, "the memory snapshot is intact")
	assert.Contains(t, h.calls, "! sudo -n qemu-img info -U -f qcow2 --output=json -- "+v11Overlay,
		"the overlay's header is read with the documented root-read shape")

	h.calls = nil
	require.NoError(t, v11DeleteSnap(h, "mem"), "the memory snapshot is deleted first")
	assert.Contains(t, h.calls, "snapshot-delete "+v6Domain+" mem")
	assert.Empty(t, h.files[v11Overlay].tags)

	require.NoError(t, v11DeleteSnap(h, "live"), "then the disk-only snapshot")
	assert.Nil(t, h.files[v11Overlay], "committed into the base")
	assert.Equal(t, v6Disk, h.source("vda"))
	assert.Empty(t, h.snaps)
}

// TestSnapshotDelete_V11_ExternalDescendants: an external snapshot with only
// external descendants is deleted (libvirt commits its overlay under the
// child's); a memory snapshot stored in a LATER overlay does not block it, but
// blocks the delete of the snapshot whose overlay it is stored in.
func TestSnapshotDelete_V11_ExternalDescendants(t *testing.T) {
	t.Run("only external descendants", func(t *testing.T) {
		h := newV11Host()
		require.NoError(t, createSnap(t, h, "live", false, v6Roots))
		require.NoError(t, createSnap(t, h, "live2", false, v6Roots))
		h.calls = nil
		require.NoError(t, v11DeleteSnap(h, "live"))
		assert.True(t, h.called("snapshot-delete "+v6Domain+" live"))
		assert.Contains(t, h.snaps, "live2")
		assert.Equal(t, v6Disk, h.files[overlayName(v11Overlay, "live2")].backing)
	})
	t.Run("a memory snapshot in a later overlay", func(t *testing.T) {
		h := newV11Host()
		require.NoError(t, createSnap(t, h, "live", false, v6Roots))
		require.NoError(t, createSnap(t, h, "live2", false, v6Roots))
		require.NoError(t, createSnap(t, h, "mem", true, v6Roots))
		overlay2 := overlayName(v11Overlay, "live2")
		require.Equal(t, []string{"mem"}, h.files[overlay2].tags)

		h.calls = nil
		requireDeleteRefused(t, h, v11DeleteSnap(h, "live2"), "mem")

		h.calls = nil
		require.NoError(t, v11DeleteSnap(h, "live"), "mem is not stored in what this delete merges")
		assert.Equal(t, []string{"mem"}, h.files[overlay2].tags, "and is intact")
	})
}

// TestSnapshotDelete_V11_HeaderAloneBlocks: an internal snapshot libvirt's
// metadata does not know of (only the overlay's own header lists it) blocks
// the delete too.
func TestSnapshotDelete_V11_HeaderAloneBlocks(t *testing.T) {
	h := newV11Host()
	require.NoError(t, createSnap(t, h, "live", false, v6Roots))
	h.files[v11Overlay].tags = []string{"made-elsewhere"}
	h.calls = nil
	requireDeleteRefused(t, h, v11DeleteSnap(h, "live"), "made-elsewhere")
}

// TestSnapshotDelete_V11_FailsClosed: a check that cannot be completed
// refuses the delete — retryable, nothing sent.
func TestSnapshotDelete_V11_FailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		setup      func(h *mediaHost)
		code       codes.Code
		unreadable bool
	}{
		"the snapshot list fails":         {setup: func(h *mediaHost) { h.fail["snapshot-list"] = exited("scripted") }, code: codes.Unavailable},
		"another snapshot cannot be read": {setup: func(h *mediaHost) { h.fail["snapshot-dumpxml "+v6Domain+" live2"] = exited("scripted") }, code: codes.Unavailable},
		"an overlay header cannot be read": {setup: func(h *mediaHost) {
			h.hostStderr["! sudo -n qemu-img info"] = "qemu-img: Could not open: Image is not in qcow2 format"
		}, code: codes.Unavailable},
		"permission denied": {setup: func(h *mediaHost) {
			h.hostStderr["! sudo -n qemu-img info"] = "qemu-img: Could not open '" + v11Overlay + "': " + qemuImgPermissionDenied
		}, code: codes.Unavailable, unreadable: true},
		"the host drops": {setup: func(h *mediaHost) { h.fail["! sudo -n qemu-img info"] = dropped() }, code: codes.Unavailable},
		"an external disk with no file": {setup: func(h *mediaHost) {
			h.snaps["live"].xml = "<domainsnapshot><name>live</name><disks><disk name='vda' snapshot='external' type='block'>" +
				"<source dev='/dev/vg/live'/></disk></disks><domain><devices/></domain></domainsnapshot>"
		}, code: codes.Unavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := newV11Host()
			require.NoError(t, createSnap(t, h, "live", false, v6Roots))
			require.NoError(t, createSnap(t, h, "live2", false, v6Roots)) // another snapshot to read
			tc.setup(h)
			h.calls = nil
			err := v11DeleteSnap(h, "live")
			require.Error(t, err)
			st, ok := status.FromError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.code, st.Code(), "%v", err)
			assert.False(t, h.called("snapshot-delete"), "nothing is deleted: %v", h.calls)
			assert.NotContains(t, st.Message(), "/var/lib", "no host path crosses the wire")
			if tc.unreadable {
				assert.Contains(t, st.Message(), "permission denied reading a disk image")
				assert.Contains(t, st.Message(), snapshotKeepsOthersCheck)
			}
		})
	}
}

// TestSnapshotDelete_V11_OrphanedMemorySnapshot: a memory snapshot an earlier
// release let a delete destroy (the lab's state after step 4) has only its
// metadata deleted once its data is provably gone; anything short of that
// proof is a definite refusal, or a retryable one when the proof cannot run.
func TestSnapshotDelete_V11_OrphanedMemorySnapshot(t *testing.T) {
	orphaned := func(t *testing.T) *mediaHost {
		t.Helper()
		h := v11Lab(t)
		_, err := h.virsh([]string{"snapshot-delete", v6Domain, "live"}) // what rc8 did
		require.NoError(t, err)
		h.calls = nil
		return h
	}

	t.Run("proven gone: metadata only", func(t *testing.T) {
		h := orphaned(t)
		require.NoError(t, v11DeleteSnap(h, "mem"))
		assert.Equal(t, []string{"snapshot-delete " + v6Domain + " mem", "snapshot-delete " + v6Domain + " mem --metadata"},
			filterCalls(h.calls, "snapshot-delete"))
		assert.Empty(t, h.snaps)
		assert.Contains(t, h.files, v6Disk, "no disk file is touched")
	})
	t.Run("its file still exists: power off first", func(t *testing.T) {
		h := newV11Host()
		require.NoError(t, createSnap(t, h, "mem", true, v6Roots)) // stored in the base
		require.NoError(t, createSnap(t, h, "live", false, v6Roots))
		h.calls = nil
		err := v11DeleteSnap(h, "mem")
		requireStaleRefusal(t, h, err)
		assert.Equal(t, []string{"mem"}, h.files[v6Disk].tags)
	})
	t.Run("the current chain still lists it", func(t *testing.T) {
		h := orphaned(t)
		h.files[v6Disk].tags = []string{"mem"}
		requireStaleRefusal(t, h, v11DeleteSnap(h, "mem"))
	})
	t.Run("the proof cannot be read", func(t *testing.T) {
		h := orphaned(t)
		h.fail["! sudo -n qemu-img info -U -f qcow2 --output=json -- "+v11Overlay] = exited("scripted")
		err := v11DeleteSnap(h, "mem")
		require.Error(t, err)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.Unavailable, st.Code(), "retryable: %v", err)
		assert.NotContains(t, filterCalls(h.calls, "snapshot-delete"), "snapshot-delete "+v6Domain+" mem --metadata")
	})
}

// requireStaleRefusal asserts the definite refusal of a memory snapshot
// libvirt cannot delete while the VM runs: no metadata-only delete.
func requireStaleRefusal(t *testing.T, h *mediaHost, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code(), "%v", err)
	assert.Contains(t, st.Message(), "delete refused, nothing changed: power the VM off and delete it again")
	assert.Contains(t, st.Message(), `"mem"`)
	assert.NotContains(t, st.Message(), "/")
	assert.NotContains(t, filterCalls(h.calls, "snapshot-delete"), "snapshot-delete "+v6Domain+" mem --metadata")
}

// filterCalls returns the calls starting with prefix.
func filterCalls(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// TestSingleHost_SnapshotDelete_V11_OnTheWire: through the Server, the
// refusal is a bare FailedPrecondition (no ErrorInfo).
func TestSingleHost_SnapshotDelete_V11_OnTheWire(t *testing.T) {
	h := v11Lab(t)
	s := &Server{provider: &fakeSeamProvider{hostConn: newMediaConn(h)}}
	_, err := s.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: v6Domain, SnapshotId: "live"})
	requireDeleteRefused(t, h, err, "mem")
	st, _ := status.FromError(err)
	assert.Empty(t, errorInfoReasons(st))
	assert.True(t, strings.HasPrefix(st.Message(), "failed to delete snapshot: delete refused, nothing changed: delete snapshot \"mem\" first"),
		st.Message())
}

// v11LiveXML and v11MemXML are the lab's two snapshots of vrgold-web as
// libvirt 10 records them: "live" disk-only on vda, "mem" a memory snapshot
// on live's overlay.
func v11LiveXML() string {
	return "<domainsnapshot><name>live</name><state>disk-snapshot</state><memory snapshot='no'/><disks>" +
		"<disk name='vda' snapshot='external' type='file'><driver type='qcow2'/><source file='" + overlayName(scdDiskPath, "live") + "'/></disk>" +
		"<disk name='sda' snapshot='no'/></disks>" + scdDomainXML(scdDomain, scdDomainOpts{}) + "</domainsnapshot>"
}

func v11MemXML() string {
	return "<domainsnapshot><name>mem</name><parent><name>live</name></parent><state>running</state><memory snapshot='internal'/><disks>" +
		"<disk name='vda' snapshot='internal'/><disk name='sda' snapshot='no'/></disks>" +
		strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}), scdDiskPath, overlayName(scdDiskPath, "live"), 1) + "</domainsnapshot>"
}

// TestSnapshotDelete_V11_ProviderAndRoutedWireForms: the contracts.Provider
// path (single host) and the routed, owner-checked clustered path refuse the
// lab's delete from libvirt's metadata — FailedPrecondition, and on the
// routed path with VM_OPERATION_FAILED — and never send it; an orphaned memory
// snapshot whose file the fake host still reads is a definite refusal on both.
func TestSnapshotDelete_V11_ProviderAndRoutedWireForms(t *testing.T) {
	ctx := context.Background()
	seed := func(fx *scdFixture, host, dom string) {
		fx.script(host, "snaps-"+dom, "live\nmem\n")
		fx.script(host, "snapxml-live", v11LiveXML())
		fx.script(host, "snapxml-mem", v11MemXML())
	}

	t.Run("single host, contracts.Provider", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		seed(fx, "single", scdDomain)
		_, err := fx.p.SnapshotDelete(ctx, contracts.VMRef{ID: scdDomain}, "live")
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, codes.FailedPrecondition, st.Code())
		assert.Contains(t, st.Message(), `delete refused, nothing changed: delete snapshot "mem" first`)
		assert.False(t, called(fx.calls(), "single virsh snapshot-delete"), "%v", fx.calls())
	})
	t.Run("routed", func(t *testing.T) {
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		seed(fx.scdFixture, "host-b", routingDomainUUID)
		_, err := NewServer(fx.p).SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{
			VmId: "web", SnapshotId: "live", TargetHostId: "host-b", Owner: teamAOwner,
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, codes.FailedPrecondition, st.Code())
		assert.True(t, errorInfoReasons(st)[contracts.VMOperationFailedReason], "a per-VM answer on its host")
		assert.Equal(t, `failed to delete snapshot: delete refused, nothing changed: delete snapshot "mem" first; it is stored in a `+
			`disk file that deleting snapshot "live" would merge away, and would be lost`, st.Message())
		assert.False(t, called(fx.calls(), "host-b virsh snapshot-delete"), "%v", fx.calls())
		assert.Contains(t, fx.calls(), "host-b virsh snapshot-dumpxml "+routingDomainUUID+" mem", "read by the owner-checked UUID")
	})
	t.Run("routed, an orphaned memory snapshot", func(t *testing.T) {
		fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
		fx.script("host-b", "snaps-"+routingDomainUUID, "mem\n")
		fx.script("host-b", "snapxml-mem", v11MemXML())
		fx.script("host-b", "refuse-snapshot-delete", "error: Failed to delete snapshot mem\nerror: Operation not supported: "+
			"disk image 'vda' for internal snapshot 'mem' is not the same as disk image currently used by VM\n")
		_, err := NewServer(fx.p).SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{
			VmId: "web", SnapshotId: "mem", TargetHostId: "host-b", Owner: teamAOwner,
		})
		require.Error(t, err)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.FailedPrecondition, st.Code(), "%v", err)
		assert.True(t, errorInfoReasons(st)[contracts.VMOperationFailedReason])
		assert.Contains(t, st.Message(), "power the VM off and delete it again")
		assert.NotContains(t, fx.calls(), "host-b virsh snapshot-delete "+routingDomainUUID+" mem --metadata",
			"the fake host still reads the file mem was taken on: not proven gone")
	})
}

// TestSnapshotDeleteMessages_GuidanceSurvivesTheStatusCap: the guidance
// comes first, so it survives the manager's 256-byte cap with the longest
// names; default-named snapshots' messages fit whole, prefix included.
func TestSnapshotDeleteMessages_GuidanceSurvivesTheStatusCap(t *testing.T) {
	const prefix = "snapshotDelete failed: failed to delete snapshot: "
	long := strings.Repeat("s", 64)
	for _, tc := range []struct {
		err   error
		guide string
	}{
		{&snapshotDeleteLosesSnapshotsError{snapshot: long, blocking: []string{long + "a", long + "b", long + "c", long + "d"}},
			`delete refused, nothing changed: delete snapshots "` + long},
		{&snapshotDeleteStaleInternalError{snapshot: long}, "delete refused, nothing changed: power the VM off and delete it again"},
	} {
		kept := prefix + tc.err.Error()
		if len(kept) > managerDetailBytes {
			kept = kept[:managerDetailBytes]
		}
		assert.Contains(t, kept, tc.guide)
	}
	for _, err := range []error{
		&snapshotDeleteLosesSnapshotsError{snapshot: "snapshot-1792831471", blocking: []string{"snapshot-1792831472"}},
		&snapshotDeleteStaleInternalError{snapshot: "snapshot-1792831471"},
	} {
		assert.LessOrEqual(t, len(prefix+err.Error()), managerDetailBytes, "fits whole: %q", err.Error())
	}
	five := []string{"a", "b", "c", "d", "e"}
	sort.Strings(five)
	assert.Equal(t, `"a", "b", "c" and 2 more`, quotedSnapshotNames(five))
	assert.Equal(t, `"`+strings.Repeat("x", maxSnapshotNameBytes)+`…"`, quoteSnapshotName(strings.Repeat("x", 100)))
}
