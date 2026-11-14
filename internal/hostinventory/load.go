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

package hostinventory

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

// Bounds of the signature inputs.
const (
	// MaxSignatureBytes bounds a signature file.
	MaxSignatureBytes = 1024
	// MaxSignersBytes bounds a signers file.
	MaxSignersBytes = 64 << 10
	// MaxSigners bounds the signer keys.
	MaxSigners = 16
)

// pemPublicKey is the PEM block type of a signer key.
const pemPublicKey = "PUBLIC KEY"

// Signer is one key that may sign an allow-list: an ECDSA P-256 key (the
// signature is an ASN.1 DER signature over the SHA-256 of the file) or an
// Ed25519 key (the signature is over the file itself).
type Signer struct {
	// Fingerprint is the SHA-256 of the key's DER SubjectPublicKeyInfo, as
	// 64 lowercase hexadecimal characters.
	Fingerprint string
	key         crypto.PublicKey
}

// ParseSigners parses a PEM file of one or more PUBLIC KEY blocks (PKIX
// SubjectPublicKeyInfo) of ECDSA P-256 or Ed25519 keys: the configured set
// of allow-list signers. Anything else in the file, an empty set or a
// repeated key is an error.
func ParseSigners(data []byte) ([]Signer, error) {
	if len(data) > MaxSignersBytes {
		return nil, fmt.Errorf("%w: the signers file is larger than %d bytes", ErrInvalid, MaxSignersBytes)
	}
	var out []Signer
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemPublicKey {
			return nil, fmt.Errorf("%w: a signers file block of type %q", ErrInvalid, block.Type)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: a signer key does not parse", ErrInvalid)
		}
		switch k := key.(type) {
		case *ecdsa.PublicKey:
			if k.Curve != elliptic.P256() {
				return nil, fmt.Errorf("%w: a signer key is ECDSA but not P-256", ErrInvalid)
			}
		case ed25519.PublicKey:
		default:
			return nil, fmt.Errorf("%w: a signer key is neither ECDSA P-256 nor Ed25519", ErrInvalid)
		}
		fp := sha256.Sum256(block.Bytes)
		s := Signer{Fingerprint: hex.EncodeToString(fp[:]), key: key}
		for _, o := range out {
			if o.Fingerprint == s.Fingerprint {
				return nil, fmt.Errorf("%w: a signer key is repeated", ErrInvalid)
			}
		}
		out = append(out, s)
		if len(out) > MaxSigners {
			return nil, fmt.Errorf("%w: more than %d signer keys", ErrInvalid, MaxSigners)
		}
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("%w: the signers file holds something other than PEM public keys", ErrInvalid)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the signers file holds no key", ErrInvalid)
	}
	return out, nil
}

// LoadOptions is what Load checks an allow-list file against.
type LoadOptions struct {
	// Data is the file's content, read once.
	Data []byte
	// PinnedSHA256 is the expected SHA-256 of Data, as 64 lowercase
	// hexadecimal characters (the manager flag). Required.
	PinnedSHA256 string
	// Signers are the keys one of which must have signed Data. Empty: no
	// signature is required.
	Signers []Signer
	// Signature is the detached signature of Data: the base64 encoding of
	// the signature bytes (surrounding white space ignored). Required when
	// Signers is not empty.
	Signature []byte
}

// LoadResult is a loaded allow-list and how it was verified.
type LoadResult struct {
	// AllowList is the parsed allow-list.
	AllowList *AllowList
	// SignedBy is the fingerprint of the signer whose signature verified;
	// empty when no signature was required.
	SignedBy string
}

// Load checks an allow-list file and parses it, in this order: a digest is
// pinned and is the SHA-256 of the data (ErrDigestMismatch); when signers are
// configured, the detached signature verifies under one of them
// (ErrUnverified); the data parses (ErrInvalid). It does no network access:
// the signature is checked against the configured keys only.
func Load(o LoadOptions) (LoadResult, error) {
	got := sha256.Sum256(o.Data)
	pinned, err := hex.DecodeString(o.PinnedSHA256)
	if err != nil || len(pinned) != sha256.Size || strings.ToLower(o.PinnedSHA256) != o.PinnedSHA256 {
		return LoadResult{}, fmt.Errorf("%w: no valid pinned digest (the file's is %x)", ErrDigestMismatch, got)
	}
	if subtle.ConstantTimeCompare(pinned, got[:]) != 1 {
		return LoadResult{}, fmt.Errorf("%w: pinned %s, the file's is %x", ErrDigestMismatch, o.PinnedSHA256, got)
	}
	var signedBy string
	if len(o.Signers) > 0 {
		if signedBy, err = verifySignature(o.Data, o.Signature, o.Signers); err != nil {
			return LoadResult{}, err
		}
	}
	a, err := Parse(o.Data)
	if err != nil {
		return LoadResult{}, err
	}
	return LoadResult{AllowList: a, SignedBy: signedBy}, nil
}

// verifySignature returns the fingerprint of the signer under which sig (base64)
// verifies over data.
func verifySignature(data, sig []byte, signers []Signer) (string, error) {
	if len(sig) == 0 || len(sig) > MaxSignatureBytes {
		return "", fmt.Errorf("%w: no signature, or one larger than %d bytes", ErrUnverified, MaxSignatureBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return "", fmt.Errorf("%w: the signature is not base64", ErrUnverified)
	}
	digest := sha256.Sum256(data)
	for _, s := range signers {
		switch k := s.key.(type) {
		case *ecdsa.PublicKey:
			if ecdsa.VerifyASN1(k, digest[:], raw) {
				return s.Fingerprint, nil
			}
		case ed25519.PublicKey:
			if len(raw) == ed25519.SignatureSize && ed25519.Verify(k, data, raw) {
				return s.Fingerprint, nil
			}
		}
	}
	return "", fmt.Errorf("%w: no configured signer made it", ErrUnverified)
}
