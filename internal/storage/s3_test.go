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

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseS3Endpoint covers the endpoint/secure derivation, including the
// plaintext-HTTP lab case (rustfs http://...:9000) and the AWS default.
func TestParseS3Endpoint(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		wantHost   string
		wantSecure bool
		wantErr    bool
	}{
		{name: "empty = AWS default secure", endpoint: "", wantHost: "s3.amazonaws.com", wantSecure: true},
		{name: "http selects plaintext", endpoint: "http://rustfs.lab.k8:9000", wantHost: "rustfs.lab.k8:9000", wantSecure: false},
		{name: "https selects TLS", endpoint: "https://minio.example.com", wantHost: "minio.example.com", wantSecure: true},
		{name: "bare host defaults to TLS", endpoint: "minio.example.com:9000", wantHost: "minio.example.com:9000", wantSecure: true},
		{name: "unsupported scheme errors", endpoint: "ftp://x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, secure, err := parseS3Endpoint(tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseS3Endpoint(%q) = nil err, want error", tt.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseS3Endpoint(%q) unexpected err: %v", tt.endpoint, err)
			}
			if host != tt.wantHost || secure != tt.wantSecure {
				t.Errorf("parseS3Endpoint(%q) = (%q,%v), want (%q,%v)", tt.endpoint, host, secure, tt.wantHost, tt.wantSecure)
			}
		})
	}
}

// TestParseS3Key covers s3://bucket/key, bucket-prefixed, and bare-key forms.
func TestParseS3Key(t *testing.T) {
	s := &S3Storage{bucket: "virtrigaud"}
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "s3://virtrigaud/vmmigrations/ns/m/export.vmdk", want: "vmmigrations/ns/m/export.vmdk"},
		{in: "virtrigaud/vmmigrations/ns/m/export.vmdk", want: "vmmigrations/ns/m/export.vmdk"},
		{in: "s3://other-bucket/key", want: "other-bucket/key"}, // bucket mismatch ⇒ whole remainder is the key
		{in: "vmmigrations/ns/m/export.vmdk", want: "vmmigrations/ns/m/export.vmdk"},
		{in: "s3://virtrigaud/", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := s.parseS3Key(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseS3Key(%q) = nil err, want error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseS3Key(%q) unexpected err: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseS3Key(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNewS3StorageValidation asserts NewS3Storage rejects missing config without
// reaching out to any network.
func TestNewS3StorageValidation(t *testing.T) {
	if _, err := NewS3Storage(StorageConfig{Type: "s3"}); err == nil {
		t.Error("NewS3Storage with no S3 config should error")
	}
	if _, err := NewS3Storage(StorageConfig{Type: "s3", S3: &S3Config{Bucket: "b"}}); err == nil {
		t.Error("NewS3Storage with no credentials should error")
	}
	// A complete (lab-shaped) config constructs a client without dialing.
	if _, err := NewS3Storage(StorageConfig{Type: "s3", S3: &S3Config{
		Endpoint: "http://rustfs.lab.k8:9000", Bucket: "virtrigaud", UsePathStyle: true,
		AccessKeyID: "ak", SecretAccessKey: "sk",
	}}); err != nil {
		t.Errorf("NewS3Storage with valid config errored: %v", err)
	}
}

// TestPVCStreamRoundTripChecksum exercises the streaming interface (UploadStream/
// DownloadStream) and the in-stream SHA256 on the hermetic PVC backend: a
// streamed upload then a streamed download must reproduce the bytes and the same
// SHA256, and a wrong ExpectedChecksum must fail the download. This validates the
// streaming + checksum contract that the S3 backend implements identically (the
// S3 round trip itself is lab-validated against rustfs).
func TestPVCStreamRoundTripChecksum(t *testing.T) {
	dir := t.TempDir()
	// PVC backend resolves pvc://<name>/<path> to <mount>/<path>; point the mount
	// at a temp dir and address objects under a "vmmigrations/..." subpath.
	st, err := NewPVCStorage(StorageConfig{Type: "pvc", MountPath: dir})
	if err != nil {
		t.Fatalf("NewPVCStorage: %v", err)
	}
	defer func() { _ = st.Close() }()

	payload := bytes.Repeat([]byte("virtrigaud-slice1-"), 4096) // ~72 KiB
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	url := "pvc://teststore/vmmigrations/ns/m/export.vmdk"
	up, err := st.UploadStream(context.Background(), StreamUploadRequest{
		DestinationURL: url,
		Reader:         bytes.NewReader(payload),
		ContentLength:  int64(len(payload)),
	})
	if err != nil {
		t.Fatalf("UploadStream: %v", err)
	}
	if up.Checksum != wantHex {
		t.Errorf("upload checksum = %s, want %s", up.Checksum, wantHex)
	}
	if up.BytesTransferred != int64(len(payload)) {
		t.Errorf("bytesTransferred = %d, want %d", up.BytesTransferred, len(payload))
	}

	// Confirm the bytes actually landed.
	written, err := os.ReadFile(filepath.Join(dir, "vmmigrations/ns/m/export.vmdk"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(written, payload) {
		t.Error("written bytes differ from payload")
	}

	// Streamed download with the correct expected checksum succeeds.
	var got bytes.Buffer
	dn, err := st.DownloadStream(context.Background(), StreamDownloadRequest{
		SourceURL:        url,
		Writer:           &got,
		ExpectedChecksum: wantHex,
	})
	if err != nil {
		t.Fatalf("DownloadStream: %v", err)
	}
	if dn.Checksum != wantHex {
		t.Errorf("download checksum = %s, want %s", dn.Checksum, wantHex)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Error("downloaded bytes differ from payload")
	}

	// A wrong expected checksum must fail with ErrorTypeChecksumMismatch.
	_, err = st.DownloadStream(context.Background(), StreamDownloadRequest{
		SourceURL:        url,
		Writer:           &bytes.Buffer{},
		ExpectedChecksum: strings.Repeat("0", 64),
	})
	if err == nil {
		t.Fatal("DownloadStream with wrong checksum should error")
	}
	if se, ok := err.(*StorageError); !ok || se.Type != ErrorTypeChecksumMismatch {
		t.Errorf("wrong-checksum error = %v, want ErrorTypeChecksumMismatch", err)
	}
}
