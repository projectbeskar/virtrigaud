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

package capabilities

import (
	"context"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestListOwnerFilter_Capability pins the SDK side of ADR-0007 A6.2: the
// list_owner_filter capability reaches
// GetCapabilitiesResponse.supports_list_owner_filter, and is not advertised
// unless a provider adds it.
func TestListOwnerFilter_Capability(t *testing.T) {
	caps, err := NewManager().AddCapability(CapabilityListOwnerFilter).
		GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !caps.GetSupportsListOwnerFilter() {
		t.Error("CapabilityListOwnerFilter must advertise supports_list_owner_filter")
	}
	plain, err := NewBuilder().Core().Build().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.GetSupportsListOwnerFilter() {
		t.Error("not advertised unless a provider adds it")
	}
}
