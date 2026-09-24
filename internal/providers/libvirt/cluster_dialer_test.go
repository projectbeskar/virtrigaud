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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

func chost(id, endpoint string, key, known []byte) hostsecret.Host {
	return hostsecret.Host{
		ID:       id,
		Endpoint: endpoint,
		Credentials: hostsecret.Credentials{
			SSHPrivateKey: key,
			KnownHosts:    known,
		},
	}
}

func writeInventory(t *testing.T, path string, hosts ...hostsecret.Host) {
	t.Helper()
	data, err := hostsecret.Marshal(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: hosts})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// TestClusteredModeEnabled proves presence == clustered, absence == single-host.
func TestClusteredModeEnabled(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "hosts.json")
	require.NoError(t, os.WriteFile(present, []byte("{}"), 0o600))

	assert.True(t, clusteredModeEnabled(present), "existing file -> clustered")
	assert.False(t, clusteredModeEnabled(filepath.Join(dir, "absent.json")), "missing file -> single-host")
}

// TestHostsFilePath_EnvOverride proves the default path and the test override.
func TestHostsFilePath_EnvOverride(t *testing.T) {
	t.Setenv(EnvHostsFile, "")
	assert.Equal(t, filepath.Join(hostsecret.MountPath, hostsecret.SecretDataKey), hostsFilePath())

	t.Setenv(EnvHostsFile, "/custom/hosts.json")
	assert.Equal(t, "/custom/hosts.json", hostsFilePath())
}

// TestSanitizeHostID keeps DNS names intact and neutralises path-hostile ids.
func TestSanitizeHostID(t *testing.T) {
	cases := map[string]string{
		"host-a.example.com": "host-a.example.com",
		"kvm01":              "kvm01",
		"../../etc/passwd":   ".._.._etc_passwd",
		"a/b":                "a_b",
		"":                   "_empty_",
		"..":                 "_.._",
		".":                  "_._",
	}
	for in, want := range cases {
		assert.Equalf(t, want, sanitizeHostID(in), "sanitizeHostID(%q)", in)
	}
}

// TestNewHostKeyPolicyFromBytes_Verifying materialises a host's known_hosts to a
// private file and points the policy at it, so the exact single-host
// knownhosts.New verification runs against THIS host's material — and only this
// host's (the #291 B6 per-host guarantee, now per clustered host).
func TestNewHostKeyPolicyFromBytes_Verifying(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "") // verification ON
	dir := t.TempDir()

	// A real known_hosts entry for host-a (any real public key works as content).
	pub, err := probeHostKey()
	require.NoError(t, err)
	known := []byte(knownhosts.Line([]string{"host-a"}, pub) + "\n")

	policy, cleanup, err := newHostKeyPolicyFromBytes(dir, "host-a", known)
	require.NoError(t, err)
	defer cleanup()

	assert.False(t, policy.insecure)
	require.NotEmpty(t, policy.knownHostsPath, "verifying policy must point at a materialised file")
	assert.Equal(t, policy.knownHostsPath, policy.knownHostsFile(), "override must win over the global mount")

	// The file holds exactly the host's bytes.
	onDisk, err := os.ReadFile(policy.knownHostsPath)
	require.NoError(t, err)
	assert.Equal(t, known, onDisk)

	// End-to-end: the real pre-flight passes for host-a and fails for host-b,
	// proving each clustered host verifies against its OWN trust material.
	assert.NoError(t, policy.verifyKnownHostsPresent("host-a"))
	assert.Error(t, policy.verifyKnownHostsPresent("host-b"),
		"host-b has no entry in host-a's known_hosts; must not verify (ADR-0004 not weakened)")

	// cleanup removes the file.
	cleanup()
	_, statErr := os.Stat(policy.knownHostsPath)
	assert.True(t, os.IsNotExist(statErr), "cleanup must remove the materialised known_hosts file")
}

