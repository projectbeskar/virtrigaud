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

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// This file re-exports the verifier and the OperationKind table, which live in
// internal/operation/grantverify so that a binary can link them without the
// Kubernetes API types. Every identifier keeps its name and behaviour; see
// the grantverify package for the documentation of each.

type (
	// Rule names the verifier rule a Refusal enforces.
	Rule = grantverify.Rule
	// Refusal is a verifier's refusal of one call.
	Refusal = grantverify.Refusal
	// Verifier verifies operation grants for one installation, Provider, host
	// and enrolment.
	Verifier = grantverify.Verifier
	// VerifierConfig is what a verifier pins and expects.
	VerifierConfig = grantverify.VerifierConfig
	// Verified is a call that passed every rule.
	Verified = grantverify.Verified
	// KindRow is one row of the OperationKind table.
	KindRow = grantverify.KindRow
	// Clock is the verifier's source of the current time.
	Clock = grantverify.Clock
	// ClockFunc adapts a function to a Clock.
	ClockFunc = grantverify.ClockFunc
	// WallClock is the operating system's wall clock, the default Clock.
	WallClock = grantverify.WallClock
)

// Verifier rules, as carried by Refusal.Rule.
const (
	// RuleEnvelopeMissing is grantverify.RuleEnvelopeMissing.
	RuleEnvelopeMissing = grantverify.RuleEnvelopeMissing
	// RuleOuterFieldSet is grantverify.RuleOuterFieldSet.
	RuleOuterFieldSet = grantverify.RuleOuterFieldSet
	// RuleNotMutatingRequest is grantverify.RuleNotMutatingRequest.
	RuleNotMutatingRequest = grantverify.RuleNotMutatingRequest
	// RuleGrantMissing is grantverify.RuleGrantMissing.
	RuleGrantMissing = grantverify.RuleGrantMissing
	// RuleChain is grantverify.RuleChain.
	RuleChain = grantverify.RuleChain
	// RuleSignature is grantverify.RuleSignature.
	RuleSignature = grantverify.RuleSignature
	// RuleAuthorityGeneration is grantverify.RuleAuthorityGeneration.
	RuleAuthorityGeneration = grantverify.RuleAuthorityGeneration
	// RuleValidity is grantverify.RuleValidity.
	RuleValidity = grantverify.RuleValidity
	// RuleUntrustedClock is grantverify.RuleUntrustedClock.
	RuleUntrustedClock = grantverify.RuleUntrustedClock
	// RuleUnknownField is grantverify.RuleUnknownField.
	RuleUnknownField = grantverify.RuleUnknownField
	// RuleUnknownEnum is grantverify.RuleUnknownEnum.
	RuleUnknownEnum = grantverify.RuleUnknownEnum
	// RuleUnspecifiedEnum is grantverify.RuleUnspecifiedEnum.
	RuleUnspecifiedEnum = grantverify.RuleUnspecifiedEnum
	// RuleGrantVersion is grantverify.RuleGrantVersion.
	RuleGrantVersion = grantverify.RuleGrantVersion
	// RuleGrantKind is grantverify.RuleGrantKind.
	RuleGrantKind = grantverify.RuleGrantKind
	// RulePlaceholderSet is grantverify.RulePlaceholderSet.
	RulePlaceholderSet = grantverify.RulePlaceholderSet
	// RuleFieldNotForKind is grantverify.RuleFieldNotForKind.
	RuleFieldNotForKind = grantverify.RuleFieldNotForKind
	// RuleCounterZero is grantverify.RuleCounterZero.
	RuleCounterZero = grantverify.RuleCounterZero
	// RuleMalformedField is grantverify.RuleMalformedField.
	RuleMalformedField = grantverify.RuleMalformedField
	// RuleBinding is grantverify.RuleBinding.
	RuleBinding = grantverify.RuleBinding
	// RuleWrongRPC is grantverify.RuleWrongRPC.
	RuleWrongRPC = grantverify.RuleWrongRPC
	// RuleMalformedRequest is grantverify.RuleMalformedRequest.
	RuleMalformedRequest = grantverify.RuleMalformedRequest
	// RuleInnerEnvelope is grantverify.RuleInnerEnvelope.
	RuleInnerEnvelope = grantverify.RuleInnerEnvelope
	// RuleFixedField is grantverify.RuleFixedField.
	RuleFixedField = grantverify.RuleFixedField
	// RuleIntentDigest is grantverify.RuleIntentDigest.
	RuleIntentDigest = grantverify.RuleIntentDigest
	// RuleSizeLimit is grantverify.RuleSizeLimit.
	RuleSizeLimit = grantverify.RuleSizeLimit
)

// Grant format constants, as defined by grantverify.
const (
	// GrantVersion is the only grant_version this package mints and verifies.
	GrantVersion = grantverify.GrantVersion
	// IDLen is the length of install_id, enrolment_id, binding_id, op_id and
	// Epoch.holder.
	IDLen = grantverify.IDLen
	// SaltLen is the length of the digest_salt this package draws.
	SaltLen = grantverify.SaltLen
	// MinSaltLen is the shortest digest_salt the verifier accepts.
	MinSaltLen = grantverify.MinSaltLen
	// DigestLen is the length of intent_digest (SHA-256).
	DigestLen = grantverify.DigestLen
	// MaxGrantBodyBytes is the largest serialised GrantBody accepted.
	MaxGrantBodyBytes = grantverify.MaxGrantBodyBytes
	// MaxCertBytes is the largest DER certificate accepted in the chain.
	MaxCertBytes = grantverify.MaxCertBytes
	// MaxSignatureBytes is the largest signature accepted.
	MaxSignatureBytes = grantverify.MaxSignatureBytes
	// MaxRequestBytes is the largest serialised request accepted.
	MaxRequestBytes = grantverify.MaxRequestBytes
	// MaxGrantLifetime is the longest validity this package mints or accepts.
	MaxGrantLifetime = grantverify.MaxGrantLifetime
	// DefaultMaxClockSkew is how far in the future issued_at may be by the
	// verifier's clock when VerifierConfig.MaxClockSkew is 0.
	DefaultMaxClockSkew = grantverify.DefaultMaxClockSkew
)

// NewVerifier returns a verifier for cfg; see grantverify.NewVerifier.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) { return grantverify.NewVerifier(cfg) }

// AsRefusal returns the Refusal in err's chain, if any.
func AsRefusal(err error) (*Refusal, bool) { return grantverify.AsRefusal(err) }

// Rows returns a copy of the OperationKind table, in kind order.
func Rows() []KindRow { return grantverify.Rows() }

// RowFor returns the table row of kind; ok is false for an unspecified or
// unimplemented kind.
func RowFor(kind providerv1.OperationKind) (row KindRow, ok bool) { return grantverify.RowFor(kind) }

// KindFor returns the operation kind of a request message that arrives on
// fullMethod; see grantverify.KindFor.
func KindFor(fullMethod string, req proto.Message) (providerv1.OperationKind, error) {
	return grantverify.KindFor(fullMethod, req)
}

// IntentDigest is SHA-256(salt || request), the GrantBody.intent_digest over
// the exact request bytes.
func IntentDigest(salt, request []byte) []byte { return grantverify.IntentDigest(salt, request) }

// DecodeRequest decodes requestBytes as the request type kind names and
// applies the verifier's request rules; see grantverify.DecodeRequest.
func DecodeRequest(kind providerv1.OperationKind, requestBytes []byte) (proto.Message, error) {
	return grantverify.DecodeRequest(kind, requestBytes)
}
