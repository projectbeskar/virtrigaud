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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// chainLen is the exact number of certificates in OperationGrant.
// signer_cert_chain: the leaf and its path length 0 issuer.
const chainLen = 2

// maxEnforceableGeneration is the highest MinAuthorityGeneration a verifier
// accepts. authority_generation is not yet bound to the generation the issuer
// certificate names, so a higher floor would be a promise the verifier cannot
// keep (a leaf from an older issuer could claim the current generation).
const maxEnforceableGeneration = 1

// DefaultMaxClockSkew is how far in the future issued_at may be by the
// verifier's clock when VerifierConfig.MaxClockSkew is 0.
const DefaultMaxClockSkew = 5 * time.Minute

// Clock is the verifier's source of the current time. Every validity check of
// a call (certificate validity, grant validity, issued_at skew) is made
// against one Now reading of it.
//
// A Clock that cannot vouch for its reading returns the zero time. Verify then
// refuses every call with GRANT_UNAUTHENTICATED (rule validity) and never
// consults the grant, so an untrustworthy clock fails closed instead of
// accepting everything. A per-host agent supplies a Clock that never returns
// a time before a persisted floor; the manager and the fake Provider use
// WallClock.
type Clock interface {
	// Now returns the current time, or the zero time when the clock is not
	// trustworthy.
	Now() time.Time
}

// ClockFunc adapts a function to a Clock.
type ClockFunc func() time.Time

// Now calls f.
func (f ClockFunc) Now() time.Time { return f() }

// WallClock is the operating system's wall clock, the default Clock.
type WallClock struct{}

// Now returns time.Now().
func (WallClock) Now() time.Time { return time.Now() }

// VerifierConfig is what a verifier pins and expects: the trust anchors, and
// the installation, Provider, host and enrolment it verifies grants for.
type VerifierConfig struct {
	// Anchors are the pinned trust anchors (self-signed CA certificates with
	// ECDSA P-256 keys). The anchor is never taken from a grant.
	Anchors []*x509.Certificate
	// InstallID is the expected 16-byte install_id.
	InstallID []byte
	// ProviderUID is the expected provider_uid.
	ProviderUID string
	// HostID is the expected host_id.
	HostID string
	// EnrolmentID is the expected 16-byte enrolment_id.
	EnrolmentID []byte
	// MinAuthorityGeneration is the lowest authority_generation accepted; 0
	// means 1. Values above 1 are refused by NewVerifier until the generation
	// is bound to the issuer certificate.
	MinAuthorityGeneration uint64
	// MaxClockSkew bounds how far in the future issued_at may be; 0 means
	// DefaultMaxClockSkew.
	MaxClockSkew time.Duration
	// Clock is the verifier's clock. When nil and Now is nil, WallClock is
	// used. Setting both Clock and Now is an error.
	Clock Clock
	// Now is the verifier's clock as a function; it is kept for callers that
	// predate Clock. nil means Clock, or WallClock when Clock is nil too.
	Now func() time.Time
}

// Verifier verifies operation grants for one installation, Provider, host and
// enrolment. It is safe for concurrent use.
type Verifier struct {
	anchors       *x509.CertPool
	installID     []byte
	providerUID   string
	hostID        string
	enrolmentID   []byte
	minGeneration uint64
	maxSkew       time.Duration
	clock         Clock
}

// Verified is a call that passed every rule.
type Verified struct {
	// Body is the decoded GrantBody.
	Body *providerv1.GrantBody
	// Kind is the operation kind the grant names.
	Kind providerv1.OperationKind
	// Request is the decoded inner request, of the type Kind names. It is the
	// only copy of the request a verifier's caller may act on.
	Request proto.Message
	// RequestBytes are the exact request bytes the grant binds. They are
	// never logged.
	RequestBytes []byte
}

