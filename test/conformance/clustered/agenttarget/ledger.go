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

// markTimeout bounds one ledger mark by the target.
const markTimeout = 10 * time.Second

// markAccepted raises host's ledger mark over an operation a mutating call
// accepted, as the manager will once the host ledger is wired to it: to the
// position the call's receipt names, in the receipt's session, or, without
// a receipt, to the agent's chain position in the session a ledger-aware
// host answer names. Like attestHost, it keeps no ledger of its own and
// trusts what the agent says (the agent's own tests cover a ledger that
// disagrees with its chain). A failure is logged to the host's log: the
// operation then stays parked until a later mark (settle marks again).
func (t *Target) markAccepted(host string, receipt *providerv1.SignedBlob) {
	h, ok := t.byName[host]
	if !ok {
		return
	}
	if err := t.mark(h, receipt); err != nil {
		h.appendLog("ledger mark by the target failed: " + err.Error())
	}
}

// mark is markAccepted for h, returning the failure.
func (t *Target) mark(h *Host, receipt *providerv1.SignedBlob) error {
	ctx, cancel := context.WithTimeout(context.Background(), markTimeout)
	defer cancel()
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return err
	}
	var session []byte
	var pos operation.LedgerPosition
	var providerUID string
	var enrolment []byte
	if receipt != nil {
		r := &providerv1.OperationReceipt{}
		if err := proto.Unmarshal(receipt.GetBody(), r); err != nil {
			return fmt.Errorf("receipt: %w", err)
		}
		session, providerUID, enrolment = r.GetAttestationId(), r.GetProviderUid(), r.GetEnrolmentId()
		pos = operation.LedgerPosition{StoreID: r.GetStoreId(), Seq: r.GetAcceptedSeq(), Head: r.GetAcceptedHead()}
	} else {
		info, err := h.Client().GetHostInfo(ctx, &providerv1.GetHostInfoRequest{AnswerNonce: n, LedgerVersion: 1})
		if err != nil {
			return fmt.Errorf("host info: %w", err)
		}
		if info.GetHostAnswer() == nil {
			return errors.New("host info: no signed answer")
		}
		ha := &providerv1.HostAnswer{}
		if err := proto.Unmarshal(info.GetHostAnswer().GetBody(), ha); err != nil {
			return fmt.Errorf("host answer: %w", err)
		}
		c := ha.GetContext()
		var p journal.Position
		if err := p.UnmarshalBinary(c.GetChainPosition()); err != nil {
			return err
		}
		session, providerUID, enrolment = c.GetAttestationId(), c.GetProviderUid(), c.GetEnrolmentId()
		pos = operation.LedgerPosition{StoreID: p.StoreID[:], Seq: p.Seq, Head: p.Head[:]}
	}
	if len(session) == 0 {
		return errors.New("the agent is not attested")
	}
	g, err := operation.MintLedgerMarkGrant(ctx, t.signer, operation.LedgerMarkMintRequest{
		ProviderUID: providerUID, HostID: h.Name, EnrolmentID: enrolment, Epoch: t.epoch, AttestationID: session,
		Ledger: pos, IssuedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	ar, err := providerv1.NewHostAuthorityClient(h.Conn()).LedgerMark(ctx,
		&providerv1.LedgerMarkRequest{TargetHostId: h.Name, AnswerNonce: n, Grant: g})
	if err != nil {
		return fmt.Errorf("ledger mark: %w", err)
	}
	if ar.GetHostAnswer() == nil {
		// Above the answer limit the mark is applied all the same.
		return nil
	}
	a := &providerv1.AuthorityAnswer{}
	if err := proto.Unmarshal(ar.GetHostAnswer().GetBody(), a); err != nil {
		return fmt.Errorf("authority answer: %w", err)
	}
	switch a.GetOutcome() {
	case providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED:
		return nil
	}
	return fmt.Errorf("ledger mark %s (%s)", a.GetOutcome(), a.GetOutcomeReason())
}

// receiptOf is the operation receipt of a mutating call's answer; nil when
// it carries none.
func receiptOf(resp any) *providerv1.SignedBlob {
	if r, ok := resp.(interface{ GetOperationReceipt() *providerv1.SignedBlob }); ok {
		return r.GetOperationReceipt()
	}
	return nil
}
