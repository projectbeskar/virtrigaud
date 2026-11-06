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
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Test expectations of the verifier.
var (
	testProviderUID = "provider-uid-1"
	testHostID      = "host-a"
	testEnrolment   = bytesOf(0xE1, IDLen)
)

// unknownField is the encoding of field 99 (varint 1), unknown to every
// message on the envelope path.
func unknownField() []byte {
	b := protowire.AppendTag(nil, 99, protowire.VarintType)
	return protowire.AppendVarint(b, 1)
}

// testIdentity is a valid identity for kind.
func testIdentity(kind providerv1.OperationKind) Identity {
	return Identity{
		ProviderUID: testProviderUID,
		HostID:      testHostID,
		EnrolmentID: testEnrolment,
		Epoch:       Epoch{Counter: 1, Holder: bytesOf(0xC1, IDLen)},
		VMUID:       "vm-uid-1",
		BindingID:   bytesOf(0xB1, IDLen),
		OpID:        bytesOf(0x01, IDLen),
		Kind:        kind,
		Attempt:     1,
		MintEpoch:   Epoch{Counter: 1, Holder: bytesOf(0xC1, IDLen)},
	}
}

// testRequest is a valid inner request for kind.
func testRequest(kind providerv1.OperationKind) proto.Message {
	row, _ := RowFor(kind)
	m := row.NewRequest()
	switch r := m.(type) {
	case *providerv1.CreateRequest:
		r.Name, r.TargetHostId = "vm-1", testHostID
	case *providerv1.DeleteRequest:
		r.Id, r.TargetHostId = "vm-1", testHostID
	case *providerv1.PowerRequest:
		r.Id, r.Op, r.TargetHostId = "vm-1", row.FixedPowerOp, testHostID
	case *providerv1.ReconfigureRequest:
		r.Id, r.DesiredJson = "vm-1", `{"cpu":2}`
	}
	return m
}

// testVerifier verifies for pki at testEpoch.
func testVerifier(t testing.TB, pki *testPKI) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{
		Anchors:     []*x509.Certificate{pki.anchor},
		InstallID:   pki.installID,
		ProviderUID: testProviderUID,
		HostID:      testHostID,
		EnrolmentID: testEnrolment,
		Now:         func() time.Time { return testEpoch },
	})
	require.NoError(t, err)
	return v
}

// call is one call under construction: every part a test may break.
type call struct {
	kind       providerv1.OperationKind
	body       *providerv1.GrantBody
	bodyExtra  []byte // appended to the serialised body before signing
	request    []byte
	keepDigest bool // do not recompute intent_digest after edits
	chain      [][]byte
	sigMutate  func([]byte) []byte
	envExtra   []byte
	grantExtra []byte
	noGrant    bool
	method     string
	outerEdit  func(proto.Message)
}

// newCall is a valid call of kind signed by pki.
func newCall(t testing.TB, pki *testPKI, kind providerv1.OperationKind) *call {
	t.Helper()
	reqBytes, err := proto.Marshal(testRequest(kind))
	require.NoError(t, err)
	body, err := buildOperationBody(pki, MintRequest{
		Identity:   testIdentity(kind),
		DigestSalt: bytesOf(0x5A, SaltLen),
		Request:    reqBytes,
		IssuedAt:   testEpoch,
	})
	require.NoError(t, err)
	row, _ := RowFor(kind)
	return &call{kind: kind, body: body, request: reqBytes, method: row.FullMethod}
}

// outer signs c and wraps it in the outer request message.
func (c *call) outer(t testing.TB, pki *testPKI) proto.Message {
	t.Helper()
	if !c.keepDigest {
		c.body.IntentDigest = IntentDigest(c.body.GetDigestSalt(), c.request)
	}
	bodyBytes, err := proto.Marshal(c.body)
	require.NoError(t, err)
	bodyBytes = append(bodyBytes, c.bodyExtra...)
	saved := pki.chain
	pki.chain = c.chain
	sig, err := pki.Sign(context.Background(), bodyBytes, time.Time{})
	pki.chain = saved
	require.NoError(t, err)
	if c.sigMutate != nil {
		sig.Signature = c.sigMutate(sig.Signature)
	}
	env := &providerv1.OperationEnvelope{Request: c.request}
	if !c.noGrant {
		env.Grant = &providerv1.OperationGrant{Body: bodyBytes, Signature: sig.Signature, SignerCertChain: sig.Chain}
		if c.grantExtra != nil {
			env.Grant.ProtoReflect().SetUnknown(c.grantExtra)
		}
	}
	if c.envExtra != nil {
		env.ProtoReflect().SetUnknown(c.envExtra)
	}
	row, _ := RowFor(c.kind)
	m := row.NewRequest()
	fd := m.ProtoReflect().Descriptor().Fields().ByName(envelopeFieldName)
	m.ProtoReflect().Set(fd, protoValueOf(env))
	if c.outerEdit != nil {
		c.outerEdit(m)
	}
	return m
}

