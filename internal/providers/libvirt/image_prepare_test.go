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
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ---------------------------------------------------------------------------
// ADR-0009 Slice 4 test harness.
//
// The prepare runs against the #334 fake host (imagepath_test.go): every "!"
// host command (sh, mktemp, ln, chmod, stat, sync, find, sha256sum, rm) runs
// for real on a scratch pool directory, so the publish protocol's link(2)
// semantics, modes and inodes are the real ones. curl and qemu-img are
// replaced by scripts that record the mode of the file they write into and
// can be told to fail; sudo only logs (nothing privileged ever runs).
// ---------------------------------------------------------------------------

// prepareCurlScript stands in for curl -q -K - ... -o <dst>: it logs its
// argv, the config it reads from stdin (-K -) and the destination's mode, then
// writes the payload ($FAKE_HOST_DIR/payload, default "payload\n") and the
// write-out ($FAKE_HOST_DIR/download.writeout, default "200"), or fails as
// $FAKE_HOST_DIR/download.fail says ("<write-out> <exit code>").
const prepareCurlScript = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_HOST_DIR/curl.log"
dst=""; cfg=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then dst="$a"; fi
  if [ "$prev" = "-K" ]; then cfg="$a"; fi
  prev="$a"
done
if [ "$cfg" = "-" ]; then cat >> "$FAKE_HOST_DIR/curlrc.log"; elif [ -n "$cfg" ]; then echo "config not on stdin" >&2; exit 2; fi
if [ -n "$dst" ]; then stat -c '%a' -- "$dst" >> "$FAKE_HOST_DIR/download.modes"; fi
if [ -f "$FAKE_HOST_DIR/download.fail" ]; then
  read -r code rc < "$FAKE_HOST_DIR/download.fail"
  printf '%s' "$code"
  echo "curl: ($rc) simulated failure" >&2
  exit "$rc"
