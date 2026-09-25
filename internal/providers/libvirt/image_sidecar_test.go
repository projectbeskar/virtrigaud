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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

const (
	testImageUID      = "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a"
	testOtherImageUID = "0d4e7c1a-9f3b-4c2d-8e5a-6b7c8d9e0f1a"
	testDigest        = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	testOtherDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testProviderUID   = "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a"
)

// testIdentityRequest is a parsed identity request for team-a/ubuntu-22.04.
func testIdentityRequest(uid, digest string) imageartifact.Request {
	return imageartifact.Request{
		Mode:         imageartifact.ModeIdentity,
		Image:        contracts.ObjectIdentity{UID: uid, Namespace: "team-a", Name: "ubuntu-22.04"},
		SourceDigest: digest,
		PreparedBy:   contracts.ObjectIdentity{UID: testProviderUID, Namespace: "team-a", Name: "libvirt"},
	}
}

// testSidecar returns a valid sidecar for uid/digest stamping inode/size.
func testSidecar(uid, digest string, inode uint64, size int64) imageSidecar {
	stamp := imageartifact.NewStamp(testIdentityRequest(uid, digest), time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	return imageSidecar{Stamp: stamp, Artifact: imageSidecarArtifact{Inode: inode, Size: size}}
}

// TestImageSidecar_Golden pins the sidecar's serialized form (ADR-0009 D3 /
// the per-provider API reference): the stamp fields at the top level plus the
// artifact's inode and size.
func TestImageSidecar_Golden(t *testing.T) {
	data, err := encodeImageSidecar(testSidecar(testImageUID, testDigest, 1835021, 2361393152))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"stampVersion": 1,
		"image": {"uid": "`+testImageUID+`", "namespace": "team-a", "name": "ubuntu-22.04"},
		"sourceDigest": "`+testDigest+`",
		"preparedBy": {"uid": "`+testProviderUID+`", "namespace": "team-a", "name": "libvirt"},
		"preparedAt": "2026-09-25T10:00:00Z",
		"artifact": {"inode": 1835021, "size": 2361393152}
	}`, string(data))
	assert.NotContains(t, string(data), "http", "a stamp never carries a URL")

	got, err := parseImageSidecar(data)
	require.NoError(t, err)
	assert.Equal(t, testSidecar(testImageUID, testDigest, 1835021, 2361393152), got)
	assert.True(t, got.Matches(testIdentityRequest(testImageUID, testDigest)))
	assert.False(t, got.Matches(testIdentityRequest(testOtherImageUID, testDigest)))
	assert.False(t, got.Matches(testIdentityRequest(testImageUID, testOtherDigest)))
}

// TestImageSidecar_UntrustedForms is the ADR-0009 D3 fail-closed list for the
// libvirt stamp: every malformed or ambiguous sidecar is untrusted.
func TestImageSidecar_UntrustedForms(t *testing.T) {
	valid, err := encodeImageSidecar(testSidecar(testImageUID, testDigest, 7, 9))
	require.NoError(t, err)
	body := strings.TrimSuffix(strings.TrimPrefix(string(valid), "{"), "}")

	cases := map[string]string{
		"empty":              "",
		"not JSON":           "stamp",
		"not an object":      `[` + string(valid) + `]`,
		"trailing data":      string(valid) + `{}`,
		"trailing garbage":   string(valid) + `x`,
		"repeated key":       `{"stampVersion":1,` + body + `}`,
		"repeated key case":  `{"StampVersion":1,` + body + `}`,
		"repeated uid":       strings.Replace(string(valid), `"uid":"`+testImageUID+`"`, `"uid":"`+testOtherImageUID+`","uid":"`+testImageUID+`"`, 1),
		"null value":         strings.Replace(string(valid), `"namespace":"team-a"`, `"namespace":null`, 1),
		"non-string uid":     strings.Replace(string(valid), `"uid":"`+testImageUID+`"`, `"uid":5`, 1),
		"string version":     strings.Replace(string(valid), `"stampVersion":1`, `"stampVersion":"1"`, 1),
		"unknown version":    strings.Replace(string(valid), `"stampVersion":1`, `"stampVersion":2`, 1),
		"fractional version": strings.Replace(string(valid), `"stampVersion":1`, `"stampVersion":1.0`, 1),
		"unknown field":      `{"extra":"x",` + body + `}`,
		"truncated digest":   strings.Replace(string(valid), testDigest, testDigest[:len(testDigest)-1], 1),
		"uppercase digest":   strings.Replace(string(valid), testDigest, strings.ToUpper(testDigest), 1),
		"bad uid":            strings.Replace(string(valid), testImageUID, "../x", 1),
		"no artifact":        strings.Replace(string(valid), `,"artifact":{"inode":7,"size":9}`, ``, 1),
		"zero inode":         strings.Replace(string(valid), `"inode":7`, `"inode":0`, 1),
		"negative inode":     strings.Replace(string(valid), `"inode":7`, `"inode":-7`, 1),
		"zero size":          strings.Replace(string(valid), `"size":9`, `"size":0`, 1),
		"deep nesting":       `{"image":` + strings.Repeat(`[`, 20) + strings.Repeat(`]`, 20) + `}`,
		"invalid UTF-8":      strings.Replace(string(valid), "team-a", "team-\xff", 1),
		"oversized":          string(valid) + strings.Repeat(" ", maxImageSidecarBytes), // valid JSON, only too large
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, string(valid), doc, "the case must change the document")
			_, err := parseImageSidecar([]byte(doc))
			assert.Error(t, err, "sidecar %q must be untrusted", doc)
		})
	}
}

// TestImageSidecar_EncodeRefusesUntrusted proves the provider never writes a
// sidecar it would not trust on read.
func TestImageSidecar_EncodeRefusesUntrusted(t *testing.T) {
	for name, s := range map[string]imageSidecar{
		"no inode":   testSidecar(testImageUID, testDigest, 0, 9),
		"no size":    testSidecar(testImageUID, testDigest, 7, 0),
		"bad digest": testSidecar(testImageUID, "sha256:x", 7, 9),
		"no uid":     testSidecar("", testDigest, 7, 9),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := encodeImageSidecar(s)
			assert.Error(t, err)
		})
	}
}

// TestImageSidecarName checks the sidecar is a dotfile next to the artifact,
// never a usable base image (#334) and within the 255-byte file-name limit
// even for the longest artifact name (ADR-0009 D1: 200 bytes).
func TestImageSidecarName(t *testing.T) {
	assert.Equal(t, ".team-a.ubuntu_3c9e1f0a7b2d4e61.virtrigaud-image.json", imageSidecarName("team-a.ubuntu_3c9e1f0a7b2d4e61"))
	longest := strings.Repeat("n", imageartifact.NameRuleLibvirt.MaxBytes())
	assert.True(t, reservedImageName(imageSidecarName(longest)))
	assert.LessOrEqual(t, len(imageSidecarName(longest)), 255)
}
