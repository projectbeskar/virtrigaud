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

package inventory

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	stReady        = providerv1.ComponentState_COMPONENT_STATE_READY
	stMissing      = providerv1.ComponentState_COMPONENT_STATE_MISSING
	stIncompatible = providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE
	stUnhealthy    = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY
	stUnmanifested = providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED
	stInvalid      = providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID
	originRO       = providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY
	originW        = providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE
	originNone     = providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE
)

func TestReady(t *testing.T) {
	inv := collectInventory(t, healthyTree(t), nil)
	require.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, inv.GetState())
	require.Len(t, inv.GetComponents(), 2)
	ch := component(t, inv, "cloud-hypervisor")
	assert.Equal(t, stReady, ch.GetState())
	assert.Empty(t, ch.GetStateReason())
	assert.Equal(t, originRO, ch.GetManifestOrigin())
	assert.Equal(t, "1.2.3-1", ch.GetObservedVersion(), "from the package database, never by running the binary")
	assert.Equal(t, "1.2.3", ch.GetDeclaredVersion())
	assert.Equal(t, "package", ch.GetSource())
	require.Len(t, ch.GetFiles(), 1)
	assert.Equal(t, "cloud-hypervisor", ch.GetFiles()[0].GetName())
	assert.Equal(t, rawDigest(chBinary), ch.GetFiles()[0].GetSha256(), "a raw 32-byte SHA-256 the agent computed")
	assert.Empty(t, ch.GetArtifactSha256())
	ag := component(t, inv, "virtrigaud-agent")
	assert.Equal(t, stReady, ag.GetState())
	assert.Equal(t, "v0.1.0", ag.GetObservedVersion())
	assert.Equal(t, rawDigest(agentBinary), ag.GetFiles()[0].GetSha256())
	assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE, inv.GetProcessIdentityTier())
	assert.Empty(t, inv.GetUnexpectedExtensions())
}

