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
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fakeCreateBackend is a providerBackend whose Create records the request it
// was handed and returns a scripted result. Every other method is unused here.
type fakeCreateBackend struct {
	providerBackend // embedded (nil): only Create is exercised

	gotReq contracts.CreateRequest
	resp   contracts.CreateResponse
	err    error
}

func (f *fakeCreateBackend) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	f.gotReq = req
	return f.resp, f.err
}

func TestServerParseCreateRequest_Owner(t *testing.T) {
	s := &Server{}

	got, err := s.parseCreateRequest(&providerv1.CreateRequest{
		Name:  "web",
		Owner: &providerv1.ObjectIdentity{Uid: ownerTeamA.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name},
	})
	require.NoError(t, err)
	assert.Equal(t, ownerTeamA, got.Owner)

	got, err = s.parseCreateRequest(&providerv1.CreateRequest{Name: "web"})
	require.NoError(t, err)
	assert.True(t, got.Owner.IsZero(), "an absent owner (older manager) must stay zero, never be inferred")
}

func TestCreateRPCError(t *testing.T) {
	conflict := createRPCError(contracts.NewConflictError(`libvirt domain "web" already exists`, errors.New("internal detail")))
	assert.Equal(t, codes.AlreadyExists, status.Code(conflict))
	assert.Equal(t, `libvirt domain "web" already exists`, status.Convert(conflict).Message(),
		"only the categorized message crosses the wire, not the cause chain")

	invalid := createRPCError(contracts.NewInvalidSpecError(`name "12" cannot be used`, nil))
	assert.Equal(t, codes.InvalidArgument, status.Code(invalid))

	// Everything else keeps the historical wrapped (non-status) form.
	transient := createRPCError(contracts.NewRetryableError("failed to list existing domains", nil))
	assert.Equal(t, codes.Unknown, status.Code(transient))
	assert.Contains(t, transient.Error(), "failed to create VM")
}

// startLibvirtGRPC serves a libvirt Server over a real loopback gRPC listener
// and returns a MANAGER-side transport client dialed to it — the production
// client the VirtualMachine controller uses — so a test observes exactly what
// crosses the wire in both directions.
func startLibvirtGRPC(t *testing.T, backend providerBackend) *transportgrpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, NewServer(backend))
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	c, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "libvirt", "owner-roundtrip", nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestCreate_OwnerRoundTripsManagerToProvider pins the whole contract hop: the
// manager's contracts.CreateRequest.Owner reaches the provider backend intact,
// and the provider's non-retryable rejections come back typed.
func TestCreate_OwnerRoundTripsManagerToProvider(t *testing.T) {
	t.Run("owner delivered", func(t *testing.T) {
		b := &fakeCreateBackend{resp: contracts.CreateResponse{ID: "web"}}
		c := startLibvirtGRPC(t, b)

		resp, err := c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
		require.NoError(t, err)
		assert.Equal(t, "web", resp.ID)
		assert.Equal(t, ownerTeamA, b.gotReq.Owner)
	})

	t.Run("conflict comes back as a non-retryable Conflict", func(t *testing.T) {
		b := &fakeCreateBackend{err: contracts.NewConflictError(`libvirt domain "web" already exists on the host and is not owned by this VirtualMachine`, nil)}
		c := startLibvirtGRPC(t, b)

		_, err := c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: ownerTeamB})
		require.Error(t, err)
		assert.True(t, contracts.IsConflict(err), "got %v", err)
		var pe *contracts.ProviderError
		require.ErrorAs(t, err, &pe)
		assert.False(t, pe.IsRetryable())
		assert.Contains(t, pe.Message, `libvirt domain "web" already exists`)
	})

	t.Run("invalid spec comes back as InvalidSpec", func(t *testing.T) {
		b := &fakeCreateBackend{err: contracts.NewInvalidSpecError(`name "12" cannot be used as a libvirt domain name`, nil)}
		c := startLibvirtGRPC(t, b)

		_, err := c.Create(context.Background(), contracts.CreateRequest{Name: "12", Owner: ownerTeamA})
		assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
	})
}

// TestCreate_EndToEnd_ForeignDomainRefusedOverTheWire runs the real *Provider
// behind the real gRPC Server against a fake virsh. Domains are namespaced
// ("<namespace>.<name>"), so a foreign domain can only collide by carrying
// exactly this VM's namespaced name — here a previous incarnation of team-a/web
// (another UID): that create reaches the manager as a Conflict with no ID,
// while the owner's retried create binds its own namespaced domain.
//
// Updated deliberately for namespaced naming: it used to show tenant B's "web"
// refused because tenant A owned "web"; that is no longer a collision at all
// (TestCreate_SameNameInAnotherNamespaceIsNoCollision).
func TestCreate_EndToEnd_ForeignDomainRefusedOverTheWire(t *testing.T) {
	installOwnershipFakeVirsh(t, map[string]string{"team-a.web": stampedDomainXML("team-a.web", staleTeamAWeb)})
	c := startLibvirtGRPC(t, &Provider{virshProvider: localTestVirshProvider()})

	resp, err := c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "got %v", err)
	assert.Empty(t, resp.ID)

	installOwnershipFakeVirsh(t, map[string]string{"team-a.web": stampedDomainXML("team-a.web", ownerTeamA)})
	resp, err = c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID, "the manager records the namespaced domain name as status.id")
}
