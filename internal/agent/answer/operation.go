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

package answer

import (
	"bytes"
	"fmt"
	"regexp"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// idLen is the length of every random identifier in an answer.
const idLen = 16

// digestLen is the length of an intent digest.
const digestLen = 32

// uidRE is the shape of a Provider or VM UID in an answer.
var uidRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)

// Operation is what an answer about one operation is made from.
type Operation struct {
	// Host carries the nonce, the host's identity and trust state; its View
	// is replaced by View.Host.
	Host Host
	// Ref is the operation the request asked about: the op_id and the
	// enrolment it named.
	Ref opstore.OpRef
	// View is the store's answer, read in one transaction.
	View opstore.OpAnswerView
}

// recordStates maps the store's operation states to answer states, number for
// number.
var recordStates = map[opstore.OpState]providerv1.OperationRecordState{
	opstore.StateAccepted:       providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED,
	opstore.StateExecuting:      providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING,
	opstore.StateCompleted:      providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED,
	opstore.StateFailedClean:    providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
	opstore.StateFailedDirty:    providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY,
	opstore.StateOutcomeUnknown: providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN,
	opstore.StateRetired:        providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED,
}

// RecordState is the answer state of what the store holds: the operation's
// state when the store holds it, NO_RECORD when it holds the enrolment but
// no trace of the op_id, and UNKNOWN_TO_AGENT when it does not hold the
// enrolment.
func RecordState(v opstore.OpAnswerView) providerv1.OperationRecordState {
	switch {
	case v.Found:
		if s, ok := recordStates[v.Op.State]; ok {
			return s
		}
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED
	case v.EnrolmentHeld:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD
	}
	return providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT
}

// OperationAnswer builds the OperationAnswer for o. It returns ErrNoAnswer
// when no answer may be made (see HostAnswer), and an error wrapping ErrSign
// when a field breaks the answer's field rules: the agent then sends no
// answer rather than a malformed one.
func OperationAnswer(o Operation) (*providerv1.OperationAnswer, error) {
	h := o.Host
	h.View = o.View.Host
	ctx, err := answerContext(h)
	if err != nil {
		return nil, err
	}
	if len(o.Ref.OpID) != idLen || len(o.Ref.EnrolmentID) != idLen {
		return nil, fmt.Errorf("%w: op_id and op_enrolment_id must be %d bytes", ErrSign, idLen)
	}
	state := RecordState(o.View)
	a := &providerv1.OperationAnswer{
		Context:       ctx,
		OpId:          bytes.Clone(o.Ref.OpID),
		OpEnrolmentId: bytes.Clone(o.Ref.EnrolmentID),
		State:         state,
	}
	if !o.View.Found {
		return a, nil
	}
	op := o.View.Op
	switch {
	case state == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED:
		return nil, fmt.Errorf("%w: operation state %v has no answer state", ErrSign, op.State)
	case !bytes.Equal(op.Ref.OpID, o.Ref.OpID) || !bytes.Equal(op.Ref.EnrolmentID, o.Ref.EnrolmentID):
		return nil, fmt.Errorf("%w: the store answered for another operation", ErrSign)
	case op.Kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED || op.Attempt == 0:
		return nil, fmt.Errorf("%w: kind and attempt must be set", ErrSign)
	case !uidRE.MatchString(op.VMUID):
		return nil, fmt.Errorf("%w: the VM UID is not a UID", ErrSign)
	case len(op.BindingID) != idLen || len(op.IntentDigest) != digestLen:
		return nil, fmt.Errorf("%w: binding id or intent digest malformed", ErrSign)
	case op.Reason != "" && !reasonRE.MatchString(op.Reason):
		return nil, fmt.Errorf("%w: outcome reason %q is malformed", ErrSign, op.Reason)
	case op.Tombstoned && !opstore.Terminal(op.State):
		return nil, fmt.Errorf("%w: a tombstone of a state that is not terminal", ErrSign)
	}
	a.Tombstoned = op.Tombstoned
	a.Kind = int32(op.Kind)
	a.Attempt = op.Attempt
	a.VmUid = op.VMUID
	a.BindingId = bytes.Clone(op.BindingID)
	a.IntentDigest = bytes.Clone(op.IntentDigest)
	a.OutcomeReason = op.Reason
	return a, nil
}

// VM is what an answer about one VM placement is made from.
type VM struct {
	// Host carries the nonce, the host's identity and trust state; its View
	// is replaced by View.Host.
	Host Host
	// VMUID and BindingID name the placement the request asked about.
	VMUID     string
	BindingID []byte
	// View is the store's answer, read in one transaction.
	View opstore.VMAnswerView
}

// presences maps the store's presences to answer presences.
var presences = map[opstore.VMPresence]providerv1.VMPresence{
	opstore.VMCreating:        providerv1.VMPresence_VM_PRESENCE_CREATING,
	opstore.VMPresent:         providerv1.VMPresence_VM_PRESENCE_PRESENT,
	opstore.VMDeletedPresence: providerv1.VMPresence_VM_PRESENCE_DELETED,
	opstore.VMOtherBinding:    providerv1.VMPresence_VM_PRESENCE_OTHER_BINDING,
	opstore.VMNoRecord:        providerv1.VMPresence_VM_PRESENCE_NO_RECORD,
}

// VMAnswer builds the VMAnswer for v. DELETED comes only from the store's VM
// tombstone of the placement, with the Delete's op_id, enrolment and terminal
// sequence number.
func VMAnswer(v VM) (*providerv1.VMAnswer, error) {
	h := v.Host
	h.View = v.View.Host
	ctx, err := answerContext(h)
	if err != nil {
		return nil, err
	}
	if !uidRE.MatchString(v.VMUID) || len(v.BindingID) != idLen {
		return nil, fmt.Errorf("%w: VM UID or binding id malformed", ErrSign)
	}
	p, ok := presences[v.View.Presence]
	if !ok {
		return nil, fmt.Errorf("%w: VM presence %d has no answer presence", ErrSign, v.View.Presence)
	}
	a := &providerv1.VMAnswer{Context: ctx, VmUid: v.VMUID, BindingId: bytes.Clone(v.BindingID), Presence: p}
	if p == providerv1.VMPresence_VM_PRESENCE_DELETED {
		if len(v.View.DeleteOpID) != idLen || len(v.View.DeleteEnrolmentID) != idLen || v.View.DeleteSeq == 0 {
			return nil, fmt.Errorf("%w: a deleted placement without its Delete", ErrSign)
		}
		a.DeleteOpId = bytes.Clone(v.View.DeleteOpID)
		a.DeleteEnrolmentId = bytes.Clone(v.View.DeleteEnrolmentID)
		a.DeleteSeq = v.View.DeleteSeq
	}
	return a, nil
}
