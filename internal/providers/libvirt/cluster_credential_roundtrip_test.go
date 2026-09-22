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

package libvirt

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// This file is the ADR-0007 P1 clustered-credential regression suite (BUG 2). The
// real-lab validation reported `parse SSH private key: ssh: no key found` from
// the clustered nodeinfo path; the unit/test:// tests missed it because they
// exercised the connection WIRING with placeholder, non-PEM key strings
// ([]byte("KEY-A")) and never round-tripped a REAL key through the render ->
// hostsecret marshal -> mounted-file consume -> ssh.ParsePrivateKey chain.
//
// hostsecret.Credentials.SSHPrivateKey is a []byte, so it is base64 in JSON: the
// render must inline the raw PEM (exactly one encode layer) and the consume must
// decode it back to raw PEM (exactly one decode layer) before ssh.ParsePrivateKey
// ever sees it. A second layer on either side yields non-PEM bytes and the exact
// "ssh: no key found" the lab saw. These tests pin BOTH the byte-level round-trip
// and the end-to-end SSH authentication so any future double-encode is caught in
// CI, not in a lab.

// TestClusteredCredentialRoundTrip renders a REAL private key into the host
// inventory exactly as the controller does (#316), reads it back exactly as the
// clustered provider does (#317), and proves the RAW PEM reaches
// ssh.ParsePrivateKey and yields a usable signer/AuthMethod — with no credential
// material leaked to logs.
func TestClusteredCredentialRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		gen  func(t *testing.T) string
	}{
		{
			name: "ed25519",
			gen:  func(t *testing.T) string { pemStr, _ := generateTestClientKeyPEM(t); return pemStr },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyPEM := tc.gen(t)
			// A distinctive, non-secret placeholder known_hosts line so the leak
			// assertion has a concrete token to look for.
			knownHostsLine := "dome ssh-ed25519 AAAADO-NOT-LOG-cluster-known-hosts-token\n"

			// --- Render EXACTLY as the controller does (#316): raw bytes in, the
			// []byte field becomes base64 in JSON. ---
			data, err := hostsecret.Marshal(hostsecret.Inventory{
				SchemaVersion: hostsecret.SchemaVersion,
				Hosts: []hostsecret.Host{{
					ID:       "dome",
					Endpoint: "qemu+ssh://virt@dome/system",
					Credentials: hostsecret.Credentials{
						SSHPrivateKey: []byte(keyPEM),
						KnownHosts:    []byte(knownHostsLine),
					},
				}},
			})
			require.NoError(t, err)

			// --- Consume EXACTLY as the clustered provider does (#317):
			// LoadInventory -> hostsecret.Unmarshal (base64 -> raw). ---
			dir := t.TempDir()
			file := filepath.Join(dir, "hosts.json")
			require.NoError(t, os.WriteFile(file, data, 0o600))

			inv, err := hostconn.LoadInventory(file)
			require.NoError(t, err)
			require.Len(t, inv.Hosts, 1)
			h := inv.Hosts[0]

			// The []byte field decodes back to the EXACT raw PEM: exactly one
			// base64 layer, never two. A double-encode here is the lab bug.
			require.Equal(t, keyPEM, string(h.Credentials.SSHPrivateKey),
				"inlined key must decode back to the exact raw PEM (single base64 layer)")
			require.Equal(t, knownHostsLine, string(h.Credentials.KnownHosts),
				"inlined known_hosts must decode back verbatim (single base64 layer)")

			// The raw PEM parses and yields a usable signer.
			signer, err := ssh.ParsePrivateKey(h.Credentials.SSHPrivateKey)
			require.NoError(t, err, `raw PEM must parse; "ssh: no key found" was the lab symptom`)
			require.NotNil(t, signer)
			require.NotNil(t, signer.PublicKey())

			// The full clustered build path (buildClusteredVirshProvider ->
			// sshAuthMethods) yields exactly one non-nil PublicKeys AuthMethod,
			// while logging NO credential material.
			var logbuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			t.Setenv(EnvInsecureSkipHostKeyVerification, "") // verification ON: also materializes known_hosts
			vp, cleanup, err := buildClusteredVirshProvider(h, filepath.Join(dir, "kh"), logger)
			require.NoError(t, err)
			defer cleanup()
			require.NotNil(t, vp.credentials)
			require.Equal(t, keyPEM, vp.credentials.SSHPrivateKey,
				"the clustered dialer must hand the raw PEM (not base64) to the SSH transport")

			auth, err := sshAuthMethods(vp.credentials)
			require.NoError(t, err)
			require.Len(t, auth, 1, "exactly one PublicKeys auth method from a key-only credential")
			require.NotNil(t, auth[0])

			// SECURITY: neither the key nor the known_hosts bytes (raw or base64)
			// may appear in logs — host id + coarse reason only.
			logs := logbuf.String()
			for _, secret := range []string{keyPEM, knownHostsLine} {
				assert.NotContains(t, logs, secret, "raw credential material leaked to logs")
				assert.NotContains(t, logs, base64.StdEncoding.EncodeToString([]byte(secret)),
					"base64 credential material leaked to logs")
			}
			assert.NotContains(t, logs, "DO-NOT-LOG", "a credential token leaked to logs")
		})
	}
}

