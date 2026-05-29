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
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optsToString flattens the alternating ["-o", "k=v", ...] option slice into a
// single space-joined string for easy substring assertions.
func optsToString(opts []string) string {
	return strings.Join(opts, " ")
}

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

// TestHostKeyPolicy_SSHHostKeyOptions_Verifying asserts the verifying policy
// produces StrictHostKeyChecking=yes pointing at the credentials-mounted
// known_hosts, and never emits the insecure literals. This covers the password
// (sshpass/ssh) and scp transports, which both consume sshHostKeyOptions.
func TestHostKeyPolicy_SSHHostKeyOptions_Verifying(t *testing.T) {
	policy := hostKeyPolicy{insecure: false}
	opts := optsToString(policy.sshHostKeyOptions())

	assert.Contains(t, opts, "StrictHostKeyChecking=yes")
	assert.Contains(t, opts, "UserKnownHostsFile="+KnownHostsFile)
	assert.Equal(t, "/etc/virtrigaud/credentials/known_hosts", KnownHostsFile,
		"known_hosts must resolve inside the existing credentials mount")

	assert.NotContains(t, opts, "accept-new")
	assert.NotContains(t, opts, "/tmp/known_hosts")
	assert.NotContains(t, opts, "no_verify")
}

// TestHostKeyPolicy_SSHHostKeyOptions_Insecure asserts the escape-hatch policy
// restores the legacy accept-new + ephemeral known_hosts behaviour.
func TestHostKeyPolicy_SSHHostKeyOptions_Insecure(t *testing.T) {
	policy := hostKeyPolicy{insecure: true}
	opts := optsToString(policy.sshHostKeyOptions())

	assert.Contains(t, opts, "StrictHostKeyChecking=accept-new")
	assert.Contains(t, opts, "UserKnownHostsFile=/tmp/known_hosts")
	assert.NotContains(t, opts, "StrictHostKeyChecking=yes")
}

// TestHostKeyPolicy_SSHConfigStanza covers the ~/.ssh/config body used by the
// key-based qemu+ssh:// transport for both policies.
func TestHostKeyPolicy_SSHConfigStanza(t *testing.T) {
	verifying := hostKeyPolicy{insecure: false}.sshConfigStanza()
	assert.Contains(t, verifying, "StrictHostKeyChecking yes")
	assert.Contains(t, verifying, "UserKnownHostsFile "+KnownHostsFile)
	assert.NotContains(t, verifying, "accept-new")
	assert.NotContains(t, verifying, "/tmp/known_hosts")

	insecure := hostKeyPolicy{insecure: true}.sshConfigStanza()
	assert.Contains(t, insecure, "StrictHostKeyChecking accept-new")
	assert.Contains(t, insecure, "UserKnownHostsFile /tmp/known_hosts")
}

// TestHostKeyPolicy_ApplyURIHostKeyOptions covers the key-based URI transport:
// no_verify=1 is set ONLY on the insecure path and deleted on the verifying
// path.
func TestHostKeyPolicy_ApplyURIHostKeyOptions(t *testing.T) {
	t.Run("verifying drops no_verify", func(t *testing.T) {
		q := url.Values{}
		q.Set("no_verify", "1") // simulate a legacy/leftover value
		hostKeyPolicy{insecure: false}.applyURIHostKeyOptions(q)
		assert.Empty(t, q.Get("no_verify"))
	})

	t.Run("insecure sets no_verify", func(t *testing.T) {
		q := url.Values{}
		hostKeyPolicy{insecure: true}.applyURIHostKeyOptions(q)
		assert.Equal(t, "1", q.Get("no_verify"))
	})
}

// TestHostKeyPolicy_VerifyKnownHostsPresent covers the loud hard-fail gate.
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
// hatch engaged the URI carries no_verify=1 (legacy behaviour) and the missing
// known_hosts does NOT block startup.
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
	assert.Contains(t, v.uri, "no_verify=1")
	assert.True(t, v.hostKey.insecure)
}

// TestSetupConnection_VerifyingLocalURI_NoSSH confirms a local (non-SSH) URI is
// unaffected by the host-key policy (no known_hosts requirement, no no_verify).
func TestSetupConnection_VerifyingLocalURI_NoSSH(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")

	v := &VirshProvider{
		config:      &ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu:///system"}},
		credentials: &Credentials{},
	}

	err := v.setupConnection()
	require.NoError(t, err)
	assert.NotContains(t, v.uri, "no_verify")
}
