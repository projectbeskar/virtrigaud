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

package hostidentity

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrIssued is wrapped by every error ValidateIssued returns.
var ErrIssued = errors.New("issued certificate refused")

// Issued is an issued agent certificate that passed ValidateIssued.
type Issued struct {
	// Leaf is the agent certificate.
	Leaf *x509.Certificate
	// KeyPin is the pin of the certificate's key.
	KeyPin string
	// Record is the enrolment record derived from the certificate.
	Record Record
}

// ParseCertificates parses one or more PEM "CERTIFICATE" blocks; anything
// else (another block type, trailing bytes) is refused.
func ParseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		rest = trimSpace(rest)
		if len(rest) == 0 {
			break
		}
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("not a sequence of PEM certificates")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("a certificate does not parse")
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificate")
	}
	return certs, nil
}

// RootPool returns a pool of the CA certificates in caBundlePEM; every
// certificate in it must be a CA.
func RootPool(caBundlePEM []byte) (*x509.CertPool, error) {
	certs, err := ParseCertificates(caBundlePEM)
	if err != nil {
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	for _, c := range certs {
		if !c.IsCA || !c.BasicConstraintsValid {
			return nil, errors.New("CA bundle: a certificate is not a CA")
		}
		pool.AddCert(c)
	}
	return pool, nil
}

// ValidateIssued checks an issued certificate (chainPEM: the leaf first, then
// any intermediates) against the authorisation before its key is pinned: it
// chains to roots for client and server authentication at now; it is not a
// CA; its lifetime is at most MaxLifetime; its subject is empty or the host
// name; its subject alternative names are
// exactly the authorised host URI and record; and its key is the authorised
// key.
func ValidateIssued(auth Authorisation, chainPEM []byte, roots *x509.CertPool, now time.Time) (Issued, error) {
	if roots == nil {
		return Issued{}, fmt.Errorf("%w: no CA bundle", ErrIssued)
	}
	certs, err := ParseCertificates(chainPEM)
	if err != nil {
		return Issued{}, fmt.Errorf("%w: %v", ErrIssued, err)
	}
	leaf := certs[0]
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage},
		}); err != nil {
			return Issued{}, fmt.Errorf("%w: it does not chain to the CA bundle for client and server authentication", ErrIssued)
		}
	}
	if leaf.IsCA {
		return Issued{}, fmt.Errorf("%w: it is a CA certificate", ErrIssued)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return Issued{}, fmt.Errorf("%w: it lacks client or server authentication", ErrIssued)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > MaxLifetime {
		return Issued{}, fmt.Errorf("%w: its lifetime exceeds %s", ErrIssued, MaxLifetime)
	}
	if !subjectAllowed(leaf.Subject.Names, auth.HostName) {
		return Issued{}, fmt.Errorf("%w: its subject is not empty or the host name", ErrIssued)
	}
	if len(leaf.DNSNames) > 0 || len(leaf.IPAddresses) > 0 || len(leaf.EmailAddresses) > 0 {
		return Issued{}, fmt.Errorf("%w: it carries non-URI subject alternative names", ErrIssued)
	}
	rec, err := RecordFromURIs(leaf.URIs, auth.HostURI)
	if err != nil {
		return Issued{}, fmt.Errorf("%w: %v", ErrIssued, err)
	}
	if !rec.Equal(auth.Record) {
		return Issued{}, fmt.Errorf("%w: its enrolment record is not the authorised one", ErrIssued)
	}
	pin, err := SPKIPin(leaf.PublicKey)
	if err != nil {
		return Issued{}, fmt.Errorf("%w: %v", ErrIssued, err)
	}
	if pin != auth.KeyPin {
		return Issued{}, fmt.Errorf("%w: it certifies a key other than the authorised one", ErrIssued)
	}
	return Issued{Leaf: leaf, KeyPin: pin, Record: rec}, nil
}
