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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ---------------------------------------------------------------------------
// Pure (host-independent) checks
// ---------------------------------------------------------------------------

// TestValidateImagePathSyntax is the table test for the pre-resolve (lexical)
// validator every image path passes before it is sent to the host.
func TestValidateImagePathSyntax(t *testing.T) {
	accept := []string{
		"/var/lib/libvirt/images/ubuntu.qcow2",
		"/var/lib/libvirt/images/with space.img",
		"/home/virt/.local/share/libvirt/images/f.qcow2",
		"/data/.../x.qcow2",
		"/data/images/$(inert).qcow2",
		"/x",
	}
	for _, p := range accept {
		assert.NoError(t, validateImagePathSyntax(p), "legitimate path %q", p)
	}

	reject := map[string]struct{ path, want string }{
		"empty":            {"", "empty"},
		"relative":         {"images/x.qcow2", "absolute"},
		"dot relative":     {"./x.qcow2", "absolute"},
		"leading dash":     {"-o/etc/shadow", "begin with '-'"},
		"dash segment":     {"/var/lib/libvirt/images/-rf", "segment may begin with '-'"},
		"dotdot":           {"/var/lib/libvirt/images/../../etc/shadow", "'..'"},
		"trailing dotdot":  {"/var/lib/libvirt/images/..", "'..'"},
		"nul":              {"/var/lib/libvirt/images/x\x00.qcow2", "control characters"},
		"newline":          {"/var/lib/libvirt/images/x\n.qcow2", "control characters"},
		"escape":           {"/var/lib/libvirt/images/\x1b[2J", "control characters"},
		"c1 control":       {"/var/lib/libvirt/images/x\u0085", "control characters"},
		"del":              {"/var/lib/libvirt/images/x\x7f", "control characters"},
		"invalid utf8":     {"/var/lib/libvirt/images/\xff.qcow2", "UTF-8"},
		"trailing slash":   {"/var/lib/libvirt/images/", "pattern"},
		"root":             {"/", "pattern"},
		"too long":         {"/" + strings.Repeat("a", 4096), "longer than"},
		"url not path":     {"https://example.com/x.qcow2", "absolute"},
		"windows-ish path": {`C:\images\x.qcow2`, "absolute"},
	}
	for name, tc := range reject {
		err := validateImagePathSyntax(tc.path)
		if assert.Error(t, err, name) {
			assert.Contains(t, err.Error(), tc.want, name)
		}
	}
}

// TestParseImageDirs covers the VIRTRIGAUD_LIBVIRT_IMAGE_DIRS grammar and the
// start-up refusal of dangerous directories.
func TestParseImageDirs(t *testing.T) {
	ok := map[string][]string{
		"":                                   {DefaultImageDir},
		"   ":                                {DefaultImageDir},
		"/srv/images":                        {"/srv/images"},
		"/srv/images/":                       {"/srv/images"},
		"/srv/images,/data/golden":           {"/srv/images", "/data/golden"},
		"/srv/images:/data/golden":           {"/srv/images", "/data/golden"},
		" /srv/images , /srv/images ,":       {"/srv/images"},
		"/var/lib/libvirt/images,/srv/x.img": {"/var/lib/libvirt/images", "/srv/x.img"},
	}
	for raw, want := range ok {
		got, err := parseImageDirs(raw)
		require.NoError(t, err, "raw %q", raw)
		assert.Equal(t, want, got, "raw %q", raw)
	}

	bad := []string{
		"relative/images",
		"/", "/etc", "/etc/libvirt", "/dev", "/proc/self", "/sys", "/root", "/boot",
		"/tmp", "/tmp/images", "/var/tmp/x", "/run/user", "/usr/share/images",
		"/var/lib/libvirt/qemu/nvram", "/var/lib/libvirt/images/cloud-init",
		"/var/lib/kubelet/pods", "/srv/images,/etc",
		"/srv/../etc", "-/srv/images", "/srv/\nimages",
	}
	for _, raw := range bad {
		_, err := parseImageDirs(raw)
		assert.Error(t, err, "raw %q must be refused", raw)
	}
}

// TestImageDirsFromEnv proves the env wiring and that a bad value fails loud.
func TestImageDirsFromEnv(t *testing.T) {
	t.Setenv(EnvImageDirs, "")
	dirs, err := imageDirsFromEnv()
	require.NoError(t, err)
	assert.Equal(t, []string{DefaultImageDir}, dirs)

	t.Setenv(EnvImageDirs, "/srv/golden")
	dirs, err = imageDirsFromEnv()
	require.NoError(t, err)
	assert.Equal(t, []string{"/srv/golden"}, dirs)

	t.Setenv(EnvImageDirs, "/etc")
	_, err = imageDirsFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvImageDirs)

	// A struct-literal provider (no constructor) still refuses to run with a
	// malformed policy rather than skipping the checks.
	_, err = (&Provider{}).imagePolicy()
	require.Error(t, err)
}

// TestReservedImageName pins the VirtRigaud-managed names a base image may
// never use.
func TestReservedImageName(t *testing.T) {
	reserved := []string{
		"web-disk.qcow2", "web-disk", "web-migrated.qcow2",
		".virtrigaud-imageprepare-x.download", ".virtrigaud-export-vm-1.qcow2", ".hidden.qcow2",
		"web-cloud-init.iso", "cloud-init.iso", "web-cidata.iso",
		"vm-temp.img", "d-download.img", "x.partial",
	}
	for _, n := range reserved {
		assert.True(t, reservedImageName(n), "%q must be reserved", n)
	}
	free := []string{"ubuntu-22.04.qcow2", "jammy-server-cloudimg-amd64.img", "win.vmdk", "disk.qcow2", "migrated.qcow2", "golden-disks.qcow2"}
	for _, n := range free {
		assert.False(t, reservedImageName(n), "%q must be usable as a base image", n)
	}
}

