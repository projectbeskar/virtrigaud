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

package mock

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	imgUID      = "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a"
	imgUID2     = "0d4e7c1a-9f3b-4c2d-8e5a-6b7c8d9e0f1a"
	digestA     = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	digestB     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	provUID     = "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a"
	provUID2    = "7b2d3e4f-5a6b-4c7d-9e8f-0a1b2c3d4e5f"
	legacyTotal = "virtrigaud_provider_image_prepare_legacy_requests_total"
)

// newImageProvider returns a mock whose imports take delay, logging to buf.
func newImageProvider(t *testing.T, delay time.Duration) (*Provider, *bytes.Buffer) {
	t.Helper()
	t.Setenv("MOCK_FAILURE_MODE", "")
	t.Setenv("MOCK_SLOW_MODE", "")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewProvider(WithImagePrepareDelay(delay), WithLogger(logger)), &buf
}

// syncWriter serializes writes to w (the log buffer is read after the test).
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

// Write implements io.Writer.
func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// identityReq returns an identity request for the VMImage namespace/name with
// uid and digest, prepared through the Provider team-a/mock (provider uid).
func identityReq(namespace, name, uid, digest, provider string) *providerv1.ImagePrepareRequest {
	return &providerv1.ImagePrepareRequest{
		ImageJson:    `{"source":{"http":{"url":"https://images.example.com/disk.qcow2"}}}`,
		Image:        &providerv1.ObjectIdentity{Uid: uid, Namespace: namespace, Name: name},
		SourceDigest: digest,
		Provider:     &providerv1.ObjectIdentity{Uid: provider, Namespace: namespace, Name: "mock"},
	}
}

// artifactName is the name the mock derives for the identity.
func artifactName(t *testing.T, namespace, name, uid, digest string) string {
	t.Helper()
	n, err := imageartifact.ArtifactName(mockImageNameRule,
		contracts.ObjectIdentity{UID: uid, Namespace: namespace, Name: name}, digest)
	require.NoError(t, err)
	return n
}

// imageCount returns how many artifacts the mock holds.
func imageCount(p *Provider) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.images)
}

// storedStamp returns a copy of the stamp of the artifact name (nil if none).
func storedStamp(t *testing.T, p *Provider, name string) *imageartifact.Stamp {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	img, ok := p.images[name]
	require.True(t, ok, "artifact %q exists", name)
	if img.stamp == nil {
		return nil
	}
	s := *img.stamp
	return &s
}

// finishTask marks the task done, failed when errMsg is set.
func finishTask(p *Provider, taskID, errMsg string) {
	p.FinishTask(taskID, errMsg)
}

// TestMockFinishTask verifies the exported task-completion hook: TaskStatus
// reports the task done (and failed with the given message), and an unknown
// task is reported as such.
func TestMockFinishTask(t *testing.T) {
	p, _ := newImageProvider(t, time.Hour)
	resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	task := resp.GetTask()
	require.NotNil(t, task)

	st, err := p.TaskStatus(context.Background(), &providerv1.TaskStatusRequest{Task: task})
	require.NoError(t, err)
	assert.False(t, st.GetDone())

	require.True(t, p.FinishTask(task.GetId(), "download failed"))
	st, err = p.TaskStatus(context.Background(), &providerv1.TaskStatusRequest{Task: task})
	require.NoError(t, err)
	assert.True(t, st.GetDone())
	assert.Equal(t, "download failed", st.GetError())

	assert.False(t, p.FinishTask("no-such-task", ""))
}

// legacyCount reads the legacy-request counter for the mock.
func legacyCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != legacyTotal {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider_type" && l.GetValue() == mockProviderType {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestMockAdvertisesImageArtifactIdentity(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImageImport())
	assert.True(t, caps.GetSupportsImageArtifactIdentity())
}

// TestMockImagePrepareFreshStampsArtifact verifies a first identity prepare
// imports under the derived name, stamps it with the request identity and
// echoes the stamp (reused=false).
func TestMockImagePrepareFreshStampsArtifact(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	name := artifactName(t, "team-a", "ubuntu", imgUID, digestA)

	resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	assert.Nil(t, resp.GetTask(), "a zero-delay import completes within the call")
	assert.Equal(t, name, resp.GetPreparedImageId())
	assert.Equal(t, "/var/lib/virtrigaud/mock/"+name+".qcow2", resp.GetPreparedImagePath())
	require.NotNil(t, resp.GetArtifact())
	assert.Equal(t, name, resp.GetArtifact().GetName())
	assert.Equal(t, imgUID, resp.GetArtifact().GetImage().GetUid())
	assert.Equal(t, "team-a", resp.GetArtifact().GetImage().GetNamespace())
	assert.Equal(t, "ubuntu", resp.GetArtifact().GetImage().GetName())
	assert.Equal(t, digestA, resp.GetArtifact().GetSourceDigest())
	assert.False(t, resp.GetArtifact().GetReused())

	stamp := storedStamp(t, p, name)
	require.NotNil(t, stamp)
	require.NoError(t, stamp.Validate())
	assert.Equal(t, imgUID, stamp.Image.UID)
	assert.Equal(t, digestA, stamp.SourceDigest)
	assert.Equal(t, imageartifact.StampIdentity{UID: provUID, Namespace: "team-a", Name: "mock"}, stamp.PreparedBy)
	_, err = time.Parse(time.RFC3339, stamp.PreparedAt)
	assert.NoError(t, err)
}

