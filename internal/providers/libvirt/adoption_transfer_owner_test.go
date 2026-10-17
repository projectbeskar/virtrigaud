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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin TransferOwner on a SINGLE-HOST provider (single-host
// adoption re-stamps the domain it adopts): the clustered compare-and-swap
// core on the provider's one host, addressed without a host id.

// singleHostOf is a single-host provider on the fake host "host-a" of a
// listFixture.
func singleHostOf() *Provider {
	return &Provider{virshProvider: localHostVP("host-a")}
}

// singleHostTransferReq is the manager's single-host TransferOwner request
// for "web": no target host.
func singleHostTransferReq(replaceable ...string) *providerv1.TransferOwnerRequest {
	req := transferReq(replaceable...)
	req.TargetHostId = ""
	return req
}

// listedOwner returns the owner UID the single-host ListVMs reports for id.
func listedOwner(t *testing.T, p *Provider, id string) string {
	t.Helper()
	resp, err := NewServer(p).ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	for _, vm := range resp.GetVms() {
		if vm.GetId() == id {
			return vm.GetProviderRaw()[contracts.VMInfoOwnerUIDKey]
		}
	}
	t.Fatalf("domain %q not listed", id)
	return ""
}

// TestSingleHost_Capabilities_AdvertiseOwnerTransfer: an initialized
// single-host provider reports supports_owner_transfer (and still not
// supports_routed_adoption); a clustered one reports routed adoption only.
func TestSingleHost_Capabilities_AdvertiseOwnerTransfer(t *testing.T) {
	caps, err := NewServer(singleHostOf()).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerTransfer())
	assert.False(t, caps.GetSupportsRoutedAdoption())

	caps, err = NewServer(clusterOf(t, []string{"host-a"})).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.False(t, caps.GetSupportsOwnerTransfer())
	assert.True(t, caps.GetSupportsRoutedAdoption())
}

// TestSingleHost_TransferOwner_StampsAnUnstampedDomain: the domain the
// single-host listing reported unstamped is stamped with the adopter, by
// UUID, and the next listing reports the adopter as its owner — so another
// Provider object's adoption sees it as owned.
func TestSingleHost_TransferOwner_StampsAnUnstampedDomain(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.scriptStamp("host-a", dom, adopter)
	p := singleHostOf()
	require.Empty(t, listedOwner(t, p, "web"))

	_, err := NewServer(p).TransferOwner(context.Background(), singleHostTransferReq())
	require.NoError(t, err)

	assert.Equal(t, []string{
		"host-a metadata --domain " + uuidWebA + " --uri " + ownerMetadataNamespaceURI + " --key " + ownerMetadataPrefix +
			" --set <owner uid='" + adopter.UID + "' namespace='infra' name='web-3f2a9c1b'/> --config",
	}, metadataCalls(fx))
	assert.Equal(t, adopter.UID, listedOwner(t, p, "web"))

	// An idempotent retry writes nothing.
	_, err = NewServer(p).TransferOwner(context.Background(), singleHostTransferReq())
	require.NoError(t, err)
	assert.Len(t, metadataCalls(fx), 1)
}

// TestSingleHost_TransferOwner_ReplacesOnlyAReplaceableStamp: a domain left
// stamped by a detached VirtualMachine is re-stamped only when the manager
// names that stamp replaceable; otherwise it is refused (AlreadyExists, a
// message naming no other owner) and nothing is written.
func TestSingleHost_TransferOwner_ReplacesOnlyAReplaceableStamp(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA, owner: staleOwner}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.scriptStamp("host-a", dom, adopter)
	p := singleHostOf()

	_, err := NewServer(p).TransferOwner(context.Background(), singleHostTransferReq())
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.NotContains(t, err.Error(), staleOwner.UID)
	assert.NotContains(t, err.Error(), staleOwner.Namespace)
	assert.Empty(t, metadataCalls(fx), "a stamp the manager did not report gone is never overwritten")

	_, err = NewServer(p).TransferOwner(context.Background(), singleHostTransferReq(staleOwner.UID))
	require.NoError(t, err)
	assert.Equal(t, adopter.UID, listedOwner(t, p, "web"))
}

// TestSingleHost_TransferOwner_RefusesAReplacedDomainOrAHost: a domain whose
// UUID is not the listed one (replaced since the listing) is NotFound, a
// request naming a host is InvalidArgument; neither writes anything.
func TestSingleHost_TransferOwner_RefusesAReplacedDomainOrAHost(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.scriptStamp("host-a", dom, adopter)
	p := singleHostOf()

	replaced := singleHostTransferReq()
	replaced.ExpectedUuid = uuidWebB
	_, err := NewServer(p).TransferOwner(context.Background(), replaced)
	assert.Equal(t, codes.NotFound, status.Code(err))

	withHost := singleHostTransferReq()
	withHost.TargetHostId = "host-a"
	_, err = NewServer(p).TransferOwner(context.Background(), withHost)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	noUUID := singleHostTransferReq()
	noUUID.ExpectedUuid = ""
	_, err = NewServer(p).TransferOwner(context.Background(), noUUID)
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "the single-host listing reports a UUID, so one is required")

	assert.Empty(t, metadataCalls(fx))
}