// TestNewHostKeyPolicyFromBytes_EmptyKnownHostsHardFails proves a clustered host
// with NO known_hosts still hard-fails verification (empty file, size 0) rather
// than falling back to any other trust material — the single-host posture,
// unweakened.
func TestNewHostKeyPolicyFromBytes_EmptyKnownHostsHardFails(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "") // verification ON
	dir := t.TempDir()

	policy, cleanup, err := newHostKeyPolicyFromBytes(dir, "host-empty", nil)
	require.NoError(t, err)
	defer cleanup()

	require.NotEmpty(t, policy.knownHostsPath)
	info, err := os.Stat(policy.knownHostsPath)
	require.NoError(t, err)
	assert.Zero(t, info.Size(), "empty known_hosts must be a zero-byte file")

	err = policy.verifyKnownHostsPresent("host-empty")
	require.Error(t, err, "empty known_hosts + verification ON must hard-fail (no TOFU)")
	assert.Contains(t, err.Error(), EnvInsecureSkipHostKeyVerification)
}

// TestNewHostKeyPolicyFromBytes_Insecure proves the escape hatch writes no file
// and disables verification, identically to the single-host insecure path.
func TestNewHostKeyPolicyFromBytes_Insecure(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "true")
	dir := t.TempDir()

	policy, cleanup, err := newHostKeyPolicyFromBytes(dir, "host-a", []byte("ignored"))
	require.NoError(t, err)
	defer cleanup()

	assert.True(t, policy.insecure)
	assert.Empty(t, policy.knownHostsPath, "insecure path writes no known_hosts file")
	// Directory stays empty (nothing materialised).
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}

// TestBuildClusteredVirshProvider proves an inventory host becomes a per-host
// VirshProvider with the right endpoint (URI), the inlined SSH key as the
// in-memory credential, and its own known_hosts policy — WITHOUT an eager dial.
func TestBuildClusteredVirshProvider(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()

	h := chost("host-a", "qemu+ssh://virt@host-a/system", []byte("PRIVATE-KEY-A"), []byte("known-a\n"))
	vp, cleanup, err := buildClusteredVirshProvider(h, dir, nil)
	require.NoError(t, err)
	defer cleanup()

	assert.Equal(t, "qemu+ssh://virt@host-a/system", vp.uri, "endpoint becomes the connection URI verbatim")
	require.NotNil(t, vp.credentials)
	assert.Equal(t, "PRIVATE-KEY-A", vp.credentials.SSHPrivateKey, "inlined key becomes the in-memory credential")
	assert.Empty(t, vp.credentials.Password, "clustered model is key-based SSH only (no inlined password)")
	require.NotEmpty(t, vp.hostKey.knownHostsPath, "policy points at this host's materialised known_hosts")

	onDisk, err := os.ReadFile(vp.hostKey.knownHostsPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("known-a\n"), onDisk)

	// No eager dial: the SSH client is nil until first use (lazy-open).
	assert.Nil(t, vp.sshClient, "buildClusteredVirshProvider must not dial (lazy-open)")

	// Empty endpoint is rejected.
	_, _, err = buildClusteredVirshProvider(chost("bad", "", []byte("k"), nil), dir, nil)
	assert.Error(t, err)
}

// TestBuildClusteredVirshProvider_RejectsUnsafeEndpoints is the dialer's
// defense-in-depth check: even if an entry reached it without passing the
// registry's admission, an endpoint that could smuggle shell metacharacters into
// the remote `virsh -c <uri>` — or one this provider cannot dial (grpc://) — is
// refused before any connection state is built, and the error never echoes the
// raw endpoint.
func TestBuildClusteredVirshProvider_RejectsUnsafeEndpoints(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()
	for _, ep := range []string{
		"qemu+ssh://virt@victim/system;id",
		"qemu+ssh://virt@victim/system$(touch /tmp/pwned)",
		"qemu+ssh://virt@victim/system%20-c%20id",
		"qemu+ssh://virt@victim/system/../x",
		" qemu+ssh://virt@victim/system",
		"grpc://agent-a:9443",
	} {
		vp, _, err := buildClusteredVirshProvider(chost("bad", ep, []byte("k"), nil), dir, nil)
		require.Errorf(t, err, "endpoint %q must be rejected", ep)
		assert.Nil(t, vp)
		assert.NotContains(t, err.Error(), ep, "error must not echo the raw endpoint")
	}
}