// TestCheckImageHeader covers the self-contained-image rule.
func TestCheckImageHeader(t *testing.T) {
	const self = "/var/lib/libvirt/images/x.vmdk"
	mk := func(js string) qemuImgInfo {
		var info qemuImgInfo
		require.NoError(t, json.Unmarshal([]byte(js), &info))
		return info
	}
	cases := map[string]struct {
		info string
		want string // "" = accepted
	}{
		"plain qcow2":        {`{"format":"qcow2","format-specific":{"type":"qcow2","data":{"compat":"1.1"}}}`, ""},
		"raw":                {`{"format":"raw"}`, ""},
		"vmdk self extent":   {`{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"` + self + `"}]}}}`, ""},
		"backing file":       {`{"format":"qcow2","backing-filename":"/etc/shadow","backing-filename-format":"raw"}`, "backing file"},
		"full backing only":  {`{"format":"qcow2","full-backing-filename":"/dev/sda"}`, "backing file"},
		"json backing":       {`{"format":"qcow2","backing-filename":"json:{\"file.filename\":\"/etc/shadow\"}"}`, "backing file"},
		"external data file": {`{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`, "external data file"},
		"vmdk foreign extent": {
			`{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"` + self + `"},{"filename":"/dev/sda"}]}}}`,
			"extent",
		},
		"qed":     {`{"format":"qed"}`, "not supported"},
		"qcow v1": {`{"format":"qcow"}`, "not supported"},
		"empty":   {`{}`, "not supported"},
	}
	for name, tc := range cases {
		got := checkImageHeader(mk(tc.info), self)
		if tc.want == "" {
			assert.Empty(t, got, name)
		} else {
			assert.Contains(t, got, tc.want, name)
		}
	}
}

// TestInspectHostImage_RealQemuImg runs the header check against the REAL
// qemu-img (when installed) so the JSON shape the parser relies on is pinned to
// what qemu-img actually prints, not to hand-written fixtures.
func TestInspectHostImage_RealQemuImg(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	mk := func(args ...string) {
		t.Helper()
		out, err := exec.Command("qemu-img", args...).CombinedOutput() //nolint:gosec // test fixture creation
		require.NoError(t, err, "qemu-img %v: %s", args, out)
	}
	victim := filepath.Join(dir, "victim.raw")
	require.NoError(t, os.WriteFile(victim, []byte("secret\n"), 0o600))
	mk("create", "-q", "-f", "qcow2", filepath.Join(dir, "plain.qcow2"), "1M")
	mk("create", "-q", "-f", "raw", filepath.Join(dir, "plain.raw"), "1M")
	mk("create", "-q", "-f", "vmdk", filepath.Join(dir, "sparse.vmdk"), "1M")
	mk("create", "-q", "-f", "qcow2", "-b", victim, "-F", "raw", filepath.Join(dir, "backed.qcow2"), "1M")
	mk("create", "-q", "-f", "qcow2", "-o", "data_file="+filepath.Join(dir, "data.raw"), filepath.Join(dir, "datafile.qcow2"), "1M")
	mk("create", "-q", "-f", "vmdk", "-o", "subformat=monolithicFlat", filepath.Join(dir, "flat.vmdk"), "1M")

	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///local"
	ctx := context.Background()
	for name, want := range map[string]string{"plain.qcow2": "qcow2", "plain.raw": "raw", "sparse.vmdk": "vmdk"} {
		got, err := inspectHostImage(ctx, vp, "test image", filepath.Join(dir, name))
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
	for name, want := range map[string]string{
		"backed.qcow2":   "backing file",
		"datafile.qcow2": "external data file",
		"flat.vmdk":      "extent",
	} {
		_, err := inspectHostImage(ctx, vp, "test image", filepath.Join(dir, name))
		requireRejected(t, err, want)
	}
}

// TestParseDomainPathRefs proves every host-path-bearing element of a domain
// definition is collected for the in-use check.
func TestParseDomainPathRefs(t *testing.T) {
	const domXML = `<domain type='kvm'>
  <name>victim</name>
  <os>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS.fd'>/var/lib/libvirt/qemu/nvram/victim_VARS.fd</nvram>
    <kernel>/srv/boot/vmlinuz</kernel>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <source file='/var/lib/libvirt/images/overlay.qcow2'/>
      <backingStore type='file'>
        <format type='qcow2'/>
        <source file='/var/lib/libvirt/images/golden-base.qcow2'/>
        <backingStore/>
      </backingStore>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='block' device='disk'><source dev='/dev/vg0/lv1'/><target dev='vdb'/></disk>
    <disk type='volume' device='disk'><source pool='tank' volume='data1'/><target dev='vdc'/></disk>
    <disk type='file' device='cdrom'><source file='/var/lib/libvirt/images/cloud-init/victim-cloud-init.iso'/></disk>
    <filesystem type='mount'><source dir='/srv/share'/><target dir='share'/></filesystem>
    <interface type='bridge'><source bridge='br0'/></interface>
  </devices>
</domain>`
	refs, err := parseDomainPathRefs(domXML)
	require.NoError(t, err)
	for _, want := range []string{
		"/var/lib/libvirt/images/overlay.qcow2",
		"/var/lib/libvirt/images/golden-base.qcow2",
		"/dev/vg0/lv1",
		"/var/lib/libvirt/images/cloud-init/victim-cloud-init.iso",
		"/var/lib/libvirt/qemu/nvram/victim_VARS.fd",
		"/usr/share/OVMF/OVMF_CODE.fd",
		"/srv/boot/vmlinuz",
	} {
		assert.Contains(t, refs.files, want)
	}
	assert.Equal(t, []string{"/srv/share"}, refs.dirs)
	assert.Equal(t, [][2]string{{"tank", "data1"}}, refs.volumes)
	assert.NotContains(t, refs.files, "br0")

	_, err = parseDomainPathRefs("<domain><devices>")
	assert.Error(t, err, "truncated XML must fail (the caller fails closed)")
}

// TestInUseSetContains covers exact and shared-directory membership.
func TestInUseSetContains(t *testing.T) {
	s := inUseSet{files: map[string]bool{"/a/b.qcow2": true}, dirs: []string{"/srv/share"}}
	assert.True(t, s.contains("/a/b.qcow2"))
	assert.True(t, s.contains("/srv/share/x.qcow2"))
	assert.True(t, s.contains("/srv/share"))
	assert.False(t, s.contains("/srv/shared/x.qcow2"))
	assert.False(t, s.contains("/a/c.qcow2"))
}

// ---------------------------------------------------------------------------
// Host-side confinement against a fake hypervisor host
//
// The fake host is the local machine: "!" host commands (realpath, stat, test,
// rm) run for real on a scratch directory tree with real symlinks, while
// `virsh`, `qemu-img`, `sudo`, `curl` and `wget` are replaced on PATH by
// scripts that serve per-host fixture data and log their argv. A VirshProvider
// with a non-ssh URI routes through runLocal, exactly the argv the SSH
// transport would quote and send.
// ---------------------------------------------------------------------------

// fakeVirshScript serves per-host fixtures from $FAKE_HOST_DIR/<uri tail>/.
const fakeVirshScript = `#!/bin/sh
uri=""
if [ "$1" = "-c" ]; then uri="$2"; shift 2; fi
host="${uri##*/}"
d="$FAKE_HOST_DIR/$host"
printf '%s\t%s\n' "$host" "$*" >> "$FAKE_HOST_DIR/virsh.log"
case "$1" in
  list)
    if [ "$3" = "--uuid" ]; then
      # UUIDs listed in "vanishing" are gone from every listing after the first.
      if [ -f "$d/vanishing" ] && [ -f "$d/listed-once" ]; then
        grep -v -F -f "$d/vanishing" "$d/uuids" || true
      else
        cat "$d/uuids" 2>/dev/null
      fi
      touch "$d/listed-once" 2>/dev/null
      exit 0
    fi
    printf ' Id   Name   State\n---------------------\n\n' ;;
  dumpxml) exec cat "$d/dom-$2.xml" ;;
  pool-list) printf ' Name      State    Autostart\n-------------------------------\n default   active   yes\n\n' ;;
  pool-info) printf 'Name:           default\nState:          running\n' ;;
  pool-dumpxml) printf "<pool type='dir'><name>default</name><target><path>%s</path></target></pool>\n<!-- /var/lib/libvirt/images -->\n" "$(cat "$d/pooldir")" ;;
  vol-path) exec cat "$d/vol-$3-$5" ;;
  *) exit 0 ;;
esac
`

// fakeQemuImgScript answers `info` from a <file>.info.json sidecar (default: a
// plain qcow2) and `info --backing-chain` from <file>.chain.json (default: the
// file alone; <file>.chainfail simulates a broken link), fails like qemu-img
// for a missing file, and makes `convert` write its target. Every call is
// logged.
const fakeQemuImgScript = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_HOST_DIR/qemu-img.log"
last=""
chain=""
for a in "$@"; do last="$a"; if [ "$a" = "--backing-chain" ]; then chain=1; fi; done
missing() { echo "qemu-img: Could not open '$last': Could not open '$last': No such file or directory" >&2; exit 1; }
case "$1" in
  info)
    if [ -n "$chain" ]; then
      if [ -f "$last.chainfail" ]; then echo "qemu-img: Could not open backing file: No such file or directory" >&2; exit 1; fi
      if [ -f "$last.chain.json" ]; then cat "$last.chain.json"; exit 0; fi
      [ -e "$last" ] || missing
      printf '[{"format":"qcow2","filename":"%s"}]\n' "$last"; exit 0
    fi
    if [ -f "$last.info.json" ]; then cat "$last.info.json"; exit 0; fi
    [ -e "$last" ] || missing
    printf '{"format":"qcow2","filename":"%s"}\n' "$last" ;;
  convert) printf 'converted\n' > "$last" ;;
  *) exit 0 ;;
