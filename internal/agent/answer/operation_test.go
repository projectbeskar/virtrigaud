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

package answer_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func opView(state opstore.OpState, tomb bool) opstore.OpAnswerView {
	ref := opstore.OpRef{EnrolmentID: bytes.Repeat([]byte{0xE}, 16), OpID: bytes.Repeat([]byte{0x0A}, 16)}
	return opstore.OpAnswerView{Found: true, EnrolmentHeld: true, Op: opstore.OpView{
		Ref: ref, VMUID: "vm-1", BindingID: bytes.Repeat([]byte{0xB}, 16), Kind: providerv1.OperationKind_OPERATION_KIND_POWER_ON,
		Attempt: 3, State: state, Tombstoned: tomb, IntentDigest: bytes.Repeat([]byte{0xD}, 32), Reason: "step_deadline",
	}}
}

// TestOperationAnswer_FieldRules: a held operation carries every identity
// field and its state number for number; NO_RECORD and UNKNOWN_TO_AGENT carry
// none; malformed fields and a tombstone of a non-terminal state give no
// answer.
func TestOperationAnswer_FieldRules(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	v := opView(opstore.StateCompleted, true)
	v.Host = h.View
	a, err := answer.OperationAnswer(answer.Operation{Host: h, Ref: v.Op.Ref, View: v})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED || !a.GetTombstoned() ||
		a.GetAttempt() != 3 || a.GetVmUid() != "vm-1" || len(a.GetIntentDigest()) != 32 || a.GetOutcomeReason() != "step_deadline" ||
		!bytes.Equal(a.GetContext().GetNonce(), h.Nonce) || a.GetKind() != int32(providerv1.OperationKind_OPERATION_KIND_POWER_ON) {
		t.Fatalf("answer: %v", a)
	}
	for s := opstore.StateAccepted; s <= opstore.StateRetired; s++ {
		if int32(answer.RecordState(opView(s, false))) != int32(s) {
			t.Fatalf("state %v is not copied number for number", s)
		}
	}
	for name, view := range map[string]opstore.OpAnswerView{
		"no record":        {EnrolmentHeld: true, Host: h.View},
		"unknown to agent": {Host: h.View},
	} {
		a, err := answer.OperationAnswer(answer.Operation{Host: h, Ref: v.Op.Ref, View: view})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.GetKind() != 0 || a.GetAttempt() != 0 || a.GetVmUid() != "" || a.GetTombstoned() || a.GetOutcomeReason() != "" {
			t.Fatalf("%s carries identity fields: %v", name, a)
		}
	}
	for name, edit := range map[string]func(*opstore.OpAnswerView){
		"tombstone of a running op": func(v *opstore.OpAnswerView) { v.Op.State, v.Op.Tombstoned = opstore.StateExecuting, true },
		"bad reason":                func(v *opstore.OpAnswerView) { v.Op.Reason = "Not A Reason" },
		"bad uid":                   func(v *opstore.OpAnswerView) { v.Op.VMUID = "VM/1" },
		"another op":                func(v *opstore.OpAnswerView) { v.Op.Ref.OpID = bytes.Repeat([]byte{1}, 16) },
		"short digest":              func(v *opstore.OpAnswerView) { v.Op.IntentDigest = v.Op.IntentDigest[:5] },
	} {
		bad := opView(opstore.StateCompleted, true)
		bad.Host = h.View
		edit(&bad)
		if _, err := answer.OperationAnswer(answer.Operation{Host: h, Ref: v.Op.Ref, View: bad}); !errors.Is(err, answer.ErrSign) {
			t.Fatalf("%s: err %v, want ErrSign", name, err)
		}
	}
	h.Nonce = nil
	if _, err := answer.OperationAnswer(answer.Operation{Host: h, Ref: v.Op.Ref, View: v}); !errors.Is(err, answer.ErrNoAnswer) {
		t.Fatalf("no nonce: %v", err)
	}
}

// TestVMAnswer_DeletedNamesItsDelete: DELETED carries the Delete's op_id,
// enrolment and sequence number, and no other presence does.
func TestVMAnswer_DeletedNamesItsDelete(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	b := bytes.Repeat([]byte{0xB}, 16)
	del := opstore.VMAnswerView{Presence: opstore.VMDeletedPresence, DeleteOpID: bytes.Repeat([]byte{1}, 16),
		DeleteEnrolmentID: bytes.Repeat([]byte{2}, 16), DeleteSeq: 9, Host: h.View}
	a, err := answer.VMAnswer(answer.VM{Host: h, VMUID: "vm-1", BindingID: b, View: del})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_DELETED || a.GetDeleteSeq() != 9 || len(a.GetDeleteOpId()) != 16 {
		t.Fatalf("deleted: %v", a)
	}
	a, err = answer.VMAnswer(answer.VM{Host: h, VMUID: "vm-1", BindingID: b, View: opstore.VMAnswerView{Presence: opstore.VMNoRecord, Host: h.View}})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetPresence() != providerv1.VMPresence_VM_PRESENCE_NO_RECORD || a.GetDeleteOpId() != nil || a.GetDeleteSeq() != 0 {
		t.Fatalf("no record: %v", a)
	}
	del.DeleteOpID = nil
	if _, err := answer.VMAnswer(answer.VM{Host: h, VMUID: "vm-1", BindingID: b, View: del}); !errors.Is(err, answer.ErrSign) {
		t.Fatalf("deleted without its Delete: %v", err)
	}
}
