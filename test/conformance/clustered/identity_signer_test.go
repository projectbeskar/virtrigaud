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

package clustered_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// testSigner is a test-only operation.Signer: an ECDSA P-256 anchor, a path
// length 0 issuer and a grant-signing leaf, made in memory for one test. It
// exists only in this test binary; release builds have no signer, and the
// development signer is compiled only under the virtrigaud_dev build tag.
type testSigner struct {
	anchor    *x509.Certificate
	issuer    *x509.Certificate
	leaf      *x509.Certificate
	leafKey   *ecdsa.PrivateKey
	installID []byte
}

var _ operation.Signer = (*testSigner)(nil)

// newTestSigner makes a fresh chain, valid from a minute ago for two hours.
func newTestSigner() (*testSigner, error) {
	now := time.Now()
	from, to := now.Add(-time.Minute), now.Add(2*time.Hour)
	anchorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	anchor, err := makeCert(&x509.Certificate{
		Subject: pkix.Name{CommonName: "conformance-anchor"}, NotBefore: from, NotAfter: to,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 1,
	}, nil, &anchorKey.PublicKey, anchorKey)
	if err != nil {
		return nil, err
	}
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	issuer, err := makeCert(&x509.Certificate{
		Subject: pkix.Name{CommonName: "conformance-issuer"}, NotBefore: from, NotAfter: to,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, MaxPathLenZero: true,
	}, anchor, &issuerKey.PublicKey, anchorKey)
	if err != nil {
		return nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leaf, err := makeCert(&x509.Certificate{
		Subject: pkix.Name{CommonName: "conformance-grant-signer"}, NotBefore: from, NotAfter: to,
		KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}, issuer, &leafKey.PublicKey, issuerKey)
	if err != nil {
		return nil, err
	}
	installID, err := operation.NewRandomID()
	if err != nil {
		return nil, err
	}
	return &testSigner{anchor: anchor, issuer: issuer, leaf: leaf, leafKey: leafKey, installID: installID}, nil
}

// makeCert signs tmpl with parentKey (self-signed when parent is nil).
func makeCert(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, parentKey *ecdsa.PrivateKey) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl.SerialNumber = serial
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", tmpl.Subject.CommonName, err)
	}
	return x509.ParseCertificate(der)
}

// InstallID implements operation.Signer.
func (s *testSigner) InstallID() []byte { return append([]byte(nil), s.installID...) }

// AuthorityGeneration implements operation.Signer.
func (s *testSigner) AuthorityGeneration() uint64 { return 1 }

// Sign implements operation.Signer: an ECDSA P-256 signature over the body's
// SHA-256 digest, in the low-S form verifiers accept.
func (s *testSigner) Sign(_ context.Context, body []byte, notAfter time.Time) (operation.Signature, error) {
	if notAfter.After(s.leaf.NotAfter) {
		return operation.Signature{}, errors.New("test signer: not_after is past the leaf's validity")
	}
	digest := sha256.Sum256(body)
	r, sv, err := ecdsa.Sign(rand.Reader, s.leafKey, digest[:])
	if err != nil {
		return operation.Signature{}, err
	}
	n := elliptic.P256().Params().N
	if sv.Cmp(new(big.Int).Rsh(n, 1)) > 0 {
		sv = new(big.Int).Sub(n, sv)
	}
	sig, err := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
	if err != nil {
		return operation.Signature{}, err
	}
	return operation.Signature{Signature: sig, Chain: [][]byte{s.leaf.Raw, s.issuer.Raw}}, nil
}

// identityFake returns a factory of fakes in operation-identity mode, each
// with a fresh test signer, and opts.
func identityFake(required bool, opts ...fake.Option) clustered.Factory {
	return func(t clustered.T) clustered.Target {
		t.Helper()
		s, err := newTestSigner()
		if err != nil {
			t.Errorf("test signer: %v", err)
			t.FailNow()
		}
		tg, err := fake.NewIdentity(fake.IdentityConfig{Anchor: s.anchor, Signer: s, Required: required}, opts...)
		if err != nil {
			t.Errorf("identity fake: %v", err)
			t.FailNow()
		}
		return tg
	}
}
