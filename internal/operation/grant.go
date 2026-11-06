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
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Grant lifetimes. not_after is defence in depth only; it never decides what
// a host has executed.
const (
	// DefaultGrantLifetime is the validity MintOperationGrant gives a grant
	// when MintRequest.Lifetime is 0.
	DefaultGrantLifetime = 5 * time.Minute
	// MaxGrantLifetime is the longest validity this package mints or accepts.
	MaxGrantLifetime = time.Hour
)

// Epoch is a manager epoch: a counter that counts from 1 and the 16-byte
// random holder of the term that holds it.
type Epoch struct {
	// Counter is the epoch counter, at least 1.
	Counter uint64
	// Holder is the 16-byte random holder identity.
	Holder []byte
}

// toProto converts e for a GrantBody. The holder is copied.
func (e Epoch) toProto() *providerv1.Epoch {
	return &providerv1.Epoch{Counter: e.Counter, Holder: append([]byte(nil), e.Holder...)}
}

// Identity is what one operation attempt's grant names, apart from what the
// Signer supplies (install_id, authority_generation) and what is derived from
// the request (intent_digest).
type Identity struct {
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// HostID is the Host the operation runs on.
	HostID string
	// EnrolmentID is the host's 16-byte enrolment identifier.
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch Epoch
	// VMUID is the VirtualMachine's metadata.uid.
	VMUID string
	// BindingID is the VM placement's 16-byte identifier.
	BindingID []byte
	// OpID is the attempt's 16-byte identifier; a new attempt has a new one.
	OpID []byte
	// Kind is the operation kind.
	Kind providerv1.OperationKind
	// Attempt is the attempt number for this VM and kind, at least 1.
	Attempt uint64
	// MintEpoch is the epoch at which OpID was minted.
	MintEpoch Epoch
}

// MintRequest is the input of MintOperationGrant.
type MintRequest struct {
	// Identity is what the grant names.
	Identity Identity
	// DigestSalt is the attempt's random salt (NewDigestSalt), at least
	// MinSaltLen bytes. A re-send of the same attempt reuses it.
	DigestSalt []byte
	// Request is the exact serialised request message the grant authorises.
	// It is never logged.
	Request []byte
	// IssuedAt is the manager's clock at signing.
	IssuedAt time.Time
	// Lifetime is the grant's validity from IssuedAt; 0 means
	// DefaultGrantLifetime. It may not exceed MaxGrantLifetime.
	Lifetime time.Duration
}

// NewRandomID draws a 16-byte random identifier (op_id, binding_id, install_id,
// enrolment_id, Epoch.holder) from crypto/rand.
func NewRandomID() ([]byte, error) {
	return randomBytes(IDLen)
}

// NewDigestSalt draws a SaltLen-byte random digest salt from crypto/rand.
func NewDigestSalt() ([]byte, error) {
	return randomBytes(SaltLen)
}

// randomBytes draws n bytes from crypto/rand.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("draw %d random bytes: %w", n, err)
	}
	return b, nil
}

// IntentDigest is SHA-256(salt || request), the GrantBody.intent_digest over
// the exact request bytes.
func IntentDigest(salt, request []byte) []byte {
	h := sha256.New()
	_, _ = h.Write(salt) // hash.Hash.Write never returns an error
	_, _ = h.Write(request)
	return h.Sum(nil)
}

