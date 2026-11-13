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

// Package signer separates the per-host agent's host private key from the
// process that terminates TLS. The signer process (vrtg-agent signer) loads
// the key and serves a Unix socket; the agent process holds a Client, a
// crypto.Signer whose Public is the key's public half and whose Sign forwards
// a digest to the signer. The agent never holds the private key bytes, so a
// compromise of the TLS-facing process cannot copy the key out and use it
// elsewhere or later.
//
// # What this does not protect against
//
// The signer is a digest oracle. crypto/tls hands Sign a digest, not the
// message, so the signer cannot tell a TLS 1.3 handshake transcript from a
// host answer or from anything else. A compromised agent process can
// therefore still obtain a signature over any digest, online, for as long as
// it can reach the signer. The separation prevents exfiltration of the key
// and offline use of it after the agent process is gone; it does not prevent
// online forgery of host answers by the agent process. A separate answer key,
// held by a process that never terminates TLS, so that the signer sees the
// whole message it signs, is a later requirement for the regulated profile;
// until it exists, host answers must not be treated as stronger than the
// agent process that relays them.
//
// # Protocol
//
// A connection carries one request and one response, each a frame: a 2-byte
// big-endian length N (at most MaxFrameBytes) and N payload bytes. A request
// is the protocol version byte, an operation byte and the operation's body;
// a response is the protocol version byte, a status byte and the body.
//
//	OpPublicKey   no body                            -> the PKIX DER public key
//	OpSign        hash byte, purpose byte, digest    -> the ASN.1 DER ECDSA signature
//
// The only hash is SHA-256 (what crypto/tls signs with an ECDSA P-256 key in
// TLS 1.3 and what host answers use) and the digest must be exactly 32 bytes.
// The purpose says which budget the signature is charged to: PurposeHandshake
// for the TLS handshakes, which unauthenticated peers can trigger, and
// PurposeAnswer for signed host answers, which only an authenticated Provider
// can. The two have separate rate limits, so a flood of handshakes cannot
// starve the answers. The signer takes the purpose on trust, since it cannot
// see what is signed: a compromised agent can lie about it, so this protects
// against outside floods only. Anything else is refused. Both ends check the peer's uid with SO_PEERCRED
// on every connection.
//
// It imports no Kubernetes package.
package signer

import (
	"crypto"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Wire constants.
const (
	// Version is the protocol version byte.
	Version byte = 1
	// OpPublicKey asks for the public key.
	OpPublicKey byte = 1
	// OpSign asks for a signature over a digest.
	OpSign byte = 2
	// PurposeHandshake charges a signature to the handshake budget.
	PurposeHandshake byte = 1
	// PurposeAnswer charges a signature to the answer budget.
	PurposeAnswer byte = 2
	// HashSHA256 is the only hash identifier.
	HashSHA256 byte = 1
	// DigestSize is the size of the only digest accepted: SHA-256.
	DigestSize = 32
	// MaxFrameBytes bounds the payload of a frame, in both directions. A
	// frame announcing more closes the connection unread.
	MaxFrameBytes = 256
	// frameHeaderBytes is the size of the length prefix.
	frameHeaderBytes = 2
)

// Status is the status byte of a response.
type Status byte

// The response statuses.
const (
	// StatusOK: the request succeeded; the body is the answer.
	StatusOK Status = 0
	// StatusBadRequest: the request is malformed or names an operation,
	// hash or digest size that is not allowed.
	StatusBadRequest Status = 1
	// StatusRateLimited: the signer's rate limit refused the request.
	StatusRateLimited Status = 2
	// StatusInternal: the signer could not sign. The body is empty.
	StatusInternal Status = 3
)

// String returns the status's name.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusBadRequest:
		return "bad request"
	case StatusRateLimited:
		return "rate limited"
	case StatusInternal:
		return "internal error"
	}
	return fmt.Sprintf("status %d", byte(s))
}

// Timing and size defaults.
const (
	// DefaultCallTimeout bounds one client call: connect, request, response.
	DefaultCallTimeout = 5 * time.Second
	// DefaultIOTimeout bounds the signer's work on one connection.
	DefaultIOTimeout = 5 * time.Second
	// DefaultMaxConns bounds the connections the signer serves at once.
	DefaultMaxConns = 32
)

// Errors.
var (
	// ErrSigner is wrapped by every failure to sign with the signer process:
	// the signer is down, refused the request, answered wrongly or answered
	// with a signature that does not verify. A caller treats it as "no
	// signature"; nothing is signed locally instead.
	ErrSigner = errors.New("host key signer")
	// ErrKey is wrapped by a failure that the signer's public key is not
	// usable (it does not decode, or is not ECDSA P-256); waiting cannot
	// cure it.
	ErrKey = errors.New("host key signer serves an unusable key")
	// ErrPeer is wrapped by a refused peer credential.
	ErrPeer = errors.New("peer credential refused")
	// ErrFrameTooLarge is returned for a frame longer than MaxFrameBytes.
	ErrFrameTooLarge = errors.New("frame too large")
)

// signHashes maps the accepted hash identifier to its crypto.Hash and the
// digest size it requires. It is the closed set the key type and the
// protocols that use it need.
var signHashes = map[byte]struct {
	hash crypto.Hash
	size int
}{
	HashSHA256: {crypto.SHA256, DigestSize},
}

// writeFrame writes payload as one frame.
func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	buf := make([]byte, frameHeaderBytes+len(payload))
	binary.BigEndian.PutUint16(buf, uint16(len(payload))) // #nosec G115 -- bounded by MaxFrameBytes above
	copy(buf[frameHeaderBytes:], payload)
	_, err := w.Write(buf)
	return err
}

// readFrame reads one frame. A length above MaxFrameBytes is refused before
// any payload is read; a short payload is an error.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [frameHeaderBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
