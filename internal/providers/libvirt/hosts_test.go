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

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestServer_ListHosts_SingleHostUnimplemented pins that the libvirt gRPC server
// reports the ADR-0007 P1 host-inventory RPC as Unimplemented when it is NOT in
// clustered topology. A bare &Server{} (nil backend) stands in for the
// single-host / uninitialized case; a real single-host *Provider (clusterReg
// nil) takes the same path. ListHosts is a clustered-only RPC (D9).
func TestServer_ListHosts_SingleHostUnimplemented(t *testing.T) {
	s := &Server{}
	resp, err := s.ListHosts(context.Background(), &providerv1.ListHostsRequest{})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestServer_GetHostInfo_SingleHostUnimplemented pins the same Unimplemented
// contract for the single-host refresh RPC outside clustered topology.
func TestServer_GetHostInfo_SingleHostUnimplemented(t *testing.T) {
	s := &Server{}
	resp, err := s.GetHostInfo(context.Background(), &providerv1.GetHostInfoRequest{HostId: "host-1"})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// TestServer_GetCapabilities_SupportsClusteringFalse pins honesty-first (D7):
// a non-clustered (single-host / uninitialized) server advertises
// supports_clustering = false.
func TestServer_GetCapabilities_SupportsClusteringFalse(t *testing.T) {
	s := &Server{}
	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.False(t, caps.SupportsClustering)
}

// serverWithClusteredHostA builds a gRPC Server backed by a clustered *Provider
// fronting a single fully-answering host "host-a", for the clustered-mode RPC
// tests. It reuses the fake dialer/conn helpers from hostinfo_test.go.
func serverWithClusteredHostA(t *testing.T) *Server {
	t.Helper()
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	p, _ := newClusteredProviderForTest(t, inv,
		fakeDialer(map[hostconn.HostID]*fakeHostConn{"host-a": conn}, nil))
	return NewServer(p)
}

// TestServer_GetCapabilities_SupportsClusteringTrue verifies the flag flips true
// once the backend is actually in clustered topology (this is the PR that makes
// it real).
func TestServer_GetCapabilities_SupportsClusteringTrue(t *testing.T) {
	s := serverWithClusteredHostA(t)
	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.True(t, caps.SupportsClustering,
		"a clustered libvirt provider advertises supports_clustering now that ListHosts/GetHostInfo are real")
}

// TestServer_ListHosts_ClusteredMapsToProto verifies the gRPC server answers
// ListHosts in clustered mode and maps the backend HostInfo onto the wire type,
// including the HostHealth enum.
func TestServer_ListHosts_ClusteredMapsToProto(t *testing.T) {
	s := serverWithClusteredHostA(t)
	resp, err := s.ListHosts(context.Background(), &providerv1.ListHostsRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Hosts, 1)

	h := resp.Hosts[0]
	assert.Equal(t, "host-a", h.Id)
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_READY, h.Health)
	assert.Equal(t, int32(8), h.AllocatableCpu)
	assert.Equal(t, int64(16384), h.AllocatableMemMib)
	assert.Equal(t, "Skylake-Client-IBRS", h.CpuModel)
	assert.Equal(t, []string{"pc-i440fx-7.2"}, h.MachineTypes)
	assert.Equal(t, int64(75161927680), h.AllocatableStorage)
}

// TestServer_GetHostInfo_ClusteredMapsToProto verifies the single-host refresh
// RPC answers in clustered mode and maps onto the wire HostInfo.
func TestServer_GetHostInfo_ClusteredMapsToProto(t *testing.T) {
	s := serverWithClusteredHostA(t)
	h, err := s.GetHostInfo(context.Background(), &providerv1.GetHostInfoRequest{HostId: "host-a"})
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, "host-a", h.Id)
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_READY, h.Health)
}

// TestServer_GetHostInfo_ClusteredUnknownHostNotFound verifies an unknown host
// id surfaces as codes.NotFound through the gRPC server in clustered mode.
func TestServer_GetHostInfo_ClusteredUnknownHostNotFound(t *testing.T) {
	s := serverWithClusteredHostA(t)
	resp, err := s.GetHostInfo(context.Background(), &providerv1.GetHostInfoRequest{HostId: "host-nope"})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.NotFound, status.Code(err))
}
