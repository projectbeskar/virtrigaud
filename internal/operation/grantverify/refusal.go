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
	"errors"
	"fmt"
)

// The ErrorInfo reasons a Refusal carries. They are plain strings, not the
// internal/providers/contracts constants, so that this package (and a binary
// that links only it) imports nothing of the API or contract packages;
// reasons_test.go pins each to its contracts value.
const (
	// ReasonGrantUnauthenticated is the reason of a refusal on signature,
	// chain, validity or generation.
	ReasonGrantUnauthenticated = "GRANT_UNAUTHENTICATED"
	// ReasonGrantMismatch is the reason of a refusal because the grant does
	// not fit the call or the verifier's version.
	ReasonGrantMismatch = "GRANT_MISMATCH"
	// ReasonIdentityRequired is the reason of a refusal because the call
	// carries no operation envelope.
	ReasonIdentityRequired = "IDENTITY_REQUIRED"
)

// Rule names the verifier rule a Refusal enforces. Tests and callers match on
// it; its text is stable.
type Rule string

const (
	// RuleEnvelopeMissing: the call carried no operation_envelope.
	RuleEnvelopeMissing Rule = "envelope_missing"
	// RuleOuterFieldSet: a field of the outer request other than
	// operation_envelope is set, or the outer request carries unknown fields.
	RuleOuterFieldSet Rule = "outer_field_set"
	// RuleNotMutatingRequest: the outer request type has no operation_envelope
	// field.
	RuleNotMutatingRequest Rule = "not_mutating_request"
	// RuleGrantMissing: the envelope carried no grant.
	RuleGrantMissing Rule = "grant_missing"
	// RuleChain: the certificate chain is not exactly a leaf and a path
	// length 0 issuer that chain to a pinned anchor, is not valid now, or a
	// certificate's key is not ECDSA P-256.
	RuleChain Rule = "chain"
	// RuleSignature: the signature over the body does not verify.
	RuleSignature Rule = "signature"
	// RuleAuthorityGeneration: authority_generation is below the verifier's
	// minimum.
	RuleAuthorityGeneration Rule = "authority_generation"
	// RuleValidity: the grant is expired, issued in the future, or valid for
	// longer than the verifier allows.
	RuleValidity Rule = "validity"
	// RuleUntrustedClock: the verifier's clock could not vouch for its
	// reading (it returned the zero time); nothing about the grant was
	// judged. The reason is GRANT_UNAUTHENTICATED; a caller that keeps a
	// durable record of what it accepted (a per-host agent) answers it as
	// its own non-definite refusal instead.
	RuleUntrustedClock Rule = "untrusted_clock"
	// RuleUnknownField: a message on the envelope path carries a field the
	// verifier does not know.
	RuleUnknownField Rule = "unknown_field"
	// RuleUnknownEnum: an enum value the verifier does not implement.
	RuleUnknownEnum Rule = "unknown_enum"
	// RuleUnspecifiedEnum: grant_kind or kind is UNSPECIFIED.
	RuleUnspecifiedEnum Rule = "unspecified_enum"
	// RuleGrantVersion: grant_version is 0 or one the verifier does not
	// implement.
	RuleGrantVersion Rule = "grant_version"
	// RuleGrantKind: a grant kind this verifier does not issue or accept (only
	// GRANT_KIND_OPERATION is implemented).
	RuleGrantKind Rule = "grant_kind"
	// RulePlaceholderSet: a field set aside at version 1 (26, 28, 29) is set.
	RulePlaceholderSet Rule = "placeholder_set"
	// RuleFieldNotForKind: a field the grant kind does not use is set.
	RuleFieldNotForKind Rule = "field_not_for_kind"
	// RuleCounterZero: a counter the grant kind needs is 0 (counters count
	// from 1).
	RuleCounterZero Rule = "counter_zero"
	// RuleMalformedField: a required field is empty or has the wrong length.
	RuleMalformedField Rule = "malformed_field"
	// RuleBinding: install_id, provider_uid, host_id or enrolment_id is not
	// the verifier's.
	RuleBinding Rule = "binding"
	// RuleWrongRPC: the kind names another RPC or request type than the call.
	RuleWrongRPC Rule = "wrong_rpc"
	// RuleMalformedRequest: the request bytes do not decode as the kind's
	// request type.
	RuleMalformedRequest Rule = "malformed_request"
	// RuleInnerEnvelope: the decoded request itself carries an envelope.
	RuleInnerEnvelope Rule = "inner_envelope"
	// RuleFixedField: the decoded request disagrees with the kind's fixed
	// field (PowerRequest.op).
	RuleFixedField Rule = "fixed_field"
	// RuleIntentDigest: intent_digest is not SHA-256(digest_salt || request).
	RuleIntentDigest Rule = "intent_digest"
	// RuleSizeLimit: the body, the signature, a certificate or the request
	// is larger than the verifier parses.
	RuleSizeLimit Rule = "size_limit"
)

// Refusal is a verifier's refusal of one call: the call is not performed.
// Reason is the google.rpc.ErrorInfo reason an agent answers with
// (ReasonGrantUnauthenticated, ReasonGrantMismatch or
// ReasonIdentityRequired). InvalidArgument is set for a request
// whose outer fields are set beside the envelope, which an agent refuses with
// INVALID_ARGUMENT. A Refusal never carries request bytes or key material.
type Refusal struct {
	// Reason is the ErrorInfo reason of the refusal.
	Reason string
	// Rule is the verifier rule that refused the call.
	Rule Rule
	// InvalidArgument is set when the refusal is an INVALID_ARGUMENT answer.
	InvalidArgument bool
	// detail is a short, secret-free description.
	detail string
}

// Error implements error.
func (r *Refusal) Error() string {
	return fmt.Sprintf("operation grant refused (%s, rule %s): %s", r.Reason, r.Rule, r.detail)
}

// AsRefusal returns the Refusal in err's chain, if any.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// mismatch is a GRANT_MISMATCH refusal: the grant does not fit this call or
// this verifier's version.
func mismatch(rule Rule, format string, args ...any) *Refusal {
	return &Refusal{Reason: ReasonGrantMismatch, Rule: rule, detail: fmt.Sprintf(format, args...)}
}

// unauthenticated is a GRANT_UNAUTHENTICATED refusal: signature, chain,
// validity or generation.
func unauthenticated(rule Rule, format string, args ...any) *Refusal {
	return &Refusal{Reason: ReasonGrantUnauthenticated, Rule: rule, detail: fmt.Sprintf(format, args...)}
}
