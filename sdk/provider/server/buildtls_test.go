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

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestKeyPair mints a self-signed cert/key PEM pair under dir and returns
// their absolute paths. The cert doubles as its own CA, which is all the
// buildServerTLSConfig tests need (they assert on config fields, not on an
// actual handshake).
func writeTestKeyPair(t *testing.T, dir, certName, keyName string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-provider"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"test-provider.svc"},
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath = filepath.Join(dir, certName)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPath = filepath.Join(dir, keyName)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// TestBuildServerTLSConfig_MTLS asserts the load-bearing security invariants
// for the provider startup path (ADR-0003 PR-2): when RequireClientCert is
// set with a CA bundle present, the resulting *tls.Config pins MinVersion to
// TLS 1.3, requires-and-verifies the client cert, and loads the CA pool.
func TestBuildServerTLSConfig_MTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir, "tls.crt", "tls.key")
	// Reuse the same self-signed cert as the client CA bundle.
	caPath := certPath

	cfg, err := buildServerTLSConfig(&TLSConfig{
		CertFile:          certPath,
		KeyFile:           keyPath,
		CAFile:            caPath,
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatalf("buildServerTLSConfig: unexpected error: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3 (%#x)", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil, want a populated pool")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(cfg.Certificates))
	}
}

// TestBuildServerTLSConfig_NoClientCert covers the server-TLS-only path
// (RequireClientCert=false): TLS is still 1.3-floored and a server cert is
// loaded, but no client-cert enforcement is configured.
func TestBuildServerTLSConfig_NoClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir, "tls.crt", "tls.key")

	cfg, err := buildServerTLSConfig(&TLSConfig{
		CertFile: certPath,
		KeyFile:  keyPath,
	})
	if err != nil {
		t.Fatalf("buildServerTLSConfig: unexpected error: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs != nil {
		t.Error("ClientCAs should be nil when RequireClientCert=false")
	}
}

// TestBuildServerTLSConfig_RequireClientCertWithoutCA verifies the guard that
// rejects RequireClientCert=true with no CAFile — an unverifiable, footgun
// configuration we refuse rather than silently accept.
func TestBuildServerTLSConfig_RequireClientCertWithoutCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir, "tls.crt", "tls.key")

	_, err := buildServerTLSConfig(&TLSConfig{
		CertFile:          certPath,
		KeyFile:           keyPath,
		RequireClientCert: true, // CAFile deliberately empty
	})
	if err == nil {
		t.Fatal("expected error when RequireClientCert=true and CAFile empty, got nil")
	}
}

// TestBuildServerTLSConfig_MissingCert ensures a missing cert/key pair is a
// hard error (the provider must refuse to start, not boot without TLS).
func TestBuildServerTLSConfig_MissingCert(t *testing.T) {
	_, err := buildServerTLSConfig(&TLSConfig{
		CertFile: "/nonexistent/tls.crt",
		KeyFile:  "/nonexistent/tls.key",
	})
	if err == nil {
		t.Fatal("expected error for missing cert/key, got nil")
	}
}
