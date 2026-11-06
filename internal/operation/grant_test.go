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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// protoMessage is proto.Message, named for the table helpers.
type protoMessage = proto.Message

// validMint is a valid MintRequest for kind.
func validMint(t *testing.T, kind providerv1.OperationKind) MintRequest {
	t.Helper()
	b, err := proto.Marshal(testRequest(kind))
	require.NoError(t, err)
	return MintRequest{Identity: testIdentity(kind), DigestSalt: bytesOf(0x5A, SaltLen), Request: b, IssuedAt: testEpoch}
}

// TestMintOperationGrant_BodyFields: the minted body is a version 1
// OPERATION body with exactly the fields of the identity, the signer and the
// request; 18 is false and 20-29 are unset; it is serialised once and the
// signature covers those bytes.
func TestMintOperationGrant_BodyFields(t *testing.T) {
	pki := validPKI(t)
	r := validMint(t, providerv1.OperationKind_OPERATION_KIND_POWER_ON)
	g, err := MintOperationGrant(context.Background(), pki, r)
	require.NoError(t, err)
	var body providerv1.GrantBody
	require.NoError(t, proto.Unmarshal(g.GetBody(), &body))
	assert.Equal(t, GrantVersion, body.GetGrantVersion())
	assert.Equal(t, providerv1.GrantKind_GRANT_KIND_OPERATION, body.GetGrantKind())
	assert.Equal(t, pki.installID, body.GetInstallId())
	assert.Equal(t, uint64(1), body.GetAuthorityGeneration())
	assert.Equal(t, testProviderUID, body.GetProviderUid())
	assert.Equal(t, testHostID, body.GetHostId())
	assert.Equal(t, testEnrolment, body.GetEnrolmentId())
	assert.Equal(t, r.Identity.OpID, body.GetOpId())
	assert.Equal(t, r.Identity.BindingID, body.GetBindingId())
	assert.Equal(t, uint64(1), body.GetAttempt())
	assert.Equal(t, IntentDigest(r.DigestSalt, r.Request), body.GetIntentDigest())
	assert.Equal(t, r.DigestSalt, body.GetDigestSalt())
	assert.False(t, body.GetExpectKnown())
	assert.Equal(t, testEpoch, body.GetIssuedAt().AsTime())
	assert.Equal(t, testEpoch.Add(DefaultGrantLifetime), body.GetNotAfter().AsTime())
	assert.Zero(t, body.GetEalSeq())
	assert.Nil(t, body.GetResetFloor())
	assert.Empty(t, body.GetCeremonyRecordDigest())
	assert.Empty(t, body.GetTargetNamespace())
	assert.Nil(t, body.GetPrecondition())
	assert.Len(t, g.GetSignerCertChain(), 2)

	// The verifier accepts it.
	env := &providerv1.OperationEnvelope{Request: r.Request, Grant: g}
	_, err = testVerifier(t, pki).Verify(providerv1.Provider_Power_FullMethodName, &providerv1.PowerRequest{OperationEnvelope: env})
	require.NoError(t, err)
}

