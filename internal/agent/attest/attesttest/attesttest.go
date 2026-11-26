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

// Package attesttest attests a per-host agent in tests, as a manager will:
// it asks for an ATTEST challenge, takes the agent's own chain position from
// the challenge answer as the ledger position, and delivers an attestation
// grant signed by a grant-signing chain under a test grant anchor.
//
// It is for tests only. It trusts the challenge answer without verifying
// it, which a manager never does, and it imports no Kubernetes package, so
// that the agent's tests may use it.
package attesttest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// positionSize is the size of AnswerContext.chain_position: store id, be64
// seq, head.
const positionSize = 16 + 8 + 32

// Signer is a grant-signing chain (a path length 0 issuer and a leaf) under
// a test grant anchor.
type Signer struct {
	key   *ecdsa.PrivateKey
	chain [][]byte
}

// NewSigner issues a grant-signing chain under the anchor (cert, key).
func NewSigner(anchor *x509.Certificate, anchorKey crypto.Signer) (*Signer, error) {
	now := time.Now()
	ik, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	issuerDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "test grant issuer"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign},
		anchor, &ik.PublicKey, anchorKey)
	if err != nil {
		return nil, fmt.Errorf("issue grant issuer: %w", err)
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		return nil, err
	}
	lk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(3),
		Subject: pkix.Name{CommonName: "test grant leaf"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature}, issuer, &lk.PublicKey, ik)
	if err != nil {
		return nil, fmt.Errorf("issue grant leaf: %w", err)
	}
	return &Signer{key: lk, chain: [][]byte{leafDER, issuerDER}}, nil
}

