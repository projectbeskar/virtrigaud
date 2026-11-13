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
	"testing"

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
