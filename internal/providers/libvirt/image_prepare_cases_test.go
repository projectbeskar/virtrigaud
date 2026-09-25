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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// requireCode asserts err, as returned on the wire, carries code.
func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, status.Code(err), "unexpected code for %v", err)
}

// rpcErr maps an internal prepare error to its wire form (as ImagePrepare does).
func rpcErr(err error) error {
	if err == nil {
		return nil
	}
	return imagePrepareRPCError(err)
}

// TestImagePrepareIdentity_ReuseOnMatchingStamp proves a second prepare of the
// same image reuses the complete artifact without downloading again, and echoes
// the stamp found on the host.
func TestImagePrepareIdentity_ReuseOnMatchingStamp(t *testing.T) {
	h := newPrepareHost(t)
	req := identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL))

	first, err := h.s.ImagePrepare(context.Background(), req)
	require.NoError(t, err)
	second, err := h.s.ImagePrepare(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, first.GetPreparedImagePath(), second.GetPreparedImagePath())
	assert.Equal(t, first.GetPreparedImageId(), second.GetPreparedImageId())
	assert.True(t, second.GetArtifact().GetReused())
	assert.Equal(t, first.GetArtifact().GetImage().GetUid(), second.GetArtifact().GetImage().GetUid())
	assert.Equal(t, first.GetArtifact().GetSourceDigest(), second.GetArtifact().GetSourceDigest())
	assert.Equal(t, 1, strings.Count(h.log("curl"), "\n"), "the second prepare does not download")
	assertNoStagingFiles(t, h.images)

	// Another image, or another source of the same image, gets its own artifact.
	other, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testOtherImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)
	assert.NotEqual(t, first.GetPreparedImagePath(), other.GetPreparedImagePath())
	assert.False(t, other.GetArtifact().GetReused())
}

// TestImagePrepareIdentity_ConflictIsFailClosed covers ADR-0009 D4's
// Conflict row: anything at the derived name that does not carry a trusted
// stamp for this image and digest — and prove the very file at the artifact
// name — is refused with AlreadyExists, never downloaded over, deleted,
// re-stamped or adopted. The message names only the requester's artifact.
func TestImagePrepareIdentity_ConflictIsFailClosed(t *testing.T) {
	name := func(t *testing.T) string { return artifactNameFor(t, testImageUID, testDigest) }
	old := time.Now().Add(-10 * time.Hour)
	cases := map[string]func(t *testing.T, h *prepareHost, artifact, sidecar string){
		"foreign stamp": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "foreign")
			plantSidecar(t, sidecar, mustSidecar(t, testOtherImageUID, testDigest, ino, size), time.Now())
		},
		"other digest": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "foreign")
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testOtherDigest, ino, size), time.Now())
		},
		"artifact without a sidecar": func(t *testing.T, _ *prepareHost, artifact, _ string) {
			plantFile(t, artifact, "legacy or planted")
		},
		"orphaned foreign sidecar": func(t *testing.T, _ *prepareHost, _, sidecar string) {
			plantSidecar(t, sidecar, mustSidecar(t, testOtherImageUID, testDigest, 7, 9), old)
		},
		"orphaned untrusted sidecar": func(t *testing.T, _ *prepareHost, _, sidecar string) {
			plantSidecar(t, sidecar, []byte(`{"stampVersion":1}`), old)
		},
		"untrusted sidecar with artifact": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "x")
			doc := strings.Replace(string(mustSidecar(t, testImageUID, testDigest, ino, size)), `"stampVersion":1`, `"stampVersion":1,"stampVersion":1`, 1)
			plantSidecar(t, sidecar, []byte(doc), time.Now())
		},
		"inode mismatch": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "swapped")
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino+1, size), time.Now())
		},
		"size mismatch": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "swapped")
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino, size+1), time.Now())
		},
		"artifact is a symlink": func(t *testing.T, h *prepareHost, artifact, sidecar string) {
			target := filepath.Join(h.images, "real.qcow2")
			ino, size := plantFile(t, target, "real")
			require.NoError(t, os.Symlink(target, artifact))
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino, size), time.Now())
		},
		"sidecar is a directory": func(t *testing.T, _ *prepareHost, artifact, sidecar string) {
			plantFile(t, artifact, "x")
			require.NoError(t, os.Mkdir(sidecar, 0o750))
		},
	}
	for caseName, plant := range cases {
		t.Run(caseName, func(t *testing.T) {
			h := newPrepareHost(t)
			logs := captureLog(t)
			artifact := filepath.Join(h.images, name(t)+qcow2Ext)
			sidecar := filepath.Join(h.images, imageSidecarName(name(t)))
			plant(t, h, artifact, sidecar)
			before := snapshotDir(t, h.images)

			_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
			requireCode(t, err, codes.AlreadyExists)
			assert.Contains(t, err.Error(), name(t))
			assert.Contains(t, err.Error(), "was not prepared for this VMImage")
			assert.NotContains(t, err.Error(), testOtherImageUID, "the recorded owner goes to the provider log only")
			assert.Empty(t, h.log("curl"), "a Conflict never downloads")
			assert.Equal(t, before, snapshotDir(t, h.images), "a Conflict never touches the pool")
			assert.Contains(t, logs.String(), "refusing prepared-image artifact")
		})
	}
}

