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
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSHKeyAuthOptions pins the exact ssh/scp flag list used by every key-based
// call site. This is the regression guard for the bug class that motivated the
// fix: a hand-rolled ssh/scp that "supports key auth" but silently omits -i (the
// key lives at a non-default path with no IdentityFile in the config).
func TestSSHKeyAuthOptions(t *testing.T) {
	assert.Equal(t,
		[]string{
			"-i", "/tmp/k",
			"-o", "IdentitiesOnly=yes",
			"-o", "PasswordAuthentication=no",
			"-o", "PubkeyAuthentication=yes",
		},
		sshKeyAuthOptions("/tmp/k"),
	)
}

// TestResolveSSHKeyFile covers both sources of the key path: the keyfile= pinned
// on the libvirt URI (preferred) and the well-known fallback.
func TestResolveSSHKeyFile(t *testing.T) {
	withKeyfile, err := url.Parse("qemu+ssh://u@h/system?keyfile=/custom/key&sshauth=privkey")
	require.NoError(t, err)
	assert.Equal(t, "/custom/key", resolveSSHKeyFile(withKeyfile))

	noKeyfile, err := url.Parse("qemu+ssh://u@h/system")
	require.NoError(t, err)
	assert.Equal(t, sshPrivateKeyPath, resolveSSHKeyFile(noKeyfile))
}

// TestWriteSSHPrivateKey asserts the key is persisted (trimmed, single trailing
// newline) at the well-known path with 0600 perms in a 0700 dir, so both the
// direct ssh/scp (-i) paths and libvirt's own keyfile= transport can read it.
func TestWriteSSHPrivateKey(t *testing.T) {
	require.NoError(t, os.RemoveAll(sshPrivateKeyDir))
	t.Cleanup(func() { _ = os.RemoveAll(sshPrivateKeyDir) })

	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc123\n-----END OPENSSH PRIVATE KEY-----"
	v := &VirshProvider{credentials: &Credentials{SSHPrivateKey: pem}}

	path, err := v.writeSSHPrivateKey()
	require.NoError(t, err)
	assert.Equal(t, sshPrivateKeyPath, path)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, pem+"\n", string(got))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	di, err := os.Stat(sshPrivateKeyDir)
	require.NoError(t, err)
	assert.Zero(t, di.Mode().Perm()&0o077, "key dir must not be group/other accessible")
}

// TestSetupConnection_KeyAuth_WritesKeyAndPinsURI is the integration point for the
// key path: with a private key configured, setupConnection must (a) write the key
// to disk and (b) pin keyfile=/sshauth=privkey on the URI so libvirt's own
// qemu+ssh transport authenticates with it.
func TestSetupConnection_KeyAuth_WritesKeyAndPinsURI(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "true") // skip known_hosts hard-fail
	require.NoError(t, os.RemoveAll(sshPrivateKeyDir))
	t.Cleanup(func() { _ = os.RemoveAll(sshPrivateKeyDir) })

	v := &VirshProvider{
		config:      &ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu+ssh://virtrigaud@172.16.56.8/system"}},
		credentials: &Credentials{Username: "virtrigaud", SSHPrivateKey: "FAKE-KEY"},
	}

	require.NoError(t, v.setupConnection())

	assert.Contains(t, v.uri, "keyfile="+url.QueryEscape(sshPrivateKeyPath))
	assert.Contains(t, v.uri, "sshauth=privkey")

	got, err := os.ReadFile(sshPrivateKeyPath)
	require.NoError(t, err)
	assert.Equal(t, "FAKE-KEY\n", string(got))
}

// TestSetupConnection_PasswordAuth_NoKeyfile is the negative guard: the key logic
// is gated on SSHPrivateKey, so a password-only connection must NOT pin keyfile=
// on the URI nor leave a key file behind.
func TestSetupConnection_PasswordAuth_NoKeyfile(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "true")
	require.NoError(t, os.RemoveAll(sshPrivateKeyDir))
	t.Cleanup(func() { _ = os.RemoveAll(sshPrivateKeyDir) })

	v := &VirshProvider{
		config:      &ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu+ssh://virtrigaud@172.16.56.8/system"}},
		credentials: &Credentials{Username: "virtrigaud", Password: "secret"},
	}

	require.NoError(t, v.setupConnection())

	assert.NotContains(t, v.uri, "keyfile=")
	assert.NotContains(t, v.uri, "sshauth=")

	_, err := os.Stat(sshPrivateKeyPath)
	assert.True(t, os.IsNotExist(err), "no private key file should be written for password auth")
}

// TestSSHConfigStanza_EnablesPubkeyAuth guards the no->yes flip: the key-based
// qemu+ssh transport reads ~/.ssh/config, so PubkeyAuthentication must be on for
// both host-key policies.
func TestSSHConfigStanza_EnablesPubkeyAuth(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		stanza := hostKeyPolicy{insecure: insecure}.sshConfigStanza()
		assert.Contains(t, stanza, "PubkeyAuthentication yes")
		assert.NotContains(t, stanza, "PubkeyAuthentication no")
	}
}

// TestRemoteVirshConnectURI pins the driver+path a remote-side `virsh -c` must
// target, so a remote virsh hits the SAME libvirtd as the rest of the provider
// regardless of whether the ssh user is root (default system) or non-root
// (default session). Query params (keyfile/no_tty/…) and the ssh transport/host
// must all be stripped.
func TestRemoteVirshConnectURI(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root system", "qemu+ssh://root@host/system", "qemu:///system"},
		{"non-root session", "qemu+ssh://user@host/session", "qemu:///session"},
		{"non-root system in libvirt group", "qemu+ssh://libvirtuser@host/system", "qemu:///system"},
		{"strips query", "qemu+ssh://host/system?keyfile=%2Fk&no_tty=1&sshauth=privkey", "qemu:///system"},
		{"local uri", "qemu:///system", "qemu:///system"},
		{"other driver", "test+ssh://host/default", "test:///default"},
		{"no path -> empty (legacy fallback)", "qemu+ssh://host", ""},
		{"empty path -> empty", "qemu+ssh://host/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, remoteVirshConnectURI(tc.in))
		})
	}
}
