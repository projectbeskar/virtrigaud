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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// ListHosts is the gRPC host-inventory RPC (ADR-0007 P1). libvirt is the first
// hypervisor slated to become a clustered ("brain-in-operator") provider, but
// the real N-host implementation — projecting a host set and reading `virsh
// nodeinfo` / `pool-info` / `domcapabilities` per host — lands in a later
// ADR-0007 P1 PR. Until then it reports Unimplemented and GetCapabilities
// advertises supports_clustering = false (D7, honesty-first).
func (s *Server) ListHosts(ctx context.Context, req *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	return nil, errors.NewUnimplemented("ListHosts")
}

// GetHostInfo is the gRPC single-host refresh RPC (ADR-0007 P1). Stubbed
// Unimplemented pending the real libvirt host-inventory implementation; see
// ListHosts.
func (s *Server) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	return nil, errors.NewUnimplemented("GetHostInfo")
}

// ListHosts keeps the in-process libvirt *Provider a full contracts.Provider
// (the providerBackend seam embeds it). The gRPC Server stubs the RPC directly
// with Unimplemented for this contract-only PR, so this backend method is not
// yet wired to any host querying; the real implementation lands in a later
// ADR-0007 P1 PR.
func (p *Provider) ListHosts(ctx context.Context) ([]contracts.HostInfo, error) {
	return nil, contracts.NewNotSupportedError("ListHosts is not yet implemented on the libvirt provider (ADR-0007 P1)")
}

// GetHostInfo keeps the in-process libvirt *Provider a full contracts.Provider.
// See ListHosts for why this is a stub in this PR.
func (p *Provider) GetHostInfo(ctx context.Context, hostID string) (contracts.HostInfo, error) {
	return contracts.HostInfo{}, contracts.NewNotSupportedError("GetHostInfo is not yet implemented on the libvirt provider (ADR-0007 P1)")
}
