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

package remote

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// capsServer answers GetCapabilities with linked clones off.
type capsServer struct {
	providerv1.UnimplementedProviderServer
}

func (capsServer) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{SupportsSnapshots: true, SupportsLinkedClones: false}, nil
}

// TestReadCapabilities_ReadsTheRuntimeWithoutCachingAClient pins that the
// Provider controller's capability read reaches the runtime endpoint and
// leaves the client cache GetProvider uses untouched.
func TestReadCapabilities_ReadsTheRuntimeWithoutCachingAClient(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, capsServer{})
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	p := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{
		Phase:    infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning,
		Endpoint: lis.Addr().String(),
	}
	r := NewResolver(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caps, err := r.ReadCapabilities(ctx, p)
	require.NoError(t, err)
	assert.True(t, caps.SupportsSnapshots)
	assert.False(t, caps.SupportsLinkedClones)
	assert.Empty(t, r.clients, "the read caches no client")
}

// TestReadCapabilities_RuntimeNotRunning pins that no read is attempted while
// the runtime is not Running.
func TestReadCapabilities_RuntimeNotRunning(t *testing.T) {
	p := newTestProvider(&infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{
		Phase:    infravirtrigaudiov1beta1.ProviderRuntimePhasePending,
		Endpoint: "provider.example.com:9443",
	}
	_, err := NewResolver(nil, nil).ReadCapabilities(context.Background(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phase=Pending")
}
