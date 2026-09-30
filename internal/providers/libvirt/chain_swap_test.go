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

// These tests pin that a copy never reads an image chain another account
// could swap, and root never writes where another account could swap the
// copy's private directory. Every image of the source chain must be a regular
// file (not a symbolic link), without an external data file, qcow2 or raw,
// in a directory only root and the SSH user can write (or a sticky one); the
// directory a root copy writes in must be just as safe. Anything else is
// refused, never copied as the SSH user either (they read every VM disk
// through the kvm group). GetDiskInfo reads such a disk as the SSH user, as
// it always has.

// TestUnsafeChainMemberReason_RealShell runs chainMemberScript and
// diskDirModeScript with the real shell and stat, on scratch files only.
func TestUnsafeChainMemberReason_RealShell(t *testing.T) {
	requireGNURealpath(t)
	ctx := context.Background()
	h := NewVirshProvider(&ProviderConfig{})
	h.uri = "test:///chain"
	mkdir := func(mode os.FileMode) string {
		d := filepath.Join(t.TempDir(), "d")
		require.NoError(t, os.Mkdir(d, 0o700))
		require.NoError(t, os.Chmod(d, mode))
		return d
	}
	file := func(dir string) string {
		p := filepath.Join(dir, "disk.qcow2")
		require.NoError(t, os.WriteFile(p, []byte("image"), 0o600))
		return p
	}

	private := mkdir(0o700)
	disk := file(private)
	reason, err := unsafeChainMemberReason(ctx, h, disk)
	require.NoError(t, err)
	assert.Empty(t, reason, "a regular file in a directory only the SSH user writes")

	link := filepath.Join(private, "link.qcow2")
	require.NoError(t, os.Symlink(disk, link))
	reason, err = unsafeChainMemberReason(ctx, h, link)
	require.NoError(t, err)
	assert.Contains(t, reason, "is a symbolic link")

	groupWritable := mkdir(0o770)
	reason, err = unsafeChainMemberReason(ctx, h, file(groupWritable))
	require.NoError(t, err)
	assert.Contains(t, reason, "is writable by its group and is not sticky")
	reason, err = unsafeHostDirReason(ctx, h, groupWritable)
	require.NoError(t, err)
	assert.Contains(t, reason, "is writable by its group and is not sticky")

	sticky := mkdir(0o770 | os.ModeSticky)
	reason, err = unsafeChainMemberReason(ctx, h, file(sticky))
	require.NoError(t, err)
	assert.Empty(t, reason, "a sticky directory: no one can swap another's file")

	reason, err = unsafeHostDirReason(ctx, h, filepath.Join(private, "missing"))
	require.NoError(t, err)
	assert.Contains(t, reason, "could not be checked", "a check that fails refuses")
}

func TestDirModeReason(t *testing.T) {
	for out, want := range map[string]string{
		"755 0\n1001":     "",
		"1777 0\n1001":    "",
		"755 1001\n1001":  "",
		"775 0\n1001":     "is writable by its group and is not sticky",
		"757 0\n1001":     "is writable by every user and is not sticky",
		"755 1002\n1001":  "is writable by its owner (uid 1002) and is not sticky",
		"":                "could not be read",
		"garbage":         "could not be read",
		"755 0\n1001\nx":  "could not be read",
		"755 0 1001 1002": "could not be read",
	} {
		got := dirModeReason("/d", out)
		if want == "" {
			assert.Empty(t, got, "%q", out)
		} else {
			assert.Contains(t, got, want, "%q", out)
		}
	}
}

// chainInfoSudo is a sudo in front of the routed fixture's: `sudo -n qemu-img
// info` answers $CHAIN_INFO_TOP's header ($CHAIN_INFO_JSON) for that image and
// a standalone qcow2 for any other; anything else goes to the fixture's sudo.
const chainInfoSudo = `#!/bin/sh
if [ "$1" = -n ] && [ "$2" = qemu-img ] && [ "$3" = info ]; then
  printf 'local sudo %s\n' "$*" >> "$FAKE_SCD_DIR/calls.log"
  for a in "$@"; do img="$a"; done
  if [ "$img" = "$CHAIN_INFO_TOP" ]; then printf '%s\n' "$CHAIN_INFO_JSON"; else printf '{"filename": "%s", "format": "qcow2"}\n' "$img"; fi
  exit 0
fi
exec "$NEXT_SUDO" "$@"
`

