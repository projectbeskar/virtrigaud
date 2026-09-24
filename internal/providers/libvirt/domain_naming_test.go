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
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the libvirt domain naming rule: a new domain is named
// "<namespace>.<name>", bounded to maxDomainNameBytes by a deterministic
// truncate-and-hash, and a request without a naming identity keeps the legacy
// bare name (still subject to the ID/UUID ambiguity guard).

// namingID is a naming identity (the UID is irrelevant to naming).
func namingID(namespace, name string) contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: "uid-" + namespace + "-" + name, Namespace: namespace, Name: name}
}

// requireInvalidSpec asserts err is a non-retryable InvalidSpec provider error.
func requireInvalidSpec(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, contracts.ErrorTypeInvalidSpec, pe.Type, "%v", err)
	assert.False(t, pe.IsRetryable())
}

func TestDomainNameFor_Table(t *testing.T) {
	long253 := strings.Repeat("a", 253)
	cases := []struct {
		name     string
		owner    contracts.ObjectIdentity
		fallback string
		want     string
	}{
		{"namespaced", namingID("team-a", "web"), "web", "team-a.web"},
		{"same name, other namespace", namingID("team-b", "web"), "web", "team-b.web"},
		{"name with dots", namingID("team-a", "web.prod"), "web.prod", "team-a.web.prod"},
		{"no uid still names", contracts.ObjectIdentity{Namespace: "team-a", Name: "web"}, "web", "team-a.web"},
		{"digits-only VM name is fine namespaced", namingID("team-a", "12"), "12", "team-a.12"},
		{"uuid-shaped VM name is fine namespaced", namingID("team-a", "1b4e28ba-2fa1-11d2-883f-0016d3cca427"), "",
			"team-a.1b4e28ba-2fa1-11d2-883f-0016d3cca427"},
		{"legacy: no owner", contracts.ObjectIdentity{}, "web", "web"},
		{"legacy: uid only", contracts.ObjectIdentity{UID: "u1"}, "web", "web"},
		{"legacy: namespace without name", contracts.ObjectIdentity{Namespace: "team-a"}, "web", "web"},
		{"legacy: name without namespace", contracts.ObjectIdentity{Name: "web"}, "web", "web"},
		{"exactly at the bound", namingID("ns", strings.Repeat("b", maxDomainNameBytes-3)), "",
			"ns." + strings.Repeat("b", maxDomainNameBytes-3)},
		{"max namespace + max name is shortened", namingID(strings.Repeat("n", 63), long253), "",
			boundDomainName(strings.Repeat("n", 63), long253)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domainNameFor(tc.owner, tc.fallback)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got), maxDomainNameBytes)
		})
	}
}

func TestDomainNameFor_LengthBoundary(t *testing.T) {
	ns := "team-a"
	// The longest name that is NOT shortened: len("team-a.") + n == bound.
	fits := strings.Repeat("x", maxDomainNameBytes-len(ns)-1)
	got, err := domainNameFor(namingID(ns, fits), "")
	require.NoError(t, err)
	assert.Equal(t, ns+"."+fits, got)
	assert.Len(t, got, maxDomainNameBytes)

	// One byte more is shortened: prefix + "-" + 8 hex digits of sha256("<ns>/<name>").
	over := fits + "x"
	got, err = domainNameFor(namingID(ns, over), "")
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(ns + "/" + over))
	wantSuffix := "-" + hex.EncodeToString(sum[:])[:domainNameHashHexLen]
	assert.True(t, strings.HasSuffix(got, wantSuffix), "got %q", got)
	assert.True(t, strings.HasPrefix(got, ns+"."), "a shortened name keeps its whole namespace")
	assert.Len(t, got, maxDomainNameBytes)

	// Stable across calls (a retried create must find the same domain).
	again, err := domainNameFor(namingID(ns, over), "")
	require.NoError(t, err)
	assert.Equal(t, got, again)

	// Two long names sharing the truncated prefix stay distinct.
	other, err := domainNameFor(namingID(ns, fits+"y"), "")
	require.NoError(t, err)
	assert.NotEqual(t, got, other)
	assert.Equal(t, got[:len(got)-len(wantSuffix)], other[:len(other)-len(wantSuffix)], "same prefix, different hash")

	// The same long name in another namespace is distinct.
	otherNS, err := domainNameFor(namingID("team-b", over), "")
	require.NoError(t, err)
	assert.NotEqual(t, got, otherNS)

	// Every suffix VirtRigaud appends still fits a 255-byte file name.
	for _, suffix := range []string{
		vmDiskVolumeSuffix + qcow2Ext, contracts.ImportedDiskNameSuffix + qcow2Ext,
		vmDiskVolumeSuffix + "-temp.img", "-domain.xml.XXXXXXXXXX", ".XXXXXXXXXX",
	} {
		assert.LessOrEqual(t, len(got+suffix), 255, "suffix %q", suffix)
	}
}

