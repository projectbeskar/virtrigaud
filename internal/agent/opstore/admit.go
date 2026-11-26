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
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Refusal is why admission refused an operation. A refusal records nothing.
// Only some refusals are definite not-performed answers, which allow the
// manager to start a new attempt (see DefiniteNotPerformed); the classification
// is the provider contract's.
type Refusal int

// The refusals.
const (
	// RefusalNone: not refused.
	RefusalNone Refusal = iota
	// RefusalQuarantined: the store lost its persistence guarantee. Not
	// definite: the store cannot say what ran.
	RefusalQuarantined
	// RefusalUnattested: continuity is not established or recovery has not
	// run. Not definite.
	RefusalUnattested
	// RefusalProviderMismatch: the grant names another Provider than the
	// active enrolment, for an op_id the store has no trace of. Definite.
	RefusalProviderMismatch
	// RefusalGrantMismatch: the grant names a frozen enrolment whose records
	// the store holds, or the op_id was minted before the active enrolment
	// started, for an op_id the store has no trace of. Definite: the store can
	// prove the op_id was never accepted.
	RefusalGrantMismatch
	// RefusalOpIDConflict: the op_id is known with other identity fields, or
	// the attempt does not increase. Not definite.
	RefusalOpIDConflict
	// RefusalVMBusy: the VM has another operation that holds it. Not
	// definite.
	RefusalVMBusy
	// RefusalVMBindingStale: the binding does not match the VM's record, a
	// Create names a placement that exists or whose Delete completed. Not
	// definite.
	RefusalVMBindingStale
	// RefusalVMNotOwned: a non-Create operation for a VM with no record. Not
	// definite.
	RefusalVMNotOwned
	// RefusalUnknownToAgent: the grant names an enrolment the store has no
	// record of: discarded after a confirmed freeze, a predecessor of the
	// enrolment this store was made for, or one of a store that was lost or
	// wiped. The store cannot say whether the op_id ran. Not definite.
	RefusalUnknownToAgent
	// RefusalStoreFull: the store is above its size threshold. Definite (the
	// op_id is not known, as known op_ids are answered first); the agent's
	// server answers RESOURCE_EXHAUSTED with its own reason STORE_FULL
	// (Reason here stays empty: it is not a provider contract reason).
	RefusalStoreFull
	// RefusalLedgerBehind: the ledger gate holds back too much already (the
	// accepted operations above the ledger mark reach their bound, or the
	// request would take the parked request bytes above their budget).
	// Definite (the op_id is not known, as known op_ids are answered first);
	// the agent's server answers RESOURCE_EXHAUSTED with its own reason
	// LEDGER_BEHIND (Reason here stays empty).
	RefusalLedgerBehind
	// RefusalExpectKnownUnknown: the grant says the manager holds a ledgered
	// receipt for the op_id (expect_known), and the store has no record or
	// tombstone of it. Nothing is accepted. Not definite: the op_id may have
	// run in a store this one replaced; the server answers
	// UNKNOWN_TO_AGENT.
	RefusalExpectKnownUnknown
)

// The reason strings of the refusals. They equal the provider contract's
// reason strings; a test in this package checks that. The agent does not
// import the contract package, to keep its dependency set small.
const (
	ReasonQuarantined      = "QUARANTINED"
	ReasonUnattested       = "UNATTESTED"
	ReasonProviderMismatch = "PROVIDER_MISMATCH"
	ReasonGrantMismatch    = "GRANT_MISMATCH"
	ReasonOpIDConflict     = "OP_ID_CONFLICT"
	ReasonVMBusy           = "VM_BUSY"
	ReasonVMBindingStale   = "VM_BINDING_STALE"
	ReasonVMNotOwned       = "VM_NOT_OWNED"
	ReasonUnknownToAgent   = "UNKNOWN_TO_AGENT"
)

