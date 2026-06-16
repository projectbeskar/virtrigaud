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
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStorageOptionsRoundTrip verifies the storage_options_json shape survives a
// marshal/parse round trip, including UsePathStyle=false (the defaulted-bool
// footgun: it must NOT be dropped — PR #235).
func TestStorageOptionsRoundTrip(t *testing.T) {
	in := StorageOptions{
		Backend:      BackendS3,
		Bucket:       "virtrigaud",
		Endpoint:     "http://rustfs.lab.k8:9000",
		Region:       "us-east-1",
		Prefix:       "prod/",
		UsePathStyle: true,
	}
	raw, err := MarshalStorageOptions(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := ParseStorageOptions(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}

	// usePathStyle=false must be present in the JSON (no omitempty footgun).
	rawFalse, _ := MarshalStorageOptions(StorageOptions{Bucket: "b", UsePathStyle: false})
	if !strings.Contains(rawFalse, "usePathStyle") {
		t.Errorf("usePathStyle=false dropped from JSON: %s", rawFalse)
	}

	// Empty json parses to a zero value (legacy pvc path passes no options).
	if got, err := ParseStorageOptions(""); err != nil || got != (StorageOptions{}) {
		t.Errorf("ParseStorageOptions(\"\") = (%+v,%v), want zero,nil", got, err)
	}
}

// TestS3StorageConfigFromRequest builds a storage config from the gRPC request
// fields and asserts credential keys land where the S3 client expects them.
func TestS3StorageConfigFromRequest(t *testing.T) {
	opts, _ := MarshalStorageOptions(StorageOptions{
		Backend: BackendS3, Bucket: "virtrigaud", Endpoint: "http://rustfs.lab.k8:9000", UsePathStyle: true,
	})
	creds := map[string]string{
		CredKeyAccessKeyID:     "AKIA",
		CredKeySecretAccessKey: "secret",
		CredKeySessionToken:    "tok",
	}
	cfg, err := S3StorageConfigFromRequest(opts, creds)
	if err != nil {
		t.Fatalf("S3StorageConfigFromRequest: %v", err)
	}
	if cfg.Type != BackendS3 || cfg.S3 == nil {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.S3.Bucket != "virtrigaud" || cfg.S3.Endpoint != "http://rustfs.lab.k8:9000" || !cfg.S3.UsePathStyle {
		t.Errorf("options not propagated: %+v", cfg.S3)
	}
	if cfg.S3.AccessKeyID != "AKIA" || cfg.S3.SecretAccessKey != "secret" || cfg.S3.SessionToken != "tok" {
		t.Errorf("credentials not propagated: %+v", cfg.S3)
	}

	// Missing credentials must error.
	if _, err := S3StorageConfigFromRequest(opts, map[string]string{}); err == nil {
		t.Error("expected error for missing credentials")
	}
	// Missing bucket must error.
	noBucket, _ := MarshalStorageOptions(StorageOptions{Backend: BackendS3})
	if _, err := S3StorageConfigFromRequest(noBucket, creds); err == nil {
		t.Error("expected error for missing bucket")
	}
}

// TestEnsureRelayMode verifies only relay/auto/empty are accepted; an explicit
// direct fails loudly with InvalidArgument (ADR-0006 D2 loud-fail).
func TestEnsureRelayMode(t *testing.T) {
	for _, mode := range []string{"", TransferModeAuto, TransferModeRelay} {
		if err := EnsureRelayMode(mode); err != nil {
			t.Errorf("EnsureRelayMode(%q) = %v, want nil", mode, err)
		}
	}
	err := EnsureRelayMode(TransferModeDirect)
	if err == nil {
		t.Fatal("EnsureRelayMode(direct) = nil, want error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("EnsureRelayMode(direct) code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestEnsurePVCOrS3Backend verifies pvc/s3/empty pass and nfs/unknown fail with
// Unimplemented.
func TestEnsurePVCOrS3Backend(t *testing.T) {
	for _, b := range []string{"", BackendPVC, BackendS3} {
		if err := EnsurePVCOrS3Backend(b); err != nil {
			t.Errorf("EnsurePVCOrS3Backend(%q) = %v, want nil", b, err)
		}
	}
	for _, b := range []string{BackendNFS, "gluster"} {
		err := EnsurePVCOrS3Backend(b)
		if err == nil {
			t.Errorf("EnsurePVCOrS3Backend(%q) = nil, want error", b)
		}
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("EnsurePVCOrS3Backend(%q) code = %v, want Unimplemented", b, status.Code(err))
		}
	}
}

// TestS3CapabilityHelpers verifies the per-direction Slice-1 advertisement
// helpers.
func TestS3CapabilityHelpers(t *testing.T) {
	if got := PVCAndS3ExportBackends(); len(got) != 2 || got[0] != BackendPVC || got[1] != BackendS3 {
		t.Errorf("PVCAndS3ExportBackends() = %v, want [pvc s3]", got)
	}
	if got := PVCAndS3ImportBackends(); len(got) != 2 || got[0] != BackendPVC || got[1] != BackendS3 {
		t.Errorf("PVCAndS3ImportBackends() = %v, want [pvc s3]", got)
	}
}
