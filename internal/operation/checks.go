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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// GrantVersion is the only grant_version this package mints and verifies.
const GrantVersion uint32 = 1

// Field lengths, in bytes, of the random identifiers and digests of a version
// 1 OPERATION grant.
const (
	// IDLen is the length of install_id, enrolment_id, binding_id, op_id and
	// Epoch.holder.
	IDLen = 16
	// SaltLen is the length of the digest_salt this package draws.
	SaltLen = 32
	// MinSaltLen is the shortest digest_salt the verifier accepts.
	MinSaltLen = 16
	// DigestLen is the length of intent_digest (SHA-256).
	DigestLen = 32
)

// envelopeFieldName is the name of the operation_envelope field on every
// mutating request message.
const envelopeFieldName protoreflect.Name = "operation_envelope"

// walkMessage refuses m, or any message reachable from it through a set field,
// when it carries unknown fields or an enum value its descriptor does not
// define. what names m in the refusal.
func walkMessage(m protoreflect.Message, what string) *Refusal {
	if len(m.GetUnknown()) > 0 {
		return mismatch(RuleUnknownField, "%s carries unknown fields", what)
	}
	var refusal *Refusal
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		refusal = walkValue(fd, v, what+"."+string(fd.Name()))
		return refusal == nil
	})
	return refusal
}

// walkValue applies walkMessage's rules to one set field.
func walkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, what string) *Refusal {
	switch {
	case fd.IsList():
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			if r := walkSingular(fd, l.Get(i), what); r != nil {
				return r
			}
		}
	case fd.IsMap():
		var refusal *Refusal
		v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
			refusal = walkSingular(fd.MapValue(), mv, what)
			return refusal == nil
		})
		return refusal
	default:
		return walkSingular(fd, v, what)
	}
	return nil
}

// walkSingular checks one value of a singular, list or map-value field.
func walkSingular(fd protoreflect.FieldDescriptor, v protoreflect.Value, what string) *Refusal {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return walkMessage(v.Message(), what)
	case protoreflect.EnumKind:
		if fd.Enum().Values().ByNumber(v.Enum()) == nil {
			return mismatch(RuleUnknownEnum, "%s has enum value %d, which is not implemented", what, v.Enum())
		}
	}
	return nil
}

// checkEpoch refuses a missing epoch, a zero counter or a holder of the wrong
// length.
func checkEpoch(e *providerv1.Epoch, what string) *Refusal {
	if e == nil {
		return mismatch(RuleMalformedField, "%s is not set", what)
	}
	if e.GetCounter() == 0 {
		return mismatch(RuleCounterZero, "%s.counter is 0", what)
	}
	if len(e.GetHolder()) != IDLen {
		return mismatch(RuleMalformedField, "%s.holder is %d bytes, not %d", what, len(e.GetHolder()), IDLen)
	}
	return nil
}

