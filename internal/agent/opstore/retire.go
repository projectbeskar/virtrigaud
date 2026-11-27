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
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// ReasonNeverReceived is the reason of the RETIRED tombstone a
// retire-if-unknown commits: the store never accepted the operation, and
// never will.
const ReasonNeverReceived = "never_received"

// auditRetired is the audit event of a retire-if-unknown that committed its
// tombstone.
const auditRetired = "retired"

// ErrEnrolmentNotActive is returned by RetireIfUnknown for an operation of
// an enrolment that is not the store's active one. Nothing is changed.
var ErrEnrolmentNotActive = errors.New("the operation's enrolment is not the store's active enrolment")

// ErrNoTraceScan is returned by RetireIfUnknown when the executor cannot
// scan its objects for a trace of the operation (it implements no
// executor.TraceScanner, or the scan failed). Nothing is changed: the store
// never retires what it cannot scan for.
var ErrNoTraceScan = errors.New("the executor's objects cannot be scanned for a trace of the operation")

// RetireOutcome is what a retire-if-unknown did.
type RetireOutcome int

const (
	// RetireApplied: the store had no trace of the operation; it committed
	// a RETIRED tombstone (ReasonNeverReceived) and raised the attempt mark
	// of the operation's VM and kind. A later delivery of the op_id meets
	// the tombstone and is never run.
	RetireApplied RetireOutcome = iota + 1
	// RetireAlreadyApplied: an earlier retire-if-unknown of the same
	// operation committed its tombstone; nothing changed.
	RetireAlreadyApplied
	// RetireNotApplicable: the store holds a record or a tombstone of the
	// operation with the same identity (RetireResult.State is its state);
	// nothing changed.
	RetireNotApplicable
	// RetireOpKnown: the store holds a record or a tombstone of the op_id
	// with other identity fields; nothing changed.
	RetireOpKnown
	// RetireTraceFound: the executor's objects carry a trace of the
	// operation that no record explains; the store is quarantined
	// (unknown_op_trace).
	RetireTraceFound
	// RetireQuarantined: a witness file of the operation, or a VM record
	// naming it busy, exists without its record; the store is quarantined
	// (inconsistent_store).
	RetireQuarantined
)

// RetireRequest is what one verified retire-if-unknown grant asks of the
// store: the identity of the operation, and the attestation session the
// manager saw the store hold no record of it in.
type RetireRequest struct {
	// Session is the attestation session the grant names.
	Session []byte
	// Op is the operation's identity, as its operation grants named it
	// (Actor, ExpectKnown and RequestBytes are not used).
	Op Request
}

// RetireResult is the outcome of a retire-if-unknown.
type RetireResult struct {
	// Outcome is what the store did.
	Outcome RetireOutcome
	// State is the operation's state after the call: StateRetired for
	// RetireApplied and RetireAlreadyApplied, its record's or tombstone's
	// state for RetireNotApplicable, unset otherwise.
	State OpState
	// Quarantined is the lost reason recorded for RetireTraceFound and
	// RetireQuarantined.
	Quarantined journal.LostReason
}