// protoValueOf wraps m for protoreflect.Message.Set.
func protoValueOf(m proto.Message) protoreflect.Value {
	return protoreflect.ValueOfMessage(m.ProtoReflect())
}

// TestVerify_AcceptsAValidGrantForEveryKind: a grant minted for each kind,
// sent on its RPC, verifies and yields the exact request bytes.
func TestVerify_AcceptsAValidGrantForEveryKind(t *testing.T) {
	pki := validPKI(t)
	v := testVerifier(t, pki)
	for _, row := range Rows() {
		t.Run(row.Kind.String(), func(t *testing.T) {
			c := newCall(t, pki, row.Kind)
			got, err := v.Verify(c.method, c.outer(t, pki))
			require.NoError(t, err)
			assert.Equal(t, row.Kind, got.Kind)
			assert.Equal(t, c.request, got.RequestBytes)
			assert.True(t, proto.Equal(testRequest(row.Kind), got.Request))
		})
	}
}

// TestVerify_Refusals: one case per verifier rule, each asserting the reason
// and the rule.
func TestVerify_Refusals(t *testing.T) {
	pki := validPKI(t)
	other := validPKI(t)
	v := testVerifier(t, pki)
	raw := unknownField()
	type tc struct {
		name   string
		kind   providerv1.OperationKind
		edit   func(c *call)
		reason string
		rule   Rule
		badArg bool
	}
	setBody := func(f func(b *providerv1.GrantBody)) func(c *call) { return func(c *call) { f(c.body) } }
	setRequest := func(f func(m proto.Message)) func(c *call) {
		return func(c *call) {
			row, _ := RowFor(c.kind)
			m := row.NewRequest()
			require.NoError(t, proto.Unmarshal(c.request, m))
			f(m)
			b, err := proto.Marshal(m)
			require.NoError(t, err)
			c.request = b
		}
	}
	const (
		un = contracts.GrantUnauthenticatedReason
		mm = contracts.GrantMismatchReason
	)
	cases := []tc{
		// Outer request: the envelope is the only field set.
		{name: "no envelope", edit: func(c *call) {
			c.outerEdit = func(m proto.Message) {
				if r, ok := m.(*providerv1.DeleteRequest); ok {
					r.OperationEnvelope = nil
				}
			}
		}, reason: contracts.IdentityRequiredReason, rule: RuleEnvelopeMissing},
		{name: "outer field beside envelope", edit: func(c *call) {
			c.outerEdit = func(m proto.Message) {
				if r, ok := m.(*providerv1.DeleteRequest); ok {
					r.Id = "other-vm"
				}
			}
		}, reason: mm, rule: RuleOuterFieldSet, badArg: true},
		{name: "outer unknown field beside envelope", edit: func(c *call) {
			c.outerEdit = func(m proto.Message) { m.ProtoReflect().SetUnknown(raw) }
		}, reason: mm, rule: RuleOuterFieldSet, badArg: true},
		// Authentication.
		{name: "no grant", edit: func(c *call) { c.noGrant = true }, reason: un, rule: RuleGrantMissing},
		{name: "chain of one", edit: func(c *call) { c.chain = [][]byte{pki.leaf.Raw} }, reason: un, rule: RuleChain},
		{name: "chain of three", edit: func(c *call) { c.chain = [][]byte{pki.leaf.Raw, pki.issuer.Raw, pki.anchor.Raw} },
			reason: un, rule: RuleChain},
		{name: "chain garbage", edit: func(c *call) { c.chain = [][]byte{{1, 2, 3}, pki.issuer.Raw} }, reason: un, rule: RuleChain},
		{name: "chain from another anchor", edit: func(c *call) { c.chain = [][]byte{pki.leaf.Raw, other.issuer.Raw} },
			reason: un, rule: RuleChain},
		{name: "signature flipped", edit: func(c *call) {
			c.sigMutate = func(s []byte) []byte { s[len(s)-1] ^= 0xFF; return s }
		}, reason: un, rule: RuleSignature},
		{name: "signature high-S (r, n-s)", edit: func(c *call) {
			c.sigMutate = func(sig []byte) []byte {
				parsed, err := parseECDSASignature(sig)
				require.NoError(t, err)
				parsed.S = new(big.Int).Sub(elliptic.P256().Params().N, parsed.S)
				out, err := asn1.Marshal(parsed)
				require.NoError(t, err)
				return out
			}
		}, reason: un, rule: RuleSignature},
		{name: "signature empty", edit: func(c *call) { c.sigMutate = func([]byte) []byte { return nil } }, reason: un, rule: RuleSignature},
		// Unknown fields everywhere on the envelope path.
		{name: "envelope unknown field", edit: func(c *call) { c.envExtra = raw }, reason: mm, rule: RuleUnknownField},
		{name: "grant unknown field", edit: func(c *call) { c.grantExtra = raw }, reason: mm, rule: RuleUnknownField},
		{name: "body unknown field", edit: func(c *call) { c.bodyExtra = raw }, reason: mm, rule: RuleUnknownField},
		{name: "epoch unknown field", edit: setBody(func(b *providerv1.GrantBody) { b.Epoch.ProtoReflect().SetUnknown(raw) }),
			reason: mm, rule: RuleUnknownField},
		{name: "mint_epoch unknown field", edit: setBody(func(b *providerv1.GrantBody) { b.MintEpoch.ProtoReflect().SetUnknown(raw) }),
			reason: mm, rule: RuleUnknownField},
		{name: "timestamp unknown field", edit: setBody(func(b *providerv1.GrantBody) { b.IssuedAt.ProtoReflect().SetUnknown(raw) }),
			reason: mm, rule: RuleUnknownField},
		{name: "inner request unknown field", edit: func(c *call) { c.request = append(c.request, raw...) }, reason: mm, rule: RuleUnknownField},
		{name: "inner request nested unknown field", kind: providerv1.OperationKind_OPERATION_KIND_DELETE,
			edit: setRequest(func(m proto.Message) {
				if r, ok := m.(*providerv1.DeleteRequest); ok {
					r.Owner = &providerv1.ObjectIdentity{Uid: "u"}
					r.Owner.ProtoReflect().SetUnknown(raw)
				}
			}), reason: mm, rule: RuleUnknownField},
		// Enums: UNSPECIFIED and unknown values.
		{name: "grant_kind unspecified", edit: setBody(func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_UNSPECIFIED }),
			reason: mm, rule: RuleUnspecifiedEnum},
		{name: "grant_kind unknown number", edit: setBody(func(b *providerv1.GrantBody) { b.GrantKind = 99 }), reason: mm, rule: RuleUnknownEnum},
		{name: "grant_kind not issuable", edit: setBody(func(b *providerv1.GrantBody) { b.GrantKind = providerv1.GrantKind_GRANT_KIND_ADMIN }),
			reason: mm, rule: RuleGrantKind},
		{name: "kind unspecified", edit: setBody(func(b *providerv1.GrantBody) { b.Kind = providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED }),
			reason: mm, rule: RuleUnspecifiedEnum},
		{name: "kind unknown number", edit: setBody(func(b *providerv1.GrantBody) { b.Kind = 99 }), reason: mm, rule: RuleUnknownEnum},
		{name: "inner request unknown enum", kind: providerv1.OperationKind_OPERATION_KIND_POWER_ON,
			edit: setRequest(func(m proto.Message) {
				if r, ok := m.(*providerv1.PowerRequest); ok {
					r.Op = 99
				}
			}), reason: mm, rule: RuleUnknownEnum},
		// Grant version.
		{name: "grant_version 0", edit: setBody(func(b *providerv1.GrantBody) { b.GrantVersion = 0 }), reason: mm, rule: RuleGrantVersion},
		{name: "grant_version 2", edit: setBody(func(b *providerv1.GrantBody) { b.GrantVersion = 2 }), reason: mm, rule: RuleGrantVersion},
		// Fields set aside at version 1 (26, 28, 29).
		{name: "eal_seq set", edit: setBody(func(b *providerv1.GrantBody) { b.EalSeq = 1 }), reason: mm, rule: RulePlaceholderSet},
		{name: "reset_floor set", edit: setBody(func(b *providerv1.GrantBody) { b.ResetFloor = timestamppb.New(testEpoch) }),
			reason: mm, rule: RulePlaceholderSet},
		{name: "ceremony_record_digest set", edit: setBody(func(b *providerv1.GrantBody) { b.CeremonyRecordDigest = []byte{1} }),
			reason: mm, rule: RulePlaceholderSet},
		// Fields the OPERATION kind does not use.
		{name: "target_namespace set", edit: setBody(func(b *providerv1.GrantBody) { b.TargetNamespace = "ns" }), reason: mm, rule: RuleFieldNotForKind},
		{name: "request_uid set", edit: setBody(func(b *providerv1.GrantBody) { b.RequestUid = "r" }), reason: mm, rule: RuleFieldNotForKind},
		{name: "action set", edit: setBody(func(b *providerv1.GrantBody) { b.Action = "drain" }), reason: mm, rule: RuleFieldNotForKind},
		{name: "spec_digest set", edit: setBody(func(b *providerv1.GrantBody) { b.SpecDigest = []byte{1} }), reason: mm, rule: RuleFieldNotForKind},
		{name: "single_use_nonce set", edit: setBody(func(b *providerv1.GrantBody) { b.SingleUseNonce = []byte{1} }), reason: mm, rule: RuleFieldNotForKind},
		{name: "precondition set", edit: setBody(func(b *providerv1.GrantBody) { b.Precondition = &providerv1.GrantPrecondition{Seq: 1} }),
			reason: mm, rule: RuleFieldNotForKind},
		// Counters count from 1.
		{name: "authority_generation 0", edit: setBody(func(b *providerv1.GrantBody) { b.AuthorityGeneration = 0 }), reason: mm, rule: RuleCounterZero},
		{name: "attempt 0", edit: setBody(func(b *providerv1.GrantBody) { b.Attempt = 0 }), reason: mm, rule: RuleCounterZero},
		{name: "epoch counter 0", edit: setBody(func(b *providerv1.GrantBody) { b.Epoch.Counter = 0 }), reason: mm, rule: RuleCounterZero},
		{name: "mint_epoch counter 0", edit: setBody(func(b *providerv1.GrantBody) { b.MintEpoch.Counter = 0 }), reason: mm, rule: RuleCounterZero},
		// Required fields and lengths.
		{name: "epoch missing", edit: setBody(func(b *providerv1.GrantBody) { b.Epoch = nil }), reason: mm, rule: RuleMalformedField},
		{name: "epoch holder short", edit: setBody(func(b *providerv1.GrantBody) { b.Epoch.Holder = []byte{1} }), reason: mm, rule: RuleMalformedField},
		{name: "op_id short", edit: setBody(func(b *providerv1.GrantBody) { b.OpId = b.OpId[:IDLen-1] }), reason: mm, rule: RuleMalformedField},
		{name: "binding_id missing", edit: setBody(func(b *providerv1.GrantBody) { b.BindingId = nil }), reason: mm, rule: RuleMalformedField},
		{name: "install_id long", edit: setBody(func(b *providerv1.GrantBody) { b.InstallId = bytesOf(1, IDLen+1) }), reason: mm, rule: RuleMalformedField},
		{name: "salt short", edit: setBody(func(b *providerv1.GrantBody) { b.DigestSalt = bytesOf(1, MinSaltLen-1) }), reason: mm, rule: RuleMalformedField},
		{name: "vm_uid empty", edit: setBody(func(b *providerv1.GrantBody) { b.VmUid = "" }), reason: mm, rule: RuleMalformedField},
		{name: "issued_at missing", edit: setBody(func(b *providerv1.GrantBody) { b.IssuedAt = nil }), reason: mm, rule: RuleMalformedField},
		{name: "not_after before issued_at", edit: setBody(func(b *providerv1.GrantBody) { b.NotAfter = timestamppb.New(testEpoch.Add(-time.Second)) }),
			reason: mm, rule: RuleMalformedField},
		// Generation and validity.
		{name: "generation below minimum", edit: func(c *call) {}, reason: un, rule: RuleAuthorityGeneration},
		{name: "expired", edit: setBody(func(b *providerv1.GrantBody) {
			b.IssuedAt = timestamppb.New(testEpoch.Add(-10 * time.Minute))
			b.NotAfter = timestamppb.New(testEpoch)
		}), reason: un, rule: RuleValidity},
		{name: "issued in the future", edit: setBody(func(b *providerv1.GrantBody) {
			b.IssuedAt = timestamppb.New(testEpoch.Add(10 * time.Minute))
			b.NotAfter = timestamppb.New(testEpoch.Add(15 * time.Minute))
		}), reason: un, rule: RuleValidity},
		{name: "lifetime too long", edit: setBody(func(b *providerv1.GrantBody) {
			b.NotAfter = timestamppb.New(testEpoch.Add(MaxGrantLifetime + time.Minute))
		}), reason: un, rule: RuleValidity},
		// Binding to this installation, Provider, host and enrolment.
		{name: "other install_id", edit: setBody(func(b *providerv1.GrantBody) { b.InstallId = bytesOf(0xFF, IDLen) }), reason: mm, rule: RuleBinding},
		{name: "other provider", edit: setBody(func(b *providerv1.GrantBody) { b.ProviderUid = "other" }), reason: mm, rule: RuleBinding},
		{name: "other host", edit: setBody(func(b *providerv1.GrantBody) { b.HostId = "host-b" }), reason: mm, rule: RuleBinding},
		{name: "other enrolment", edit: setBody(func(b *providerv1.GrantBody) { b.EnrolmentId = bytesOf(0xFF, IDLen) }), reason: mm, rule: RuleBinding},
		// The kind is bound to the RPC, the request type and the fixed field.
		{name: "delete grant on Power", edit: func(c *call) { c.method = providerv1.Provider_Power_FullMethodName }, reason: mm, rule: RuleWrongRPC},
		{name: "power-on grant for a reboot", kind: providerv1.OperationKind_OPERATION_KIND_POWER_ON,
			edit: setRequest(func(m proto.Message) {
				if r, ok := m.(*providerv1.PowerRequest); ok {
					r.Op = providerv1.PowerOp_POWER_OP_REBOOT
				}
			}),
			reason: mm, rule: RuleFixedField},
		{name: "reboot grant with op unset", kind: providerv1.OperationKind_OPERATION_KIND_REBOOT,
			edit: setRequest(func(m proto.Message) {
				if r, ok := m.(*providerv1.PowerRequest); ok {
					r.Op = providerv1.PowerOp_POWER_OP_UNSPECIFIED
				}
			}),
			reason: mm, rule: RuleFixedField},
		{name: "graceful shutdown grant for power off", kind: providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL,
			edit: setRequest(func(m proto.Message) {
				if r, ok := m.(*providerv1.PowerRequest); ok {
					r.Op = providerv1.PowerOp_POWER_OP_OFF
				}
			}),
			reason: mm, rule: RuleFixedField},
		{name: "inner envelope", edit: setRequest(func(m proto.Message) {
			if r, ok := m.(*providerv1.DeleteRequest); ok {
				r.OperationEnvelope = &providerv1.OperationEnvelope{Request: []byte{1}}
			}
		}), reason: mm, rule: RuleInnerEnvelope},
		{name: "request does not decode", edit: func(c *call) { c.request = []byte{0xFF, 0xFF, 0xFF} }, reason: mm, rule: RuleMalformedRequest},
		// Intent digest over the exact bytes.
		{name: "intent digest of other bytes", edit: func(c *call) {
			c.keepDigest = true
			c.body.IntentDigest = IntentDigest(c.body.GetDigestSalt(), append([]byte{0}, c.request...))
		}, reason: mm, rule: RuleIntentDigest},
		{name: "request swapped after signing", edit: func(c *call) {
			c.keepDigest = true
			c.body.IntentDigest = IntentDigest(c.body.GetDigestSalt(), c.request)
			r := mustAs[*providerv1.DeleteRequest](t, testRequest(providerv1.OperationKind_OPERATION_KIND_DELETE))
			r.Id = "another-vm"
			b, err := proto.Marshal(r)
			require.NoError(t, err)
			c.request = b
		}, reason: mm, rule: RuleIntentDigest},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			kind := tt.kind
			if kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED {
				kind = providerv1.OperationKind_OPERATION_KIND_DELETE
			}
			c := newCall(t, pki, kind)
			tt.edit(c)
			verifier := v
			if tt.rule == RuleAuthorityGeneration {
				var err error
				verifier, err = NewVerifier(VerifierConfig{Anchors: []*x509.Certificate{pki.anchor}, InstallID: pki.installID,
					ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment,
					MinAuthorityGeneration: 2, Now: func() time.Time { return testEpoch }})
				require.NoError(t, err)
			}
			got, err := verifier.Verify(c.method, c.outer(t, pki))
			require.Error(t, err)
			assert.Nil(t, got)
			r, ok := AsRefusal(err)
			require.True(t, ok, "not a Refusal: %v", err)
			assert.Equal(t, tt.reason, r.Reason, "%v", err)
			assert.Equal(t, tt.rule, r.Rule, "%v", err)
			assert.Equal(t, tt.badArg, r.InvalidArgument)
		})
	}
}

