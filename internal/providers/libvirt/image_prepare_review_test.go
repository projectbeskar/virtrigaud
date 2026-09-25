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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// ---------------------------------------------------------------------------
// Security review of ADR-0009 Slice 4 (libvirt): regression tests.
// ---------------------------------------------------------------------------

// payloadSHA256 is the sha256 the harness's fake download hashes to.
func payloadSHA256() string {
	sum := sha256.Sum256([]byte("payload\n"))
	return hex.EncodeToString(sum[:])
}

// useRealCurl puts the machine's real curl in front of the harness's fake
// (skips when there is none).
func useRealCurl(t *testing.T, realCurl string) {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "curl"), //nolint:gosec // test fixture must be executable
		[]byte("#!/bin/sh\nexec "+realCurl+" \"$@\"\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// lookPathCurl returns the real curl, or skips.
func lookPathCurl(t *testing.T) string {
	t.Helper()
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	return realCurl
}

// TestImagePrepare_RealCurl_GlobURLIsOneRequest drives the REAL curl (config
// on stdin) against a local server with a URL full of curl glob syntax: the
// hypervisor host sends exactly ONE request for the literal URL — never the
// fan-out `[1-100]` / `{a,b}` would expand to — and a 404 is permanent.
func TestImagePrepare_RealCurl_GlobURLIsOneRequest(t *testing.T) {
	realCurl := lookPathCurl(t)
	var requests atomic.Int32
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		paths = append(paths, r.URL.RawPath+"|"+r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	h := newPrepareHost(t)
	useRealCurl(t, realCurl)
	url := srv.URL + "/img[1-100]{a,b,c}.qcow2"
	_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(url)))
	requireCode(t, err, codes.InvalidArgument)
	assert.NotContains(t, err.Error(), "404", "no HTTP status in tenant-visible text")
	assert.EqualValues(t, 1, requests.Load(), "exactly one request, never a glob fan-out: %v", paths)
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepare_RealCurl_SizeLimit proves --max-filesize refuses a source
// larger than the provider's download limit (permanent), with the limit from
// VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB.
func TestImagePrepare_RealCurl_SizeLimit(t *testing.T) {
	realCurl := lookPathCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2147483648") // 2 GiB announced
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)

	h := newPrepareHost(t)
	useRealCurl(t, realCurl)
	t.Setenv(EnvImageMaxDownloadGiB, "1")
	_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(srv.URL+"/big.qcow2")))
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "larger than this provider's download limit")
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepare_RealCurl_Success proves the real curl reads the URL from
// stdin and downloads the image (the rest of the pipeline is the harness's).
func TestImagePrepare_RealCurl_Success(t *testing.T) {
	realCurl := lookPathCurl(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("image-bytes"))
	}))
	t.Cleanup(srv.Close)

	h := newPrepareHost(t)
	useRealCurl(t, realCurl)
	resp, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(srv.URL+"/ok.qcow2?sig=secret")))
	require.NoError(t, err)
	assert.False(t, resp.GetArtifact().GetReused())
	assert.EqualValues(t, 1, requests.Load())
	assert.FileExists(t, resp.GetPreparedImagePath())
}

// TestImagePrepare_WriteOutMustBeOneTransfer proves the http-code parsing
// fails closed: a write-out that is not exactly one transfer is permanent,
// whether curl failed ("404404404", the fan-out a glob would cause — before,
// it parsed as no status and was retried forever) or succeeded.
func TestImagePrepare_WriteOutMustBeOneTransfer(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, h *prepareHost){
		"failed fan-out":    failDownload("404404404 22"),
		"succeeded fan-out": func(t *testing.T, h *prepareHost) { plantFile(t, filepath.Join(h.root, "download.writeout"), "200200") },
		"garbage":           failDownload("4x4 22"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newPrepareHost(t)
			setup(t, h)
			_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
			requireCode(t, err, codes.InvalidArgument)
			assert.Contains(t, err.Error(), "exactly one file")
			assertNoStagingFiles(t, h.images)
		})
	}
	for out, single := range map[string]bool{"": true, "200": true, "000": true, "404\n": true,
		"404404": false, "20": false, "abc": false, "-12": false} {
		_, got := parseCurlHTTPCode(out)
		assert.Equal(t, single, got, "write-out %q", out)
	}
}

