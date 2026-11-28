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
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// LedgerPosition is a position of a host's operation store as the host
// ledger records it: the store, a chain sequence number and the chain head
// at it.
type LedgerPosition struct {
	// StoreID is the 16-byte store identifier.
	StoreID []byte
	// Seq is the chain sequence number, at least 1.
	Seq uint64
	// Head is the 32-byte chain head at Seq.
	Head []byte
}

// toProto converts p for a GrantBody. The bytes are copied.
func (p LedgerPosition) toProto() *providerv1.LedgerPosition {
	return &providerv1.LedgerPosition{StoreId: append([]byte(nil), p.StoreID...), Seq: p.Seq, Head: append([]byte(nil), p.Head...)}
}

// AttestMintRequest is the input of MintAttestGrant.
type AttestMintRequest struct {
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// HostID is the Host the grant is for.
	HostID string
	// EnrolmentID is the host's 16-byte active enrolment.
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch Epoch
	// ChallengeNonce is the 16-byte nonce of the host's attestation
	// challenge (from its signed ChallengeAnswer).
	ChallengeNonce []byte
	// Ledger is the ledger position the host attests against.
	Ledger LedgerPosition
	// Checkpoint, when set, is a checkpoint at or below Ledger through which
	// the host can reach Ledger.
	Checkpoint *LedgerPosition
	// IssuedAt is the manager's clock at signing.
	IssuedAt time.Time
	// Lifetime is the grant's validity from IssuedAt; 0 means
	// DefaultGrantLifetime. It may not exceed MaxGrantLifetime.
	Lifetime time.Duration
}

// MintAttestGrant builds a GRANT_KIND_ATTEST GrantBody at LedgerGrantVersion
// for r (fields 1-8, 24, 30-33; every other field unset), checks it with
// grantverify.CheckAttestBody, serialises it once and has s sign those exact
// bytes. A nil signer is refused with ErrNoSigner.
//
// No manager controller calls it yet: the host ledger protocol is not wired
// to the manager.
func MintAttestGrant(ctx context.Context, s Signer, r AttestMintRequest) (*providerv1.OperationGrant, error) {
	if isNilSigner(s) {
		return nil, fmt.Errorf("mint attestation grant: %w", ErrNoSigner)
	}
	lifetime := r.Lifetime
	if lifetime == 0 {
		lifetime = DefaultGrantLifetime
	}
	if lifetime < 0 || lifetime > MaxGrantLifetime {
		return nil, fmt.Errorf("mint attestation grant: lifetime %s is outside (0, %s]", lifetime, MaxGrantLifetime)
	}
	if r.IssuedAt.IsZero() {
		return nil, errors.New("mint attestation grant: issued_at is not set")
	}
	body := &providerv1.GrantBody{
		GrantVersion:        LedgerGrantVersion,
		GrantKind:           providerv1.GrantKind_GRANT_KIND_ATTEST,
		InstallId:           append([]byte(nil), s.InstallID()...),
		ProviderUid:         r.ProviderUID,
		HostId:              r.HostID,
		EnrolmentId:         append([]byte(nil), r.EnrolmentID...),
		AuthorityGeneration: s.AuthorityGeneration(),
		Epoch:               r.Epoch.toProto(),
		SingleUseNonce:      append([]byte(nil), r.ChallengeNonce...),
		Ledger:              r.Ledger.toProto(),
		IssuedAt:            timestamppb.New(r.IssuedAt),
		NotAfter:            timestamppb.New(r.IssuedAt.Add(lifetime)),
	}
	if r.Checkpoint != nil {
		body.LedgerCheckpoint = r.Checkpoint.toProto()
	}
	if refusal := grantverify.CheckAttestBody(body); refusal != nil {
		return nil, fmt.Errorf("mint attestation grant: %w", refusal)
	}
	return signGrantBody(ctx, s, body, "mint attestation grant")
}

// LedgerMarkMintRequest is the input of MintLedgerMarkGrant.
type LedgerMarkMintRequest struct {
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// HostID is the Host the grant is for.
	HostID string
	// EnrolmentID is the host's 16-byte active enrolment.
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch Epoch
	// AttestationID is the 16-byte attestation session the mark is for (the
	// session the host's signed answers name).
	AttestationID []byte
	// Ledger is the ledger position the host's mark is raised to.
	Ledger LedgerPosition
	// IssuedAt is the manager's clock at signing.
	IssuedAt time.Time
	// Lifetime is the grant's validity from IssuedAt; 0 means
	// DefaultGrantLifetime. It may not exceed MaxGrantLifetime.
	Lifetime time.Duration
}