// buildOperationBody builds the version 1 OPERATION body r and s describe and
// checks it, without signing it.
func buildOperationBody(s Signer, r MintRequest) (*providerv1.GrantBody, error) {
	lifetime := r.Lifetime
	if lifetime == 0 {
		lifetime = DefaultGrantLifetime
	}
	if lifetime < 0 || lifetime > MaxGrantLifetime {
		return nil, fmt.Errorf("grant lifetime %s is outside (0, %s]", lifetime, MaxGrantLifetime)
	}
	if r.IssuedAt.IsZero() {
		return nil, errors.New("grant issued_at is not set")
	}
	if len(r.Request) > MaxRequestBytes {
		return nil, fmt.Errorf("request is larger than %d bytes", MaxRequestBytes)
	}
	id := r.Identity
	body := &providerv1.GrantBody{
		GrantVersion:        GrantVersion,
		GrantKind:           providerv1.GrantKind_GRANT_KIND_OPERATION,
		InstallId:           append([]byte(nil), s.InstallID()...),
		ProviderUid:         id.ProviderUID,
		HostId:              id.HostID,
		EnrolmentId:         append([]byte(nil), id.EnrolmentID...),
		AuthorityGeneration: s.AuthorityGeneration(),
		Epoch:               id.Epoch.toProto(),
		VmUid:               id.VMUID,
		BindingId:           append([]byte(nil), id.BindingID...),
		OpId:                append([]byte(nil), id.OpID...),
		Kind:                id.Kind,
		Attempt:             id.Attempt,
		IntentDigest:        IntentDigest(r.DigestSalt, r.Request),
		DigestSalt:          append([]byte(nil), r.DigestSalt...),
		MintEpoch:           id.MintEpoch.toProto(),
		IssuedAt:            timestamppb.New(r.IssuedAt),
		NotAfter:            timestamppb.New(r.IssuedAt.Add(lifetime)),
	}
	if refusal := checkOperationBody(body); refusal != nil {
		return nil, fmt.Errorf("mint operation grant: %w", refusal)
	}
	row, _ := RowFor(id.Kind) // checkOperationBody refused an unknown kind
	if _, refusal := decodeRequest(row, r.Request); refusal != nil {
		return nil, fmt.Errorf("mint operation grant: %w", refusal)
	}
	return body, nil
}

// CheckMintRequest reports whether MintOperationGrant would accept r for a
// signer with installID and authorityGeneration, without signing. It lets a
// caller refuse an attempt before it persists anything.
func CheckMintRequest(installID []byte, authorityGeneration uint64, r MintRequest) error {
	_, err := buildOperationBody(staticIdentity{installID: installID, generation: authorityGeneration}, r)
	return err
}

// staticIdentity is a Signer that only answers InstallID and
// AuthorityGeneration; CheckMintRequest uses it and never signs.
type staticIdentity struct {
	installID  []byte
	generation uint64
}

func (s staticIdentity) InstallID() []byte           { return s.installID }
func (s staticIdentity) AuthorityGeneration() uint64 { return s.generation }
func (s staticIdentity) Sign(context.Context, []byte, time.Time) (Signature, error) {
	return Signature{}, errors.New("staticIdentity does not sign")
}

// MintOperationGrant builds a version 1 GRANT_KIND_OPERATION GrantBody for r
// (fields 1-8, 10-17 and 30-31; expect_known false; 20-29 unset), serialises
// it once, and has s sign those exact bytes. It refuses, before signing, an
// identity or request that the verifier would refuse on its own: a zero
// counter, an identifier of the wrong length, an UNSPECIFIED or unknown kind,
// and request bytes that do not decode as the kind's request type, carry
// unknown fields or an envelope of their own, or disagree with the kind's
// fixed field.
func MintOperationGrant(ctx context.Context, s Signer, r MintRequest) (*providerv1.OperationGrant, error) {
	if s == nil {
		return nil, fmt.Errorf("mint operation grant: %w", ErrNoSigner)
	}
	body, err := buildOperationBody(s, r)
	if err != nil {
		return nil, err
	}
	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mint operation grant: serialise body: %w", err)
	}
	if len(bodyBytes) > MaxGrantBodyBytes {
		return nil, fmt.Errorf("mint operation grant: body is larger than %d bytes", MaxGrantBodyBytes)
	}
	sig, err := s.Sign(ctx, bodyBytes, body.GetNotAfter().AsTime())
	if err != nil {
		return nil, fmt.Errorf("mint operation grant: sign: %w", err)
	}
	return &providerv1.OperationGrant{
		Body:            bodyBytes,
		Signature:       sig.Signature,
		SignerCertChain: sig.Chain,
	}, nil
}

// DecodeRequest decodes requestBytes as the request type kind names and
// applies the verifier's request rules (no unknown fields or enum values, no
// envelope of its own, the kind's fixed field). The error never quotes the
// request.
func DecodeRequest(kind providerv1.OperationKind, requestBytes []byte) (proto.Message, error) {
	row, ok := RowFor(kind)
	if !ok {
		return nil, mismatch(RuleUnknownEnum, "kind %d is not implemented", kind)
	}
	m, refusal := decodeRequest(row, requestBytes)
	if refusal != nil {
		return nil, refusal
	}
	return m, nil
}