// TestImagePrepare_TenantTextHasNoOracle proves tenant-visible errors carry
// neither a curl exit code, an HTTP status nor the computed checksum.
func TestImagePrepare_TenantTextHasNoOracle(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(t *testing.T, h *prepareHost)
		imageJSON string
		forbidden []string
	}{
		"HTTP 503":        {setup: failDownload("503 22"), forbidden: []string{"503", "22", "curl"}},
		"connect refused": {setup: failDownload("000 7"), forbidden: []string{"exit", "000", "curl"}},
		"HTTP 404":        {setup: failDownload("404 22"), forbidden: []string{"404", "22"}},
		"checksum mismatch": {
			imageJSON: `{"source":{"libvirt":{"url":"` + testImageURL + `","checksum":"` + strings.Repeat("0", 64) + `"}}}`,
			// sha256("payload\n"), what the host computed
			forbidden: []string{payloadSHA256()[:16], "got"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPrepareHost(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			js := tc.imageJSON
			if js == "" {
				js = urlImageJSON(testImageURL)
			}
			_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, js))
			require.Error(t, err)
			for _, f := range tc.forbidden {
				assert.NotContains(t, err.Error(), f)
			}
		})
	}
}

// TestImagePrepare_PoolLookupClassification proves only libvirt's "Storage
// pool not found" is permanent; virsh's generic "failed to get pool" (printed
// for every lookup failure, e.g. a libvirtd restart) stays retryable.
func TestImagePrepare_PoolLookupClassification(t *testing.T) {
	h := newPrepareHost(t)
	plantFile(t, filepath.Join(h.root, "pool.recvfail"), "")
	_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.Error(t, err)
	assert.NotEqual(t, codes.InvalidArgument, status.Code(err), "a lost connection is transient: %v", err)
	assert.Empty(t, h.log("curl"))

	require.NoError(t, os.Remove(filepath.Join(h.root, "pool.recvfail")))
	plantFile(t, filepath.Join(h.root, "pool.missing"), "")
	_, err = h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	requireCode(t, err, codes.InvalidArgument)
}

// TestImagePrepareIdentity_LinkNeverFollowsTheDestination covers `ln -T` and
// the symlink rule: a directory or a symlink (even one pointing at this
// prepare's own file) raced in at the artifact name is someone else's — a
// Conflict, with this prepare's sidecar withdrawn and the intruder untouched.
func TestImagePrepareIdentity_LinkNeverFollowsTheDestination(t *testing.T) {
	name := artifactNameFor(t, testImageUID, testDigest)
	for caseName, plant := range map[string]func(t *testing.T, staged, artifact string){
		"directory": func(t *testing.T, _, artifact string) { require.NoError(t, os.Mkdir(artifact, 0o750)) },
		"symlink to our file": func(t *testing.T, staged, artifact string) {
			require.NoError(t, os.Symlink(staged, artifact))
		},
	} {
		t.Run(caseName, func(t *testing.T) {
			h := newPrepareHost(t)
			artifact := filepath.Join(h.images, name+qcow2Ext)
			sidecar := filepath.Join(h.images, imageSidecarName(name))
			host := &hookedHost{VirshProvider: h.vp}
			host.before = func(args []string) *hookAnswer {
				if src, dst, ok := linkCall(args); ok && dst == artifact {
					plant(t, src, artifact)
				}
				return nil
			}
			req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
			require.NoError(t, err)
			_, err = h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
			requireCode(t, rpcErr(err), codes.AlreadyExists)
			assert.NoFileExists(t, sidecar, "our sidecar is withdrawn")
			fi, lerr := os.Lstat(artifact)
			require.NoError(t, lerr, "the intruder is left in place")
			if fi.IsDir() {
				entries, _ := os.ReadDir(artifact)
				assert.Empty(t, entries, "nothing is linked INTO a directory at the artifact name")
			}
		})
	}
}

// TestImagePrepareIdentity_LostLinkAnswerIsPublished proves a transport error
// on the artifact link, when the link did succeed on the host, is recognised
// (the artifact is our staged file): the sidecar is kept and the prepare
// succeeds instead of withdrawing a correct stamp.
func TestImagePrepareIdentity_LostLinkAnswerIsPublished(t *testing.T) {
	h := newPrepareHost(t)
	name := artifactNameFor(t, testImageUID, testDigest)
	artifact := filepath.Join(h.images, name+qcow2Ext)
	host := &hookedHost{VirshProvider: h.vp}
	host.before = func(args []string) *hookAnswer {
		if src, dst, ok := linkCall(args); ok && dst == artifact {
			require.NoError(t, os.Link(src, dst)) // the host did it...
			return transportFailure(args)         // ...but the answer was lost
		}
		return nil
	}
	req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)
	res, err := h.preparer(host).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
	require.NoError(t, err)
	assert.Equal(t, artifact, res.Path)
	assert.False(t, res.Reused)

	// The next prepare reuses it: the stamp was kept and matches.
	again, err := h.preparer(nil).prepare(context.Background(), req, urlImageJSON(testImageURL), "", h.policy)
	require.NoError(t, err)
	assert.True(t, again.Reused)
	assertNoStagingFiles(t, h.images)
}