// RetireIfUnknown retires an operation the store has no trace of, so that
// a delivery of it can never run, in this order:
//
//  1. a store that awaits its first attestation returns ErrAttestState, a
//     grant for another session ErrStaleAttestation, a store that is not
//     ready (recovery pending, a freeze unfinished) ErrAttestState, an
//     enrolment other than the active one ErrEnrolmentNotActive: nothing is
//     changed and nothing is quarantined;
//  2. without the store lock, an operation the store already holds a
//     record or tombstone of is not scanned for (one read; step 3 decides);
//     otherwise the executor's objects are scanned for a trace of the
//     operation (executor.TraceScanner; ErrNoTraceScan when it
//     cannot be scanned). A trace is explained when the store holds a
//     record or tombstone of the op_id, or (for a placement trace of a
//     Create) a VM record of the placement: every such object is made only
//     after its record is committed, and records are never removed while
//     their tombstone is missing, so a read after the scan sees it. A trace
//     nothing explains quarantines the store (unknown_op_trace);
//  3. under the store lock, which serialises every admission, in one
//     transaction: the session and readiness again; a tombstone or record
//     of the op_id with the same identity gives RetireNotApplicable (or
//     RetireAlreadyApplied for this call's own earlier tombstone), with
//     other identity fields RetireOpKnown; a witness file of the
//     operation, or a VM record naming it busy, quarantines the store
//     (inconsistent_store); otherwise the RETIRED tombstone and the raised
//     attempt mark are committed and anchored, and RetireApplied returned.
//
// A delivery racing the call is decided by the lock: either it was admitted
// first (its record exists, and the retirement is not applicable) or it
// meets the tombstone (and is answered RETIRED, never run). A store that is
// already lost returns an error wrapping journal.ErrLost.
func (s *Store) RetireIfUnknown(ctx context.Context, r RetireRequest) (RetireResult, error) {
	if err := r.Op.validate(); err != nil {
		return RetireResult{}, fmt.Errorf("retire: %w", err)
	}
	if len(r.Session) != idLen {
		return RetireResult{}, fmt.Errorf("retire: %w: malformed session", ErrAttestState)
	}
	if err := s.retireAllowed(r); err != nil {
		return RetireResult{}, fmt.Errorf("retire: %w", err)
	}
	if res, done, err := s.retireTraceScan(ctx, r.Op); err != nil || done {
		return res, err
	}
	s.retirePoint(retirePointLocking)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.retireAllowed(r); err != nil && !guard.Off(guard.RetireRecheckLocked) {
		return RetireResult{}, fmt.Errorf("retire: %w", err)
	}
	return s.retireLocked(ctx, r)
}

// retireAllowed applies step 1 of RetireIfUnknown (the store's state and
// the session); the enrolment is checked in the transaction.
func (s *Store) retireAllowed(r RetireRequest) error {
	if st, lost := s.j.State(); st == journal.StateLost {
		return &journal.LostError{Reason: lost}
	}
	cur := s.attestation.Load()
	if cur == nil {
		return ErrAttestState
	}
	if !bytes.Equal(cur.Session, r.Session) && !guard.Off(guard.RetireSession) {
		return ErrStaleAttestation
	}
	if st, _ := s.j.State(); st != journal.StateReady || !s.recovered.Load() || s.freezing.Load() {
		return fmt.Errorf("%w: the store is not ready", ErrAttestState)
	}
	return nil
}

// The points of RetireIfUnknown a test hook can interleave a delivery at.
const (
	// retirePointScanned: the trace scan returned; the store was not read
	// after it yet.
	retirePointScanned = "scanned"
	// retirePointLocking: the scan is over; the store lock is not taken
	// yet.
	retirePointLocking = "locking"
)

// retirePoint calls the test hook at point, if one is set.
func (s *Store) retirePoint(point string) {
	if s.retireHook != nil {
		s.retireHook(point)
	}
}

// executorOp is the executor's view of the operation of r, without request
// bytes.
func executorOp(r Request) executor.Operation {
	return executor.Operation{OpID: bytes.Clone(r.OpID), VMUID: r.VMUID, BindingID: bytes.Clone(r.BindingID), Kind: r.Kind,
		Attempt: r.Attempt}
}

