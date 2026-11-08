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
	// RefusalGrantMismatch: the grant names an enrolment that is not active,
	// or the op_id was minted before the enrolment started, for an op_id the
	// store has no trace of and an enrolment whose records the store still
	// holds. Definite.
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
	// RefusalUnknownToAgent: the grant names an enrolment whose records were
	// discarded after a confirmed freeze; the store can no longer say whether
	// the op_id ran. Not definite.
	RefusalUnknownToAgent
	// RefusalStoreFull: the store is above its size threshold. Definite (the
	// op_id is not known, as known op_ids are answered first); a server
	// answers RESOURCE_EXHAUSTED, with no reason string.
	RefusalStoreFull
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
// PROVIDER_MISMATCH and store full. Every other refusal leaves the outcome to
// a later answer for the same op_id. It matches the provider contract's
// classification of the reason strings; a test checks that.
func (r Refusal) DefiniteNotPerformed() bool {
	switch r {
	case RefusalGrantMismatch, RefusalProviderMismatch, RefusalStoreFull:
		return true
	}
	return false
}

// Reason returns the refusal's reason string ("" for RefusalNone and
// RefusalStoreFull).
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
	case RefusalUnknownToAgent:
		return ReasonUnknownToAgent
	}
	return ""
}

// String implements fmt.Stringer.
func (r Refusal) String() string {
	if r == RefusalStoreFull {
		return "STORE_FULL"
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

// Decision is the answer to an admission.
type Decision struct {
	// Outcome is the decision.
	Outcome Outcome
	// Refusal says why, for OutcomeRefused.
	Refusal Refusal
	// Op is the operation's state, for OutcomeAccepted and OutcomeKnown.
	Op OpView
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
//  4. the grant names an enrolment whose records were discarded ⇒
//     UNKNOWN_TO_AGENT (not definite);
//  5. no active enrolment, or the grant names another one ⇒
//     GRANT_MISMATCH; another Provider ⇒ PROVIDER_MISMATCH (definite: the
//     store holds every record of the enrolments it can still name);
//  6. an op_id minted before the enrolment's start epoch ⇒ GRANT_MISMATCH;
//  7. the VM has another operation that holds it ⇒ VM_BUSY;
//  8. not a Create: no VM record ⇒ VM_NOT_OWNED, another binding ⇒
//     VM_BINDING_STALE; a Create: a VM record with another binding or a
//     completed Create of this placement, or a VM tombstone for this
//     placement ⇒ VM_BINDING_STALE;
//  9. an attempt not above the highest accepted for the VM and kind in this
//     enrolment ⇒ OP_ID_CONFLICT;
//  10. the store's live size above its threshold ⇒ store full;
//  11. otherwise the operation, its VM record and its attempt mark are
//     written in one transaction, anchored, and only then reported accepted.
//
// Decisions 1–10 write nothing. An error means the store could not decide
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
	if err != nil {
		return Decision{}, err
	}
	s.flush(buf)
	if d.Outcome != OutcomeAccepted {
		ev := AuditEvent{Event: "known", Ref: OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}, VMUID: r.VMUID,
			Kind: r.Kind, Actor: r.Actor, State: d.Op.State}
		if d.Outcome == OutcomeRefused {
			ev.Event, ev.Reason, ev.State = "refused", d.Refusal.String(), 0
		}
		s.emit(ev)
	}
	return d, nil
}

func refused(r Refusal) Decision { return Decision{Outcome: OutcomeRefused, Refusal: r} }

func (s *Store) admitLocked(ctx context.Context, r Request) (Decision, *auditBuf, error) {
	buf := &auditBuf{}
	switch st, _ := s.j.State(); {
	case st == journal.StateLost:
		return refused(RefusalQuarantined), buf, nil
	case st != journal.StateReady || !s.ready:
		return refused(RefusalUnattested), buf, nil
	}
	id := r.identity()
	key := opKey(r.EnrolmentID, r.OpID)
	var d Decision
	var accept bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		d, accept, err = decide(tx, r, id, key)
		return err
	}); err != nil {
		return Decision{}, buf, fmt.Errorf("admit: %w", err)
	}
	if !accept {
		return d, buf, nil
	}
	if s.j.LiveSize() > s.opts.MaxStoreBytes {
		return refused(RefusalStoreFull), buf, nil
	}
	if err := s.j.CrashPoint(PointBeforeAccept); err != nil {
		return Decision{}, buf, err
	}
	rec := &pb.OpRecord{Identity: id, State: StateAccepted}
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
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
	return Decision{Outcome: OutcomeAccepted, Op: viewOfRecord(rec)}, buf, nil
}

// decide runs rows 2–9 of the admission table in a read transaction. accept
// is true when the operation is to be recorded.
func decide(tx *journal.Tx, r Request, id *pb.OpIdentity, key []byte) (Decision, bool, error) {
	enr, err := activeEnrolment(tx)
	if err != nil {
		return Decision{}, false, err
	}
	enrolmentCheck := func() (Decision, bool) {
		if enr == nil || !sameBytes(enr.GetEnrolmentId(), r.EnrolmentID) {
			retired, err := isRetired(tx, r.EnrolmentID)
			if err == nil && retired {
				return refused(RefusalUnknownToAgent), true
			}
			return refused(RefusalGrantMismatch), true
		}
		if enr.GetProviderUid() != r.ProviderUID {
			return refused(RefusalProviderMismatch), true
		}
		return Decision{}, false
	}
	if guard.Off(guard.KnownBeforeMismatch) {
		if d, done := enrolmentCheck(); done {
			return d, false, nil
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
	if d, done := enrolmentCheck(); done {
		return d, false, nil
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
			if tx.Get(bucketVMTombstones, vmTombstoneKey(r.VMUID, r.BindingID)) != nil {
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