esac
`

// fakeLoggerScript logs its argv to $FAKE_HOST_DIR/<name>.log and succeeds
// (stands in for sudo: nothing privileged ever runs in tests).
const fakeLoggerScript = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_HOST_DIR/$(basename "$0").log"
exit 0
`

// fakeDownloadScript stands in for curl (-fsSL URL -o DST) and wget (-O DST
// URL): it writes a payload to DST and, when $FAKE_HOST_DIR/download.info.json
// exists, installs it as DST's qemu-img info sidecar.
const fakeDownloadScript = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_HOST_DIR/$(basename "$0").log"
dst=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ] || [ "$prev" = "-O" ]; then dst="$a"; fi
  prev="$a"
done
printf 'payload\n' > "$dst"
if [ -f "$FAKE_HOST_DIR/download.info.json" ]; then cp "$FAKE_HOST_DIR/download.info.json" "$dst.info.json"; fi
`

// fakeHost is a scratch hypervisor host for the confinement tests.
type fakeHost struct {
	t       *testing.T
	root    string // FAKE_HOST_DIR (fixtures + logs)
	base    string // canonical scratch filesystem root (not under a forbidden dir)
	images  string // the allowed image dir == default pool dir (canonical)
	outside string // a directory outside the allowlist (canonical)
}

// requireGNURealpath skips the host-side tests where GNU coreutils realpath is
// unavailable (the provider requires it on the hypervisor host).
func requireGNURealpath(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("realpath", "-e", "-z", "--", "/").Output(); err != nil || string(out) != "/\x00" {
		t.Skip("GNU coreutils realpath (-e/-z) not available")
	}
}

// scratchBase returns a canonical scratch directory that is NOT below one of
// forbiddenImageDirRoots (t.TempDir lives in /tmp, which the policy forbids as
// an image directory), removed at test end.
func scratchBase(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	candidates := []string{wd}
	if home, herr := os.UserHomeDir(); herr == nil {
		candidates = append(candidates, home)
	}
	for _, parent := range candidates {
		dir, err := os.MkdirTemp(parent, "_imagepath_test_")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		canon, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		if !isForbiddenImageDir(canon) {
			return canon
		}
	}
	t.Skip("no writable scratch directory outside the forbidden image-dir roots")
	return ""
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	requireGNURealpath(t)

	bin := t.TempDir()
	for name, script := range map[string]string{
		"virsh": fakeVirshScript, "qemu-img": fakeQemuImgScript, "sudo": fakeLoggerScript,
		"curl": fakeDownloadScript, "wget": fakeDownloadScript,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := &fakeHost{t: t, root: t.TempDir(), base: scratchBase(t)}
	t.Setenv("FAKE_HOST_DIR", h.root)
	h.images = filepath.Join(h.base, "images")
	h.outside = filepath.Join(h.base, "secret")
	require.NoError(t, os.MkdirAll(h.images, 0o750))
	require.NoError(t, os.MkdirAll(h.outside, 0o750))
	return h
}

// host returns a local VirshProvider for fake host `name` (its data lives in
// FAKE_HOST_DIR/name) with the default pool at h.images.
func (h *fakeHost) host(name string) *VirshProvider {
	h.t.Helper()
	dir := filepath.Join(h.root, name)
	require.NoError(h.t, os.MkdirAll(dir, 0o750))
	require.NoError(h.t, os.WriteFile(filepath.Join(dir, "pooldir"), []byte(h.images), 0o600))
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///" + name
	return vp
}

// file writes a non-empty file and returns its path.
func (h *fakeHost) file(dir, name string) string {
	h.t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(h.t, os.WriteFile(p, []byte("image-bytes"), 0o600))
	return p
}

// info installs a qemu-img info sidecar for path.
func (h *fakeHost) info(path, js string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(path+".info.json", []byte(js), 0o600))
}

// domain defines a domain (uuid) on host `name` with the given XML.
func (h *fakeHost) domain(name, uuid, domXML string) {
	h.t.Helper()
	dir := filepath.Join(h.root, name)
	f, err := os.OpenFile(filepath.Join(dir, "uuids"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(h.t, err)
	_, err = f.WriteString(uuid + "\n")
	require.NoError(h.t, err)
	require.NoError(h.t, f.Close())
	if domXML != "" {
		require.NoError(h.t, os.WriteFile(filepath.Join(dir, "dom-"+uuid+".xml"), []byte(domXML), 0o600))
	}
}

// log returns the content of a fake tool's log.
func (h *fakeHost) log(tool string) string {
	b, _ := os.ReadFile(filepath.Join(h.root, tool+".log")) //nolint:gosec // test reads its own fixture log
	return string(b)
}

func diskDomainXML(sources ...string) string {
	var b strings.Builder
	b.WriteString("<domain type='kvm'><name>d</name><devices>")
	for _, s := range sources {
		b.WriteString("<disk type='file' device='disk'><source file='" + s + "'/></disk>")
	}
	b.WriteString("</devices></domain>")
	return b.String()
}

const (
	uuidA = "11111111-2222-3333-4444-555555555555"
	uuidB = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

// requireRejected asserts err is the non-retryable InvalidArgument rejection
// and its message contains want.
func requireRejected(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "must be InvalidArgument: %v", err)
	assert.True(t, isInvalidArgument(err))
	assert.Contains(t, err.Error(), want)
}

// TestConfine_AllowedImage is the positive case: a regular, self-contained
// image directly in the allowed directory resolves to its canonical path, is
// copied (not adopted), and its probed format is reported.
func TestConfine_AllowedImage(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	img := h.file(h.images, "ubuntu-22.04.qcow2")
	h.domain("h1", uuidA, diskDomainXML(filepath.Join(h.images, "web-disk.qcow2")))

	pol := imagePathPolicy{dirs: []string{h.images}}
	got, err := pol.confine(context.Background(), vp, imagePathRequest{Path: img})
	require.NoError(t, err)
	assert.Equal(t, img, got.Path)
	assert.Equal(t, "qcow2", got.Format)
	assert.False(t, got.AdoptInPlace, "a base image must never be attached in place")

	// A symlink INSIDE the allowed dir to another file inside it is fine, and
	// the canonical target is what gets used.
	link := filepath.Join(h.images, "latest.qcow2")
	require.NoError(t, os.Symlink(img, link))
	got, err = pol.confine(context.Background(), vp, imagePathRequest{Path: link})
	require.NoError(t, err)
	assert.Equal(t, img, got.Path, "the canonical path, not the caller's string, must be used")

	// A configured allowed dir that is itself a symlink is canonicalized too.
	aliasDir := filepath.Join(h.base, "images-alias")
	require.NoError(t, os.Symlink(h.images, aliasDir))
	pol2 := imagePathPolicy{dirs: []string{aliasDir}}
	got, err = pol2.confine(context.Background(), vp, imagePathRequest{Path: filepath.Join(aliasDir, "ubuntu-22.04.qcow2")})
	require.NoError(t, err)
	assert.Equal(t, img, got.Path)
}

// TestConfine_Rejections is the table of paths the confinement must refuse.
func TestConfine_Rejections(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")

	// Fixtures.
	secret := h.file(h.outside, "tenant-b-data.qcow2")
	escape := filepath.Join(h.images, "escape.qcow2")
	require.NoError(t, os.Symlink(secret, escape))
	devLink := filepath.Join(h.images, "devnull.img")
	require.NoError(t, os.Symlink("/dev/null", devLink))
	etcLink := filepath.Join(h.images, "shadow.qcow2")
	require.NoError(t, os.Symlink("/etc/shadow", etcLink))
	dirLink := filepath.Join(h.images, "up.qcow2")
	require.NoError(t, os.Symlink("..", dirLink))
	victimDisk := h.file(h.images, "victim-disk.qcow2")
	importedDisk := h.file(h.images, "victim-migrated.qcow2")
	inUse := h.file(h.images, "golden-live.qcow2")        // attached to a domain under a neutral name
	backingInUse := h.file(h.images, "golden-base.qcow2") // backing file of a domain's overlay
	shared := filepath.Join(h.images, "sharedvol.qcow2")
	h.file(h.images, "sharedvol.qcow2")
	staging := h.file(h.images, ".virtrigaud-imageprepare-x.download")
	require.NoError(t, os.Mkdir(filepath.Join(h.images, "adir.qcow2"), 0o750))
	empty := filepath.Join(h.images, "empty.qcow2")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	backed := h.file(h.images, "backed.qcow2")
	h.info(backed, `{"format":"qcow2","backing-filename":"/etc/shadow","backing-filename-format":"raw"}`)
	dataFile := h.file(h.images, "datafile.qcow2")
	h.info(dataFile, `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`)
	vmdk := h.file(h.images, "desc.vmdk")
	h.info(vmdk, `{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/dev/sda"}]}}}`)
	sub := filepath.Join(h.images, "cloud-init")
	require.NoError(t, os.Mkdir(sub, 0o750))
	seed := h.file(sub, "victim-cloud-init.iso")

	h.domain("h1", uuidA, diskDomainXML(inUse))
	h.domain("h1", uuidB, `<domain><devices>
	  <disk type='file' device='disk'><source file='`+filepath.Join(h.images, "overlay-x.qcow2")+`'/>
	    <backingStore type='file'><source file='`+backingInUse+`'/></backingStore></disk>
	  <disk type='volume' device='disk'><source pool='default' volume='sharedvol.qcow2'/></disk>
	</devices></domain>`)
	require.NoError(t, os.WriteFile(filepath.Join(h.root, "h1", "vol-default-sharedvol.qcow2"), []byte(shared+"\n"), 0o600))

	const notAllowed = "does not resolve to a file directly inside an allowed image directory"
	cases := map[string]struct{ path, want string }{
		"symlink escaping the allowed dir": {escape, notAllowed},
		"dotdot traversal":                 {h.images + "/../secret/tenant-b-data.qcow2", "'..'"},
		"/etc/shadow":                      {"/etc/shadow", notAllowed},
		"symlink to /etc/shadow":           {etcLink, notAllowed},
		"/dev/sda":                         {"/dev/sda", notAllowed},
		"/dev/null":                        {"/dev/null", notAllowed},
		"symlink to a device":              {devLink, notAllowed},
		"/proc/self/environ":               {"/proc/self/environ", notAllowed},
		"symlink to a directory":           {dirLink, notAllowed},
		"subdirectory (cloud-init seed)":   {seed, notAllowed},
		"outside file directly":            {secret, notAllowed},
		"another VM's disk (by name)":      {victimDisk, "VirtRigaud-managed file"},
		"another VM's imported disk":       {importedDisk, "VirtRigaud-managed file"},
		"staging dotfile":                  {staging, "VirtRigaud-managed file"},
		"another VM's disk (in use)":       {inUse, "existing VM"},
		"another VM's backing file":        {backingInUse, "existing VM"},
		"another VM's volume disk":         {shared, "existing VM"},
		"leading dash":                     {"-" + secret, "begin with '-'"},
		"relative path":                    {"images/ubuntu.qcow2", "absolute"},
		"nonexistent in allowed dir":       {filepath.Join(h.images, "nope.qcow2"), "does not exist"},
		"a directory":                      {filepath.Join(h.images, "adir.qcow2"), "not a non-empty regular file"},
		"an empty file":                    {empty, "not a non-empty regular file"},
		"qcow2 with backing file":          {backed, "backing file"},
		"qcow2 with external data file":    {dataFile, "external data file"},
		"vmdk with a foreign extent":       {vmdk, "extent"},
		"control characters":               {filepath.Join(h.images, "x\n.qcow2"), "control characters"},
	}
	pol := imagePathPolicy{dirs: []string{h.images}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: tc.path})
			requireRejected(t, err, tc.want)
		})
	}

	// The canonical target of a symlink never leaks into the message, and the
	// allowed directories are not disclosed either.
	_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: escape})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "tenant-b-data", "symlink target must not be disclosed")
	assert.NotContains(t, err.Error(), h.outside)
	_, err = pol.confine(context.Background(), vp, imagePathRequest{Path: "/etc/shadow"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), h.images, "allowed directories must not be disclosed")

	// None of the rejected paths was ever handed to qemu-img convert.
	assert.NotContains(t, h.log("qemu-img"), "convert")
}

