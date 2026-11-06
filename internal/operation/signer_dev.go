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

package operation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

// SignerCompiled reports whether a signer is compiled into this build: true,
// because it was built with the virtrigaud_dev tag.
const SignerCompiled = true

// PEM block types of a development signer key file.
const (
	// DevSignerKeyPEMType is the PKCS#8 ECDSA P-256 anchor key.
	DevSignerKeyPEMType = "PRIVATE KEY"
	// DevSignerInstallIDPEMType holds the 16-byte install_id.
	DevSignerInstallIDPEMType = "VIRTRIGAUD DEV INSTALL ID"
)

// Development signer certificate lifetimes.
const (
	// devAnchorLifetime is the validity of the self-signed anchor certificate.
	devAnchorLifetime = 10 * 365 * 24 * time.Hour
	// devIssuerLifetime is the validity of the path length 0 issuer.
	devIssuerLifetime = 24 * time.Hour
	// devLeafLifetime is the validity of the grant-signing leaf.
	devLeafLifetime = time.Hour
	// devBackdate is how far before now each certificate becomes valid, to
	// absorb clock skew.
	devBackdate = time.Minute
	// devAuthorityGeneration is the stand-in authority generation.
	devAuthorityGeneration uint64 = 1
)

// DevSigner is the development-only Signer: a STAND-IN for real custody of
// the grant-signing key, compiled in only under the virtrigaud_dev build tag.
//
// It holds an ECDSA P-256 anchor key loaded from a key file (exportable by
// construction, unlike a key held by a KMS or HSM), a self-signed anchor
// certificate made from it, and in-process ECDSA P-256 keys for a path length
// 0 issuer and a short-lived grant-signing leaf, which it re-mints before they
// expire. install_id comes from the key file; authority_generation is 1. Its
// anchor's subject and key are stable for a key file, so a verifier that
// pinned Anchor once keeps verifying after a restart.
type DevSigner struct {
	installID  []byte
	anchorKey  *ecdsa.PrivateKey
	anchorCert *x509.Certificate
	now        func() time.Time

	mu         sync.Mutex
	issuerKey  *ecdsa.PrivateKey
	issuerCert *x509.Certificate
	leafKey    *ecdsa.PrivateKey
	leafCert   *x509.Certificate
}

// Compile-time assertion that DevSigner is a Signer.
var _ Signer = (*DevSigner)(nil)

// NewDevSigner returns a development signer for anchorKey (ECDSA P-256) and
// installID (16 bytes). now is its clock; nil means time.Now.
func NewDevSigner(anchorKey *ecdsa.PrivateKey, installID []byte, now func() time.Time) (*DevSigner, error) {
	if anchorKey == nil || anchorKey.Curve != elliptic.P256() {
		return nil, errors.New("new development signer: the anchor key is not an ECDSA P-256 key")
	}
	if len(installID) != IDLen {
		return nil, fmt.Errorf("new development signer: install id is %d bytes, not %d", len(installID), IDLen)
	}
	if now == nil {
		now = time.Now
	}
	s := &DevSigner{installID: append([]byte(nil), installID...), anchorKey: anchorKey, now: now}
	t := now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               devSubject("anchor", anchorKey),
		NotBefore:             t.Add(-devBackdate),
		NotAfter:              t.Add(devAnchorLifetime),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	cert, err := createCert(tmpl, tmpl, &anchorKey.PublicKey, anchorKey)
	if err != nil {
		return nil, fmt.Errorf("new development signer: anchor certificate: %w", err)
	}
	s.anchorCert = cert
	return s, nil
}

// LoadDevSignerKeyFile reads a development signer key file (EncodeDevSignerKey)
// and returns its signer. The file must hold exactly one DevSignerKeyPEMType
// block (PKCS#8 ECDSA P-256) and one DevSignerInstallIDPEMType block (16
// bytes), and nothing else.
func LoadDevSignerKeyFile(path string) (*DevSigner, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- an administrator-chosen development key file
	if err != nil {
		return nil, fmt.Errorf("read development signer key file: %w", err)
	}
	key, installID, err := decodeDevSignerKey(data)
	if err != nil {
		return nil, fmt.Errorf("development signer key file %s: %w", path, err)
	}
	return NewDevSigner(key, installID, nil)
}

// LoadSigner loads the development signer from the key file at path. With an
// empty path it returns no signer and no error.
func LoadSigner(path string) (Signer, error) {
	if path == "" {
		return nil, nil
	}
	s, err := LoadDevSignerKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", DevSignerKeyFileFlagName, err)
	}
	return s, nil
}

// EncodeDevSignerKey renders key and installID as a development signer key
// file (PEM). The output holds a private key; write it with mode 0600.
func EncodeDevSignerKey(key *ecdsa.PrivateKey, installID []byte) ([]byte, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("encode development signer key: not an ECDSA P-256 key")
	}
	if len(installID) != IDLen {
		return nil, fmt.Errorf("encode development signer key: install id is %d bytes, not %d", len(installID), IDLen)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode development signer key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: DevSignerKeyPEMType, Bytes: der})
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: DevSignerInstallIDPEMType, Bytes: installID})...)
	return out, nil
}

// GenerateDevSignerKey draws a new anchor key and install id and renders them
// as a development signer key file (EncodeDevSignerKey).
func GenerateDevSignerKey() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate development signer key: %w", err)
	}
	installID, err := NewRandomID()
	if err != nil {
		return nil, fmt.Errorf("generate development signer key: %w", err)
	}
	return EncodeDevSignerKey(key, installID)
}