// DefiniteNotPerformed reports whether the refusal says the operation was
// definitely not performed and never will be: GRANT_MISMATCH,
// PROVIDER_MISMATCH, store full and ledger behind. Every other refusal leaves the outcome to
// a later answer for the same op_id. It matches the provider contract's
// classification of the reason strings; a test checks that.
func (r Refusal) DefiniteNotPerformed() bool {
	switch r {
	case RefusalGrantMismatch, RefusalProviderMismatch, RefusalStoreFull, RefusalLedgerBehind:
		return true
	}
	return false
}

// Reason returns the refusal's reason string ("" for RefusalNone,
// RefusalStoreFull and RefusalLedgerBehind; UNKNOWN_TO_AGENT for
// RefusalExpectKnownUnknown).
func (r Refusal) Reason() string {
	switch r {
	case RefusalQuarantined:
		return ReasonQuarantined
	case RefusalUnattested:
		return ReasonUnattested
	case RefusalProviderMismatch:
		return ReasonProviderMismatch
	case RefusalGrantMismatch:
		return ReasonGrantMismatch
	case RefusalOpIDConflict:
		return ReasonOpIDConflict
	case RefusalVMBusy:
		return ReasonVMBusy
	case RefusalVMBindingStale:
		return ReasonVMBindingStale
	case RefusalVMNotOwned:
		return ReasonVMNotOwned
	case RefusalUnknownToAgent, RefusalExpectKnownUnknown:
		return ReasonUnknownToAgent
	}
	return ""
}

// String implements fmt.Stringer.
func (r Refusal) String() string {
	switch r {
	case RefusalStoreFull:
		return "STORE_FULL"
	case RefusalLedgerBehind:
		return "LEDGER_BEHIND"
	case RefusalExpectKnownUnknown:
		return AuditExpectKnownUnknown
	}
	if r == RefusalNone {
		return "NONE"
	}
	return r.Reason()
}

// Outcome is the kind of an admission decision.
type Outcome int

const (
	// OutcomeAccepted: a new operation was recorded, durably and anchored.
	OutcomeAccepted Outcome = iota + 1
	// OutcomeKnown: the op_id is known with the same identity fields; the
	// answer is its current state (from its record or its tombstone). It is
	// never executed again.
	OutcomeKnown
	// OutcomeRefused: refused; nothing was recorded.
	OutcomeRefused
)

// AuditExpectKnownUnknown is the audit reason of RefusalExpectKnownUnknown.
const AuditExpectKnownUnknown = "expect_known_unknown"

// Decision is the answer to an admission.
type Decision struct {
	// Outcome is the decision.
	Outcome Outcome
	// Refusal says why, for OutcomeRefused.
	Refusal Refusal
	// Op is the operation's state, for OutcomeAccepted and OutcomeKnown.
	Op OpView
	// Acceptance is where the operation was accepted and the attestation
	// session at the decision, for its receipt: set only by a store with
	// the ledger gate, while it is attested, for an operation whose record
	// is ACCEPTED and whose acceptance head the store holds.
	Acceptance *Acceptance
}

// Acceptance is the chain position at which an operation was accepted, and
// the attestation session the store held when it decided, read under the
// store lock.
type Acceptance struct {
	// Position is the store, the accepted sequence number and the chain
	// head at it.
	Position journal.Position
	// Session is the attestation session.
	Session []byte
}

