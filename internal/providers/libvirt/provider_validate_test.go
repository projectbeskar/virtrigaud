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
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// This file covers Validate's topology dispatch (ADR-0007 P1, BUG 1). Before the
// fix, Validate unconditionally ran the single-host `virsh -c <endpoint> list` —
// which in CLUSTERED mode has no endpoint, so it executed `virsh -c "" list`,
// failed "cannot connect to the hypervisor", and marked the whole provider
// not-ready before the manager ever called GetHostInfo. The fix makes Validate
// clustered-aware while leaving the single-host path byte-for-byte unchanged.

// newTestClusteredProvider builds a clustered Provider through the real New()
// path from a mounted inventory of the given hosts, mirroring the production
// mode-detection (a mounted file selects clustered topology). Fake key/known_hosts
// bytes are fine: New() projects the hosts lazily and never dials here.
func newTestClusteredProvider(t *testing.T, hosts ...hostsecret.Host) *Provider {
	t.Helper()
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()
	hostsFile := filepath.Join(dir, hostsecret.SecretDataKey)
	writeInventory(t, hostsFile, hosts...)
	t.Setenv(EnvHostsFile, hostsFile)
	t.Setenv(EnvClusterKnownHostsDir, filepath.Join(dir, "kh"))

	p, err := New()
	require.NoError(t, err)
	require.NotNil(t, p.clusterReg, "New() with a mounted inventory must enter clustered mode")
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestValidate_ClusteredMode_Succeeds proves that in clustered mode Validate
// returns success by checking the registry (which fronts >= 1 host) instead of
// running the single-host virsh probe against a non-existent endpoint.
func TestValidate_ClusteredMode_Succeeds(t *testing.T) {
	p := newTestClusteredProvider(t,
		chost("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("kh-a\n")),
		chost("host-b", "qemu+ssh://virt@host-b/system", []byte("KEY-B"), []byte("kh-b\n")),
	)
	require.True(t, p.clustered())

	require.NoError(t, p.Validate(context.Background()),
		"clustered Validate must succeed on registry readiness, not run single-host virsh")
}

// TestValidate_ClusteredMode_DoesNotUseSingleHostVirsh is the hermetic proof that
// clustered Validate never touches the single-host virsh handle. With
// virshProvider set to nil, the OLD code path (single-host) returns
// "virsh provider not initialized"; the clustered path must ignore that handle
// entirely and still succeed via the registry. This is the fails-before /
// passes-after signal for BUG 1 that does not depend on a `virsh` binary being
// absent from the test host.
func TestValidate_ClusteredMode_DoesNotUseSingleHostVirsh(t *testing.T) {
	p := newTestClusteredProvider(t,
		chost("host-a", "qemu+ssh://virt@host-a/system", []byte("KEY-A"), []byte("kh-a\n")),
	)
	// Poison the single-host handle: any fall-through to the legacy path would
	// surface it (nil-guard error today, or `virsh -c "" list` in production).
	p.virshProvider = nil

	require.NoError(t, p.Validate(context.Background()),
		"clustered Validate must not dereference or invoke the single-host virsh handle")
}

// TestValidate_ClusteredMode_ZeroHosts_NotReady proves that a clustered provider
// currently fronting no hosts (e.g. a malformed inventory the watcher has not
// reconciled yet) reports a RETRYABLE not-ready rather than a hard failure or a
// false success — the manager re-checks once the inventory becomes valid.
func TestValidate_ClusteredMode_ZeroHosts_NotReady(t *testing.T) {
	t.Setenv(EnvInsecureSkipHostKeyVerification, "")
	dir := t.TempDir()
	hostsFile := filepath.Join(dir, hostsecret.SecretDataKey)
	// Malformed inventory -> clustered mode, empty host set (fail-safe).
	require.NoError(t, os.WriteFile(hostsFile, []byte("{ not valid json"), 0o600))
	t.Setenv(EnvHostsFile, hostsFile)
	t.Setenv(EnvClusterKnownHostsDir, filepath.Join(dir, "kh"))

	p, err := New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	require.True(t, p.clustered())
	require.Empty(t, p.clusterReg.Hosts())

	err = p.Validate(context.Background())
	require.Error(t, err, "a clustered provider with zero hosts is not ready")
	assert.Contains(t, err.Error(), "no hosts")
}

// TestValidate_SingleHost_RunsVirshListPath proves the single-host path is
// UNCHANGED: Validate still executes the `virsh ... list --all --name` probe. A
// fake `virsh` on PATH returns a canned domain list so the assertion is hermetic
// (no real hypervisor, no real virsh).
func TestValidate_SingleHost_RunsVirshListPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shim not applicable on Windows")
	}
	binDir := t.TempDir()
	fakeVirsh := filepath.Join(binDir, "virsh")
	// Prints two domain names and exits 0, regardless of args.
	require.NoError(t, os.WriteFile(fakeVirsh, []byte("#!/bin/sh\necho domA\necho domB\n"), 0o755)) //nolint:gosec // test shim
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu:///system"}})
	vp.uri = "qemu:///system" // local (non-ssh) -> runLocal execs `virsh` from PATH
	p := &Provider{virshProvider: vp}
	require.False(t, p.clustered(), "no registry -> single-host mode")

	require.NoError(t, p.Validate(context.Background()),
		"single-host Validate must still run the virsh list probe (unchanged)")
}

// TestValidate_SingleHost_NilProvider_Unchanged proves the single-host routing is
// unchanged: with no registry and no virsh handle, Validate still returns the
// exact "virsh provider not initialized" error it always did (it is NOT diverted
// to the clustered branch).
func TestValidate_SingleHost_NilProvider_Unchanged(t *testing.T) {
	p := &Provider{} // clusterReg nil -> single-host; virshProvider nil
	require.False(t, p.clustered())

	err := p.Validate(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "virsh provider not initialized")
}
