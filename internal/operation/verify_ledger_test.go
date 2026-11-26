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
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ledgerVerifier verifies for pki at testEpoch at the host ledger grant
// version.
func ledgerVerifier(t testing.TB, pki *testPKI) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{
		Anchors:      []*x509.Certificate{pki.anchor},
		InstallID:    pki.installID,
		ProviderUID:  testProviderUID,
		HostID:       testHostID,
		EnrolmentID:  testEnrolment,
		Now:          func() time.Time { return testEpoch },
		GrantVersion: LedgerGrantVersion,
	})
	require.NoError(t, err)
	return v
}

func testAttestMint() AttestMintRequest {
	return AttestMintRequest{
		ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment,
		Epoch:          Epoch{Counter: 1, Holder: bytesOf(0xC1, IDLen)},
		ChallengeNonce: bytesOf(0xA7, 16),
		Ledger:         LedgerPosition{StoreID: bytesOf(0x5E, IDLen), Seq: 3000, Head: bytesOf(0x4E, 32)},
		IssuedAt:       testEpoch,
	}
}

func TestNewVerifier_GrantVersion(t *testing.T) {
	pki := validPKI(t)
	_, err := NewVerifier(VerifierConfig{Anchors: []*x509.Certificate{pki.anchor}, InstallID: pki.installID,
		ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment, GrantVersion: 3})
	require.Error(t, err, "an unimplemented grant version is refused")
}

// A verifier at the ledger grant version accepts a version 2 operation
// grant and refuses version 1; a version 1 verifier refuses version 2.
func TestVerify_OperationAtLedgerVersion(t *testing.T) {
	pki := validPKI(t)
	v1, v2 := testVerifier(t, pki), ledgerVerifier(t, pki)
	kind := providerv1.OperationKind_OPERATION_KIND_CREATE

	c := newCall(t, pki, kind)
	_, err := v2.Verify(c.method, c.outer(t, pki))
	assert.Equal(t, RuleGrantVersion, ruleOf(t, err), "a version 2 verifier refuses a version 1 grant")

	c = newCall(t, pki, kind)
	c.body.GrantVersion = LedgerGrantVersion
	got, err := v2.Verify(c.method, c.outer(t, pki))
	require.NoError(t, err)
	assert.Equal(t, LedgerGrantVersion, got.Body.GetGrantVersion())
	_, err = v1.Verify(c.method, c.outer(t, pki))
	assert.Equal(t, RuleGrantVersion, ruleOf(t, err), "a version 1 verifier refuses a version 2 grant")

	// The ledger fields stay unset in an operation grant at version 2.
	for name, set := range map[string]func(*providerv1.GrantBody){
		"ledger":            func(b *providerv1.GrantBody) { b.Ledger = &providerv1.LedgerPosition{} },
		"ledger_checkpoint": func(b *providerv1.GrantBody) { b.LedgerCheckpoint = &providerv1.LedgerPosition{} },
		"attestation_id":    func(b *providerv1.GrantBody) { b.AttestationId = bytesOf(1, 16) },
	} {
		c := newCall(t, pki, kind)
		c.body.GrantVersion = LedgerGrantVersion
		set(c.body)
		_, err := v2.Verify(c.method, c.outer(t, pki))
		assert.Equal(t, RuleFieldNotForKind, ruleOf(t, err), name)
	}

	// An attestation grant in an operation envelope is refused on its kind.
	c = newCall(t, pki, kind)
	c.body.GrantVersion, c.body.GrantKind = LedgerGrantVersion, providerv1.GrantKind_GRANT_KIND_ATTEST
	_, err = v2.Verify(c.method, c.outer(t, pki))
	assert.Equal(t, RuleGrantKind, ruleOf(t, err))
}

func TestMintOperationGrant_LedgerVersion(t *testing.T) {
	pki := validPKI(t)
	kind := providerv1.OperationKind_OPERATION_KIND_CREATE
	reqBytes, err := proto.Marshal(testRequest(kind))
	require.NoError(t, err)
	g, err := MintOperationGrant(context.Background(), pki, MintRequest{Identity: testIdentity(kind), DigestSalt: bytesOf(0x5A, SaltLen),
		Request: reqBytes, IssuedAt: testEpoch, GrantVersion: LedgerGrantVersion})
	require.NoError(t, err)
	b := &providerv1.GrantBody{}
	require.NoError(t, proto.Unmarshal(g.GetBody(), b))
	assert.Equal(t, LedgerGrantVersion, b.GetGrantVersion())
	_, err = MintOperationGrant(context.Background(), pki, MintRequest{Identity: testIdentity(kind), DigestSalt: bytesOf(0x5A, SaltLen),
		Request: reqBytes, IssuedAt: testEpoch, GrantVersion: 9})
	require.Error(t, err)
}

func attestCall(t testing.TB, pki *testPKI, edit func(*providerv1.GrantBody)) *providerv1.AttestRequest {
	t.Helper()
	g, err := MintAttestGrant(context.Background(), pki, testAttestMint())
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
	return &providerv1.AttestRequest{TargetHostId: testHostID, AnswerNonce: bytesOf(0x11, 16), Grant: g}
}

