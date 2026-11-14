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

package grantverify_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func testConfig(t *testing.T) grantverify.VerifierConfig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "anchor"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	anchor, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	id := make([]byte, grantverify.IDLen)
	return grantverify.VerifierConfig{Anchors: []*x509.Certificate{anchor}, InstallID: id, ProviderUID: "p", HostID: "h", EnrolmentID: id}
}

// TestNewVerifier_ClockAndNowAreExclusive: both set is a configuration error.
func TestNewVerifier_ClockAndNowAreExclusive(t *testing.T) {
	cfg := testConfig(t)
	cfg.Clock = grantverify.WallClock{}
	cfg.Now = time.Now
	_, err := grantverify.NewVerifier(cfg)
	require.Error(t, err)
}

// TestVerify_UntrustworthyClockFailsClosed: a Clock that returns the zero time
// refuses every call, before the grant is looked at.
func TestVerify_UntrustworthyClockFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Clock = grantverify.ClockFunc(func() time.Time { return time.Time{} })
	v, err := grantverify.NewVerifier(cfg)
	require.NoError(t, err)
	// Any outer message with an envelope reaches the clock check first.
	_, err = v.Verify(providerv1.Provider_Power_FullMethodName, &providerv1.PowerRequest{
		OperationEnvelope: &providerv1.OperationEnvelope{Grant: &providerv1.OperationGrant{}},
	})
	r, ok := grantverify.AsRefusal(err)
	require.True(t, ok)
	assert.Equal(t, grantverify.ReasonGrantUnauthenticated, r.Reason)
	assert.Equal(t, grantverify.RuleUntrustedClock, r.Rule)
}

// TestVerify_ClockIsReadOncePerCall: the verifier reads its Clock, not the
// wall clock, and does so once per call.
func TestVerify_ClockIsReadOncePerCall(t *testing.T) {
	cfg := testConfig(t)
	calls := 0
	cfg.Clock = grantverify.ClockFunc(func() time.Time { calls++; return time.Time{} })
	v, err := grantverify.NewVerifier(cfg)
	require.NoError(t, err)
	_, _ = v.Verify(providerv1.Provider_Power_FullMethodName, &providerv1.PowerRequest{
		OperationEnvelope: &providerv1.OperationEnvelope{Grant: &providerv1.OperationGrant{}},
	})
	assert.Equal(t, 1, calls)
}
