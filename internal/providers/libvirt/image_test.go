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

package libvirt

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestParseLibvirtImageSource_RichSpec verifies the rich v1beta1.VMImageSpec
// shape (nested source.libvirt) is parsed including the storage pool, which only
// this shape can express.
func TestParseLibvirtImageSource_RichSpec(t *testing.T) {
	imageJSON := `{
		"source": {
			"libvirt": {
				"url": "https://images.example.com/jammy.qcow2",
				"format": "qcow2",
				"checksum": "abc123",
				"checksumType": "sha512",
				"storagePool": "vmpool"
			}
		}
	}`

	src := parseLibvirtImageSource(imageJSON)

	assert.Equal(t, "", src.Path)
	assert.Equal(t, "https://images.example.com/jammy.qcow2", src.URL)
	assert.Equal(t, "qcow2", src.Format)
	assert.Equal(t, "abc123", src.Checksum)
	assert.Equal(t, "sha512", src.ChecksumType)
	assert.Equal(t, "vmpool", src.StoragePool)
}

// TestParseLibvirtImageSource_RichSpecPath verifies the path branch of the rich
// spec (host-local image, no download).
func TestParseLibvirtImageSource_RichSpecPath(t *testing.T) {
	imageJSON := `{"source":{"libvirt":{"path":"/var/lib/libvirt/images/base.qcow2"}}}`

	src := parseLibvirtImageSource(imageJSON)

	assert.Equal(t, "/var/lib/libvirt/images/base.qcow2", src.Path)
	assert.Equal(t, "", src.URL)
	assert.Equal(t, "", src.StoragePool)
}

// TestParseLibvirtImageSource_FlatContractsVMImage verifies the fallback flat
// contracts.VMImage shape (Go field names, no storage pool) is parsed when no
// nested source.libvirt is present. This is the shape Create already round-trips.
func TestParseLibvirtImageSource_FlatContractsVMImage(t *testing.T) {
	img := contracts.VMImage{
		Path:         "/srv/templates/rocky.qcow2",
		Format:       "qcow2",
		Checksum:     "deadbeef",
		ChecksumType: "sha256",
	}
	raw, err := json.Marshal(img)
	require.NoError(t, err)

	src := parseLibvirtImageSource(string(raw))

	assert.Equal(t, "/srv/templates/rocky.qcow2", src.Path)
	assert.Equal(t, "qcow2", src.Format)
	assert.Equal(t, "deadbeef", src.Checksum)
	assert.Equal(t, "sha256", src.ChecksumType)
	// Flat shape cannot carry a storage pool.
	assert.Equal(t, "", src.StoragePool)
}

// TestParseLibvirtImageSource_FlatURL verifies the flat shape's URL field parses.
func TestParseLibvirtImageSource_FlatURL(t *testing.T) {
	img := contracts.VMImage{URL: "https://example.com/img.qcow2"}
	raw, err := json.Marshal(img)
	require.NoError(t, err)

	src := parseLibvirtImageSource(string(raw))

	assert.Equal(t, "https://example.com/img.qcow2", src.URL)
	assert.Equal(t, "", src.Path)
}

// TestParseLibvirtImageSource_RichSpecWins verifies that when a payload carries
// both a usable nested source.libvirt and flat-shaped fields, the rich shape wins
// (it is tried first and is the only one expressing a storage pool).
func TestParseLibvirtImageSource_RichSpecWins(t *testing.T) {
	// Hand-built payload that satisfies both unmarshals; the rich source.libvirt
	// has a url+pool, the flat Path is set too. Rich must win.
	imageJSON := `{
		"source":{"libvirt":{"url":"https://rich/url.qcow2","storagePool":"richpool"}},
		"Path":"/flat/path.qcow2"
	}`

	src := parseLibvirtImageSource(imageJSON)

	assert.Equal(t, "https://rich/url.qcow2", src.URL)
	assert.Equal(t, "richpool", src.StoragePool)
	assert.Equal(t, "", src.Path, "rich source.libvirt should win over the flat Path field")
}

