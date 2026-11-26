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
	"sort"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The ledger gate (Options.LedgerGate): an operation the store accepted is
// run only once the manager's host ledger covers its acceptance, that is once
// the ledger mark of the current attestation session is at or above the
// chain position at which it was accepted. Until then it is parked: nothing
// about it is committed, and the request bytes the agent holds for it stay
// in memory, within a budget.
const (
	// DefaultMaxPendingReceipts is the default number of accepted
	// operations above the ledger mark beyond which new operations are
	// refused (RefusalLedgerBehind).
	DefaultMaxPendingReceipts = 4096
	// DefaultParkedRequestBudget is the default bound on the request bytes
	// held in memory for parked operations: a new operation whose request
	// would take them above it is refused (RefusalLedgerBehind).
	DefaultParkedRequestBudget = 256 << 20
	// MaxReportedPendingReceipts caps the pending receipt count a host
	// answer reports.
	MaxReportedPendingReceipts = 65535
)

// ErrAwaitingLedger is returned by Execute for an accepted operation whose
// acceptance the ledger mark does not cover yet: nothing is committed, and
// the operation runs once a ledger mark covers it (LedgerWake is closed when
// the mark rises).
var ErrAwaitingLedger = errors.New("the operation's acceptance is not covered by the host ledger yet")

// ErrAwaitingRequest is returned by Execute for an accepted operation whose
// acceptance the ledger mark covers but whose request bytes the agent does
// not hold (they were lost by a restart, or did not fit the parked request
// budget): nothing is committed, and the operation runs once a delivery of
// the same op_id brings its request again (DepositRequest).
var ErrAwaitingRequest = errors.New("the operation's request bytes are not held; awaiting a delivery of the same op_id")

// ErrStaleAttestation is returned by LedgerMark for a mark that names
// another attestation session than the store's current one. Nothing is
// changed, and the store is not quarantined.
var ErrStaleAttestation = errors.New("the ledger mark names another attestation session")

// LedgerMarkOutcome is what a ledger mark did.
type LedgerMarkOutcome int

const (
	// LedgerMarkApplied: the mark rose to the ledger position.
	LedgerMarkApplied LedgerMarkOutcome = iota + 1
	// LedgerMarkAlreadyApplied: the mark was already at or above the
	// position, which is on the store's chain; nothing changed.
	LedgerMarkAlreadyApplied
	// LedgerMarkQuarantined: the position is not on the store's chain; the
	// store is quarantined (LedgerMarkResult.Quarantined says why).
	LedgerMarkQuarantined
)

// LedgerMarkRequest is what one verified ledger mark grant asks of the
// store.
type LedgerMarkRequest struct {
	// Session is the attestation session the grant names.
	Session []byte
	// Ledger is the manager's ledger position for this store.
	Ledger journal.Position
}

// LedgerMarkResult is the outcome of a ledger mark.
type LedgerMarkResult struct {
	// Outcome is what the mark did.
	Outcome LedgerMarkOutcome
	// Attestation is the session after the call; zero when the store was
	// quarantined.
	Attestation Attestation
	// Quarantined is the lost reason recorded; empty unless Outcome is
	// LedgerMarkQuarantined.
	Quarantined journal.LostReason
}

// LedgerBacklog is what the gate holds back, for a host answer's ledger
// state.
type LedgerBacklog struct {
	// PendingReceipts is the number of accepted operations whose acceptance
	// the ledger mark does not cover, capped at MaxReportedPendingReceipts.
	PendingReceipts uint32
	// ParkedRequestBytes is the request bytes held for parked operations.
	ParkedRequestBytes uint64
	// ParkedRequestBudgetBytes is the budget of ParkedRequestBytes.
	ParkedRequestBudgetBytes uint64
}

// pendingOp is an accepted operation above the ledger mark.
type pendingOp struct {
	key string
	seq uint64
}

// LedgerGate reports whether the store gates the first step of an accepted
// operation on the ledger mark (Options.LedgerGate).
func (s *Store) LedgerGate() bool { return s.opts.LedgerGate }

// LedgerWake returns a channel that is closed the next time a parked
// operation may be able to run: the ledger mark rose, a request was
// deposited, or a re-enrolment froze operations. Take it before calling
// Execute, so that a change between the call and the wait is not missed.
func (s *Store) LedgerWake() <-chan struct{} {
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	return s.wake
}