// snapshotDir returns name -> "<mode>:<inode>:<content>" of dir's entries.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		ino, _, mode := fileIdentity(t, p)
		content := ""
		if e.Type().IsRegular() {
			b, err := os.ReadFile(p) //nolint:gosec // test reads its own fixture
			if err == nil {
				content = string(b)
			}
		}
		out[e.Name()] = mode.String() + ":" + strconv.FormatUint(ino, 10) + ":" + content
	}
	return out
}

// TestImagePrepareIdentity_InProgressAndAbandoned covers a matching stamp
// without its artifact (a publish between its two links, or a crashed one):
// in progress (retryable Unavailable, nothing touched) while younger than the
// staleness bound max(2 x spec.prepare.timeout, 2h), abandoned after it — the
// stale stamp and stale staging files are removed and the image is imported
// again; fresh staging files of other prepares are kept.
func TestImagePrepareIdentity_InProgressAndAbandoned(t *testing.T) {
	name := artifactNameFor(t, testImageUID, testDigest)

	t.Run("in progress", func(t *testing.T) {
		h := newPrepareHost(t)
		sidecar := filepath.Join(h.images, imageSidecarName(name))
		plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, 7, 9), time.Now().Add(-90*time.Minute))
		before := snapshotDir(t, h.images)

		_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
		requireCode(t, err, codes.Unavailable)
		assert.Contains(t, err.Error(), "still being prepared")
		assert.Empty(t, h.log("curl"))
		assert.Equal(t, before, snapshotDir(t, h.images))
	})

	t.Run("a longer prepare.timeout extends the bound", func(t *testing.T) {
		h := newPrepareHost(t)
		sidecar := filepath.Join(h.images, imageSidecarName(name))
		plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, 7, 9), time.Now().Add(-3*time.Hour))
		js := `{"source":{"libvirt":{"url":"` + testImageURL + `"}},"prepare":{"timeout":"2h"}}`

		_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, js))
		requireCode(t, err, codes.Unavailable)
		assert.FileExists(t, sidecar)
	})

	t.Run("abandoned", func(t *testing.T) {
		h := newPrepareHost(t)
		sidecar := filepath.Join(h.images, imageSidecarName(name))
		stale := time.Now().Add(-3 * time.Hour)
		plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, 7, 9), stale)
		staleStaging := filepath.Join(h.images, imagePrepareStagingPrefix+"OLDOLDOLD0.download")
		legacyStaging := filepath.Join(h.images, imagePrepareStagingPrefix+"ubuntu.download")
		freshStaging := filepath.Join(h.images, imagePrepareStagingPrefix+"FRESHFRESH.partial")
		for _, p := range []string{staleStaging, legacyStaging} {
			plantFile(t, p, "left by a crashed prepare")
			require.NoError(t, os.Chtimes(p, stale, stale))
		}
		plantFile(t, freshStaging, "another prepare, still writing")
		unrelated := filepath.Join(h.images, "old-unrelated.qcow2")
		plantFile(t, unrelated, "not ours")
		require.NoError(t, os.Chtimes(unrelated, stale, stale))

		resp, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
		require.NoError(t, err)
		assert.False(t, resp.GetArtifact().GetReused())

		ino, size, _ := fileIdentity(t, resp.GetPreparedImagePath())
		raw, err := os.ReadFile(sidecar) //nolint:gosec // test reads its own fixture
		require.NoError(t, err)
		doc, err := parseImageSidecar(raw)
		require.NoError(t, err)
		assert.Equal(t, imageSidecarArtifact{Inode: ino, Size: size}, doc.Artifact, "a fresh stamp replaced the abandoned one")

		assert.NoFileExists(t, staleStaging, "stale staging files are swept")
		assert.NoFileExists(t, legacyStaging, "the pre-ADR shared download name is swept when stale")
		assert.FileExists(t, freshStaging, "a live staging file is never swept")
		assert.FileExists(t, unrelated, "only VirtRigaud staging files are swept")
		assert.Equal(t, []string{filepath.Base(freshStaging)}, stagingEntries(t, h.images))
	})
}

