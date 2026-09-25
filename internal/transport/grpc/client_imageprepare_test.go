/*
Copyright 2025.

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

package grpc

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/mock"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// imagePrepareFakeServer is a minimal ProviderServer whose ImagePrepare is
// configurable, used to exercise the manager-side Client.PrepareImage transport
// (#154). It embeds the Unimplemented server so only ImagePrepare needs a body.
type imagePrepareFakeServer struct {
	providerv1.UnimplementedProviderServer
	fn func(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error)
}

func (s *imagePrepareFakeServer) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	return s.fn(ctx, req)
}

// TestClient_PrepareImage_AsyncTaskRef verifies the request fields are forwarded
// and an async TaskRef plus the prepared-image location (id/path) are surfaced on
// the contract response (#214).
func TestClient_PrepareImage_AsyncTaskRef(t *testing.T) {
	var got *providerv1.ImagePrepareRequest
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			got = req
			return &providerv1.ImagePrepareResponse{
				Task:              &providerv1.TaskRef{Id: "task-42"},
				PreparedImageId:   "ubuntu-tmpl",
				PreparedImagePath: "/pool/ubuntu-tmpl.qcow2",
			}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep")

	resp, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{
		ImageJSON:   `{"source":{"vsphere":{"ovaURL":"http://x/y.ova"}}}`,
		TargetName:  "ubuntu-tmpl",
		StorageHint: "datastore1",
	})
	require.NoError(t, err)
	assert.Equal(t, "task-42", resp.TaskRef)
	// The prepared location round-trips even on the async path (known at trigger).
	assert.Equal(t, "ubuntu-tmpl", resp.PreparedImageID)
	assert.Equal(t, "/pool/ubuntu-tmpl.qcow2", resp.PreparedImagePath)

	// Request fields forwarded verbatim (JSON / Json field-name mapping).
	require.NotNil(t, got)
	assert.Equal(t, `{"source":{"vsphere":{"ovaURL":"http://x/y.ova"}}}`, got.GetImageJson())
	assert.Equal(t, "ubuntu-tmpl", got.GetTargetName())
	assert.Equal(t, "datastore1", got.GetStorageHint())
}

// TestClient_PrepareImage_SyncEmptyTask verifies a synchronous provider (nil
// Task) yields an empty TaskRef, which the controller treats as "completed",
// while still surfacing the prepared-image location (#214).
func TestClient_PrepareImage_SyncEmptyTask(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			return &providerv1.ImagePrepareResponse{
				PreparedImageId:   "prepared-tmpl",
				PreparedImagePath: "/pool/prepared-tmpl.qcow2",
			}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep")

	resp, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{TargetName: "t"})
	require.NoError(t, err)
	assert.Empty(t, resp.TaskRef)
	assert.Equal(t, "prepared-tmpl", resp.PreparedImageID)
	assert.Equal(t, "/pool/prepared-tmpl.qcow2", resp.PreparedImagePath)
}

// testImageDigest is a syntactically valid ADR-0009 D2 source digest.
const testImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestClient_PrepareImage_IdentityRoundTrip verifies every ADR-0009 D7 field
// crosses the wire: the request's image identity, source digest and Provider
// identity reach the provider (with the empty target_name the new manager
// sends), and the provider's artifact echo reaches the contract response and
// confirms the request's identity.
func TestClient_PrepareImage_IdentityRoundTrip(t *testing.T) {
	var got *providerv1.ImagePrepareRequest
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			got = req
			return &providerv1.ImagePrepareResponse{
				PreparedImageId:   "team-a.ubuntu_3c9e1f0a7b2d4e61",
				PreparedImagePath: "/pool/team-a.ubuntu_3c9e1f0a7b2d4e61.qcow2",
				Artifact: &providerv1.PreparedArtifact{
					Name:         "team-a.ubuntu_3c9e1f0a7b2d4e61",
					Image:        &providerv1.ObjectIdentity{Uid: "img-uid", Namespace: "team-a", Name: "ubuntu"},
					SourceDigest: testImageDigest,
					Reused:       true,
				},
			}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep-identity")

	req := contracts.ImagePrepareRequest{
		ImageJSON:    `{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`,
		Image:        contracts.ObjectIdentity{UID: "img-uid", Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: testImageDigest,
		Provider:     contracts.ObjectIdentity{UID: "prov-uid", Namespace: "team-a", Name: "libvirt"},
	}
	resp, err := cli.PrepareImage(context.Background(), req)
	require.NoError(t, err)

	require.NotNil(t, got)
	assert.Empty(t, got.GetTargetName())
	assert.Equal(t, "img-uid", got.GetImage().GetUid())
	assert.Equal(t, "team-a", got.GetImage().GetNamespace())
	assert.Equal(t, "ubuntu", got.GetImage().GetName())
	assert.Equal(t, testImageDigest, got.GetSourceDigest())
	assert.Equal(t, "prov-uid", got.GetProvider().GetUid())
	assert.Equal(t, "team-a", got.GetProvider().GetNamespace())
	assert.Equal(t, "libvirt", got.GetProvider().GetName())

	require.NotNil(t, resp.Artifact)
	assert.Equal(t, contracts.PreparedArtifact{
		Name:         "team-a.ubuntu_3c9e1f0a7b2d4e61",
		Image:        contracts.ObjectIdentity{UID: "img-uid", Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: testImageDigest,
		Reused:       true,
	}, *resp.Artifact)
	assert.Equal(t, "team-a.ubuntu_3c9e1f0a7b2d4e61", resp.PreparedImageID)
	assert.True(t, resp.ConfirmsIdentity(req))
}

// TestClient_PrepareImage_LegacyRequestSendsNoIdentity verifies a request
// without any identity field is sent as a plain legacy request, and that a
// response without an artifact (an older provider, or legacy mode) surfaces a
// nil Artifact that confirms nothing.
func TestClient_PrepareImage_LegacyRequestSendsNoIdentity(t *testing.T) {
	var got *providerv1.ImagePrepareRequest
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			got = req
			return &providerv1.ImagePrepareResponse{PreparedImageId: "ubuntu"}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep-legacy")

	req := contracts.ImagePrepareRequest{TargetName: "ubuntu"}
	resp, err := cli.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.GetImage())
	assert.Nil(t, got.GetProvider())
	assert.Empty(t, got.GetSourceDigest())
	assert.Equal(t, "ubuntu", got.GetTargetName())
	assert.Nil(t, resp.Artifact)
	assert.False(t, resp.ConfirmsIdentity(req))
}

// TestClient_PrepareImage_RefusesPartialIdentity verifies the client never
// sends a request whose ADR-0009 identity is incomplete: such a request would
// otherwise lose its UID-less identity on the wire and be served as a legacy
// bare-name request. Each is refused as a non-retryable InvalidSpec with no
// RPC.
func TestClient_PrepareImage_RefusesPartialIdentity(t *testing.T) {
	var calls atomic.Int32
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			calls.Add(1)
			return &providerv1.ImagePrepareResponse{PreparedImageId: "ubuntu"}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep-partial")

	uid := "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a"
	provider := contracts.ObjectIdentity{UID: "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a", Namespace: "team-a", Name: "libvirt"}
	for name, req := range map[string]contracts.ImagePrepareRequest{
		"image namespace/name without UID, with target name": {
			TargetName: "ubuntu", Image: contracts.ObjectIdentity{Namespace: "team-a", Name: "ubuntu"}},
		"image namespace/name without UID, with digest": {
			Image: contracts.ObjectIdentity{Namespace: "team-a", Name: "ubuntu"}, SourceDigest: testImageDigest},
		"digest without image, with target name":   {TargetName: "ubuntu", SourceDigest: testImageDigest},
		"provider without image, with target name": {TargetName: "ubuntu", Provider: provider},
		"provider without UID": {
			Image: contracts.ObjectIdentity{UID: uid, Namespace: "team-a", Name: "ubuntu"}, SourceDigest: testImageDigest,
			Provider: contracts.ObjectIdentity{Namespace: "team-a", Name: "libvirt"}},
		"identity with a target name": {
			TargetName: "ubuntu", Image: contracts.ObjectIdentity{UID: uid, Namespace: "team-a", Name: "ubuntu"},
			SourceDigest: testImageDigest},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cli.PrepareImage(context.Background(), req)
			require.Error(t, err)
			assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
			assert.False(t, contracts.IsRetryable(err))
		})
	}
	assert.Zero(t, calls.Load(), "no refused request reaches the provider")
}

// TestClient_PrepareImage_ConflictMapped verifies the provider's refusal of an
// artifact whose stamp does not match (AlreadyExists, ADR-0009 D4) reaches the
// manager as a typed, non-retryable Conflict, and that an in-progress prepare
// (Unavailable) is retryable.
func TestClient_PrepareImage_ConflictMapped(t *testing.T) {
	for name, tc := range map[string]struct {
		code          codes.Code
		wantConflict  bool
		wantRetryable bool
	}{
		"stamp mismatch is a Conflict": {codes.AlreadyExists, true, false},
		"in progress is retryable":     {codes.Unavailable, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
				fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
					return nil, status.Error(tc.code, "refused")
				},
			})
			defer cleanup()
			cli := newTestClient(t, dialer, "test-imgprep-conflict")

			_, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{
				Image:        contracts.ObjectIdentity{UID: "img-uid"},
				SourceDigest: testImageDigest,
			})
			require.Error(t, err)
			assert.Equal(t, tc.wantConflict, contracts.IsConflict(err))
			assert.Equal(t, tc.wantRetryable, contracts.IsRetryable(err))
		})
	}
}

// TestClient_PrepareImage_InProgressIsTypedAndNotAnInfraFailure verifies the
// provider's "still being prepared for this VMImage" answer
// (imageartifact.InProgressError, ADR-0009 D4) reaches the manager as a typed,
// retryable InProgress error, and does not count toward the circuit breaker:
// a long import through one Provider must not open the breaker of another
// that shares its image location. A plain Unavailable still counts, and the
// reason is honoured on ImagePrepare only.
func TestClient_PrepareImage_InProgressIsTypedAndNotAnInfraFailure(t *testing.T) {
	inProgress := imageartifact.InProgressError("team-a.ubuntu_3c9e1f0a7b2d4e61")
	prepare := providerv1.Provider_ImagePrepare_FullMethodName
	assert.False(t, countsTowardBreaker(prepare, inProgress), "in progress says nothing about the provider's health")
	assert.True(t, countsTowardBreaker(prepare, status.Error(codes.Unavailable, "provider down")))
	assert.True(t, countsTowardBreaker(providerv1.Provider_Create_FullMethodName, inProgress),
		"on any other RPC the reason is not a VirtRigaud answer: the Unavailable counts")
	assert.True(t, countsTowardBreaker(providerv1.Provider_TaskStatus_FullMethodName, inProgress))

	// The typed mapping is ImagePrepare's too: another RPC maps the same
	// status to a plain retryable error.
	other := (&Client{}).mapGRPCError("create", inProgress)
	assert.False(t, contracts.IsInProgress(other), "%v", other)
	assert.True(t, contracts.IsRetryable(other))

	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			return nil, inProgress
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep-inprogress")

	_, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{
		Image:        contracts.ObjectIdentity{UID: "img-uid"},
		SourceDigest: testImageDigest,
	})
	require.Error(t, err)
	assert.True(t, contracts.IsInProgress(err), "%v", err)
	assert.True(t, contracts.IsRetryable(err))
	assert.False(t, contracts.IsConflict(err))
}

// TestClient_PrepareImage_AgainstMockProvider runs the ADR-0009 contract end to
// end over gRPC against the mock provider: an identity prepare is confirmed by
// its echo, a re-prepare reuses the artifact, a foreign artifact at the derived
// name is a typed Conflict, and a legacy request gets no echo.
func TestClient_PrepareImage_AgainstMockProvider(t *testing.T) {
	prov := mock.NewProvider(mock.WithImagePrepareDelay(0))
	dialer, cleanup := startBufconnServer(t, prov)
	defer cleanup()
	cli := newTestClient(t, dialer, "mock")

	caps, err := cli.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, caps.SupportsImageImport)
	assert.True(t, caps.SupportsImageArtifactIdentity)

	req := contracts.ImagePrepareRequest{
		ImageJSON:    `{"source":{"http":{"url":"https://images.example.com/disk.qcow2"}}}`,
		Image:        contracts.ObjectIdentity{UID: "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a", Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: testImageDigest,
		Provider:     contracts.ObjectIdentity{UID: "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a", Namespace: "team-a", Name: "mock"},
	}
	first, err := cli.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	require.True(t, first.ConfirmsIdentity(req))
	assert.False(t, first.Artifact.Reused)
	assert.Equal(t, first.Artifact.Name, first.PreparedImageID)

	again, err := cli.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	require.True(t, again.ConfirmsIdentity(req))
	assert.True(t, again.Artifact.Reused)

	// Another VMImage whose derived name is occupied by an unstamped artifact.
	other := req
	other.Image.UID = "0d4e7c1a-9f3b-4c2d-8e5a-6b7c8d9e0f1a"
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt, other.Image, other.SourceDigest)
	require.NoError(t, err)
	prov.PlantImageArtifact(name, nil)
	_, err = cli.PrepareImage(context.Background(), other)
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "a stamp mismatch is a non-retryable Conflict: %v", err)

	legacy, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{TargetName: "ubuntu"})
	require.NoError(t, err)
	assert.Nil(t, legacy.Artifact)
	assert.Equal(t, "ubuntu", legacy.PreparedImageID)
	assert.False(t, legacy.ConfirmsIdentity(req))
}

// TestClient_PrepareImage_ErrorMapped verifies a provider error is mapped through
// mapGRPCError rather than leaking the raw gRPC status.
func TestClient_PrepareImage_ErrorMapped(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "bad image spec")
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep")

	_, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{TargetName: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad image spec")
}