// wakeParkedLocked closes the wake channel and makes a new one. s.parkMu is
// held.
func (s *Store) wakeParkedLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// wakeParked is wakeParkedLocked, taking s.parkMu.
func (s *Store) wakeParked() {
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	s.wakeParkedLocked()
}

// Backlog returns what the gate holds back. It does not wait for the store
// lock.
func (s *Store) Backlog() LedgerBacklog {
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	n := len(s.pending)
	if n > MaxReportedPendingReceipts {
		n = MaxReportedPendingReceipts
	}
	return LedgerBacklog{
		PendingReceipts:          uint32(n),              // #nosec G115 -- capped above
		ParkedRequestBytes:       uint64(s.parkedBytes),  // #nosec G115 -- never negative
		ParkedRequestBudgetBytes: uint64(s.parkBudget()), // #nosec G115 -- positive
	}
}

// parkBudget is the parked request budget.
func (s *Store) parkBudget() int64 {
	if s.opts.ParkedRequestBudget > 0 {
		return s.opts.ParkedRequestBudget
	}
	return DefaultParkedRequestBudget
}

// maxPending is the pending receipt bound.
func (s *Store) maxPending() int {
	if s.opts.MaxPendingReceipts > 0 {
		return s.opts.MaxPendingReceipts
	}
	return DefaultMaxPendingReceipts
}

// DepositRequest gives the store the request bytes of an operation that is
// parked without them (ErrAwaitingRequest), when they fit the parked request
// budget, and wakes the parked operations. It does nothing for an operation
// that is not parked, or that already has its request.
func (s *Store) DepositRequest(ref OpRef, payload []byte) {
	if !s.opts.LedgerGate || payload == nil {
		return
	}
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	k := runKey(ref)
	if held, ok := s.parked[k]; !ok || held != nil {
		return
	}
	if s.depositLocked(k, payload) {
		s.wakeParkedLocked()
	}
}

// depositLocked holds payload for the parked operation k when it fits the
// budget and reports whether it did. s.parkMu is held, and k holds no
// request.
func (s *Store) depositLocked(k string, payload []byte) bool {
	n := int64(len(payload))
	if s.parkedBytes+n > s.parkBudget() && !guard.Off(guard.LedgerBehindBytes) {
		return false
	}
	s.parked[k] = bytes.Clone(payload)
	s.parkedBytes += n
	return true
}

// unparkLocked forgets the parked operation k and its request. s.parkMu is
// held.
func (s *Store) unparkLocked(k string) {
	if held, ok := s.parked[k]; ok {
		s.parkedBytes -= int64(len(held))
		delete(s.parked, k)
	}
}

// unpark is unparkLocked, taking s.parkMu.
func (s *Store) unpark(k string) {
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	s.unparkLocked(k)
}

// covered reports whether the ledger mark of the current attestation session
// covers rec's acceptance. A record that names no acceptance position is
// never covered.
func (s *Store) covered(rec *pb.OpRecord) bool {
	a := s.attestation.Load()
	return a != nil && rec.GetAcceptedSeq() != 0 && rec.GetAcceptedSeq() <= a.Mark.Seq
}

// gated reports whether the ledger gate applies.
func (s *Store) gated() bool { return s.opts.LedgerGate && !guard.Off(guard.LedgerGate) }

// gateLocked decides whether the accepted operation rec (key k, no step
// intent yet) may start, with payload the request bytes of this call (nil
// when it brings none). s.mu is held. It returns the request bytes to run
// with: payload, or the bytes the store held for it, which it then forgets.
// Otherwise the operation stays parked, keeping the request bytes it has
// (within the budget), and the error is ErrAwaitingLedger or
// ErrAwaitingRequest.
func (s *Store) gateLocked(k string, rec *pb.OpRecord, payload []byte) ([]byte, error) {
	if !s.gated() {
		return payload, nil
	}
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	held, parked := s.parked[k]
	switch {
	case payload == nil:
		payload = held
	case !parked:
		if !s.depositLocked(k, payload) {
			s.parked[k] = nil
		}
	case held == nil:
		s.depositLocked(k, payload)
	}
	if !s.covered(rec) {
		if _, ok := s.parked[k]; !ok {
			s.parked[k] = nil
		}
		return nil, ErrAwaitingLedger
	}
	if payload == nil && !s.runsWithoutRequest(providerv1.OperationKind(rec.GetIdentity().GetKind())) &&
		!guard.Off(guard.AwaitRequest) {
		if _, ok := s.parked[k]; !ok {
			s.parked[k] = nil
		}
		return nil, ErrAwaitingRequest
	}
	s.unparkLocked(k)
	return payload, nil
}

