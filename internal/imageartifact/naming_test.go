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

package imageartifact

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

const (
	testUID    = "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a"
	testDigest = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	// otherDigest differs from testDigest in its last hex digit.
	otherDigest = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1f"
)

var allRules = []NameRule{NameRuleVSphere, NameRuleLibvirt, NameRuleProxmox}

// testImage returns an image identity with testUID.
func testImage(namespace, name string) contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: testUID, Namespace: namespace, Name: name}
}

// mustName returns ArtifactName(rule, image, digest), failing on error.
func mustName(t *testing.T, rule NameRule, image contracts.ObjectIdentity, digest string) string {
	t.Helper()
	name, err := ArtifactName(rule, image, digest)
	require.NoError(t, err)
	return name
}

// TestArtifactNameGolden pins the ADR-0009 D1 rule per hypervisor. A failure
// means every prepared artifact would be renamed and imported again.
func TestArtifactNameGolden(t *testing.T) {
	img := testImage("team-a", "ubuntu-22.04")
	assert.Equal(t, "team-a.ubuntu-22.04_f06d2f97535bae75", mustName(t, NameRuleVSphere, img, testDigest))
	assert.Equal(t, "team-a.ubuntu-22.04_f06d2f97535bae75", mustName(t, NameRuleLibvirt, img, testDigest))
	assert.Equal(t, "team-a.ubuntu-22.04-f06d2f97535bae75", mustName(t, NameRuleProxmox, img, testDigest))
}

// TestNameRuleParameters pins the per-hypervisor budget and separator table of
// ADR-0009 D1.
func TestNameRuleParameters(t *testing.T) {
	for _, tc := range []struct {
		rule NameRule
		max  int
		sep  string
		name string
	}{
		{NameRuleVSphere, 80, "_", "vsphere"},
		{NameRuleLibvirt, 200, "_", "libvirt"},
		{NameRuleProxmox, 80, "-", "proxmox"},
		{NameRule(0), 0, "", "NameRule(0)"},
	} {
		assert.Equal(t, tc.max, tc.rule.MaxBytes(), tc.name)
		assert.Equal(t, tc.sep, tc.rule.Separator(), tc.name)
		assert.Equal(t, tc.name, tc.rule.String())
	}
	_, err := ArtifactName(NameRule(0), testImage("a", "b"), testDigest)
	assert.Error(t, err, "an unknown rule names nothing")
}

// TestArtifactNameTruncation verifies long identities are cut to the budget,
// keep the namespace whole, trim a trailing '.' or '-' before the hash, and
// stay distinct.
func TestArtifactNameTruncation(t *testing.T) {
	longNS := strings.Repeat("n", 63)
	longName := strings.Repeat("a", 250)
	for _, rule := range allRules {
		t.Run(rule.String(), func(t *testing.T) {
			name := mustName(t, rule, testImage(longNS, longName), testDigest)
			assert.LessOrEqual(t, len(name), rule.MaxBytes())
			assert.Equal(t, rule.MaxBytes(), len(name), "a long identity fills the budget exactly")
			assert.True(t, strings.HasPrefix(name, longNS), "the namespace (at most 63 bytes) is always kept whole")

			// Two long names that share the kept prefix stay distinct through the hash.
			other := testImage(longNS, longName+"b")
			other.UID = "6a1d7b8f-3e1c-4b9f-8b64-1a2b3c4d5e6f"
			assert.NotEqual(t, name, mustName(t, rule, other, testDigest))
		})
	}

	// The cut lands on '-': it is trimmed, so no segment ends in '-' before
	// the separator. vSphere keeps 80-1-16 = 63 prefix bytes: "ns." + 59 + '-'.
	name := mustName(t, NameRuleVSphere, testImage("ns", strings.Repeat("a", 59)+"-"+strings.Repeat("b", 10)), testDigest)
	assert.Equal(t, "ns."+strings.Repeat("a", 59)+"_", name[:len(name)-16])

	// The cut lands on '.': trimmed too.
	name = mustName(t, NameRuleProxmox, testImage("ns", strings.Repeat("a", 59)+".b"), testDigest)
	assert.Equal(t, "ns."+strings.Repeat("a", 59)+"-", name[:len(name)-16])

	// A 63-byte namespace fills the vSphere prefix: the '.' is cut and the
	// name is the namespace, the separator and the hash.
	name = mustName(t, NameRuleVSphere, testImage(longNS, "x"), testDigest)
	assert.Equal(t, longNS+"_", name[:len(name)-16])
}