// Admit decides on one delivery of an operation, under the store lock, by the
// admission table:
//
//  1. store lost ⇒ QUARANTINED; continuity or recovery pending ⇒ UNATTESTED
//     (neither is definite: nothing is said about the op_id);
//  2. a tombstone for (enrolment, op_id), whatever the enrolment's state,
//     with the same identity fields ⇒ its terminal state, never executed
//     again; with any field different ⇒ OP_ID_CONFLICT;
//  3. a record likewise ⇒ its current state, or OP_ID_CONFLICT;
//  4. the grant names an enrolment the store has no record of ⇒
//     UNKNOWN_TO_AGENT (not definite: it may have run elsewhere, for example
//     in a store this one replaced);
//  5. the grant names a frozen enrolment the store holds ⇒ GRANT_MISMATCH;
//     the active enrolment with another Provider ⇒ PROVIDER_MISMATCH
//     (definite: the store holds every record of these enrolments);
//  6. an op_id minted before the enrolment's start epoch ⇒ GRANT_MISMATCH;
//  7. the VM has another operation that holds it ⇒ VM_BUSY;
//  8. not a Create: no VM record ⇒ VM_NOT_OWNED, another binding ⇒
//     VM_BINDING_STALE; a Create: a VM record with another binding or a
//     completed Create of this placement, or a VM tombstone for this
//     placement ⇒ VM_BINDING_STALE;
//  9. an attempt not above the highest accepted for the VM and kind in this
//     enrolment ⇒ OP_ID_CONFLICT;
//  10. the store's live size above its threshold ⇒ store full;
//  11. with the ledger gate: the accepted operations above the ledger mark
//     at their bound, or a request that would take the parked request bytes
//     above their budget ⇒ ledger behind;
//  12. otherwise the operation, its VM record and its attempt mark are
//     written in one transaction, anchored, and only then reported accepted.
//
// An op_id with neither record nor tombstone whose grant sets expect_known
// is refused (RefusalExpectKnownUnknown) before rows 4 to 11.
//
// Decisions 1–11 write nothing. An error means the store could not decide
// (for example it failed while writing, and is then quarantined); the caller
// must treat the operation as not admitted and must not answer "not
// performed" unless a later Lookup says so.
func (s *Store) Admit(ctx context.Context, r Request) (Decision, error) {
	if err := r.validate(); err != nil {
		return Decision{}, err
	}
	if _, err := s.exec.Plan(r.Kind); err != nil {
		return Decision{}, fmt.Errorf("admit: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, buf, err := s.admitLocked(ctx, r)
	s.settle(buf, err)
	if err != nil {
		return Decision{}, err
	}
	if d.Outcome != OutcomeAccepted {
		ev := AuditEvent{Event: "known", Ref: OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}, VMUID: r.VMUID,
			Kind: r.Kind, Actor: r.Actor, State: d.Op.State}
		if d.Outcome == OutcomeRefused {
			ev.Event, ev.Reason, ev.State = "refused", d.Refusal.String(), 0
		}
		s.emitNonStored(ev)
	}
	return d, nil
}

func refused(r Refusal) Decision { return Decision{Outcome: OutcomeRefused, Refusal: r} }

func (s *Store) admitLocked(ctx context.Context, r Request) (Decision, *auditBuf, error) {
	buf := s.newAuditBuf()
	switch st, _ := s.j.State(); {
	case st == journal.StateLost:
		return refused(RefusalQuarantined), buf, nil
	case st != journal.StateReady || !s.ready || s.freezing.Load():
		return refused(RefusalUnattested), buf, nil
	}
	id := r.identity()
	key := opKey(r.EnrolmentID, r.OpID)
	var d Decision
	var accept bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		if d, accept, err = decide(tx, r, id, key); err != nil {
			return err
		}
		if d.Outcome == OutcomeKnown && d.Op.State == StateAccepted && !d.Op.Tombstoned {
			d.Acceptance = s.acceptanceTx(tx, d.Op.AcceptedSeq)
		}
		return nil
	}); err != nil {
		return Decision{}, buf, fmt.Errorf("admit: %w", err)
	}
	if !accept {
		return d, buf, nil
	}
	if s.j.LiveSize() > s.opts.MaxStoreBytes {
		return refused(RefusalStoreFull), buf, nil
	}
	if !s.admitBacklogLocked(r.RequestBytes) {
		return refused(RefusalLedgerBehind), buf, nil
	}
	if err := s.j.CrashPoint(PointBeforeAccept); err != nil {
		return Decision{}, buf, err
	}
	rec := &pb.OpRecord{Identity: id, State: StateAccepted}
	pos, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		rec.AcceptedSeq = tx.Seq()
		if err := put(tx, bucketOps, key, rec); err != nil {
			return err
		}
		vm := &pb.VMRecord{}
		if _, err := get(tx, bucketVMs, []byte(r.VMUID), vm); err != nil {
			return err
		}
		vm.VmUid, vm.BindingId = r.VMUID, r.BindingID
		vm.BusyOpId, vm.BusyEnrolmentId = r.OpID, r.EnrolmentID
		if r.Kind == providerv1.OperationKind_OPERATION_KIND_CREATE && vm.GetLifecycle() != pb.VMLifecycle_VM_LIFECYCLE_PRESENT {
			vm.Lifecycle = pb.VMLifecycle_VM_LIFECYCLE_CREATING
		}
		if err := put(tx, bucketVMs, []byte(r.VMUID), vm); err != nil {
			return err
		}
		if err := put(tx, bucketAttempts, attemptKey(r.EnrolmentID, r.VMUID, int32(r.Kind)), &pb.AttemptMark{Attempt: r.Attempt}); err != nil {
			return err
		}
		return buf.write(tx, AuditEvent{Event: "accepted", Ref: OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID},
			VMUID: r.VMUID, Kind: r.Kind, Actor: r.Actor, State: StateAccepted})
	})
	if err != nil {
		return Decision{}, buf, fmt.Errorf("admit: record operation: %w", err)
	}
	d = Decision{Outcome: OutcomeAccepted, Op: viewOfRecord(rec)}
	if s.opts.LedgerGate {
		s.parkAcceptedLocked(runKey(OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}), rec.GetAcceptedSeq(), r.RequestBytes)
		if a := s.attestation.Load(); a != nil && pos.Seq == rec.GetAcceptedSeq() {
			d.Acceptance = &Acceptance{Position: pos, Session: bytes.Clone(a.Session)}
		}
	}
	return d, buf, nil
}

