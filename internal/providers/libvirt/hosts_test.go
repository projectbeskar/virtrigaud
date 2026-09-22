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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestServer_ListHosts_Unimplemented pins that the libvirt gRPC server reports
// the ADR-0007 P1 host-inventory RPC as Unimplemented in this contract-only PR.
// libvirt is the first hypervisor slated to become a clustered provider, but the
// real N-host implementation lands in a later ADR-0007 P1 PR.
func TestServer_ListHosts_Unimplemented(t *testing.T) {
	s := &Server{}
	resp, err := s.ListHosts(context.Background(), &providerv1.ListHostsRequest{})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestServer_GetHostInfo_Unimplemented pins the same Unimplemented contract for
// the single-host refresh RPC.
func TestServer_GetHostInfo_Unimplemented(t *testing.T) {
	s := &Server{}
	resp, err := s.GetHostInfo(context.Background(), &providerv1.GetHostInfoRequest{HostId: "host-1"})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestServer_GetCapabilities_SupportsClusteringFalse pins honesty-first (D7):
// until the real clustered implementation ships, libvirt advertises
// supports_clustering = false.
func TestServer_GetCapabilities_SupportsClusteringFalse(t *testing.T) {
	s := &Server{}
	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.False(t, caps.SupportsClustering)
}
