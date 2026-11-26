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

package grantverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Lengths of the host ledger fields of an authority grant.
const (
	// LedgerHeadLen is the length of LedgerPosition.head (a chain head).
	LedgerHeadLen = 32
	// ChallengeNonceLen is the length of an attestation challenge nonce
	// (GrantBody.single_use_nonce of an ATTEST grant).
	ChallengeNonceLen = 16
	// LedgerCheckpointEvery is the interval of the chain checkpoints a
	// ledger checkpoint may name: its seq is a multiple of it.
	LedgerCheckpointEvery = 1024
)

// VerifiedAuthority is an authority grant that passed every rule.
type VerifiedAuthority struct {
	// Body is the decoded GrantBody.
	Body *providerv1.GrantBody
	// GrantDigest is GrantDigest of the grant, which the host's signed
	// answer echoes.
	GrantDigest []byte
}

// authorityKinds maps each HostAuthority method that carries a grant to the
// one grant kind it carries.
var authorityKinds = map[string]providerv1.GrantKind{
	providerv1.HostAuthority_Attest_FullMethodName:          providerv1.GrantKind_GRANT_KIND_ATTEST,
	providerv1.HostAuthority_LedgerMark_FullMethodName:      providerv1.GrantKind_GRANT_KIND_LEDGER_MARK,
	providerv1.HostAuthority_RetireIfUnknown_FullMethodName: providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN,
}

// GrantDigest is SHA-256(be32(len(body)) || body || signature) over g: the
// binding of a host-signed answer to the one delivery of a grant. Each send
// mints a fresh grant and ECDSA signatures are randomised, so it is unique
// per send. It is nil for a nil grant.
func GrantDigest(g *providerv1.OperationGrant) []byte {
	if g == nil {
		return nil
	}
	h := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(g.GetBody()))) // #nosec G115 -- bounded by MaxGrantBodyBytes before it is called on a verified grant
	_, _ = h.Write(n[:])
	_, _ = h.Write(g.GetBody())
	_, _ = h.Write(g.GetSignature())
	return h.Sum(nil)
}

// CheckStrictRequest refuses m, or any message reachable from it through a
// set field, when it carries a field or an enum value its descriptor does
// not define: the strict path of a host authority request. The error is a
// *Refusal.
func CheckStrictRequest(m proto.Message) error {
	if m == nil {
		return mismatch(RuleNotMutatingRequest, "no request")
	}
	if r := walkMessage(m.ProtoReflect(), string(m.ProtoReflect().Descriptor().Name())); r != nil {
		return r
	}
	return nil
}

// VerifyAuthority checks one host authority call that arrived on fullMethod
// with request req (an AttestRequest, LedgerMarkRequest or
// RetireIfUnknownRequest) and returns the decoded grant. Otherwise the error
// is a *Refusal and nothing may be applied.
//
// The rules, in order:
//
//  1. the verifier is configured for LedgerGrantVersion, and fullMethod is a
//     HostAuthority method that carries a grant with the request type of
//     that method (else GRANT_MISMATCH);
//  2. the grant is present and within the size caps, and the request and
//     every message in it carry no unknown fields or enum values;
//  3. the chain and the signature verify as for an operation grant (else
//     GRANT_UNAUTHENTICATED);
//  4. the body decodes, at grant_version LedgerGrantVersion, and its
//     grant_kind is the one fullMethod carries, checked before any other
//     field (else GRANT_MISMATCH);
//  5. the kind's field rules (CheckAttestBody; LEDGER_MARK and
//     RETIRE_IF_UNKNOWN are not implemented and are refused);
//  6. authority_generation, validity, and the installation, Provider, host
//     and enrolment are the verifier's.
//
// clock, when not nil, replaces the verifier's Clock for this call.
func (v *Verifier) VerifyAuthority(fullMethod string, req proto.Message, clock Clock) (*VerifiedAuthority, error) {
	if clock == nil {
		clock = v.clock
	}
	if v.version != LedgerGrantVersion {
		return nil, mismatch(RuleGrantVersion, "this verifier implements no authority grant")
	}
	want, ok := authorityKinds[fullMethod]
	if !ok {
		return nil, mismatch(RuleWrongRPC, "%s carries no authority grant", fullMethod)
	}
	grant, refusal := authorityGrantOf(fullMethod, req)
	if refusal != nil {
		return nil, refusal
	}
	if grant == nil {
		return nil, unauthenticated(RuleGrantMissing, "the call carries no grant")
	}
	if refusal := checkSizes(&providerv1.OperationEnvelope{Grant: grant}); refusal != nil {
		return nil, refusal
	}
	if refusal := walkMessage(req.ProtoReflect(), string(req.ProtoReflect().Descriptor().Name())); refusal != nil {
		return nil, refusal
	}
	now := clock.Now()
	if now.IsZero() {
		return nil, unauthenticated(RuleUntrustedClock, "the verifier's clock is not trustworthy")
	}
	if refusal := v.authenticate(grant, now); refusal != nil {
		return nil, refusal
	}
	body := &providerv1.GrantBody{}
	if err := proto.Unmarshal(grant.GetBody(), body); err != nil {
		return nil, mismatch(RuleMalformedField, "the grant body does not decode")
	}
	if r := walkMessage(body.ProtoReflect(), "GrantBody"); r != nil {
		return nil, r
	}
	if body.GetGrantVersion() != LedgerGrantVersion {
		return nil, mismatch(RuleGrantVersion, "grant_version %d is not implemented", body.GetGrantVersion())
	}
	// The kind first: an authority grant of another kind, or an operation
	// grant, is refused before any field is read as if it were this kind's.
	if body.GetGrantKind() != want {
		return nil, mismatch(RuleGrantKind, "grant_kind %s does not belong to %s", body.GetGrantKind(), fullMethod)
	}
	switch want {
	case providerv1.GrantKind_GRANT_KIND_ATTEST:
		if refusal := CheckAttestBody(body); refusal != nil {
			return nil, refusal
		}
	default:
		return nil, mismatch(RuleGrantKind, "grant_kind %s is not implemented", want)
	}
	if body.GetAuthorityGeneration() < v.minGeneration {
		return nil, unauthenticated(RuleAuthorityGeneration, "authority_generation %d is below %d",
			body.GetAuthorityGeneration(), v.minGeneration)
	}
	if refusal := v.checkValidity(body, now); refusal != nil {
		return nil, refusal
	}
	if !bytes.Equal(body.GetInstallId(), v.installID) || body.GetHostId() != v.hostID ||
		body.GetProviderUid() != v.providerUID || !bytes.Equal(body.GetEnrolmentId(), v.enrolmentID) {
		return nil, mismatch(RuleBinding, "the grant is for another installation, Provider, host or enrolment")
	}
	return &VerifiedAuthority{Body: body, GrantDigest: GrantDigest(grant)}, nil
}

