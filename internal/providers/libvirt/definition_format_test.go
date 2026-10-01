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
	"os/exec"
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

// These tests pin the rule that every disk read as root is opened in the
// format its DOMAIN DEFINITION names (<driver type=...>), never in a format
// qemu-img probes from the image: a guest owns every byte of a raw disk, so
// it can make the disk look like a qcow2 header naming any file on the host
// as its backing or data file. The clone and export copies, GetDiskInfo, the
// disk in-use scan and Delete's own-chain walk never open that file — a raw
// disk has no chain — and a disk in any other format is never copied as
// root.

// forgedVictim is the file a forged header names: another tenant's disk. It
// exists nowhere — every tool that could open it is a fake.
const forgedVictim = "/var/lib/libvirt/images/team-b.secret-disk.qcow2"

// forgedHeaderQemuImg is a qemu-img in front of a fixture's. For
// $FORGED_IMAGE — a disk its definition declares raw, whose guest wrote a
// qcow2 header naming $FORGED_VICTIM as its backing file and data file —
// `info` answers what qemu-img's format probe would read (that header) unless
// the call pins -f raw. Every call that names the victim is recorded in
// $FORGED_READS. Anything else goes on to the fixture's qemu-img.
const forgedHeaderQemuImg = `#!/bin/sh
for a in "$@"; do
  case "$a" in *"$FORGED_VICTIM"*) printf 'qemu-img %s\n' "$*" >> "$FORGED_READS" ;; esac
done
if [ "$1" = info ]; then
  for a in "$@"; do img="$a"; done
  if [ "$img" = "$FORGED_IMAGE" ]; then
    printf 'local qemu-img %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
    case " $* " in
    *" -f raw "*) printf '{"filename": "%s", "format": "raw", "virtual-size": 10737418240, "actual-size": 10737418240}\n' "$img" ;;
    *) printf '{"filename": "%s", "format": "qcow2", "virtual-size": 10737418240, "actual-size": 1073741824, "backing-filename": "%s", "full-backing-filename": "%s", "backing-filename-format": "qcow2", "format-specific": {"type": "qcow2", "data": {"data-file": "%s"}}}\n' "$img" "$FORGED_VICTIM" "$FORGED_VICTIM" "$FORGED_VICTIM" ;;
    esac
    exit 0
  fi
fi
exec "$NEXT_QEMU_IMG" "$@"
`

// forgedHeaderSudo is a sudo in front of a fixture's: `sudo -n qemu-img ...`
// runs forgedHeaderQemuImg (by path, logged as the fixture logs it);
// anything else goes on to the fixture's sudo. It never runs the real sudo.
const forgedHeaderSudo = `#!/bin/sh
if [ "$1" = -n ] && [ "$2" = qemu-img ]; then
  printf 'local sudo %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
  shift 2
  exec "$(dirname "$0")/qemu-img" "$@"
fi
exec "$NEXT_SUDO" "$@"
`

// installForgedHeader makes image (declared raw by its definition) carry a
// guest-forged qcow2 header naming forgedVictim, in front of the fixture's
// fakes (which must already be on PATH — never the real tools). It returns
// the file recording every call that named the victim.
func installForgedHeader(t *testing.T, image string) string {
	t.Helper()
	next := map[string]string{}
	for _, tool := range []string{"qemu-img", "sudo"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.Contains(t, p, os.TempDir(), "the fixture's fake %s must come first on PATH, never the real one", tool)
		require.NotContains(t, p, "virtrigaud-host-guard-", "a fixture's fake %s, not the host guard's shim", tool)
		next[tool] = p
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(forgedHeaderQemuImg), 0o755)) //nolint:gosec // test shim must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(forgedHeaderSudo), 0o755))        //nolint:gosec // test shim must be executable
	reads := filepath.Join(t.TempDir(), "victim-reads.log")
	t.Setenv("FORGED_IMAGE", image)
	t.Setenv("FORGED_VICTIM", forgedVictim)
	t.Setenv("FORGED_READS", reads)
	t.Setenv("NEXT_QEMU_IMG", next["qemu-img"])
	t.Setenv("NEXT_SUDO", next["sudo"])
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return reads
}

// requireVictimNeverNamed asserts no command named the forged header's file.
func requireVictimNeverNamed(t *testing.T, reads string, calls []string) {
	t.Helper()
	_, err := os.Stat(reads)
	assert.True(t, os.IsNotExist(err), "no qemu-img call named the file the forged header names")
	for _, c := range calls {
		assert.NotContains(t, c, forgedVictim, "the forged header's file is never opened: %q", c)
	}
}

