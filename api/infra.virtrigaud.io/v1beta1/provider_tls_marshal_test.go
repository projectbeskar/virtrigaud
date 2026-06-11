/*
Copyright 2025.

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

package v1beta1

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProviderTLSSpec_EnabledFalseSurvivesMarshal guards the defaulted-bool
// footgun: ProviderTLSSpec.Enabled defaults to true, so if its JSON tag carried
// `omitempty` an explicit `false` would be dropped on serialization (e.g. a
// controller Update), the apiserver would re-apply the default, and tls.enabled
// would silently flip false→true — wedging an explicitly-plaintext provider into
// runtime Failed. The field must therefore always serialize.
func TestProviderTLSSpec_EnabledFalseSurvivesMarshal(t *testing.T) {
	b, err := json.Marshal(ProviderTLSSpec{Enabled: false})
	if err != nil {
		t.Fatalf("marshal ProviderTLSSpec: %v", err)
	}
	if !strings.Contains(string(b), `"enabled":false`) {
		t.Fatalf("ProviderTLSSpec.Enabled=false was dropped on marshal (omitempty footgun); got %s", b)
	}

	// And it must round-trip back to false.
	var got ProviderTLSSpec
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Enabled {
		t.Fatalf("ProviderTLSSpec.Enabled round-tripped to true; want false")
	}
}

// TestProviderHealthCheck_EnabledFalseSurvivesMarshal mirrors the guard for the
// other defaulted bool in the same spec family.
func TestProviderHealthCheck_EnabledFalseSurvivesMarshal(t *testing.T) {
	b, err := json.Marshal(ProviderHealthCheck{Enabled: false})
	if err != nil {
		t.Fatalf("marshal ProviderHealthCheck: %v", err)
	}
	if !strings.Contains(string(b), `"enabled":false`) {
		t.Fatalf("ProviderHealthCheck.Enabled=false was dropped on marshal (omitempty footgun); got %s", b)
	}
}
