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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestListHosts_Unimplemented pins that the mock provider — which models no
// clustered host set — reports the ADR-0007 P1 host-inventory RPC as gRPC
// Unimplemented, exactly like the production single-host providers.
func TestListHosts_Unimplemented(t *testing.T) {
	p := NewProvider()
	resp, err := p.ListHosts(context.Background(), &providerv1.ListHostsRequest{})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestGetHostInfo_Unimplemented pins the same Unimplemented contract for the
// single-host refresh RPC.
func TestGetHostInfo_Unimplemented(t *testing.T) {
	p := NewProvider()
	resp, err := p.GetHostInfo(context.Background(), &providerv1.GetHostInfoRequest{HostId: "host-1"})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestGetCapabilities_SupportsClusteringFalse pins honesty-first (D7): the mock
// provider advertises supports_clustering = false.
func TestGetCapabilities_SupportsClusteringFalse(t *testing.T) {
	p := NewProvider()
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.False(t, caps.SupportsClustering)
}
