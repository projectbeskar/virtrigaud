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

package migration

import (
	"encoding/json"
	"fmt"
)

// S3 credential map keys (the keys of the gRPC ExportDiskRequest/
// ImportDiskRequest.credentials map for the s3 backend, and the expected keys of
// the referenced Kubernetes Secret). Formalized per ADR-0006 so the controller,
// the Secret, and every provider agree on one spelling. Credential *values* are
// secret material and must never appear in logs, events, or Status.
const (
	// CredKeyAccessKeyID is the S3 access key ID.
	CredKeyAccessKeyID = "accessKeyID"
	// CredKeySecretAccessKey is the S3 secret access key.
	CredKeySecretAccessKey = "secretAccessKey"
	// CredKeySessionToken is the optional S3 session token (STS / temporary
	// credentials). Empty when long-lived credentials are used.
	CredKeySessionToken = "sessionToken"
)

// StorageOptions carries the non-secret, backend-specific parameters that the
// controller resolves from MigrationStorage and ships to a provider in the gRPC
// storage_options_json field. It deliberately holds NO credential material —
// credentials travel in the separate credentials map and are redacted
// everywhere (ADR-0006 security section).
type StorageOptions struct {
	// Backend is the staging backend ("s3" today; "nfs"/"pvc" later).
	Backend string `json:"backend,omitempty"`
	// Bucket is the S3 bucket name.
	Bucket string `json:"bucket,omitempty"`
	// Endpoint is the S3 endpoint URL. An "http://" scheme selects plaintext
	// HTTP (e.g. a lab rustfs/MinIO); empty means the AWS default endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the S3 region (empty = SDK default, "us-east-1").
	Region string `json:"region,omitempty"`
	// Prefix is the key prefix under the bucket for this migration's objects.
	Prefix string `json:"prefix,omitempty"`
	// UsePathStyle selects path-style addressing (needed by most non-AWS S3).
	// No omitempty: a defaulted bool must survive the JSON round-trip even when
	// false (PR #235 footgun).
	UsePathStyle bool `json:"usePathStyle"`

	// --- NFS backend (ADR-0006 Slice 4) ---
	// Server is the NFS server host. It is SSRF-validated by the controller before
	// being shipped, and re-sanitised by NFSURL when the qemu-img URL is built.
	Server string `json:"server,omitempty"`
	// Export is the absolute exported path on the server (e.g. "/export/migrations").
	Export string `json:"export,omitempty"`
	// Path is an optional sub-path within the export for this migration's data.
	Path string `json:"path,omitempty"`
	// UID/GID are the AUTH_SYS identity presented to the NFS server via the libnfs
	// URL query params. Nil means the qemu-img process identity (the pod or SSH
	// user). The export's squash policy is the real authorization boundary.
	UID *int64 `json:"uid,omitempty"`
	GID *int64 `json:"gid,omitempty"`
}

// MarshalStorageOptions serializes StorageOptions to the compact JSON carried in
// the gRPC storage_options_json field.
func MarshalStorageOptions(opts StorageOptions) (string, error) {
	b, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("marshal storage options: %w", err)
	}
	return string(b), nil
}

// ParseStorageOptions deserializes the gRPC storage_options_json field. An empty
// string yields a zero StorageOptions (the legacy pvc path passes no options).
func ParseStorageOptions(raw string) (StorageOptions, error) {
	var opts StorageOptions
	if raw == "" {
		return opts, nil
	}
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return StorageOptions{}, fmt.Errorf("parse storage options json: %w", err)
	}
	return opts, nil
}
