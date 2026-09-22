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
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConstants pins the shared contract values the provider (a later PR) will
// rely on. Changing any of these is a schema/mount-path break and must be a
// deliberate, versioned decision.
func TestConstants(t *testing.T) {
	assert.Equal(t, 1, SchemaVersion, "SchemaVersion must be 1 for the first slice")
	assert.Equal(t, "hosts.json", SecretDataKey)
	assert.Equal(t, "/etc/virtrigaud/hosts", MountPath)
}

// TestMarshal_SortsHostsByID proves the rendering is deterministic regardless of
// the order in which Hosts were gathered: input c,a,b must render a,b,c.
func TestMarshal_SortsHostsByID(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "host-c", Endpoint: "qemu+ssh://virt@c/system"},
			{ID: "host-a", Endpoint: "qemu+ssh://virt@a/system"},
			{ID: "host-b", Endpoint: "qemu+ssh://virt@b/system"},
		},
	}

	data, err := Marshal(inv)
	require.NoError(t, err)

	got, err := Unmarshal(data)
	require.NoError(t, err)
	require.Len(t, got.Hosts, 3)
	assert.Equal(t, "host-a", got.Hosts[0].ID)
	assert.Equal(t, "host-b", got.Hosts[1].ID)
	assert.Equal(t, "host-c", got.Hosts[2].ID)

	// The ordering must also be visible in the raw byte stream (not just after a
	// re-parse), since it is the byte stream that lands in the Secret.
	s := string(data)
	assert.Less(t, strings.Index(s, "host-a"), strings.Index(s, "host-b"))
	assert.Less(t, strings.Index(s, "host-b"), strings.Index(s, "host-c"))
}

// TestMarshal_Deterministic proves two inventories that differ only in host
// ordering render to byte-identical Secrets — the property that makes
// re-reconciles diff-stable and avoids spurious Secret updates.
func TestMarshal_Deterministic(t *testing.T) {
	a := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "h2", Endpoint: "e2", Labels: map[string]string{"z": "1", "a": "2"}},
			{ID: "h1", Endpoint: "e1", Labels: map[string]string{"a": "2", "z": "1"}},
		},
	}
	b := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "h1", Endpoint: "e1", Labels: map[string]string{"z": "1", "a": "2"}},
			{ID: "h2", Endpoint: "e2", Labels: map[string]string{"a": "2", "z": "1"}},
		},
	}

	da, err := Marshal(a)
	require.NoError(t, err)
	db, err := Marshal(b)
	require.NoError(t, err)
	assert.Equal(t, string(da), string(db), "host order and label-map order must not affect the rendered bytes")
}

// TestMarshal_DoesNotMutateInput guards against Marshal reordering the caller's
// slice in place (it sorts a copy).
func TestMarshal_DoesNotMutateInput(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "host-c"},
			{ID: "host-a"},
			{ID: "host-b"},
		},
	}
	_, err := Marshal(inv)
	require.NoError(t, err)
	assert.Equal(t, "host-c", inv.Hosts[0].ID, "Marshal must not sort the caller's slice in place")
	assert.Equal(t, "host-a", inv.Hosts[1].ID)
	assert.Equal(t, "host-b", inv.Hosts[2].ID)
}

// TestMarshal_EmptyInventory proves an empty inventory renders "hosts": [] (never
// null), so a cluster provider with zero registered Hosts still gets a
// well-formed, stable document.
func TestMarshal_EmptyInventory(t *testing.T) {
	data, err := Marshal(Inventory{SchemaVersion: SchemaVersion})
	require.NoError(t, err)

	s := string(data)
	assert.Contains(t, s, `"hosts": []`, "empty inventory must render an empty array, not null")
	assert.NotContains(t, s, "null")

	got, err := Unmarshal(data)
	require.NoError(t, err)
	assert.Equal(t, SchemaVersion, got.SchemaVersion)
	assert.Empty(t, got.Hosts)
}

// TestMarshal_CredentialsAlwaysPresentButEmpty proves the schema-stability
// invariant: the credentials key is ALWAYS present, and for a host with no
// populated Credentials it renders as an empty object ({}) rather than null or a
// missing key — so a consumer can always rely on the key existing.
func TestMarshal_CredentialsAlwaysPresentButEmpty(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts:         []Host{{ID: "host-a", Endpoint: "e", Labels: map[string]string{"k": "v"}}},
	}
	data, err := Marshal(inv)
	require.NoError(t, err)

	// Structural assertion on the raw JSON: credentials present and empty.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	hosts, ok := raw["hosts"].([]any)
	require.True(t, ok)
	require.Len(t, hosts, 1)
	host, ok := hosts[0].(map[string]any)
	require.True(t, ok)
	creds, ok := host["credentials"]
	require.True(t, ok, "credentials key must always be present")
	assert.Equal(t, map[string]any{}, creds, "credentials must be an empty object in the structural PR")
}

