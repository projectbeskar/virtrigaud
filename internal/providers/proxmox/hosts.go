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

package proxmox

import (
	"context"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// ListHosts is the gRPC host-inventory RPC (ADR-0007 P1). The Proxmox provider
// talks to a single PVE endpoint (the cluster's own API) rather than fronting a
// projected host set for the operator, so it does not participate in the
// operator-driven clustering model. It reports Unimplemented and advertises
// supports_clustering = false (D7, honesty-first).
func (p *Provider) ListHosts(ctx context.Context, req *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	return nil, errors.NewUnimplemented("ListHosts")
}

// GetHostInfo is the gRPC single-host refresh RPC (ADR-0007 P1). Unimplemented
// on Proxmox for the same reason as ListHosts.
func (p *Provider) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	return nil, errors.NewUnimplemented("GetHostInfo")
}
