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
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optsToString flattens the alternating ["-o", "k=v", ...] option slice into a
// single space-joined string for substring assertions.
func optsToString(opts []string) string {
	return strings.Join(opts, " ")
}

// TestSSHHostFromEndpoint pins the host derivation from the PVE API endpoint: the
// SSH host is the SAME host as the API endpoint, with the API port (8006)
// stripped (SSH uses 22).
func TestSSHHostFromEndpoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with port", "https://pve.lab.k8:8006", "pve.lab.k8"},
		{"https no port", "https://pve.lab.k8", "pve.lab.k8"},
		{"bare host with port", "pve.lab.k8:8006", "pve.lab.k8"},
		{"bare host", "pve.lab.k8", "pve.lab.k8"},
		{"ip with port", "https://10.0.0.5:8006", "10.0.0.5"},
		{"trailing path", "https://pve.lab.k8:8006/api2/json", "pve.lab.k8"},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sshHostFromEndpoint(tc.in))
		})
	}
}

// TestResolveProxmoxHostKeyPolicy_EnvParsing verifies the escape-hatch env var is
// honoured ONLY for the literal word "true" (case-insensitive, trimmed) and keeps
// verification ON for every other value (mirrors ADR-0003 / #149 semantics).
func TestResolveProxmoxHostKeyPolicy_EnvParsing(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		wantInsecure bool
	}{
		{"empty -> verify", "", false},
		{"false -> verify", "false", false},
		{"1 -> verify (not the word true)", "1", false},
		{"yes -> verify (not the word true)", "yes", false},
		{"true -> insecure", "true", true},
		{"TRUE -> insecure (case-insensitive)", "TRUE", true},
		{"  true  -> insecure (trimmed)", "  true  ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvProxmoxInsecureSkipHostKeyVerification, tt.envValue)
			assert.Equal(t, tt.wantInsecure, resolveProxmoxHostKeyPolicy().insecure)
		})
	}
}

// TestProxmoxHostKeyOptions_Verifying asserts the verifying policy produces
// StrictHostKeyChecking=yes pointing at the credentials-mounted known_hosts, and
// never emits the insecure literals.
func TestProxmoxHostKeyOptions_Verifying(t *testing.T) {
	opts := optsToString(proxmoxHostKeyPolicy{insecure: false}.sshHostKeyOptions())

	assert.Contains(t, opts, "StrictHostKeyChecking=yes")
	assert.Contains(t, opts, "UserKnownHostsFile="+proxmoxKnownHostsFile)
	assert.Equal(t, "/etc/virtrigaud/credentials/known_hosts", proxmoxKnownHostsFile,
		"known_hosts must resolve inside the existing credentials mount")

	assert.NotContains(t, opts, "accept-new")
	assert.NotContains(t, opts, "/tmp/known_hosts")
}

// TestProxmoxHostKeyOptions_Insecure asserts the escape-hatch policy restores the
// accept-new + ephemeral known_hosts behaviour.
func TestProxmoxHostKeyOptions_Insecure(t *testing.T) {
	opts := optsToString(proxmoxHostKeyPolicy{insecure: true}.sshHostKeyOptions())

	assert.Contains(t, opts, "StrictHostKeyChecking=accept-new")
	assert.Contains(t, opts, "UserKnownHostsFile=/tmp/known_hosts")
	assert.NotContains(t, opts, "StrictHostKeyChecking=yes")
}

// TestProxmoxVerifyKnownHostsPresent covers the loud hard-fail gate: insecure is a
// no-op; verifying with no known_hosts on disk yields the actionable error.
func TestProxmoxVerifyKnownHostsPresent(t *testing.T) {
	t.Run("insecure path is a no-op", func(t *testing.T) {
		assert.NoError(t, proxmoxHostKeyPolicy{insecure: true}.verifyKnownHostsPresent("pve.lab.k8"))
	})

	t.Run("verifying + missing known_hosts -> actionable error", func(t *testing.T) {
		err := proxmoxHostKeyPolicy{insecure: false}.verifyKnownHostsPresent("pve.lab.k8")
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "pve.lab.k8")                              // (a) host
		assert.Contains(t, msg, proxmoxKnownHostsFile)                     // (b) file path
		assert.Contains(t, msg, "ssh-keyscan")                             // (c) recipe
		assert.Contains(t, msg, EnvProxmoxInsecureSkipHostKeyVerification) // (d) escape hatch
	})
}

