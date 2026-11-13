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

// Package trusttest builds a complete test PKI for a per-host agent: the
// host identity issuer, the Provider identity issuer, a grant anchor, the
// host's key and certificate, and Provider client certificates.
package trusttest

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// Default identities of a test PKI.
const (
	TrustDomain  = "example.test"
	HostNS       = "hosts"
	HostName     = "host-a"
	ProviderNS   = "infra"
	ProviderName = "ch"
	ProviderUID  = "0b7a1c3e-1111-4222-8333-944455556666"
)

// PKI is a test PKI.
type PKI struct {
	// HostCA issues host identity certificates.
	HostCA *hostidentitytest.CA
	// ProviderCA issues Provider identity certificates.
	ProviderCA *hostidentitytest.CA
	// GrantCA is the grant anchor.
	GrantCA *hostidentitytest.CA
	// HostKey is the host key.
	HostKey *ecdsa.PrivateKey
	// HostCertPEM is the host certificate.
	HostCertPEM []byte
	// HostURI is the host's URI identity.
	HostURI string
	// Record is the enrolment record in the host certificate.
	Record hostidentity.Record
	// ProviderSANs is the content of the Provider allow-list.
	ProviderSANs string
}

// New returns a PKI with a host certificate for HostNS/HostName enrolled with
// ProviderUID, and an allow-list naming ProviderNS/ProviderName.
func New(t testing.TB) *PKI {
	t.Helper()
	p := &PKI{
		HostCA:     hostidentitytest.NewCA(t),
		ProviderCA: hostidentitytest.NewCA(t),
		GrantCA:    hostidentitytest.NewCA(t),
		HostKey:    hostidentitytest.NewKey(t),
	}
	var err error
	if p.HostURI, err = hostidentity.HostURI(TrustDomain, HostNS, HostName); err != nil {
		t.Fatal(err)
	}
	p.Record = hostidentity.Record{EnrolmentID: id(0x11), ProviderUID: ProviderUID, InstallID: id(0x22), Mode: hostidentity.ModeIdentity}
	p.HostCertPEM = p.IssueHost(t, p.HostKey, hostidentity.RequestURIs(p.HostURI, p.Record), hostidentitytest.IssueOptions{})
	p.ProviderSANs = "# Providers this host serves\n" + ProviderIdentity().String() + "\n"
	return p
}

func id(b byte) []byte {
	out := make([]byte, hostidentity.IDLen)
	for i := range out {
		out[i] = b + byte(i)
	}
	return out
}

// ProviderIdentity is the allowed Provider.
func ProviderIdentity() trust.ProviderIdentity {
	return trust.ProviderIdentity{TrustDomain: TrustDomain, Namespace: ProviderNS, Name: ProviderName}
}

// ProviderURI is the URI identity of the allowed Provider with uid.
func ProviderURI(uid string) string { return ProviderIdentity().String() + "/" + uid }

// IssueHost issues a host certificate for key with uris from the host
// identity issuer.
func (p *PKI) IssueHost(t testing.TB, key *ecdsa.PrivateKey, uris []string, o hostidentitytest.IssueOptions) []byte {
	t.Helper()
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris})
	return p.HostCA.Issue(t, csr, o)
}

// ProviderCert returns a TLS client certificate with the URI identity uri,
// issued by ca (the Provider identity issuer when nil).
func (p *PKI) ProviderCert(t testing.TB, ca *hostidentitytest.CA, uri string) tls.Certificate {
	t.Helper()
	if ca == nil {
		ca = p.ProviderCA
	}
	key := hostidentitytest.NewKey(t)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: []string{uri}})
	certPEM := ca.Issue(t, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key, Leaf: leaf}
}

// HostCAPool is a pool of the host identity issuer, for a client verifying
// the agent.
func (p *PKI) HostCAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.HostCA.Cert)
	return pool
}

// WriteTrust writes the trust configuration files into dir.
func (p *PKI) WriteTrust(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		trust.FileHostCA:       p.HostCA.PEM,
		trust.FileProviderCA:   p.ProviderCA.PEM,
		trust.FileGrantAnchors: p.GrantCA.PEM,
		trust.FileProviderSANs: []byte(p.ProviderSANs),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil { // #nosec G306 -- test trust configuration
			t.Fatal(err)
		}
	}
}

// WritePKI writes the host certificate into dir, the agent's key material
// directory. The host key is not written there: the agent never holds it
// (WriteKey writes it for the signer).
func (p *PKI) WritePKI(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, trust.FileHostCert), p.HostCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// WriteKey writes the host private key into dir as the signer's key file
// (trust.FileHostKey, mode 0600) and returns its path.
func (p *PKI) WriteKey(t testing.TB, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(p.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, trust.FileHostKey)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
