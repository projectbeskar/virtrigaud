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

package grantverify

import (
	"crypto/elliptic"
	"encoding/asn1"
	"errors"
	"math/big"
)

// ECDSASignature is the ASN.1 DER form of an ECDSA signature.
type ECDSASignature struct {
	R, S *big.Int
}

// ParseECDSASignature decodes sig as one DER ECDSA signature with nothing
// after it and both values positive.
func ParseECDSASignature(sig []byte) (ECDSASignature, error) {
	var s ECDSASignature
	rest, err := asn1.Unmarshal(sig, &s)
	if err != nil || len(rest) != 0 || s.R == nil || s.S == nil || s.R.Sign() <= 0 || s.S.Sign() <= 0 {
		return ECDSASignature{}, errors.New("not a DER ECDSA signature")
	}
	return s, nil
}

// IsLowS reports whether sig is a DER ECDSA signature whose s is at most
// half the order of curve. Grant signatures are low-S only, so a signature
// has one accepted encoding: (r, n-s) is refused.
func IsLowS(sig []byte, curve elliptic.Curve) bool {
	s, err := ParseECDSASignature(sig)
	if err != nil {
		return false
	}
	half := new(big.Int).Rsh(curve.Params().N, 1)
	return s.S.Cmp(half) <= 0
}

// NormalizeLowS returns sig with s replaced by n-s when s is above half the
// order of curve, so a signer always emits the low-S form.
func NormalizeLowS(sig []byte, curve elliptic.Curve) ([]byte, error) {
	s, err := ParseECDSASignature(sig)
	if err != nil {
		return nil, err
	}
	n := curve.Params().N
	if s.S.Cmp(new(big.Int).Rsh(n, 1)) <= 0 {
		return sig, nil
	}
	s.S = new(big.Int).Sub(n, s.S)
	out, err := asn1.Marshal(s)
	if err != nil {
		return nil, errors.New("re-encode ECDSA signature")
	}
	return out, nil
}
