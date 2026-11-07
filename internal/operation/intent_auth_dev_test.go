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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The development signer's intent authenticator is stable for a key file
// (it survives a restart) and is domain-separated from grant signatures: an
// intent tag never verifies as a grant signature, and a grant signature never
// opens as an intent tag, over the very same bytes.
func TestDevSigner_IntentAuthenticatorDomainSeparation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	s, err := NewDevSigner(key, bytesOf(7, IDLen), nil)
	require.NoError(t, err)
	a := IntentAuthenticatorOf(s)
	require.NotNil(t, a)
	restarted, err := NewDevSigner(key, bytesOf(7, IDLen), nil)
	require.NoError(t, err)

	msg := IntentMessage([]byte("Create"), []byte("request bytes"))
	tag := a.SealIntent(msg)
	assert.True(t, IntentAuthenticatorOf(restarted).OpenIntent(msg, tag), "same key file, same intent key")

	// A grant signature over the intent message's bytes.
	sig, err := s.Sign(context.Background(), msg, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, a.OpenIntent(msg, sig.Signature), "a grant signature is not an intent tag")

	// The intent tag as a grant signature over the same bytes, by the leaf.
	leaf, err := x509.ParseCertificate(sig.Chain[0])
	require.NoError(t, err)
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok)
	digest := sha256.Sum256(msg)
	assert.False(t, ecdsa.VerifyASN1(pub, digest[:], tag), "an intent tag is not a grant signature")
	_, perr := parseECDSASignature(tag)
	assert.Error(t, perr, "an intent tag is not even a DER signature")

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	s2, err := NewDevSigner(other, bytesOf(7, IDLen), nil)
	require.NoError(t, err)
	assert.False(t, IntentAuthenticatorOf(s2).OpenIntent(msg, tag), "another key file's authenticator refuses it")
}
