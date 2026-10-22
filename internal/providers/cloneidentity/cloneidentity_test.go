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

package cloneidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

func TestInstanceID(t *testing.T) {
	a := contracts.ObjectIdentity{UID: "6b1c2d3e-4f50-4a61-8b72-9c8d7e6f5a4b", Namespace: "team-a", Name: "copy"}
	assert.Equal(t, "virtrigaud-6b1c2d3e-4f50-4a61-8b72-9c8d7e6f5a4b", InstanceID(a, "team-a.copy"))
	assert.Equal(t, InstanceID(a, "team-a.copy"), InstanceID(a, "other"), "derived from the UID alone: a re-sent Clone renders the same id")

	b := a
	b.UID = "11111111-2222-3333-4444-555555555555"
	assert.NotEqual(t, InstanceID(a, "team-a.copy"), InstanceID(b, "team-a.copy"),
		"a new VirtualMachine of the same namespace and name is a new instance")

	assert.Equal(t, "virtrigaud-team-a.copy", InstanceID(contracts.ObjectIdentity{Namespace: "team-a", Name: "copy"}, "team-a.copy"),
		"without a UID (older manager) the provider's name for the clone")
	assert.Equal(t, InstanceID(contracts.ObjectIdentity{UID: " "}, "x"), "virtrigaud-x")
}

func TestParseCustomization(t *testing.T) {
	c, err := ParseCustomization("")
	require.NoError(t, err)
	assert.Equal(t, Customization{}, c)

	c, err = ParseCustomization(`{"hostname":"web2","domain":"example.com.","userData":{"cloudInit":{"inline":"#cloud-config\n"}},` +
		`"timeZone":"UTC","networks":[{"name":"n"}],"tags":[],"sysprep":null}`)
	require.NoError(t, err)
	assert.Equal(t, "web2", c.Hostname)
	assert.Equal(t, "example.com", c.Domain, "a trailing dot is dropped")
	assert.Equal(t, "#cloud-config\n", c.InlineUserData)
	assert.False(t, c.SecretUserData)
	assert.Equal(t, []string{"networks", "timeZone"}, c.Unapplied, "set fields that are not identity; empty ones are not listed")

	c, err = ParseCustomization(`{"userData":{"cloudInit":{"secretRef":{"name":"s"}},"ignition":{"inline":"{}"}}}`)
	require.NoError(t, err)
	assert.True(t, c.SecretUserData)
	assert.Empty(t, c.InlineUserData)
	assert.Equal(t, []string{"userData.ignition"}, c.Unapplied)

	_, err = ParseCustomization("{not json")
	assert.Error(t, err)
}

func TestHostname(t *testing.T) {
	target := contracts.ObjectIdentity{Namespace: "team-a", Name: "copy"}
	assert.Equal(t, "custom", Hostname(Customization{Hostname: "custom"}, target, "x"))
	assert.Equal(t, "copy", Hostname(Customization{}, target, "x"))
	assert.Equal(t, "x", Hostname(Customization{}, contracts.ObjectIdentity{}, "x"))
	assert.Equal(t, "copy.example.com", LocalHostname("copy", "example.com"))
	assert.Equal(t, "copy", LocalHostname("copy", ""))
}

// metaMap parses a meta-data document for assertions.
func metaMap(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(doc), &m), "%s", doc)
	return m
}