func TestStates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*tree)
		comp   string
		state  providerv1.ComponentState
		reason string
		origin providerv1.ManifestOrigin
		check  func(*testing.T, *providerv1.HostComponent)
	}{
		{name: "missing: declared, not installed", comp: "cloud-hypervisor", state: stMissing, reason: ReasonNotInstalled, origin: originRO,
			mutate: func(tr *tree) { require.NoError(t, os.Remove(tr.path("/usr/bin/cloud-hypervisor"))) },
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Equal(t, "1.2.3", c.GetDeclaredVersion())
				assert.Empty(t, c.GetObservedVersion())
				assert.Empty(t, c.GetFiles())
				assert.Empty(t, c.GetArtifactSha256())
			}},
		{name: "missing: required, undeclared, absent", comp: "cloud-hypervisor", state: stMissing, reason: ReasonNotDeclared, origin: originNone,
			mutate: func(tr *tree) {
				require.NoError(t, os.Remove(tr.path("/usr/bin/cloud-hypervisor")))
				require.NoError(t, os.Remove(tr.path(ReadOnlyManifestDir+"/cloud-hypervisor.json")))
			},
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Empty(t, c.GetDeclaredVersion())
				assert.Empty(t, c.GetSource())
			}},
		{name: "incompatible: out of range", comp: "cloud-hypervisor", state: stIncompatible, reason: ReasonVersionOutOfRange, origin: originRO,
			mutate: func(tr *tree) {
				tr.write("/var/lib/dpkg/status", []byte("Package: cloud-hypervisor\nStatus: install ok installed\nVersion: 0.9.1\n\n"))
			},
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Equal(t, ">=1.0.0", c.GetRequiredRange())
				assert.Equal(t, "0.9.1", c.GetObservedVersion())
			}},
		{name: "incompatible: no version source", comp: "cloud-hypervisor", state: stIncompatible, reason: ReasonVersionUnreadable, origin: originRO,
			mutate: func(tr *tree) { require.NoError(t, os.Remove(tr.path("/var/lib/dpkg/status"))) },
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Equal(t, ">=1.0.0", c.GetRequiredRange())
				assert.Empty(t, c.GetObservedVersion())
			}},
		{name: "incompatible: a removed package is not a version", comp: "cloud-hypervisor", state: stIncompatible, reason: ReasonVersionUnreadable, origin: originRO,
			mutate: func(tr *tree) {
				tr.write("/var/lib/dpkg/status", []byte("Package: cloud-hypervisor\nStatus: deinstall ok config-files\nVersion: 1.2.3\n\n"))
			}},
		{name: "unhealthy: file digest differs", comp: "cloud-hypervisor", state: stUnhealthy, reason: ReasonFileDigestMismatch, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.2.3",
					fmt.Sprintf(`"files":{"cloud-hypervisor":%q}`, digestOf([]byte("something else")))))
			},
			check: func(t *testing.T, c *providerv1.HostComponent) {
				require.Len(t, c.GetFiles(), 1)
				assert.Equal(t, rawDigest(chBinary), c.GetFiles()[0].GetSha256(), "the digest the agent computed is still reported")
			}},
		{name: "ready: declared file digest matches", comp: "cloud-hypervisor", state: stReady, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.2.3",
					fmt.Sprintf(`"files":{"cloud-hypervisor":%q}`, digestOf(chBinary))))
			}},
		{name: "unhealthy: unexpected directory extension overlays the binary", comp: "cloud-hypervisor", state: stUnhealthy, reason: ReasonUnexpectedExtension, origin: originRO,
			mutate: func(tr *tree) { tr.write("/run/extensions/rogue/usr/bin/cloud-hypervisor", []byte("x")) }},
		{name: "unmanifested: present, undeclared", comp: "cloud-hypervisor", state: stUnmanifested, reason: ReasonPresentUndeclared, origin: originNone,
			mutate: func(tr *tree) { require.NoError(t, os.Remove(tr.path(ReadOnlyManifestDir+"/cloud-hypervisor.json"))) },
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Empty(t, c.GetDeclaredVersion())
				assert.Equal(t, "1.2.3-1", c.GetObservedVersion())
				assert.Len(t, c.GetFiles(), 1)
			}},
		{name: "invalid: unsupported digest names the component", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonUnsupportedDigest, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.2.3",
					`"files":{"cloud-hypervisor":"md5:abc"}`))
			},
			check: func(t *testing.T, c *providerv1.HostComponent) {
				assert.Empty(t, c.GetDeclaredVersion(), "the declaration of an invalid manifest is not reported")
			}},
		{name: "invalid: version too long", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonVersionTooLong, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", strings.Repeat("1", 65), ""))
			}},
		{name: "invalid: bad version characters", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonParseError, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.0 ; rm -rf /", ""))
			}},
		{name: "invalid: a file the catalog does not know", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonParseError, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.2.3",
					fmt.Sprintf(`"files":{"bash":%q}`, digestOf(nil))))
			}},
		{name: "invalid: duplicate across directories, origin is the image's", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonDuplicateDeclaration, origin: originRO,
			mutate: func(tr *tree) {
				tr.manifest(WritableManifestDir, "override.json", manifestJSON("cloud-hypervisor", "9.9.9", ""))
			}},
		{name: "invalid: duplicate in the writable directory", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonDuplicateDeclaration, origin: originW,
			mutate: func(tr *tree) {
				require.NoError(t, os.Remove(tr.path(ReadOnlyManifestDir+"/cloud-hypervisor.json")))
				tr.manifest(WritableManifestDir, "a.json", manifestJSON("cloud-hypervisor", "1.2.3", ""))
				tr.manifest(WritableManifestDir, "b.json", manifestJSON("cloud-hypervisor", "1.2.3", ""))
			}},
		{name: "ready: a writable manifest adds a declaration", comp: "cloud-hypervisor", state: stReady, origin: originW,
			mutate: func(tr *tree) {
				require.NoError(t, os.Remove(tr.path(ReadOnlyManifestDir+"/cloud-hypervisor.json")))
				tr.manifest(WritableManifestDir, "local.json", manifestJSON("cloud-hypervisor", "1.2.3", ""))
			}},
		{name: "precedence: invalid manifest wins over missing", comp: "cloud-hypervisor", state: stInvalid, reason: ReasonParseError, origin: originRO,
			mutate: func(tr *tree) {
				require.NoError(t, os.Remove(tr.path("/usr/bin/cloud-hypervisor")))
				tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", `{"component":`)
			},
			check: func(t *testing.T, c *providerv1.HostComponent) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := healthyTree(t)
			tt.mutate(tr)
			inv := collectInventory(t, tr, nil)
			var c *providerv1.HostComponent
			if tt.name == "precedence: invalid manifest wins over missing" {
				// A manifest that does not parse names no component: it is hash-named.
				c = component(t, inv, hashedName("cloud-hypervisor.json"))
				assert.Equal(t, providerv1.ComponentState_COMPONENT_STATE_MISSING, component(t, inv, "cloud-hypervisor").GetState(),
					"the component itself is then undeclared and absent")
			} else {
				c = component(t, inv, tt.comp)
			}
			assert.Equal(t, tt.state, c.GetState())
			assert.Equal(t, tt.reason, c.GetStateReason())
			assert.Equal(t, tt.origin, c.GetManifestOrigin())
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestManifestInvalidNaming(t *testing.T) {
	tests := []struct {
		name, base, doc, reason string
	}{
		{"not json", "broken.json", `not json`, ReasonParseError},
		{"trailing data", "trail.json", manifestJSON("cloud-hypervisor", "1", "") + ` {}`, ReasonParseError},
		{"unknown field", "unk.json", `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","surprise":1}`, ReasonParseError},
		{"wrong schema", "schema.json", `{"schemaVersion":2,"component":"cloud-hypervisor","version":"1"}`, ReasonParseError},
		{"unknown component", "other.json", manifestJSON("not-in-catalog", "1.0.0", ""), ReasonUnknownComponent},
		{"invalid component name", "name.json", manifestJSON("Bad_Name/..", "1.0.0", ""), ReasonInvalidName},
		{"reserved prefix is not a catalog name", "res.json", manifestJSON("manifest-0123456789abcdef", "1.0.0", ""), ReasonUnknownComponent},
		{"too large", "big.json", manifestJSON("cloud-hypervisor", "1", "")[:10] + strings.Repeat(" ", MaxManifestBytes), ReasonTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := healthyTree(t)
			base := "zq-" + tt.base
			tr.manifest(WritableManifestDir, base, tt.doc)
			inv := collectInventory(t, tr, nil)
			want := hashedName(base)
			c := component(t, inv, want)
			assert.Equal(t, stInvalid, c.GetState())
			assert.Equal(t, tt.reason, c.GetStateReason())
			assert.Equal(t, originW, c.GetManifestOrigin())
			assert.True(t, strings.HasPrefix(c.GetName(), "manifest-") && len(c.GetName()) == len("manifest-")+16, "a hashed name: %s", c.GetName())
			assert.NotContains(t, c.String(), strings.TrimSuffix(base, ".json"), "the base name is never reported")
			assert.Equal(t, stReady, component(t, inv, "cloud-hypervisor").GetState(), "the valid declaration is unaffected")
		})
	}
}