func TestVerifyAuthority_Attest(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	req := attestCall(t, pki, nil)
	got, err := v.VerifyAuthority(providerv1.HostAuthority_Attest_FullMethodName, req, nil)
	require.NoError(t, err)
	assert.Equal(t, providerv1.GrantKind_GRANT_KIND_ATTEST, got.Body.GetGrantKind())
	assert.Equal(t, uint64(3000), got.Body.GetLedger().GetSeq())

	// The grant digest is SHA-256(be32(len(body)) || body || signature).
	h := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(req.GetGrant().GetBody())))
	h.Write(n[:])
	h.Write(req.GetGrant().GetBody())
	h.Write(req.GetGrant().GetSignature())
	assert.Equal(t, h.Sum(nil), got.GrantDigest)
	assert.Nil(t, grantverify.GrantDigest(nil))
}

func TestVerifyAuthority_Refusals(t *testing.T) {
	pki := validPKI(t)
	v := ledgerVerifier(t, pki)
	attest := providerv1.HostAuthority_Attest_FullMethodName
	cases := map[string]struct {
		edit   func(*providerv1.GrantBody)
		method string
		req    func(*providerv1.AttestRequest) proto.Message
		rule   Rule
	}{
		"version 1":           {edit: func(b *providerv1.GrantBody) { b.GrantVersion = 1 }, rule: RuleGrantVersion},
		"operation kind":      {edit: func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_OPERATION }, rule: RuleGrantKind},
		"ledger mark kind":    {edit: func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_LEDGER_MARK }, rule: RuleGrantKind},
		"no nonce":            {edit: func(b *providerv1.GrantBody) { b.SingleUseNonce = nil }, rule: RuleMalformedField},
		"short nonce":         {edit: func(b *providerv1.GrantBody) { b.SingleUseNonce = bytesOf(1, 8) }, rule: RuleMalformedField},
		"no ledger":           {edit: func(b *providerv1.GrantBody) { b.Ledger = nil }, rule: RuleMalformedField},
		"ledger seq 0":        {edit: func(b *providerv1.GrantBody) { b.Ledger.Seq = 0 }, rule: RuleCounterZero},
		"short head":          {edit: func(b *providerv1.GrantBody) { b.Ledger.Head = bytesOf(1, 31) }, rule: RuleMalformedField},
		"short store":         {edit: func(b *providerv1.GrantBody) { b.Ledger.StoreId = bytesOf(1, 15) }, rule: RuleMalformedField},
		"attestation id set":  {edit: func(b *providerv1.GrantBody) { b.AttestationId = bytesOf(1, 16) }, rule: RuleFieldNotForKind},
		"operation field set": {edit: func(b *providerv1.GrantBody) { b.OpId = bytesOf(1, 16) }, rule: RuleFieldNotForKind},
		"expect_known set":    {edit: func(b *providerv1.GrantBody) { b.ExpectKnown = true }, rule: RuleFieldNotForKind},
		"admin field set":     {edit: func(b *providerv1.GrantBody) { b.Action = "x" }, rule: RuleFieldNotForKind},
		"placeholder set":     {edit: func(b *providerv1.GrantBody) { b.EalSeq = 1 }, rule: RulePlaceholderSet},
		"generation 0":        {edit: func(b *providerv1.GrantBody) { b.AuthorityGeneration = 0 }, rule: RuleCounterZero},
		"another enrolment":   {edit: func(b *providerv1.GrantBody) { b.EnrolmentId = bytesOf(9, 16) }, rule: RuleBinding},
		"another Provider":    {edit: func(b *providerv1.GrantBody) { b.ProviderUid = "other" }, rule: RuleBinding},
		"another host":        {edit: func(b *providerv1.GrantBody) { b.HostId = "host-b" }, rule: RuleBinding},
		"expired": {edit: func(b *providerv1.GrantBody) {
			b.NotAfter.Seconds = testEpoch.Unix() - 1
			b.IssuedAt.Seconds = testEpoch.Unix() - 10
		}, rule: RuleValidity},
		"checkpoint not 1024": {edit: func(b *providerv1.GrantBody) { b.LedgerCheckpoint = cpAt(b, 1000) }, rule: RuleMalformedField},
		"checkpoint above":    {edit: func(b *providerv1.GrantBody) { b.LedgerCheckpoint = cpAt(b, 4096) }, rule: RuleMalformedField},
		"checkpoint at 0":     {edit: func(b *providerv1.GrantBody) { b.LedgerCheckpoint = cpAt(b, 0) }, rule: RuleCounterZero},
		"checkpoint elsewhere": {edit: func(b *providerv1.GrantBody) {
			b.LedgerCheckpoint = cpAt(b, 2048)
			b.LedgerCheckpoint.StoreId = bytesOf(2, 16)
		}, rule: RuleMalformedField},
		"wrong method": {method: providerv1.HostAuthority_LedgerMark_FullMethodName,
			req: func(r *providerv1.AttestRequest) proto.Message { return r }, rule: RuleWrongRPC},
		"attest grant on ledger mark": {method: providerv1.HostAuthority_LedgerMark_FullMethodName,
			req: func(r *providerv1.AttestRequest) proto.Message {
				return &providerv1.LedgerMarkRequest{TargetHostId: r.GetTargetHostId(), AnswerNonce: r.GetAnswerNonce(), Grant: r.GetGrant()}
			}, rule: RuleGrantKind},
		"challenge method": {method: providerv1.HostAuthority_Challenge_FullMethodName, rule: RuleWrongRPC},
		"no grant":         {req: func(r *providerv1.AttestRequest) proto.Message { r.Grant = nil; return r }, rule: RuleGrantMissing},
		"unknown request field": {req: func(r *providerv1.AttestRequest) proto.Message {
			r.ProtoReflect().SetUnknown(unknownField())
			return r
		}, rule: RuleUnknownField},
		"unknown grant field": {req: func(r *providerv1.AttestRequest) proto.Message {
			r.Grant.ProtoReflect().SetUnknown(unknownField())
			return r
		}, rule: RuleUnknownField},
		"oversized body": {req: func(r *providerv1.AttestRequest) proto.Message {
			r.Grant.Body = make([]byte, grantverify.MaxGrantBodyBytes+1)
			return r
		}, rule: RuleSizeLimit},
		"bad signature": {req: func(r *providerv1.AttestRequest) proto.Message {
			r.Grant.Body = append([]byte(nil), r.Grant.Body...)
			r.Grant.Body[len(r.Grant.Body)-1] ^= 1
			return r
		}, rule: RuleSignature},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := attestCall(t, pki, c.edit)
			var req proto.Message = r
			if c.req != nil {
				req = c.req(r)
			}
			m := attest
			if c.method != "" {
				m = c.method
			}
			_, err := v.VerifyAuthority(m, req, nil)
			assert.Equal(t, c.rule, ruleOf(t, err))
		})
	}
	// A version 1 verifier verifies no authority grant.
	_, err := testVerifier(t, pki).VerifyAuthority(attest, attestCall(t, pki, nil), nil)
	assert.Equal(t, RuleGrantVersion, ruleOf(t, err))
	// An untrusted clock refuses before the grant is judged.
	_, err = v.VerifyAuthority(attest, attestCall(t, pki, nil), grantverify.ClockFunc(func() time.Time { return time.Time{} }))
	assert.Equal(t, RuleUntrustedClock, ruleOf(t, err))
}