// TestImagePrepareIdentity_ArtifactEEXISTWithdrawsOurSidecar covers ADR-0009
// D6: when the artifact link finds the name taken after this prepare published
// its sidecar, the prepare unlinks ITS OWN sidecar first (so it never stamps a
// file it did not publish), leaves the other file alone and returns Conflict.
func TestImagePrepareIdentity_ArtifactEEXISTWithdrawsOurSidecar(t *testing.T) {
	h := newPrepareHost(t)
	name := artifactNameFor(t, testImageUID, testDigest)
	artifact := filepath.Join(h.images, name+qcow2Ext)
	sidecar := filepath.Join(h.images, imageSidecarName(name))

	host := &hookedHost{VirshProvider: h.vp}
	host.before = func(args []string) *hookAnswer {
		if _, dst, ok := linkCall(args); ok && dst == artifact {
			require.FileExists(t, sidecar, "the sidecar is published before the artifact")
			plantFile(t, artifact, "raced in")
		}
		return nil
	}
	req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)

	_, err = h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
	requireCode(t, rpcErr(err), codes.AlreadyExists)
	assert.NoFileExists(t, sidecar, "our sidecar is withdrawn")
	content, rerr := os.ReadFile(artifact) //nolint:gosec // test reads its own fixture
	require.NoError(t, rerr)
	assert.Equal(t, "raced in", string(content), "the other file is left untouched")
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepareIdentity_SidecarEEXISTReprobes covers ADR-0009 D6: a prepare
// whose sidecar link finds the name taken never links the artifact; it re-runs
// the D4 probe and reuses, waits (in progress) or refuses (Conflict), and
// removes its own staging files either way.
func TestImagePrepareIdentity_SidecarEEXISTReprobes(t *testing.T) {
	name := artifactNameFor(t, testImageUID, testDigest)
	cases := map[string]struct {
		plant    func(t *testing.T, artifact, sidecar string)
		wantCode codes.Code
		reused   bool
	}{
		"matching stamp, artifact not linked yet": {
			plant: func(t *testing.T, _, sidecar string) {
				plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, 7, 9), time.Now())
			},
			wantCode: codes.Unavailable,
		},
		"matching stamp and artifact": {
			plant: func(t *testing.T, artifact, sidecar string) {
				ino, size := plantFile(t, artifact, "published by the other prepare")
				plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino, size), time.Now())
			},
			wantCode: codes.OK,
			reused:   true,
		},
		"foreign stamp": {
			plant: func(t *testing.T, _, sidecar string) {
				plantSidecar(t, sidecar, mustSidecar(t, testOtherImageUID, testDigest, 7, 9), time.Now())
			},
			wantCode: codes.AlreadyExists,
		},
	}
	for caseName, tc := range cases {
		t.Run(caseName, func(t *testing.T) {
			h := newPrepareHost(t)
			artifact := filepath.Join(h.images, name+qcow2Ext)
			sidecar := filepath.Join(h.images, imageSidecarName(name))
			var artifactLinks int
			host := &hookedHost{VirshProvider: h.vp}
			host.before = func(args []string) *hookAnswer {
				_, dst, ok := linkCall(args)
				switch {
				case ok && dst == sidecar:
					tc.plant(t, artifact, sidecar)
				case ok && dst == artifact:
					artifactLinks++
				}
				return nil
			}
			req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
			require.NoError(t, err)

			res, err := h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				assert.Equal(t, tc.reused, res.Reused)
				assert.Equal(t, artifact, res.Path)
			} else {
				requireCode(t, rpcErr(err), tc.wantCode)
			}
			assert.Zero(t, artifactLinks, "only the sidecar's creator links the artifact")
			assert.FileExists(t, sidecar, "the other prepare's sidecar is left alone")
			assertNoStagingFiles(t, h.images)
		})
	}
}

