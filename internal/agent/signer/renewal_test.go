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
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renewalURIs are a host URI identity and an enrolment record.
var renewalURIs = []string{
	"spiffe://example.test/virtrigaud/host/hosts/host-a",
	"spiffe://example.test/virtrigaud/record/enrolment/" + strings.Repeat("03", 16),
	"spiffe://example.test/virtrigaud/record/provider/0b7a1c3e-1111-4222-8333-944455556666",
	"spiffe://example.test/virtrigaud/record/install/" + strings.Repeat("02", 16),
	"spiffe://example.test/virtrigaud/record/mode/identity",
}

// renewalSigner writes a host key file in a private directory and serves it
// with renewal on.
func renewalSigner(t *testing.T, renewal bool) (*running, string) {
	t.Helper()
	dir := shortTempDir(t)
	keyDir := filepath.Join(dir, "keys")
	require.NoError(t, os.Mkdir(keyDir, 0o700))
	k, created, err := CreateKey(filepath.Join(keyDir, "host.key"), EUID())
	require.NoError(t, err)
	require.True(t, created)
	keyFile := filepath.Join(keyDir, "host.key")
	r := serve(t, filepath.Join(dir, "s.sock"), ServerOptions{Key: k, Renewal: renewal, KeyFile: keyFile, OwnerUID: EUID(), RenewalBurst: 100}, nil)
	return r, keyFile
}

func pinOf(t *testing.T, pub *ecdsa.PublicKey) []byte {
	t.Helper()
	p, err := keyPin(pub)
	require.NoError(t, err)
	return p
}

func TestRenewalRequestActivate(t *testing.T) {
	r, keyFile := renewalSigner(t, true)
	c := r.client(t)
	ctx := context.Background()
	old := ecPub(t, c.Public())

	der, err := c.RenewalRequest(ctx, renewalURIs)
	require.NoError(t, err)
	csr, err := x509.ParseCertificateRequest(der)
	require.NoError(t, err)
	newPub := ecPub(t, csr.PublicKey)
	assert.False(t, newPub.Equal(old), "the renewal key is a new key")
	fi, err := os.Stat(PendingKeyFile(keyFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "the pending key is private")

	// Asking again reuses the pending key.
	der2, err := c.RenewalRequest(ctx, renewalURIs)
	require.NoError(t, err)
	csr2, err := x509.ParseCertificateRequest(der2)
	require.NoError(t, err)
	assert.True(t, ecPub(t, csr2.PublicKey).Equal(newPub), "the pending key is kept across requests")

	// The key in use still signs until activation.
	d := sha256.Sum256([]byte("before"))
	sig, err := c.AnswerSigner().Sign(rand.Reader, d[:], crypto.SHA256)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(old, d[:], sig))

	// Activating another key is refused and changes nothing.
	_, err = c.ActivateRenewal(ctx, pinOf(t, &newKey(t).PublicKey))
	require.ErrorIs(t, err, ErrRefused)

	got, err := c.ActivateRenewal(ctx, pinOf(t, newPub))
	require.NoError(t, err)
	assert.True(t, got.Equal(newPub))
	_, err = os.Stat(PendingKeyFile(keyFile))
	assert.ErrorIs(t, err, os.ErrNotExist, "the pending key file became the host key file")
	onDisk, err := LoadKey(keyFile, EUID())
	require.NoError(t, err)
	assert.True(t, onDisk.PublicKey.Equal(newPub), "the host key file holds the new key")

	// A new client sees the new key; signatures are by it.
	c2 := r.client(t)
	assert.True(t, ecPub(t, c2.Public()).Equal(newPub))
	sig, err = c2.AnswerSigner().Sign(rand.Reader, d[:], crypto.SHA256)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(newPub, d[:], sig))

	// Activating again is idempotent.
	got, err = c2.ActivateRenewal(ctx, pinOf(t, newPub))
	require.NoError(t, err)
	assert.True(t, got.Equal(newPub))
}

func TestRenewalDiscard(t *testing.T) {
	r, keyFile := renewalSigner(t, true)
	c := r.client(t)
	ctx := context.Background()
	der, err := c.RenewalRequest(ctx, renewalURIs)
	require.NoError(t, err)
	csr, err := x509.ParseCertificateRequest(der)
	require.NoError(t, err)
	// Another pin leaves it.
	require.NoError(t, c.DiscardRenewal(ctx, pinOf(t, &newKey(t).PublicKey)))
	_, err = os.Stat(PendingKeyFile(keyFile))
	require.NoError(t, err)
	require.NoError(t, c.DiscardRenewal(ctx, pinOf(t, ecPub(t, csr.PublicKey))))
	_, err = os.Stat(PendingKeyFile(keyFile))
	assert.ErrorIs(t, err, os.ErrNotExist)
	// The next request makes a fresh key.
	der2, err := c.RenewalRequest(ctx, renewalURIs)
	require.NoError(t, err)
	csr2, err := x509.ParseCertificateRequest(der2)
	require.NoError(t, err)
	assert.False(t, ecPub(t, csr2.PublicKey).Equal(ecPub(t, csr.PublicKey)))
	// With no pending key, discard succeeds.
	require.NoError(t, c.DiscardRenewal(ctx, pinOf(t, ecPub(t, csr2.PublicKey))))
	require.NoError(t, c.DiscardRenewal(ctx, pinOf(t, ecPub(t, csr2.PublicKey))))
}

func TestRenewalRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("not served without renewal", func(t *testing.T) {
		r, keyFile := renewalSigner(t, false)
		c := r.client(t)
		_, err := c.RenewalRequest(ctx, renewalURIs)
		require.ErrorIs(t, err, ErrSigner, "a signer that serves no renewal does not read the request")
		_, err = c.ActivateRenewal(ctx, pinOf(t, &newKey(t).PublicKey))
		require.ErrorIs(t, err, ErrRefused, "a small renewal request is read and refused")
		_, err = os.Stat(PendingKeyFile(keyFile))
		assert.ErrorIs(t, err, os.ErrNotExist, "no key was made")
	})
	t.Run("not a host identity", func(t *testing.T) {
		r, keyFile := renewalSigner(t, true)
		c := r.client(t)
		_, err := c.RenewalRequest(ctx, []string{"https://example.test/x"})
		require.ErrorIs(t, err, ErrRefused)
		_, err = c.RenewalRequest(ctx, renewalURIs[:3])
		require.ErrorIs(t, err, ErrRefused)
		_, err = os.Stat(PendingKeyFile(keyFile))
		assert.ErrorIs(t, err, os.ErrNotExist, "no key was made for a refused request")
	})
	t.Run("activation without a pending key", func(t *testing.T) {
		r, _ := renewalSigner(t, true)
		c := r.client(t)
		_, err := c.ActivateRenewal(ctx, pinOf(t, &newKey(t).PublicKey))
		require.ErrorIs(t, err, ErrRefused)
	})
	t.Run("malformed bodies", func(t *testing.T) {
		r, _ := renewalSigner(t, true)
		for name, req := range map[string][]byte{
			"no URI count":       {Version, OpRenewalRequest},
			"zero URIs":          {Version, OpRenewalRequest, 0},
			"truncated URI":      binary.BigEndian.AppendUint16([]byte{Version, OpRenewalRequest, 1}, 10),
			"short pin":          append([]byte{Version, OpRenewalActivate}, make([]byte, 31)...),
			"long discard pin":   append([]byte{Version, OpRenewalDiscard}, make([]byte, 33)...),
			"renewal as purpose": append([]byte{Version, OpSign, HashSHA256, purposeRenewal}, make([]byte, 32)...),
		} {
			st, _ := rawCall(t, r.socket, req)
			assert.Equal(t, StatusBadRequest, st, name)
		}
	})
}

// TestRenewalCrashBetweenRenameAndUse loads the key file the way a restarted
// signer does after a crash just after the pending key was renamed into
// place: the new key is in use, whole.
func TestRenewalRateLimited(t *testing.T) {
	dir := shortTempDir(t)
	keyDir := filepath.Join(dir, "keys")
	require.NoError(t, os.Mkdir(keyDir, 0o700))
	keyFile := filepath.Join(keyDir, "host.key")
	k, _, err := CreateKey(keyFile, EUID())
	require.NoError(t, err)
	r := serve(t, filepath.Join(dir, "s.sock"), ServerOptions{Key: k, Renewal: true, KeyFile: keyFile, OwnerUID: EUID(),
		RenewalPerSecond: 0.001, RenewalBurst: 2}, nil)
	c := r.client(t)
	for range 2 {
		_, err := c.RenewalRequest(context.Background(), renewalURIs)
		require.NoError(t, err)
	}
	_, err = c.RenewalRequest(context.Background(), renewalURIs)
	require.ErrorIs(t, err, ErrRateLimited)
	// The answer budget is not charged.
	d := sha256.Sum256([]byte("x"))
	_, err = c.AnswerSigner().Sign(rand.Reader, d[:], crypto.SHA256)
	require.NoError(t, err)
}

func TestRenewalCrashBetweenRenameAndUse(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	require.NoError(t, os.Mkdir(keyDir, 0o700))
	keyFile := filepath.Join(keyDir, "host.key")
	_, _, err := CreateKey(keyFile, EUID())
	require.NoError(t, err)
	pending, created, err := createKeyAt(PendingKeyFile(keyFile), EUID())
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, os.Rename(PendingKeyFile(keyFile), keyFile))
	k, err := LoadKey(keyFile, EUID())
	require.NoError(t, err)
	assert.True(t, k.PublicKey.Equal(&pending.PublicKey))
}

// rawCall sends one raw request and returns the status.
func rawCall(t *testing.T, socket string, req []byte) (Status, []byte) {
	t.Helper()
	c := &Client{o: ClientOptions{Socket: socket, ServerUID: EUID(), Timeout: DefaultCallTimeout, peerUID: PeerUID}, ctx: context.Background()}
	body, err := c.call(context.Background(), req)
	if err == nil {
		return StatusOK, body
	}
	switch {
	case errors.Is(err, ErrRateLimited):
		return StatusRateLimited, nil
	case errors.Is(err, ErrRefused) && strings.Contains(err.Error(), StatusBadRequest.String()):
		return StatusBadRequest, nil
	}
	return StatusInternal, nil
}

// ecPub returns k as an ECDSA public key, failing the test otherwise.
func ecPub(t testing.TB, k any) *ecdsa.PublicKey {
	t.Helper()
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("not an ECDSA public key: %T", k)
	}
	return pub
}

// TestRequestFrameBoundWithoutRenewal: a signer that serves no renewal reads
// requests of at most MaxSignFrameBytes, as before renewal existed.
func TestRequestFrameBoundWithoutRenewal(t *testing.T) {
	r, _ := renewalSigner(t, false)
	big := append([]byte{Version, OpRenewalRequest, 1}, make([]byte, MaxSignFrameBytes)...)
	st, _ := rawCall(t, r.socket, big)
	assert.Equal(t, StatusInternal, st, "the connection is closed unread")
	r2, _ := renewalSigner(t, true)
	st, _ = rawCall(t, r2.socket, big)
	assert.Equal(t, StatusBadRequest, st, "a renewal signer reads it and refuses its content")
}
