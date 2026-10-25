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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin V12: a disk-only snapshot whose overlay file is already on
// the host is refused before anything changes, as a definite Conflict
// (AlreadyExists) with a constant text, on a single host and on a clustered
// (routed) provider; libvirt's own refusal of it is classified the same way.

// v12OverlayRefusal is libvirt 10's refusal of an existing overlay file
// (qemuSnapshotPrepareDiskExternal).
const v12OverlayRefusal = "error: unsupported configuration: external snapshot file for disk vda already exists and is " +
	"not a block device: /var/lib/libvirt/images/vrgold-web-disk.x\n"

func TestExternalOverlayPath(t *testing.T) {
	for source, want := range map[string]string{
		"/var/lib/libvirt/images/web-disk.qcow2": "/var/lib/libvirt/images/web-disk.snap",
		"/var/lib/libvirt/images/web-disk.pre":   "/var/lib/libvirt/images/web-disk.snap", // an earlier overlay
		"/var/lib/libvirt/images/web-disk":       "/var/lib/libvirt/images/web-disk.snap",
		"/var/lib/libvirt/im.ages/web-disk":      "/var/lib/libvirt/im.ages/web-disk.snap", // a '.' in a directory only
		"/pool/a.b.qcow2":                        "/pool/a.b.snap",
	} {
		assert.Equal(t, want, externalOverlayPath(source, "snap"), source)
	}
}

func TestSnapshotOverlayTargets(t *testing.T) {
	doc, err := parseDomainDisks(`<domain><name>web</name><devices>
  <disk type='file' device='disk'><source file='/pool/web-disk.qcow2'/><target dev='vda'/></disk>
  <disk type='file' device='disk'><source file='/pool/web-data.raw'/><target dev='vdb'/></disk>
  <disk type='file' device='disk' snapshot='no'><source file='/pool/web-scratch.qcow2'/><target dev='vdc'/></disk>
  <disk type='file' device='disk'><source file='/pool/web-ro.qcow2'/><target dev='vdd'/><readonly/></disk>
  <disk type='block' device='disk'><source dev='/dev/sdz'/><target dev='vde'/></disk>
  <disk type='file' device='cdrom'><source file='/seed/cloud-init.iso'/><target dev='sda'/><readonly/></disk>
</devices></domain>`)
	require.NoError(t, err)
	targets, paths := doc.snapshotOverlayTargets("s1")
	assert.Equal(t, []string{"vda", "vdb"}, targets)
	assert.Equal(t, []string{"/pool/web-disk.s1", "/pool/web-data.s1"}, paths)
}

// assertOverlayRefusal asserts err is the V12 refusal on the wire.
func assertOverlayRefusal(t *testing.T, err error) {
	t.Helper()
	assertRefusalStatus(t, err, codes.AlreadyExists)
	assert.Equal(t, wireSnapshotOverlayExists, status.Convert(err).Message())
	assertConstantText(t, err, "/var/lib", "vrgold", "vda", "virsh")
	assert.LessOrEqual(t, len("snapshotCreate: "+wireSnapshotOverlayExists), 256,
		"the manager's status detail keeps the whole text")
}

