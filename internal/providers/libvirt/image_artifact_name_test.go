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
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestImageArtifactNameIsNeverReservedOrADomainName checks ADR-0009 D1
// properties 3 and 6 against this provider's own rules: a libvirt
// prepared-image artifact file ("<name>.qcow2", NameRuleLibvirt) is never a
// #334 reserved image name, and its base name is never a name domainNameFor
// can produce (with or without an owner), so an artifact can never be mistaken
// for a VM's disk or domain.
func TestImageArtifactNameIsNeverReservedOrADomainName(t *testing.T) {
	const digest = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	for _, id := range []contracts.ObjectIdentity{
		{UID: "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a", Namespace: "team-a", Name: "ubuntu-22.04"},
		{UID: "u1", Namespace: "a", Name: "b-disk"},
		{UID: "u2", Namespace: "a", Name: "b-migrated"},
		{UID: "u3", Namespace: strings.Repeat("n", 63), Name: strings.Repeat("x", 253)},
		{UID: "u4", Namespace: "default", Name: "vm.download"},
	} {
		name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt, id, digest)
		if err != nil {
			t.Fatalf("ArtifactName(%+v): %v", id, err)
		}
		if file := name + qcow2Ext; reservedImageName(file) {
			t.Errorf("artifact file %q is a reserved image name", file)
		}
		if got, err := domainNameFor(contracts.ObjectIdentity{Namespace: id.Namespace, Name: id.Name}, ""); err == nil && got == name {
			t.Errorf("artifact name %q equals the domain name of %s/%s", name, id.Namespace, id.Name)
		}
		if validLegacyName(name) == nil {
			t.Errorf("artifact name %q is a valid legacy (bare) domain name", name)
		}
	}
}