// TestConfine_NoExistenceOracle proves a path outside the allowed directories
// gets the same answer whether or not it exists on the host.
func TestConfine_NoExistenceOracle(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	pol := imagePathPolicy{dirs: []string{h.images}}

	_, errExists := pol.confine(context.Background(), vp, imagePathRequest{Path: "/etc/passwd"})
	_, errMissing := pol.confine(context.Background(), vp, imagePathRequest{Path: "/etc/virtrigaud-does-not-exist"})
	requireRejected(t, errExists, "allowed image directory")
	requireRejected(t, errMissing, "allowed image directory")
	reason := func(err error) string { return err.Error()[strings.Index(err.Error(), "rejected:"):] }
	assert.Equal(t, reason(errExists), reason(errMissing))
}

// TestConfine_ConfiguredDirThatResolvesToSystemDir proves a configured image
// directory that is a symlink into a forbidden root is ignored on the host.
func TestConfine_ConfiguredDirThatResolvesToSystemDir(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	etcAlias := filepath.Join(h.base, "etc-alias")
	require.NoError(t, os.Symlink("/etc", etcAlias))

	pol := imagePathPolicy{dirs: []string{etcAlias}}
	_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: filepath.Join(etcAlias, "hostname")})
	requireRejected(t, err, "allowed image directory")
}