// scdDomainXMLAs is scdDomainXML with the primary disk declared format.
func scdDomainXMLAs(name string, o scdDomainOpts, format string) string {
	return strings.Replace(scdDomainXML(name, o), "<driver name='qemu' type='qcow2'/>", "<driver name='qemu' type='"+format+"'/>", 1)
}

func TestSingleHost_Clone_RawDiskIsCopiedAsRawNeverThroughAForgedHeader(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXMLAs(scdDomain, scdDomainOpts{}, "raw")})
	reads := installForgedHeader(t, scdDiskPath)

	_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
	require.NoError(t, err)
	calls := fx.calls()
	assert.Contains(t, calls, "local sudo -n qemu-img convert -f raw -O qcow2 "+scdDiskPath+" "+scdCloneWriteFile,
		"the source is opened as the raw disk its definition declares")
	for _, c := range calls {
		assert.NotContains(t, c, "qemu-img info", "a raw disk has no chain to read: %q", c)
	}
	requireVictimNeverNamed(t, reads, calls)
}

func TestSingleHost_GetDiskInfo_RawDiskIsReadAsRaw(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXMLAs(scdDomain, scdDomainOpts{}, "raw")})
	reads := installForgedHeader(t, scdDiskPath)

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain})
	require.NoError(t, err)
	assert.Equal(t, "raw", resp.Format, "the definition's format, never the probed one")
	assert.EqualValues(t, scdVirtualSize, resp.VirtualSizeBytes)
	calls := fx.calls()
	assert.Contains(t, calls, "local sudo -n qemu-img info -U -f raw --output=json -- "+scdDiskPath)
	requireVictimNeverNamed(t, reads, calls)
}

func TestSingleHost_GetDiskInfo_OtherFormatIsReadAsTheSSHUserWithItsFormatPinned(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXMLAs(scdDomain, scdDomainOpts{}, "vmdk")})

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain})
	require.NoError(t, err)
	assert.Equal(t, "vmdk", resp.Format)
	calls := fx.calls()
	assert.Contains(t, calls, "local qemu-img info -U -f vmdk --output=json -- "+scdDiskPath)
	for _, c := range calls {
		assert.False(t, strings.HasPrefix(c, "local sudo"), "a vmdk disk is never read as root: %q", c)
	}
}

func TestSingleHost_Clone_OtherFormatIsRefusedBeforeAnyCopy(t *testing.T) {
	for _, format := range []string{"vmdk", "qed", "luks"} {
		t.Run(format, func(t *testing.T) {
			fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXMLAs(scdDomain, scdDomainOpts{}, format)})
			_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, codes.FailedPrecondition, st.Code())
			assert.Contains(t, st.Message(), fmt.Sprintf("its disk format %q is not qcow2 or raw", format))
			assert.NotContains(t, st.Message(), "/var/lib", "no host path")
			for _, c := range fx.calls() {
				assert.NotContains(t, c, "qemu-img", "the disk is never opened, by anyone: %q", c)
			}
		})
	}
}

func TestClustered_Export_RawDiskIsReadAsRawNeverThroughAForgedHeader(t *testing.T) {
	rawWeb := func(t *testing.T) (*routedSCD, string) {
		fx := ownedWebOnB(t, scdDomainXMLAs("web", scdDomainOpts{owner: ownerTeamA}, "raw"))
		fx.p.hostDiskTransportFn = anyTransport
		return fx, installForgedHeader(t, scdDiskPath)
	}
	t.Run("nfs", func(t *testing.T) {
		fx, reads := rawWeb(t)
		_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2",
		})
		require.NoError(t, err)
		calls := fx.calls()
		assert.Contains(t, calls, "local qemu-img convert -U -f raw -O qcow2 "+scdDiskPath+" nfs://nas/e/web.qcow2?uid="+scdSSHUID+"&gid="+scdSSHGID)
		requireVictimNeverNamed(t, reads, calls)
	})
	t.Run("s3", func(t *testing.T) {
		fx, reads := rawWeb(t)
		_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "s3",
			DestinationUrl:     "s3://bucket/web.qcow2",
			StorageOptionsJson: `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`,
			Credentials:        map[string]string{"accessKeyID": "a", "secretAccessKey": "s"},
		})
		require.Error(t, err, "the local fakes have no SSH stream: the export fails after the flatten")
		calls := fx.calls()
		assert.Contains(t, calls, "local qemu-img convert -U -f raw -O qcow2 "+scdDiskPath+" "+s3ExportTemp)
		requireVictimNeverNamed(t, reads, calls)
	})
	t.Run("disk info", func(t *testing.T) {
		fx, reads := rawWeb(t)
		resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
		})
		require.NoError(t, err)
		assert.Equal(t, "raw", resp.Format)
		calls := fx.calls()
		assert.Contains(t, calls, "local sudo -n qemu-img info -U -f raw --output=json -- "+scdDiskPath)
		requireVictimNeverNamed(t, reads, calls)
	})
}

