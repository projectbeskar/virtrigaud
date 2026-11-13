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

package opstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestAnswerOp_StatesFromOneRead: an accepted operation is answered from its
// record, a finished one from its tombstone (Tombstoned), an op_id the store
// has no trace of in an enrolment it holds is not found with the enrolment
// held, and one in an enrolment it never held is not found and not held; the
// position is the committed one.
func TestAnswerOp_StatesFromOneRead(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	b := rid()
	create := h.req("vm", b, kCreate, 1)
	h.mustAccept(create)
	v, err := h.s.AnswerOp(ctx, ref(create), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Found || v.Op.State != opstore.StateAccepted || v.Op.Tombstoned || !v.EnrolmentHeld {
		t.Fatalf("accepted: %+v", v)
	}
	if !bytes.Equal(v.Op.IntentDigest, create.IntentDigest) || v.Op.Attempt != 1 || v.Op.VMUID != "vm" {
		t.Fatalf("identity fields: %+v", v.Op)
	}
	if v.Host.Position != h.s.Position() || v.Host.Enrolment == nil {
		t.Fatalf("position %v, want %v", v.Host.Position, h.s.Position())
	}
	h.run(create)
	v, err = h.s.AnswerOp(ctx, ref(create), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Found || v.Op.State != opstore.StateCompleted || !v.Op.Tombstoned {
		t.Fatalf("completed: %+v", v)
	}
	unknown := h.req("vm", b, kPowerOn, 1)
	v, err = h.s.AnswerOp(ctx, ref(unknown), 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Found || !v.EnrolmentHeld {
		t.Fatalf("no record in a held enrolment: %+v", v)
	}
	v, err = h.s.AnswerOp(ctx, opstore.OpRef{EnrolmentID: rid(), OpID: rid()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Found || v.EnrolmentHeld {
		t.Fatalf("an enrolment the store never held: %+v", v)
	}
}

// TestAnswerVM_DeletedOnlyFromTheVMTombstone: a VM's placement is answered
// present after its Create, other-binding for another placement, deleted only
// after its Delete completed (with the Delete's op_id), and no-record for a VM
// the store never held: the absence of a record never says deleted.
func TestAnswerVM_DeletedOnlyFromTheVMTombstone(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	presence := func(vm string, b []byte) opstore.VMAnswerView {
		t.Helper()
		v, err := h.s.AnswerVM(ctx, vm, b, 0)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := presence("never", rid()).Presence; got != opstore.VMNoRecord {
		t.Fatalf("a VM the store never held: %v, want no record", got)
	}
	b := rid()
	create := h.req("vm", b, kCreate, 1)
	h.mustAccept(create)
	if got := presence("vm", b).Presence; got != opstore.VMCreating {
		t.Fatalf("accepted Create: %v", got)
	}
	h.run(create)
	if got := presence("vm", b).Presence; got != opstore.VMPresent {
		t.Fatalf("completed Create: %v", got)
	}
	if got := presence("vm", nil).Presence; got != opstore.VMPresent {
		t.Fatalf("any placement: %v", got)
	}
	if got := presence("vm", rid()).Presence; got != opstore.VMOtherBinding {
		t.Fatalf("another placement: %v", got)
	}
	del := h.req("vm", b, kDelete, 1)
	h.do(del)
	v := presence("vm", b)
	if v.Presence != opstore.VMDeletedPresence || !bytes.Equal(v.DeleteOpID, del.OpID) ||
		!bytes.Equal(v.DeleteEnrolmentID, del.EnrolmentID) || v.DeleteSeq == 0 {
		t.Fatalf("after Delete: %+v", v)
	}
	if got := presence("vm", nil).Presence; got != opstore.VMNoRecord {
		t.Fatalf("any placement after Delete: %v", got)
	}
	if got := presence("vm", rid()).Presence; got != opstore.VMNoRecord {
		t.Fatalf("a placement that never existed, after the VM's Delete: %v, want no record", got)
	}
}

// TestPanicCountIsDurable: the count of an operation's panicked runs
// survives a restart, and ResolveUnknown then records the outcome unknown
// with the given reason and frees nothing (the VM stays held).
func TestPanicCountIsDurable(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	for want := 1; want <= 2; want++ {
		n, err := h.s.RecordPanic(ctx, ref(r))
		if err != nil || n != want {
			t.Fatalf("record: %d %v, want %d", n, err, want)
		}
	}
	_ = h.s.Close()
	s := h.open()
	if err := s.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Panics(ctx, ref(r)); err != nil || n != 2 {
		t.Fatalf("after a restart: %d %v", n, err)
	}
	v, err := s.ResolveUnknown(ctx, ref(r), opstore.ReasonRunPanicked)
	if err != nil || v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonRunPanicked {
		t.Fatalf("resolve: %+v %v", v, err)
	}
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 2)), opstore.RefusalVMBusy)
}

// TestPanicCountIsPruned: the panic count of an operation is removed with
// its terminal write, and the counts of a frozen enrolment's operations with
// its records when the freeze is confirmed.
func TestPanicCountIsPruned(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	count := func(r opstore.Request) int {
		t.Helper()
		n, err := h.s.Panics(ctx, ref(r))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	b := rid()
	create := h.req("vm", b, kCreate, 1)
	h.mustAccept(create)
	if _, err := h.s.RecordPanic(ctx, ref(create)); err != nil {
		t.Fatal(err)
	}
	h.run(create)
	if n := count(create); n != 0 {
		t.Fatalf("after the terminal write: %d", n)
	}
	open := h.req("vm", b, kPowerOn, 1)
	h.mustAccept(open)
	if _, err := h.s.RecordPanic(ctx, ref(open)); err != nil {
		t.Fatal(err)
	}
	old := h.enrol
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, InstallID: rid(), StartEpoch: h.start + 1}
	if err := h.s.Reenrol(ctx, next); err != nil {
		t.Fatal(err)
	}
	// The freeze retired the operation (no step intent): a terminal write.
	if n := count(open); n != 0 {
		t.Fatalf("after the freeze: %d", n)
	}
	held := h.req("vm2", rid(), kCreate, 1)
	held.EnrolmentID = next.ID
	held.MintEpoch.Counter = next.StartEpoch
	h.mustAccept(held)
	if _, err := h.s.RecordPanic(ctx, ref(held)); err != nil {
		t.Fatal(err)
	}
	// A count left for an operation of the frozen enrolment (one whose
	// outcome stayed unknown, for example).
	frozen := opstore.OpRef{EnrolmentID: old, OpID: rid()}
	if _, err := h.s.RecordPanic(ctx, frozen); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ConfirmFreeze(ctx, old); err != nil {
		t.Fatal(err)
	}
	if n, err := h.s.Panics(ctx, frozen); err != nil || n != 0 {
		t.Fatalf("a count of the discarded enrolment survived: %d %v", n, err)
	}
	if n := count(held); n != 1 {
		t.Fatalf("a count of the active enrolment was pruned: %d", n)
	}
}

// TestRecordPanicNeverBlocksOnAHeldLock: RecordPanic gives up, counting
// nothing, when the store lock stays held (a panic may have unwound a method
// that held it).
func TestRecordPanicNeverBlocksOnAHeldLock(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	opstore.HoldLock(h.s)
	defer opstore.ReleaseLock(h.s)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.s.RecordPanic(ctx, ref(r)); !errors.Is(err, opstore.ErrStoreBusy) {
		t.Fatalf("RecordPanic with the lock held: %v, want ErrStoreBusy", err)
	}
}