// TestConfine_ImportedDisk covers the only attach-in-place case: a VM's own
// migration landing disk in the pool directory.
func TestConfine_ImportedDisk(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	own := h.file(h.images, "web-migrated.qcow2")
	base := h.file(h.images, "ubuntu.qcow2")
	busy := h.file(h.images, "db-migrated.qcow2")
	h.domain("h1", uuidA, diskDomainXML(busy))
	// A separate image directory (not the pool) so the adopt rule is not
	// satisfied just by the allowlist.
	golden := filepath.Join(h.base, "golden")
	require.NoError(t, os.Mkdir(golden, 0o750))
	pol := imagePathPolicy{dirs: []string{h.images, golden}}
	ctx := context.Background()

	got, err := pol.confine(ctx, vp, imagePathRequest{Path: own, ImportedDisk: true, VMName: "web", PoolDir: h.images})
	require.NoError(t, err)
	assert.True(t, got.AdoptInPlace, "a VM's own imported disk is attached in place")
	assert.Equal(t, own, got.Path)

	// Another VM's imported disk is neither adoptable nor a usable base image.
	_, err = pol.confine(ctx, vp, imagePathRequest{Path: own, ImportedDisk: true, VMName: "attacker", PoolDir: h.images})
	requireRejected(t, err, "VirtRigaud-managed file")

	// The imported-disk flag on a base image only ever yields a copy.
	got, err = pol.confine(ctx, vp, imagePathRequest{Path: base, ImportedDisk: true, VMName: "ubuntu", PoolDir: h.images})
	require.NoError(t, err)
	assert.False(t, got.AdoptInPlace)

	// Own name, but another domain already uses it.
	_, err = pol.confine(ctx, vp, imagePathRequest{Path: busy, ImportedDisk: true, VMName: "db", PoolDir: h.images})
	requireRejected(t, err, "existing VM")

	// Own name but NOT in the pool directory: not adoptable, and reserved.
	elsewhere := h.file(golden, "web-migrated.qcow2")
	_, err = pol.confine(ctx, vp, imagePathRequest{Path: elsewhere, ImportedDisk: true, VMName: "web", PoolDir: h.images})
	requireRejected(t, err, "VirtRigaud-managed file")

	// Without the ImportedDisk flag (a VMImage path) it is never adopted.
	_, err = pol.confine(ctx, vp, imagePathRequest{Path: own, VMName: "web", PoolDir: h.images})
	requireRejected(t, err, "VirtRigaud-managed file")
}

