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

// commonNameFromGetCertificate calls the *tls.Config's GetCertificate
// callback and parses the leaf's Subject CommonName. It is the
// observation point for "did the watcher pick up the rotated cert?".
func commonNameFromGetCertificate(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate is nil; AutoReload wiring is broken")
	}
	cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate returned error: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("GetCertificate returned an empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return leaf.Subject.CommonName
}

// writeNamedKeyPair mints a self-signed cert/key PEM pair with the given
// CommonName at the fixed certName/keyName paths under dir, overwriting
// any existing files. Returns the cert/key paths.
func writeNamedKeyPair(t *testing.T, dir, cn string) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	certPEM, keyPEM := mintSelfSignedPEM(t, cn)
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// mintSelfSignedPEM produces a self-signed cert/key pair PEM with the
// given CommonName. Returns bytes (not paths) so the caller controls the
// filename, which the rotation test needs.
func mintSelfSignedPEM(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{cn + ".svc"},
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// TestBuildServerTLSConfig_AutoReload_WiresGetCertificate asserts the
// core PR-3 wiring invariant: with AutoReload=true the resulting
// *tls.Config serves the leaf via GetCertificate (the certwatcher
// callback) and NOT via the static Certificates slice, and a non-nil
// watcher is returned for the caller to Start.
func TestBuildServerTLSConfig_AutoReload_WiresGetCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeNamedKeyPair(t, dir, "provider-v1")

	cfg, watcher, err := buildServerTLSConfig(&TLSConfig{
		CertFile:   certPath,
		KeyFile:    keyPath,
		AutoReload: true,
	})
	if err != nil {
		t.Fatalf("buildServerTLSConfig: unexpected error: %v", err)
	}
	if watcher == nil {
		t.Fatal("watcher must be non-nil when AutoReload=true")
	}
	if len(cfg.Certificates) != 0 {
		t.Errorf("Certificates should be empty under AutoReload; GetCertificate is the source")
	}
	if cn := commonNameFromGetCertificate(t, cfg); cn != "provider-v1" {
		t.Errorf("GetCertificate CN = %q, want %q", cn, "provider-v1")
	}
}

// TestBuildServerTLSConfig_AutoReload_PicksUpRotatedCert writes a new
// cert to the watched path and asserts the watcher's GetCertificate
// returns the rotated leaf — proving hot-reload works without a restart.
func TestBuildServerTLSConfig_AutoReload_PicksUpRotatedCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeNamedKeyPair(t, dir, "provider-old")

	cfg, watcher, err := buildServerTLSConfig(&TLSConfig{
		CertFile:   certPath,
		KeyFile:    keyPath,
		AutoReload: true,
	})
	if err != nil {
		t.Fatalf("buildServerTLSConfig: unexpected error: %v", err)
	}
	if cn := commonNameFromGetCertificate(t, cfg); cn != "provider-old" {
		t.Fatalf("initial CN = %q, want provider-old", cn)
	}

	// Rotate: overwrite the cert/key at the same path with a new CN, then
	// drive the watcher's read directly (ReadCertificate re-reads from
	// disk — the same thing the fsnotify loop calls on a file event). We
	// avoid depending on Start()'s fsnotify timing, which is flaky in CI.
	newCertPEM, newKeyPEM := mintSelfSignedPEM(t, "provider-new")
	if err := os.WriteFile(certPath, newCertPEM, 0o600); err != nil {
		t.Fatalf("rotate cert: %v", err)
	}
	if err := os.WriteFile(keyPath, newKeyPEM, 0o600); err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if err := watcher.ReadCertificate(); err != nil {
		t.Fatalf("ReadCertificate after rotation: %v", err)
	}

	if cn := commonNameFromGetCertificate(t, cfg); cn != "provider-new" {
		t.Errorf("post-rotation CN = %q, want provider-new (hot-reload did not take effect)", cn)
	}
}

// TestBuildServerTLSConfig_StaticLoad_NoWatcher is the regression guard
// for the PR-2 behaviour: AutoReload=false loads the leaf once into
// Certificates, returns no watcher, and leaves GetCertificate nil.
func TestBuildServerTLSConfig_StaticLoad_NoWatcher(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeNamedKeyPair(t, dir, "provider-static")

	cfg, watcher, err := buildServerTLSConfig(&TLSConfig{
		CertFile:   certPath,
		KeyFile:    keyPath,
		AutoReload: false,
	})
	if err != nil {
		t.Fatalf("buildServerTLSConfig: unexpected error: %v", err)
	}
	if watcher != nil {
		t.Error("watcher must be nil when AutoReload=false")
	}
	if cfg.GetCertificate != nil {
		t.Error("GetCertificate must be nil on the static-load path")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1 (static load)", len(cfg.Certificates))
	}
}
