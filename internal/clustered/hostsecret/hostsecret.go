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
// (id, endpoint, labels) plus a defined-but-empty Credentials sub-document (see
// Credentials).
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

	// Credentials is the per-host connection material. It is DEFINED here so the
	// Secret schema is stable across the projected-Secret slice, but the Provider
	// controller does NOT populate it in the first (structural) PR — see
	// Credentials. It is always present in the JSON (an empty document renders as
	// {}), never omitted, so consumers can rely on the key existing.
	Credentials Credentials `json:"credentials"`
}

// Credentials is the per-host connection material a clustered provider needs to
// open a connection to the host (e.g. an SSH private key + known_hosts entry,
// or a future agent token).
//
// SECURITY: this struct is DEFINED so the host-inventory schema is stable, but
// its fields are left UNPOPULATED in the first, structural projected-Secret PR.
// Resolving each Host's (or the Provider's) credentialSecretRef and inlining the
// material into this document is a separate, security-reviewed PR (ADR-0007 P1).
// Until then every rendered host carries an empty Credentials ({}), and the
// render path never reads or logs any credential value. The concrete field shape
// below is provisional and owned by the credential-resolution PR; because the
// document is versioned (SchemaVersion) and consumed only from a mounted file by
// our own provider, that PR may extend or reshape it additively (or bump the
// version) without a wire-contract break.
type Credentials struct {
	// SSHPrivateKeyRef names the key, within the host's mounted credential
	// material, that holds the SSH private key. Reserved for the
	// credential-resolution PR; always empty today.
	SSHPrivateKeyRef string `json:"sshPrivateKeyRef,omitempty"`

	// KnownHostsRef names the key holding the host's known_hosts entry, pinning
	// the host key for strict host-key verification (ADR-0004). Reserved for the
	// credential-resolution PR; always empty today.
	KnownHostsRef string `json:"knownHostsRef,omitempty"`
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
