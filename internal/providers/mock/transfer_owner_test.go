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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// listedOwner returns the owner UID the mock's ListVMs reports for id.
func listedOwner(t *testing.T, p *Provider, id string) string {
	t.Helper()
	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	for _, vm := range resp.GetVms() {
		if vm.GetId() == id {
			return vm.GetProviderRaw()[contracts.VMInfoOwnerUIDKey]
		}
	}
	t.Fatalf("VM %q not listed", id)
	return ""
}

// TestTransferOwner_CompareAndSwap: the mock stamps an unstamped VM, accepts an
// idempotent retry, refuses another owner unless its UID is replaceable, and
// reports the stamp in ListVMs; it advertises supports_owner_transfer.
func TestTransferOwner_CompareAndSwap(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()
	first := &providerv1.ObjectIdentity{Uid: "11111111-1111-4111-8111-111111111111", Namespace: "a", Name: "demo-vm-1"}
	second := &providerv1.ObjectIdentity{Uid: "22222222-2222-4222-8222-222222222222", Namespace: "b", Name: "demo-vm-1"}

	_, err := p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "vm-1", Owner: first})
	require.NoError(t, err)
	assert.Equal(t, first.GetUid(), listedOwner(t, p, "vm-1"))
	_, err = p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "vm-1", Owner: first})
	require.NoError(t, err, "idempotent")

	_, err = p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "vm-1", Owner: second})
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.NotContains(t, err.Error(), first.GetUid())
	_, err = p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "vm-1", Owner: second,
		ReplaceableOwnerUids: []string{first.GetUid()}})
	require.NoError(t, err)
	assert.Equal(t, second.GetUid(), listedOwner(t, p, "vm-1"))

	_, err = p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "nope", Owner: first})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "vm-2", Owner: first, TargetHostId: "h"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	caps, err := p.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerTransfer())
}
