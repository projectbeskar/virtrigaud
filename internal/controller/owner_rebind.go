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

package controller

import (
	"context"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// providerRebindsOwnedVM reports whether instance's Create and Clone, sent
// again for the same owner, return the VM an earlier attempt made instead of
// making a second one: the provider reports
// GetCapabilitiesResponse.supports_owner_rebind (rc2 R4). It is the only thing
// that lets the manager send a Create or Clone whose outcome is unknown again.
//
// A provider that does not report capabilities, or reports the capability
// false — every provider image that predates the field, including a Proxmox
// image that ignores CloneRequest.target_vm and would make a second VMID — is
// never sent one again. An error means the capability could not be read: the
// caller sends nothing on that answer.
func providerRebindsOwnedVM(ctx context.Context, instance contracts.Provider) (bool, error) {
	reporter, ok := instance.(contracts.CapabilityReporter)
	if !ok {
		return false, nil
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return false, fmt.Errorf("read the provider's capabilities (supports_owner_rebind): %w", err)
	}
	return caps.SupportsOwnerRebind, nil
}