// TestVerify_ChainShapes: the chain is exactly a leaf and a path length 0
// issuer with ECDSA P-256 keys, valid at the verifier's time.
func TestVerify_ChainShapes(t *testing.T) {
	issuerPathLen1 := defaultIssuer()
	issuerPathLen1.maxPathLen, issuerPathLen1.maxPathLenZero = 1, false
	issuerP384 := defaultIssuer()
	issuerP384.curve = elliptic.P384()
	leafP384 := defaultLeaf()
	leafP384.curve = elliptic.P384()
	leafCA := defaultLeaf()
	leafCA.isCA, leafCA.keyUsage = true, x509.KeyUsageDigitalSignature|x509.KeyUsageCertSign
	leafNoSign := defaultLeaf()
	leafNoSign.keyUsage = x509.KeyUsageKeyEncipherment
	leafExpired := defaultLeaf()
	leafExpired.notAfter = testEpoch.Add(-time.Minute)
	leafNotYet := defaultLeaf()
	leafNotYet.notBefore = testEpoch.Add(time.Minute)
	leafServerAuth := defaultLeaf()
	leafServerAuth.extKeyUsage, leafServerAuth.dnsNames = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"agent.example"}
	leafServerAuth.keyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	leafServerAuthDS := defaultLeaf()
	leafServerAuthDS.extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	leafClientAuth := defaultLeaf()
	leafClientAuth.extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	leafAnyEKU := defaultLeaf()
	leafAnyEKU.extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	leafUnknownEKU := defaultLeaf()
	leafUnknownEKU.unknownEKU = []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 99999, 1}}
	leafKeyEncipherment := defaultLeaf()
	leafKeyEncipherment.keyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	leafCertSign := defaultLeaf()
	leafCertSign.keyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
	issuerNoCertSign := defaultIssuer()
	issuerNoCertSign.keyUsage = x509.KeyUsageDigitalSignature
	for name, p := range map[string]struct{ issuer, leaf certOpts }{
		"TLS server certificate":           {defaultIssuer(), leafServerAuth},
		"serverAuth EKU, digitalSignature": {defaultIssuer(), leafServerAuthDS},
		"clientAuth EKU":                   {defaultIssuer(), leafClientAuth},
		"any EKU":                          {defaultIssuer(), leafAnyEKU},
		"unknown EKU OID":                  {defaultIssuer(), leafUnknownEKU},
		"leaf keyEncipherment too":         {defaultIssuer(), leafKeyEncipherment},
		"leaf certSign too":                {defaultIssuer(), leafCertSign},
		"issuer without certSign":          {issuerNoCertSign, defaultLeaf()},
		"issuer path length 1":             {issuerPathLen1, defaultLeaf()},
		"issuer P-384":                     {issuerP384, defaultLeaf()},
		"leaf P-384":                       {defaultIssuer(), leafP384},
		"leaf is a CA":                     {defaultIssuer(), leafCA},
		"leaf cannot sign":                 {defaultIssuer(), leafNoSign},
		"leaf expired":                     {defaultIssuer(), leafExpired},
		"leaf not yet valid":               {defaultIssuer(), leafNotYet},
	} {
		t.Run(name, func(t *testing.T) {
			pki := newTestPKI(t, p.issuer, p.leaf)
			c := newCall(t, pki, providerv1.OperationKind_OPERATION_KIND_DELETE)
			_, err := testVerifier(t, pki).Verify(c.method, c.outer(t, pki))
			r, ok := AsRefusal(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, contracts.GrantUnauthenticatedReason, r.Reason)
			assert.Equal(t, RuleChain, r.Rule)
		})
	}

	t.Run("leaf signed by the anchor directly", func(t *testing.T) {
		pki := validPKI(t)
		lk := mustKey(t, elliptic.P256())
		pki.leafKey = lk
		pki.leaf = mustCert(t, "direct-leaf", defaultLeaf(), &lk.PublicKey, pki.anchor, pki.anchorKey)
		c := newCall(t, pki, providerv1.OperationKind_OPERATION_KIND_DELETE)
		_, err := testVerifier(t, pki).Verify(c.method, c.outer(t, pki))
		r, ok := AsRefusal(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, RuleChain, r.Rule)
	})
}

