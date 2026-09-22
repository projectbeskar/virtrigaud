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
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// This file is the libvirt provider's CLUSTERED-mode wiring (ADR-0007 D3): the
// mode detection, the production Dialer that turns one inventory host into one
// per-host connection, and the credential-sourcing refactor that lets that
// Dialer reuse the exact single-host SSH/host-key machinery with the host's
// OWN inlined material. It reads the mounted inventory file only — never the
// Kubernetes API (#297).

const (
	// EnvHostsFile overrides the mounted host-inventory file path (tests). In
	// production the file is at hostsecret.MountPath/hostsecret.SecretDataKey.
	EnvHostsFile = "VIRTRIGAUD_LIBVIRT_HOSTS_FILE"

	// EnvClusterKnownHostsDir overrides the writable directory into which a
	// clustered host's inlined known_hosts bytes are materialised (tests). In
	// production it defaults to a subdirectory of the pod's writable /tmp mount
	// (the pod root filesystem is read-only; /tmp is an emptyDir).
	EnvClusterKnownHostsDir = "VIRTRIGAUD_LIBVIRT_KNOWN_HOSTS_DIR"

	// defaultClusterKnownHostsDir is the production known_hosts materialisation
	// directory: under /tmp so it is writable with a read-only root filesystem.
	// These are public host keys (not secret), but each host's file is written
	// 0600 and removed when its connection drains.
	defaultClusterKnownHostsDir = "/tmp/virtrigaud-cluster-knownhosts"
)

// hostsFilePath returns the mounted host-inventory file path, overridable via
// EnvHostsFile for tests.
func hostsFilePath() string {
	if p := strings.TrimSpace(os.Getenv(EnvHostsFile)); p != "" {
		return p
	}
	return filepath.Join(hostsecret.MountPath, hostsecret.SecretDataKey)
}

// clusterKnownHostsDir returns the writable known_hosts materialisation
// directory, overridable via EnvClusterKnownHostsDir for tests.
func clusterKnownHostsDir() string {
	if p := strings.TrimSpace(os.Getenv(EnvClusterKnownHostsDir)); p != "" {
		return p
	}
	return defaultClusterKnownHostsDir
}

// clusteredModeEnabled reports whether the host-inventory file is present. Its
// PRESENCE selects clustered topology; its ABSENCE keeps the byte-for-byte
// single-host path (ADR-0007 D9). A stat error other than not-exist is treated
// as "present" so a transiently-unreadable mount never silently downgrades a
// clustered provider to single-host (which would dial the wrong/absent
// PROVIDER_ENDPOINT).
func clusteredModeEnabled(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// newClusterDialer returns the production hostconn.Dialer: it builds one
// per-host *virshConn from an inventory entry, reusing the single-host SSH
// transport and host-key machinery with THIS host's endpoint and inlined
// credential material. It performs NO network dial — the SSH connection opens
// lazily on the first virsh/host command (ADR-0007 D3 lazy-open); the only I/O
// here is materialising the host's known_hosts bytes to a private temp file.
//
// knownHostsDir must be writable (the pod's /tmp). logger is the provider's
// structured logger.
func newClusterDialer(knownHostsDir string, logger *slog.Logger) hostconn.Dialer {
	return func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		vp, cleanup, err := buildClusteredVirshProvider(h, knownHostsDir, logger)
		if err != nil {
			return nil, err
		}
		return newClusteredVirshConn(hostconn.HostID(h.ID), vp, cleanup), nil
	}
}

