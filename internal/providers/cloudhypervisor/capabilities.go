//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// GetCapabilities implements providerv1.ProviderServer: the Provider fronts
// several hosts, whose agents verify an operation grant on every mutating
// call, and it refuses a mutating call that carries none. It claims nothing
// else: snapshots, clones, disk export and import, image preparation and
// online reconfiguration are not served by a per-host agent, and those
// methods answer Unimplemented.
func (p *Provider) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsClustering:        true,
		SupportsOperationIdentity: true,
		RequiresOperationIdentity: true,
	}, nil
}