func TestClustered_Export_OtherFormatIsRefusedWithoutAHostPath(t *testing.T) {
	for _, backend := range []string{"nfs", "s3"} {
		t.Run(backend, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXMLAs("web", scdDomainOpts{owner: ownerTeamA}, "vmdk"))
			fx.p.hostDiskTransportFn = anyTransport
			req := &providerv1.ExportDiskRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: backend, DestinationUrl: "nfs://nas/e/web.qcow2",
			}
			if backend == "s3" {
				req.DestinationUrl = "s3://bucket/web.qcow2"
				req.StorageOptionsJson = `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`
				req.Credentials = map[string]string{"accessKeyID": "a", "secretAccessKey": "s"}
			}
			_, err := NewServer(fx.p).ExportDisk(context.Background(), req)
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, codes.FailedPrecondition, st.Code(), "got %v", err)
			assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st))
			assert.Contains(t, st.Message(), copyRefusedWire)
			assert.NotContains(t, st.Message(), "/var/lib", "no host path on the wire")
			for _, c := range fx.calls() {
				assert.NotContains(t, c, "qemu-img convert", "nothing is copied: %q", c)
			}
		})
	}
}

// forgedSidecar is the fakeHost qemu-img answer for a raw disk whose guest
// wrote a qcow2 header naming victim as its backing file and data file.
func forgedSidecar(disk, victim string) string {
	return fmt.Sprintf(`{"filename":%q,"format":"qcow2","backing-filename":%q,"full-backing-filename":%q,`+
		`"backing-filename-format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":%q}}}`, disk, victim, victim, victim)
}

// TestConfine_RawDiskForgedHeaderIsNeverFollowed: the disk in-use scan opens
// each disk in its definition's format, so a raw disk — which has no chain —
// is never opened at all, and the file its forged header names is never read.
// The control (the same disk declared qcow2) shows the fixture sees a
// followed header.
func TestConfine_RawDiskForgedHeaderIsNeverFollowed(t *testing.T) {
	for _, format := range []string{"raw", "qcow2"} {
		t.Run(format, func(t *testing.T) {
			h := newFakeHost(t)
			vp := h.host("h1")
			disk := h.file(h.images, "vm-raw.img")
			victim := h.file(h.images, "other-tenant.qcow2")
			h.info(disk, forgedSidecar(disk, victim))
			h.domain("h1", uuidA, typedDiskDomainXML(format, disk))

			free := h.file(h.images, "ubuntu.qcow2")
			pol := imagePathPolicy{dirs: []string{h.images}}
			_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: free})
			require.NoError(t, err)
			qlog := h.log("qemu-img")
			if format == "raw" {
				assert.NotContains(t, qlog, disk, "a raw disk has no chain: the scan never opens it")
				assert.NotContains(t, qlog, victim, "the forged header's file is never read")
				return
			}
			assert.Contains(t, qlog, "info -U -f qcow2 --output=json -- "+disk)
			assert.Contains(t, qlog, victim, "control: a qcow2 disk's header is followed")
		})
	}
}

// TestDelete_SingleHost_RawDiskForgedHeaderIsNeverFollowed: Delete's own-chain
// walk opens the disk in its definition's format; a raw disk has no chain, so
// a file its guest's forged header names — even one named like the domain's
// own pre-snapshot disk — is neither read nor removed.
func TestDelete_SingleHost_RawDiskForgedHeaderIsNeverFollowed(t *testing.T) {
	c := newCreateHost(t)
	top := c.file(c.images, "team-a.web-disk.snap1")
	victim := c.file(c.images, "team-a.web-disk.qcow2")
	c.info(top, forgedSidecar(top, victim))
	c.define("h1", "team-a.web", uuidSource,
		strings.Replace(depDomainXML("team-a.web", uuidSource, top, ""), "type='qcow2'", "type='raw'", 1))
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("team-a.web\n"), 0o600))

	_, err := c.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web"})
	require.NoError(t, err)
	assert.Equal(t, []string{top}, c.removals(), "only the domain's own disk")
	assert.FileExists(t, victim)
	assert.NotContains(t, c.log("qemu-img"), victim, "the forged header's file is never read")
}
