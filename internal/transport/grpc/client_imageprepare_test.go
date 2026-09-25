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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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
// without an identity (UID) sends none on the wire — not even a partial
// namespace/name — and that a response without an artifact (an older provider,
// or legacy mode) surfaces a nil Artifact that confirms nothing.
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

	req := contracts.ImagePrepareRequest{
		TargetName: "ubuntu",
		Image:      contracts.ObjectIdentity{Namespace: "team-a", Name: "ubuntu"},
		Provider:   contracts.ObjectIdentity{Namespace: "team-a", Name: "libvirt"},
	}
	resp, err := cli.PrepareImage(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.GetImage(), "an identity without a UID is never sent")
	assert.Nil(t, got.GetProvider(), "a Provider identity without a UID is never sent")
	assert.Empty(t, got.GetSourceDigest())
	assert.Equal(t, "ubuntu", got.GetTargetName())
	assert.Nil(t, resp.Artifact)
	assert.False(t, resp.ConfirmsIdentity(req))
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
