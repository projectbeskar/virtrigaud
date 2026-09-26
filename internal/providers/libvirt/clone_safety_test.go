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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the clone hardening of the delete-safety fix: linked clones
// are refused, a clone's disk is never world-writable, and its UEFI varstore
// is never written through a symlink or over another domain's varstore.

// cloneReq full-clones team-a.web into team-b/copy.
func cloneReq() contracts.CloneRequest {
	return contracts.CloneRequest{
		Source:     contracts.VMRef{ID: "team-a.web"},
		TargetName: "copy",
		TargetVM:   contracts.ObjectIdentity{Namespace: "team-b", Name: "copy"},
	}
}

// TestClone_LinkedIsRefusedBeforeAnyHostCommand pins that libvirt linked
// clones are disabled: a Linked request is an sdk InvalidSpec (gRPC
// InvalidArgument) naming FullClone, and no host command runs — the provider's
// host connection refuses and counts any call.
func TestClone_LinkedIsRefusedBeforeAnyHostCommand(t *testing.T) {
	p := &Provider{virshProvider: newUnroutableVirshProvider()}
	req := cloneReq()
	req.Linked = true

	_, err := p.Clone(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	assert.Contains(t, err.Error(), "linked clones are disabled on libvirt in this release")
	assert.Contains(t, err.Error(), "use FullClone")

	_, err = NewServer(p).Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "team-a.web", TargetName: "copy", Linked: true,
		TargetVm: &providerv1.ObjectIdentity{Namespace: "team-b", Name: "copy"},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a non-retryable answer the breaker never counts")
	assert.Contains(t, status.Convert(err).Message(), "use FullClone")
	assert.Zero(t, p.virshProvider.unroutableHits.Load(), "no host command may run")
}

func TestClone_FullCloneIsUnchanged(t *testing.T) {
	c := newCreateHost(t)
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	c.resetLogs()

	resp, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	assert.Equal(t, "team-b.copy", resp.TargetVmID)
	disk := filepath.Join(c.images, "team-b.copy-disk.qcow2")
	assert.FileExists(t, disk)
	assert.Contains(t, c.log("qemu-img"), "convert -O qcow2 "+filepath.Join(c.images, "team-a.web-disk.qcow2")+" "+disk,
		"an independent, flattened copy")
	assert.NotContains(t, c.log("qemu-img"), "create", "no overlay")
	assert.NoFileExists(t, disk+".chain.json", "no backing file")
	assert.Contains(t, c.domainXML("team-b.copy"), "<source file='"+disk+"'/>")
}

func TestClone_DiskIsNotWorldWritable(t *testing.T) {
	c := newCreateHost(t)
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	c.resetLogs()

	_, err = c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	disk := filepath.Join(c.images, "team-b.copy-disk.qcow2")
	sudo := splitLines(c.log("sudo"))
	assert.Contains(t, sudo, "chown libvirt-qemu:kvm "+disk)
	assert.Contains(t, sudo, "chmod 0660 "+disk, "owner and group only")
	for _, l := range sudo {
		assert.NotContains(t, l, "777", "no world-writable guest disk on a shared host")
	}
}

// uefiSource creates team-a/web and gives it a per-VM UEFI varstore in a
// scratch nvram directory; it returns the source varstore and the path a clone
// named team-b.copy gets (rewriteNVRAMPath).
func (c *createHost) uefiSource() (src, target string) {
	c.t.Helper()
	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(c.t, err)
	nv := filepath.Join(c.base, "nvram")
	require.NoError(c.t, os.MkdirAll(nv, 0o750))
	src = c.file(nv, "team-a.web_VARS.fd")
	dir := filepath.Join(c.root, "h1")
	x, err := os.ReadFile(filepath.Join(dir, "dom-team-a.web.xml")) //nolint:gosec // test reads its own fixture
	require.NoError(c.t, err)
	require.Contains(c.t, string(x), "</os>")
	uefi := strings.Replace(string(x), "</os>", "<nvram>"+src+"</nvram></os>", 1)
	d, err := parseDomainDisks(uefi)
	require.NoError(c.t, err)
	for _, key := range []string{"team-a.web", d.UUID} {
		require.NoError(c.t, os.WriteFile(filepath.Join(dir, "dom-"+key+".xml"), []byte(uefi), 0o600))
	}
	c.resetLogs()
	return src, filepath.Join(nv, "team-b.copy_VARS.fd")
}

func TestClone_NVRAMCopyNeverFollowsSymlinks(t *testing.T) {
	c := newCreateHost(t)
	src, target := c.uefiSource()

	_, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	sudo := splitLines(c.log("sudo"))
	assert.Contains(t, sudo, "dd if="+src+" of="+target+" iflag=nofollow oflag=nofollow status=none",
		"copied with O_NOFOLLOW on both ends, never `cp` as root")
	assert.Contains(t, sudo, "chmod 0600 "+target)
	for _, l := range sudo {
		assert.False(t, strings.HasPrefix(l, "cp "), "no cp of the varstore: %q", l)
	}
	assert.Contains(t, c.domainXML("team-b.copy"), "<nvram>"+target+"</nvram>")
}

// requireCloneRefusedBeforeWriting asserts a clone was refused as a Conflict
// before it wrote any file or defined anything.
func (c *createHost) requireCloneRefusedBeforeWriting(err error, want string) {
	c.t.Helper()
	require.Error(c.t, err)
	assert.True(c.t, contracts.IsConflict(err), "want a Conflict: %v", err)
	assert.Contains(c.t, err.Error(), want)
	assert.NotContains(c.t, err.Error(), c.base, "no host path in the message")
	assert.NoFileExists(c.t, filepath.Join(c.images, "team-b.copy-disk.qcow2"), "no clone disk was written")
	assert.Empty(c.t, c.log("sudo"), "nothing was copied, chowned or chmodded")
	for _, call := range c.virshCalls("h1") {
		assert.False(c.t, strings.HasPrefix(call, "define "), "nothing was defined: %q", call)
	}
}

func TestClone_NVRAMTargetSymlinkIsRefused(t *testing.T) {
	for name, dangling := range map[string]bool{"to a file": false, "dangling": true} {
		t.Run(name, func(t *testing.T) {
			c := newCreateHost(t)
			_, target := c.uefiSource()
			victim := filepath.Join(c.outside, "victim")
			if !dangling {
				victim = c.file(c.outside, "victim")
			}
			require.NoError(t, os.Symlink(victim, target))

			_, err := c.p.Clone(context.Background(), cloneReq())
			c.requireCloneRefusedBeforeWriting(err, "is a symbolic link")
		})
	}
}

func TestClone_NVRAMTargetInUseIsRefused(t *testing.T) {
	c := newCreateHost(t)
	_, target := c.uefiSource()
	require.NoError(t, os.WriteFile(target, []byte("vars"), 0o600))
	x := fmt.Sprintf("<domain type='kvm'><name>other</name><uuid>%s</uuid><os><nvram>%s</nvram></os><devices/></domain>", uuidB, target)
	c.define("h1", "other", uuidB, x)

	_, err := c.p.Clone(context.Background(), cloneReq())
	c.requireCloneRefusedBeforeWriting(err, "is in use by another domain")
}

func TestClone_NVRAMStaleTargetIsOverwritten(t *testing.T) {
	c := newCreateHost(t)
	src, target := c.uefiSource()
	require.NoError(t, os.WriteFile(target, []byte("left by a failed clone"), 0o600))

	_, err := c.p.Clone(context.Background(), cloneReq())
	require.NoError(t, err)
	assert.Contains(t, splitLines(c.log("sudo")), "dd if="+src+" of="+target+" iflag=nofollow oflag=nofollow status=none")
}
