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
)

// OpAnswerView is what a signed answer about one operation is made from: what
// the store holds for the operation and the host view, read in one read
// transaction so that the answer reflects exactly the chain position it
// names.
type OpAnswerView struct {
	// Found is set when the store holds a record or a tombstone of the
	// operation.
	Found bool
	// Op is the operation, when Found. Op.Tombstoned is set when its
	// tombstone exists (the operation is over), and its state is then the
	// tombstone's.
	Op OpView
	// EnrolmentHeld is set when the store holds the records of the enrolment
	// the operation was asked under: the active enrolment or a frozen
	// predecessor. When it is not set and Found is not set, the store cannot
	// say anything about the operation (UNKNOWN_TO_AGENT); when it is set and
	// Found is not set, the store has no record of it (NO_RECORD).
	EnrolmentHeld bool
	// Host is the host view of the same read.
	Host HostView
}

// AnswerOp reads what the store holds for ref and the host view, in one read
// transaction. A tombstone takes precedence over the record: the operation is
// over. It is served in every state in which the journal could be opened.
func (s *Store) AnswerOp(ctx context.Context, ref OpRef, continuitySeq uint64) (OpAnswerView, error) {
	var v OpAnswerView
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		if v.Host, err = hostViewTx(tx, continuitySeq); err != nil {
			return err
		}
		key := opKey(ref.EnrolmentID, ref.OpID)
		t := &pb.Tombstone{}
		ok, err := get(tx, bucketTombstones, key, t)
		if err != nil {
			return err
		}
		if ok {
			v.Found, v.Op = true, viewOfTombstone(t)
		} else {
			rec := &pb.OpRecord{}
			ok, err := get(tx, bucketOps, key, rec)
			if err != nil {
				return err
			}
			if ok {
				v.Found, v.Op = true, viewOfRecord(rec)
			}
		}
		v.EnrolmentHeld, err = enrolmentHeld(tx, ref.EnrolmentID)
		return err
	})
	if err != nil {
		return OpAnswerView{}, fmt.Errorf("answer %s: %w", ref, err)
	}
	return v, nil
}

// enrolmentHeld reports whether the store holds the records of enrolment id:
// it has the enrolment's record (active or frozen) and has not discarded it.
func enrolmentHeld(tx *journal.Tx, id []byte) (bool, error) {
	if len(id) != idLen {
		return false, nil
	}
	known, err := has(tx, bucketEnrolments, id)
	if err != nil || !known {
		return false, err
	}
	retired, err := isRetired(tx, id)
	if err != nil {
		return false, err
	}
	return !retired, nil
}

// VMPresence is what the store records about one VM placement.
type VMPresence int

// The VM presences.
const (
	// VMNoRecord: the store has no record of the VM. It never means that the
	// VM was deleted.
	VMNoRecord VMPresence = iota + 1
	// VMCreating: a Create of the placement was accepted and has not
	// completed.
	VMCreating
	// VMPresent: the placement exists (its Create completed, or a later
	// operation holds it).
	VMPresent
	// VMDeletedPresence: a Delete of exactly this placement completed; the
	// VM tombstone written in the Delete's terminal transaction says so.
	VMDeletedPresence
	// VMOtherBinding: the store records the VM under another placement.
	VMOtherBinding
)

// VMAnswerView is what a signed answer about one VM placement is made from,
// read in one read transaction with the host view.
type VMAnswerView struct {
	// Presence is what the store records.
	Presence VMPresence
	// DeleteOpID, DeleteEnrolmentID and DeleteSeq name the completed Delete,
	// for VMDeletedPresence.
	DeleteOpID        []byte
	DeleteEnrolmentID []byte
	DeleteSeq         uint64
	// Host is the host view of the same read.
	Host HostView
}

// AnswerVM reads what the store records about the placement binding of
// vmUID, and the host view, in one read transaction: a VM tombstone for
// exactly (vmUID, binding) is VMDeletedPresence; otherwise a VM record with
// that binding is VMCreating or VMPresent by its lifecycle, a VM record with
// another binding VMOtherBinding, and nothing VMNoRecord. The absence of a VM
// record never yields VMDeletedPresence. A nil binding asks about the VM
// whatever its placement: a VM record is VMCreating or VMPresent, and no
// record VMNoRecord.
func (s *Store) AnswerVM(ctx context.Context, vmUID string, binding []byte, continuitySeq uint64) (VMAnswerView, error) {
	var v VMAnswerView
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		if v.Host, err = hostViewTx(tx, continuitySeq); err != nil {
			return err
		}
		if binding != nil {
			t := &pb.VMTombstone{}
			ok, err := get(tx, bucketVMTombstones, vmTombstoneKey(vmUID, binding), t)
			if err != nil {
				return err
			}
			if ok {
				v.Presence = VMDeletedPresence
				v.DeleteOpID, v.DeleteEnrolmentID, v.DeleteSeq = t.GetDeleteOpId(), t.GetEnrolmentId(), t.GetTerminalSeq()
				return nil
			}
		}
		vm := &pb.VMRecord{}
		ok, err := get(tx, bucketVMs, []byte(vmUID), vm)
		switch {
		case err != nil:
			return err
		case !ok && guard.Off(guard.DeletedOnlyFromVMTombstone) && binding != nil:
			v.Presence = VMDeletedPresence
		case !ok:
			v.Presence = VMNoRecord
		case binding != nil && !sameBytes(vm.GetBindingId(), binding):
			v.Presence = VMOtherBinding
		case vm.GetLifecycle() == pb.VMLifecycle_VM_LIFECYCLE_CREATING:
			v.Presence = VMCreating
		default:
			v.Presence = VMPresent
		}
		return nil
	})
	if err != nil {
		return VMAnswerView{}, fmt.Errorf("answer VM %s: %w", vmUID, err)
	}
	return v, nil
}