// NewVerifier returns a verifier for cfg. It refuses a config with no
// anchors, an anchor that is not a CA with an ECDSA P-256 key, or a missing
// expectation.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if len(cfg.Anchors) == 0 {
		return nil, errors.New("new operation-grant verifier: no trust anchor")
	}
	pool := x509.NewCertPool()
	for i, a := range cfg.Anchors {
		if a == nil || !a.IsCA || !isP256(a.PublicKey) {
			return nil, fmt.Errorf("new operation-grant verifier: anchor %d is not a CA certificate with an ECDSA P-256 key", i)
		}
		pool.AddCert(a)
	}
	if len(cfg.InstallID) != IDLen || len(cfg.EnrolmentID) != IDLen {
		return nil, fmt.Errorf("new operation-grant verifier: install and enrolment ids must be %d bytes", IDLen)
	}
	if cfg.ProviderUID == "" || cfg.HostID == "" {
		return nil, errors.New("new operation-grant verifier: provider uid and host id are required")
	}
	if cfg.MinAuthorityGeneration > maxEnforceableGeneration {
		return nil, fmt.Errorf("new operation-grant verifier: a minimum authority generation above %d is not supported: "+
			"authority_generation is not yet bound to the issuer certificate", maxEnforceableGeneration)
	}
	v := &Verifier{
		anchors:       pool,
		installID:     append([]byte(nil), cfg.InstallID...),
		providerUID:   cfg.ProviderUID,
		hostID:        cfg.HostID,
		enrolmentID:   append([]byte(nil), cfg.EnrolmentID...),
		minGeneration: cfg.MinAuthorityGeneration,
		maxSkew:       cfg.MaxClockSkew,
		clock:         cfg.Clock,
	}
	if v.minGeneration == 0 {
		v.minGeneration = 1
	}
	if v.maxSkew == 0 {
		v.maxSkew = DefaultMaxClockSkew
	}
	if cfg.Clock != nil && cfg.Now != nil {
		return nil, errors.New("new operation-grant verifier: set Clock or Now, not both")
	}
	switch {
	case cfg.Now != nil:
		v.clock = ClockFunc(cfg.Now)
	case v.clock == nil:
		v.clock = WallClock{}
	}
	return v, nil
}

// Verify checks one call that arrived on fullMethod with the outer request
// message outer. On success it returns the decoded grant body and inner
// request; otherwise the error is a *Refusal (AsRefusal) and the call must not
// be performed.
//
// Verify is stateless: the same envelope verifies again until its not_after.
// A caller that executes operations must enforce single execution itself,
// keyed on the grant's (op_id, attempt), and never on the signature or the
// grant or envelope bytes (a new grant for the same attempt has different
// bytes). Signatures are accepted in low-S form only, so a signature has one
// encoding. A caller acts only on Verified.Request, never on its own decoding
// of the outer request.
//
// authority_generation is checked against the configured minimum only; it is
// not yet bound to the generation the issuer certificate names, which is why
// NewVerifier refuses a minimum above 1.
//
// The rules, in order:
//
//  1. outer has operation_envelope set (else IDENTITY_REQUIRED), no other
//     field set and no unknown fields (else INVALID_ARGUMENT,
//     GRANT_MISMATCH);
//  2. the grant is present, its chain is exactly a leaf and a path length 0
//     issuer chaining to a pinned anchor, valid now, every key ECDSA P-256,
//     and the signature over the exact body bytes verifies (else
//     GRANT_UNAUTHENTICATED);
//  3. the envelope, the grant, the body and every message nested in it carry
//     no unknown fields or unknown enum values; the version, grant kind,
//     placeholders, counters and field lengths are those of a version 1
//     OPERATION grant (else GRANT_MISMATCH);
//  4. authority_generation is at least the minimum, and the grant is within
//     its validity (else GRANT_UNAUTHENTICATED);
//  5. installation, Provider, host and enrolment are the verifier's; the
//     kind names fullMethod and outer's type; the request decodes as that
//     type with no unknown fields, no envelope of its own and the kind's
//     fixed field; and intent_digest is SHA-256(digest_salt || request)
//     (else GRANT_MISMATCH).
func (v *Verifier) Verify(fullMethod string, outer proto.Message) (*Verified, error) {
	return v.verify(fullMethod, outer, true)
}

// VerifyHostBound is Verify without the Provider and enrolment comparison of
// rule 5: the installation and the host are still compared, and every other
// rule applies in the same order. It is for a caller that keeps a durable
// record of every operation it accepted (a per-host agent's operation store):
// such a caller must answer an op_id it knows from its record before it
// refuses a Provider or enrolment that differs from its own, and must then
// compare both itself (Verified.Body carries them), because a grant naming an
// enrolment whose records the caller no longer holds proves nothing about
// whether its op_id ran. Every other caller uses Verify.
func (v *Verifier) VerifyHostBound(fullMethod string, outer proto.Message) (*Verified, error) {
	return v.verify(fullMethod, outer, false)
}