func cpAt(b *providerv1.GrantBody, seq uint64) *providerv1.LedgerPosition {
	return &providerv1.LedgerPosition{StoreId: append([]byte(nil), b.GetLedger().GetStoreId()...), Seq: seq, Head: bytesOf(0x77, 32)}
}

func TestMintAttestGrant_Refusals(t *testing.T) {
	pki := validPKI(t)
	ctx := context.Background()
	r := testAttestMint()
	r.ChallengeNonce = nil
	_, err := MintAttestGrant(ctx, pki, r)
	require.Error(t, err)
	r = testAttestMint()
	r.IssuedAt = time.Time{}
	_, err = MintAttestGrant(ctx, pki, r)
	require.Error(t, err)
	r = testAttestMint()
	r.Lifetime = 2 * time.Hour
	_, err = MintAttestGrant(ctx, pki, r)
	require.Error(t, err)
	_, err = MintAttestGrant(ctx, nil, testAttestMint())
	require.ErrorIs(t, err, ErrNoSigner)
	r = testAttestMint()
	r.Checkpoint = &LedgerPosition{StoreID: r.Ledger.StoreID, Seq: 2048, Head: bytesOf(0x33, 32)}
	_, err = MintAttestGrant(ctx, pki, r)
	require.NoError(t, err)
}

func TestCheckStrictRequest(t *testing.T) {
	ok := &providerv1.ChallengeRequest{TargetHostId: testHostID, AnswerNonce: bytesOf(1, 16), Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST}
	require.NoError(t, grantverify.CheckStrictRequest(ok))
	bad := proto.Clone(ok).(*providerv1.ChallengeRequest)
	bad.ProtoReflect().SetUnknown(unknownField())
	assert.Equal(t, RuleUnknownField, ruleOf(t, grantverify.CheckStrictRequest(bad)))
	enum := proto.Clone(ok).(*providerv1.ChallengeRequest)
	enum.Purpose = 9
	assert.Equal(t, RuleUnknownEnum, ruleOf(t, grantverify.CheckStrictRequest(enum)))
	assert.Equal(t, RuleNotMutatingRequest, ruleOf(t, grantverify.CheckStrictRequest(nil)))
}

func ruleOf(t testing.TB, err error) Rule {
	t.Helper()
	r, ok := grantverify.AsRefusal(err)
	if !ok {
		t.Fatalf("not a refusal: %v", err)
	}
	return r.Rule
}
