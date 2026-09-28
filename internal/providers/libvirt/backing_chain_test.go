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
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// backingInfo is a qemu-img info answer for disk backed by backing (as the
// full-backing-filename), with an optional backing format.
func backingInfo(disk, backing, format string) string {
	fmtField := ""
	if format != "" {
		fmtField = fmt.Sprintf(`,"backing-filename-format":%q`, format)
	}
	return fmt.Sprintf(`{"filename":%q,"format":"qcow2","backing-filename":%q,"full-backing-filename":%q%s}`,
		disk, backing, backing, fmtField)
}

// TestBackingChainFiles_FollowsOnlyLocalRegularFiles pins the chain walk the
// disk in-use check runs as root (sudo -n qemu-img info -U): one image at a
// time, following a backing file only when it is an absolute local path to a
// regular file, in the format its parent names. Anything else fails the
// check closed and is never handed to qemu-img.
func TestBackingChainFiles_FollowsOnlyLocalRegularFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("a local chain is walked one image at a time", func(t *testing.T) {
		h := newFakeHost(t)
		vp := h.host("h1")
		top := h.file(h.images, "top.qcow2")
		mid := h.file(h.images, "mid.qcow2")
		base := h.file(h.images, "base.raw")
		link := filepath.Join(h.images, "mid-link.qcow2")
		require.NoError(t, os.Symlink(mid, link))
		h.info(top, backingInfo(top, link, "qcow2"))
		h.info(link, backingInfo(mid, base, "raw")) // the fake answers by the name it is given

		refs, err := backingChainFiles(ctx, vp, top)
		require.NoError(t, err)
		assert.Subset(t, refs, []string{top, link, mid, base})
		qlog := splitLines(h.log("qemu-img"))
		assert.Equal(t, []string{
			"info -U --output=json -- " + top,
			"info -U -f qcow2 --output=json -- " + link,
			"info -U -f raw --output=json -- " + base,
		}, qlog, "a symlink to a regular file is followed; each level in its named format")
	})

	fifoDir := func(h *fakeHost) string {
		p := filepath.Join(h.images, "fifo.qcow2")
		require.NoError(t, syscall.Mkfifo(p, 0o600))
		return p
	}
	for name, tc := range map[string]struct {
		backing func(h *fakeHost) string
		format  string
	}{
		"nbd":              {backing: func(*fakeHost) string { return "nbd://10.0.0.9:10809/export" }},
		"http":             {backing: func(*fakeHost) string { return "http://images.example/base.qcow2" }},
		"json:":            {backing: func(*fakeHost) string { return `json:{"file":{"driver":"file","filename":"/etc/shadow"}}` }},
		"file: protocol":   {backing: func(*fakeHost) string { return "file:/etc/shadow" }},
		"relative":         {backing: func(*fakeHost) string { return "base.qcow2" }},
		"a FIFO":           {backing: fifoDir},
		"a device":         {backing: func(*fakeHost) string { return "/dev/null" }},
		"a directory":      {backing: func(h *fakeHost) string { return h.outside }},
		"a strange format": {backing: func(h *fakeHost) string { return h.file(h.images, "b.qcow2") }, format: "qcow2 --image-opts"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t)
			vp := h.host("h1")
			top := h.file(h.images, "top.qcow2")
			backing := tc.backing(h)
			h.info(top, backingInfo(top, backing, tc.format))

			_, err := backingChainFiles(ctx, vp, top)
			require.Error(t, err)
			assert.True(t, contracts.IsRetryable(err), "a check that could not run: %v", err)
			assert.NotContains(t, err.Error(), backing, "no host detail in the error")
			assert.Equal(t, []string{"info -U --output=json -- " + top}, splitLines(h.log("qemu-img")),
				"the backing name is never handed to qemu-img")
		})
	}

	t.Run("a loop is cut at the depth limit", func(t *testing.T) {
		h := newFakeHost(t)
		vp := h.host("h1")
		a := h.file(h.images, "a.qcow2")
		b := h.file(h.images, "b.qcow2")
		h.info(a, backingInfo(a, b, "qcow2"))
		h.info(b, backingInfo(b, a, "qcow2"))
		_, err := backingChainFiles(ctx, vp, a)
		require.Error(t, err)
		assert.Len(t, splitLines(h.log("qemu-img")), maxBackingChainDepth+1)
	})

	t.Run("a missing backing file ends the chain", func(t *testing.T) {
		h := newFakeHost(t)
		vp := h.host("h1")
		top := h.file(h.images, "top.qcow2")
		gone := filepath.Join(h.images, "gone.qcow2")
		h.info(top, backingInfo(top, gone, ""))
		refs, err := backingChainFiles(ctx, vp, top)
		require.NoError(t, err)
		assert.Contains(t, refs, gone, "recorded from the header")
	})
}

// TestDiskDependents_NonLocalBackingFailsTheCheck: a domain on the host whose
// disk names a protocol backing file (which could reach any file, e.g. a json:
// or nbd: export of the requester's disk) makes the dependency check fail
// closed — VM_DISK_CHECK_FAILED, not "no dependents".
func TestDiskDependents_NonLocalBackingFailsTheCheck(t *testing.T) {
	h, vp, src, clone := linkedCloneHost(t, false)
	h.info(clone, backingInfo(clone, `json:{"file":{"driver":"file","filename":"`+src+`"}}`, ""))

	err := refuseIfDiskHasDependents(context.Background(), vp, "team-a.template", guardOpSnapshotRevert)
	var dc *diskCheckFailedError
	require.ErrorAs(t, err, &dc, "%v", err)
}

// TestBackingChainFiles_TopImageAndDataFiles: the disk itself is checked like
// every backing file — a FIFO or a device at the top is refused before
// qemu-img opens it — and a qcow2 external data file is recorded (so a file
// used as another domain's data file counts as in use) but never opened by
// the walk; one named by anything but a local path fails the check closed.
func TestBackingChainFiles_TopImageAndDataFiles(t *testing.T) {
	ctx := context.Background()

	for name, top := range map[string]func(h *fakeHost) string{
		"a FIFO": func(h *fakeHost) string {
			p := filepath.Join(h.images, "fifo-disk.qcow2")
			require.NoError(t, syscall.Mkfifo(p, 0o600))
			return p
		},
		"a device": func(*fakeHost) string { return "/dev/null" },
	} {
		t.Run("top is "+name, func(t *testing.T) {
			h := newFakeHost(t)
			vp := h.host("h1")
			_, err := backingChainFiles(ctx, vp, top(h))
			require.Error(t, err)
			assert.Empty(t, h.log("qemu-img"), "qemu-img never opens it")
		})
	}

	t.Run("a local data file is recorded, not opened", func(t *testing.T) {
		h := newFakeHost(t)
		vp := h.host("h1")
		disk := h.file(h.images, "vm-disk.qcow2")
		data := h.file(h.images, "vm-data.raw")
		h.info(disk, `{"filename":"`+disk+`","format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"`+data+`"}}}`)
		refs, err := backingChainFiles(ctx, vp, disk)
		require.NoError(t, err)
		assert.Contains(t, refs, data)
		assert.Equal(t, []string{"info -U --output=json -- " + disk}, splitLines(h.log("qemu-img")))
	})

	t.Run("a non-local data file fails the check", func(t *testing.T) {
		h := newFakeHost(t)
		vp := h.host("h1")
		disk := h.file(h.images, "vm-disk.qcow2")
		h.info(disk, `{"filename":"`+disk+`","format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"nbd://10.0.0.9/x"}}}`)
		_, err := backingChainFiles(ctx, vp, disk)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "10.0.0.9")
	})
}
