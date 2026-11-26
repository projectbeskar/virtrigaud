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

func testLedgerMarkMint() LedgerMarkMintRequest {
	return LedgerMarkMintRequest{
		ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment,
		Epoch:         Epoch{Counter: 1, Holder: bytesOf(0xC1, IDLen)},
		AttestationID: bytesOf(0xA8, 16),
		Ledger:        LedgerPosition{StoreID: bytesOf(0x5E, IDLen), Seq: 3100, Head: bytesOf(0x4F, 32)},
		IssuedAt:      testEpoch,
	}
}

func ledgerMarkCall(t testing.TB, pki *testPKI, edit func(*providerv1.GrantBody)) *providerv1.LedgerMarkRequest {
	t.Helper()
	g, err := MintLedgerMarkGrant(context.Background(), pki, testLedgerMarkMint())
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
	return &providerv1.LedgerMarkRequest{TargetHostId: testHostID, AnswerNonce: bytesOf(0x11, 16), Grant: g}
}

// A ledger mark grant verifies on LedgerMark only, with its session and
// ledger position.
func TestVerifyAuthority_LedgerMark(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	req := ledgerMarkCall(t, pki, nil)
	got, err := v.VerifyAuthority(providerv1.HostAuthority_LedgerMark_FullMethodName, req, nil)
	require.NoError(t, err)
	assert.Equal(t, providerv1.GrantKind_GRANT_KIND_LEDGER_MARK, got.Body.GetGrantKind())
	assert.Equal(t, uint64(3100), got.Body.GetLedger().GetSeq())
	assert.Equal(t, bytesOf(0xA8, 16), got.Body.GetAttestationId())
	assert.Equal(t, grantverify.GrantDigest(req.GetGrant()), got.GrantDigest)

	// The same grant on Attest is refused on its kind, before any field.
	_, err = v.VerifyAuthority(providerv1.HostAuthority_Attest_FullMethodName,
		&providerv1.AttestRequest{TargetHostId: testHostID, AnswerNonce: req.GetAnswerNonce(), Grant: req.GetGrant()}, nil)
	assert.Equal(t, RuleGrantKind, ruleOf(t, err))
	// And on RetireIfUnknown.
	_, err = v.VerifyAuthority(providerv1.HostAuthority_RetireIfUnknown_FullMethodName,
		&providerv1.RetireIfUnknownRequest{TargetHostId: testHostID, AnswerNonce: req.GetAnswerNonce(), Grant: req.GetGrant()}, nil)
	assert.Equal(t, RuleGrantKind, ruleOf(t, err))
}

