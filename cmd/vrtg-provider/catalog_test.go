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

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

// catalogFile is providers/catalog.yaml as vrtg-provider reads and writes it,
// plus the descriptive schema block it carries.
type catalogFile struct {
	ProviderCatalog `yaml:",inline"`
	Schema          yaml.MapSlice `yaml:"schema"`
}

var (
	catalogProfiles = []string{"core", "snapshot", "clone", "image-prepare", "advanced"}
	releaseTag      = regexp.MustCompile(`^v\d+\.\d+\.\d+(-rc\d+)?$`)
)

// TestRepositoryCatalog checks the catalog this repository ships: it parses
// with the structure vrtg-provider publishes into, names a release tag (not a
// stale 0.1.x), and claims no conformance result that no run backs.
func TestRepositoryCatalog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoCheckout(t), "providers", "catalog.yaml"))
	require.NoError(t, err)

	var catalog catalogFile
	require.NoError(t, yaml.UnmarshalStrict(data, &catalog))
	assert.Equal(t, "v1", catalog.Metadata.Version)
	_, err = time.Parse(time.RFC3339, catalog.Metadata.LastUpdated)
	require.NoError(t, err, "metadata.lastUpdated must be RFC 3339")
	require.NotEmpty(t, catalog.Providers)

	// The CI job "Catalog Validation" requires these three of every entry.
	names := map[string]bool{}
	for _, p := range catalog.Providers {
		t.Run(p.Name, func(t *testing.T) {
			assert.NotEmpty(t, p.Name)
			assert.NotEmpty(t, p.DisplayName)
			assert.NotEmpty(t, p.Description)
			assert.False(t, names[p.Name], "duplicate provider %q", p.Name)
			names[p.Name] = true

			// An entry that names an image names a release tag of it; an
			// entry without a published image has neither.
			if p.Image == "" {
				assert.Empty(t, p.Tag)
			} else {
				assert.Regexp(t, releaseTag, p.Tag)
			}

			assert.Contains(t, p.Capabilities, "core")
			for _, capability := range p.Capabilities {
				assert.Contains(t, catalogProfiles, capability)
				assert.Contains(t, p.Conformance.Profiles, capability, "every listed profile has a conformance entry")
			}
			assert.Len(t, p.Conformance.Profiles, len(p.Capabilities), "a conformance entry for a profile that is not listed")

			// A result is claimed only with the run that backs it.
			for profile, status := range p.Conformance.Profiles {
				if status == "skip" {
					continue
				}
				assert.NotEmpty(t, p.Conformance.ReportURL, "profile %s is %q without a report", profile, status)
				assert.NotEmpty(t, p.Conformance.LastTested, "profile %s is %q without a date", profile, status)
			}
			if p.Conformance.LastTested != "" {
				_, parseErr := time.Parse(time.RFC3339, p.Conformance.LastTested)
				assert.NoError(t, parseErr)
			}
			assert.Equal(t, generateBadgeURL(p.Conformance.Profiles), p.Conformance.BadgeURL,
				"the badge must be the one the profiles produce")
		})
	}
}