// verify implements Verify; bindAll includes the Provider and enrolment in
// the binding comparison.
func (v *Verifier) verify(fullMethod string, outer proto.Message, bindAll bool) (*Verified, error) {
	env, refusal := envelopeOf(outer)
	if refusal != nil {
		return nil, refusal
	}
	grant := env.GetGrant()
	if grant == nil {
		return nil, unauthenticated(RuleGrantMissing, "the envelope carries no grant")
	}
	if refusal := checkSizes(env); refusal != nil {
		return nil, refusal
	}
	now := v.clock.Now()
	if now.IsZero() {
		return nil, unauthenticated(RuleValidity, "the verifier's clock is not trustworthy")
	}
	if refusal := v.authenticate(grant, now); refusal != nil {
		return nil, refusal
	}
	if refusal := walkMessage(env.ProtoReflect(), "OperationEnvelope"); refusal != nil {
		return nil, refusal
	}
	body := &providerv1.GrantBody{}
	if err := proto.Unmarshal(grant.GetBody(), body); err != nil {
		return nil, mismatch(RuleMalformedField, "the grant body does not decode")
	}
	if refusal := CheckOperationBody(body); refusal != nil {
		return nil, refusal
	}
	if body.GetAuthorityGeneration() < v.minGeneration {
		return nil, unauthenticated(RuleAuthorityGeneration, "authority_generation %d is below %d",
			body.GetAuthorityGeneration(), v.minGeneration)
	}
	if !now.Before(body.GetNotAfter().AsTime()) {
		return nil, unauthenticated(RuleValidity, "the grant expired")
	}
	if body.GetIssuedAt().AsTime().After(now.Add(v.maxSkew)) {
		return nil, unauthenticated(RuleValidity, "the grant is issued in the future")
	}
	if body.GetNotAfter().AsTime().Sub(body.GetIssuedAt().AsTime()) > MaxGrantLifetime {
		return nil, unauthenticated(RuleValidity, "the grant is valid for longer than %s", MaxGrantLifetime)
	}
	if !bytes.Equal(body.GetInstallId(), v.installID) || body.GetHostId() != v.hostID {
		return nil, mismatch(RuleBinding, "the grant is for another installation, Provider, host or enrolment")
	}
	if bindAll && (body.GetProviderUid() != v.providerUID || !bytes.Equal(body.GetEnrolmentId(), v.enrolmentID)) {
		return nil, mismatch(RuleBinding, "the grant is for another installation, Provider, host or enrolment")
	}
	row, _ := RowFor(body.GetKind()) // checkOperationBody refused an unknown kind
	outerType := outer.ProtoReflect().Descriptor().FullName()
	if row.FullMethod != fullMethod || row.RequestType() != outerType {
		return nil, mismatch(RuleWrongRPC, "kind %s does not name %s with %s", body.GetKind(), fullMethod, outerType)
	}
	req, refusal := DecodeRequestFor(row, env.GetRequest())
	if refusal != nil {
		return nil, refusal
	}
	want := IntentDigest(body.GetDigestSalt(), env.GetRequest())
	if subtle.ConstantTimeCompare(want, body.GetIntentDigest()) != 1 {
		return nil, mismatch(RuleIntentDigest, "intent_digest does not match the request")
	}
	return &Verified{Body: body, Kind: body.GetKind(), Request: req, RequestBytes: env.GetRequest()}, nil
}

// checkSizes applies the size caps before anything is parsed: the grant's
// parts are refused as GRANT_UNAUTHENTICATED (they were not authenticated),
// the request as GRANT_MISMATCH.
func checkSizes(env *providerv1.OperationEnvelope) *Refusal {
	g := env.GetGrant()
	if len(g.GetBody()) > MaxGrantBodyBytes {
		return unauthenticated(RuleSizeLimit, "the grant body is larger than %d bytes", MaxGrantBodyBytes)
	}
	if len(g.GetSignature()) > MaxSignatureBytes {
		return unauthenticated(RuleSizeLimit, "the signature is larger than %d bytes", MaxSignatureBytes)
	}
	if len(g.GetSignerCertChain()) > chainLen {
		return unauthenticated(RuleChain, "the chain has %d certificates, not %d", len(g.GetSignerCertChain()), chainLen)
	}
	for i, c := range g.GetSignerCertChain() {
		if len(c) > MaxCertBytes {
			return unauthenticated(RuleSizeLimit, "certificate %d is larger than %d bytes", i, MaxCertBytes)
		}
	}
	if len(env.GetRequest()) > MaxRequestBytes {
		return mismatch(RuleSizeLimit, "the request is larger than %d bytes", MaxRequestBytes)
	}
	return nil
}

