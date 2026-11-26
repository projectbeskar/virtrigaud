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

package opstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// Attestation is the agent's current attestation session against the
// manager's host ledger. It is held in memory only: every start begins
// without one, awaiting attestation.
type Attestation struct {
	// Session is the 16-byte attestation id: the challenge nonce of the last
	// attestation applied since the store opened.
	Session []byte
	// Mark is the ledger mark: the highest ledger position an attestation
	// confirmed on the store's chain.
	Mark journal.Position
}

// AttestRequest is what one verified attestation grant asks of the store.
type AttestRequest struct {
	// Session is the challenge nonce the grant names; it becomes the
	// attestation id.
	Session []byte
	// Ledger is the manager's ledger position for this store.
	Ledger journal.Position
	// Checkpoint, when set, is a chain checkpoint at or below Ledger.Seq
	// that the store may hold when it holds no head at Ledger.Seq (its
	// StoreID is not used).
	Checkpoint *journal.Position
	// Reattest is true for a re-attestation of an attested agent, false for
	// the first attestation after a start.
	Reattest bool
}

// AttestResult is the outcome of an attestation the store applied or that
// quarantined it.
type AttestResult struct {
	// First is true when this was the first attestation since the store
	// opened: the agent's start sequence may go on.
	First bool
	// Attestation is the session after the call; zero when Quarantined is
	// set.
	Attestation Attestation
	// Quarantined is the lost reason the attestation recorded; empty when it
	// was applied.
	Quarantined journal.LostReason
}

// ErrAttestState is returned for a first attestation while the store is
// attested, or a re-attestation while it awaits its first one. Nothing is
// changed.
var ErrAttestState = errors.New("the attestation does not match the store's attestation state")

// Attestation returns the store's current attestation session; ok is false
// while the store awaits its first attestation. It does not wait for the
// store lock.
func (s *Store) Attestation() (a Attestation, ok bool) {
	p := s.attestation.Load()
	if p == nil {
		return Attestation{}, false
	}
	return Attestation{Session: bytes.Clone(p.Session), Mark: p.Mark}, true
}

// Attest checks a manager's ledger position against the store's chain and,
// when it passes, makes r.Session the store's attestation session, under the
// store lock:
//
//   - a position that names another store quarantines the store
//     (ledger_store_mismatch);
//   - a position beyond the store's chain quarantines it (ledger_ahead);
//   - the store's head at the position must equal the position's head
//     (otherwise ledger_mismatch); when the store holds no head there, a
//     checkpoint at or below the position whose head the store holds and
//     equals passes, a held checkpoint head that differs is ledger_mismatch,
//     and anything else is ledger_unverifiable;
//   - when it passes, the mark becomes the position on a first attestation
//     and the higher of the old mark and the position on a re-attestation,
//     and no checkpoint prunes the chain at or above the mark from then on.
//
// A quarantine is returned in AttestResult.Quarantined, not as an error. A
// store that is already lost returns an error wrapping journal.ErrLost; a
// request that does not match the store's attestation state returns
// ErrAttestState. Neither changes anything.
func (s *Store) Attest(ctx context.Context, r AttestRequest) (AttestResult, error) {
	if len(r.Session) != idLen || r.Ledger.Seq == 0 {
		return AttestResult{}, fmt.Errorf("attest: %w: malformed session or ledger position", ErrAttestState)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, lost := s.j.State(); st == journal.StateLost {
		return AttestResult{}, fmt.Errorf("attest: %w", &journal.LostError{Reason: lost})
	}
	cur := s.attestation.Load()
	if r.Reattest != (cur != nil) {
		return AttestResult{}, fmt.Errorf("attest: %w", ErrAttestState)
	}
	var verdict journal.LostReason
	var detail string
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		pos, ok := tx.CommittedPosition()
		if !ok {
			return errors.New("store position unreadable")
		}
		verdict, detail = checkLedger(tx, pos, r)
		return nil
	}); err != nil {
		return AttestResult{}, fmt.Errorf("attest: %w", err)
	}
	if verdict != "" {
		s.j.Quarantine(verdict, detail)
		return AttestResult{Quarantined: verdict}, nil
	}
	next := &Attestation{Session: bytes.Clone(r.Session), Mark: r.Ledger}
	if cur != nil && cur.Mark.Seq > r.Ledger.Seq && !guard.Off(guard.AttestMarkMax) {
		next.Mark = cur.Mark
	}
	s.attestation.Store(next)
	s.j.SetRetentionFloor(next.Mark.Seq)
	return AttestResult{First: cur == nil, Attestation: Attestation{Session: bytes.Clone(next.Session), Mark: next.Mark}}, nil
}

// checkLedger compares the ledger position of r with the store's chain at
// the committed position pos, in tx. It returns the lost reason and a short
// detail when the position is not on the chain, or "" when it is.
func checkLedger(tx *journal.Tx, pos journal.Position, r AttestRequest) (journal.LostReason, string) {
	if r.Ledger.StoreID != pos.StoreID && !guard.Off(guard.AttestStoreMatch) {
		return journal.LostLedgerStoreMismatch, "the attested ledger names another store"
	}
	if r.Ledger.Seq > pos.Seq && !guard.Off(guard.AttestNotAhead) {
		return journal.LostLedgerAhead, fmt.Sprintf("the attested ledger position %d is beyond the chain (%d)", r.Ledger.Seq, pos.Seq)
	}
	if guard.Off(guard.AttestHeadCompare) {
		return "", ""
	}
	if head, ok := tx.HeadAt(r.Ledger.Seq); ok {
		if head != r.Ledger.Head {
			return journal.LostLedgerMismatch, fmt.Sprintf("the chain head at %d differs from the attested ledger", r.Ledger.Seq)
		}
		return "", ""
	}
	cp := r.Checkpoint
	if cp == nil || cp.Seq == 0 || cp.Seq > r.Ledger.Seq {
		return journal.LostLedgerUnverifiable, fmt.Sprintf("the store holds no head at %d and no attested checkpoint", r.Ledger.Seq)
	}
	head, ok := tx.HeadAt(cp.Seq)
	switch {
	case !ok:
		return journal.LostLedgerUnverifiable, fmt.Sprintf("the store holds no head at %d nor at the checkpoint %d", r.Ledger.Seq, cp.Seq)
	case head != cp.Head:
		return journal.LostLedgerMismatch, fmt.Sprintf("the chain head at the checkpoint %d differs from the attested ledger", cp.Seq)
	}
	return "", ""
}