// TestClusteredProvider_SSHKeyAuthenticatesEndToEnd drives the WHOLE clustered
// stack against a real in-memory SSH server — render -> hosts.json ->
// LoadInventory -> production Dialer -> ClusterRegistry.ConnFor -> virshConn.Virsh
// -> real SSH handshake authenticated with the INLINED key — reproducing the
// lab's GetHostInfo/nodeinfo-over-SSH scenario. If the inlined key did not reach
// ssh.ParsePrivateKey as raw PEM, the handshake would fail with the lab's
// "build ssh auth method: parse SSH private key: ssh: no key found"; this asserts
// it does not.
func TestClusteredProvider_SSHKeyAuthenticatesEndToEnd(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "") // real host-key verification, from the inlined known_hosts

	hostKey := generateTestHostKey(t)
	clientKeyPEM, clientPub := generateTestClientKeyPEM(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{authorizedKey: clientPub})
	knownHostsLine := knownhosts.Line([]string{addr}, hostKey.PublicKey()) + "\n"

	data, err := hostsecret.Marshal(hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts: []hostsecret.Host{{
			ID:       "dome",
			Endpoint: "qemu+ssh://virtrigaud@" + addr + "/system",
			Credentials: hostsecret.Credentials{
				SSHPrivateKey: []byte(clientKeyPEM),
				KnownHosts:    []byte(knownHostsLine),
			},
		}},
	})
	require.NoError(t, err)

	dir := t.TempDir()
	file := filepath.Join(dir, "hosts.json")
	require.NoError(t, os.WriteFile(file, data, 0o600))

	inv, err := hostconn.LoadInventory(file)
	require.NoError(t, err)
	reg, err := hostconn.NewClusterRegistry(inv, newClusterDialer(filepath.Join(dir, "kh"), nil), nil)
	require.NoError(t, err)
	defer func() { _ = reg.Close() }()

	// ConnFor builds the per-host connection lazily (no dial yet).
	lease, err := reg.ConnFor(context.Background(), hostconn.HostID("dome"))
	require.NoError(t, err)
	defer func() { _ = lease.Close() }()

	// The FIRST command triggers the lazy SSH dial: TCP connect + host-key
	// verification (against the inlined known_hosts) + public-key auth (with the
	// inlined key). The remote `virsh` need not exist — we assert only that SSH
	// AUTH itself succeeded, i.e. none of the auth-failure signatures appear.
	res, runErr := lease.Virsh(context.Background(), "nodeinfo")
	stderr := ""
	if res != nil {
		stderr = res.Stderr
	}
	for _, bad := range []string{
		"parse SSH private key",
		"ssh: no key found",
		"unable to authenticate",
		"build ssh auth method",
	} {
		if runErr != nil {
			require.NotContainsf(t, runErr.Error(), bad,
				"inlined key must authenticate end-to-end; got auth-failure error %q", runErr.Error())
		}
		require.NotContainsf(t, stderr, bad,
			"inlined key must authenticate end-to-end; got auth-failure stderr %q", stderr)
	}
}