// TestImagePrepareIdentity_ConcurrentPrepares races several prepares of the
// same image on one host: exactly one creates the sidecar and links the
// artifact; the others reuse it or report it in progress; the result is one
// consistent artifact and no leftovers.
func TestImagePrepareIdentity_ConcurrentPrepares(t *testing.T) {
	h := newPrepareHost(t)
	name := artifactNameFor(t, testImageUID, testDigest)
	artifact := filepath.Join(h.images, name+qcow2Ext)
	var mu sync.Mutex
	artifactLinks, staged := 0, map[string]bool{}
	host := &hookedHost{VirshProvider: h.vp}
	host.before = func(args []string) *hookAnswer {
		mu.Lock()
		defer mu.Unlock()
		if src, dst, ok := linkCall(args); ok && dst == artifact {
			artifactLinks++
			staged[src] = true
		}
		return nil
	}
	req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)

	const n = 6
	var wg sync.WaitGroup
	results := make([]imagePrepareResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
		}(i)
	}
	wg.Wait()

	created := 0
	for i := range results {
		if errs[i] != nil {
			assert.Equal(t, codes.Unavailable, status.Code(rpcErr(errs[i])), "a losing prepare is in progress, not %v", errs[i])
			continue
		}
		assert.Equal(t, artifact, results[i].Path)
		if !results[i].Reused {
			created++
		}
	}
	assert.Equal(t, 1, created, "exactly one prepare publishes the artifact")
	assert.Equal(t, 1, artifactLinks, "only the sidecar's creator links the artifact")

	ino, size, mode := fileIdentity(t, artifact)
	assert.Equal(t, os.FileMode(0o444), mode)
	raw, err := os.ReadFile(filepath.Join(h.images, imageSidecarName(name))) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	doc, err := parseImageSidecar(raw)
	require.NoError(t, err)
	assert.Equal(t, imageSidecarArtifact{Inode: ino, Size: size}, doc.Artifact)
	assertNoStagingFiles(t, h.images)

	// Every prepare staged privately: the downloads had distinct names.
	var downloads []string
	for _, line := range strings.Split(strings.TrimSpace(h.log("curl")), "\n") {
		f := strings.Fields(line)
		downloads = append(downloads, f[len(f)-1])
	}
	seen := map[string]bool{}
	for _, d := range downloads {
		assert.False(t, seen[d], "download %s was shared", d)
		seen[d] = true
		assert.True(t, strings.HasPrefix(filepath.Base(d), imagePrepareStagingPrefix) && strings.HasSuffix(d, downloadStagingSuffix), d)
	}
}