// TestConfine_FailsClosed proves an unreadable domain definition blocks the
// image (retryable) instead of letting an incomplete in-use set through.
func TestConfine_FailsClosed(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	img := h.file(h.images, "ubuntu.qcow2")
	h.domain("h1", uuidA, "") // listed, but dumpxml fails

	_, err := imagePathPolicy{dirs: []string{h.images}}.confine(context.Background(), vp, imagePathRequest{Path: img})
	requireGenericHostFailure(t, err, h)
}

// requireGenericHostFailure asserts err is the generic, retryable host-check
// failure and discloses nothing about the host: no domain UUIDs, no allowed or
// other directories, no command line.
func requireGenericHostFailure(t *testing.T, err error, h *fakeHost) {
	t.Helper()
	require.Error(t, err)
	assert.False(t, isInvalidArgument(err), "a host read failure is not the caller's fault")
	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	assert.True(t, pe.IsRetryable())
	assert.Contains(t, err.Error(), hostCheckFailedMessage)
	for _, leak := range []string{uuidA, uuidB, h.images, h.base, "virsh", "dumpxml", "vol-path", "qemu-img"} {
		assert.NotContains(t, err.Error(), leak, "host detail must stay in the provider log")
	}
}

// TestConfine_UnresolvableVolumeFailsClosed proves a volume disk whose path
// cannot be resolved blocks the check instead of being skipped.
func TestConfine_UnresolvableVolumeFailsClosed(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	img := h.file(h.images, "ubuntu.qcow2")
	h.domain("h1", uuidA, `<domain><devices><disk type='volume' device='disk'>`+
		`<source pool='inactive' volume='data.qcow2'/></disk></devices></domain>`) // no vol-path fixture

	_, err := imagePathPolicy{dirs: []string{h.images}}.confine(context.Background(), vp, imagePathRequest{Path: img})
	requireGenericHostFailure(t, err, h)
}

// TestConfine_DomainUndefinedDuringCheckIsSkipped proves a domain that vanishes
// between the list and its dumpxml does not fail the create.
func TestConfine_DomainUndefinedDuringCheckIsSkipped(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	img := h.file(h.images, "ubuntu.qcow2")
	h.domain("h1", uuidA, "") // listed first, then gone; dumpxml fails
	require.NoError(t, os.WriteFile(filepath.Join(h.root, "h1", "vanishing"), []byte(uuidA+"\n"), 0o600))

	got, err := imagePathPolicy{dirs: []string{h.images}}.confine(context.Background(), vp, imagePathRequest{Path: img})
	require.NoError(t, err)
	assert.Equal(t, img, got.Path)
}

// TestConfine_ShutOffDomainBackingChainIsInUse proves the base images of a
// STOPPED domain count as in use although its dumpxml carries no
// <backingStore>: the chain is read from the images themselves (qemu-img -U).
func TestConfine_ShutOffDomainBackingChainIsInUse(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	overlay := h.file(h.images, "vm1-overlay.qcow2")
	base := h.file(h.images, "golden-base.qcow2")
	require.NoError(t, os.WriteFile(overlay+".chain.json", []byte(`[
	  {"filename":"`+overlay+`","format":"qcow2","backing-filename":"golden-base.qcow2","full-backing-filename":"`+base+`"},
	  {"filename":"`+base+`","format":"qcow2"}]`), 0o600))

	// A broken chain (a deleted link) is walked one level at a time.
	overlay2 := h.file(h.images, "vm2-overlay.qcow2")
	mid := h.file(h.images, "golden-mid.qcow2")
	require.NoError(t, os.WriteFile(overlay2+".chainfail", nil, 0o600))
	h.info(overlay2, `{"filename":"`+overlay2+`","format":"qcow2","full-backing-filename":"`+mid+`"}`)
	h.info(mid, `{"filename":"`+mid+`","format":"qcow2","full-backing-filename":"`+filepath.Join(h.images, "deleted.qcow2")+`"}`)

	// A data file of a stopped VM's disk is in use too.
	withData := h.file(h.images, "vm3.qcow2")
	dataFile := h.file(h.images, "vm3-data.raw")
	require.NoError(t, os.WriteFile(withData+".chain.json", []byte(`[{"filename":"`+withData+`","format":"qcow2",`+
		`"format-specific":{"type":"qcow2","data":{"data-file":"`+dataFile+`"}}}]`), 0o600))

	h.domain("h1", uuidA, diskDomainXML(overlay, overlay2)) // shut off: no <backingStore>
	h.domain("h1", uuidB, diskDomainXML(withData))

	pol := imagePathPolicy{dirs: []string{h.images}}
	for _, p := range []string{base, mid, dataFile} {
		_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: p})
		requireRejected(t, err, "existing VM")
	}
	assert.Contains(t, h.log("qemu-img"), "info -U --backing-chain --output=json -- "+overlay)

	// Unrelated images stay usable.
	free := h.file(h.images, "ubuntu.qcow2")
	_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: free})
	require.NoError(t, err)
}

// TestHostConnRunner_ImportHeaderCheck proves the import paths' adapter carries
// the header check over a hostconn.Conn: a staged object with a backing file is
// refused before any convert.
func TestHostConnRunner_ImportHeaderCheck(t *testing.T) {
	conn := &fakeSeamConn{runHostOut: `{"format":"qcow2","backing-filename":"/etc/shadow"}`}
	_, err := inspectHostImageAs(context.Background(), hostConnRunner{conn: conn}, importedImageSubject,
		"/var/lib/libvirt/images/.virtrigaud-import-x.vmdk", "qcow2")
	requireRejected(t, err, "backing file")

	conn = &fakeSeamConn{runHostOut: `{"format":"qcow2"}`}
	format, err := inspectHostImageAs(context.Background(), hostConnRunner{conn: conn}, importedImageSubject,
		"/var/lib/libvirt/images/.virtrigaud-import-x.qcow2", "qcow2")
	require.NoError(t, err)
	assert.Equal(t, "qcow2", format)
}

