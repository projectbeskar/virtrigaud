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

// Package hostidentitytest builds test subordinate CAs, agent keys,
// certificate signing requests and issued certificates for the enrolment
// tests. It is for tests only.
package hostidentitytest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/url"
	"time"
)

// T is the part of testing.TB the helpers use (GinkgoT satisfies it too).
type T interface {
	Helper()
	Fatalf(format string, args ...any)
}

// CA is a test subordinate CA.
type CA struct {
	// Cert is the CA certificate.
	Cert *x509.Certificate
	// Key is the CA private key.
	Key crypto.Signer
	// PEM is the CA certificate in PEM.
	PEM []byte
}

// NewCA returns a self-signed test CA.
func NewCA(t T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test host-agent sub-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return &CA{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// IssueOptions shapes an issued certificate.
type IssueOptions struct {
	// Lifetime defaults to 30 days.
	Lifetime time.Duration
	// IsCA sets the CA bit.
	IsCA bool
	// ExtKeyUsage defaults to client and server authentication.
	ExtKeyUsage []x509.ExtKeyUsage
	// URIs override the CSR's URIs when non-nil.
	URIs []string
	// PublicKey overrides the CSR's key when non-nil.
	PublicKey crypto.PublicKey
	// Subject overrides the CSR's subject when non-nil.
	Subject *pkix.Name
	// KeyUsage defaults to digital signature.
	KeyUsage x509.KeyUsage
}

// Issue signs csrPEM as the CA would and returns the certificate in PEM.
func (ca *CA) Issue(t T, csrPEM []byte, o IssueOptions) []byte {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatalf("decode CSR PEM")
		return nil
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if o.Lifetime == 0 {
		o.Lifetime = 30 * 24 * time.Hour
	}
	if o.ExtKeyUsage == nil {
		o.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	uris := csr.URIs
	if o.URIs != nil {
		uris = ParseURIs(t, o.URIs)
	}
	pub := csr.PublicKey
	if o.PublicKey != nil {
		pub = o.PublicKey
	}
	subject := csr.Subject
	if o.Subject != nil {
		subject = *o.Subject
	}
	if o.KeyUsage == 0 {
		o.KeyUsage = x509.KeyUsageDigitalSignature
	}
	now := time.Now().Add(-time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               subject,
		NotBefore:             now,
		NotAfter:              now.Add(o.Lifetime),
		URIs:                  uris,
		KeyUsage:              o.KeyUsage,
		ExtKeyUsage:           o.ExtKeyUsage,
		BasicConstraintsValid: true,
		IsCA:                  o.IsCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		t.Fatalf("issue certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// NewKey returns an ECDSA P-256 agent key.
func NewKey(t T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// CSROptions shapes a certificate signing request.
type CSROptions struct {
	// CommonName is the subject common name (empty: none).
	CommonName string
	// URIs are the URI subject alternative names.
	URIs []string
	// DNSNames are DNS subject alternative names.
	DNSNames []string
	// RequestCA adds a basic constraints extension with the CA bit set.
	RequestCA bool
	// BreakSignature flips a bit of the signature, so proof of possession
	// fails.
	BreakSignature bool
}

// NewCSR returns a PEM certificate signing request signed by key.
func NewCSR(t T, key crypto.Signer, o CSROptions) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: o.CommonName},
		URIs:     ParseURIs(t, o.URIs),
		DNSNames: o.DNSNames,
	}
	if o.RequestCA {
		val, err := asn1.Marshal(struct {
			IsCA bool `asn1:"optional"`
		}{IsCA: true})
		if err != nil {
			t.Fatalf("marshal basic constraints: %v", err)
		}
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{
			Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: val,
		})
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	if o.BreakSignature {
		der[len(der)-3] ^= 0x01
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// ParseURIs parses each string as a URI.
func ParseURIs(t T, ss []string) []*url.URL {
	t.Helper()
	out := make([]*url.URL, 0, len(ss))
	for _, s := range ss {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("parse URI %q: %v", s, err)
		}
		out = append(out, u)
	}
	return out
}