// TestClusterDialer_BuildsConnPerHost drives the production Dialer through a
// ClusterRegistry: first ConnFor lazily builds a *virshConn carrying that host's
// endpoint + credential material, and Close removes the host's known_hosts file.
func TestClusterDialer_BuildsConnPerHost(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	khDir := t.TempDir()

	dialer := newClusterDialer(khDir, nil)
	reg, err := hostconn.NewClusterRegistry(
		hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{
			chost("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("kh-a\n")),
			chost("host-b", "qemu+ssh://virt@host-b/system", []byte("KEY-B"), []byte("kh-b\n")),
		}},
		dialer, nil,
	)
	require.NoError(t, err)
	defer func() { _ = reg.Close() }()

	// Lazy-open: nothing materialised until first use.
	entries, _ := os.ReadDir(khDir)
	assert.Empty(t, entries, "no known_hosts materialised before first ConnFor (lazy-open)")

	c, err := reg.ConnFor(context.Background(), "host-b")
	require.NoError(t, err)
	// ConnFor returns an (unexported) lease wrapper; its HostID identifies the
	// underlying host. The per-*virshConn field assertions (uri/creds/policy)
	// live in TestBuildClusteredVirshProvider; here we assert the Dialer built
	// the RIGHT host by inspecting its materialised known_hosts.
	assert.Equal(t, hostconn.HostID("host-b"), c.HostID())

	// host-b's known_hosts was materialised with host-b's bytes.
	khB := filepath.Join(khDir, "host-b.known_hosts")
	onDisk, err := os.ReadFile(khB)
	require.NoError(t, err)
	assert.Equal(t, []byte("kh-b\n"), onDisk)

	// Releasing the lease and removing the host drains it and removes the file.
	_ = c.Close() // release the lease
	require.NoError(t, reg.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{
		chost("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("kh-a\n")),
	}}))
	_, statErr := os.Stat(khB)
	assert.True(t, os.IsNotExist(statErr), "draining host-b must remove its materialised known_hosts file")
}

// TestSingleHostHostKeyPolicyUnchanged is the credential-refactor parity check:
// the single-host policy (empty override) still resolves to the global
// KnownHostsFile — byte-for-byte, exactly as before the refactor.
func TestSingleHostHostKeyPolicyUnchanged(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	p := resolveHostKeyPolicy()
	assert.Empty(t, p.knownHostsPath, "single-host policy carries no per-host override")
	assert.Equal(t, KnownHostsFile, p.knownHostsFile(), "single-host still verifies against the credential mount")
}

// TestNew_ClusteredMode_FromFile proves the top-level New() enters clustered
// mode from a mounted inventory file, projects the hosts (lazily, no dial), and
// Close shuts down cleanly.
func TestNew_ClusteredMode_FromFile(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()
	hostsFile := filepath.Join(dir, hostsecret.SecretDataKey)
	writeInventory(t, hostsFile,
		chost("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("kh-a\n")),
		chost("host-b", "qemu+ssh://virt@host-b/system", []byte("KEY-B"), []byte("kh-b\n")),
	)
	t.Setenv(EnvHostsFile, hostsFile)
	t.Setenv(EnvClusterKnownHostsDir, filepath.Join(dir, "kh"))

	p, err := New()
	require.NoError(t, err)
	require.NotNil(t, p.clusterReg, "New() with a mounted inventory must enter clustered mode")
	assert.Equal(t, []hostconn.HostID{"host-a", "host-b"}, p.registry.Hosts())
	assert.Equal(t, hostconn.HostID(""), p.hostID, "clustered mode has no single hostID")

	require.NoError(t, p.Close())
}

// TestNew_ClusteredMode_MalformedFileIsNonFatal proves a malformed inventory at
// startup does NOT crash the clustered provider: it starts empty and the watcher
// will reconcile when the file becomes valid.
func TestNew_ClusteredMode_MalformedFileIsNonFatal(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()
	hostsFile := filepath.Join(dir, hostsecret.SecretDataKey)
	require.NoError(t, os.WriteFile(hostsFile, []byte("{ this is not valid json"), 0o600))
	t.Setenv(EnvHostsFile, hostsFile)
	t.Setenv(EnvClusterKnownHostsDir, filepath.Join(dir, "kh"))

	p, err := New()
	require.NoError(t, err, "malformed inventory must not be fatal in clustered mode")
	require.NotNil(t, p.clusterReg)
	assert.Empty(t, p.registry.Hosts(), "malformed inventory -> empty host set (watcher will reconcile)")
	require.NoError(t, p.Close())
}
