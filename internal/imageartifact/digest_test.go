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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// goldenLibvirtSource is the source of the first golden vector. Its URL holds
// '&', '<' and '>' to pin that the canonical form does not HTML-escape.
func goldenLibvirtSource() infravirtrigaudiov1beta1.ImageSource {
	return infravirtrigaudiov1beta1.ImageSource{Libvirt: &infravirtrigaudiov1beta1.LibvirtImageSource{
		URL:          "https://images.example.com/ubuntu-22.04.qcow2?a=1&b=<2>",
		Format:       infravirtrigaudiov1beta1.ImageFormatQCOW2,
		Checksum:     "abc123",
		ChecksumType: infravirtrigaudiov1beta1.ChecksumTypeSHA256,
		StoragePool:  "default",
	}}
}

// goldenProxmoxSource is the source of the second golden vector: an integer
// and a bool, to pin number and boolean encoding.
func goldenProxmoxSource() infravirtrigaudiov1beta1.ImageSource {
	templateID, fullClone := 9000, true
	return infravirtrigaudiov1beta1.ImageSource{Proxmox: &infravirtrigaudiov1beta1.ProxmoxImageSource{
		TemplateID: &templateID, Storage: "local-lvm", Node: "pve1", Format: "qcow2", FullClone: &fullClone,
	}}
}

// httpSource returns an HTTP source with every field set.
func httpSource() infravirtrigaudiov1beta1.ImageSource {
	return infravirtrigaudiov1beta1.ImageSource{HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{
		URL:          "https://images.example.com/disk.qcow2",
		Headers:      map[string]string{"X-Token": "secret-1", "Accept": "application/octet-stream"},
		Checksum:     "deadbeef",
		ChecksumType: infravirtrigaudiov1beta1.ChecksumTypeSHA256,
		Authentication: &infravirtrigaudiov1beta1.HTTPAuthentication{
			Bearer: &infravirtrigaudiov1beta1.BearerTokenConfig{
				SecretRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "token"}, TokenKey: "token",
			},
		},
		Timeout: &metav1.Duration{Duration: 30 * time.Minute},
	}}
}

// mustDigest returns SourceDigest(src), failing the test on error.
func mustDigest(t *testing.T, src infravirtrigaudiov1beta1.ImageSource) string {
	t.Helper()
	d, err := SourceDigest(src)
	require.NoError(t, err)
	return d
}

// TestSourceDigestGolden pins the canonical form and the digest (ADR-0009 D2).
// A failure here means every prepared artifact would be renamed and imported
// again: change the canonical form only deliberately, with a
// SourceDigestVersion bump.
func TestSourceDigestGolden(t *testing.T) {
	for name, tc := range map[string]struct {
		src           infravirtrigaudiov1beta1.ImageSource
		wantCanonical string
		wantDigest    string
	}{
		"libvirt URL import": {
			src: goldenLibvirtSource(),
			wantCanonical: `{"source":{"libvirt":{"checksum":"abc123","checksumType":"sha256","format":"qcow2",` +
				`"storagePool":"default","url":"https://images.example.com/ubuntu-22.04.qcow2?a=1&b=<2>"}},"v":1}`,
			wantDigest: "sha256:2598d734534c348d73aaa718c8e529dab9b155b64711b2b8d60f872dfcc0e5e5",
		},
		"proxmox template": {
			src: goldenProxmoxSource(),
			wantCanonical: `{"source":{"proxmox":{"format":"qcow2","fullClone":true,"node":"pve1",` +
				`"storage":"local-lvm","templateID":9000}},"v":1}`,
			wantDigest: "sha256:869df67bab02828f841116caead1a7231c3e77099759614e1a48d529d79a8f45",
		},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, err := canonicalSource(tc.src)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCanonical, string(canonical))
			// The digest is exactly sha256 over the canonical bytes.
			sum := sha256.Sum256([]byte(tc.wantCanonical))
			assert.Equal(t, SourceDigestPrefix+hex.EncodeToString(sum[:]), tc.wantDigest)
			assert.Equal(t, tc.wantDigest, mustDigest(t, tc.src))
		})
	}
}

