//go:build virtrigaud_dev

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

package agenttarget

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// attestTimeout bounds one attestation of a host by the target.
const attestTimeout = 10 * time.Second

// attestHost attests h's agent, as the manager will once the host ledger is
// wired to it: an ATTEST challenge, then an attestation grant at the
// agent's own chain position (this target keeps no ledger of its own, so it
// trusts the agent's position: a rollback the crash-point suite injects is
// not detected here, which the agent's own tests cover).
func (t *Target) attestHost(h *Host) error {
	ctx, cancel := context.WithTimeout(context.Background(), attestTimeout)
	defer cancel()
	ha := providerv1.NewHostAuthorityClient(h.Conn())
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return err
	}
	resp, err := ha.Challenge(ctx, &providerv1.ChallengeRequest{TargetHostId: h.Name, AnswerNonce: n,
		Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST})
	if err != nil {
		return fmt.Errorf("challenge: %w", err)
	}
	if resp.GetHostAnswer() == nil {
		return errors.New("challenge: no signed answer")
	}
	ch := &providerv1.ChallengeAnswer{}
	if err := proto.Unmarshal(resp.GetHostAnswer().GetBody(), ch); err != nil {
		return fmt.Errorf("challenge answer: %w", err)
	}
	if len(ch.GetChallengeNonce()) == 0 {
		return fmt.Errorf("challenge not issued: %s", ch.GetNotIssuedReason())
	}
	var pos journal.Position
	if err := pos.UnmarshalBinary(ch.GetContext().GetChainPosition()); err != nil {
		return err
	}
	g, err := operation.MintAttestGrant(ctx, t.signer, operation.AttestMintRequest{
		ProviderUID: ch.GetContext().GetProviderUid(), HostID: h.Name, EnrolmentID: ch.GetContext().GetEnrolmentId(),
		Epoch: t.epoch, ChallengeNonce: ch.GetChallengeNonce(),
		Ledger:   operation.LedgerPosition{StoreID: pos.StoreID[:], Seq: pos.Seq, Head: pos.Head[:]},
		IssuedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	ar, err := ha.Attest(ctx, &providerv1.AttestRequest{TargetHostId: h.Name, AnswerNonce: n, Grant: g})
	if err != nil {
		return fmt.Errorf("attest: %w", err)
	}
	a := &providerv1.AuthorityAnswer{}
	if err := proto.Unmarshal(ar.GetHostAnswer().GetBody(), a); err != nil {
		return fmt.Errorf("authority answer: %w", err)
	}
	if a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED {
		return fmt.Errorf("attestation %s (%s)", a.GetOutcome(), a.GetOutcomeReason())
	}
	return nil
}
