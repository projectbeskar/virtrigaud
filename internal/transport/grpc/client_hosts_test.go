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

package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/mock"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestClient_ListHosts_MapsProtoToContract verifies the manager-side gRPC client
// threads the ADR-0007 P1 host inventory off the wire and maps every provider.v1
// HostInfo field (including the HostHealth enum) onto contracts.HostInfo.
func TestClient_ListHosts_MapsProtoToContract(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		ListHostsFn: func(_ context.Context, _ *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
			return &providerv1.ListHostsResponse{
				Hosts: []*providerv1.HostInfo{
					{
						Id:                 "host-a",
						Address:            "10.0.0.1:22",
						AllocatableCpu:     32,
						AllocatableMemMib:  262144,
						AllocatableStorage: 1 << 42,
						Health:             providerv1.HostHealth_HOST_HEALTH_READY,
						Labels:             map[string]string{"zone": "rack-1", "pool": "ssd"},
						CpuModel:           "EPYC-Milan",
						CpuFeatures:        []string{"avx2", "sse4.2"},
						MachineTypes:       []string{"pc-q35-8.2"},
						EmulatorVersion:    "8.2.0",
					},
					{
						Id:     "host-b",
						Health: providerv1.HostHealth_HOST_HEALTH_NOT_READY,
					},
				},
			}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-hosts")

	hosts, err := cli.ListHosts(context.Background())
	require.NoError(t, err)
	require.Len(t, hosts, 2)

	assert.Equal(t, contracts.HostInfo{
		ID:                 "host-a",
		Address:            "10.0.0.1:22",
		AllocatableCPU:     32,
		AllocatableMemMiB:  262144,
		AllocatableStorage: 1 << 42,
		Health:             contracts.HostHealthReady,
		Labels:             map[string]string{"zone": "rack-1", "pool": "ssd"},
		CPUModel:           "EPYC-Milan",
		CPUFeatures:        []string{"avx2", "sse4.2"},
		MachineTypes:       []string{"pc-q35-8.2"},
		EmulatorVersion:    "8.2.0",
	}, hosts[0])

	// NOT_READY maps through, and the zero enum/omitted fields stay zero-valued.
	assert.Equal(t, "host-b", hosts[1].ID)
	assert.Equal(t, contracts.HostHealthNotReady, hosts[1].Health)
}

// TestClient_GetHostInfo_MapsProtoToContract verifies GetHostInfo passes the
// host id on the wire and maps the returned HostInfo, including an unspecified
// health enum falling back to HostHealthUnspecified.
func TestClient_GetHostInfo_MapsProtoToContract(t *testing.T) {
	var gotHostID string
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		GetHostInfoFn: func(_ context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
			gotHostID = req.HostId
			return &providerv1.HostInfo{Id: req.HostId, Address: "10.0.0.9:22"}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-hosts")

	host, err := cli.GetHostInfo(context.Background(), "host-x")
	require.NoError(t, err)
	assert.Equal(t, "host-x", gotHostID, "the host id must be sent on the wire")
	assert.Equal(t, "host-x", host.ID)
	assert.Equal(t, "10.0.0.9:22", host.Address)
	assert.Equal(t, contracts.HostHealthUnspecified, host.Health)
}

// TestClient_GetCapabilities_SupportsClusteringSurface verifies the manager-side
// client maps the ADR-0007 P1 supports_clustering flag off the proto response
// onto contracts.Capabilities, so it reaches the Provider CR status / scheduler.
func TestClient_GetCapabilities_SupportsClusteringSurface(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
		GetCapabilitiesFn: func(_ context.Context, _ *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
			return &providerv1.GetCapabilitiesResponse{SupportsClustering: true}, nil
		},
	})
	defer cleanup()
	cli := newTestClient(t, dialer, "test-hosts")

	caps, err := cli.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, caps.SupportsClustering)
}

// TestClient_HostInventory_UnimplementedThroughMockProvider is the full-stack
// round trip: the real mock provider (a non-clustered provider, like the
// production single-host providers) answers ListHosts/GetHostInfo with
// Unimplemented, and the manager-side client surfaces that as an error rather
// than a silent empty result. This pins the contract-only PR's guarantee that
// every provider honestly reports Unimplemented over the wire.
func TestClient_HostInventory_UnimplementedThroughMockProvider(t *testing.T) {
	dialer, cleanup := startBufconnServer(t, mock.NewProvider())
	defer cleanup()
	cli := newTestClient(t, dialer, "mock")

	_, err := cli.ListHosts(context.Background())
	require.Error(t, err, "mock provider must report ListHosts Unimplemented")
	assert.True(t, contracts.IsNotSupported(err), "gRPC Unimplemented must map to a typed NotSupported error")

	_, err = cli.GetHostInfo(context.Background(), "host-1")
	require.Error(t, err, "mock provider must report GetHostInfo Unimplemented")
	assert.True(t, contracts.IsNotSupported(err), "gRPC Unimplemented must map to a typed NotSupported error")

	// And the mock honestly advertises it does not cluster.
	caps, err := cli.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, caps.SupportsClustering)
}