// installChainInfo puts chainInfoSudo in front of the fixture's sudo.
func installChainInfo(t *testing.T, top, infoJSON string) {
	t.Helper()
	next, err := exec.LookPath("sudo")
	require.NoError(t, err)
	require.Contains(t, next, os.TempDir(), "the fixture's fake sudo must come first on PATH, never the real one")
	require.NotContains(t, next, "virtrigaud-host-guard-", "a fixture's fake sudo, not the host guard's shim")
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(chainInfoSudo), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("CHAIN_INFO_TOP", top)
	t.Setenv("CHAIN_INFO_JSON", infoJSON)
	t.Setenv("NEXT_SUDO", next)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCheckCopySource_RefusesASwappableChain(t *testing.T) {
	ctx := context.Background()
	h := localHostVP("host-b")
	overlayBase := filepath.Base(scdOverlayPath)

	t.Run("a safe chain", func(t *testing.T) {
		fx := newRoutedSCD(t, nil)
		fx.script("local", "backing-"+overlayBase, scdDiskPath)
		require.NoError(t, checkCopySource(ctx, h, scdOverlayPath, "qcow2"))
		calls := fx.calls()
		for _, img := range []string{scdOverlayPath, scdDiskPath} {
			assert.Contains(t, calls, "local sh -c "+chainMemberScript+" sh "+img+" /var/lib/libvirt/images",
				"every image of the chain is checked")
		}
	})
	t.Run("the disk is a symbolic link", func(t *testing.T) {
		fx := newRoutedSCD(t, nil)
		fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
		refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "qcow2"), scdDiskPath+" is a symbolic link")
	})
	t.Run("a raw disk is a symbolic link", func(t *testing.T) {
		fx := newRoutedSCD(t, nil)
		fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
		refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "raw"), "is a symbolic link")
	})
	t.Run("a backing file is a symbolic link", func(t *testing.T) {
		fx := newRoutedSCD(t, nil)
		fx.script("local", "backing-"+overlayBase, scdDiskPath)
		fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
		refusedCopy(t, checkCopySource(ctx, h, scdOverlayPath, "qcow2"), scdDiskPath+" is a symbolic link")
	})
	t.Run("the chain's directory is group-writable", func(t *testing.T) {
		fx := newRoutedSCD(t, nil)
		fx.script("local", "unsafe-dir-images", "")
		refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "qcow2"), "the directory /var/lib/libvirt/images is writable by its group")
	})
	t.Run("an external data file", func(t *testing.T) {
		newRoutedSCD(t, nil)
		installChainInfo(t, scdDiskPath, `{"filename": "`+scdDiskPath+`", "format": "qcow2", `+
			`"format-specific": {"type": "qcow2", "data": {"data-file": "/var/lib/libvirt/images/data.raw"}}}`)
		refusedCopy(t, checkCopySource(ctx, h, scdDiskPath, "qcow2"), "has an external data file")
	})
	t.Run("a backing file in another format", func(t *testing.T) {
		newRoutedSCD(t, nil)
		installChainInfo(t, scdOverlayPath, `{"filename": "`+scdOverlayPath+`", "format": "qcow2", "backing-filename": "`+
			scdDiskPath+`", "full-backing-filename": "`+scdDiskPath+`", "backing-filename-format": "vmdk"}`)
		refusedCopy(t, checkCopySource(ctx, h, scdOverlayPath, "qcow2"), `as "vmdk", not qcow2 or raw`)
	})
}

// scdAltDiskPath is a source disk outside the pool directory, so the source
// directory ("vm") and the clone's output directory ("images") differ.
const scdAltDiskPath = "/srv/vm/" + scdDomain + "-disk.qcow2"

func TestSingleHost_Clone_SwappableSourceIsRefusedNotCopied(t *testing.T) {
	for name, tc := range map[string]struct {
		xml, script, want string
	}{
		"symbolic-link source": {
			xml:    scdDomainXML(scdDomain, scdDomainOpts{}),
			script: "symlink-" + filepath.Base(scdDiskPath),
			want:   "is a symbolic link",
		},
		"group-writable source directory": {
			xml:    scdDomainXML(scdDomain, scdDomainOpts{}),
			script: "unsafe-dir-images",
			want:   "is writable by its group and is not sticky",
		},
		"group-writable output directory": {
			xml:    strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}), scdDiskPath, scdAltDiskPath, 1),
			script: "unsafe-dir-images",
			want:   "the directory /var/lib/libvirt/images is writable by its group and is not sticky; root does not write there",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newSCDFixture(t, map[string]string{scdDomain: tc.xml})
			fx.script("local", tc.script, "")
			_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
			st, ok := status.FromError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, codes.FailedPrecondition, st.Code(), "got %v", err)
			assert.Contains(t, st.Message(), tc.want)
			for _, c := range fx.calls() {
				assert.NotContains(t, c, "qemu-img convert", "nothing is copied, by root or the SSH user: %q", c)
			}
		})
	}
}

func TestClustered_Export_SwappableSourceIsRefusedWithoutAHostPath(t *testing.T) {
	for _, backend := range []string{"nfs", "s3"} {
		t.Run(backend, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
			fx.p.hostDiskTransportFn = anyTransport
			fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
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

func TestSingleHost_GetDiskInfo_SwappableDiskIsReadAsTheSSHUser(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")

	resp, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{VmId: scdDomain})
	require.NoError(t, err)
	assert.EqualValues(t, scdVirtualSize, resp.VirtualSizeBytes, "read, as the SSH user")
	calls := fx.calls()
	assert.Contains(t, calls, "local qemu-img info -U -f qcow2 --output=json -- "+scdDiskPath)
	for _, c := range calls {
		assert.False(t, strings.HasPrefix(c, "local sudo"), "never as root: %q", c)
	}
}