// retireTraceScan is step 2 of RetireIfUnknown, without the store lock. done
// is true when the call ends here (a trace no record explains quarantined
// the store).
func (s *Store) retireTraceScan(ctx context.Context, r Request) (RetireResult, bool, error) {
	if guard.Off(guard.RetireTraceScan) {
		return RetireResult{}, false, nil
	}
	// A cheap read first: an operation the store already holds a record or
	// tombstone of (known, or retired before) is not scanned for; the locked
	// step answers from its record. A replayed retirement grant then costs
	// one read, never a scan.
	known, err := s.retireKnownCheap(ctx, r)
	if err != nil {
		return RetireResult{}, false, fmt.Errorf("retire: %w", err)
	}
	if known {
		return RetireResult{}, false, nil
	}
	scanner, ok := s.exec.(executor.TraceScanner)
	if !ok {
		return RetireResult{}, false, fmt.Errorf("retire: %w", ErrNoTraceScan)
	}
	tr, err := scanner.Traces(ctx, executorOp(r))
	if err != nil {
		return RetireResult{}, false, fmt.Errorf("retire: %w: %w", ErrNoTraceScan, err)
	}
	s.retirePoint(retirePointScanned)
	if !tr.Found() {
		return RetireResult{}, false, nil
	}
	explained := false
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		key := opKey(r.EnrolmentID, r.OpID)
		for _, b := range []string{bucketOps, bucketTombstones} {
			ok, err := has(tx, b, key)
			if err != nil {
				return err
			}
			explained = explained || ok
		}
		if !explained && !tr.Op && tr.Placement {
			vm := &pb.VMRecord{}
			ok, err := get(tx, bucketVMs, []byte(r.VMUID), vm)
			if err != nil {
				return err
			}
			explained = ok && sameBytes(vm.GetBindingId(), r.BindingID)
		}
		return nil
	}); err != nil {
		return RetireResult{}, false, fmt.Errorf("retire: read the store after the trace scan: %w", err)
	}
	if explained {
		// The operation is known; the locked step answers from its record.
		return RetireResult{}, false, nil
	}
	s.j.Quarantine(journal.LostUnknownOpTrace, "a retire-if-unknown found a trace of an operation the store has no record of")
	s.emitNonStored(AuditEvent{Event: "quarantined", Ref: OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}, VMUID: r.VMUID,
		Kind: r.Kind, Reason: string(journal.LostUnknownOpTrace)})
	return RetireResult{Outcome: RetireTraceFound, Quarantined: journal.LostUnknownOpTrace}, true, nil
}

// retireKnownCheap reports, in one read without the store lock, whether the
// store holds a record or tombstone of r's op_id.
func (s *Store) retireKnownCheap(ctx context.Context, r Request) (bool, error) {
	known := false
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		key := opKey(r.EnrolmentID, r.OpID)
		for _, b := range []string{bucketOps, bucketTombstones} {
			ok, err := has(tx, b, key)
			if err != nil {
				return err
			}
			known = known || ok
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read the operation before the trace scan: %w", err)
	}
	return known, nil
}

// retireLocked is step 3 of RetireIfUnknown. s.mu is held.
func (s *Store) retireLocked(ctx context.Context, r RetireRequest) (RetireResult, error) {
	op := r.Op
	key := opKey(op.EnrolmentID, op.OpID)
	var res RetireResult
	var known bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		enr, err := activeEnrolment(tx)
		if err != nil {
			return err
		}
		if (enr == nil || !sameBytes(enr.GetEnrolmentId(), op.EnrolmentID) || enr.GetProviderUid() != r.Op.ProviderUID) &&
			!guard.Off(guard.RetireEnrolmentActive) {
			return ErrEnrolmentNotActive
		}
		if res, known, err = retireKnown(tx, key, op.identity()); err != nil || known {
			return err
		}
		lost, err := s.retireEvidence(tx, op)
		if err != nil {
			return err
		}
		if lost != "" {
			res = RetireResult{Outcome: RetireQuarantined, Quarantined: journal.LostInconsistentStore}
			known = true
		}
		return nil
	}); err != nil {
		return RetireResult{}, fmt.Errorf("retire: %w", err)
	}
	if res.Outcome == RetireQuarantined {
		s.j.Quarantine(journal.LostInconsistentStore, "a retire-if-unknown found a witness or a busy VM of an operation the store has no record of")
		s.emitNonStored(AuditEvent{Event: "quarantined", Ref: OpRef{EnrolmentID: op.EnrolmentID, OpID: op.OpID}, VMUID: op.VMUID,
			Kind: op.Kind, Reason: string(journal.LostInconsistentStore)})
		return res, nil
	}
	if known {
		return res, nil
	}
	buf := s.newAuditBuf()
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		id := op.identity()
		if err := put(tx, bucketTombstones, key, &pb.Tombstone{Identity: id, State: StateRetired, Reason: ReasonNeverReceived}); err != nil {
			return err
		}
		if !guard.Off(guard.RetireAttemptMark) {
			mk := attemptKey(op.EnrolmentID, op.VMUID, int32(op.Kind))
			mark := &pb.AttemptMark{}
			if _, err := get(tx, bucketAttempts, mk, mark); err != nil {
				return err
			}
			if op.Attempt > mark.GetAttempt() {
				if err := put(tx, bucketAttempts, mk, &pb.AttemptMark{Attempt: op.Attempt}); err != nil {
					return err
				}
			}
		}
		return buf.write(tx, AuditEvent{Event: auditRetired, Ref: OpRef{EnrolmentID: op.EnrolmentID, OpID: op.OpID},
			VMUID: op.VMUID, Kind: op.Kind, Reason: ReasonNeverReceived, State: StateRetired})
	})
	s.settle(buf, err)
	if err != nil {
		return RetireResult{}, fmt.Errorf("retire: commit the tombstone: %w", err)
	}
	return RetireResult{Outcome: RetireApplied, State: StateRetired}, nil
}

