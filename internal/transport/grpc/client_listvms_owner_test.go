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
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the manager side of the ADR-0007 A6.2 wire: the owner
// filter reaches the provider as ListVMsRequest.owner_namespace / owner_name,
// the answer's owner_filter_applied mark and the supports_list_owner_filter
// capability are carried through, and an incomplete filter is never sent.

// ownerFilterServer answers ListVMs with a canned response and records the
// request.
type ownerFilterServer struct {
	providerv1.UnimplementedProviderServer
	list *providerv1.ListVMsResponse
	caps *providerv1.GetCapabilitiesResponse

	mu    sync.Mutex
	calls []*providerv1.ListVMsRequest
}

func (s *ownerFilterServer) ListVMs(_ context.Context, r *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := proto.Clone(r).(*providerv1.ListVMsRequest); ok {
		s.calls = append(s.calls, c)
	}
	return s.list, nil
}

func (s *ownerFilterServer) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return s.caps, nil
}

func (s *ownerFilterServer) requests() []*providerv1.ListVMsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*providerv1.ListVMsRequest(nil), s.calls...)
}

// TestClient_ListVMsForOwner_SendsTheFilterAndMapsTheMark: the filter is sent,
// and the answer's VMs, unreachable hosts and owner_filter_applied mark come
// back; the capability maps.
func TestClient_ListVMsForOwner_SendsTheFilterAndMapsTheMark(t *testing.T) {
	srv := &ownerFilterServer{
		list: &providerv1.ListVMsResponse{
			Vms: []*providerv1.VMInfo{{Id: "team-a.web", Name: "team-a.web", HostId: "host-a",
				OwnerNamespace: "team-a", OwnerName: "web",
				ProviderRaw: map[string]string{contracts.VMInfoOwnerUIDKey: "uid-old"}}},
			UnreachableHostIds: []string{"host-c"},
			OwnerFilterApplied: true,
		},
		caps: &providerv1.GetCapabilitiesResponse{SupportsClustering: true, SupportsListOwnerFilter: true},
	}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")

	var lister contracts.OwnerFilteredLister = c
	list, err := lister.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	assert.True(t, list.OwnerFilterApplied)
	require.Len(t, list.VMs, 1)
	assert.Equal(t, "host-a", list.VMs[0].HostID)
	assert.Equal(t, "team-a", list.VMs[0].OwnerNamespace)
	assert.Equal(t, "web", list.VMs[0].OwnerName)
	assert.Equal(t, []string{"uid-old"}, contracts.OwnerUIDs(list.VMs[0]))
	assert.Equal(t, []string{"host-c"}, list.UnreachableHostIDs)

	reqs := srv.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "team-a", reqs[0].GetOwnerNamespace())
	assert.Equal(t, "web", reqs[0].GetOwnerName())

	caps, err := c.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, caps.SupportsListOwnerFilter)
}

// TestClient_ListVMsForOwner_UnmarkedAnswerStaysUnmarked: a provider that
// ignored the filter (older, or not clustered) answers without the mark, and
// the caller sees OwnerFilterApplied=false; an unfiltered ListVMs sends no
// filter and is never marked.
func TestClient_ListVMsForOwner_UnmarkedAnswerStaysUnmarked(t *testing.T) {
	srv := &ownerFilterServer{
		list: &providerv1.ListVMsResponse{Vms: []*providerv1.VMInfo{{Id: "web"}, {Id: "db"}}},
		caps: &providerv1.GetCapabilitiesResponse{},
	}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")

	list, err := c.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	assert.False(t, list.OwnerFilterApplied, "an answer without the provider's mark is not a filtered answer")
	assert.Len(t, list.VMs, 2)

	plain, err := c.ListVMs(context.Background())
	require.NoError(t, err)
	assert.False(t, plain.OwnerFilterApplied)
	reqs := srv.requests()
	require.Len(t, reqs, 2)
	assert.Empty(t, reqs[1].GetOwnerNamespace(), "an unfiltered ListVMs sends no filter")
	assert.Empty(t, reqs[1].GetOwnerName())

	caps, err := c.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.False(t, caps.SupportsListOwnerFilter, "false from a provider that does not report it")
}

// TestClient_ListVMsForOwner_IncompleteFilterIsNotSent: a filter with only a
// namespace or only a name is refused before any call.
func TestClient_ListVMsForOwner_IncompleteFilterIsNotSent(t *testing.T) {
	srv := &ownerFilterServer{list: &providerv1.ListVMsResponse{}, caps: &providerv1.GetCapabilitiesResponse{}}
	dialer, cleanup := startBufconnServer(t, srv)
	defer cleanup()
	c := newTestClient(t, dialer, "libvirt")

	for _, f := range []contracts.OwnerFilter{{Namespace: "team-a"}, {Name: "web"}, {}} {
		_, err := c.ListVMsForOwner(context.Background(), f)
		require.Error(t, err, "%+v", f)
		assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
	}
	assert.Empty(t, srv.requests(), "nothing was sent")
}