// runsWithoutRequest reports whether the executor runs operations of kind
// without their request bytes (executor.RequestOptional).
func (s *Store) runsWithoutRequest(kind providerv1.OperationKind) bool {
	o, ok := s.exec.(executor.RequestOptional)
	return ok && o.RunsWithoutRequest(kind)
}

// awaitingRequest reports whether the accepted operation ref, whose
// acceptance the ledger mark covers, cannot start because the agent holds no
// request bytes for it (op is its record as the caller read it).
func (s *Store) awaitingRequest(ref OpRef, op OpView) bool {
	if !s.opts.LedgerGate || op.State != StateAccepted || op.Tombstoned {
		return false
	}
	a := s.attestation.Load()
	if a == nil || op.AcceptedSeq == 0 || op.AcceptedSeq > a.Mark.Seq || s.runsWithoutRequest(op.Kind) {
		return false
	}
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	return s.parked[runKey(ref)] == nil
}

// admitBacklogLocked applies the backlog bound to a new operation whose
// request is payload: it is refused while MaxPendingReceipts accepted
// operations sit above the ledger mark, or when payload would take the
// parked request bytes above their budget. s.mu is held.
func (s *Store) admitBacklogLocked(payload []byte) bool {
	if !s.opts.LedgerGate {
		return true
	}
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	if len(s.pending) >= s.maxPending() && !guard.Off(guard.LedgerBehindCount) {
		return false
	}
	if s.parkedBytes+int64(len(payload)) > s.parkBudget() && !guard.Off(guard.LedgerBehindBytes) {
		return false
	}
	return true
}

// parkAcceptedLocked records a newly accepted operation (key k, accepted at
// seq) as pending above the mark, with its request bytes. s.mu is held, and
// admitBacklogLocked admitted it.
func (s *Store) parkAcceptedLocked(k string, seq uint64, payload []byte) {
	if !s.opts.LedgerGate {
		return
	}
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	s.pending = append(s.pending, pendingOp{key: k, seq: seq})
	if _, ok := s.parked[k]; !ok {
		if payload == nil || !s.depositLocked(k, payload) {
			s.parked[k] = nil
		}
	}
}

// prunePendingLocked forgets the pending operations at or below mark.
// s.parkMu is held.
func (s *Store) prunePendingLocked(mark uint64) {
	i := sort.Search(len(s.pending), func(i int) bool { return s.pending[i].seq > mark })
	s.pending = append(s.pending[:0:0], s.pending[i:]...)
}

// rebuildBacklogLocked recomputes, in one read, the accepted operations
// above mark, and forgets the parked requests of operations that are no
// longer accepted. s.mu is held.
func (s *Store) rebuildBacklogLocked(ctx context.Context, mark uint64) error {
	if !s.opts.LedgerGate {
		return nil
	}
	var pending []pendingOp
	accepted := map[string]bool{}
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEach(bucketOps, nil, func(_, raw []byte) error {
			rec := &pb.OpRecord{}
			if err := unmarshalRecord(raw, rec); err != nil {
				return err
			}
			if rec.GetState() != StateAccepted {
				return nil
			}
			k := runKey(OpRef{EnrolmentID: rec.GetIdentity().GetEnrolmentId(), OpID: rec.GetIdentity().GetOpId()})
			accepted[k] = true
			if rec.GetAcceptedSeq() == 0 || rec.GetAcceptedSeq() > mark {
				pending = append(pending, pendingOp{key: k, seq: rec.GetAcceptedSeq()})
			}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("read the accepted operations: %w", err)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].seq < pending[j].seq })
	s.parkMu.Lock()
	defer s.parkMu.Unlock()
	s.pending = pending
	for k := range s.parked {
		if !accepted[k] {
			s.unparkLocked(k)
		}
	}
	return nil
}