// TestVerify_NotAMutatingRequest: a read request has no envelope field.
func TestVerify_NotAMutatingRequest(t *testing.T) {
	v := testVerifier(t, validPKI(t))
	_, err := v.Verify(providerv1.Provider_Describe_FullMethodName, &providerv1.DescribeRequest{Id: "x"})
	r, ok := AsRefusal(err)
	require.True(t, ok)
	assert.Equal(t, RuleNotMutatingRequest, r.Rule)
	_, err = v.Verify(providerv1.Provider_Delete_FullMethodName, nil)
	_, ok = AsRefusal(err)
	assert.True(t, ok)
}

// TestVerify_RefusalCarriesNoRequestBytes: a refusal's text never quotes the
// request, whatever rule refused it.
func TestVerify_RefusalCarriesNoRequestBytes(t *testing.T) {
	pki := validPKI(t)
	c := newCall(t, pki, providerv1.OperationKind_OPERATION_KIND_CREATE)
	row, _ := RowFor(c.kind)
	m := mustAs[*providerv1.CreateRequest](t, row.NewRequest())
	require.NoError(t, proto.Unmarshal(c.request, m))
	m.UserData = []byte("#cloud-config\npassword: s3cret-value")
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	c.request = append(b, unknownField()...)
	_, err = testVerifier(t, pki).Verify(c.method, c.outer(t, pki))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret-value")
}

