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
	fn func(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error)
}

func (s *imagePrepareFakeServer) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	return s.fn(ctx, req)
}

// TestClient_PrepareImage_AsyncTaskRef verifies the request fields are forwarded
// and an async TaskRef is surfaced on the contract response.
func TestClient_PrepareImage_AsyncTaskRef(t *testing.T) {
	var got *providerv1.ImagePrepareRequest
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
			got = req
			return &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-42"}}, nil
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

	// Request fields forwarded verbatim (JSON / Json field-name mapping).
	require.NotNil(t, got)
	assert.Equal(t, `{"source":{"vsphere":{"ovaURL":"http://x/y.ova"}}}`, got.GetImageJson())
	assert.Equal(t, "ubuntu-tmpl", got.GetTargetName())
	assert.Equal(t, "datastore1", got.GetStorageHint())
}

// TestClient_PrepareImage_SyncEmptyTask verifies a synchronous provider (nil
// Task) yields an empty TaskRef, which the controller treats as "completed".
func TestClient_PrepareImage_SyncEmptyTask(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
			return &providerv1.TaskResponse{}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep")

	resp, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{TargetName: "t"})
	require.NoError(t, err)
	assert.Empty(t, resp.TaskRef)
}

// TestClient_PrepareImage_ErrorMapped verifies a provider error is mapped through
// mapGRPCError rather than leaking the raw gRPC status.
func TestClient_PrepareImage_ErrorMapped(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "bad image spec")
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-imgprep")

	_, err := cli.PrepareImage(context.Background(), contracts.ImagePrepareRequest{TargetName: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad image spec")
}
