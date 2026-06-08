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

package proxmox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestParseProxmoxImageSource_RichProxmox verifies the rich v1beta1.VMImageSpec
// shape (nested source.proxmox) is parsed including the Proxmox-native fields that
// only this shape can express.
func TestParseProxmoxImageSource_RichProxmox(t *testing.T) {
	imageJSON := `{
		"source": {
			"proxmox": {
				"templateID": 9001,
				"templateName": "rocky-9-template",
				"storage": "vms",
				"node": "pve2",
				"format": "raw"
			}
		}
	}`

	src := parseProxmoxImageSource(imageJSON)

	require.NotNil(t, src.TemplateID)
	assert.Equal(t, 9001, *src.TemplateID)
	assert.Equal(t, "rocky-9-template", src.TemplateName)
	assert.Equal(t, "vms", src.Storage)
	assert.Equal(t, "pve2", src.Node)
	assert.Equal(t, "raw", src.Format)
	assert.Equal(t, "", src.URL)
	assert.True(t, src.referencesExistingTemplate())
	assert.False(t, src.isEmpty())
}

// TestParseProxmoxImageSource_HTTP verifies the generic source.http.url import
// shape is parsed.
func TestParseProxmoxImageSource_HTTP(t *testing.T) {
	imageJSON := `{"source":{"http":{"url":"https://images.example.com/jammy.img"}}}`

	src := parseProxmoxImageSource(imageJSON)

	assert.Equal(t, "https://images.example.com/jammy.img", src.URL)
	assert.Nil(t, src.TemplateID)
	assert.Equal(t, "", src.TemplateName)
	assert.False(t, src.referencesExistingTemplate())
	assert.False(t, src.isEmpty())
}

// TestParseProxmoxImageSource_ProxmoxWinsOverHTTP verifies that when both a
// source.proxmox existing-template reference and a source.http import URL appear,
// the existing-template reference takes precedence (verify-only beats import).
func TestParseProxmoxImageSource_ProxmoxWinsOverHTTP(t *testing.T) {
	imageJSON := `{
		"source": {
			"proxmox": {"templateName": "base"},
			"http": {"url": "https://images.example.com/jammy.img"}
		}
	}`

	src := parseProxmoxImageSource(imageJSON)

	assert.Equal(t, "base", src.TemplateName)
	assert.True(t, src.referencesExistingTemplate())
	// The URL is still captured; the caller's precedence (referencesExistingTemplate
	// first) is what makes this verify-only.
	assert.Equal(t, "https://images.example.com/jammy.img", src.URL)
}

// TestParseProxmoxImageSource_FlatContractsVMImage verifies the fallback flat
// contracts.VMImage shape (Go field names) is parsed when no nested source is
// present. This is the shape Create already round-trips.
func TestParseProxmoxImageSource_FlatContractsVMImage(t *testing.T) {
	img := contracts.VMImage{
		TemplateName: "win2022-template",
		Format:       "qcow2",
	}
	raw, err := json.Marshal(img)
	require.NoError(t, err)

	src := parseProxmoxImageSource(string(raw))

	assert.Equal(t, "win2022-template", src.TemplateName)
	assert.Equal(t, "qcow2", src.Format)
	assert.True(t, src.referencesExistingTemplate())
}

// TestParseProxmoxImageSource_FlatContractsURL verifies the flat shape's URL field
// is treated as an import source.
func TestParseProxmoxImageSource_FlatContractsURL(t *testing.T) {
	img := contracts.VMImage{
		URL:    "https://images.example.com/debian.img",
		Format: "qcow2",
	}
	raw, err := json.Marshal(img)
	require.NoError(t, err)

	src := parseProxmoxImageSource(string(raw))

	assert.Equal(t, "https://images.example.com/debian.img", src.URL)
	assert.False(t, src.referencesExistingTemplate())
	assert.False(t, src.isEmpty())
}

// TestParseProxmoxImageSource_Empty verifies that empty, malformed, or
// source-less specs yield an empty source so the caller can return InvalidSpec.
func TestParseProxmoxImageSource_Empty(t *testing.T) {
	cases := map[string]string{
		"empty string":     "",
		"whitespace":       "   ",
		"empty object":     "{}",
		"empty source":     `{"source":{}}`,
		"empty proxmox":    `{"source":{"proxmox":{}}}`,
		"unrelated source": `{"source":{"registry":{"image":"foo:bar"}}}`,
		"malformed json":   `{"source":`,
	}
	for name, imageJSON := range cases {
		t.Run(name, func(t *testing.T) {
			src := parseProxmoxImageSource(imageJSON)
			assert.True(t, src.isEmpty(), "expected empty source for %q", imageJSON)
		})
	}
}

// TestResolveImageStorage verifies the hint > source > default precedence.
func TestResolveImageStorage(t *testing.T) {
	assert.Equal(t, "hint", resolveImageStorage("hint", "src"))
	assert.Equal(t, "src", resolveImageStorage("", "src"))
	assert.Equal(t, "src", resolveImageStorage("   ", "src"))
	assert.Equal(t, defaultProxmoxStorage, resolveImageStorage("", ""))
	assert.Equal(t, defaultProxmoxStorage, resolveImageStorage("  ", "  "))
}