func TestRewriteMetaData(t *testing.T) {
	t.Run("the provider's own meta-data keeps its network config", func(t *testing.T) {
		src := "instance-id: team-a.web\nlocal-hostname: web\nnetwork:\n  version: 1\n  config:\n    # comment kept\n" +
			"    - type: physical\n      name: eth0\n"
		out, err := RewriteMetaData(src, "virtrigaud-uid", "copy")
		require.NoError(t, err)
		m := metaMap(t, out)
		assert.Equal(t, "virtrigaud-uid", m["instance-id"])
		assert.Equal(t, "copy", m["local-hostname"])
		assert.NotContains(t, m, "hostname", "a hostname key is only rewritten, never added")
		assert.Contains(t, out, "# comment kept")
		assert.Contains(t, out, "name: eth0")
		assert.NotContains(t, out, "team-a.web")

		again, err := RewriteMetaData(src, "virtrigaud-uid", "copy")
		require.NoError(t, err)
		assert.Equal(t, out, again, "deterministic")
	})
	t.Run("JSON meta-data", func(t *testing.T) {
		out, err := RewriteMetaData(`{"instance-id": "i-1", "hostname": "web", "region": "x"}`, "virtrigaud-uid", "copy.example.com")
		require.NoError(t, err)
		m := metaMap(t, out)
		assert.Equal(t, "virtrigaud-uid", m["instance-id"])
		assert.Equal(t, "copy.example.com", m["local-hostname"])
		assert.Equal(t, "copy.example.com", m["hostname"])
		assert.Equal(t, "x", m["region"])
	})
	t.Run("empty and missing keys", func(t *testing.T) {
		out, err := RewriteMetaData("", "virtrigaud-uid", "copy")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"instance-id": "virtrigaud-uid", "local-hostname": "copy"}, metaMap(t, out))

		out, err = RewriteMetaData("local-hostname: web\n", "virtrigaud-uid", "copy")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"instance-id": "virtrigaud-uid", "local-hostname": "copy"}, metaMap(t, out))
	})
	t.Run("values that would read as another type stay strings", func(t *testing.T) {
		out, err := RewriteMetaData("instance-id: x\n", "12345", "true")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"instance-id": "12345", "local-hostname": "true"}, metaMap(t, out))
	})
	t.Run("not a mapping", func(t *testing.T) {
		for _, src := range []string{"- a\n- b\n", "just text", "a: [unclosed"} {
			_, err := RewriteMetaData(src, "virtrigaud-uid", "copy")
			assert.Error(t, err, "%q", src)
		}
	})
}

func TestRewriteUserDataHostname(t *testing.T) {
	t.Run("the provider's default user-data", func(t *testing.T) {
		src := "#cloud-config\nhostname: web\n\npackages:\n  - qemu-guest-agent\n"
		out, changed, skipped := RewriteUserDataHostname(src, "copy", "")
		assert.True(t, changed)
		assert.Empty(t, skipped)
		assert.Equal(t, "#cloud-config\nhostname: \"copy\"\n\npackages:\n  - qemu-guest-agent\n", out)
	})
	t.Run("fqdn keeps the old domain unless one is given", func(t *testing.T) {
		src := "#cloud-config\r\nhostname: 'web' # the name\r\nfqdn: web.corp.example\r\nusers:\r\n  - name: ops\r\n    hostname: nested\r\n"
		out, changed, _ := RewriteUserDataHostname(src, "copy", "")
		assert.True(t, changed)
		assert.Equal(t, "#cloud-config\r\nhostname: \"copy\"\r\nfqdn: \"copy.corp.example\"\r\nusers:\r\n  - name: ops\r\n    hostname: nested\r\n", out,
			"CRLF kept; nested keys untouched")

		out, _, _ = RewriteUserDataHostname(src, "copy", "lab.example")
		assert.Contains(t, out, "fqdn: \"copy.lab.example\"")

		out, _, _ = RewriteUserDataHostname("#cloud-config\nfqdn: web\n", "copy", "")
		assert.Equal(t, "#cloud-config\nfqdn: \"copy\"\n", out)
	})
	t.Run("left untouched", func(t *testing.T) {
		for name, src := range map[string]string{
			"no hostname":       "#cloud-config\npackages: [curl]\n",
			"a shell script":    "#!/bin/sh\nhostname: web\n",
			"MIME multi-part":   "Content-Type: multipart/mixed\nhostname: web\n",
			"a jinja template":  "## template: jinja\n#cloud-config\nhostname: web\n",
			"a similar key":     "#cloud-config\nhostnames: [a]\nhostname_x: y\n",
			"no space after it": "#cloud-config\nhostname:web\n",
		} {
			out, changed, _ := RewriteUserDataHostname(src, "copy", "")
			assert.False(t, changed, name)
			assert.Equal(t, src, out, name)
		}
	})
	t.Run("a value that is not a plain one-line scalar is skipped", func(t *testing.T) {
		src := "#cloud-config\nhostname: |\n  web\nfqdn: *anchor\n"
		out, changed, skipped := RewriteUserDataHostname(src, "copy", "")
		assert.False(t, changed)
		assert.Equal(t, src, out)
		assert.Equal(t, []string{"hostname", "fqdn"}, skipped)
	})
	t.Run("the rewrite is valid YAML", func(t *testing.T) {
		out, _, _ := RewriteUserDataHostname("#cloud-config\nhostname: web\nfqdn: \"web.x\"\n", "a\"b", "")
		var m map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(out), &m))
		assert.Equal(t, "a\"b", m["hostname"])
		assert.Equal(t, "a\"b.x", m["fqdn"])
	})
}