// TestProxmoxLogVerificationMode_WARN asserts the escape hatch fires a WARN log
// carrying the audit signal (provider + host fields, MITM wording, env-var name).
func TestProxmoxLogVerificationMode_WARN(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	proxmoxHostKeyPolicy{insecure: true}.logVerificationMode(logger, "pve.lab.k8")

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "DISABLED")
	assert.Contains(t, out, "MITM")
	assert.Contains(t, out, "audit-flagged")
	assert.Contains(t, out, "provider=proxmox")
	assert.Contains(t, out, "host=pve.lab.k8")
	assert.Contains(t, out, EnvProxmoxInsecureSkipHostKeyVerification)
}

// TestProxmoxLogVerificationMode_INFO asserts the verifying path logs an INFO line
// naming the known_hosts path.
func TestProxmoxLogVerificationMode_INFO(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	proxmoxHostKeyPolicy{insecure: false}.logVerificationMode(logger, "pve.lab.k8")

	out := buf.String()
	assert.Contains(t, out, "level=INFO")
	assert.Contains(t, out, "host-key verification: enabled")
	assert.Contains(t, out, "known_hosts="+proxmoxKnownHostsFile)
	assert.NotContains(t, out, "level=WARN")
}

// TestProxmoxLogVerificationMode_NilLogger ensures a nil logger does not panic.
func TestProxmoxLogVerificationMode_NilLogger(t *testing.T) {
	assert.NotPanics(t, func() {
		proxmoxHostKeyPolicy{insecure: false}.logVerificationMode(nil, "host")
		proxmoxHostKeyPolicy{insecure: true}.logVerificationMode(nil, "host")
	})
}

// TestProxmoxSSHKeyAuthOptions pins the exact ssh flag list used by the key-based
// path: -i is mandatory (the key is at a non-default path), IdentitiesOnly stops
// agent wandering, and the auth toggles force pubkey-only.
func TestProxmoxSSHKeyAuthOptions(t *testing.T) {
	assert.Equal(t,
		[]string{
			"-i", "/tmp/k",
			"-o", "IdentitiesOnly=yes",
			"-o", "PasswordAuthentication=no",
			"-o", "PubkeyAuthentication=yes",
		},
		proxmoxSSHKeyAuthOptions("/tmp/k"),
	)
}

// TestProxmoxSSHMultiplexOptions covers the on/off escape hatch (#194).
func TestProxmoxSSHMultiplexOptions(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		t.Setenv(EnvProxmoxDisableSSHMultiplexing, "")
		opts := optsToString(proxmoxSSHMultiplexOptions())
		assert.Contains(t, opts, "ControlMaster=auto")
		assert.Contains(t, opts, "ControlPath="+proxmoxSSHControlPath)
		assert.Contains(t, opts, "ControlPersist="+proxmoxSSHControlPersist)
	})
	t.Run("disabled by literal true", func(t *testing.T) {
		t.Setenv(EnvProxmoxDisableSSHMultiplexing, "true")
		assert.Nil(t, proxmoxSSHMultiplexOptions())
	})
	t.Run("any other value keeps it on", func(t *testing.T) {
		t.Setenv(EnvProxmoxDisableSSHMultiplexing, "1")
		assert.NotNil(t, proxmoxSSHMultiplexOptions())
	})
}

