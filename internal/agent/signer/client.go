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

package signer

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// ClientOptions configures a Client.
type ClientOptions struct {
	// Socket is the path of the signer's Unix socket.
	Socket string
	// ServerUID is the only uid the signer may have: its user's.
	ServerUID uint32
	// Lifetime is the context of the Client: when it ends every call fails.
	// Nil uses the context given to Connect.
	Lifetime context.Context //nolint:containedctx // crypto.Signer.Sign carries no context; this is the Client's lifetime
	// Timeout bounds one call (DefaultCallTimeout when zero).
	Timeout time.Duration
	// peerUID reads a connection's peer uid (PeerUID; tests replace it).
	peerUID func(net.Conn) (uint32, error)
}

// Client is the agent's view of the host key: a crypto.Signer whose Public is
// the key the signer holds and whose Sign sends the digest to the signer
// process. It never holds the private key.
//
// Each call opens its own connection, checks the signer's uid, sends one
// request, reads one response and closes the connection, so a signer that
// restarts is simply reached again on the next call, no connection is
// pooled and no goroutine runs between calls. A call that cannot get a
// verified signature fails with an error wrapping ErrSigner; nothing is
// signed locally.
//
// Because crypto.Signer.Sign has no context, the Client carries a lifetime
// context (ClientOptions.Lifetime): when it ends every call fails.
type Client struct {
	ctx context.Context //nolint:containedctx // crypto.Signer.Sign carries no context; this is the Client's lifetime
	o   ClientOptions
	pub *ecdsa.PublicKey
}

var _ crypto.Signer = (*Client)(nil)

// Connect returns a Client for the signer at o.Socket after fetching and
// checking its public key (ECDSA P-256). ctx bounds the fetch together with
// o.Timeout; the Client's lifetime is o.Lifetime, or ctx when that is nil.
func Connect(ctx context.Context, o ClientOptions) (*Client, error) {
	if o.Timeout == 0 {
		o.Timeout = DefaultCallTimeout
	}
	if o.peerUID == nil {
		o.peerUID = PeerUID
	}
	if o.Lifetime == nil {
		o.Lifetime = ctx
	}
	c := &Client{ctx: o.Lifetime, o: o}
	body, err := c.call(ctx, []byte{Version, OpPublicKey})
	if err != nil {
		return nil, err
	}
	pub, err := parsePublicKey(body)
	if err != nil {
		return nil, err
	}
	c.pub = pub
	return c, nil
}

// ConnectWithRetry is Connect, tried again every retryEvery until it
// succeeds or ctx ends; it is for the start-up, when the signer may still be
// starting or restarting. The error of the last attempt is returned when ctx
// ends.
//
// A failure that waiting cannot cure (the peer is not the signer's user, or
// the signer serves a key of the wrong type) ends it at once. onRetry, when
// not nil, is called with each failure that will be retried.
func ConnectWithRetry(ctx context.Context, o ClientOptions, retryEvery time.Duration, onRetry func(error)) (*Client, error) {
	for {
		c, err := Connect(ctx, o)
		if err == nil {
			return c, nil
		}
		if errors.Is(err, ErrPeer) || errors.Is(err, ErrKey) {
			return nil, err
		}
		if onRetry != nil && ctx.Err() == nil {
			onRetry(err)
		}
		t := time.NewTimer(retryEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
	}
}

// Public implements crypto.Signer: the signer's public key.
func (c *Client) Public() crypto.PublicKey { return c.pub }

// Sign implements crypto.Signer. digest must be a SHA-256 digest (32 bytes)
// and opts the crypto.SHA256 hash, which is what crypto/tls and the host
// answer signing pass for an ECDSA P-256 key; anything else is refused
// without reaching the signer. rand is ignored: the signer draws its own
// randomness. The returned signature is ASN.1 DER and has been verified
// against the public key.
//
// A Client signs as PurposeHandshake (the TLS handshake budget); AnswerSigner
// returns the view that signs as PurposeAnswer.
func (c *Client) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return c.sign(r, digest, opts, PurposeHandshake)
}

// AnswerSigner returns a crypto.Signer for the same key that charges its
// signatures to the answer budget of the signer, so that host answers are not
// starved by a flood of handshakes.
func (c *Client) AnswerSigner() crypto.Signer { return answerSigner{c} }

// answerSigner is a Client's view that signs as PurposeAnswer.
type answerSigner struct{ c *Client }

// Public implements crypto.Signer.
func (a answerSigner) Public() crypto.PublicKey { return a.c.Public() }

// Sign implements crypto.Signer; see Client.Sign.
func (a answerSigner) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return a.c.sign(r, digest, opts, PurposeAnswer)
}

// sign sends a sign request charged to purpose.
func (c *Client) sign(_ io.Reader, digest []byte, opts crypto.SignerOpts, purpose byte) ([]byte, error) {
	if h, ok := opts.(crypto.Hash); !ok || h != crypto.SHA256 {
		return nil, fmt.Errorf("%w: only crypto.SHA256 digests are signed", ErrSigner)
	}
	if len(digest) != DigestSize {
		return nil, fmt.Errorf("%w: a SHA-256 digest is %d bytes, not %d", ErrSigner, DigestSize, len(digest))
	}
	if err := c.ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w: %v", ErrSigner, ErrUnavailable, err)
	}
	req := make([]byte, 0, 4+len(digest))
	req = append(req, Version, OpSign, HashSHA256, purpose)
	req = append(req, digest...)
	sig, err := c.call(c.ctx, req)
	if err != nil {
		return nil, err
	}
	if !guard.Off(guard.SignerVerify) && !ecdsa.VerifyASN1(c.pub, digest, sig) {
		return nil, fmt.Errorf("%w: the signature does not verify", ErrSigner)
	}
	return sig, nil
}

// call sends one request and returns the body of an OK response.
func (c *Client) call(ctx context.Context, req []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.o.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: connect: %v", ErrSigner, ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSigner, err)
		}
	}
	if !guard.Off(guard.SignerClientPeerCheck) {
		uid, err := c.o.peerUID(conn)
		if err == nil {
			err = CheckPeer(uid, c.o.ServerUID)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w: %w", ErrSigner, ErrUnavailable, err)
		}
	}
	if err := writeFrame(conn, req); err != nil {
		return nil, fmt.Errorf("%w: %w: send: %v", ErrSigner, ErrUnavailable, err)
	}
	resp, err := readFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: receive: %v", ErrSigner, ErrUnavailable, err)
	}
	return parseResponse(resp)
}

// parseResponse returns the body of an OK response payload, and an error
// wrapping ErrSigner for anything else.
func parseResponse(resp []byte) ([]byte, error) {
	if len(resp) < 2 || resp[0] != Version {
		return nil, fmt.Errorf("%w: malformed response", ErrSigner)
	}
	if st := Status(resp[1]); st != StatusOK {
		kind := ErrRefused
		if st == StatusRateLimited {
			kind = ErrRateLimited
		}
		return nil, fmt.Errorf("%w: %w: the signer answered: %s", ErrSigner, kind, st)
	}
	return resp[2:], nil
}

// parsePublicKey decodes the body of a public key response: an ECDSA P-256
// key, or an error wrapping ErrSigner and ErrKey.
func parsePublicKey(body []byte) (*ecdsa.PublicKey, error) {
	pk, err := x509.ParsePKIXPublicKey(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w: the public key does not decode", ErrSigner, ErrKey)
	}
	pub, ok := pk.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: %w: the public key is not ECDSA P-256", ErrSigner, ErrKey)
	}
	return pub, nil
}