// TestMintOperationGrant_Refusals: the minter refuses, before signing, what
// the verifier would refuse on its own.
func TestMintOperationGrant_Refusals(t *testing.T) {
	pki := validPKI(t)
	for name, edit := range map[string]func(r *MintRequest){
		"attempt 0":            func(r *MintRequest) { r.Identity.Attempt = 0 },
		"epoch counter 0":      func(r *MintRequest) { r.Identity.Epoch.Counter = 0 },
		"mint epoch counter 0": func(r *MintRequest) { r.Identity.MintEpoch.Counter = 0 },
		"kind unspecified":     func(r *MintRequest) { r.Identity.Kind = providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED },
		"kind unknown":         func(r *MintRequest) { r.Identity.Kind = 99 },
		"op id short":          func(r *MintRequest) { r.Identity.OpID = []byte{1} },
		"binding id missing":   func(r *MintRequest) { r.Identity.BindingID = nil },
		"enrolment id missing": func(r *MintRequest) { r.Identity.EnrolmentID = nil },
		"vm uid empty":         func(r *MintRequest) { r.Identity.VMUID = "" },
		"salt short":           func(r *MintRequest) { r.DigestSalt = []byte{1} },
		"issued at unset":      func(r *MintRequest) { r.IssuedAt = time.Time{} },
		"lifetime too long":    func(r *MintRequest) { r.Lifetime = MaxGrantLifetime + time.Second },
		"lifetime negative":    func(r *MintRequest) { r.Lifetime = -time.Second },
		"request wrong op": func(r *MintRequest) {
			r.Request = mustMarshal(t, &providerv1.PowerRequest{Op: providerv1.PowerOp_POWER_OP_OFF})
		},
		"request inner envelope": func(r *MintRequest) {
			r.Request = mustMarshal(t, &providerv1.PowerRequest{Op: providerv1.PowerOp_POWER_OP_ON,
				OperationEnvelope: &providerv1.OperationEnvelope{}})
		},
		"request unknown field": func(r *MintRequest) { r.Request = append(r.Request, unknownField()...) },
		"request garbage":       func(r *MintRequest) { r.Request = []byte{0xFF, 0xFF} },
	} {
		t.Run(name, func(t *testing.T) {
			r := validMint(t, providerv1.OperationKind_OPERATION_KIND_POWER_ON)
			edit(&r)
			_, err := MintOperationGrant(context.Background(), pki, r)
			assert.Error(t, err)
			assert.Error(t, CheckMintRequest(pki.installID, 1, r))
		})
	}
	t.Run("authority generation 0", func(t *testing.T) {
		p := validPKI(t)
		p.generation = 0
		_, err := MintOperationGrant(context.Background(), p, validMint(t, providerv1.OperationKind_OPERATION_KIND_POWER_ON))
		assert.Error(t, err)
	})
	t.Run("nil signer", func(t *testing.T) {
		_, err := MintOperationGrant(context.Background(), nil, validMint(t, providerv1.OperationKind_OPERATION_KIND_POWER_ON))
		assert.True(t, errors.Is(err, ErrNoSigner))
	})
	t.Run("valid request passes the check", func(t *testing.T) {
		assert.NoError(t, CheckMintRequest(pki.installID, 1, validMint(t, providerv1.OperationKind_OPERATION_KIND_POWER_ON)))
	})
}

// mustMarshal serialises m.
func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	return b
}

// TestRandomIdentifiers: ids and salts have their lengths and differ.
func TestRandomIdentifiers(t *testing.T) {
	a, err := NewRandomID()
	require.NoError(t, err)
	b, err := NewRandomID()
	require.NoError(t, err)
	assert.Len(t, a, IDLen)
	assert.NotEqual(t, a, b)
	s, err := NewDigestSalt()
	require.NoError(t, err)
	assert.Len(t, s, SaltLen)
}

// TestDecodeRequest: a request decodes as its kind's type; an unknown kind,
// another kind's fixed op and unknown fields are refused.
func TestDecodeRequest(t *testing.T) {
	b := mustMarshal(t, testRequest(providerv1.OperationKind_OPERATION_KIND_REBOOT))
	m, err := DecodeRequest(providerv1.OperationKind_OPERATION_KIND_REBOOT, b)
	require.NoError(t, err)
	assert.True(t, proto.Equal(testRequest(providerv1.OperationKind_OPERATION_KIND_REBOOT), m))
	_, err = DecodeRequest(providerv1.OperationKind_OPERATION_KIND_POWER_ON, b)
	assert.Error(t, err)
	_, err = DecodeRequest(99, b)
	assert.Error(t, err)
	_, err = DecodeRequest(providerv1.OperationKind_OPERATION_KIND_REBOOT, append(b, unknownField()...))
	assert.Error(t, err)
}