// authorityGrantOf returns the grant of req, which must be the request type
// of fullMethod.
func authorityGrantOf(fullMethod string, req proto.Message) (*providerv1.OperationGrant, *Refusal) {
	switch r := req.(type) {
	case *providerv1.AttestRequest:
		if fullMethod == providerv1.HostAuthority_Attest_FullMethodName {
			return r.GetGrant(), nil
		}
	case *providerv1.LedgerMarkRequest:
		if fullMethod == providerv1.HostAuthority_LedgerMark_FullMethodName {
			return r.GetGrant(), nil
		}
	case *providerv1.RetireIfUnknownRequest:
		if fullMethod == providerv1.HostAuthority_RetireIfUnknown_FullMethodName {
			return r.GetGrant(), nil
		}
	}
	return nil, mismatch(RuleWrongRPC, "the request is not the request type of %s", fullMethod)
}

// checkValidity applies the validity rules of every grant: not expired, not
// issued in the future beyond the skew, and valid for at most
// MaxGrantLifetime.
func (v *Verifier) checkValidity(body *providerv1.GrantBody, now time.Time) *Refusal {
	if !now.Before(body.GetNotAfter().AsTime()) {
		return unauthenticated(RuleValidity, "the grant expired")
	}
	if body.GetIssuedAt().AsTime().After(now.Add(v.maxSkew)) {
		return unauthenticated(RuleValidity, "the grant is issued in the future")
	}
	if body.GetNotAfter().AsTime().Sub(body.GetIssuedAt().AsTime()) > MaxGrantLifetime {
		return unauthenticated(RuleValidity, "the grant is valid for longer than %s", MaxGrantLifetime)
	}
	return nil
}

