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
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"
)

// TestResolveHostKeyPolicy_EnvParsing verifies the escape-hatch env var is
// honoured ONLY for the literal word "true" (case-insensitive, trimmed) and
// keeps verification ON for every other value. Mirrors ADR-0003's
// isInsecureOptedIn semantics.
func TestResolveHostKeyPolicy_EnvParsing(t *testing.T) {
	tests := []struct {
		name         string
		setEnv       bool
		envValue     string
		wantInsecure bool
	}{
		{name: "unset -> verify", setEnv: false, wantInsecure: false},
		{name: "empty -> verify", setEnv: true, envValue: "", wantInsecure: false},
		{name: "false -> verify", setEnv: true, envValue: "false", wantInsecure: false},
		{name: "1 -> verify (not the word true)", setEnv: true, envValue: "1", wantInsecure: false},
		{name: "yes -> verify (not the word true)", setEnv: true, envValue: "yes", wantInsecure: false},
		{name: "true -> insecure", setEnv: true, envValue: "true", wantInsecure: true},
		{name: "TRUE -> insecure (case-insensitive)", setEnv: true, envValue: "TRUE", wantInsecure: true},
		{name: "  true  -> insecure (trimmed)", setEnv: true, envValue: "  true  ", wantInsecure: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(EnvInsecureSkipHostKeyVerification, tt.envValue)
			} else {
				// Ensure a clean env even if the host has it set.
				t.Setenv(EnvInsecureSkipHostKeyVerification, "")
			}
			policy := resolveHostKeyPolicy()
			assert.Equal(t, tt.wantInsecure, policy.insecure)
		})
	}
}

// TestHostKeyPolicy_HostKeyCallback_Insecure asserts the escape-hatch policy
// builds a callback that accepts any host key (functionally equivalent to
// ssh.InsecureIgnoreHostKey, but never calling that specific symbol — see the
// hostKeyCallback doc) without ever consulting KnownHostsFile.
func TestHostKeyPolicy_HostKeyCallback_Insecure(t *testing.T) {
	cb, err := hostKeyPolicy{insecure: true}.hostKeyCallback()
	require.NoError(t, err)
	require.NotNil(t, cb)
	// A callback that never errors, for any hostname/address/key, IS the
	// insecure contract; there is no real handshake to drive here.
	assert.NoError(t, cb("anything:22", nil, nil))
}

// TestHostKeyPolicy_HostKeyCallback_Verifying asserts the verifying policy
// builds a knownhosts-backed callback (never nil, never an unconditional
// accept) from KnownHostsFile. Acceptance/rejection behavior against a real
// key is covered end-to-end in sshclient_test.go against an in-memory SSH
// server.
func TestHostKeyPolicy_HostKeyCallback_Verifying(t *testing.T) {
	// KnownHostsFile does not exist in the test environment, so knownhosts.New
	// itself fails — hostKeyCallback must surface that, not silently fall back
	// to an accept-all callback.
	_, err := hostKeyPolicy{insecure: false}.hostKeyCallback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), KnownHostsFile)
}

// TestKnownHostsHasEntry_SpecificHost is the #291 finding B6 regression test:
// seeding known_hosts for one host must NOT make an unrelated host appear
// "present". Before this fix, verifyKnownHostsPresent only checked the file
// was non-empty, so a Provider seeded for host-a but pointed at host-b passed
// the gate with zero trust material for host-b.
func TestKnownHostsHasEntry_SpecificHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")

	pub, err := probeHostKey() // any real ssh.PublicKey works as the seeded key
	require.NoError(t, err)
	line := knownhosts.Line([]string{"host-a"}, pub)
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))

	present, err := knownHostsHasEntry(path, "host-a")
	require.NoError(t, err)
	assert.True(t, present, "host-a has a known_hosts entry and must be reported present")

	present, err = knownHostsHasEntry(path, "host-b")
	require.NoError(t, err)
	assert.False(t, present, "host-b has NO known_hosts entry and must NOT be reported present just because host-a is (closes #291 B6)")
}

// TestKnownHostsHasEntry_MissingFile verifies a nonexistent known_hosts path
// surfaces as an error (so verifyKnownHostsPresent's caller falls back to the
// actionable hard-fail message), not a false "present".
func TestKnownHostsHasEntry_MissingFile(t *testing.T) {
	_, err := knownHostsHasEntry(filepath.Join(t.TempDir(), "does-not-exist"), "host-a")
	assert.Error(t, err)
}

// TestHostPort covers the host:port normalization used consistently by the
// real dial and the known_hosts probe, including the IPv6 double-bracketing
// trap: url.URL.Host already brackets a bare IPv6 literal ("[::1]"), and
// net.JoinHostPort brackets any host containing a colon on its own — composing
// the two naively produces "[[::1]]:22".
func TestHostPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"172.16.56.8", "172.16.56.8:22"},
		{"172.16.56.8:2222", "172.16.56.8:2222"},
		{"host.example.com", "host.example.com:22"},
		{"[::1]", "[::1]:22"},
		{"[::1]:2222", "[::1]:2222"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, hostPort(tc.in))
		})
	}
}

