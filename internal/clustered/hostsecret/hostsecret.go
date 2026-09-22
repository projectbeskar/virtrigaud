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

// Package hostsecret defines the versioned host-inventory schema that the
// Provider controller renders into a clustered provider's projected Secret
// (ADR-0007 D3), and that a clustered provider will later consume from a
// mounted file.
//
// The document is written to a Kubernetes Secret (never a ConfigMap) owned by
// the Provider, in the provider's namespace, and mounted read-only into the
// provider pod at MountPath. The provider reads MountPath/SecretDataKey from
// disk and never the Kubernetes API — preserving the no-API-access invariant
// hardened in #297. This package is deliberately dependency-free (stdlib only)
// so both the manager (root module) and the provider packages can import it
// without coupling to controller-runtime.
package hostsecret

import (
	"encoding/json"
	"sort"
)

const (
	// SchemaVersion is the current host-inventory schema version. It is written
	// into every rendered document so the provider can gate format evolution: a
	// provider that only understands version N treats an unknown (newer) version
	// as a hard error rather than silently mis-parsing a changed shape.
	SchemaVersion = 1

	// SecretDataKey is the key in the projected Secret's data map under which the
	// marshaled Inventory JSON is stored (a single key: the whole inventory is
	// one document).
	SecretDataKey = "hosts.json"

	// MountPath is the read-only directory at which the Provider controller
	// mounts the host-inventory Secret into the provider pod. The provider reads
	// MountPath/SecretDataKey (wired in a later PR); the controller and provider
	// agree on the location via these shared constants rather than duplicating a
	// path literal on each side.
	MountPath = "/etc/virtrigaud/hosts"
)

// Inventory is the versioned host-inventory document rendered into the projected
// Secret. It is the complete set of hosts a clustered provider fronts, as
// derived by the operator from the Host CRs — the provider is told which hosts
// to operate on; it does not decide the inventory (ADR-0007 D1/D3).
type Inventory struct {
	// SchemaVersion is the document's schema version (always SchemaVersion when
	// produced by Marshal). See the SchemaVersion constant.
	SchemaVersion int `json:"schemaVersion"`

	// Hosts is the set of hosts the provider fronts. Marshal emits it sorted by
	// ID and never as JSON null (an empty inventory renders "hosts": []), so
	// re-renders of an unchanged inventory are byte-for-byte stable.
	Hosts []Host `json:"hosts"`
}

// Host is one host a clustered provider fronts. It carries connection metadata
// (id, endpoint, labels) plus the per-host connection Credentials the provider
// uses to reach it (see Credentials).
type Host struct {
	// ID is the stable host identifier — the Host CR name. It is the key the
	// provider uses to open and address a per-host connection, and the sort key
	// for deterministic rendering.
	ID string `json:"id"`

	// Endpoint is the host connection URI (libvirt: qemu+ssh://user@host/system;
	// a future agent host: grpc://host:port). Copied from Host.spec.endpoint.
	Endpoint string `json:"endpoint"`

	// Labels are the host's placement facts (storage-pool/network visibility,
	// zone/rack), copied from Host.spec.labels. Omitted when empty.
	Labels map[string]string `json:"labels,omitempty"`

	// Credentials is the per-host connection material a clustered provider reads
	// to open a connection to this host. It is always present in the JSON (a host
	// with no resolvable credentials is never rendered — see the controller's
	// credential resolution — so a rendered host always carries usable material,
	// but an unpopulated Credentials still renders as {} for schema stability).
	Credentials Credentials `json:"credentials"`
}

// Credentials is the per-host connection material a clustered provider needs to
// open a connection to the host. Today this is the SSH material the libvirt
// provider already consumes for a single host (ADR-0004 / ADR-0008): an SSH
// private key plus the host's known_hosts entry. The field set intentionally
// MIRRORS the keys of the libvirt credential Secret so the clustered provider (a
// later PR) can consume the inlined material through the SAME code path it uses
// today for single-host credentials, reading it from the mounted inventory file
// instead of the per-provider credential mount — never the Kubernetes API (#297).
//
// SECURITY: the values here are secret material. They live ONLY inside the
// Opaque, Provider-owned inventory Secret (the same protection boundary as the
// source credential Secret). This is a deliberate credential DUPLICATION
// (source Secret → rendered inventory Secret): the #297 no-API-access invariant
// requires the provider to read connection material from a file, so the operator
// copies it there once. The render path MUST NOT log any value in this struct.
//
// The material is carried as raw bytes ([]byte, base64 in JSON) copied verbatim
// from the source Secret's data map — no string round-trip, no trimming — so a
// PEM key or a known_hosts line survives byte-for-byte with no encoding
// corruption. Username is NOT carried here: the SSH user is part of the endpoint
// URI (qemu+ssh://user@host/system), which the in-process SSH client reads
// directly. Password auth is deliberately NOT inlined: the clustered model
// standardizes on key-based SSH with known_hosts verification (ADR-0004), and
// ADR-0008 D8 already leans to removing SSH password auth.
//
// The document is versioned (SchemaVersion): populating these already-declared
// fields is additive, so SchemaVersion stays 1.
type Credentials struct {
	// SSHPrivateKey is the SSH private key material used to authenticate to the
	// host. It mirrors the "ssh-privatekey" key of the libvirt credential Secret
	// (read today at /etc/virtrigaud/credentials/ssh-privatekey; the key name
	// follows the kubernetes.io/ssh-auth convention). Raw PEM/OpenSSH bytes,
	// verbatim from the source Secret. Omitted only for an unpopulated
	// Credentials ({}); a rendered host always carries it.
	SSHPrivateKey []byte `json:"sshPrivateKey,omitempty"`

	// KnownHosts is the host's known_hosts entry, pinning the host key for strict
	// SSH host-key verification (ADR-0004 — no TOFU). It mirrors the "known_hosts"
	// key of the libvirt credential Secret (read today at
	// /etc/virtrigaud/credentials/known_hosts). Raw bytes, verbatim from the
	// source Secret. Omitted when the source Secret carries no known_hosts entry;
	// the provider still enforces its ADR-0004 host-key policy at connect time
	// (hard-fail unless the audit-flagged insecure escape hatch is set), so the
	// operator does not second-guess that policy here.
	KnownHosts []byte `json:"knownHosts,omitempty"`
}

// Marshal serializes inv into the bytes stored under SecretDataKey. It is
// deterministic so that re-rendering an unchanged inventory produces identical
// bytes (no spurious Secret updates, diff-stable review):
//   - hosts are sorted by ID (a stable, total order over unique Host CR names);
//   - a nil/empty host slice is rendered as [] (never JSON null);
//   - encoding/json emits map keys (labels) in sorted order.
//
// Marshal does not mutate inv.
func Marshal(inv Inventory) ([]byte, error) {
	// Sort a copy so the caller's slice is never reordered.
	hosts := make([]Host, len(inv.Hosts))
	copy(hosts, inv.Hosts)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })

	// make() above returns a non-nil slice even when len==0, so an empty
	// inventory marshals to "hosts": [] rather than "hosts": null.
	normalized := Inventory{
		SchemaVersion: inv.SchemaVersion,
		Hosts:         hosts,
	}
	return json.MarshalIndent(normalized, "", "  ")
}

// Unmarshal parses a host-inventory document (as produced by Marshal) into an
// Inventory. Callers that need to reject unknown future schema versions should
// compare the returned SchemaVersion against SchemaVersion themselves.
func Unmarshal(data []byte) (Inventory, error) {
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}