// retireKnown answers a retire-if-unknown from the tombstone or record of
// key, when there is one (known true).
func retireKnown(tx *journal.Tx, key []byte, id *pb.OpIdentity) (RetireResult, bool, error) {
	if guard.Off(guard.RetireRecordLookup) {
		return RetireResult{}, false, nil
	}
	t := &pb.Tombstone{}
	ok, err := get(tx, bucketTombstones, key, t)
	if err != nil {
		return RetireResult{}, false, err
	}
	if ok {
		switch {
		case !sameIdentity(t.GetIdentity(), id):
			return RetireResult{Outcome: RetireOpKnown}, true, nil
		case t.GetState() == StateRetired && t.GetReason() == ReasonNeverReceived:
			return RetireResult{Outcome: RetireAlreadyApplied, State: StateRetired}, true, nil
		}
		return RetireResult{Outcome: RetireNotApplicable, State: t.GetState()}, true, nil
	}
	rec := &pb.OpRecord{}
	ok, err = get(tx, bucketOps, key, rec)
	if err != nil {
		return RetireResult{}, false, err
	}
	if !ok {
		return RetireResult{}, false, nil
	}
	if !sameIdentity(rec.GetIdentity(), id) {
		return RetireResult{Outcome: RetireOpKnown}, true, nil
	}
	return RetireResult{Outcome: RetireNotApplicable, State: rec.GetState()}, true, nil
}

// retireEvidence reports, for an operation the store has no record of,
// evidence that it lost one: a witness file of the operation, or a VM record
// naming it busy. It returns the lost reason, or "" when there is none.
func (s *Store) retireEvidence(tx *journal.Tx, op Request) (journal.LostReason, error) {
	if guard.Off(guard.RetireWitnessCheck) {
		return "", nil
	}
	vm := &pb.VMRecord{}
	ok, err := get(tx, bucketVMs, []byte(op.VMUID), vm)
	if err != nil {
		return "", err
	}
	if ok && sameBytes(vm.GetBusyOpId(), op.OpID) && sameBytes(vm.GetBusyEnrolmentId(), op.EnrolmentID) {
		return journal.LostInconsistentStore, nil
	}
	names, err := s.fsys.ReadDir(s.witnessDir(op.EnrolmentID))
	if err != nil {
		return "", fmt.Errorf("list witnesses: %w", err)
	}
	prefix := fmt.Sprintf("%x.", op.OpID)
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return journal.LostInconsistentStore, nil
		}
	}
	return "", nil
}
