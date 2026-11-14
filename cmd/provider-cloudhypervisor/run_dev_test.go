//go:build virtrigaud_dev

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

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDevelopmentFlags: the agent client certificate is required, and a
// certificate or inventory that cannot be read stops the Provider.
func TestDevelopmentFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"--port=1"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "--agent-client-cert") {
		t.Errorf("no client certificate: exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	if code := realMain([]string{"--agent-client-cert=c", "--agent-client-key=k", "extra"}, &out, &errOut); code != exitUsage {
		t.Errorf("extra argument: exit %d", code)
	}
	errOut.Reset()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	if code := realMain([]string{"--agent-client-cert=" + missing, "--agent-client-key=" + missing,
		"--hosts-file=" + filepath.Join(dir, "hosts.json")}, &out, &errOut); code != exitError {
		t.Errorf("unreadable certificate: exit %d\n%s", code, errOut.String())
	}
	out.Reset()
	if code := realMain([]string{"--version"}, &out, &errOut); code != exitOK || !strings.Contains(out.String(), "development build") {
		t.Errorf("--version: exit %d\n%s", code, out.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the test expects the certificate to be missing")
	}
}

// TestHostsSecretFlags: --hosts-secret excludes --hosts-file, its namespace
// comes from the flag, the environment or the pod's namespace file, and
// outside a cluster it stops the Provider.
func TestHostsSecretFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	base := []string{"--agent-client-cert=c", "--agent-client-key=k"}
	if code := realMain(append(base, "--hosts-secret=ch-hosts", "--hosts-file=/x"), &out, &errOut); code != exitUsage ||
		!strings.Contains(errOut.String(), "mutually exclusive") {
		t.Errorf("both sources: exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	if code := realMain(append(base, "--hosts-secret-namespace=infra"), &out, &errOut); code != exitUsage {
		t.Errorf("namespace without a Secret: exit %d\n%s", code, errOut.String())
	}
	f, err := parseFlags(append(base, "--hosts-secret=ch-hosts"), &errOut)
	if err != nil || f.hostsSecret != "ch-hosts" || f.hostsFileSet {
		t.Errorf("--hosts-secret: %+v %v", f, err)
	}

	dir := t.TempDir()
	nsFile := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsFile, []byte("pod-ns\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ flag, env, file, want string }{
		{"flag-ns", "env-ns", nsFile, "flag-ns"},
		{"", "env-ns", nsFile, "env-ns"},
		{"", "", nsFile, "pod-ns"},
	} {
		got, err := secretNamespace(c.flag, c.env, c.file)
		if err != nil || got != c.want {
			t.Errorf("secretNamespace(%q, %q): %q %v, want %q", c.flag, c.env, got, err, c.want)
		}
	}
	if _, err := secretNamespace("", "", filepath.Join(dir, "absent")); err == nil {
		t.Error("an unknown namespace is an error")
	}

	// Outside a cluster there are no in-cluster credentials: the Provider
	// stops (after loading its certificate, so use readable files).
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv(envProviderNamespace, "infra")
	t.Setenv("VIRTRIGAUD_PROVIDER_INSECURE", "true")
	certFile, keyFile := writeTestKeyPair(t, dir)
	errOut.Reset()
	if code := realMain([]string{"--agent-client-cert=" + certFile, "--agent-client-key=" + keyFile, "--hosts-secret=ch-hosts"},
		&out, &errOut); code != exitError || !strings.Contains(errOut.String(), "API credentials") {
		t.Errorf("outside a cluster: exit %d\n%s", code, errOut.String())
	}
}

// writeTestKeyPair writes a self-signed certificate and its key to dir.
func writeTestKeyPair(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "provider"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