// envelopeOf returns outer's operation_envelope after the outer-request
// rules: the field exists and is set, and nothing else of outer is.
func envelopeOf(outer proto.Message) (*providerv1.OperationEnvelope, *Refusal) {
	if outer == nil {
		return nil, mismatch(RuleNotMutatingRequest, "no request")
	}
	m := outer.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(envelopeFieldName)
	if fd == nil || fd.Message() == nil ||
		fd.Message().FullName() != (&providerv1.OperationEnvelope{}).ProtoReflect().Descriptor().FullName() {
		return nil, mismatch(RuleNotMutatingRequest, "%s has no operation_envelope field", m.Descriptor().FullName())
	}
	if !m.Has(fd) {
		return nil, &Refusal{Reason: ReasonIdentityRequired, Rule: RuleEnvelopeMissing,
			detail: "the call carries no operation_envelope"}
	}
	outerSet := len(m.GetUnknown()) > 0
	m.Range(func(other protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if other.Number() != fd.Number() {
			outerSet = true
		}
		return !outerSet
	})
	if outerSet {
		return nil, &Refusal{Reason: ReasonGrantMismatch, Rule: RuleOuterFieldSet, InvalidArgument: true,
			detail: "a field of the outer request is set beside operation_envelope"}
	}
	env, ok := m.Get(fd).Message().Interface().(*providerv1.OperationEnvelope)
	if !ok {
		return nil, mismatch(RuleNotMutatingRequest, "operation_envelope has an unexpected type")
	}
	return env, nil
}

// authenticate checks the chain and the signature over the exact body bytes.
func (v *Verifier) authenticate(g *providerv1.OperationGrant, now time.Time) *Refusal {
	chain := g.GetSignerCertChain()
	if len(chain) != chainLen {
		return unauthenticated(RuleChain, "the chain has %d certificates, not %d", len(chain), chainLen)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return unauthenticated(RuleChain, "the leaf certificate does not parse")
	}
	issuer, err := x509.ParseCertificate(chain[1])
	if err != nil {
		return unauthenticated(RuleChain, "the issuer certificate does not parse")
	}
	if !IsGrantSigningLeaf(leaf) {
		return unauthenticated(RuleChain, "the leaf is not a grant-signing certificate (profile: not a CA, key usage "+
			"digitalSignature only, no extended key usage, ECDSA P-256 key)")
	}
	if !issuer.IsCA || !issuer.BasicConstraintsValid || issuer.MaxPathLen != 0 || !issuer.MaxPathLenZero ||
		issuer.KeyUsage&x509.KeyUsageCertSign == 0 || !isP256(issuer.PublicKey) {
		return unauthenticated(RuleChain, "the issuer is not a path length 0 CA with an ECDSA P-256 key")
	}
	// The leaf has no extended key usage (IsGrantSigningLeaf), so
	// ExtKeyUsageAny only stops x509.Verify from demanding serverAuth; it
	// admits no certificate with an EKU.
	intermediates := x509.NewCertPool()
	intermediates.AddCert(issuer)
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         v.anchors,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return unauthenticated(RuleChain, "the chain does not verify to a pinned anchor at the verifier's time")
	}
	through := false
	for _, c := range chains {
		// leaf, issuer, anchor: the issuer is exactly the one sent.
		if len(c) == chainLen+1 && c[1].Equal(issuer) && isP256(c[2].PublicKey) {
			through = true
			break
		}
	}
	if !through {
		return unauthenticated(RuleChain, "the chain does not run leaf, sent issuer, pinned anchor")
	}
	pub, _ := leaf.PublicKey.(*ecdsa.PublicKey) // isP256 checked the type
	digest := sha256.Sum256(g.GetBody())
	if !ecdsa.VerifyASN1(pub, digest[:], g.GetSignature()) {
		return unauthenticated(RuleSignature, "the signature over the body does not verify")
	}
	if !IsLowS(g.GetSignature(), pub.Curve) {
		return unauthenticated(RuleSignature, "the signature is not in low-S form")
	}
	return nil
}

// IsGrantSigningLeaf reports whether c has the grant-signing leaf profile:
// not a CA, key usage exactly digitalSignature, no extended key usage of any
// kind (known or unknown OID), and an ECDSA P-256 key. A certificate issued
// for another purpose under the same anchor, such as a TLS server or client
// certificate, does not have it and cannot sign grants. A dedicated
// grant-signing extended key usage OID may replace the "no EKU" rule later.
func IsGrantSigningLeaf(c *x509.Certificate) bool {
	return !c.IsCA && c.KeyUsage == x509.KeyUsageDigitalSignature &&
		len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 && isP256(c.PublicKey)
}

// isP256 reports whether key is an ECDSA public key on P-256, the only key
// type a grant chain may use.
func isP256(key any) bool {
	pub, ok := key.(*ecdsa.PublicKey)
	return ok && pub.Curve == elliptic.P256()
}