// TestSourceDigestDeterministic verifies equal sources give equal digests,
// repeatedly and across deep copies, and that the input is not modified.
func TestSourceDigestDeterministic(t *testing.T) {
	src := httpSource()
	first := mustDigest(t, src)
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, mustDigest(t, *src.DeepCopy()))
	}
	assert.Equal(t, httpSource(), src, "SourceDigest must not modify its input (the transport fields are cleared on a copy)")
	require.NoError(t, ValidateSourceDigest(first), "SourceDigest output is a valid digest")
}

// TestSourceDigestExcludesTransportFields pins ADR-0009 Q7: the transport-only
// source.http fields (timeout, headers, authentication) never change the
// digest, so rotating a token or a header does not orphan an artifact.
func TestSourceDigestExcludesTransportFields(t *testing.T) {
	base := mustDigest(t, httpSource())
	for name, mutate := range map[string]func(*infravirtrigaudiov1beta1.HTTPImageSource){
		"timeout changed":        func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Timeout = &metav1.Duration{Duration: time.Hour} },
		"timeout removed":        func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Timeout = nil },
		"header value rotated":   func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Headers["X-Token"] = "secret-2" },
		"header added":           func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Headers["X-Other"] = "1" },
		"headers removed":        func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Headers = nil },
		"authentication changed": func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Authentication.Bearer.SecretRef.Name = "token-2" },
		"authentication removed": func(h *infravirtrigaudiov1beta1.HTTPImageSource) { h.Authentication = nil },
		"authentication kind swap": func(h *infravirtrigaudiov1beta1.HTTPImageSource) {
			h.Authentication = &infravirtrigaudiov1beta1.HTTPAuthentication{BasicAuth: &infravirtrigaudiov1beta1.BasicAuthConfig{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := httpSource()
			mutate(src.HTTP)
			assert.Equal(t, base, mustDigest(t, src))
		})
	}
}

// TestSourceDigestCoversContentAndLocation pins ADR-0009 D2/Q7: every
// content-defining field and every location field changes the digest.
func TestSourceDigestCoversContentAndLocation(t *testing.T) {
	type mutation func(*infravirtrigaudiov1beta1.ImageSource)
	cases := map[string]struct {
		base   func() infravirtrigaudiov1beta1.ImageSource
		mutate mutation
	}{
		"libvirt url":  {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Libvirt.URL += "x" }},
		"libvirt path": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Libvirt.Path = "/var/lib/libvirt/images/a.qcow2" }},
		"libvirt format": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) {
			s.Libvirt.Format = infravirtrigaudiov1beta1.ImageFormatRaw
		}},
		"libvirt checksum": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Libvirt.Checksum = "abc124" }},
		"libvirt checksumType": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) {
			s.Libvirt.ChecksumType = infravirtrigaudiov1beta1.ChecksumTypeSHA512
		}},
		"libvirt storagePool": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Libvirt.StoragePool = "other" }},
		"proxmox storage":     {goldenProxmoxSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Proxmox.Storage = "nfs" }},
		"proxmox node":        {goldenProxmoxSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.Proxmox.Node = "pve2" }},
		"proxmox templateID": {goldenProxmoxSource, func(s *infravirtrigaudiov1beta1.ImageSource) {
			id := 9001
			s.Proxmox.TemplateID = &id
		}},
		"http url":      {httpSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.HTTP.URL += "2" }},
		"http checksum": {httpSource, func(s *infravirtrigaudiov1beta1.ImageSource) { s.HTTP.Checksum = "deadbeee" }},
		"vsphere ovaURL added": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) {
			s.VSphere = &infravirtrigaudiov1beta1.VSphereImageSource{OVAURL: "https://x/y.ova"}
		}},
		"source kind swapped": {goldenLibvirtSource, func(s *infravirtrigaudiov1beta1.ImageSource) {
			s.HTTP = &infravirtrigaudiov1beta1.HTTPImageSource{URL: s.Libvirt.URL}
			s.Libvirt = nil
		}},
	}
	seen := map[string]string{}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := tc.base()
			before := mustDigest(t, src)
			tc.mutate(&src)
			after := mustDigest(t, src)
			assert.NotEqual(t, before, after)
			if other, dup := seen[after]; dup {
				t.Errorf("mutations %q and %q produced the same digest", name, other)
			}
			seen[after] = name
		})
	}
}