// acceptanceTx is the Acceptance of an operation accepted at seq, read in
// tx under the store lock: nil without the ledger gate, while the store is
// not attested, or when the store no longer holds the head at seq.
func (s *Store) acceptanceTx(tx *journal.Tx, seq uint64) *Acceptance {
	a := s.attestation.Load()
	if !s.opts.LedgerGate || a == nil || seq == 0 {
		return nil
	}
	pos, ok := tx.CommittedPosition()
	if !ok {
		return nil
	}
	head, ok := tx.HeadAt(seq)
	if !ok {
		return nil
	}
	return &Acceptance{Position: journal.Position{StoreID: pos.StoreID, Seq: seq, Head: head}, Session: bytes.Clone(a.Session)}
}

// decide runs rows 2–9 of the admission table in a read transaction. accept
// is true when the operation is to be recorded.
func decide(tx *journal.Tx, r Request, id *pb.OpIdentity, key []byte) (Decision, bool, error) {
	enr, err := activeEnrolment(tx)
	if err != nil {
		return Decision{}, false, err
	}
	// A retired enrolment is discarded, also while a confirmed discard is
	// still removing its records: none of them is consulted, and a
	// removed tombstone must never let its op_id read as never accepted.
	if retired, err := isRetired(tx, r.EnrolmentID); err != nil {
		return Decision{}, false, err
	} else if retired && !guard.Off(guard.RetiredNotHeld) {
		return refused(RefusalUnknownToAgent), false, nil
	}
	enrolmentCheck := func() (Decision, bool, error) {
		if enr == nil || !sameBytes(enr.GetEnrolmentId(), r.EnrolmentID) {
			// Definite only for an enrolment whose records this store holds
			// (the active one, or a frozen predecessor): then the op_id was
			// provably never accepted. An enrolment the store has no record of
			// (discarded, a predecessor of the enrolment this store was made
			// for, or one of a store that was lost or wiped) may have run it.
			known, err := has(tx, bucketEnrolments, r.EnrolmentID)
			if err != nil {
				return Decision{}, false, err
			}
			if !known && !guard.Off(guard.UnknownEnrolmentNotDefinite) {
				return refused(RefusalUnknownToAgent), true, nil
			}
			return refused(RefusalGrantMismatch), true, nil
		}
		if enr.GetProviderUid() != r.ProviderUID {
			return refused(RefusalProviderMismatch), true, nil
		}
		return Decision{}, false, nil
	}
	if guard.Off(guard.KnownBeforeMismatch) {
		if d, done, err := enrolmentCheck(); err != nil || done {
			return d, false, err
		}
	}

	if !guard.Off(guard.TombstoneLookup) {
		t := &pb.Tombstone{}
		ok, err := get(tx, bucketTombstones, key, t)
		if err != nil {
			return Decision{}, false, err
		}
		if ok {
			if !sameIdentity(t.GetIdentity(), id) {
				return refused(RefusalOpIDConflict), false, nil
			}
			// The op is over. Its record, if not compacted, carries the
			// same state; the tombstone is authoritative.
			return Decision{Outcome: OutcomeKnown, Op: viewOfTombstone(t)}, false, nil
		}
	}
	rec := &pb.OpRecord{}
	ok, err := get(tx, bucketOps, key, rec)
	if err != nil {
		return Decision{}, false, err
	}
	if ok {
		if !sameIdentity(rec.GetIdentity(), id) {
			return refused(RefusalOpIDConflict), false, nil
		}
		return Decision{Outcome: OutcomeKnown, Op: viewOfRecord(rec)}, false, nil
	}
	if r.ExpectKnown && !guard.Off(guard.ExpectKnownRefused) {
		// The manager holds a ledgered receipt for this op_id: it was
		// accepted, here or in a store this one replaced. Never accept it
		// as new, and never refuse it definitely.
		return refused(RefusalExpectKnownUnknown), false, nil
	}
	if d, done, err := enrolmentCheck(); err != nil || done {
		return d, false, err
	}

	if r.MintEpoch.Counter < enr.GetStartEpoch() && !guard.Off(guard.MintEpoch) {
		return refused(RefusalGrantMismatch), false, nil
	}

	vm := &pb.VMRecord{}
	hasVM, err := get(tx, bucketVMs, []byte(r.VMUID), vm)
	if err != nil {
		return Decision{}, false, err
	}
	if hasVM && len(vm.GetBusyOpId()) > 0 && !guard.Off(guard.VMBusy) {
		return refused(RefusalVMBusy), false, nil
	}

	if !guard.Off(guard.Binding) {
		if r.Kind == providerv1.OperationKind_OPERATION_KIND_CREATE {
			if hasVM && (!sameBytes(vm.GetBindingId(), r.BindingID) || vm.GetLifecycle() == pb.VMLifecycle_VM_LIFECYCLE_PRESENT) {
				return refused(RefusalVMBindingStale), false, nil
			}
			stale, err := has(tx, bucketVMTombstones, vmTombstoneKey(r.VMUID, r.BindingID))
			if err != nil {
				return Decision{}, false, err
			}
			if stale {
				return refused(RefusalVMBindingStale), false, nil
			}
		} else {
			if !hasVM {
				return refused(RefusalVMNotOwned), false, nil
			}
			if !sameBytes(vm.GetBindingId(), r.BindingID) {
				return refused(RefusalVMBindingStale), false, nil
			}
		}
	}

	mark := &pb.AttemptMark{}
	if _, err := get(tx, bucketAttempts, attemptKey(r.EnrolmentID, r.VMUID, int32(r.Kind)), mark); err != nil {
		return Decision{}, false, err
	}
	if r.Attempt <= mark.GetAttempt() && !guard.Off(guard.Attempt) {
		return refused(RefusalOpIDConflict), false, nil
	}
	return Decision{}, true, nil
}

// IsRefusal reports whether d is a refusal with reason r.
func (d Decision) IsRefusal(r Refusal) bool { return d.Outcome == OutcomeRefused && d.Refusal == r }