// TestParseLibvirtImageSource_Empty verifies empty / whitespace input yields an
// empty source (the caller turns this into an InvalidSpec error).
func TestParseLibvirtImageSource_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "{}", `{"source":{}}`, `{"source":{"libvirt":{}}}`} {
		src := parseLibvirtImageSource(in)
		assert.Equal(t, "", src.Path, "input %q", in)
		assert.Equal(t, "", src.URL, "input %q", in)
	}
}

// TestParseLibvirtImageSource_Unparseable verifies malformed JSON yields an empty
// source rather than panicking.
func TestParseLibvirtImageSource_Unparseable(t *testing.T) {
	src := parseLibvirtImageSource(`{not json`)
	assert.Equal(t, "", src.Path)
	assert.Equal(t, "", src.URL)
}

// TestResolveTargetPool covers the storage-pool precedence: StorageHint over
// source pool over the provider default.
func TestResolveTargetPool(t *testing.T) {
	tests := []struct {
		name        string
		storageHint string
		sourcePool  string
		want        string
	}{
		{"hint wins over source", "hintpool", "srcpool", "hintpool"},
		{"hint wins over empty source", "hintpool", "", "hintpool"},
		{"source used when no hint", "", "srcpool", "srcpool"},
		{"default when neither", "", "", defaultStoragePool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveTargetPool(tt.storageHint, tt.sourcePool))
		})
	}
}

// TestTargetImagePath verifies the target file is <poolPath>/<targetName>.qcow2.
func TestTargetImagePath(t *testing.T) {
	assert.Equal(t,
		"/var/lib/libvirt/images/fedora-tmpl.qcow2",
		targetImagePath("/var/lib/libvirt/images", "fedora-tmpl"))
	// Trailing slash on the pool path is normalized by filepath.Join.
	assert.Equal(t,
		"/pool/jammy.qcow2",
		targetImagePath("/pool/", "jammy"))
}

// TestChecksumTool verifies algorithm-to-binary mapping, the sha256 default, and
// rejection of an unknown algorithm.
func TestChecksumTool(t *testing.T) {
	tests := []struct {
		in       string
		wantTool string
		wantOK   bool
	}{
		{"", "sha256sum", true},
		{"sha256", "sha256sum", true},
		{"SHA256", "sha256sum", true},
		{"md5", "md5sum", true},
		{"sha1", "sha1sum", true},
		{"sha512", "sha512sum", true},
		{"  sha512  ", "sha512sum", true},
		{"crc32", "sha256sum", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			tool, ok := checksumTool(tt.in)
			assert.Equal(t, tt.wantTool, tool)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

// TestImagePrepare_NilVirshProvider verifies the guard fires before any host
// interaction when the inner virsh provider is missing.
func TestImagePrepare_NilVirshProvider(t *testing.T) {
	p := &Provider{}
	_, _, err := p.imagePrepare(context.Background(),
		`{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`, "tmpl", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestImagePrepare_MissingTargetName verifies the target-name guard, which fires
// before any host interaction (so it is host-independent).
func TestImagePrepare_MissingTargetName(t *testing.T) {
	// A non-nil virshProvider is required to pass the first guard; an empty one is
	// sufficient because the target-name check short-circuits before any command
	// runs.
	p := &Provider{virshProvider: &VirshProvider{}}
	_, _, err := p.imagePrepare(context.Background(),
		`{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`, "  ", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target name is required")
}

// TestImagePrepare_NoSource verifies the InvalidSpec path when the image spec has
// neither a path nor a url. This fires before any host interaction.
func TestImagePrepare_NoSource(t *testing.T) {
	p := &Provider{virshProvider: &VirshProvider{}}
	_, _, err := p.imagePrepare(context.Background(), `{"source":{"libvirt":{}}}`, "tmpl", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path or url")

	// Empty image JSON is likewise rejected (no fabricated success).
	_, _, err = p.imagePrepare(context.Background(), "", "tmpl", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path or url")
}

// TestServer_ImagePrepare_NilProvider_Direct mirrors the server-layer nil guard
// at the RPC boundary (complements TestServer_ImagePrepare_NilProvider in
// server_test.go by exercising a populated request).
func TestServer_ImagePrepare_NilProvider_Direct(t *testing.T) {
	s := &Server{}
	resp, err := s.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:   `{"source":{"libvirt":{"url":"https://x/y.qcow2"}}}`,
		TargetName:  "tmpl",
		StorageHint: "pool",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}
