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

package client

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// slice4ProviderClient is a providerv1.ProviderClient answering ListVMs and
// TransferOwner (ADR-0007 Addendum A, slice 4); every other method panics
// (nil embedded interface) and is never called here.
type slice4ProviderClient struct {
	providerv1.ProviderClient
	list     *providerv1.ListVMsResponse
	transfer *providerv1.TransferOwnerRequest
}

func (f *slice4ProviderClient) ListVMs(context.Context, *providerv1.ListVMsRequest, ...grpc.CallOption) (*providerv1.ListVMsResponse, error) {
	return f.list, nil
}

func (f *slice4ProviderClient) TransferOwner(_ context.Context, r *providerv1.TransferOwnerRequest, _ ...grpc.CallOption) (*providerv1.TransferOwnerResponse, error) {
	f.transfer = r
	return &providerv1.TransferOwnerResponse{}, nil
}

// TestListVMsResponse_KeepsUnreachableHosts: ListVMsResponse returns the
// whole answer (host ids and unreachable hosts); ListVMs its VMs.
func TestListVMsResponse_KeepsUnreachableHosts(t *testing.T) {
	fake := &slice4ProviderClient{list: &providerv1.ListVMsResponse{
		Vms:                []*providerv1.VMInfo{{Id: "web", HostId: "host-a"}},
		UnreachableHostIds: []string{"host-b"},
	}}
	c := &Client{config: &Config{}, client: fake}

	resp, err := c.ListVMsResponse(context.Background())
	if err != nil {
		t.Fatalf("ListVMsResponse: %v", err)
	}
	if got := resp.GetUnreachableHostIds(); len(got) != 1 || got[0] != "host-b" {
		t.Errorf("unreachable_host_ids = %v, want [host-b]", got)
	}
	vms, err := c.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 1 || vms[0].GetHostId() != "host-a" {
		t.Errorf("ListVMs = %v, want one VM on host-a", vms)
	}
}

// TestTransferOwner_SendsTheRequest: TransferOwner forwards the request.
func TestTransferOwner_SendsTheRequest(t *testing.T) {
	fake := &slice4ProviderClient{}
	c := &Client{config: &Config{}, client: fake}
	req := &providerv1.TransferOwnerRequest{Id: "web", TargetHostId: "host-a", ExpectedUuid: "u"}
	if _, err := c.TransferOwner(context.Background(), req); err != nil {
		t.Fatalf("TransferOwner: %v", err)
	}
	if fake.transfer != req {
		t.Errorf("the request was not forwarded")
	}
}
