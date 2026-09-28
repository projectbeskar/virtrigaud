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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// reconfigureResultServer answers Reconfigure and Describe with fixed
// responses, so the tests below pin how the manager's client maps the
// restart_required and max_memory_mib wire fields.
type reconfigureResultServer struct {
	providerv1.UnimplementedProviderServer
	reconfigure *providerv1.TaskResponse
	describe    *providerv1.DescribeResponse
}

func (s *reconfigureResultServer) Reconfigure(context.Context, *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	return s.reconfigure, nil
}

func (s *reconfigureResultServer) Describe(context.Context, *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	return s.describe, nil
}

// TestClient_Reconfigure_MapsRestartRequired: a provider's restart_required
// answer reaches the manager as ReconfigureResult.RestartRequired, and a task
// reference as TaskRef; an older provider that never sets the field reads as
// false (everything applied live).
func TestClient_Reconfigure_MapsRestartRequired(t *testing.T) {
	cases := []struct {
		name string
		resp *providerv1.TaskResponse
		want contracts.ReconfigureResult
	}{
		{"applied live (older provider, fields unset)", &providerv1.TaskResponse{}, contracts.ReconfigureResult{}},
		{"applied, restart required", &providerv1.TaskResponse{RestartRequired: true}, contracts.ReconfigureResult{RestartRequired: true}},
		{"asynchronous", &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-1"}}, contracts.ReconfigureResult{TaskRef: "task-1"}},
		{"marked honest (review L1)", &providerv1.TaskResponse{HonestResult: true, RestartRequired: true},
			contracts.ReconfigureResult{RestartRequired: true, Honest: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newTestClientForVMOps(t, &reconfigureResultServer{reconfigure: tc.resp}, "reconfigure-result", "reconfigure-result")
			got, err := cli.Reconfigure(context.Background(), contracts.VMRef{ID: "vm-1"}, contracts.CreateRequest{Name: "vm-1"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClient_GetCapabilities_HonestReconfigure (review H3): the capability
// round-trips, and is false from a provider that predates it — which makes
// the manager hold clustered resizes instead of trusting its reply.
func TestClient_GetCapabilities_HonestReconfigure(t *testing.T) {
	for name, advertised := range map[string]bool{"advertised": true, "absent (older provider)": false} {
		t.Run(name, func(t *testing.T) {
			dialer, cleanup := startBufconnServer(t, &fakeProviderServer{
				GetCapabilitiesFn: func(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
					return &providerv1.GetCapabilitiesResponse{SupportsHonestReconfigure: advertised}, nil
				},
			})
			defer cleanup()
			caps, err := newTestClient(t, dialer, "test-caps-honest").GetCapabilities(context.Background())
			require.NoError(t, err)
			assert.Equal(t, advertised, caps.SupportsHonestReconfigure)
		})
	}
}

// TestClient_Describe_MapsMaxMemory: the provider-reported memory ceiling
// reaches the manager as DescribeResponse.MaxMemoryMiB; 0 means not reported.
func TestClient_Describe_MapsMaxMemory(t *testing.T) {
	for _, maxMiB := range []int64{0, 8192} {
		vcpus := int32(maxMiB / 1024)
		cli := newTestClientForVMOps(t, &reconfigureResultServer{
			describe: &providerv1.DescribeResponse{Exists: true, PowerState: "On", MaxMemoryMib: maxMiB, Vcpus: vcpus},
		}, "describe-maxmem", "describe-maxmem")
		got, err := cli.Describe(context.Background(), contracts.VMRef{ID: "vm-1"})
		require.NoError(t, err)
		assert.Equal(t, maxMiB, got.MaxMemoryMiB)
		assert.Equal(t, vcpus, got.VCPUs, "the reported vCPUs; 0 = not reported")
	}
}