// TestArtifactNameIdentitySensitivity verifies the UID and the digest are in
// the name (ADR-0009 D1 properties 1, 2): a re-created VMImage (new UID) or a
// changed source (new digest) gets a new artifact name, while the same
// identity always gets the same name.
func TestArtifactNameIdentitySensitivity(t *testing.T) {
	for _, rule := range allRules {
		base := mustName(t, rule, testImage("team-a", "ubuntu"), testDigest)
		assert.Equal(t, base, mustName(t, rule, testImage("team-a", "ubuntu"), testDigest), "deterministic")

		recreated := testImage("team-a", "ubuntu")
		recreated.UID = "0d4e7c1a-9f3b-4c2d-8e5a-6b7c8d9e0f1a"
		assert.NotEqual(t, base, mustName(t, rule, recreated, testDigest), "a re-created VMImage gets a new artifact")
		assert.NotEqual(t, base, mustName(t, rule, testImage("team-a", "ubuntu"), otherDigest), "a changed source gets a new artifact")

		// Same name in two namespaces: distinct prefixes, distinct names.
		assert.NotEqual(t, base, mustName(t, rule, testImage("team-b", "ubuntu"), testDigest))
		// The namespace boundary is unambiguous: "team.a-ubuntu" != "team-a.ubuntu".
		assert.NotEqual(t, base, mustName(t, rule, testImage("team", "a-ubuntu"), testDigest))
	}
}