// TestCreateVolumeFromImageFile_RejectsCraftedImport proves ImportDisk's
// landing step refuses an imported disk whose header points at a host file,
// on both its convert and its attach-in-place branch.
func TestCreateVolumeFromImageFile_RejectsCraftedImport(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	inPool := h.file(h.images, "web-migrated.qcow2") // attach-in-place branch
	h.info(inPool, `{"format":"qcow2","backing-filename":"/etc/shadow","backing-filename-format":"raw"}`)
	external := h.file(h.outside, "staged.qcow2") // convert branch
	h.info(external, `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`)

	sp := NewStorageProvider(vp)
	_, err := sp.CreateVolumeFromImageFile(context.Background(), inPool, "web-migrated", "default", 0)
	requireRejected(t, err, "backing file")
	_, err = sp.CreateVolumeFromImageFile(context.Background(), external, "web-migrated", "default", 0)
	requireRejected(t, err, "external data file")
	assert.NotContains(t, h.log("qemu-img"), "convert")
	assert.Contains(t, h.log("qemu-img"), "info --output=json -f qcow2 -- "+inPool)
}

// TestImagePrepare_ExistingTargetInUseIsRejected covers the upgrade story: an
// earlier release attached a prepared image in place as the first VM's disk,
// so the "already prepared" short-circuit must not hand it out again.
func TestImagePrepare_ExistingTargetInUseIsRejected(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}
	legacy := h.file(h.images, "ubuntu.qcow2")
	h.domain("h1", uuidA, diskDomainXML(legacy))

	_, _, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`, "ubuntu", "")
	requireRejected(t, err, "legacy in-place attach")
	assert.Contains(t, err.Error(), "create a VMImage with a new name")
	assert.NotContains(t, err.Error(), h.images, "the host path is not disclosed")

	// Not in use: the idempotent no-op still returns the prepared location.
	h.file(h.images, "debian.qcow2")
	id, path, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`, "debian", "")
	require.NoError(t, err)
	assert.Equal(t, "debian", id)
	assert.Equal(t, filepath.Join(h.images, "debian.qcow2"), path)
	assert.Empty(t, h.log("curl"), "an existing, unused target is not re-downloaded")
}

// TestConfine_OverSSH_HostilePathIsInert drives the confinement through the
// REAL SSH transport (loopback sshd, remote /bin/sh) with a path full of shell
// syntax: it must reach realpath as one inert argument and be rejected.
func TestConfine_OverSSH_HostilePathIsInert(t *testing.T) {
	h := newFakeHost(t)
	vp := newInjectionTestProvider(t)
	mark := filepath.Join(t.TempDir(), "pwned")
	pol := imagePathPolicy{dirs: []string{h.images}}

	for _, p := range []string{
		h.images + "/x$(touch " + mark + ").qcow2",
		h.images + "/x`touch " + mark + "`.qcow2",
		h.images + "/x;touch " + mark + ".qcow2",
		"/etc/shadow;touch " + mark,
	} {
		_, err := pol.confine(context.Background(), vp, imagePathRequest{Path: p})
		requireRejected(t, err, "rejected")
	}
	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "an injected command ran on the host")
}

// ---------------------------------------------------------------------------
// Integration with Create / ImagePrepare / DownloadCloudImage
// ---------------------------------------------------------------------------

// TestCreateVM_RejectsHostileImagePathBeforeDiskWork proves the Create path
// confines the image first: /etc/shadow never reaches qemu-img, and the
// rejection leaves the provider (and the gRPC server) as InvalidArgument —
// not the retryable "failed to create VM" wrapper.
func TestCreateVM_RejectsHostileImagePathBeforeDiskWork(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}

	for _, img := range []contracts.VMImage{
		{Path: "/etc/shadow"},
		{Path: "/etc/shadow", ImportedDisk: true},
		{TemplateName: "/etc/shadow"}, // any path-shaped image spec is confined
	} {
		_, err := p.createVM(context.Background(), vp, contracts.CreateRequest{Name: "web", Image: img})
		requireRejected(t, err, "allowed image directory")
		var pe *contracts.ProviderError
		if errors.As(err, &pe) {
			assert.False(t, pe.IsRetryable(), "a rejected path must not be reported retryable")
		}
	}
	assert.NotContains(t, h.log("qemu-img"), "convert")

	// Through the gRPC server: the status code on the wire is InvalidArgument.
	s := NewServer(p)
	_, err := s.Create(context.Background(), &providerv1.CreateRequest{Name: "web", ImageJson: `{"Path":"/etc/shadow"}`})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestCreateDiskFromHostImage_CopiesBaseImage proves an allowed base image is
// COPIED into the VM's own disk (with the probed format), never adopted.
func TestCreateDiskFromHostImage_CopiesBaseImage(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	img := h.file(h.images, "ubuntu.qcow2")
	h.info(img, `{"format":"raw"}`)
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}

	vol, err := p.createDiskFromHostImage(context.Background(), vp, NewStorageProvider(vp),
		contracts.CreateRequest{Name: "web", Owner: ownerTeamA, Image: contracts.VMImage{Path: img}},
		"team-a.web", img, vmDiskVolumeName("team-a.web"), 10)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.images, "team-a.web-disk.qcow2"), vol.Path, "the VM gets its own disk, named after its domain")
	assert.NotEqual(t, img, vol.Path)
	assert.Contains(t, h.log("qemu-img"), "convert -f raw -O qcow2 "+img+" "+vol.Path)
}