// TestImagePrepareLegacy_SymlinkAtTheNameIsPresent proves legacy mode treats
// a (dangling) symlink at the bare name as present: it is never downloaded or
// linked over.
func TestImagePrepareLegacy_SymlinkAtTheNameIsPresent(t *testing.T) {
	h := newPrepareHost(t)
	artifact := filepath.Join(h.images, "ubuntu.qcow2")
	require.NoError(t, os.Symlink(filepath.Join(h.images, "gone.qcow2"), artifact))
	_, path, err := prepareLegacyImage(h.p, urlImageJSON(testImageURL), "ubuntu", "")
	require.NoError(t, err)
	assert.Equal(t, artifact, path)
	assert.Empty(t, h.log("curl"), "a present name is never downloaded over")
	fi, err := os.Lstat(artifact)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, fi.Mode()&os.ModeSymlink, "left as it was")
}

// TestImagePrepareIdentity_StampFilesMustBeOursAndNotWritable proves a
// sidecar or artifact that another principal could rewrite — group- or
// other-writable here; owned by another user in the unit case — carries no
// trusted stamp: Conflict, never reuse or "in progress".
func TestImagePrepareIdentity_StampFilesMustBeOursAndNotWritable(t *testing.T) {
	name := artifactNameFor(t, testImageUID, testDigest)
	for caseName, plant := range map[string]func(t *testing.T, artifact, sidecar string){
		"group-writable artifact": func(t *testing.T, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "x")
			require.NoError(t, os.Chmod(artifact, 0o664))
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino, size), time.Now())
		},
		"other-writable sidecar": func(t *testing.T, artifact, sidecar string) {
			ino, size := plantFile(t, artifact, "x")
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, ino, size), time.Now())
			require.NoError(t, os.Chmod(sidecar, 0o646))
		},
		"writable sidecar without artifact": func(t *testing.T, _, sidecar string) {
			plantSidecar(t, sidecar, mustSidecar(t, testImageUID, testDigest, 7, 9), time.Now())
			require.NoError(t, os.Chmod(sidecar, 0o666))
		},
	} {
		t.Run(caseName, func(t *testing.T) {
			h := newPrepareHost(t)
			artifact := filepath.Join(h.images, name+qcow2Ext)
			sidecar := filepath.Join(h.images, imageSidecarName(name))
			plant(t, artifact, sidecar)
			_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
			requireCode(t, err, codes.AlreadyExists)
			assert.Empty(t, h.log("curl"))
		})
	}

	// Ownership (a file of another user cannot be made in a test without
	// root): the observation of an otherwise perfect match that the SSH user
	// does not own carries no stamp.
	doc := testSidecar(testImageUID, testDigest, 5, 10)
	good := hostFileStat{exists: true, regular: true, inode: 5, size: 10, mtime: 90, perm: 0o444, owned: true}
	pr := artifactProbe{hostNow: 100, artifact: good, sidecar: good, doc: &doc}
	assert.True(t, pr.observation(time.Hour).Complete)
	for _, mutate := range []func(p *artifactProbe){
		func(p *artifactProbe) { p.artifact.owned = false },
		func(p *artifactProbe) { p.sidecar.owned = false },
		func(p *artifactProbe) { p.artifact.perm = 0o644 | 0o020 },
		func(p *artifactProbe) { p.sidecar.perm = 0o602 },
	} {
		p := pr
		mutate(&p)
		obs := p.observation(time.Hour)
		assert.Nil(t, obs.Stamp)
		assert.Equal(t, imageartifact.OutcomeConflict, imageartifact.Decide(obs, testIdentityRequest(testImageUID, testDigest)))
	}
}