// countCalls counts the calls that contain sub.
func countCalls(calls []string, sub string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func TestSingleHostSnapshotCreate_OverlayCollision(t *testing.T) {
	run := func(t *testing.T, setup func(*scdFixture), req *providerv1.SnapshotCreateRequest) ([]string, error) {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		if setup != nil {
			setup(fx)
		}
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(), req)
		return fx.calls(), err
	}

	t.Run("the overlay exists: refused before anything changes", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("local", "overlay-exists", "1\n") }, scdSnapshotRequest("x"))
		assertOverlayRefusal(t, err)
		assert.Equal(t, 1, countCalls(calls, "sh -c "+overlayCollisionScript+" sh /var/lib/libvirt/images/vrgold-web-disk.x"),
			"the probe names the overlay libvirt would create: %v", calls)
		assert.Zero(t, countCalls(calls, "snapshot-create-as"), "no snapshot is attempted")
		assert.Zero(t, countCalls(calls, "change-media"), "no medium is ejected")
		assert.Zero(t, countCalls(calls, "metadata"), "nothing is recorded")
		assert.Zero(t, countCalls(calls, " rm "), "the leftover file is never removed")
	})
	t.Run("the probe cannot run: not performed, nothing changed", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("local", "fail-overlay-probe", "") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
		assert.Zero(t, countCalls(calls, "snapshot-create-as"))
		assert.Zero(t, countCalls(calls, "change-media"))
	})
	t.Run("the probe answers garbage: not performed", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) { fx.script("local", "overlay-exists", "7\n") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
	})
	t.Run("a memory snapshot of a running domain has no overlay: no probe", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) {
			fx.script("single", "state", "running\n")
			fx.script("local", "overlay-exists", "1\n")
		}, &providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "mem", Description: "d", IncludeMemory: true})
		require.NoError(t, err)
		assert.Zero(t, countCalls(calls, overlayCollisionScript), "%v", calls)
	})
	t.Run("libvirt refuses the existing overlay (the probe could not see it): Conflict, not 'not created'", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) {
			fx.script("single", "fail-snapshot-create-as", "")
			fx.script("single", "stderr-snapshot-create-as", v12OverlayRefusal)
		}, scdSnapshotRequest("x"))
		assertOverlayRefusal(t, err)
		assert.Equal(t, 2, countCalls(calls, "snapshot-list"), "the absence is proven first")
	})
	t.Run("libvirt's refusal but the snapshot exists afterwards: unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) {
			fx.script("single", "fail-snapshot-create-as", "")
			fx.script("single", "leaves-snapshot-create-as", "")
			fx.script("single", "stderr-snapshot-create-as", v12OverlayRefusal)
		}, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
	})
	t.Run("no collision: the snapshot is made", func(t *testing.T) {
		calls, err := run(t, nil, scdSnapshotRequest("x"))
		require.NoError(t, err)
		assert.Equal(t, 1, countCalls(calls, "snapshot-create-as"))
	})
}

func TestClusteredSnapshotCreate_OverlayCollision(t *testing.T) {
	webXML := scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})
	req := func() *providerv1.SnapshotCreateRequest {
		return &providerv1.SnapshotCreateRequest{VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre",
			Description: "d", RequestToken: "11111111-2222-4333-8444-000000000001"}
	}

	t.Run("the overlay exists: a definite AlreadyExists, never retried as unknown", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		fx.script("local", "overlay-exists", "1\n")
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(), req())
		assertRefusalStatus(t, err, codes.AlreadyExists)
		assert.Equal(t, wireSnapshotOverlayExists, status.Convert(err).Message())
		assert.False(t, contracts.HasErrorInfoReason(err, contracts.OutcomeUnknownRetryReason), "%v", err)
		assert.Zero(t, countCalls(fx.calls(), "snapshot-create-as"))
		assert.Zero(t, countCalls(fx.calls(), "change-media"))
	})
	t.Run("libvirt refuses the existing overlay: AlreadyExists", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		r21ScriptedExits(t)
		fx.script("host-b", "fail-snapshot-create-as", "")
		fx.script("host-b", "stderr-snapshot-create-as", v12OverlayRefusal)
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(), req())
		assertRefusalStatus(t, err, codes.AlreadyExists)
		assert.Equal(t, wireSnapshotOverlayExists, status.Convert(err).Message())
	})
	t.Run("another libvirt failure keeps its answer", func(t *testing.T) {
		fx := ownedWebOnB(t, webXML)
		r21ScriptedExits(t)
		fx.script("host-b", "fail-snapshot-create-as", "")
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(), req())
		require.Error(t, err)
		assert.NotEqual(t, codes.AlreadyExists, status.Code(err), "%v", err)
	})
}
