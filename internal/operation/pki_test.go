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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testEpoch is the fixed verifier and signer clock of these tests.
var testEpoch = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// testPKI is a test-only signer: an anchor, a path length 0 issuer and a
// leaf, each knob adjustable so the chain rules can be exercised. It exists
// only in test binaries; release builds have no signer.
type testPKI struct {
	anchorKey  *ecdsa.PrivateKey
	anchor     *x509.Certificate
	issuerKey  *ecdsa.PrivateKey
	issuer     *x509.Certificate
	leafKey    crypto.Signer
	leaf       *x509.Certificate
	installID  []byte
	generation uint64
	// chain overrides the chain Sign returns when set.
	chain [][]byte
}

// certOpts adjusts one certificate of a testPKI.
type certOpts struct {
	curve          elliptic.Curve
	notBefore      time.Time
	notAfter       time.Time
	isCA           bool
	maxPathLen     int
	maxPathLenZero bool
	keyUsage       x509.KeyUsage
	extKeyUsage    []x509.ExtKeyUsage
	unknownEKU     []asn1.ObjectIdentifier
	dnsNames       []string
}

// mustKey draws an ECDSA key on curve.
func mustKey(t testing.TB, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return k
}

// mustCert signs a certificate for pub with parent / parentKey.
func mustCert(t testing.TB, cn string, o certOpts, pub any, parent *x509.Certificate, parentKey any) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             o.notBefore,
		NotAfter:              o.notAfter,
		KeyUsage:              o.keyUsage,
		BasicConstraintsValid: true,
		IsCA:                  o.isCA,
		MaxPathLen:            o.maxPathLen,
		MaxPathLenZero:        o.maxPathLenZero,
		ExtKeyUsage:           o.extKeyUsage,
		UnknownExtKeyUsage:    o.unknownEKU,
		DNSNames:              o.dnsNames,
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

// defaultCA / defaultIssuer / defaultLeaf are the valid shapes.
func defaultCA() certOpts {
	return certOpts{curve: elliptic.P256(), notBefore: testEpoch.Add(-time.Hour), notAfter: testEpoch.Add(24 * time.Hour),
		isCA: true, maxPathLen: 1, keyUsage: x509.KeyUsageCertSign}
}

func defaultIssuer() certOpts {
	o := defaultCA()
	o.maxPathLen, o.maxPathLenZero = 0, true
	return o
}

func defaultLeaf() certOpts {
	return certOpts{curve: elliptic.P256(), notBefore: testEpoch.Add(-time.Hour), notAfter: testEpoch.Add(time.Hour),
		keyUsage: x509.KeyUsageDigitalSignature}
}

// newTestPKI builds a valid anchor, issuer and leaf, then applies the
// optional adjustments.
func newTestPKI(t testing.TB, issuer, leaf certOpts) *testPKI {
	t.Helper()
	p := &testPKI{installID: bytesOf(0xA1, IDLen), generation: 1}
	p.anchorKey = mustKey(t, elliptic.P256())
	p.anchor = mustCert(t, "test-anchor", defaultCA(), &p.anchorKey.PublicKey, nil, p.anchorKey)
	p.issuerKey = mustKey(t, issuer.curve)
	p.issuer = mustCert(t, "test-issuer", issuer, &p.issuerKey.PublicKey, p.anchor, p.anchorKey)
	lk := mustKey(t, leaf.curve)
	p.leafKey = lk
	p.leaf = mustCert(t, "test-leaf", leaf, &lk.PublicKey, p.issuer, p.issuerKey)
	return p
}

// validPKI is newTestPKI with the valid shapes.
func validPKI(t testing.TB) *testPKI { return newTestPKI(t, defaultIssuer(), defaultLeaf()) }

// InstallID implements Signer.
func (p *testPKI) InstallID() []byte { return p.installID }

// AuthorityGeneration implements Signer.
func (p *testPKI) AuthorityGeneration() uint64 { return p.generation }

// Sign implements Signer.
func (p *testPKI) Sign(_ context.Context, body []byte, _ time.Time) (Signature, error) {
	digest := sha256.Sum256(body)
	sig, err := p.leafKey.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return Signature{}, err
	}
	if pub, ok := p.leafKey.Public().(*ecdsa.PublicKey); ok {
		if sig, err = normalizeLowS(sig, pub.Curve); err != nil {
			return Signature{}, err
		}
	}
	chain := p.chain
	if chain == nil {
		chain = [][]byte{p.leaf.Raw, p.issuer.Raw}
	}
	return Signature{Signature: sig, Chain: chain}, nil
}

// bytesOf returns n bytes of value b.
func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// mustAs asserts v's type, failing the test otherwise.
func mustAs[T any](t testing.TB, v any) T {
	t.Helper()
	out, ok := v.(T)
	if !ok {
		t.Fatalf("got %T, want %T", v, out)
	}
	return out
}
