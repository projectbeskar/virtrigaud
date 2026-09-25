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

package vsphere

import (
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestImageArtifactNameIsASafeVSphereName checks ADR-0009 D1 against this
// provider's own name rule: every vSphere prepared-image artifact name
// (NameRuleVSphere) is accepted by unsafeNameReason — no '/', '\', '%' or ':',
// never MOID-shaped — and fits the 80-character entity-name limit.
func TestImageArtifactNameIsASafeVSphereName(t *testing.T) {
	const digest = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	for _, id := range []contracts.ObjectIdentity{
		{UID: "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a", Namespace: "team-a", Name: "ubuntu-22.04"},
		{UID: "u1", Namespace: "vm", Name: "1"},
		{UID: "u2", Namespace: strings.Repeat("n", 63), Name: strings.Repeat("x", 253)},
	} {
		name, err := imageartifact.ArtifactName(imageartifact.NameRuleVSphere, id, digest)
		if err != nil {
			t.Fatalf("ArtifactName(%+v): %v", id, err)
		}
		if reason := unsafeNameReason(name); reason != "" {
			t.Errorf("artifact name %q is unsafe on vSphere: %s", name, reason)
		}
		if len(name) > 80 {
			t.Errorf("artifact name %q is longer than 80 characters", name)
		}
	}
}