// checkOperationBody applies every rule that a version 1 OPERATION grant body
// must satisfy on its own, independent of the call it arrives with and of the
// verifier's clock and expectations. The minter runs it on every body it
// builds, so a minted grant always passes it.
func checkOperationBody(b *providerv1.GrantBody) *Refusal {
	if r := walkMessage(b.ProtoReflect(), "GrantBody"); r != nil {
		return r
	}
	if b.GetGrantVersion() == 0 {
		return mismatch(RuleGrantVersion, "grant_version is 0")
	}
	if b.GetGrantVersion() != GrantVersion {
		return mismatch(RuleGrantVersion, "grant_version %d is not implemented", b.GetGrantVersion())
	}
	if b.GetGrantKind() == providerv1.GrantKind_GRANT_KIND_UNSPECIFIED {
		return mismatch(RuleUnspecifiedEnum, "grant_kind is UNSPECIFIED")
	}
	if b.GetGrantKind() != providerv1.GrantKind_GRANT_KIND_OPERATION {
		return mismatch(RuleGrantKind, "grant_kind %s is not issuable at grant_version %d", b.GetGrantKind(), GrantVersion)
	}
	if b.GetKind() == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED {
		return mismatch(RuleUnspecifiedEnum, "kind is UNSPECIFIED")
	}
	if _, ok := RowFor(b.GetKind()); !ok {
		return mismatch(RuleUnknownEnum, "kind %d is not implemented", b.GetKind())
	}
	// Fields 26, 28 and 29 are unset at version 1.
	if b.GetEalSeq() != 0 || b.GetResetFloor() != nil || len(b.GetCeremonyRecordDigest()) > 0 {
		return mismatch(RulePlaceholderSet, "a field set aside at grant_version 1 (26, 28, 29) is set")
	}
	// Fields 20 to 25 belong to ADMIN and ENROL grants.
	if b.GetTargetNamespace() != "" || b.GetRequestUid() != "" || b.GetAction() != "" ||
		len(b.GetSpecDigest()) > 0 || len(b.GetSingleUseNonce()) > 0 || b.GetPrecondition() != nil {
		return mismatch(RuleFieldNotForKind, "an administrative field (20-25) is set in an OPERATION grant")
	}
	if b.GetAuthorityGeneration() == 0 {
		return mismatch(RuleCounterZero, "authority_generation is 0")
	}
	if b.GetAttempt() == 0 {
		return mismatch(RuleCounterZero, "attempt is 0")
	}
	if r := checkEpoch(b.GetEpoch(), "epoch"); r != nil {
		return r
	}
	if r := checkEpoch(b.GetMintEpoch(), "mint_epoch"); r != nil {
		return r
	}
	for _, f := range []struct {
		name string
		got  []byte
	}{
		{"install_id", b.GetInstallId()},
		{"enrolment_id", b.GetEnrolmentId()},
		{"binding_id", b.GetBindingId()},
		{"op_id", b.GetOpId()},
	} {
		if len(f.got) != IDLen {
			return mismatch(RuleMalformedField, "%s is %d bytes, not %d", f.name, len(f.got), IDLen)
		}
	}
	if len(b.GetDigestSalt()) < MinSaltLen {
		return mismatch(RuleMalformedField, "digest_salt is %d bytes, fewer than %d", len(b.GetDigestSalt()), MinSaltLen)
	}
	if len(b.GetIntentDigest()) != DigestLen {
		return mismatch(RuleMalformedField, "intent_digest is %d bytes, not %d", len(b.GetIntentDigest()), DigestLen)
	}
	for _, f := range []struct{ name, got string }{
		{"provider_uid", b.GetProviderUid()},
		{"host_id", b.GetHostId()},
		{"vm_uid", b.GetVmUid()},
	} {
		if f.got == "" {
			return mismatch(RuleMalformedField, "%s is empty", f.name)
		}
	}
	for _, ts := range []struct {
		name string
		set  bool
		err  error
	}{
		{"issued_at", b.GetIssuedAt() != nil, b.GetIssuedAt().CheckValid()},
		{"not_after", b.GetNotAfter() != nil, b.GetNotAfter().CheckValid()},
	} {
		if !ts.set || ts.err != nil {
			return mismatch(RuleMalformedField, "%s is not set or not a valid timestamp", ts.name)
		}
	}
	if !b.GetIssuedAt().AsTime().Before(b.GetNotAfter().AsTime()) {
		return mismatch(RuleMalformedField, "issued_at is not before not_after")
	}
	return nil
}

// decodeRequest decodes requestBytes as the request type row names and
// applies the request rules: no unknown fields or enum values anywhere in it,
// no operation_envelope of its own, and the kind's fixed field.
func decodeRequest(row KindRow, requestBytes []byte) (proto.Message, *Refusal) {
	m := row.NewRequest()
	if err := proto.Unmarshal(requestBytes, m); err != nil {
		// The decode error can quote request content; it is not returned.
		return nil, mismatch(RuleMalformedRequest, "request does not decode as %s", row.RequestType())
	}
	if fd := m.ProtoReflect().Descriptor().Fields().ByName(envelopeFieldName); fd != nil && m.ProtoReflect().Has(fd) {
		return nil, mismatch(RuleInnerEnvelope, "the decoded %s carries an operation_envelope", row.RequestType())
	}
	if r := walkMessage(m.ProtoReflect(), string(row.RequestType())); r != nil {
		return nil, r
	}
	if row.FixedPowerOp != providerv1.PowerOp_POWER_OP_UNSPECIFIED {
		pr, ok := m.(*providerv1.PowerRequest)
		if !ok || pr.GetOp() != row.FixedPowerOp {
			return nil, mismatch(RuleFixedField, "kind %s needs op %s", row.Kind, row.FixedPowerOp)
		}
	}
	return m, nil
}