// TestBuildSSHCommand_PasswordBranch asserts the password (sshpass) branch: the
// argv carries `sshpass -e ssh`, the host-key + multiplex options, and the
// password is delivered via the SSHPASS env var (NOT in argv, so it never shows
// in `ps`).
func TestBuildSSHCommand_PasswordBranch(t *testing.T) {
	t.Setenv(EnvProxmoxInsecureSkipHostKeyVerification, "true") // skip known_hosts hard-fail
	t.Setenv(EnvProxmoxDisableSSHMultiplexing, "")

	tr := &sshTransport{
		host:    "pve.lab.k8",
		creds:   sshCredentials{User: "root", Password: "s3cr3t"},
		hostKey: proxmoxHostKeyPolicy{insecure: true},
		logger:  slog.Default(),
	}

	cmd, err := tr.buildSSHCommand(context.Background(), "pvesm path local-lvm:vm-100-disk-0")
	require.NoError(t, err)

	argv := strings.Join(cmd.Args, " ")
	assert.Equal(t, "sshpass", cmd.Args[0], "password branch must invoke sshpass")
	assert.Contains(t, argv, "-e")
	assert.Contains(t, argv, "ssh")
	assert.Contains(t, argv, "PasswordAuthentication=yes")
	assert.Contains(t, argv, "PubkeyAuthentication=no")
	assert.Contains(t, argv, "StrictHostKeyChecking=accept-new") // insecure policy
	assert.Contains(t, argv, "ControlMaster=auto")
	assert.Contains(t, argv, "root@pve.lab.k8")
	assert.Contains(t, argv, "pvesm path local-lvm:vm-100-disk-0")

	// The password MUST NOT be in argv; it must be in SSHPASS env.
	assert.NotContains(t, argv, "s3cr3t", "password must not appear in argv")
	var foundSSHPass bool
	for _, e := range cmd.Env {
		if e == "SSHPASS=s3cr3t" {
			foundSSHPass = true
		}
	}
	assert.True(t, foundSSHPass, "password must be delivered via SSHPASS env var")
}

// TestBuildSSHCommand_KeyBranch asserts the key (`ssh -i`) branch: the argv
// carries `ssh -i <keyfile>` with the key-auth options and host-key/multiplex
// options. The key is materialized to the proxmox key dir with 0600 perms.
func TestBuildSSHCommand_KeyBranch(t *testing.T) {
	t.Setenv(EnvProxmoxInsecureSkipHostKeyVerification, "true")
	t.Setenv(EnvProxmoxDisableSSHMultiplexing, "")
	require.NoError(t, os.RemoveAll(proxmoxSSHPrivateKeyDir))
	t.Cleanup(func() { _ = os.RemoveAll(proxmoxSSHPrivateKeyDir) })

	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc123\n-----END OPENSSH PRIVATE KEY-----"
	tr := &sshTransport{
		host:    "pve.lab.k8",
		creds:   sshCredentials{User: "root", PrivateKey: pem},
		hostKey: proxmoxHostKeyPolicy{insecure: true},
		logger:  slog.Default(),
	}

	cmd, err := tr.buildSSHCommand(context.Background(), "qemu-img check /var/tmp/x.qcow2")
	require.NoError(t, err)

	argv := strings.Join(cmd.Args, " ")
	assert.Equal(t, "ssh", cmd.Args[0], "key branch must invoke ssh directly (no sshpass)")
	assert.Contains(t, argv, "-i "+proxmoxSSHPrivateKeyPath)
	assert.Contains(t, argv, "IdentitiesOnly=yes")
	assert.Contains(t, argv, "PubkeyAuthentication=yes")
	assert.Contains(t, argv, "PasswordAuthentication=no")
	assert.Contains(t, argv, "StrictHostKeyChecking=accept-new")
	assert.Contains(t, argv, "root@pve.lab.k8")
	assert.Contains(t, argv, "qemu-img check /var/tmp/x.qcow2")

	// The key file must exist with 0600 perms in a 0700 dir, and never appear as
	// plaintext in the argv.
	assert.NotContains(t, argv, "abc123", "key material must not appear in argv")
	fi, statErr := os.Stat(proxmoxSSHPrivateKeyPath)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	di, statErr := os.Stat(proxmoxSSHPrivateKeyDir)
	require.NoError(t, statErr)
	assert.Zero(t, di.Mode().Perm()&0o077, "key dir must not be group/other accessible")
	got, readErr := os.ReadFile(proxmoxSSHPrivateKeyPath)
	require.NoError(t, readErr)
	assert.Equal(t, pem+"\n", string(got))
}