// TestMockImagePrepareReusesMatchingStamp verifies an idempotent re-prepare
// reuses the artifact with no new import, even through another Provider
// sharing the location (ADR-0009 D5: preparedBy is not part of the rule).
func TestMockImagePrepareReusesMatchingStamp(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	_, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)

	for _, provider := range []string{provUID, provUID2} {
		resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provider))
		require.NoError(t, err)
		assert.Nil(t, resp.GetTask())
		assert.True(t, resp.GetArtifact().GetReused())
		assert.Equal(t, imgUID, resp.GetArtifact().GetImage().GetUid())
	}
	assert.Equal(t, 1, imageCount(p), "no second artifact")
	name := artifactName(t, "team-a", "ubuntu", imgUID, digestA)
	assert.Equal(t, provUID, storedStamp(t, p, name).PreparedBy.UID, "the stamp is never rewritten")
}

// TestMockImagePrepareConflictOnStampMismatch verifies every artifact at the
// derived name that was not prepared for the requesting identity is refused
// with AlreadyExists and left exactly as it was (ADR-0009 D4).
func TestMockImagePrepareConflictOnStampMismatch(t *testing.T) {
	name := artifactName(t, "team-a", "ubuntu", imgUID, digestA)
	req := imageartifact.Request{
		Mode:         imageartifact.ModeIdentity,
		Image:        contracts.ObjectIdentity{UID: imgUID, Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: digestA,
	}
	good := imageartifact.NewStamp(req, time.Now())
	otherUID := good
	otherUID.Image.UID = imgUID2
	otherDigest := good
	otherDigest.SourceDigest = digestB
	untrusted := good
	untrusted.StampVersion = 2

	for caseName, planted := range map[string]*imageartifact.Stamp{
		"unstamped (legacy, manual or planted)": nil,
		"stamped for another VMImage UID":       &otherUID,
		"stamped for another source digest":     &otherDigest,
		"untrusted stamp version":               &untrusted,
	} {
		t.Run(caseName, func(t *testing.T) {
			p, logs := newImageProvider(t, 0)
			p.PlantImageArtifact(name, planted)

			_, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
			require.Error(t, err)
			assert.Equal(t, codes.AlreadyExists, status.Code(err))
			assert.Contains(t, err.Error(), name)
			assert.NotContains(t, err.Error(), imgUID2, "the recorded owner goes to the log only")

			assert.Equal(t, 1, imageCount(p), "no import")
			if planted == nil {
				assert.Nil(t, storedStamp(t, p, name), "never re-stamped")
			} else {
				assert.Equal(t, *planted, *storedStamp(t, p, name), "never overwritten or re-stamped")
			}
			assert.Contains(t, logs.String(), "Refusing a prepared-image artifact")
		})
	}
}

// TestMockImagePrepareAsyncInProgressThenReuse verifies an asynchronous import:
// the first call returns a task, a re-prepare while it runs is a retryable
// Unavailable, and once the task is done the re-prepare reuses the artifact
// (the confirm call whose echo the manager records, ADR-0009 D7).
func TestMockImagePrepareAsyncInProgressThenReuse(t *testing.T) {
	p, _ := newImageProvider(t, time.Hour)
	first, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	require.NotNil(t, first.GetTask())
	assert.False(t, first.GetArtifact().GetReused())

	_, err = p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))

	finishTask(p, first.GetTask().GetId(), "")
	resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	assert.Nil(t, resp.GetTask())
	assert.True(t, resp.GetArtifact().GetReused())
	assert.Equal(t, first.GetPreparedImageId(), resp.GetPreparedImageId())
}

// TestMockImagePrepareAbandonedIsReimported verifies an artifact whose import
// failed (incomplete, matching stamp, not live) is removed and imported again,
// while a failed import of ANOTHER identity at the name would be a Conflict.
func TestMockImagePrepareAbandonedIsReimported(t *testing.T) {
	p, logs := newImageProvider(t, time.Hour)
	first, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	finishTask(p, first.GetTask().GetId(), "download failed")

	again, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	require.NotNil(t, again.GetTask())
	assert.NotEqual(t, first.GetTask().GetId(), again.GetTask().GetId(), "a new import")
	assert.False(t, again.GetArtifact().GetReused())
	assert.Contains(t, logs.String(), "abandoned")
}