// CheckAttestBody applies every rule that a GRANT_KIND_ATTEST body at
// LedgerGrantVersion must satisfy on its own: the common fields (1 to 8, 30
// and 31) set, counters at least 1 and lengths exact; single_use_nonce (the
// challenge nonce) of ChallengeNonceLen bytes; ledger set with a 16-byte
// store, a seq of at least 1 and a 32-byte head; ledger_checkpoint, when
// set, in the same store, at a seq of at least 1, a multiple of
// LedgerCheckpointEvery and at most ledger.seq, with a 32-byte head; and
// every other field unset. A minter runs it on every attestation body it
// builds.
func CheckAttestBody(b *providerv1.GrantBody) *Refusal {
	if r := walkMessage(b.ProtoReflect(), "GrantBody"); r != nil {
		return r
	}
	if b.GetGrantVersion() != LedgerGrantVersion {
		return mismatch(RuleGrantVersion, "grant_version %d is not implemented", b.GetGrantVersion())
	}
	if b.GetGrantKind() != providerv1.GrantKind_GRANT_KIND_ATTEST {
		return mismatch(RuleGrantKind, "grant_kind %s is not an attestation grant", b.GetGrantKind())
	}
	if r := checkCommonFields(b); r != nil {
		return r
	}
	// Fields 10 to 18 belong to operation grants.
	if b.GetVmUid() != "" || len(b.GetBindingId()) > 0 || len(b.GetOpId()) > 0 ||
		b.GetKind() != providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED || b.GetAttempt() != 0 ||
		len(b.GetIntentDigest()) > 0 || len(b.GetDigestSalt()) > 0 || b.GetMintEpoch() != nil || b.GetExpectKnown() {
		return mismatch(RuleFieldNotForKind, "an operation field (10-18) is set in an attestation grant")
	}
	// Fields 20 to 23 and 25 belong to administrative grants; 24 is the
	// challenge nonce.
	if b.GetTargetNamespace() != "" || b.GetRequestUid() != "" || b.GetAction() != "" ||
		len(b.GetSpecDigest()) > 0 || b.GetPrecondition() != nil {
		return mismatch(RuleFieldNotForKind, "an administrative field (20-23, 25) is set in an attestation grant")
	}
	if placeholderSet(b) {
		return mismatch(RulePlaceholderSet, "a field set aside (26, 28, 29) is set")
	}
	if len(b.GetAttestationId()) > 0 {
		return mismatch(RuleFieldNotForKind, "attestation_id is set in an attestation grant")
	}
	if len(b.GetSingleUseNonce()) != ChallengeNonceLen {
		return mismatch(RuleMalformedField, "single_use_nonce is %d bytes, not %d", len(b.GetSingleUseNonce()), ChallengeNonceLen)
	}
	l := b.GetLedger()
	if r := checkLedgerPosition(l, "ledger"); r != nil {
		return r
	}
	if cp := b.GetLedgerCheckpoint(); cp != nil {
		if r := checkLedgerPosition(cp, "ledger_checkpoint"); r != nil {
			return r
		}
		switch {
		case !bytes.Equal(cp.GetStoreId(), l.GetStoreId()):
			return mismatch(RuleMalformedField, "ledger_checkpoint names another store than ledger")
		case cp.GetSeq()%LedgerCheckpointEvery != 0:
			return mismatch(RuleMalformedField, "ledger_checkpoint.seq is not a multiple of %d", LedgerCheckpointEvery)
		case cp.GetSeq() > l.GetSeq():
			return mismatch(RuleMalformedField, "ledger_checkpoint.seq is above ledger.seq")
		}
	}
	return nil
}

// checkCommonFields checks the fields every grant kind sets (1 to 8, 30 and
// 31) beyond version and kind.
func checkCommonFields(b *providerv1.GrantBody) *Refusal {
	if b.GetAuthorityGeneration() == 0 {
		return mismatch(RuleCounterZero, "authority_generation is 0")
	}
	if r := checkEpoch(b.GetEpoch(), "epoch"); r != nil {
		return r
	}
	if len(b.GetInstallId()) != IDLen || len(b.GetEnrolmentId()) != IDLen {
		return mismatch(RuleMalformedField, "install_id and enrolment_id must be %d bytes", IDLen)
	}
	if b.GetProviderUid() == "" || b.GetHostId() == "" {
		return mismatch(RuleMalformedField, "provider_uid and host_id are required")
	}
	if b.GetIssuedAt() == nil || b.GetIssuedAt().CheckValid() != nil || b.GetNotAfter() == nil || b.GetNotAfter().CheckValid() != nil {
		return mismatch(RuleMalformedField, "issued_at or not_after is not set or not a valid timestamp")
	}
	if !b.GetIssuedAt().AsTime().Before(b.GetNotAfter().AsTime()) {
		return mismatch(RuleMalformedField, "issued_at is not before not_after")
	}
	return nil
}

// checkLedgerPosition checks a set LedgerPosition: a 16-byte store, a seq of
// at least 1 and a 32-byte head.
func checkLedgerPosition(p *providerv1.LedgerPosition, what string) *Refusal {
	switch {
	case p == nil:
		return mismatch(RuleMalformedField, "%s is not set", what)
	case len(p.GetStoreId()) != IDLen:
		return mismatch(RuleMalformedField, "%s.store_id is %d bytes, not %d", what, len(p.GetStoreId()), IDLen)
	case p.GetSeq() == 0:
		return mismatch(RuleCounterZero, "%s.seq is 0", what)
	case len(p.GetHead()) != LedgerHeadLen:
		return mismatch(RuleMalformedField, "%s.head is %d bytes, not %d", what, len(p.GetHead()), LedgerHeadLen)
	}
	return nil
}
