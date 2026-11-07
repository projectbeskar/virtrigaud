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
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// IntentAuthContext is the domain-separation prefix of every message an
// IntentAuthenticator authenticates: it is the first bytes of the HMAC input
// and the HKDF info the key is derived under, so an intent tag is bound to
// this purpose and version only.
const IntentAuthContext = "virtrigaud.io/operation-intent/v1\x00"

// IntentTagLen is the length of an intent tag (HMAC-SHA256).
const IntentTagLen = sha256.Size

// minIntentKeyLen is the shortest key NewIntentAuthenticator accepts.
const minIntentKeyLen = 32

// ErrNoIntentAuthenticator is returned when operation intents would have to be
// authenticated and no authenticator is configured: nothing is stored, loaded
// or sent.
var ErrNoIntentAuthenticator = errors.New("no operation-intent authenticator is configured")

// IntentAuthenticator authenticates the stored intent of an operation attempt
// (its exact request bytes, salt and identity) so that a manager never signs
// a grant over stored bytes it did not write itself. It is a MAC under a key
// that never leaves the manager process.
//
// Why a MAC and not the grant signer: operation grants are signed over the
// GrantBody bytes as they are (SHA-256 of the body, no domain-separation
// prefix), by a short-lived in-process leaf key that a restart replaces. A
// signature by the same key over an intent would not be domain-separated
// from a grant signature, and could not be verified after the restart it must
// survive. The MAC key is instead derived with HKDF-SHA256 from the signer's
// long-lived key material under its own label (IntentAuthContext), so it is
// independent of every grant-signing key; a tag is 32 bytes of HMAC-SHA256,
// never a DER ECDSA signature, and a grant signature (DER, 70-72 bytes) is
// never IntentTagLen bytes. A real key custody will supply its own
// IntentAuthenticator (for example a KMS MAC key).
type IntentAuthenticator interface {
	// SealIntent returns the tag of msg (IntentMessage).
	SealIntent(msg []byte) []byte
	// OpenIntent reports whether tag is msg's tag, in constant time.
	OpenIntent(msg, tag []byte) bool
}

// IntentAuthenticatorSource is a Signer that can authenticate operation
// intents (the development signer can).
type IntentAuthenticatorSource interface {
	IntentAuthenticator() IntentAuthenticator
}

// IntentAuthenticatorOf returns s's IntentAuthenticator, or nil when s is nil
// or cannot authenticate intents.
func IntentAuthenticatorOf(s Signer) IntentAuthenticator {
	if src, ok := s.(IntentAuthenticatorSource); ok && !isNilSigner(s) {
		return src.IntentAuthenticator()
	}
	return nil
}

// hmacIntentAuthenticator is an IntentAuthenticator with an HMAC-SHA256 key.
type hmacIntentAuthenticator struct {
	key []byte
}

// NewIntentAuthenticator returns an HMAC-SHA256 IntentAuthenticator with key
// (at least 32 bytes; copied).
func NewIntentAuthenticator(key []byte) (IntentAuthenticator, error) {
	if len(key) < minIntentKeyLen {
		return nil, fmt.Errorf("operation-intent key is %d bytes, at least %d needed", len(key), minIntentKeyLen)
	}
	return &hmacIntentAuthenticator{key: append([]byte(nil), key...)}, nil
}

// DeriveIntentKey derives the 32-byte operation-intent MAC key from secret
// (long-lived key material of the signer) with HKDF-SHA256 and the info
// IntentAuthContext.
func DeriveIntentKey(secret []byte) ([]byte, error) {
	if len(secret) < minIntentKeyLen {
		return nil, fmt.Errorf("operation-intent key material is %d bytes, at least %d needed", len(secret), minIntentKeyLen)
	}
	key, err := hkdf.Key(sha256.New, secret, nil, IntentAuthContext, IntentTagLen)
	if err != nil {
		return nil, fmt.Errorf("derive the operation-intent key: %w", err)
	}
	return key, nil
}

// SealIntent implements IntentAuthenticator.
func (a *hmacIntentAuthenticator) SealIntent(msg []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(msg)
	return m.Sum(nil)
}

// OpenIntent implements IntentAuthenticator.
func (a *hmacIntentAuthenticator) OpenIntent(msg, tag []byte) bool {
	if len(tag) != IntentTagLen {
		return false
	}
	return hmac.Equal(a.SealIntent(msg), tag)
}

// String never renders the key.
func (a *hmacIntentAuthenticator) String() string {
	return "IntentAuthenticator{HMAC-SHA256, key redacted}"
}

// GoString never renders the key.
func (a *hmacIntentAuthenticator) GoString() string { return a.String() }

// IntentMessage is the canonical message an intent tag covers:
// IntentAuthContext followed by each field as a 4-byte big-endian length and
// its bytes, so no two field lists encode alike.
func IntentMessage(fields ...[]byte) []byte {
	n := len(IntentAuthContext)
	for _, f := range fields {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	out = append(out, IntentAuthContext...)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f))) // #nosec G115 -- intents are capped far below 4 GiB
		out = append(out, f...)
	}
	return out
}
