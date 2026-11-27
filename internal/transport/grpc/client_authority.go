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

package grpc

import (
	"context"
	"fmt"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Compile-time assertion that the client calls the host authority service.
var _ contracts.HostAuthorityCaller = (*Client)(nil)

// authorityCallTimeout bounds one host authority call.
const authorityCallTimeout = 30 * time.Second

// ledgerPosition converts p for a mint request.
func ledgerPosition(p contracts.LedgerPosition) operation.LedgerPosition {
	return operation.LedgerPosition{StoreID: append([]byte(nil), p.StoreID...), Seq: p.Seq, Head: append([]byte(nil), p.Head...)}
}

// authorityClient returns the host authority client of a client in
// operation-identity mode, or operation.ErrNoSigner.
func (c *Client) authorityClient(what string) (providerv1.HostAuthorityClient, error) {
	if !operation.SignerCompiled || c.opSigner == nil {
		return nil, fmt.Errorf("%s: %w", what, operation.ErrNoSigner)
	}
	return providerv1.NewHostAuthorityClient(c.conn), nil
}

// Challenge implements contracts.HostAuthorityCaller.
func (c *Client) Challenge(ctx context.Context, q contracts.ChallengeQuery) (contracts.AuthorityRead, error) {
	ha, err := c.authorityClient("challenge")
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	read := contracts.AuthorityRead{SignedRead: contracts.SignedRead{Nonce: nonce}}
	ctx, cancel := context.WithTimeout(ctx, authorityCallTimeout)
	defer cancel()
	resp, err := ha.Challenge(ctx, &providerv1.ChallengeRequest{TargetHostId: q.HostID, AnswerNonce: nonce, Purpose: q.Purpose})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return read, c.mapGRPCError("challenge", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return read, nil
}

// Attest implements contracts.HostAuthorityCaller: it mints an attestation
// grant over exactly what o names and delivers it.
func (c *Client) Attest(ctx context.Context, o contracts.AttestOrder) (contracts.AuthorityRead, error) {
	ha, err := c.authorityClient("attest")
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	req := operation.AttestMintRequest{ProviderUID: o.ProviderUID, HostID: o.HostID, EnrolmentID: o.EnrolmentID,
		Epoch: operation.Epoch{Counter: o.Epoch.Counter, Holder: o.Epoch.Holder}, ChallengeNonce: o.ChallengeNonce,
		Ledger: ledgerPosition(o.Ledger), IssuedAt: time.Now()}
	if o.Checkpoint != nil {
		cp := ledgerPosition(*o.Checkpoint)
		req.Checkpoint = &cp
	}
	grant, err := operation.MintAttestGrant(ctx, c.opSigner, req)
	if err != nil {
		return contracts.AuthorityRead{}, fmt.Errorf("attest: %w", err)
	}
	read := contracts.AuthorityRead{SignedRead: contracts.SignedRead{Nonce: nonce}, GrantDigest: grantverify.GrantDigest(grant)}
	ctx, cancel := context.WithTimeout(ctx, authorityCallTimeout)
	defer cancel()
	resp, err := ha.Attest(ctx, &providerv1.AttestRequest{TargetHostId: o.HostID, AnswerNonce: nonce, Grant: grant})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return read, c.mapGRPCError("attest", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return read, nil
}

// LedgerMark implements contracts.HostAuthorityCaller: it mints a ledger
// mark grant over exactly what o names and delivers it.
func (c *Client) LedgerMark(ctx context.Context, o contracts.LedgerMarkOrder) (contracts.AuthorityRead, error) {
	ha, err := c.authorityClient("ledger mark")
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	grant, err := operation.MintLedgerMarkGrant(ctx, c.opSigner, operation.LedgerMarkMintRequest{ProviderUID: o.ProviderUID,
		HostID: o.HostID, EnrolmentID: o.EnrolmentID, Epoch: operation.Epoch{Counter: o.Epoch.Counter, Holder: o.Epoch.Holder},
		AttestationID: o.AttestationID, Ledger: ledgerPosition(o.Ledger), IssuedAt: time.Now()})
	if err != nil {
		return contracts.AuthorityRead{}, fmt.Errorf("ledger mark: %w", err)
	}
	read := contracts.AuthorityRead{SignedRead: contracts.SignedRead{Nonce: nonce}, GrantDigest: grantverify.GrantDigest(grant)}
	ctx, cancel := context.WithTimeout(ctx, authorityCallTimeout)
	defer cancel()
	resp, err := ha.LedgerMark(ctx, &providerv1.LedgerMarkRequest{TargetHostId: o.HostID, AnswerNonce: nonce, Grant: grant})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return read, c.mapGRPCError("ledgerMark", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return read, nil
}

// RetireIfUnknown implements contracts.HostAuthorityCaller: it mints a
// retire-if-unknown grant over exactly what o names and delivers it.
func (c *Client) RetireIfUnknown(ctx context.Context, o contracts.RetireOrder) (contracts.AuthorityRead, error) {
	ha, err := c.authorityClient("retire-if-unknown")
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	nonce, err := drawAnswerNonce()
	if err != nil {
		return contracts.AuthorityRead{}, err
	}
	id := o.Identity
	grant, err := operation.MintRetireGrant(ctx, c.opSigner, operation.RetireMintRequest{
		Identity: operation.Identity{ProviderUID: id.ProviderUID, HostID: id.HostID, EnrolmentID: id.EnrolmentID,
			Epoch: operation.Epoch{Counter: id.Epoch.Counter, Holder: id.Epoch.Holder}, VMUID: id.VMUID, BindingID: id.BindingID,
			OpID: id.OpID, Kind: o.Kind, Attempt: id.Attempt,
			MintEpoch: operation.Epoch{Counter: id.MintEpoch.Counter, Holder: id.MintEpoch.Holder}},
		IntentDigest: o.IntentDigest, AttestationID: o.AttestationID, IssuedAt: time.Now()})
	if err != nil {
		return contracts.AuthorityRead{}, fmt.Errorf("retire-if-unknown: %w", err)
	}
	read := contracts.AuthorityRead{SignedRead: contracts.SignedRead{Nonce: nonce}, GrantDigest: grantverify.GrantDigest(grant)}
	ctx, cancel := context.WithTimeout(ctx, authorityCallTimeout)
	defer cancel()
	resp, err := ha.RetireIfUnknown(ctx, &providerv1.RetireIfUnknownRequest{TargetHostId: id.HostID, AnswerNonce: nonce, Grant: grant})
	if err != nil {
		read.Answer = answerFromStatus(err)
		return read, c.mapGRPCError("retireIfUnknown", err)
	}
	read.Answer = answerFromField(resp.GetHostAnswer())
	return read, nil
}
