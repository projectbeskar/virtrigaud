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

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestClient_GetCapabilities_StorageBackendsSurface verifies the manager-side
// gRPC client maps the ADR-0006 storage-backend / transfer-mode fields off the
// proto GetCapabilitiesResponse onto contracts.Capabilities, so they reach the
// Provider CR status and the migration gate. Before this wiring the fields were
// silently dropped at the client boundary.
func TestClient_GetCapabilities_StorageBackendsSurface(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		GetCapabilitiesFn: func(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
			return &providerv1.GetCapabilitiesResponse{
				SupportedExportBackends: []string{"pvc"},
				SupportedImportBackends: []string{"pvc"},
				SupportedTransferModes:  []string{"relay"},
			}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-caps")

	caps, err := cli.GetCapabilities(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{"pvc"}, caps.SupportedExportBackends)
	assert.Equal(t, []string{"pvc"}, caps.SupportedImportBackends)
	assert.Equal(t, []string{"relay"}, caps.SupportedTransferModes)
}