// buildClusteredVirshProvider constructs a per-host VirshProvider from one
// inventory entry WITHOUT initializing it (no loadCredentialsFromEnv, no eager
// testConnection): the endpoint becomes the connection URI, the inlined SSH key
// becomes the in-memory credential, and the inlined known_hosts becomes this
// host's verification policy. The SSH connection is dialed lazily on first use
// (ensureSSHClient), and dialSSH re-runs the ADR-0004 host-key pre-flight
// against THIS host's known_hosts on every dial — so a mis-seeded host fails
// loudly at first use, never silently.
//
// The returned cleanup removes the materialised known_hosts file; it is run by
// the connection's Close (drain/registry-close).
func buildClusteredVirshProvider(h hostsecret.Host, knownHostsDir string, logger *slog.Logger) (*VirshProvider, func(), error) {
	endpoint := strings.TrimSpace(h.Endpoint)
	if endpoint == "" {
		return nil, nil, fmt.Errorf("clustered host %q has an empty endpoint", h.ID)
	}
	if _, err := url.Parse(endpoint); err != nil {
		return nil, nil, fmt.Errorf("clustered host %q endpoint is not a valid URI: %w", h.ID, err)
	}

	policy, cleanup, err := newHostKeyPolicyFromBytes(knownHostsDir, h.ID, h.Credentials.KnownHosts)
	if err != nil {
		return nil, nil, err
	}

	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{Endpoint: endpoint}})
	// Set the connection state directly — the same fields setupConnection
	// resolves for single-host, minus the eager dial. The SSH user is part of
	// the endpoint URI (qemu+ssh://user@host/system), so no username is injected
	// here; the clustered model is key-based SSH only (no inlined password).
	vp.uri = endpoint
	vp.env = os.Environ()
	vp.credentials = &Credentials{SSHPrivateKey: string(h.Credentials.SSHPrivateKey)}
	vp.hostKey = policy
	vp.logger = logger
	return vp, cleanup, nil
}

// newHostKeyPolicyFromBytes is the credential-sourcing refactor's clustered
// half: it builds a host-key verification policy from a host's inlined
// known_hosts BYTES instead of the single-host credential-mount file, WITHOUT
// forking the verification logic — it materialises the bytes to a private file
// and points hostKeyPolicy.knownHostsPath at it, so the exact same
// knownhosts.New(path) path verifies against this host's own trust material.
//
// It preserves the ADR-0004 posture byte-for-byte: the insecure escape hatch is
// read from the same process-wide env var (resolveHostKeyPolicy), and a host
// with EMPTY known_hosts still gets a (zero-byte) file — so verifyKnownHostsPresent
// hard-fails at connect time unless the operator has opted into the insecure
// mode, exactly as the single-host path does. Never weakened, never TOFU.
//
// The returned cleanup removes the materialised file (a no-op on the insecure
// path, where no file is written).
func newHostKeyPolicyFromBytes(knownHostsDir, hostID string, knownHosts []byte) (hostKeyPolicy, func(), error) {
	noop := func() {}
	policy := resolveHostKeyPolicy()
	if policy.insecure {
		// Insecure escape hatch: verification is off; no known_hosts file needed.
		return policy, noop, nil
	}
	path, err := materializeKnownHosts(knownHostsDir, hostID, knownHosts)
	if err != nil {
		return hostKeyPolicy{}, noop, err
	}
	policy.knownHostsPath = path
	return policy, func() { _ = os.Remove(path) }, nil
}

// materializeKnownHosts writes a host's known_hosts bytes to a private file in
// dir and returns its path. An EMPTY input is written deliberately (a zero-byte
// file), so the host-key pre-flight hard-fails for a host with no trust material
// rather than falling back to some other host's known_hosts. The bytes are
// public host keys, not secret, but the file is 0600 for tidiness under a
// shared /tmp.
func materializeKnownHosts(dir, hostID string, knownHosts []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create known_hosts dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, sanitizeHostID(hostID)+".known_hosts")
	if err := os.WriteFile(path, knownHosts, 0o600); err != nil {
		return "", fmt.Errorf("materialize known_hosts for host %q: %w", hostID, err)
	}
	return path, nil
}

// sanitizeHostID maps a host id to a safe single filename component. Host ids
// are Host CR names (DNS-1123, already filename-safe), but any character outside
// [A-Za-z0-9._-] is replaced defensively so a hostile/malformed id can never
// escape the known_hosts directory via a path separator.
func sanitizeHostID(id string) string {
	if id == "" {
		return "_empty_"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	// A name consisting only of dots (".", "..") would be path-hostile; the
	// replacement above leaves dots intact, so guard the pure-dots case.
	out := b.String()
	if strings.Trim(out, ".") == "" {
		return "_" + out + "_"
	}
	return out
}