// TestSourceDigestIgnoresOutsideSource documents that only spec.source is
// hashed: spec.prepare, spec.metadata, spec.distribution and
// spec.consumerNamespaceSelector are not inputs (editing a selector must never
// re-import an image).
func TestSourceDigestIgnoresOutsideSource(t *testing.T) {
	a := infravirtrigaudiov1beta1.VMImageSpec{Source: goldenLibvirtSource()}
	b := infravirtrigaudiov1beta1.VMImageSpec{
		Source:                    goldenLibvirtSource(),
		Prepare:                   &infravirtrigaudiov1beta1.ImagePrepare{},
		Metadata:                  &infravirtrigaudiov1beta1.ImageMetadata{DisplayName: "Ubuntu"},
		Distribution:              &infravirtrigaudiov1beta1.OSDistribution{Name: "ubuntu"},
		ConsumerNamespaceSelector: &metav1.LabelSelector{},
	}
	assert.Equal(t, mustDigest(t, a.Source), mustDigest(t, b.Source))
}

// TestCanonicalJSONSortsKeys verifies the canonical form does not depend on
// the order keys (Go struct fields) were written in, at every nesting level.
func TestCanonicalJSONSortsKeys(t *testing.T) {
	x, err := canonicalJSON([]byte(`{"v":1,"source":{"b":{"y":2,"x":1},"a":[{"d":4,"c":3}]}}`))
	require.NoError(t, err)
	y, err := canonicalJSON([]byte(`{ "source" : {"a":[{"c":3,"d":4}], "b":{"x":1,"y":2}}, "v":1 }`))
	require.NoError(t, err)
	assert.Equal(t, string(x), string(y))
	assert.Equal(t, `{"source":{"a":[{"c":3,"d":4}],"b":{"x":1,"y":2}},"v":1}`, string(x))

	// Numbers are kept exactly as written (no float round-trip).
	n, err := canonicalJSON([]byte(`{"n":9007199254740993}`))
	require.NoError(t, err)
	assert.Equal(t, `{"n":9007199254740993}`, string(n))
}

// TestSourceDigestIsVersioned verifies the version is inside the hashed
// envelope: the same source under another version has another digest, so a
// version bump renames every artifact (a deliberate migration).
func TestSourceDigestIsVersioned(t *testing.T) {
	canonical, err := canonicalSource(goldenLibvirtSource())
	require.NoError(t, err)
	assert.Contains(t, string(canonical), `"v":1`)
	assert.Equal(t, 1, SourceDigestVersion)

	src := goldenLibvirtSource()
	typed := []byte(`{"source":{"libvirt":{"checksum":"abc123","checksumType":"sha256","format":"qcow2",` +
		`"storagePool":"default","url":"https://images.example.com/ubuntu-22.04.qcow2?a=1&b=<2>"}},"v":2}`)
	v2, err := canonicalJSON(typed)
	require.NoError(t, err)
	sum := sha256.Sum256(v2)
	assert.NotEqual(t, SourceDigestPrefix+hex.EncodeToString(sum[:]), mustDigest(t, src))
}

// TestValidateSourceDigest pins the digest syntax a provider accepts.
func TestValidateSourceDigest(t *testing.T) {
	valid := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, ValidateSourceDigest(valid))
	for name, d := range map[string]string{
		"empty":          "",
		"no prefix":      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"other algo":     "sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"uppercase hex":  "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"truncated":      valid[:len(valid)-1],
		"too long":       valid + "0",
		"non-hex":        "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg",
		"trailing space": valid[:len(valid)-1] + " ",
		"prefix only":    "sha256:",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, ValidateSourceDigest(d))
		})
	}
}

// TestSourceDigestPatternMatchesCRD keeps SourceDigestPattern and the pattern
// of VMImage status.providerStatus[].sourceDigest in the generated CRD
// identical, so the API server admits exactly the digests providers accept.
func TestSourceDigestPatternMatchesCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "infra.virtrigaud.io_vmimages.yaml"))
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(raw, &obj))
	versions, found, err := unstructured.NestedSlice(obj, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found)
	var pattern string
	for _, v := range versions {
		version, ok := v.(map[string]any)
		require.True(t, ok)
		if version["name"] != "v1beta1" {
			continue
		}
		pattern, found, err = unstructured.NestedString(version, "schema", "openAPIV3Schema", "properties", "status",
			"properties", "providerStatus", "additionalProperties", "properties", "sourceDigest", "pattern")
		require.NoError(t, err)
		require.True(t, found, "the VMImage CRD has status.providerStatus[].sourceDigest with a pattern")
	}
	assert.Equal(t, SourceDigestPattern, pattern)
}