// TestBuildSSHCommand_NoCredentials asserts the no-credential case returns an
// actionable error naming the credential keys, rather than building a broken
// command.
func TestBuildSSHCommand_NoCredentials(t *testing.T) {
	t.Setenv(EnvProxmoxInsecureSkipHostKeyVerification, "true")

	tr := &sshTransport{
		host:    "pve.lab.k8",
		creds:   sshCredentials{User: "root"},
		hostKey: proxmoxHostKeyPolicy{insecure: true},
		logger:  slog.Default(),
	}

	_, err := tr.buildSSHCommand(context.Background(), "true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), credFileSSHPassword)
	assert.Contains(t, err.Error(), credFileSSHPrivateKey)
}

// TestBuildSSHCommand_VerifyingMissingKnownHosts_HardFails is the integration
// point: with verification on (default) and no known_hosts on disk, building any
// SSH command must hard-fail with the actionable error rather than connecting
// insecurely.
func TestBuildSSHCommand_VerifyingMissingKnownHosts_HardFails(t *testing.T) {
	t.Setenv(EnvProxmoxInsecureSkipHostKeyVerification, "") // verification ON

	tr := &sshTransport{
		host:    "pve.lab.k8",
		creds:   sshCredentials{User: "root", Password: "x"},
		hostKey: proxmoxHostKeyPolicy{insecure: false},
		logger:  slog.Default(),
	}

	_, err := tr.buildSSHCommand(context.Background(), "true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-key verification")
	assert.Contains(t, err.Error(), proxmoxKnownHostsFile)
}

// TestLoadSSHCredentials_EnvFallback covers the env-var fallback (the file mount
// is absent in the test env) and the root default. Credentials are loaded from
// PROVIDER_* with PVE_* fallback, exactly like the control-plane token loading.
func TestLoadSSHCredentials_EnvFallback(t *testing.T) {
	t.Run("provider env wins, user defaults to root when unset", func(t *testing.T) {
		t.Setenv("PROVIDER_SSH_PASSWORD", "pw-provider")
		t.Setenv("PVE_SSH_PASSWORD", "pw-pve")
		creds := loadSSHCredentials()
		assert.Equal(t, "root", creds.User, "user defaults to root (root@pam → node root)")
		assert.Equal(t, "pw-provider", creds.Password, "PROVIDER_* wins over PVE_*")
	})

	t.Run("pve env fallback", func(t *testing.T) {
		t.Setenv("PROVIDER_SSH_USER", "")
		t.Setenv("PVE_SSH_USER", "migrator")
		t.Setenv("PVE_SSH_PRIVATE_KEY", "KEYDATA")
		creds := loadSSHCredentials()
		assert.Equal(t, "migrator", creds.User)
		assert.Equal(t, "KEYDATA", creds.PrivateKey)
	})
}

// TestSSHTransport_AuthBranchSelection covers the password-wins precedence.
func TestSSHTransport_AuthBranchSelection(t *testing.T) {
	pwOnly := &sshTransport{creds: sshCredentials{Password: "p"}}
	assert.True(t, pwOnly.usePassword())
	assert.False(t, pwOnly.useKey())

	keyOnly := &sshTransport{creds: sshCredentials{PrivateKey: "k"}}
	assert.False(t, keyOnly.usePassword())
	assert.True(t, keyOnly.useKey())

	both := &sshTransport{creds: sshCredentials{Password: "p", PrivateKey: "k"}}
	assert.True(t, both.usePassword(), "password wins when both are set")
	assert.False(t, both.useKey())

	none := &sshTransport{creds: sshCredentials{}}
	assert.False(t, none.usePassword())
	assert.False(t, none.useKey())
}

// TestProxmoxShellQuote guards the remote-shell injection defense.
func TestProxmoxShellQuote(t *testing.T) {
	assert.Equal(t, "'/var/tmp/x.qcow2'", proxmoxShellQuote("/var/tmp/x.qcow2"))
	assert.Equal(t, `'a'\''b'`, proxmoxShellQuote("a'b"))
	assert.Equal(t, "'a b'", proxmoxShellQuote("a b"))
}