func TestDomainNameFor_TruncationNeverEndsASegment(t *testing.T) {
	// A name whose truncation point lands inside a run of '-' (valid in a
	// DNS-1123 subdomain) is trimmed back to an alphanumeric before the hash.
	name := "a" + strings.Repeat("-", 240) + "b"
	got, err := domainNameFor(namingID("team-a", name), "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "team-a.a-"), "got %q", got)
	assert.NotContains(t, got, "--", "the trailing '-' run is trimmed before the hash suffix")
	assert.NoError(t, ambiguousDomainNameError(got))
}

func TestDomainNameFor_UniqueAcrossNamespaces(t *testing.T) {
	seen := map[string]contracts.ObjectIdentity{}
	for _, ns := range []string{"a", "team-a", "team-b", "a-b", "b"} {
		for _, name := range []string{"web", "a.web", "b.web", "web.a", "team-a.web"} {
			got, err := domainNameFor(namingID(ns, name), name)
			require.NoError(t, err)
			if prev, dup := seen[got]; dup {
				t.Fatalf("%s/%s and %s/%s both map to %q", prev.Namespace, prev.Name, ns, name, got)
			}
			seen[got] = namingID(ns, name)
		}
	}
}

// TestDomainNameFor_AmbiguityGuard: the legacy fallback keeps the #333 guard,
// and a namespaced name can never be all-digit or UUID-shaped.
func TestDomainNameFor_AmbiguityGuard(t *testing.T) {
	for _, legacy := range []string{"12", "0", "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "1b4e28ba2fa111d2883f0016d3cca427"} {
		_, err := domainNameFor(contracts.ObjectIdentity{}, legacy)
		requireInvalidSpec(t, err)
	}

	for _, ns := range []string{"0", "12", "1b4e28ba", "deadbeef"} {
		for _, name := range []string{"0", "12", "1b4e28ba-2fa1-11d2-883f-0016d3cca427", "1b4e28ba2fa111d2883f0016d3cca427", "web"} {
			got, err := domainNameFor(namingID(ns, name), name)
			require.NoError(t, err, "%s/%s", ns, name)
			assert.False(t, looksLikeVirshDomainID(got), "%q must never look like a domain ID", got)
			assert.False(t, looksLikeUUID(got), "%q must never look like a UUID", got)
			assert.Contains(t, got, domainNameSeparator)
		}
	}
}

func TestDomainNameFor_RejectsNonDNSIdentity(t *testing.T) {
	for _, owner := range []contracts.ObjectIdentity{
		{Namespace: "team.a", Name: "web"}, // a '.' in the namespace would make the split ambiguous
		{Namespace: "Team-A", Name: "web"},
		{Namespace: "team-a", Name: "../../etc/x"},
		{Namespace: "team-a", Name: "-web"},
		{Namespace: "team-a", Name: "web/x"},
		{Namespace: "team-a", Name: "we b"},
		{Namespace: strings.Repeat("n", 64), Name: "web"},
		{Namespace: "team-a", Name: strings.Repeat("w", 254)},
	} {
		_, err := domainNameFor(owner, "web")
		requireInvalidSpec(t, err)
	}
}

func TestCreateDomainName(t *testing.T) {
	got, err := createDomainName(contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", got)

	got, err = createDomainName(contracts.CreateRequest{Name: "web"})
	require.NoError(t, err)
	assert.Equal(t, "web", got, "no owner (older manager): legacy name")

	_, err = createDomainName(contracts.CreateRequest{Name: "db", Owner: ownerTeamA})
	requireInvalidSpec(t, err)
}