// TestMockImagePrepareIdentityChangesGetNewArtifacts verifies ADR-0009 D1: the
// same VMImage name in two namespaces, a re-created VMImage (new UID) and a
// changed source (new digest) each get their own artifact — never a Conflict
// with, or reuse of, another's.
func TestMockImagePrepareIdentityChangesGetNewArtifacts(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	names := map[string]bool{}
	for _, req := range []*providerv1.ImagePrepareRequest{
		identityReq("team-a", "ubuntu", imgUID, digestA, provUID),
		identityReq("team-b", "ubuntu", "3a4b5c6d-7e8f-4a0b-9c1d-2e3f4a5b6c7d", digestA, provUID2),
		identityReq("team-a", "ubuntu", imgUID2, digestA, provUID), // re-created
		identityReq("team-a", "ubuntu", imgUID, digestB, provUID),  // source changed
	} {
		resp, err := p.ImagePrepare(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, resp.GetArtifact().GetReused())
		names[resp.GetPreparedImageId()] = true
	}
	assert.Len(t, names, 4)
	assert.Equal(t, 4, imageCount(p))
}

// TestMockImagePrepareLegacy verifies an identity-less request from an older
// manager is served in deprecated legacy mode (ADR-0009 D7, Q3): the bare
// name, reuse by name, no artifact echo, and the deprecation signal (a WARN
// log and one counter increment per request).
func TestMockImagePrepareLegacy(t *testing.T) {
	p, logs := newImageProvider(t, 0)
	before := legacyCount(t)
	legacy := &providerv1.ImagePrepareRequest{ImageJson: "{}", TargetName: "ubuntu"}

	resp, err := p.ImagePrepare(context.Background(), legacy)
	require.NoError(t, err)
	assert.Equal(t, "ubuntu", resp.GetPreparedImageId())
	assert.Equal(t, "/var/lib/virtrigaud/mock/ubuntu.qcow2", resp.GetPreparedImagePath())
	assert.Nil(t, resp.GetArtifact(), "no artifact echo in legacy mode")
	assert.Nil(t, storedStamp(t, p, "ubuntu"), "a legacy artifact is unstamped")

	resp, err = p.ImagePrepare(context.Background(), legacy)
	require.NoError(t, err)
	assert.Equal(t, "ubuntu", resp.GetPreparedImageId(), "reused by name")
	assert.Equal(t, 1, imageCount(p))

	assert.Equal(t, before+2, legacyCount(t))
	assert.Equal(t, 2, strings.Count(logs.String(), imageartifact.LegacyRequestWarning))
	assert.Contains(t, logs.String(), "level=WARN")
}

// TestMockLegacyAndIdentityArtifactsAreDisjoint verifies ADR-0009 D1.3: an
// identity prepare never adopts a legacy bare-name artifact (its name differs),
// and a legacy request can never address a new-scheme artifact (a bare name
// with '_' is refused).
func TestMockLegacyAndIdentityArtifactsAreDisjoint(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	_, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{TargetName: "ubuntu"})
	require.NoError(t, err)

	resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
	require.NoError(t, err)
	assert.False(t, resp.GetArtifact().GetReused(), "the legacy artifact is not adopted")
	assert.NotEqual(t, "ubuntu", resp.GetPreparedImageId())
	assert.Nil(t, storedStamp(t, p, "ubuntu"), "the legacy artifact is left alone")

	before := legacyCount(t)
	_, err = p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{TargetName: resp.GetPreparedImageId()})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, before, legacyCount(t), "a malformed request is refused before it is served")
}

// TestMockImagePrepareRejectsMalformed verifies malformed requests are
// InvalidArgument and import nothing.
func TestMockImagePrepareRejectsMalformed(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	withTarget := identityReq("team-a", "ubuntu", imgUID, digestA, provUID)
	withTarget.TargetName = "ubuntu"
	noDigest := identityReq("team-a", "ubuntu", imgUID, "", provUID)
	for name, req := range map[string]*providerv1.ImagePrepareRequest{
		"neither identity nor target_name": {ImageJson: "{}"},
		"identity with target_name":        withTarget,
		"identity without digest":          noDigest,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ImagePrepare(context.Background(), req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
	assert.Equal(t, 0, imageCount(p))
}

// TestMockImagePrepareConcurrentSingleImport verifies concurrent prepares of
// one identity import exactly once: the others reuse the artifact (sync) and
// none sees a Conflict. Run with -race.
func TestMockImagePrepareConcurrentSingleImport(t *testing.T) {
	p, _ := newImageProvider(t, 0)
	const n = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		imported int
		errs     []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := p.ImagePrepare(context.Background(), identityReq("team-a", "ubuntu", imgUID, digestA, provUID))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if !resp.GetArtifact().GetReused() {
				imported++
			}
		}()
	}
	wg.Wait()
	assert.Empty(t, errs)
	assert.Equal(t, 1, imported)
	assert.Equal(t, 1, imageCount(p))
}