fi
if [ -f "$FAKE_HOST_DIR/payload" ]; then cat -- "$FAKE_HOST_DIR/payload" > "$dst"; else printf 'payload\n' > "$dst"; fi
if [ -f "$FAKE_HOST_DIR/download.info.json" ]; then cp "$FAKE_HOST_DIR/download.info.json" "$dst.info.json"; fi
if [ -f "$FAKE_HOST_DIR/download.writeout" ]; then cat -- "$FAKE_HOST_DIR/download.writeout"; else printf '200'; fi
`

// prepareSELinuxEnabledScript stands in for selinuxenabled: SELinux is on
// unless $FAKE_HOST_DIR/selinux.disabled exists.
const prepareSELinuxEnabledScript = `#!/bin/sh
[ ! -f "$FAKE_HOST_DIR/selinux.disabled" ]
`

// prepareHost is a fake host set up for ImagePrepare.
type prepareHost struct {
	*fakeHost
	vp *VirshProvider
	p  *Provider
	s  *Server
}

// newPrepareHost returns a fake host "h1" whose default pool (and only allowed
// image directory) is h.images, with the prepare-specific fake tools.
func newPrepareHost(t *testing.T) *prepareHost {
	t.Helper()
	h := newFakeHost(t)

	convert := `  convert)
    stat -c '%a' -- "$last" >> "$FAKE_HOST_DIR/convert.modes"
    if [ -f "$FAKE_HOST_DIR/convert.fail" ]; then echo "qemu-img: simulated write error" >&2; exit 1; fi
    printf 'converted\n' > "$last" ;;`
	qemuImg := strings.Replace(fakeQemuImgScript, `  convert) printf 'converted\n' > "$last" ;;`, convert, 1)
	require.NotEqual(t, fakeQemuImgScript, qemuImg, "the convert case of the shared fake changed")
	qemuImg = strings.Replace(qemuImg, "case \"$1\" in\n",
		"if [ \"$1\" = info ] && [ -f \"$FAKE_HOST_DIR/info.fail\" ]; then echo \"qemu-img: Could not open: Image is not in qcow2 format\" >&2; exit 1; fi\ncase \"$1\" in\n", 1)
	virsh := strings.Replace(fakeVirshScript, "case \"$1\" in\n",
		"if [ \"$1\" = pool-dumpxml ] && [ -f \"$FAKE_HOST_DIR/pool.missing\" ]; then echo \"error: failed to get pool '$3'\" >&2; "+
			"echo \"error: Storage pool not found: no storage pool with matching name '$3'\" >&2; exit 1; fi\n"+
			"if [ \"$1\" = pool-dumpxml ] && [ -f \"$FAKE_HOST_DIR/pool.recvfail\" ]; then echo \"error: failed to get pool '$3'\" >&2; "+
			"echo \"error: Cannot recv data: Connection reset by peer\" >&2; exit 1; fi\ncase \"$1\" in\n", 1)

	bin := t.TempDir()
	for name, script := range map[string]string{
		"curl": prepareCurlScript, "qemu-img": qemuImg, "virsh": virsh, "selinuxenabled": prepareSELinuxEnabledScript,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	vp := h.host("h1")
	p := &Provider{virshProvider: vp, imageDirs: []string{h.images}}
	return &prepareHost{fakeHost: h, vp: vp, p: p, s: NewServer(p)}
}

// policy is the confinement policy of the harness (for imagePreparer.prepare).
func (h *prepareHost) policy() (imagePathPolicy, error) {
	return imagePathPolicy{dirs: []string{h.images}}, nil
}

// preparer returns an imagePreparer over host (the plain fake host when nil).
func (h *prepareHost) preparer(host imageHost) *imagePreparer {
	if host == nil {
		host = h.vp
	}
	return &imagePreparer{host: host, now: time.Now}
}

// hookedHost wraps the fake host so a test can act just before a host command
// runs, or fail it.
type hookedHost struct {
	*VirshProvider
	mu     sync.Mutex
	before func(args []string) *hookAnswer
	calls  [][]string
}

// hookAnswer is a hook's answer to a host command it handles itself.
type hookAnswer struct {
	res *VirshResult
	err error
}

// hostCalls returns a copy of the host commands seen so far.
func (h *hookedHost) hostCalls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.calls...)
}

// runVirshCommand records args, gives the hook a chance to act or answer, and
// otherwise runs the command on the fake host.
func (h *hookedHost) runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, append([]string(nil), args...))
	hook := h.before
	h.mu.Unlock()
	if hook != nil {
		if a := hook(args); a != nil {
			return a.res, a.err
		}
	}
	return h.VirshProvider.runVirshCommand(ctx, args...)
}

// runHostStdin records the host command (as "!" + argv, like runVirshCommand
// sees it), gives the hook a chance to act or answer, and otherwise runs it on
// the fake host with stdin.
func (h *hookedHost) runHostStdin(ctx context.Context, stdin []byte, argv ...string) (*VirshResult, error) {
	args := append([]string{"!"}, argv...)
	h.mu.Lock()
	h.calls = append(h.calls, args)
	hook := h.before
	h.mu.Unlock()
	if hook != nil {
		if a := hook(args); a != nil {
			return a.res, a.err
		}
	}
	return h.VirshProvider.runHostStdin(ctx, stdin, argv...)
}

// linkCall reports whether args is the publish link script and returns its
// source and destination.
func linkCall(args []string) (src, dst string, ok bool) {
	if len(args) == 7 && args[0] == "!" && args[1] == "sh" && args[3] == linkNoClobberScript {
		return args[5], args[6], true
	}
	return "", "", false
}

// scriptCall reports whether args runs the fixed host script script.
func scriptCall(args []string, script string) bool {
	return len(args) > 3 && args[0] == "!" && args[1] == "sh" && args[2] == "-c" && args[3] == script
}

// transportFailure is what a host command returns when the SSH transport
// fails before reaching the host.
func transportFailure(args []string) *hookAnswer {
	res := &VirshResult{Command: strings.Join(args, " "), ExitCode: -1, Stderr: "ssh: connect: connection refused"}
	return &hookAnswer{res: res, err: &VirshError{Command: res.Command, ExitCode: -1, Stderr: res.Stderr}}
}

const (
	testImageURL    = "https://images.example.com/ubuntu.qcow2?X-Amz-Signature=supersecret"
	testImageURLLog = "https://images.example.com/ubuntu.qcow2?…"
)

// identityProtoReq is an identity ImagePrepareRequest for team-a/ubuntu-22.04.
func identityProtoReq(uid, digest, imageJSON string) *providerv1.ImagePrepareRequest {
	return &providerv1.ImagePrepareRequest{
		ImageJson:    imageJSON,
		Image:        &providerv1.ObjectIdentity{Uid: uid, Namespace: "team-a", Name: "ubuntu-22.04"},
		SourceDigest: digest,
		Provider:     &providerv1.ObjectIdentity{Uid: testProviderUID, Namespace: "team-a", Name: "libvirt"},
	}
}

// urlImageJSON is a VMImageSpec with a libvirt URL source.
func urlImageJSON(url string) string {
	return `{"source":{"libvirt":{"url":"` + url + `"}}}`
}

// artifactNameFor is the libvirt artifact name of team-a/ubuntu-22.04.
func artifactNameFor(t *testing.T, uid, digest string) string {
	t.Helper()
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt,
		contracts.ObjectIdentity{UID: uid, Namespace: "team-a", Name: "ubuntu-22.04"}, digest)
	require.NoError(t, err)
	return name
}

// prepareLegacyImage runs a legacy (identity-less) prepare of target through
// the provider and returns the pre-ADR (id, path) pair.
func prepareLegacyImage(p *Provider, imageJSON, target, hint string) (string, string, error) {
	res, err := p.imagePrepare(context.Background(),
		imageartifact.Request{Mode: imageartifact.ModeLegacy, LegacyTargetName: target}, imageJSON, hint)
	return res.ID, res.Path, err
}

// stagingEntries lists the prepare staging files in dir (test-fixture qemu-img
// info sidecars excluded).
func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), imagePrepareStagingPrefix) && !strings.HasSuffix(e.Name(), ".info.json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// assertNoStagingFiles asserts no prepare staging file is left in dir.
func assertNoStagingFiles(t *testing.T, dir string) {
	t.Helper()
	assert.Empty(t, stagingEntries(t, dir), "every staging file is removed")
}

// fileIdentity returns path's inode, size and permission bits (not following a
// symlink).
func fileIdentity(t *testing.T, path string) (uint64, int64, os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return st.Ino, fi.Size(), fi.Mode().Perm()
}

// plantFile writes content at path (mode 0644) and returns its inode and size.
func plantFile(t *testing.T, path, content string) (uint64, int64) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644)) //nolint:gosec // test fixture
	ino, size, _ := fileIdentity(t, path)
	return ino, size
}

// plantSidecar writes a sidecar document at path with the given mtime.
func plantSidecar(t *testing.T, path string, doc []byte, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, doc, 0o444)) //nolint:gosec // test fixture: a published stamp is 0444
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

// mustSidecar encodes a sidecar for uid/digest recording inode/size.
func mustSidecar(t *testing.T, uid, digest string, inode uint64, size int64) []byte {
	t.Helper()
	data, err := encodeImageSidecar(testSidecar(uid, digest, inode, size))
	require.NoError(t, err)
	return data
}

// captureLog redirects the standard logger (and so slog.Default) into a buffer
// for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// libvirtLegacyCount reads the legacy-request counter for this provider.
func libvirtLegacyCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "virtrigaud_provider_image_prepare_legacy_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider_type" && l.GetValue() == libvirtProviderType {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Identity mode: fresh prepare, reuse, fail-closed Conflict.
// ---------------------------------------------------------------------------

// TestImagePrepareIdentity_FreshPrepare covers a first prepare end to end: the
// artifact at the derived name, read-only and never chowned, its sidecar stamp
// with the artifact's inode and size, the echo, private staging that is
// cleaned up, and a URL that never reaches a command line or a log.
func TestImagePrepareIdentity_FreshPrepare(t *testing.T) {
	h := newPrepareHost(t)
	logs := captureLog(t)
	name := artifactNameFor(t, testImageUID, testDigest)

	resp, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)

	artifact := filepath.Join(h.images, name+qcow2Ext)
	assert.Nil(t, resp.GetTask(), "libvirt prepares synchronously")
	assert.Equal(t, name, resp.GetPreparedImageId())
	assert.Equal(t, artifact, resp.GetPreparedImagePath())
	require.NotNil(t, resp.GetArtifact())
	assert.Equal(t, name, resp.GetArtifact().GetName())
	assert.Equal(t, testImageUID, resp.GetArtifact().GetImage().GetUid())
	assert.Equal(t, "team-a", resp.GetArtifact().GetImage().GetNamespace())
	assert.Equal(t, "ubuntu-22.04", resp.GetArtifact().GetImage().GetName())
	assert.Equal(t, testDigest, resp.GetArtifact().GetSourceDigest())
	assert.False(t, resp.GetArtifact().GetReused())

	// The artifact: the converted image, read-only.
	content, err := os.ReadFile(artifact) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, "converted\n", string(content))
	ino, size, mode := fileIdentity(t, artifact)
	assert.Equal(t, os.FileMode(0o444), mode, "a prepared image is read-only")

	// The sidecar: a trusted stamp for this request recording the artifact.
	sidecar := filepath.Join(h.images, imageSidecarName(name))
	raw, err := os.ReadFile(sidecar) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	doc, err := parseImageSidecar(raw)
	require.NoError(t, err)
	assert.True(t, doc.Matches(testIdentityRequest(testImageUID, testDigest)))
	assert.Equal(t, imageSidecarArtifact{Inode: ino, Size: size}, doc.Artifact)
	assert.Equal(t, testProviderUID, doc.PreparedBy.UID)
	_, _, sidecarMode := fileIdentity(t, sidecar)
	assert.Equal(t, os.FileMode(0o444), sidecarMode)
	assert.NotContains(t, string(raw), "images.example.com", "a stamp never records the URL")

	// Staging: private (0600) mktemp files in the pool directory, all removed.
	assertNoStagingFiles(t, h.images)
	for _, modes := range []string{"download.modes", "convert.modes"} {
		assert.Equal(t, "600\n", h.fixture(modes), "%s: a staging file is private to the SSH user", modes)
	}

	// The URL reaches curl only on stdin (-K -), never on a command line or in
	// a file; the SSH user's ~/.curlrc is ignored (-q first); globbing is off
	// (one transfer); protocols, connect time, total time (the 30m default
	// spec.prepare.timeout) and size (256 GiB default) are bounded.
	curlArgs := h.log("curl")
	assert.NotContains(t, curlArgs, "images.example.com", "the URL is never on a command line")
	assert.True(t, strings.HasPrefix(curlArgs, "-q -K - --globoff "), "curl argv: %s", curlArgs)
	assert.Contains(t, curlArgs, "--proto =http,https,ftp --proto-redir =http,https,ftp")
	assert.Contains(t, curlArgs, "--connect-timeout 30 --max-time 1800 --max-filesize 274877906944")
	assert.Contains(t, curlArgs, "-o "+filepath.Join(h.images, imagePrepareStagingPrefix))
	assert.Equal(t, "globoff\n"+`url = "`+testImageURL+`"`+"\n", h.fixture("curlrc.log"))
	assert.NotContains(t, logs.String(), "supersecret", "the query (a presigned token) is never logged")
	assert.Contains(t, logs.String(), testImageURLLog)

	// Finalized with restorecon (SELinux enabled, non-interactive sudo), never
	// chowned or made writable (finalizeClonedDisk is not applied to a
	// prepared image).
	sudo := h.log("sudo")
	assert.Contains(t, sudo, "-n restorecon -- "+filepath.Join(h.images, imagePrepareStagingPrefix))
	assert.NotContains(t, sudo, "chown")
	assert.NotContains(t, sudo, "777")
	assert.Contains(t, h.log("virsh"), "pool-refresh --pool default")

	// Create can consume it: the artifact passes the #334 confinement.
	img, err := imagePathPolicy{dirs: []string{h.images}}.confine(context.Background(), h.vp, imagePathRequest{Path: artifact})
	require.NoError(t, err)
	assert.Equal(t, artifact, img.Path)
	assert.False(t, img.AdoptInPlace, "a prepared image is always copied, never attached in place")
}

// fixture returns the content of FAKE_HOST_DIR/name ("" when absent).
func (h *prepareHost) fixture(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.root, name)) //nolint:gosec // test reads its own fixture
	return string(b)
}