// TestHostKeyPolicy_VerifyKnownHostsPresent covers the loud hard-fail gate,
// including the #291 B6 specific-host strengthening.
func TestHostKeyPolicy_VerifyKnownHostsPresent(t *testing.T) {
	t.Run("insecure path is a no-op", func(t *testing.T) {
		err := hostKeyPolicy{insecure: true}.verifyKnownHostsPresent("172.16.56.8")
		assert.NoError(t, err)
	})

	t.Run("verifying + missing known_hosts -> actionable error", func(t *testing.T) {
		// KnownHostsFile points at /etc/virtrigaud/credentials/known_hosts,
		// which does not exist in the test environment.
		err := hostKeyPolicy{insecure: false}.verifyKnownHostsPresent("172.16.56.8")
		require.Error(t, err)
		msg := err.Error()
		// (a) names the host
		assert.Contains(t, msg, "172.16.56.8")
		// (b) names the expected file path
		assert.Contains(t, msg, KnownHostsFile)
		// (c) gives the ssh-keyscan recipe
		assert.Contains(t, msg, "ssh-keyscan")
		// (d) names the escape-hatch env var as the explicit opt-out
		assert.Contains(t, msg, EnvInsecureSkipHostKeyVerification)
	})
}

// TestHostKeyPolicy_LogVerificationMode_WARN asserts that the escape hatch
// fires a WARN log carrying the audit signal (provider + host structured
// fields, MITM wording, env-var name). Captured via an in-memory slog handler.
func TestHostKeyPolicy_LogVerificationMode_WARN(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	hostKeyPolicy{insecure: true}.logVerificationMode(logger, "172.16.56.8")

	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "DISABLED")
	assert.Contains(t, out, "MITM")
	assert.Contains(t, out, "audit-flagged")
	assert.Contains(t, out, "provider=libvirt")
	assert.Contains(t, out, "host=172.16.56.8")
	assert.Contains(t, out, EnvInsecureSkipHostKeyVerification)
}

// TestHostKeyPolicy_LogVerificationMode_INFO asserts the verifying path logs an
// INFO line naming the known_hosts path (the auditor's greppable signal).
func TestHostKeyPolicy_LogVerificationMode_INFO(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	hostKeyPolicy{insecure: false}.logVerificationMode(logger, "172.16.56.8")

	out := buf.String()
	assert.Contains(t, out, "level=INFO")
	assert.Contains(t, out, "host-key verification: enabled")
	assert.Contains(t, out, "known_hosts="+KnownHostsFile)
	assert.NotContains(t, out, "level=WARN")
}

// TestHostKeyPolicy_LogVerificationMode_NilLogger ensures a nil logger does not
// panic (falls back to slog.Default()).
func TestHostKeyPolicy_LogVerificationMode_NilLogger(t *testing.T) {
	assert.NotPanics(t, func() {
		hostKeyPolicy{insecure: false}.logVerificationMode(nil, "host")
		hostKeyPolicy{insecure: true}.logVerificationMode(nil, "host")
	})
}

// TestSetupConnection_VerifyingMissingKnownHosts_HardFails is the integration
// point: with verification on (default) and no known_hosts on disk, setting up
// an SSH-based connection must hard-fail with the actionable error rather than
// silently connecting insecurely.
func TestSetupConnection_VerifyingMissingKnownHosts_HardFails(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "") // verification ON (default)

	v := &VirshProvider{
		config: &ProviderConfig{
			Spec: ProviderSpec{Endpoint: "qemu+ssh://virtrigaud@172.16.56.8/system"},
		},
		credentials: &Credentials{Username: "virtrigaud"},
	}

	err := v.setupConnection()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-key verification")
	assert.Contains(t, err.Error(), KnownHostsFile)
	assert.Contains(t, err.Error(), EnvInsecureSkipHostKeyVerification)
}

// TestSetupConnection_EscapeHatch_SetsNoVerify confirms that with the escape
// hatch engaged, setupConnection succeeds despite the missing known_hosts and
// resolves v.hostKey.insecure — the in-process client (sshclient.go) reads
// this flag on every dial. ADR-0008 PR 3 removed the no_verify=1 URI
// query-param mechanism entirely (there is no more external ssh/virsh CLI
// reading the URI), so unlike the pre-PR-3 test this no longer asserts
// anything about v.uri's contents.
func TestSetupConnection_EscapeHatch_SetsNoVerify(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "true")

	v := &VirshProvider{
		config: &ProviderConfig{
			Spec: ProviderSpec{Endpoint: "qemu+ssh://virtrigaud@172.16.56.8/system"},
		},
		credentials: &Credentials{Username: "virtrigaud"},
	}

	err := v.setupConnection()
	require.NoError(t, err)
	assert.True(t, v.hostKey.insecure)
}

// TestSetupConnection_VerifyingLocalURI_NoSSH confirms a local (non-SSH) URI is
// unaffected by the host-key policy (no known_hosts requirement) — there is no
// host to dial.
func TestSetupConnection_VerifyingLocalURI_NoSSH(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")

	v := &VirshProvider{
		config:      &ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu:///system"}},
		credentials: &Credentials{},
	}

	err := v.setupConnection()
	require.NoError(t, err)
	assert.Equal(t, "qemu:///system", v.uri)
}