// MintLedgerMarkGrant builds a GRANT_KIND_LEDGER_MARK GrantBody at
// LedgerGrantVersion for r (fields 1-8, 30-32 and 34; every other field
// unset), checks it with grantverify.CheckLedgerMarkBody, serialises it once
// and has s sign those exact bytes. A nil signer is refused with
// ErrNoSigner.
//
// No manager controller calls it yet: the host ledger protocol is not wired
// to the manager.
func MintLedgerMarkGrant(ctx context.Context, s Signer, r LedgerMarkMintRequest) (*providerv1.OperationGrant, error) {
	const what = "mint ledger mark grant"
	if isNilSigner(s) {
		return nil, fmt.Errorf("%s: %w", what, ErrNoSigner)
	}
	lifetime := r.Lifetime
	if lifetime == 0 {
		lifetime = DefaultGrantLifetime
	}
	if lifetime < 0 || lifetime > MaxGrantLifetime {
		return nil, fmt.Errorf("%s: lifetime %s is outside (0, %s]", what, lifetime, MaxGrantLifetime)
	}
	if r.IssuedAt.IsZero() {
		return nil, fmt.Errorf("%s: issued_at is not set", what)
	}
	body := &providerv1.GrantBody{
		GrantVersion:        LedgerGrantVersion,
		GrantKind:           providerv1.GrantKind_GRANT_KIND_LEDGER_MARK,
		InstallId:           append([]byte(nil), s.InstallID()...),
		ProviderUid:         r.ProviderUID,
		HostId:              r.HostID,
		EnrolmentId:         append([]byte(nil), r.EnrolmentID...),
		AuthorityGeneration: s.AuthorityGeneration(),
		Epoch:               r.Epoch.toProto(),
		Ledger:              r.Ledger.toProto(),
		AttestationId:       append([]byte(nil), r.AttestationID...),
		IssuedAt:            timestamppb.New(r.IssuedAt),
		NotAfter:            timestamppb.New(r.IssuedAt.Add(lifetime)),
	}
	if refusal := grantverify.CheckLedgerMarkBody(body); refusal != nil {
		return nil, fmt.Errorf("%s: %w", what, refusal)
	}
	return signGrantBody(ctx, s, body, what)
}

// RetireMintRequest is the input of MintRetireGrant.
type RetireMintRequest struct {
	// Identity is the identity of the operation retired, from the attempt's
	// authenticated intent: its Provider, host, enrolment, epoch, VM,
	// binding, op_id, kind, attempt and mint epoch.
	Identity Identity
	// IntentDigest is the salted intent digest of the operation's exact
	// request bytes (32 bytes), as its operation grants named it. The salt
	// itself is never in a retirement grant.
	IntentDigest []byte
	// AttestationID is the 16-byte attestation session the retirement is
	// for (the session of the host's signed answer that it holds no record
	// of the operation).
	AttestationID []byte
	// IssuedAt is the manager's clock at signing.
	IssuedAt time.Time
	// Lifetime is the grant's validity from IssuedAt; 0 means
	// DefaultGrantLifetime. It may not exceed MaxGrantLifetime.
	Lifetime time.Duration
}

// MintRetireGrant builds a GRANT_KIND_RETIRE_IF_UNKNOWN GrantBody at
// LedgerGrantVersion for r (fields 1-8, 10-15, 17, 30, 31 and 34; every
// other field unset), checks it with grantverify.CheckRetireBody,
// serialises it once and has s sign those exact bytes. A nil signer is
// refused with ErrNoSigner.
func MintRetireGrant(ctx context.Context, s Signer, r RetireMintRequest) (*providerv1.OperationGrant, error) {
	const what = "mint retirement grant"
	if isNilSigner(s) {
		return nil, fmt.Errorf("%s: %w", what, ErrNoSigner)
	}
	lifetime := r.Lifetime
	if lifetime == 0 {
		lifetime = DefaultGrantLifetime
	}
	if lifetime < 0 || lifetime > MaxGrantLifetime {
		return nil, fmt.Errorf("%s: lifetime %s is outside (0, %s]", what, lifetime, MaxGrantLifetime)
	}
	if r.IssuedAt.IsZero() {
		return nil, fmt.Errorf("%s: issued_at is not set", what)
	}
	id := r.Identity
	body := &providerv1.GrantBody{
		GrantVersion:        LedgerGrantVersion,
		GrantKind:           providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN,
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
		IntentDigest:        append([]byte(nil), r.IntentDigest...),
		MintEpoch:           id.MintEpoch.toProto(),
		AttestationId:       append([]byte(nil), r.AttestationID...),
		IssuedAt:            timestamppb.New(r.IssuedAt),
		NotAfter:            timestamppb.New(r.IssuedAt.Add(lifetime)),
	}
	if refusal := grantverify.CheckRetireBody(body); refusal != nil {
		return nil, fmt.Errorf("%s: %w", what, refusal)
	}
	return signGrantBody(ctx, s, body, what)
}
