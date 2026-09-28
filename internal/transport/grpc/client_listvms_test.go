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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// slice4Server answers ListVMs with a canned response and records the
// TransferOwner request (ADR-0007 Addendum A, slice 4).
type slice4Server struct {
	providerv1.UnimplementedProviderServer
	list        *providerv1.ListVMsResponse
	transferErr error

	mu       sync.Mutex
	transfer *providerv1.TransferOwnerRequest
}

func (s *slice4Server) ListVMs(context.Context, *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	return s.list, nil
}

func (s *slice4Server) TransferOwner(_ context.Context, r *providerv1.TransferOwnerRequest) (*providerv1.TransferOwnerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfer = proto.Clone(r).(*providerv1.TransferOwnerRequest)
	if s.transferErr != nil {
		return nil, s.transferErr
	}
	return &providerv1.TransferOwnerResponse{}, nil
}

func (s *slice4Server) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{SupportsClustering: true, SupportsRoutedAdoption: true}, nil
}

// TestClient_ListVMs_CarriesHostIDsAndUnreachableHosts: the manager's client
// keeps each VM's host and the hosts the provider could not list.
func TestClient_ListVMs_CarriesHostIDsAndUnreachableHosts(t *testing.T) {
	srv := &slice4Server{list: &providerv1.ListVMsResponse{
		Vms: []*providerv1.VMInfo{
			{Id: "web", Name: "web", HostId: "host-a", ProviderRaw: map[string]string{"uuid": "u-a"}},
			{Id: "web", Name: "web", HostId: "host-b", ProviderRaw: map[string]string{"uuid": "u-b"}},
		},
		UnreachableHostIds: []string{"host-c"},
	}}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")

	list, err := c.ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, list.VMs, 2)
	assert.Equal(t, "host-a", list.VMs[0].HostID)
	assert.Equal(t, "host-b", list.VMs[1].HostID)
	assert.Equal(t, []string{"host-c"}, list.UnreachableHostIDs)

	caps, err := c.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, caps.SupportsRoutedAdoption)
}

// TestClient_ListVMs_SingleHostHasNoHosts: a single-host provider's answer
// maps to VMs without a host and no unreachable host.
func TestClient_ListVMs_SingleHostHasNoHosts(t *testing.T) {
	srv := &slice4Server{list: &providerv1.ListVMsResponse{Vms: []*providerv1.VMInfo{{Id: "web", Name: "web"}}}}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()

	list, err := newTestClient(t, dialer, "libvirt").ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, list.VMs, 1)
	assert.Empty(t, list.VMs[0].HostID)
	assert.Empty(t, list.UnreachableHostIDs)
}

// TestClient_TransferOwner_ThreadsTheRequestAndMapsErrors: every field
// reaches the provider, and its refusals come back typed.
func TestClient_TransferOwner_ThreadsTheRequestAndMapsErrors(t *testing.T) {
	srv := &slice4Server{}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")

	req := contracts.TransferOwnerRequest{
		VM: contracts.VMRef{ID: "web", HostID: "host-a",
			Owner: contracts.ObjectIdentity{UID: "uid-1", Namespace: "infra", Name: "web-0123456789"}},
		ReplaceableOwnerUIDs: []string{"uid-dead"},
		ExpectedUUID:         "11111111-2222-4333-8444-555555555555",
	}
	require.NoError(t, c.TransferOwner(context.Background(), req))
	srv.mu.Lock()
	got := srv.transfer
	srv.mu.Unlock()
	assert.Equal(t, "web", got.GetId())
	assert.Equal(t, "host-a", got.GetTargetHostId())
	assert.Equal(t, "uid-1", got.GetOwner().GetUid())
	assert.Equal(t, "infra", got.GetOwner().GetNamespace())
	assert.Equal(t, "web-0123456789", got.GetOwner().GetName())
	assert.Equal(t, []string{"uid-dead"}, got.GetReplaceableOwnerUids())
	assert.Equal(t, req.ExpectedUUID, got.GetExpectedUuid())

	srv.transferErr = status.Error(codes.AlreadyExists, "owned by another VirtualMachine")
	err := c.TransferOwner(context.Background(), req)
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "%v", err)

	srv.transferErr = status.Error(codes.Unimplemented, "single-host")
	err = c.TransferOwner(context.Background(), req)
	require.Error(t, err)
	assert.True(t, contracts.IsNotSupported(err), "%v", err)
}
