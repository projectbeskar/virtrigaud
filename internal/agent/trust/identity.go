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

package trust

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// The files of the key material directory.
const (
	// FileHostKey holds the host's PEM private key (PKCS #8 or SEC 1),
	// ECDSA P-256. It never leaves the host and is read by the signer
	// process only: the agent process neither reads nor holds it, and refuses
	// to start when it can read a file of this name.
	FileHostKey = "host.key"
	// FileHostCert holds the host's PEM identity certificate (the leaf
	// only; its issuer is FileHostCA of the trust configuration).
	FileHostCert = "host.crt"
)

// ErrIdentity is wrapped by every host identity error.
var ErrIdentity = errors.New("host identity refused")

// Identity is the host's identity: its signer, its certificate and the
// enrolment record the certificate carries. The agent process holds no
// private key: Signer forwards to the signer process.
type Identity struct {
	// TLS is the certificate and key the agent serves TLS with.
	TLS tls.Certificate
	// Leaf is the host identity certificate.
	Leaf *x509.Certificate
	// Signer is the host key's signer. It signs TLS handshakes and host
	// answers, nothing else; its private half is in another process.
	Signer crypto.Signer
	// HostURI is the host's URI identity.
	HostURI string
	// HostNamespace and HostName name the Host the identity is for.
	HostNamespace, HostName string
	// Record is the enrolment record the certificate carries.
	Record hostidentity.Record
}

// HostID is the host identity an answer names: the Host's name.
func (id *Identity) HostID() string { return id.HostName }

// LoadIdentity reads the host certificate from pkiDir with read (ReadFile
// when nil) and validates it against cfg at now, with signer as the host key:
// the signer's public key is ECDSA P-256 and the certificate is a single leaf
// for that key, not a CA, with the server authentication usage, that chains
// to a host identity issuer of cfg and is valid at now; its URI subject alternative names are exactly the
// host's URI identity and an enrolment record (hostidentity.RecordFromURIs).
func LoadIdentity(pkiDir string, cfg *Config, now time.Time, read func(string) ([]byte, error), signer crypto.Signer) (*Identity, error) {
	if read == nil {
		read = ReadFile
	}
	if signer == nil {
		return nil, fmt.Errorf("%w: no host key signer", ErrIdentity)
	}
	key, ok := signer.Public().(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the host key is not ECDSA P-256", ErrIdentity)
	}
	certPEM, err := read(filepath.Join(pkiDir, FileHostCert))
	if err != nil {
		return nil, fmt.Errorf("%w: read the host certificate: %v", ErrIdentity, err)
	}
	certs, err := hostidentity.ParseCertificates(certPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: host certificate: %v", ErrIdentity, err)
	}
	if len(certs) != 1 {
		return nil, fmt.Errorf("%w: the host certificate file must hold exactly one certificate, found %d", ErrIdentity, len(certs))
	}
	leaf := certs[0]
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key) {
		return nil, fmt.Errorf("%w: the host certificate does not certify the host key", ErrIdentity)
	}
	if leaf.IsCA {
		return nil, fmt.Errorf("%w: the host certificate is a CA certificate", ErrIdentity)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return nil, fmt.Errorf("%w: the host certificate lacks the server authentication usage", ErrIdentity)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: cfg.HostCAPool, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("%w: the host certificate does not verify against the host identity issuer: %v", ErrIdentity, err)
	}
	hostURI, ns, name, err := hostURIOf(leaf)
	if err != nil {
		return nil, err
	}
	rec, err := hostidentity.RecordFromURIs(leaf.URIs, hostURI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIdentity, err)
	}
	return &Identity{
		TLS:           tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: signer, Leaf: leaf},
		Leaf:          leaf,
		Signer:        signer,
		HostURI:       hostURI,
		HostNamespace: ns,
		HostName:      name,
		Record:        rec,
	}, nil
}

// SameEnrolment reports whether two identities are the same host under the
// same enrolment: a renewed certificate may change the key, never these.
func (id *Identity) SameEnrolment(o *Identity) bool {
	return id.HostURI == o.HostURI && id.Record.Equal(o.Record)
}

// hostURIOf returns the one host URI identity among the certificate's URI
// subject alternative names, and the Host's namespace and name.
func hostURIOf(leaf *x509.Certificate) (uri, namespace, name string, err error) {
	for _, u := range leaf.URIs {
		s := u.String()
		if hostidentity.ValidateHostURI(s) != nil {
			continue
		}
		if uri != "" {
			return "", "", "", fmt.Errorf("%w: the host certificate carries two host identities", ErrIdentity)
		}
		uri = s
		rest := strings.TrimPrefix(u.Path, hostPathPrefix)
		namespace, name, _ = strings.Cut(rest, "/")
	}
	if uri == "" {
		return "", "", "", fmt.Errorf("%w: the host certificate carries no host identity", ErrIdentity)
	}
	return uri, namespace, name, nil
}

// ParseHostKey decodes one PEM ECDSA P-256 private key, PKCS #8 or SEC 1. It
// is for the signer process, the only process that holds the host key.
func ParseHostKey(b []byte) (*ecdsa.PrivateKey, error) { return parseKey(b) }

// parseKey decodes one PEM ECDSA P-256 private key, PKCS #8 or SEC 1.
func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(b)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("%w: the host key file must hold exactly one PEM block", ErrIdentity)
	}
	var k any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		k, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		k, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("%w: the host key is not a private key", ErrIdentity)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: the host key does not decode", ErrIdentity)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the host key is not ECDSA P-256", ErrIdentity)
	}
	return ek, nil
}