func TestHashedNameCollision(t *testing.T) {
	tr := healthyTree(t)
	tr.manifest(ReadOnlyManifestDir, "same.json", "garbage")
	tr.manifest(WritableManifestDir, "same.json", manifestJSON("not-in-catalog", "1", ""))
	inv := collectInventory(t, tr, nil)
	n := 0
	for _, c := range inv.GetComponents() {
		if c.GetName() == hashedName("same.json") {
			n++
			assert.Equal(t, originRO, c.GetManifestOrigin(), "one entry, origin READ_ONLY")
			assert.Equal(t, ReasonParseError, c.GetStateReason(), "the image's manifest is the one the entry is about")
		}
	}
	assert.Equal(t, 1, n)
}

func TestNonRegularManifestIsInvalid(t *testing.T) {
	tr := healthyTree(t)
	tr.mkdir(WritableManifestDir)
	require.NoError(t, os.Symlink("/etc/passwd", tr.path(WritableManifestDir+"/link.json")))
	tr.mkdir(WritableManifestDir + "/dir.json")
	inv := collectInventory(t, tr, nil)
	for _, base := range []string{"link.json", "dir.json"} {
		c := component(t, inv, hashedName(base))
		assert.Equal(t, stInvalid, c.GetState(), base)
		assert.Equal(t, ReasonParseError, c.GetStateReason(), base)
	}
}