func TestVerifyAuthority_LedgerMarkRefusals(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	m := providerv1.HostAuthority_LedgerMark_FullMethodName
	cases := map[string]struct {
		edit func(*providerv1.GrantBody)
		rule Rule
	}{
		"version 1":            {func(b *providerv1.GrantBody) { b.GrantVersion = 1 }, RuleGrantVersion},
		"attest kind":          {func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_ATTEST }, RuleGrantKind},
		"operation kind":       {func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_OPERATION }, RuleGrantKind},
		"retire kind":          {func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN }, RuleGrantKind},
		"no session":           {func(b *providerv1.GrantBody) { b.AttestationId = nil }, RuleMalformedField},
		"short session":        {func(b *providerv1.GrantBody) { b.AttestationId = bytesOf(1, 15) }, RuleMalformedField},
		"long session":         {func(b *providerv1.GrantBody) { b.AttestationId = bytesOf(1, 17) }, RuleMalformedField},
		"no ledger":            {func(b *providerv1.GrantBody) { b.Ledger = nil }, RuleMalformedField},
		"ledger seq 0":         {func(b *providerv1.GrantBody) { b.Ledger.Seq = 0 }, RuleCounterZero},
		"short head":           {func(b *providerv1.GrantBody) { b.Ledger.Head = bytesOf(1, 31) }, RuleMalformedField},
		"short store":          {func(b *providerv1.GrantBody) { b.Ledger.StoreId = bytesOf(1, 15) }, RuleMalformedField},
		"nonce set":            {func(b *providerv1.GrantBody) { b.SingleUseNonce = bytesOf(1, 16) }, RuleFieldNotForKind},
		"checkpoint set":       {func(b *providerv1.GrantBody) { b.LedgerCheckpoint = cpAt(b, 2048) }, RuleFieldNotForKind},
		"operation field set":  {func(b *providerv1.GrantBody) { b.OpId = bytesOf(1, 16) }, RuleFieldNotForKind},
		"expect_known set":     {func(b *providerv1.GrantBody) { b.ExpectKnown = true }, RuleFieldNotForKind},
		"admin field set":      {func(b *providerv1.GrantBody) { b.Action = "x" }, RuleFieldNotForKind},
		"placeholder set":      {func(b *providerv1.GrantBody) { b.EalSeq = 1 }, RulePlaceholderSet},
		"generation 0":         {func(b *providerv1.GrantBody) { b.AuthorityGeneration = 0 }, RuleCounterZero},
		"another enrolment":    {func(b *providerv1.GrantBody) { b.EnrolmentId = bytesOf(9, 16) }, RuleBinding},
		"another Provider":     {func(b *providerv1.GrantBody) { b.ProviderUid = "other" }, RuleBinding},
		"another host":         {func(b *providerv1.GrantBody) { b.HostId = "host-b" }, RuleBinding},
		"another installation": {func(b *providerv1.GrantBody) { b.InstallId = bytesOf(9, 16) }, RuleBinding},
		"expired": {func(b *providerv1.GrantBody) {
			b.NotAfter.Seconds = testEpoch.Unix() - 1
			b.IssuedAt.Seconds = testEpoch.Unix() - 10
		}, RuleValidity},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.VerifyAuthority(m, ledgerMarkCall(t, pki, c.edit), nil)
			assert.Equal(t, c.rule, ruleOf(t, err))
		})
	}
}

func TestMintLedgerMarkGrant_Refusals(t *testing.T) {
	pki := validPKI(t)
	ctx := context.Background()
	for name, edit := range map[string]func(*LedgerMarkMintRequest){
		"no session":   func(r *LedgerMarkMintRequest) { r.AttestationID = nil },
		"no issued_at": func(r *LedgerMarkMintRequest) { r.IssuedAt = time.Time{} },
		"too long":     func(r *LedgerMarkMintRequest) { r.Lifetime = 2 * time.Hour },
		"seq 0":        func(r *LedgerMarkMintRequest) { r.Ledger.Seq = 0 },
	} {
		r := testLedgerMarkMint()
		edit(&r)
		_, err := MintLedgerMarkGrant(ctx, pki, r)
		assert.Error(t, err, name)
	}
	_, err := MintLedgerMarkGrant(ctx, nil, testLedgerMarkMint())
	require.ErrorIs(t, err, ErrNoSigner)
}

// expect_known is minted only at the ledger grant version, and a version 2
// operation grant that sets it verifies.
func TestMintOperationGrant_ExpectKnown(t *testing.T) {
	pki := validPKI(t)
	kind := providerv1.OperationKind_OPERATION_KIND_CREATE
	reqBytes, err := proto.Marshal(testRequest(kind))
	require.NoError(t, err)
	r := MintRequest{Identity: testIdentity(kind), DigestSalt: bytesOf(0x5A, SaltLen), Request: reqBytes, IssuedAt: testEpoch,
		ExpectKnown: true}
	_, err = MintOperationGrant(context.Background(), pki, r)
	require.Error(t, err, "expect_known is refused at grant version 1")
	r.GrantVersion = LedgerGrantVersion
	g, err := MintOperationGrant(context.Background(), pki, r)
	require.NoError(t, err)
	b := &providerv1.GrantBody{}
	require.NoError(t, proto.Unmarshal(g.GetBody(), b))
	assert.True(t, b.GetExpectKnown())

	c := newCall(t, pki, kind)
	c.body.GrantVersion, c.body.ExpectKnown = LedgerGrantVersion, true
	got, err := ledgerVerifier(t, pki).Verify(c.method, c.outer(t, pki))
	require.NoError(t, err)
	assert.True(t, got.Body.GetExpectKnown())
	assert.Len(t, got.GrantDigest, grantverify.DigestLen)
}
