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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func testRetireMint() RetireMintRequest {
	return RetireMintRequest{
		Identity:      testIdentity(providerv1.OperationKind_OPERATION_KIND_CREATE),
		IntentDigest:  bytesOf(0xD1, grantverify.DigestLen),
		AttestationID: bytesOf(0xA8, 16),
		IssuedAt:      testEpoch,
	}
}

func retireCall(t testing.TB, pki *testPKI, edit func(*providerv1.GrantBody)) *providerv1.RetireIfUnknownRequest {
	t.Helper()
	g, err := MintRetireGrant(context.Background(), pki, testRetireMint())
	require.NoError(t, err)
	if edit != nil {
		b := &providerv1.GrantBody{}
		require.NoError(t, proto.Unmarshal(g.GetBody(), b))
		edit(b)
		raw, err := proto.Marshal(b)
		require.NoError(t, err)
		sig, err := pki.Sign(context.Background(), raw, time.Time{})
		require.NoError(t, err)
		g = &providerv1.OperationGrant{Body: raw, Signature: sig.Signature, SignerCertChain: sig.Chain}
	}
	return &providerv1.RetireIfUnknownRequest{TargetHostId: testHostID, AnswerNonce: bytesOf(0x11, 16), Grant: g}
}

// A retirement grant verifies on RetireIfUnknown only, with the identity of
// the operation and the session.
func TestVerifyAuthority_Retire(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	req := retireCall(t, pki, nil)
	got, err := v.VerifyAuthority(providerv1.HostAuthority_RetireIfUnknown_FullMethodName, req, nil)
	require.NoError(t, err)
	id := testRetireMint().Identity
	assert.Equal(t, providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN, got.Body.GetGrantKind())
	assert.Equal(t, id.OpID, got.Body.GetOpId())
	assert.Equal(t, id.Attempt, got.Body.GetAttempt())
	assert.Equal(t, bytesOf(0xA8, 16), got.Body.GetAttestationId())
	assert.Empty(t, got.Body.GetDigestSalt(), "the salt never travels in a retirement grant")
	assert.Equal(t, grantverify.GrantDigest(req.GetGrant()), got.GrantDigest)

	// The same grant on the other methods is refused on its kind.
	_, err = v.VerifyAuthority(providerv1.HostAuthority_LedgerMark_FullMethodName,
		&providerv1.LedgerMarkRequest{TargetHostId: testHostID, AnswerNonce: req.GetAnswerNonce(), Grant: req.GetGrant()}, nil)
	assert.Equal(t, RuleGrantKind, ruleOf(t, err))
	_, err = v.VerifyAuthority(providerv1.HostAuthority_Attest_FullMethodName,
		&providerv1.AttestRequest{TargetHostId: testHostID, AnswerNonce: req.GetAnswerNonce(), Grant: req.GetGrant()}, nil)
	assert.Equal(t, RuleGrantKind, ruleOf(t, err))
}

func TestVerifyAuthority_RetireRefusals(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	m := providerv1.HostAuthority_RetireIfUnknown_FullMethodName
	cases := map[string]struct {
		edit func(*providerv1.GrantBody)
		rule Rule
	}{
		"version 1":         {func(b *providerv1.GrantBody) { b.GrantVersion = 1 }, RuleGrantVersion},
		"ledger mark kind":  {func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_LEDGER_MARK }, RuleGrantKind},
		"operation kind":    {func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_OPERATION }, RuleGrantKind},
		"no session":        {func(b *providerv1.GrantBody) { b.AttestationId = nil }, RuleMalformedField},
		"short session":     {func(b *providerv1.GrantBody) { b.AttestationId = bytesOf(1, 15) }, RuleMalformedField},
		"no op_id":          {func(b *providerv1.GrantBody) { b.OpId = nil }, RuleMalformedField},
		"short binding":     {func(b *providerv1.GrantBody) { b.BindingId = bytesOf(1, 15) }, RuleMalformedField},
		"no vm":             {func(b *providerv1.GrantBody) { b.VmUid = "" }, RuleMalformedField},
		"kind unspecified":  {func(b *providerv1.GrantBody) { b.Kind = providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED }, RuleUnspecifiedEnum},
		"kind unknown":      {func(b *providerv1.GrantBody) { b.Kind = 99 }, RuleUnknownEnum},
		"attempt 0":         {func(b *providerv1.GrantBody) { b.Attempt = 0 }, RuleCounterZero},
		"short digest":      {func(b *providerv1.GrantBody) { b.IntentDigest = bytesOf(1, 31) }, RuleMalformedField},
		"no mint epoch":     {func(b *providerv1.GrantBody) { b.MintEpoch = nil }, RuleMalformedField},
		"salt set":          {func(b *providerv1.GrantBody) { b.DigestSalt = bytesOf(1, 32) }, RuleFieldNotForKind},
		"expect_known set":  {func(b *providerv1.GrantBody) { b.ExpectKnown = true }, RuleFieldNotForKind},
		"nonce set":         {func(b *providerv1.GrantBody) { b.SingleUseNonce = bytesOf(1, 16) }, RuleFieldNotForKind},
		"ledger set":        {func(b *providerv1.GrantBody) { b.Ledger = &providerv1.LedgerPosition{Seq: 1} }, RuleFieldNotForKind},
		"checkpoint set":    {func(b *providerv1.GrantBody) { b.LedgerCheckpoint = &providerv1.LedgerPosition{Seq: 1} }, RuleFieldNotForKind},
		"admin field set":   {func(b *providerv1.GrantBody) { b.Action = "x" }, RuleFieldNotForKind},
		"placeholder set":   {func(b *providerv1.GrantBody) { b.EalSeq = 1 }, RulePlaceholderSet},
		"generation 0":      {func(b *providerv1.GrantBody) { b.AuthorityGeneration = 0 }, RuleCounterZero},
		"another enrolment": {func(b *providerv1.GrantBody) { b.EnrolmentId = bytesOf(9, 16) }, RuleBinding},
		"another host":      {func(b *providerv1.GrantBody) { b.HostId = "host-b" }, RuleBinding},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.VerifyAuthority(m, retireCall(t, pki, c.edit), nil)
			assert.Equal(t, c.rule, ruleOf(t, err))
		})
	}
}

func TestMintRetireGrant_Refusals(t *testing.T) {
	pki := validPKI(t)
	ctx := context.Background()
	for name, edit := range map[string]func(*RetireMintRequest){
		"no session":   func(r *RetireMintRequest) { r.AttestationID = nil },
		"no digest":    func(r *RetireMintRequest) { r.IntentDigest = nil },
		"no op_id":     func(r *RetireMintRequest) { r.Identity.OpID = nil },
		"no issued_at": func(r *RetireMintRequest) { r.IssuedAt = time.Time{} },
		"too long":     func(r *RetireMintRequest) { r.Lifetime = 2 * time.Hour },
	} {
		r := testRetireMint()
		edit(&r)
		_, err := MintRetireGrant(ctx, pki, r)
		assert.Error(t, err, name)
	}
	_, err := MintRetireGrant(ctx, nil, testRetireMint())
	require.ErrorIs(t, err, ErrNoSigner)
}