// TestImageSidecar_LoggedFieldsAreKubernetesNames proves the stamp's echoed
// and logged identity fields must be Kubernetes names (no log forging).
func TestImageSidecar_LoggedFieldsAreKubernetesNames(t *testing.T) {
	valid, err := encodeImageSidecar(testSidecar(testImageUID, testDigest, 7, 9))
	require.NoError(t, err)
	for name, doc := range map[string]string{
		"newline in namespace": strings.Replace(string(valid), `"namespace":"team-a","name":"ubuntu-22.04"`, `"namespace":"team-a\nWARN forged","name":"ubuntu-22.04"`, 1),
		"uppercase name":       strings.Replace(string(valid), `"name":"ubuntu-22.04"`, `"name":"Ubuntu"`, 1),
		"empty image name":     strings.Replace(string(valid), `"name":"ubuntu-22.04"`, `"name":""`, 1),
		"bad preparedBy uid":   strings.Replace(string(valid), `"uid":"`+testProviderUID+`"`, `"uid":"x y"`, 1),
		"bad preparedBy name":  strings.Replace(string(valid), `"name":"libvirt"`, `"name":"a/b"`, 1),
		"bad preparedAt":       strings.Replace(string(valid), `"preparedAt":"2026-09-25T10:00:00Z"`, `"preparedAt":"yesterday"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, string(valid), doc)
			_, err := parseImageSidecar([]byte(doc))
			assert.Error(t, err)
		})
	}
	// A stamp with no preparedBy (a request that named no Provider) is fine.
	s := testSidecar(testImageUID, testDigest, 7, 9)
	s.PreparedBy = imageartifact.StampIdentity{}
	data, err := encodeImageSidecar(s)
	require.NoError(t, err)
	_, err = parseImageSidecar(data)
	assert.NoError(t, err)
}

// TestImagePrepare_RestoreconOnlyWithSELinux proves restorecon runs (through
// non-interactive sudo) only when SELinux is enabled on the host.
func TestImagePrepare_RestoreconOnlyWithSELinux(t *testing.T) {
	h := newPrepareHost(t)
	plantFile(t, filepath.Join(h.root, "selinux.disabled"), "")
	_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.NoError(t, err)
	assert.NotContains(t, h.log("sudo"), "restorecon")
}

// TestImagePrepare_ManagerRequestShapes sends what the Slice 2 manager sends —
// built like controller.imagePrepareRequest: the VMImage spec without its
// consumerNamespaceSelector, an EMPTY target name, the image identity, the
// imageartifact.SourceDigest of spec.source and the Provider identity — over
// the manager's transport client to the real provider: it takes the identity
// path and the echo confirms it. An identity-less request (an older manager)
// takes deprecated legacy mode.
func TestImagePrepare_ManagerRequestShapes(t *testing.T) {
	h := newPrepareHost(t)
	c := startLibvirtGRPC(t, h.p)

	image := &infravirtrigaudiov1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ubuntu-22.04", UID: types.UID(testImageUID)},
		Spec: infravirtrigaudiov1beta1.VMImageSpec{
			Source: infravirtrigaudiov1beta1.ImageSource{Libvirt: &infravirtrigaudiov1beta1.LibvirtImageSource{
				URL: "https://images.example.com/ubuntu-22.04.qcow2", Format: infravirtrigaudiov1beta1.ImageFormatQCOW2,
			}},
			Prepare:                   &infravirtrigaudiov1beta1.ImagePrepare{Timeout: &metav1.Duration{Duration: 45 * time.Minute}},
			ConsumerNamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "b"}},
		},
	}
	digest, err := imageartifact.SourceDigest(image.Spec.Source)
	require.NoError(t, err)
	spec := image.Spec
	spec.ConsumerNamespaceSelector = nil
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	req := contracts.ImagePrepareRequest{
		ImageJSON:    string(specJSON),
		TargetName:   "",
		Image:        contracts.ObjectIdentity{UID: string(image.UID), Namespace: image.Namespace, Name: image.Name},
		SourceDigest: digest,
		Provider:     contracts.ObjectIdentity{UID: testProviderUID, Namespace: "team-a", Name: "libvirt"},
	}
	legacyBefore := libvirtLegacyCount(t)

	resp, err := c.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, resp.ConfirmsIdentity(req), "identity path, echo confirmed: %+v", resp.Artifact)
	wantName, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt, req.Image, digest)
	require.NoError(t, err)
	assert.Equal(t, wantName, resp.PreparedImageID)
	assert.Equal(t, filepath.Join(h.images, wantName+qcow2Ext), resp.PreparedImagePath)
	assert.Contains(t, h.log("curl"), "--max-time 2700", "spec.prepare.timeout bounds the download")
	assert.Equal(t, legacyBefore, libvirtLegacyCount(t), "an identity request is not legacy")

	legacy := contracts.ImagePrepareRequest{ImageJSON: string(specJSON), TargetName: image.Name}
	lresp, err := c.PrepareImage(context.Background(), legacy)
	require.NoError(t, err)
	assert.Nil(t, lresp.Artifact, "legacy mode echoes no artifact")
	assert.Equal(t, image.Name, lresp.PreparedImageID)
	assert.Equal(t, filepath.Join(h.images, image.Name+qcow2Ext), lresp.PreparedImagePath)
	assert.Equal(t, legacyBefore+1, libvirtLegacyCount(t))
}