// TestRoundTrip proves Unmarshal(Marshal(x)) preserves the data (modulo the
// deterministic host ordering Marshal imposes).
func TestRoundTrip(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "host-b", Endpoint: "qemu+ssh://virt@b/system", Labels: map[string]string{"rack": "r7"}},
			{ID: "host-a", Endpoint: "qemu+ssh://virt@a/system"},
		},
	}
	data, err := Marshal(inv)
	require.NoError(t, err)

	got, err := Unmarshal(data)
	require.NoError(t, err)
	require.Len(t, got.Hosts, 2)
	assert.Equal(t, "host-a", got.Hosts[0].ID)
	assert.Equal(t, "host-b", got.Hosts[1].ID)
	assert.Equal(t, "qemu+ssh://virt@b/system", got.Hosts[1].Endpoint)
	assert.Equal(t, map[string]string{"rack": "r7"}, got.Hosts[1].Labels)
	assert.Equal(t, Credentials{}, got.Hosts[1].Credentials)
}

// TestUnmarshal_Invalid returns an error rather than a partially-populated
// Inventory on malformed input.
func TestUnmarshal_Invalid(t *testing.T) {
	_, err := Unmarshal([]byte("{not json"))
	require.Error(t, err)
}

// TestCredentials_RoundTripByteForByte proves inlined credential material — a
// multi-line PEM private key and a known_hosts entry — survives Marshal→Unmarshal
// byte-for-byte, with no PEM/newline/base64 corruption. This is the property the
// clustered provider relies on when it later parses the key from the mounted file
// through the same ssh.ParsePrivateKey / knownhosts path it uses today.
func TestCredentials_RoundTripByteForByte(t *testing.T) {
	// Realistic shapes (NOT real keys): embedded newlines, trailing newline, and
	// a base64-ish body are exactly what would trip up a careless string/byte or
	// double-base64 conversion.
	privateKey := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQyNTUxOQAAACAt+FAKE+MATERIAL+ONLY==\n-----END OPENSSH PRIVATE KEY-----\n")
	knownHosts := []byte("host-a ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFAKEKNOWNHOSTSLINE\n")

	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{{
			ID:       "host-a",
			Endpoint: "qemu+ssh://virt@host-a/system",
			Credentials: Credentials{
				SSHPrivateKey: privateKey,
				KnownHosts:    knownHosts,
			},
		}},
	}

	data, err := Marshal(inv)
	require.NoError(t, err)

	got, err := Unmarshal(data)
	require.NoError(t, err)
	require.Len(t, got.Hosts, 1)
	assert.True(t, bytes.Equal(privateKey, got.Hosts[0].Credentials.SSHPrivateKey),
		"SSH private key must round-trip byte-for-byte")
	assert.True(t, bytes.Equal(knownHosts, got.Hosts[0].Credentials.KnownHosts),
		"known_hosts must round-trip byte-for-byte")
}

// TestCredentials_OmitEmpty proves an unpopulated field is omitted (credentials
// {} when both empty), while a populated field is present — so a rendered host
// with a key but no known_hosts carries exactly sshPrivateKey and no knownHosts
// key.
func TestCredentials_OmitEmpty(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "empty", Endpoint: "e"},
			{ID: "keyonly", Endpoint: "e", Credentials: Credentials{SSHPrivateKey: []byte("KEY")}},
		},
	}
	data, err := Marshal(inv)
	require.NoError(t, err)

	var raw struct {
		Hosts []struct {
			ID          string         `json:"id"`
			Credentials map[string]any `json:"credentials"`
		} `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Len(t, raw.Hosts, 2)

	// hosts are sorted by id: "empty" < "keyonly".
	assert.Equal(t, map[string]any{}, raw.Hosts[0].Credentials, "empty credentials render as {}")
	_, hasKey := raw.Hosts[1].Credentials["sshPrivateKey"]
	assert.True(t, hasKey, "populated key must be present")
	_, hasKH := raw.Hosts[1].Credentials["knownHosts"]
	assert.False(t, hasKH, "absent known_hosts must be omitted, not rendered null/empty")
}