// Sign signs the exact bytes of body as a grant.
func (s *Signer) Sign(body *providerv1.GrantBody) (*providerv1.OperationGrant, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		return nil, err
	}
	d := sha256.Sum256(raw)
	sig, err := s.key.Sign(rand.Reader, d[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	if sig, err = grantverify.NormalizeLowS(sig, elliptic.P256()); err != nil {
		return nil, err
	}
	return &providerv1.OperationGrant{Body: raw, Signature: sig, SignerCertChain: s.chain}, nil
}

// Target is the host a grant is for.
type Target struct {
	// ProviderUID, HostID, InstallID and EnrolmentID are the host's
	// enrolment as its certificate names it.
	ProviderUID string
	// HostID is the host identity.
	HostID string
	// InstallID is the 16-byte installation.
	InstallID []byte
	// EnrolmentID is the 16-byte active enrolment.
	EnrolmentID []byte
}

// AttestGrant is an attestation grant for t naming challenge nonce at
// ledger, valid for five minutes from now.
func (s *Signer) AttestGrant(t Target, nonce []byte, ledger *providerv1.LedgerPosition) (*providerv1.OperationGrant, error) {
	now := time.Now()
	body := &providerv1.GrantBody{
		GrantVersion: grantverify.LedgerGrantVersion, GrantKind: providerv1.GrantKind_GRANT_KIND_ATTEST,
		InstallId: t.InstallID, ProviderUid: t.ProviderUID, HostId: t.HostID, EnrolmentId: t.EnrolmentID,
		AuthorityGeneration: 1, Epoch: &providerv1.Epoch{Counter: 1, Holder: make([]byte, 16)},
		SingleUseNonce: nonce, Ledger: ledger,
		IssuedAt: timestamppb.New(now.Add(-time.Minute)), NotAfter: timestamppb.New(now.Add(5 * time.Minute)),
	}
	if r := grantverify.CheckAttestBody(body); r != nil {
		return nil, r
	}
	return s.Sign(body)
}

// Attest attests the agent of hostID behind cc at its own chain position: a
// Challenge(ATTEST), then an Attest with the challenge answer's chain
// position as the ledger and its enrolment (Provider, installation,
// enrolment) as the grant's. It returns the agent's authority answer, or an
// error when no challenge was issued or no answer was signed.
func Attest(ctx context.Context, cc grpc.ClientConnInterface, s *Signer, hostID string) (*providerv1.AuthorityAnswer, error) {
	return AttestAt(ctx, cc, s, hostID, nil)
}

// AttestAt is Attest at the ledger position ledger instead of the agent's
// own chain position (nil: the agent's).
func AttestAt(ctx context.Context, cc grpc.ClientConnInterface, s *Signer, hostID string, ledger *providerv1.LedgerPosition) (*providerv1.AuthorityAnswer, error) {
	ha := providerv1.NewHostAuthorityClient(cc)
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	resp, err := ha.Challenge(ctx, &providerv1.ChallengeRequest{TargetHostId: hostID, AnswerNonce: n,
		Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST})
	if err != nil {
		return nil, fmt.Errorf("challenge: %w", err)
	}
	if resp.GetHostAnswer() == nil {
		return nil, errors.New("challenge: no signed answer")
	}
	ch := &providerv1.ChallengeAnswer{}
	if err := proto.Unmarshal(resp.GetHostAnswer().GetBody(), ch); err != nil {
		return nil, fmt.Errorf("challenge answer: %w", err)
	}
	if len(ch.GetChallengeNonce()) == 0 {
		return nil, fmt.Errorf("challenge not issued: %s", ch.GetNotIssuedReason())
	}
	pos := ch.GetContext().GetChainPosition()
	if len(pos) != positionSize {
		return nil, fmt.Errorf("chain position of %d bytes", len(pos))
	}
	if ledger == nil {
		ledger = &providerv1.LedgerPosition{StoreId: pos[:16], Seq: beUint64(pos[16:24]), Head: pos[24:]}
	}
	c := ch.GetContext()
	t := Target{ProviderUID: c.GetProviderUid(), HostID: hostID, InstallID: c.GetInstallId(), EnrolmentID: c.GetEnrolmentId()}
	g, err := s.AttestGrant(t, ch.GetChallengeNonce(), ledger)
	if err != nil {
		return nil, err
	}
	ar, err := ha.Attest(ctx, &providerv1.AttestRequest{TargetHostId: hostID, AnswerNonce: n, Grant: g})
	if err != nil {
		return nil, fmt.Errorf("attest: %w", err)
	}
	if ar.GetHostAnswer() == nil {
		return nil, errors.New("attest: no signed answer")
	}
	a := &providerv1.AuthorityAnswer{}
	if err := proto.Unmarshal(ar.GetHostAnswer().GetBody(), a); err != nil {
		return nil, fmt.Errorf("authority answer: %w", err)
	}
	return a, nil
}

// LedgerMarkGrant is a ledger mark grant for t naming the attestation
// session at ledger, valid for five minutes from now.
func (s *Signer) LedgerMarkGrant(t Target, session []byte, ledger *providerv1.LedgerPosition) (*providerv1.OperationGrant, error) {
	now := time.Now()
	body := &providerv1.GrantBody{
		GrantVersion: grantverify.LedgerGrantVersion, GrantKind: providerv1.GrantKind_GRANT_KIND_LEDGER_MARK,
		InstallId: t.InstallID, ProviderUid: t.ProviderUID, HostId: t.HostID, EnrolmentId: t.EnrolmentID,
		AuthorityGeneration: 1, Epoch: &providerv1.Epoch{Counter: 1, Holder: make([]byte, 16)},
		AttestationId: session, Ledger: ledger,
		IssuedAt: timestamppb.New(now.Add(-time.Minute)), NotAfter: timestamppb.New(now.Add(5 * time.Minute)),
	}
	if r := grantverify.CheckLedgerMarkBody(body); r != nil {
		return nil, r
	}
	return s.Sign(body)
}

// Mark raises the ledger mark of the agent of hostID behind cc, as a
// manager's heartbeat raise does: a GetHostInfo that asks for the ledger
// fields gives the session and the chain position, and a ledger mark grant
// for that session at ledger (nil: that chain position) is delivered. It
// returns the agent's authority answer, or an error when the agent is not
// attested or no answer was signed.
func Mark(ctx context.Context, cc grpc.ClientConnInterface, s *Signer, hostID string, ledger *providerv1.LedgerPosition) (*providerv1.AuthorityAnswer, error) {
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	info, err := providerv1.NewProviderClient(cc).GetHostInfo(ctx, &providerv1.GetHostInfoRequest{AnswerNonce: n, LedgerVersion: 1})
	if err != nil {
		return nil, fmt.Errorf("host info: %w", err)
	}
	if info.GetHostAnswer() == nil {
		return nil, errors.New("host info: no signed answer")
	}
	ha := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(info.GetHostAnswer().GetBody(), ha); err != nil {
		return nil, fmt.Errorf("host answer: %w", err)
	}
	c := ha.GetContext()
	if len(c.GetAttestationId()) != 16 {
		return nil, errors.New("the agent is not attested")
	}
	pos := c.GetChainPosition()
	if len(pos) != positionSize {
		return nil, fmt.Errorf("chain position of %d bytes", len(pos))
	}
	if ledger == nil {
		ledger = &providerv1.LedgerPosition{StoreId: pos[:16], Seq: beUint64(pos[16:24]), Head: pos[24:]}
	}
	t := Target{ProviderUID: c.GetProviderUid(), HostID: hostID, InstallID: c.GetInstallId(), EnrolmentID: c.GetEnrolmentId()}
	g, err := s.LedgerMarkGrant(t, c.GetAttestationId(), ledger)
	if err != nil {
		return nil, err
	}
	ar, err := providerv1.NewHostAuthorityClient(cc).LedgerMark(ctx, &providerv1.LedgerMarkRequest{TargetHostId: hostID, AnswerNonce: n, Grant: g})
	if err != nil {
		return nil, fmt.Errorf("ledger mark: %w", err)
	}
	if ar.GetHostAnswer() == nil {
		return nil, errors.New("ledger mark: no signed answer")
	}
	a := &providerv1.AuthorityAnswer{}
	if err := proto.Unmarshal(ar.GetHostAnswer().GetBody(), a); err != nil {
		return nil, fmt.Errorf("authority answer: %w", err)
	}
	return a, nil
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}