// decodeDevSignerKey parses a development signer key file. Errors never quote
// the file's content.
func decodeDevSignerKey(data []byte) (*ecdsa.PrivateKey, []byte, error) {
	var key *ecdsa.PrivateKey
	var installID []byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case DevSignerKeyPEMType:
			if key != nil {
				return nil, nil, errors.New("more than one private key block")
			}
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, nil, errors.New("the private key block is not a PKCS#8 key")
			}
			k, ok := parsed.(*ecdsa.PrivateKey)
			if !ok || k.Curve != elliptic.P256() {
				return nil, nil, errors.New("the private key is not an ECDSA P-256 key")
			}
			key = k
		case DevSignerInstallIDPEMType:
			if installID != nil {
				return nil, nil, errors.New("more than one install id block")
			}
			if len(block.Bytes) != IDLen {
				return nil, nil, fmt.Errorf("the install id is %d bytes, not %d", len(block.Bytes), IDLen)
			}
			installID = append([]byte(nil), block.Bytes...)
		default:
			return nil, nil, fmt.Errorf("unexpected PEM block type %q", block.Type)
		}
	}
	if len(bytesTrimSpace(rest)) > 0 {
		return nil, nil, errors.New("trailing data that is not a PEM block")
	}
	if key == nil || installID == nil {
		return nil, nil, errors.New("a private key block and an install id block are both required")
	}
	return key, installID, nil
}

// bytesTrimSpace drops ASCII whitespace from both ends of b.
func bytesTrimSpace(b []byte) []byte {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

// Anchor is the signer's self-signed trust anchor, for a verifier to pin.
func (s *DevSigner) Anchor() *x509.Certificate { return s.anchorCert }

// InstallID implements Signer.
func (s *DevSigner) InstallID() []byte { return append([]byte(nil), s.installID...) }

// AuthorityGeneration implements Signer: always 1 (a stand-in).
func (s *DevSigner) AuthorityGeneration() uint64 { return devAuthorityGeneration }

// Sign implements Signer: ECDSA P-256 over SHA-256 of body (ASN.1 DER), with
// the chain leaf, issuer. It re-mints the leaf (and the issuer) when the
// current one would expire before notAfter.
func (s *DevSigner) Sign(ctx context.Context, body []byte, notAfter time.Time) (Signature, error) {
	if err := ctx.Err(); err != nil {
		return Signature{}, fmt.Errorf("development signer: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if notAfter.After(now.Add(MaxGrantLifetime)) {
		return Signature{}, fmt.Errorf("development signer: not_after is more than %s ahead", MaxGrantLifetime)
	}
	if err := s.ensureChain(now, notAfter); err != nil {
		return Signature{}, err
	}
	digest := sha256.Sum256(body)
	sig, err := ecdsa.SignASN1(rand.Reader, s.leafKey, digest[:])
	if err != nil {
		return Signature{}, fmt.Errorf("development signer: sign: %w", err)
	}
	return Signature{Signature: sig, Chain: [][]byte{s.leafCert.Raw, s.issuerCert.Raw}}, nil
}

// ensureChain re-mints the issuer and the leaf when they do not cover
// [now, notAfter]. s.mu is held.
func (s *DevSigner) ensureChain(now, notAfter time.Time) error {
	// The issuer covers every leaf minted now (valid until now+devLeafLifetime,
	// which is at least notAfter: Sign bounds it by MaxGrantLifetime).
	if s.issuerCert == nil || !covers(s.issuerCert, now, now.Add(devLeafLifetime)) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return fmt.Errorf("development signer: issuer key: %w", err)
		}
		serial, err := newSerial()
		if err != nil {
			return err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               devSubject("issuer", key),
			NotBefore:             now.Add(-devBackdate),
			NotAfter:              now.Add(devIssuerLifetime),
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
			MaxPathLen:            0,
			MaxPathLenZero:        true,
		}
		cert, err := createCert(tmpl, s.anchorCert, &key.PublicKey, s.anchorKey)
		if err != nil {
			return fmt.Errorf("development signer: issuer certificate: %w", err)
		}
		s.issuerKey, s.issuerCert = key, cert
		s.leafCert = nil
	}
	if s.leafCert == nil || !covers(s.leafCert, now, notAfter) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return fmt.Errorf("development signer: leaf key: %w", err)
		}
		serial, err := newSerial()
		if err != nil {
			return err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               devSubject("grant-signer", key),
			NotBefore:             now.Add(-devBackdate),
			NotAfter:              now.Add(devLeafLifetime),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
		}
		cert, err := createCert(tmpl, s.issuerCert, &key.PublicKey, s.issuerKey)
		if err != nil {
			return fmt.Errorf("development signer: leaf certificate: %w", err)
		}
		s.leafKey, s.leafCert = key, cert
	}
	return nil
}

// covers reports whether c is valid over [from, to].
func covers(c *x509.Certificate, from, to time.Time) bool {
	return !from.Before(c.NotBefore) && !to.After(c.NotAfter)
}

// createCert signs tmpl with parent's key and parses the result.
func createCert(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

// newSerial draws a random 128-bit certificate serial number.
func newSerial() (*big.Int, error) {
	b, err := randomBytes(IDLen)
	if err != nil {
		return nil, fmt.Errorf("development signer: serial: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}

// devSubject names a development certificate by its role and a short,
// public fingerprint of its key, so the anchor's subject is stable for a key.
func devSubject(role string, key *ecdsa.PrivateKey) pkix.Name {
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	fp := "unknown"
	if err == nil {
		sum := sha256.Sum256(pub)
		fp = fmt.Sprintf("%x", sum[:8])
	}
	return pkix.Name{
		Organization: []string{"VirtRigaud development operation grants"},
		CommonName:   "virtrigaud-dev-" + role + "-" + fp,
	}
}