// TestImagePrepareIdentity_SingleInputRule covers ADR-0009 D1: in identity
// mode a source with both path and url, or only a path, is InvalidSpec before
// any host command runs (converting the path would mint a stamped copy of any
// file in the allowed image directories).
func TestImagePrepareIdentity_SingleInputRule(t *testing.T) {
	h := newPrepareHost(t)
	allowed := h.file(h.images, "allowed.qcow2")
	for name, js := range map[string]string{
		"path and url":   `{"source":{"libvirt":{"path":"` + allowed + `","url":"https://x.example/y.qcow2"}}}`,
		"path only":      `{"source":{"libvirt":{"path":"` + allowed + `"}}}`,
		"flat path+url":  `{"Path":"` + allowed + `","URL":"https://x.example/y.qcow2"}`,
		"no source":      `{"source":{"libvirt":{}}}`,
		"file:// url":    urlImageJSON("file:///etc/shadow"),
		"no host":        urlImageJSON("https:///x.qcow2"),
		"control in url": `{"source":{"libvirt":{"url":"https://x.example/a\u0000b"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			host := &hookedHost{VirshProvider: h.vp}
			req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, js))
			require.NoError(t, err)
			_, err = h.preparer(host).prepare(context.Background(), req, js, "", h.policy)
			requireCode(t, rpcErr(err), codes.InvalidArgument)
			assert.Empty(t, host.hostCalls(), "refused before any host command")
		})
	}
	resp, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest,
		`{"source":{"libvirt":{"path":"`+allowed+`","url":"https://x.example/y.qcow2"}}}`))
	requireCode(t, err, codes.InvalidArgument)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "exactly one input")
}

// TestImagePrepare_FailureClassification pins which synchronous failures are
// permanent (InvalidArgument: the manager records them on the VMImage and
// holds) and which stay retryable (the manager retries). No error text ever
// carries the source URL.
func TestImagePrepare_FailureClassification(t *testing.T) {
	payload := "payload\n"
	sum := sha256.Sum256([]byte(payload))
	goodSum := hex.EncodeToString(sum[:])
	withChecksum := func(sum string) string {
		return `{"source":{"libvirt":{"url":"` + testImageURL + `","checksum":"` + sum + `","checksumType":"sha256"}}}`
	}

	cases := map[string]struct {
		setup     func(t *testing.T, h *prepareHost)
		imageJSON string
		hook      func(args []string) *hookAnswer
		permanent bool
	}{
		"HTTP 404":        {setup: failDownload("404 22"), permanent: true},
		"HTTP 403":        {setup: failDownload("403 22"), permanent: true},
		"HTTP 410":        {setup: failDownload("410 22"), permanent: true},
		"HTTP 503":        {setup: failDownload("503 22")},
		"HTTP 429":        {setup: failDownload("429 22")},
		"HTTP 408":        {setup: failDownload("408 22")},
		"connect refused": {setup: failDownload("000 7")},
		"DNS failure":     {setup: failDownload("000 6")},
		"timeout":         {setup: failDownload("000 28")},
		"TLS unverified":  {setup: failDownload("000 60"), permanent: true},
		"remote missing":  {setup: failDownload("000 78"), permanent: true},
		"checksum mismatch": {
			imageJSON: withChecksum(strings.Repeat("0", 64)), permanent: true,
		},
		"checksum ok": {imageJSON: withChecksum(goodSum)},
		"unsupported checksum type": {
			imageJSON: `{"source":{"libvirt":{"url":"` + testImageURL + `","checksum":"ab","checksumType":"crc32"}}}`, permanent: true,
		},
		"unreadable image": {
			setup: func(t *testing.T, h *prepareHost) { plantFile(t, filepath.Join(h.root, "info.fail"), "") }, permanent: true,
		},
		"unsupported image format": {
			setup: func(t *testing.T, h *prepareHost) {
				plantFile(t, filepath.Join(h.root, "download.info.json"), `{"format":"qed"}`)
			}, permanent: true,
		},
		"image with a backing file": {
			setup: func(t *testing.T, h *prepareHost) {
				plantFile(t, filepath.Join(h.root, "download.info.json"), `{"format":"qcow2","backing-filename":"/etc/shadow"}`)
			}, permanent: true,
		},
		"pool does not exist": {
			setup: func(t *testing.T, h *prepareHost) { plantFile(t, filepath.Join(h.root, "pool.missing"), "") }, permanent: true,
		},
		"pool outside the allowed image directories": {
			setup: func(t *testing.T, h *prepareHost) {
				require.NoError(t, os.WriteFile(filepath.Join(h.root, "h1", "pooldir"), []byte(h.outside), 0o600))
			}, permanent: true,
		},
		"pool read fails in transport": {hook: failWhen(func(a []string) bool { return len(a) > 0 && a[0] == "pool-dumpxml" })},
		"probe fails in transport":     {hook: failWhen(func(a []string) bool { return scriptCall(a, imageArtifactProbeScript) })},
		"download fails in transport":  {hook: failWhen(func(a []string) bool { return len(a) > 1 && a[1] == "curl" })},
		"convert fails": {
			setup: func(t *testing.T, h *prepareHost) { plantFile(t, filepath.Join(h.root, "convert.fail"), "") },
		},
		"link fails in transport": {hook: failWhen(func(a []string) bool { _, _, ok := linkCall(a); return ok })},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPrepareHost(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			js := tc.imageJSON
			if js == "" {
				js = urlImageJSON(testImageURL)
			}
			host := &hookedHost{VirshProvider: h.vp, before: tc.hook}
			req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, js))
			require.NoError(t, err)

			_, err = h.preparer(host).prepare(context.Background(), req, js, "", h.policy)
			if name == "checksum ok" {
				require.NoError(t, err)
				return
			}
			wire := rpcErr(err)
			require.Error(t, wire)
			if tc.permanent {
				assert.Equal(t, codes.InvalidArgument, status.Code(wire), "permanent: %v", wire)
			} else {
				assert.NotEqual(t, codes.InvalidArgument, status.Code(wire), "transient must stay retryable: %v", wire)
				assert.NotEqual(t, codes.AlreadyExists, status.Code(wire), "transient must stay retryable: %v", wire)
			}
			assert.NotContains(t, wire.Error(), "images.example.com", "the URL is never echoed")
			assert.NotContains(t, wire.Error(), "supersecret")
			assertNoStagingFiles(t, h.images)
			name := artifactNameFor(t, testImageUID, testDigest)
			assert.NoFileExists(t, filepath.Join(h.images, name+qcow2Ext), "a failed prepare publishes nothing")
			assert.NoFileExists(t, filepath.Join(h.images, imageSidecarName(name)), "a failed prepare leaves no stamp")
		})
	}
}

// failDownload makes the fake curl fail with "<http code> <exit code>".
func failDownload(spec string) func(t *testing.T, h *prepareHost) {
	return func(t *testing.T, h *prepareHost) {
		require.NoError(t, os.WriteFile(filepath.Join(h.root, "download.fail"), []byte(spec+"\n"), 0o600))
	}
}

// failWhen fails, as a broken SSH transport would, every host command match
// selects.
func failWhen(match func(args []string) bool) func(args []string) *hookAnswer {
	return func(args []string) *hookAnswer {
		if match(args) {
			return transportFailure(args)
		}
		return nil
	}
}

// TestImagePrepareIdentity_NoHardLinks proves a pool filesystem that cannot
// hold hard links fails the prepare with an explicit, permanent error.
func TestImagePrepareIdentity_NoHardLinks(t *testing.T) {
	h := newPrepareHost(t)
	host := &hookedHost{VirshProvider: h.vp}
	host.before = func(args []string) *hookAnswer {
		if _, _, ok := linkCall(args); ok {
			return &hookAnswer{res: &VirshResult{Stdout: "unsupported\n", Stderr: "ln: Operation not permitted"}}
		}
		return nil
	}
	req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)
	_, err = h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
	requireCode(t, rpcErr(err), codes.InvalidArgument)
	assert.Contains(t, err.Error(), "without hard links")
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepareLegacy_DeprecatedPath covers deprecated legacy mode (a
// manager older than ADR-0009): the bare-name artifact <pool>/<target>.qcow2,
// reused by name, no artifact echo, the deprecation signal on every request,
// and the ADR-0009 provider-internal fixes (read-only, never chowned, private
// staging, published with ln). It never sees an identity-mode artifact.
func TestImagePrepareLegacy_DeprecatedPath(t *testing.T) {
	h := newPrepareHost(t)
	logs := captureLog(t)
	before := libvirtLegacyCount(t)
	req := &providerv1.ImagePrepareRequest{ImageJson: urlImageJSON(testImageURL), TargetName: "ubuntu-22.04"}

	resp, err := h.s.ImagePrepare(context.Background(), req)
	require.NoError(t, err)
	artifact := filepath.Join(h.images, "ubuntu-22.04.qcow2")
	assert.Equal(t, "ubuntu-22.04", resp.GetPreparedImageId())
	assert.Equal(t, artifact, resp.GetPreparedImagePath())
	assert.Nil(t, resp.GetArtifact(), "legacy mode echoes no artifact")
	_, _, mode := fileIdentity(t, artifact)
	assert.Equal(t, os.FileMode(0o444), mode)
	assert.NotContains(t, h.log("sudo"), "chown")
	assert.NotContains(t, h.log("sudo"), "777")
	assert.NoFileExists(t, filepath.Join(h.images, imageSidecarName("ubuntu-22.04")), "legacy mode writes no stamp")
	assertNoStagingFiles(t, h.images)
	assert.Equal(t, before+1, libvirtLegacyCount(t))
	assert.Contains(t, logs.String(), imageartifact.LegacyRequestWarning)
	assert.Contains(t, logs.String(), "provider_type=libvirt")

	// Reused by name, still signalled.
	again, err := h.s.ImagePrepare(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, artifact, again.GetPreparedImagePath())
	assert.Equal(t, 1, strings.Count(h.log("curl"), "\n"))
	assert.Equal(t, before+2, libvirtLegacyCount(t))

	// An identity prepare of the same VMImage gets its own artifact, and a
	// legacy request never reaches it.
	id, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)
	assert.NotEqual(t, artifact, id.GetPreparedImagePath())
	assert.False(t, id.GetArtifact().GetReused(), "a bare-name artifact is never adopted")
	assert.Equal(t, before+2, libvirtLegacyCount(t), "identity requests are not counted as legacy")
}

// TestImagePrepareLegacy_ProbeErrorIsRetryable proves legacy mode keeps the
// ADR-0009 D4 fix: a failed probe of the final name is retryable and is never
// taken as "absent" (no download, convert or rm follows).
func TestImagePrepareLegacy_ProbeErrorIsRetryable(t *testing.T) {
	h := newPrepareHost(t)
	artifact := filepath.Join(h.images, "ubuntu.qcow2")
	plantFile(t, artifact, "prepared earlier")
	host := &hookedHost{VirshProvider: h.vp, before: failWhen(func(a []string) bool { return scriptCall(a, pathPresentScript) })}

	_, err := h.preparer(host).prepare(context.Background(),
		imageartifact.Request{Mode: imageartifact.ModeLegacy, LegacyTargetName: "ubuntu"}, urlImageJSON(testImageURL), "", h.policy)
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "a probe error is retryable: %v", err)
	assert.Empty(t, h.log("curl"))
	assert.NotContains(t, h.log("qemu-img"), "convert")
	for _, call := range host.hostCalls() {
		if scriptCall(call, pathPresentScript) {
			continue // the failed probe itself
		}
		assert.NotContains(t, call, artifact, "nothing runs on the final name after a failed probe: %v", call)
	}
	content, rerr := os.ReadFile(artifact) //nolint:gosec // test reads its own fixture
	require.NoError(t, rerr)
	assert.Equal(t, "prepared earlier", string(content))
}

// TestImagePrepareLegacy_ConcurrentPublishReusesByName proves a legacy
// prepare whose link finds the bare name taken (a concurrent legacy prepare)
// reuses it by name, as the pre-ADR provider did, and never overwrites it.
func TestImagePrepareLegacy_ConcurrentPublishReusesByName(t *testing.T) {
	h := newPrepareHost(t)
	artifact := filepath.Join(h.images, "ubuntu.qcow2")
	host := &hookedHost{VirshProvider: h.vp}
	host.before = func(args []string) *hookAnswer {
		if _, dst, ok := linkCall(args); ok && dst == artifact {
			plantFile(t, artifact, "published concurrently")
		}
		return nil
	}
	res, err := h.preparer(host).prepare(context.Background(),
		imageartifact.Request{Mode: imageartifact.ModeLegacy, LegacyTargetName: "ubuntu"}, urlImageJSON(testImageURL), "", h.policy)
	require.NoError(t, err)
	assert.Equal(t, artifact, res.Path)
	assert.Nil(t, res.Stamp)
	content, rerr := os.ReadFile(artifact) //nolint:gosec // test reads its own fixture
	require.NoError(t, rerr)
	assert.Equal(t, "published concurrently", string(content), "never overwritten")
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepare_MalformedRequests pins the request-level rejections at the
// RPC boundary (imageartifact.ParseRequest): InvalidArgument, no host work.
func TestImagePrepare_MalformedRequests(t *testing.T) {
	h := newPrepareHost(t)
	for name, req := range map[string]*providerv1.ImagePrepareRequest{
		"neither identity nor target": {ImageJson: urlImageJSON(testImageURL)},
		"identity with a target name": func() *providerv1.ImagePrepareRequest {
			r := identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL))
			r.TargetName = "ubuntu"
			return r
		}(),
		"malformed digest":         identityProtoReq(testImageUID, "sha256:abc", urlImageJSON(testImageURL)),
		"digest without identity":  {ImageJson: urlImageJSON(testImageURL), TargetName: "x", SourceDigest: testDigest},
		"legacy name not DNS-1123": {ImageJson: urlImageJSON(testImageURL), TargetName: "../etc/x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.s.ImagePrepare(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
		})
	}
	assert.Empty(t, h.log("curl"))
	assert.Empty(t, h.log("virsh"))
}

// TestServer_AdvertisesImageArtifactIdentity: a single-host provider
// advertises the ADR-0009 capability; a clustered one hides it together with
// image import (ImagePrepare stays Unimplemented there).
func TestServer_AdvertisesImageArtifactIdentity(t *testing.T) {
	caps, err := (&Server{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImageImport())
	assert.True(t, caps.GetSupportsImageArtifactIdentity())

	clustered := clusteredCapabilities()
	assert.False(t, clustered.GetSupportsImageImport())
	assert.False(t, clustered.GetSupportsImageArtifactIdentity())
}

// TestImagePrepareStaleness pins ADR-0009 D4's bound,
// max(2 x spec.prepare.timeout, 2h).
func TestImagePrepareStaleness(t *testing.T) {
	for js, want := range map[string]time.Duration{
		"":                                   2 * time.Hour,
		`{"source":{}}`:                      2 * time.Hour,
		`{"prepare":{"timeout":"30m"}}`:      2 * time.Hour,
		`{"prepare":{"timeout":"90m"}}`:      3 * time.Hour,
		`{"prepare":{"timeout":"-5m"}}`:      2 * time.Hour,
		`{"prepare":{"timeout":"bogus"}}`:    2 * time.Hour,
		`{"prepare":{"timeout":"10000h"}}`:   2 * maxImagePrepareTimeout,
		`{"prepare":{"timeout":"2562047h"}}`: 2 * maxImagePrepareTimeout,
	} {
		assert.Equal(t, want, imagePrepareStaleness(js), "spec %s", js)
	}
}

// TestRedactURL proves the log form of a source URL drops user-info, query and
// fragment.
func TestRedactURL(t *testing.T) {
	assert.Equal(t, "https://h.example/p/x.qcow2?…", redactURL("https://user:pw@h.example/p/x.qcow2?sig=secret#frag"))
	assert.Equal(t, "ftp://h.example/x.img", redactURL("ftp://h.example/x.img"))
	assert.Equal(t, "<unparseable URL>", redactURL("http://[::1"))
}

// TestCurlURLConfig proves the URL is quoted for curl's config syntax and URL
// globbing is off.
func TestCurlURLConfig(t *testing.T) {
	assert.Equal(t, "globoff\nurl = \"https://h/a\\\"b\\\\c\"\n", string(curlURLConfig(`https://h/a"b\c`)))
}

// TestMakeHostTempSuffix proves the suffix form of the staging helper creates
// a private, exclusive file whose name ends in the suffix, and refuses a bad
// template or suffix.
func TestMakeHostTempSuffix(t *testing.T) {
	h := newPrepareHost(t)
	template := filepath.Join(h.images, imagePrepareStagingPrefix+mktempTemplateSuffix)
	a, err := makeHostTempSuffix(context.Background(), h.vp, template, convertStagingSuffix, false)
	require.NoError(t, err)
	b, err := makeHostTempSuffix(context.Background(), h.vp, template, convertStagingSuffix, false)
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	for _, p := range []string{a, b} {
		assert.True(t, strings.HasSuffix(p, convertStagingSuffix), p)
		assert.True(t, reservedImageName(filepath.Base(p)), "a staging file is never a usable base image")
		_, _, mode := fileIdentity(t, p)
		assert.Equal(t, os.FileMode(0o600), mode)
	}
	for _, bad := range []string{"/x", "a/b", "x\n"} {
		_, err := makeHostTempSuffix(context.Background(), h.vp, template, bad, false)
		assert.Error(t, err, "suffix %q", bad)
	}
	_, err = makeHostTempSuffix(context.Background(), h.vp, "relative-XXXXXXXXXX", "", false)
	assert.Error(t, err)
}

// TestImageArtifactProbeScriptReadsOneByteMore keeps the probe's read bound in
// step with the sidecar size limit (an oversized sidecar must be seen as such).
func TestImageArtifactProbeScriptReadsOneByteMore(t *testing.T) {
	assert.Contains(t, imageArtifactProbeScript, "head -c "+strconv.Itoa(maxImageSidecarBytes+1)+" --")
}

// TestParseArtifactProbe covers the probe parser's fail-closed edges.
func TestParseArtifactProbe(t *testing.T) {
	_, err := parseArtifactProbe("1700000000\nabsent\n")
	assert.Error(t, err, "truncated output is an error, never absent")
	_, err = parseArtifactProbe("x\nabsent\nabsent\n")
	assert.Error(t, err)
	_, err = parseArtifactProbe("1\nregular file|a|1|1|444|1\nabsent\n")
	assert.Error(t, err)
	_, err = parseArtifactProbe("1\nregular file|1|1|1|444\nabsent\n")
	assert.Error(t, err, "the owner flag is required")
	_, err = parseArtifactProbe("1\nregular file|1|1|1|9z9|1\nabsent\n")
	assert.Error(t, err)
	_, err = parseArtifactProbe("1\nregular file|1|1|1|444|x\nabsent\n")
	assert.Error(t, err)
	_, err = parseArtifactProbe("1\nabsent\nregular file|1|2|3|444|1\n")
	assert.Error(t, err, "a regular sidecar without its content marker is an error")

	pr, err := parseArtifactProbe("100\nabsent\nabsent\n")
	require.NoError(t, err)
	assert.False(t, pr.observation(time.Hour).Exists)

	pr, err = parseArtifactProbe("100\nabsent\nregular file|5|10|40|444|1\nreadable\nnot json")
	require.NoError(t, err)
	obs := pr.observation(time.Hour)
	assert.True(t, obs.Exists)
	assert.Nil(t, obs.Stamp, "an untrusted sidecar carries no stamp")
	assert.Error(t, pr.docErr)
}

// TestImagePrepare_OverTheWire runs the real *Provider behind the real gRPC
// Server and the MANAGER's transport client: the identity echo confirms the
// request (contracts.ImagePrepareResponse.ConfirmsIdentity), a Conflict comes
// back typed and non-retryable, a permanent source failure as InvalidSpec (so
// the manager records it on the VMImage and holds), and an in-progress
// artifact as retryable.
func TestImagePrepare_OverTheWire(t *testing.T) {
	h := newPrepareHost(t)
	c := startLibvirtGRPC(t, h.p)
	req := contracts.ImagePrepareRequest{
		ImageJSON:    urlImageJSON(testImageURL),
		Image:        contracts.ObjectIdentity{UID: testImageUID, Namespace: "team-a", Name: "ubuntu-22.04"},
		SourceDigest: testDigest,
		Provider:     contracts.ObjectIdentity{UID: testProviderUID, Namespace: "team-a", Name: "libvirt"},
	}

	resp, err := c.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, resp.ConfirmsIdentity(req), "the echo confirms the request: %+v", resp.Artifact)
	assert.Empty(t, resp.TaskRef)
	assert.False(t, resp.Artifact.Reused)

	// Another image whose derived name is occupied by a foreign artifact.
	other := req
	other.Image.UID = testOtherImageUID
	name := artifactNameFor(t, testOtherImageUID, testDigest)
	plantFile(t, filepath.Join(h.images, name+qcow2Ext), "planted")
	_, err = c.PrepareImage(context.Background(), other)
	assert.True(t, contracts.IsConflict(err), "got %v", err)

	// A source that answers 404: InvalidSpec, never retried hot.
	failDownload("404 22")(t, h)
	missing := req
	missing.SourceDigest = testOtherDigest
	_, err = c.PrepareImage(context.Background(), missing)
	assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
	assert.NotContains(t, err.Error(), "supersecret")

	// A matching stamp whose artifact is not linked yet: retryable.
	inflight := req
	inflight.Image.UID = "11111111-2222-4333-8444-555555555555"
	inflightName, nerr := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt, inflight.Image, testDigest)
	require.NoError(t, nerr)
	doc := testSidecar(testImageUID, testDigest, 7, 9)
	doc.Image.UID = inflight.Image.UID
	data, eerr := encodeImageSidecar(doc)
	require.NoError(t, eerr)
	plantSidecar(t, filepath.Join(h.images, imageSidecarName(inflightName)), data, time.Now())
	_, err = c.PrepareImage(context.Background(), inflight)
	assert.True(t, contracts.IsRetryable(err), "got %v", err)
}