// TestNewVerifier_RefusesBadConfig: anchors, ids and expectations are
// required.
func TestNewVerifier_RefusesBadConfig(t *testing.T) {
	pki := validPKI(t)
	good := VerifierConfig{Anchors: []*x509.Certificate{pki.anchor}, InstallID: pki.installID,
		ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment}
	_, err := NewVerifier(good)
	require.NoError(t, err)
	for name, edit := range map[string]func(c *VerifierConfig){
		"no anchors":         func(c *VerifierConfig) { c.Anchors = nil },
		"anchor not a CA":    func(c *VerifierConfig) { c.Anchors = []*x509.Certificate{pki.leaf} },
		"nil anchor":         func(c *VerifierConfig) { c.Anchors = []*x509.Certificate{nil} },
		"short install id":   func(c *VerifierConfig) { c.InstallID = []byte{1} },
		"short enrolment id": func(c *VerifierConfig) { c.EnrolmentID = nil },
		"no provider uid":    func(c *VerifierConfig) { c.ProviderUID = "" },
		"no host id":         func(c *VerifierConfig) { c.HostID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			edit(&cfg)
			_, err := NewVerifier(cfg)
			assert.Error(t, err)
		})
	}
}

// TestVerify_HighSIsAValidSignatureButRefused: (r, n-s) verifies as ECDSA, so
// only the low-S rule refuses it, and the verifier is stateless: the same
// envelope verifies twice (callers dedupe on op_id and attempt).
func TestVerify_HighSIsAValidSignatureButRefused(t *testing.T) {
	pki := validPKI(t)
	v := testVerifier(t, pki)
	c := newCall(t, pki, providerv1.OperationKind_OPERATION_KIND_DELETE)
	outer := c.outer(t, pki)
	_, err := v.Verify(c.method, outer)
	require.NoError(t, err)
	_, err = v.Verify(c.method, outer)
	require.NoError(t, err, "Verify is stateless")

	g := mustAs[*providerv1.DeleteRequest](t, outer).GetOperationEnvelope().GetGrant()
	parsed, err := parseECDSASignature(g.GetSignature())
	require.NoError(t, err)
	parsed.S = new(big.Int).Sub(elliptic.P256().Params().N, parsed.S)
	high, err := asn1.Marshal(parsed)
	require.NoError(t, err)
	digest := sha256.Sum256(g.GetBody())
	pub := mustAs[*ecdsa.PublicKey](t, pki.leaf.PublicKey)
	require.True(t, ecdsa.VerifyASN1(pub, digest[:], high), "(r, n-s) is a valid ECDSA signature")
	g.Signature = high
	_, err = v.Verify(c.method, outer)
	r, ok := AsRefusal(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, RuleSignature, r.Rule)
}