// LedgerMark raises the ledger mark of the current attestation session to a
// manager's ledger position, under the store lock:
//
//   - a store that awaits its first attestation returns ErrAttestState, and
//     a mark for another session ErrStaleAttestation (neither changes
//     anything nor quarantines);
//   - a position that names another store quarantines the store
//     (ledger_store_mismatch), one beyond the store's chain ledger_ahead;
//   - the store's head at the position must equal the position's head
//     (otherwise ledger_mismatch). At or below the mark, a head the store no
//     longer holds is not compared and the mark is already applied; above
//     it, the store keeps every head (the retention floor), so a head it
//     does not hold quarantines it (ledger_unverifiable);
//   - a position above the mark raises it (and the retention floor), and the
//     parked operations it now covers are woken; at or below it nothing
//     changes (LedgerMarkAlreadyApplied).
//
// A store that is already lost returns an error wrapping journal.ErrLost.
func (s *Store) LedgerMark(ctx context.Context, r LedgerMarkRequest) (LedgerMarkResult, error) {
	if len(r.Session) != idLen || r.Ledger.Seq == 0 {
		return LedgerMarkResult{}, fmt.Errorf("ledger mark: %w: malformed session or ledger position", ErrAttestState)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, lost := s.j.State(); st == journal.StateLost {
		return LedgerMarkResult{}, fmt.Errorf("ledger mark: %w", &journal.LostError{Reason: lost})
	}
	cur := s.attestation.Load()
	if cur == nil {
		return LedgerMarkResult{}, fmt.Errorf("ledger mark: %w", ErrAttestState)
	}
	if !bytes.Equal(cur.Session, r.Session) && !guard.Off(guard.LedgerMarkSession) {
		return LedgerMarkResult{}, fmt.Errorf("ledger mark: %w", ErrStaleAttestation)
	}
	var verdict journal.LostReason
	var detail string
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		pos, ok := tx.CommittedPosition()
		if !ok {
			return errors.New("store position unreadable")
		}
		verdict, detail = checkMark(tx, pos, cur.Mark.Seq, r.Ledger)
		return nil
	}); err != nil {
		return LedgerMarkResult{}, fmt.Errorf("ledger mark: %w", err)
	}
	if verdict != "" {
		s.j.Quarantine(verdict, detail)
		return LedgerMarkResult{Outcome: LedgerMarkQuarantined, Quarantined: verdict}, nil
	}
	if r.Ledger.Seq <= cur.Mark.Seq {
		return LedgerMarkResult{Outcome: LedgerMarkAlreadyApplied, Attestation: cloneAttestation(cur)}, nil
	}
	next := &Attestation{Session: bytes.Clone(cur.Session), Mark: r.Ledger}
	s.attestation.Store(next)
	s.j.SetRetentionFloor(next.Mark.Seq)
	s.parkMu.Lock()
	s.prunePendingLocked(next.Mark.Seq)
	s.wakeParkedLocked()
	s.parkMu.Unlock()
	return LedgerMarkResult{Outcome: LedgerMarkApplied, Attestation: cloneAttestation(next)}, nil
}

// checkMark compares the ledger position l of a ledger mark with the store's
// chain at the committed position pos, the current mark being at mark, in
// tx. It returns the lost reason and a short detail when l is not on the
// chain, or "" when it is.
func checkMark(tx *journal.Tx, pos journal.Position, mark uint64, l journal.Position) (journal.LostReason, string) {
	if l.StoreID != pos.StoreID && !guard.Off(guard.LedgerMarkStoreMatch) {
		return journal.LostLedgerStoreMismatch, "the ledger mark names another store"
	}
	if l.Seq > pos.Seq && !guard.Off(guard.LedgerMarkNotAhead) {
		return journal.LostLedgerAhead, fmt.Sprintf("the ledger mark position %d is beyond the chain (%d)", l.Seq, pos.Seq)
	}
	if guard.Off(guard.LedgerMarkHeadCompare) {
		return "", ""
	}
	head, ok := tx.HeadAt(l.Seq)
	switch {
	case ok && head != l.Head:
		return journal.LostLedgerMismatch, fmt.Sprintf("the chain head at %d differs from the ledger mark", l.Seq)
	case !ok && l.Seq > mark:
		return journal.LostLedgerUnverifiable, fmt.Sprintf("the store holds no head at %d above its mark %d", l.Seq, mark)
	}
	return "", ""
}

// cloneAttestation copies a.
func cloneAttestation(a *Attestation) Attestation {
	return Attestation{Session: bytes.Clone(a.Session), Mark: a.Mark}
}