// TestCreateDiskFromHostImage_AdoptsOwnImportedDisk proves the migration
// landing disk is still attached in place (no copy): <domain>-migrated.qcow2,
// where <domain> is the namespaced domain name for a request with an owner and
// the bare VM name for an older manager's request.
func TestCreateDiskFromHostImage_AdoptsOwnImportedDisk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owner  contracts.ObjectIdentity
		domain string
	}{
		{"namespaced", ownerTeamA, "team-a.web"},
		{"legacy (no owner)", contracts.ObjectIdentity{}, "web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			vp := h.host("h1")
			own := h.file(h.images, tc.domain+"-migrated.qcow2")
			p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}

			vol, err := p.createDiskFromHostImage(context.Background(), vp, NewStorageProvider(vp),
				contracts.CreateRequest{Name: "web", Owner: tc.owner, Image: contracts.VMImage{Path: own, ImportedDisk: true}},
				tc.domain, own, vmDiskVolumeName(tc.domain), 10)
			require.NoError(t, err)
			assert.Equal(t, own, vol.Path)
			assert.NotContains(t, h.log("qemu-img"), "convert")
		})
	}
}

// TestCreate_Clustered_ConfinesOnTargetHost proves the checks run against the
// LEASED target host (ADR-0007), not a default host: the image is in use only
// on host-b, and only host-b is consulted.
func TestCreate_Clustered_ConfinesOnTargetHost(t *testing.T) {
	h := newFakeHost(t)
	vpA, vpB := h.host("host-a"), h.host("host-b")
	img := h.file(h.images, "golden.qcow2")
	h.domain("host-b", uuidB, diskDomainXML(img)) // in use on host-b only

	inv := twoHostInventory()
	vcs := map[string]*virshConn{
		"host-a": newClusteredVirshConn("host-a", vpA, nil),
		"host-b": newClusteredVirshConn("host-b", vpB, nil),
	}
	p, _ := newClusteredProviderForTest(t, inv, func(_ context.Context, host hostsecret.Host) (hostconn.Conn, error) {
		return vcs[host.ID], nil
	})
	p.imageDirs = []string{h.images}
	p.virshProvider = h.host("default-host")

	_, err := p.Create(context.Background(), contracts.CreateRequest{
		Name: "web", TargetHostID: "host-b", Image: contracts.VMImage{Path: img},
	})
	requireRejected(t, err, "existing VM")

	virshLog := h.log("virsh")
	assert.Contains(t, virshLog, "host-b\tlist --all --uuid", "the in-use check must run on the target host")
	assert.NotContains(t, virshLog, "host-a\t", "a non-target host is never consulted")
	assert.NotContains(t, virshLog, "default-host\t", "the single-host default is never consulted")
}

// TestImagePrepare_ConfinesSourcePath proves ImagePrepare's host-path source is
// confined before checksum/convert.
func TestImagePrepare_ConfinesSourcePath(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}

	for _, js := range []string{
		`{"source":{"libvirt":{"path":"/etc/shadow"}}}`,
		`{"source":{"libvirt":{"path":"/etc/shadow","url":"https://x/y.qcow2","checksum":"abc"}}}`,
		`{"Path":"/dev/sda"}`,
	} {
		_, _, err := p.imagePrepare(context.Background(), js, "ubuntu", "")
		requireRejected(t, err, "allowed image directory")
	}
	assert.NotContains(t, h.log("qemu-img"), "convert")

	// An allowed source is prepared (converted with the probed format).
	src := h.file(h.images, "upstream.img")
	h.info(src, `{"format":"raw"}`)
	id, path, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{"path":"`+src+`"}}}`, "ubuntu", "")
	require.NoError(t, err)
	assert.Equal(t, "ubuntu", id)
	assert.Equal(t, filepath.Join(h.images, "ubuntu.qcow2"), path)
	assert.Contains(t, h.log("qemu-img"), "convert -f raw -O qcow2 "+src)

	// The gRPC server reports the rejection as InvalidArgument.
	s := NewServer(p)
	_, err = s.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson: `{"source":{"libvirt":{"path":"/etc/shadow"}}}`, TargetName: "x",
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestImagePrepare_RejectsReservedTargetName proves a VMImage cannot be
// prepared under a name that aliases a VM disk or an imported disk.
func TestImagePrepare_RejectsReservedTargetName(t *testing.T) {
	p := &Provider{virshProvider: &VirshProvider{}}
	for _, name := range []string{"victim-disk", "victim-migrated", ".hidden"} {
		_, _, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`, name, "")
		requireRejected(t, err, "collides")
	}
}

// TestImagePrepare_RejectsDownloadWithBackingFile proves a downloaded image
// whose header points at a host file is refused before conversion (qemu-img
// convert would otherwise flatten /etc/shadow into the template).
func TestImagePrepare_RejectsDownloadWithBackingFile(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	require.NoError(t, os.WriteFile(filepath.Join(h.root, "download.info.json"),
		[]byte(`{"format":"qcow2","backing-filename":"/etc/shadow","backing-filename-format":"raw"}`), 0o600))
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}

	_, _, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{"url":"https://evil.example/x.qcow2"}}}`, "evil", "")
	requireRejected(t, err, "backing file")
	assert.NotContains(t, err.Error(), "evil.example", "the URL is not echoed into the error")
	assert.NotContains(t, h.log("qemu-img"), "convert")
	_, statErr := os.Stat(filepath.Join(h.images, ".virtrigaud-imageprepare-evil.download"))
	assert.True(t, os.IsNotExist(statErr), "the rejected download is removed")
}

// TestDownloadCloudImage_RejectsBackingFile is the same guard on Create's URL
// path.
func TestDownloadCloudImage_RejectsBackingFile(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	require.NoError(t, os.WriteFile(filepath.Join(h.root, "download.info.json"),
		[]byte(`{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/dev/sda"}]}}}`), 0o600))

	// DownloadCloudImage stages in /tmp/<volume>-temp.img: use a unique volume
	// name and remove the fake's sidecar afterwards.
	volume := fmt.Sprintf("imagepath-test-%d-disk", time.Now().UnixNano())
	staged := filepath.Join("/tmp", volume+"-temp.img")
	t.Cleanup(func() { _ = os.Remove(staged); _ = os.Remove(staged + ".info.json") })

	_, err := NewStorageProvider(vp).DownloadCloudImage(context.Background(), "https://evil.example/x.vmdk", volume, "default", 10)
	requireRejected(t, err, "extent")
	assert.NotContains(t, err.Error(), "evil.example", "the URL is not echoed into the error")
	assert.NotContains(t, h.log("qemu-img"), "convert")
	_, statErr := os.Stat(staged)
	assert.True(t, os.IsNotExist(statErr), "the rejected download is removed")
}
