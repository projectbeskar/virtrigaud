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
	"bytes"
	"crypto/x509"
	"errors"
)

// ErrHierarchiesShared is returned by CheckHierarchySeparation.
var ErrHierarchiesShared = errors.New("the host-agent CA and the operation-grant anchors share a certificate or an issuer")

// CheckHierarchySeparation checks that the host-agent CA hierarchy (whose
// leaves sign host answers) and the operation-grant anchors (whose leaves
// sign grants) share no certificate, no key and no ancestor: no certificate
// of one set equals, has the key of, or chains to a certificate of the
// other. Whoever holds the host-agent CA key could otherwise issue a leaf
// that verifies as a grant signer, or the converse. The manager refuses to
// start when it fails.
func CheckHierarchySeparation(hostAnchors, grantAnchors []*x509.Certificate) error {
	if crosses(hostAnchors, grantAnchors) || crosses(grantAnchors, hostAnchors) {
		return ErrHierarchiesShared
	}
	return nil
}

// crosses reports whether a certificate of from equals, shares its key with,
// or chains to a certificate of to.
func crosses(from, to []*x509.Certificate) bool {
	roots := x509.NewCertPool()
	for _, c := range to {
		roots.AddCert(c)
	}
	for _, f := range from {
		for _, c := range to {
			if f.Equal(c) || bytes.Equal(f.RawSubjectPublicKeyInfo, c.RawSubjectPublicKeyInfo) {
				return true
			}
		}
		if _, err := f.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: f.NotBefore,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
			return true
		}
	}
	return false
}