var (
	// hashSuffix matches the separator-independent tail of every name.
	hashSuffix = regexp.MustCompile(`[0-9a-f]{16}$`)
	// charsetUnderscore is the alphabet of a '_'-rule name.
	charsetUnderscore = regexp.MustCompile(`^[a-z0-9._-]+$`)
	// pveDNSName is PVE's "dns-name" format for a VM name.
	pveDNSName = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.)*[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?$`)
	// vcenterMoID is the shape of a vCenter VM managed object ID.
	vcenterMoID = regexp.MustCompile(`^vm-[0-9]+$`)
	// libvirtReservedSuffixes are the #334 reserved file-name suffixes.
	libvirtReservedSuffixes = []string{"-disk.qcow2", "-migrated.qcow2", ".download", ".partial", "-disk-temp.img"}
)

// propertyIdentities returns identities that exercise short, dotted, long and
// boundary-length namespaces and names. Every identity has its own UID, as
// every object in a cluster does (short, UUID-shaped and 128-byte UIDs).
func propertyIdentities() []contracts.ObjectIdentity {
	namespaces := []string{"a", "team-a", "default", strings.Repeat("n", 62) + "x", "x" + strings.Repeat("-y", 31)}
	names := []string{"a", "ubuntu-22.04", "a.b.c", "vm-1", strings.Repeat("a", 253),
		strings.Repeat("a.", 100) + "b", strings.Repeat("a-", 40) + "b", "0"}
	uidShapes := []func(int) string{
		func(i int) string { return fmt.Sprintf("u%d", i) },
		func(i int) string { return fmt.Sprintf("5f0c6a7e-2d0b-4a8e-9a53-%012d", i) },
		func(i int) string { return strings.Repeat("F", 120) + fmt.Sprintf("%08d", i) },
	}
	var out []contracts.ObjectIdentity
	for _, ns := range namespaces {
		for _, n := range names {
			for _, uid := range uidShapes {
				out = append(out, contracts.ObjectIdentity{UID: uid(len(out)), Namespace: ns, Name: n})
			}
		}
	}
	return out
}

// TestArtifactNameProperties checks the ADR-0009 D1 properties over many
// identities, per hypervisor: within the budget, in the charset, always ending
// in the separator and 16 hex digits, and — under the '_' rules — never equal
// to a Kubernetes (DNS-1123) name or a "<namespace>.<name>" domain name, never
// MOID-shaped (vSphere), and never a #334 reserved or hidden file (libvirt).
func TestArtifactNameProperties(t *testing.T) {
	for _, rule := range allRules {
		t.Run(rule.String(), func(t *testing.T) {
			seen := map[string]string{}
			for _, id := range propertyIdentities() {
				for _, digest := range []string{testDigest, otherDigest} {
					name := mustName(t, rule, id, digest)
					desc := fmt.Sprintf("%s %+v %s -> %q", rule, id, digest, name)

					require.LessOrEqual(t, len(name), rule.MaxBytes(), desc)
					require.True(t, hashSuffix.MatchString(name), desc)
					require.Equal(t, rule.Separator(), name[len(name)-17:len(name)-16], desc)
					require.True(t, strings.HasPrefix(name, id.Namespace), "namespace kept whole: "+desc)
					require.False(t, strings.HasSuffix(name[:len(name)-17], "."), desc)
					require.False(t, strings.HasSuffix(name[:len(name)-17], "-"), desc)

					switch rule {
					case NameRuleVSphere, NameRuleLibvirt:
						require.True(t, charsetUnderscore.MatchString(name), desc)
						require.Contains(t, name, "_", desc)
						require.NotEmpty(t, validation.IsDNS1123Subdomain(name), "never a Kubernetes name: "+desc)
						require.NotEqual(t, id.Namespace+"."+id.Name, name, desc)
					case NameRuleProxmox:
						require.True(t, pveDNSName.MatchString(name), "a PVE dns-name: "+desc)
						require.NotContains(t, name, "_", desc)
					}
					if rule == NameRuleVSphere {
						require.False(t, vcenterMoID.MatchString(name), desc)
						require.False(t, strings.ContainsAny(name, `/\%:`), desc)
					}
					if rule == NameRuleLibvirt {
						file := name + ".qcow2"
						require.False(t, strings.HasPrefix(file, "."), "never a dotfile: "+desc)
						for _, suffix := range libvirtReservedSuffixes {
							require.False(t, strings.HasSuffix(file, suffix), desc)
						}
						require.LessOrEqual(t, len(file), 255, desc)
					}

					// Distinct (identity, digest) pairs never share a name.
					if prev, dup := seen[name]; dup {
						t.Fatalf("collision: %s and %s", desc, prev)
					}
					seen[name] = desc
				}
			}
		})
	}
}

// TestArtifactNameRejectsInvalidIdentity verifies a malformed identity or
// digest never names an artifact.
func TestArtifactNameRejectsInvalidIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		image  contracts.ObjectIdentity
		digest string
	}{
		"empty uid":             {contracts.ObjectIdentity{Namespace: "a", Name: "b"}, testDigest},
		"uid with slash":        {contracts.ObjectIdentity{UID: "a/b", Namespace: "a", Name: "b"}, testDigest},
		"uid with space":        {contracts.ObjectIdentity{UID: "a b", Namespace: "a", Name: "b"}, testDigest},
		"uid too long":          {contracts.ObjectIdentity{UID: strings.Repeat("a", 129), Namespace: "a", Name: "b"}, testDigest},
		"empty namespace":       {contracts.ObjectIdentity{UID: testUID, Name: "b"}, testDigest},
		"namespace with dot":    {contracts.ObjectIdentity{UID: testUID, Namespace: "a.b", Name: "b"}, testDigest},
		"namespace uppercase":   {contracts.ObjectIdentity{UID: testUID, Namespace: "A", Name: "b"}, testDigest},
		"namespace too long":    {contracts.ObjectIdentity{UID: testUID, Namespace: strings.Repeat("a", 64), Name: "b"}, testDigest},
		"empty name":            {contracts.ObjectIdentity{UID: testUID, Namespace: "a"}, testDigest},
		"name with underscore":  {contracts.ObjectIdentity{UID: testUID, Namespace: "a", Name: "b_c"}, testDigest},
		"name with slash":       {contracts.ObjectIdentity{UID: testUID, Namespace: "a", Name: "b/c"}, testDigest},
		"name with leading dot": {contracts.ObjectIdentity{UID: testUID, Namespace: "a", Name: ".b"}, testDigest},
		"missing digest":        {testImage("a", "b"), ""},
		"malformed digest":      {testImage("a", "b"), "sha256:abc"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, rule := range allRules {
				_, err := ArtifactName(rule, tc.image, tc.digest)
				assert.Error(t, err, rule.String())
			}
		})
	}
}
