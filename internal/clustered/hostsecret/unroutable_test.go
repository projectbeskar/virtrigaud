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

package hostsecret

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarshal_UnroutableHostIDs pins the id-only tombstones (ADR-0007 A6.1
// review, item 2a): sorted, deduplicated, empty entries dropped, no endpoint or
// credential key, and omitted entirely when there are none — so an inventory
// without tombstones renders byte for byte as before.
func TestMarshal_UnroutableHostIDs(t *testing.T) {
	base := Inventory{SchemaVersion: SchemaVersion, Hosts: []Host{{ID: "host-a", Endpoint: "e"}}}
	without, err := Marshal(base)
	require.NoError(t, err)
	assert.NotContains(t, string(without), "unroutableHostIds", "omitted when empty")

	withEmpty := base
	withEmpty.UnroutableHostIDs = []string{"", ""}
	same, err := Marshal(withEmpty)
	require.NoError(t, err)
	assert.Equal(t, string(without), string(same), "only empty ids: nothing rendered")

	with := base
	with.UnroutableHostIDs = []string{"host-z", "host-c", "host-z"}
	data, err := Marshal(with)
	require.NoError(t, err)
	got, err := Unmarshal(data)
	require.NoError(t, err)
	assert.Equal(t, []string{"host-c", "host-z"}, got.UnroutableHostIDs)
	assert.Len(t, got.Hosts, 1)
	assert.Equal(t, []string{"host-z", "host-c", "host-z"}, with.UnroutableHostIDs, "the caller's slice is not reordered")
}
