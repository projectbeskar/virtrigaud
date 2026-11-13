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

package server

import (
	"context"
	"regexp"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdktasks "github.com/projectbeskar/virtrigaud/sdk/provider/tasks"
)

// Power states a Describe reports.
const (
	// PowerOn is the power state of a running VM.
	PowerOn = "On"
	// PowerOff is the power state of a VM that is not running.
	PowerOff = "Off"
)

// Limits of the read requests.
const (
	// maxHostIDLen bounds target_host_id.
	maxHostIDLen = 253
	// maxDetailBlobBytes bounds a signed answer carried as a status detail:
	// details travel in a response trailer, which proxies commonly cap.
	maxDetailBlobBytes = 16 << 10
)

// uidRE is the shape of an owner UID.
var uidRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)

// outcomes maps an operation's state to the typed outcome of a TaskStatus
// answer: running only while its record is runnable (a live runner owns it,
// or recovery left it to one), unknown while its outcome is unknown, and the
// terminal outcome otherwise.
var outcomes = map[opstore.OpState]providerv1.TaskOutcome{
	opstore.StateAccepted:       providerv1.TaskOutcome_TASK_OUTCOME_RUNNING,
	opstore.StateExecuting:      providerv1.TaskOutcome_TASK_OUTCOME_RUNNING,
	opstore.StateCompleted:      providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED,
	opstore.StateFailedClean:    providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN,
	opstore.StateFailedDirty:    providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY,
	opstore.StateOutcomeUnknown: providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN,
	opstore.StateRetired:        providerv1.TaskOutcome_TASK_OUTCOME_RETIRED,
}

// taskStatusOf is the unsigned TaskStatus answer of an operation's state.
func taskStatusOf(op opstore.OpView) *providerv1.TaskStatusResponse {
	o, ok := outcomes[op.State]
	if !ok {
		o = providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN
	}
	msg := ""
	if o == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN || o == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY {
		msg = "the operation failed on the host"
		if op.Reason != "" {
			msg += " (" + op.Reason + ")"
		}
	}
	reason := op.Reason
	if o == providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED || o == providerv1.TaskOutcome_TASK_OUTCOME_RUNNING {
		reason = ""
	}
	return sdktasks.Status(o, reason, msg)
}

// unknownToAgent is the NotFound answer for an operation the host holds no
// record of, with the signed answer as a status detail when there is one.
func unknownToAgent(blob *providerv1.SignedBlob) error {
	st := status.New(codes.NotFound, "this host has no record of the operation")
	details := []proto.Message{&errdetails.ErrorInfo{Reason: opstore.ReasonUnknownToAgent, Domain: ErrorInfoDomain}}
	if blob != nil && proto.Size(blob) <= maxDetailBlobBytes {
		details = append(details, blob)
	}
	var anys []*anypb.Any
	for _, d := range details {
		a, err := anypb.New(d)
		if err != nil {
			return st.Err()
		}
		anys = append(anys, a)
	}
	p := st.Proto()
	p.Details = anys
	return status.FromProto(p).Err()
}

// checkTaskStatus applies the request rules of TaskStatus: a signed request
// names the operation (op_id, op_enrolment_id, this host) and no task; a
// plain one names a task and nothing else.
func checkTaskStatus(req *providerv1.TaskStatusRequest, hostID string) error {
	if err := checkNonce(req.GetAnswerNonce(), false); err != nil {
		return err
	}
	if len(req.GetAnswerNonce()) == 0 {
		if len(req.GetOpId()) != 0 || len(req.GetOpEnrolmentId()) != 0 || req.GetTargetHostId() != "" || req.GetContinuitySeq() != 0 {
			return status.Error(codes.InvalidArgument, "op_id, op_enrolment_id, target_host_id and continuity_seq are set only with answer_nonce")
		}
		if req.GetTask().GetId() == "" {
			return status.Error(codes.InvalidArgument, "no task")
		}
		return nil
	}
	switch {
	case req.GetTask() != nil:
		return status.Error(codes.InvalidArgument, "a request with answer_nonce names the operation, not a task")
	case len(req.GetOpId()) != idLen || len(req.GetOpEnrolmentId()) != idLen:
		return status.Errorf(codes.InvalidArgument, "op_id and op_enrolment_id must be %d bytes", idLen)
	case req.GetTargetHostId() == "" || len(req.GetTargetHostId()) > maxHostIDLen:
		return status.Error(codes.InvalidArgument, "target_host_id is required with answer_nonce")
	case req.GetTargetHostId() != hostID:
		return status.Error(codes.InvalidArgument, "target_host_id names another host")
	}
	return nil
}

// TaskStatus answers for one operation from the operation store. A plain
// request names the task reference the operation's call returned; a request
// with answer_nonce names the operation by op_id and enrolment and is answered
// with a signed OperationAnswer made from the same store read as the
// unsigned fields. An operation the store holds no record of is NotFound
// with UNKNOWN_TO_AGENT, never done and never "not performed".
func (p *providerService) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	s := p.s
	if s.opts.Operations == nil {
		return nil, status.Error(codes.Unimplemented, "this agent executes no operations")
	}
	if err := checkTaskStatus(req, s.opts.Credentials.Load().Identity.HostID()); err != nil {
		return nil, err
	}
	nonce := req.GetAnswerNonce()
	var ref opstore.OpRef
	if len(nonce) == 0 {
		var err error
		if ref, err = ParseTaskRef(req.GetTask().GetId()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		ref = opstore.OpRef{EnrolmentID: req.GetOpEnrolmentId(), OpID: req.GetOpId()}
	}
	// The unsigned fields come from the same store read as the signed
	// answer; the store is read here only when no answer was built.
	var view opstore.OpAnswerView
	var readErr error
	read, didRead := func() error {
		view, readErr = s.opts.Operations.AnswerOp(ctx, ref, req.GetContinuitySeq())
		return readErr
	}, false
	blob := s.signAnswer(ctx, nonce, func(h answer.Host) (proto.Message, error) {
		didRead = true
		if err := read(); err != nil {
			return nil, err
		}
		return answer.OperationAnswer(answer.Operation{Host: h, Ref: ref, View: view})
	})
	if !didRead {
		_ = read()
	}
	if readErr != nil {
		s.logNoAnswer(readErr, "the operation store cannot be read")
		return nil, status.Error(codes.Unavailable, "the host's operation store cannot be read")
	}
	if !view.Found {
		return nil, unknownToAgent(blob)
	}
	resp := taskStatusOf(view.Op)
	resp.HostAnswer = blob
	return resp, nil
}

// checkDescribe applies the request rules of Describe: an owner UID, this
// host when a host is named, and binding_id exactly with answer_nonce.
func checkDescribe(req *providerv1.DescribeRequest, hostID string) error {
	if err := checkNonce(req.GetAnswerNonce(), false); err != nil {
		return err
	}
	signed := len(req.GetAnswerNonce()) != 0
	switch {
	case !uidRE.MatchString(req.GetOwner().GetUid()):
		return status.Error(codes.InvalidArgument, "owner.uid is required and must be a UID")
	case req.GetTargetHostId() != "" && req.GetTargetHostId() != hostID:
		return status.Error(codes.InvalidArgument, "target_host_id names another host")
	case signed && len(req.GetBindingId()) != idLen:
		return status.Errorf(codes.InvalidArgument, "binding_id must be %d bytes with answer_nonce", idLen)
	case !signed && (len(req.GetBindingId()) != 0 || req.GetContinuitySeq() != 0):
		return status.Error(codes.InvalidArgument, "binding_id and continuity_seq are set only with answer_nonce")
	}
	return nil
}

// Describe answers for the owner's VM from the operation store: the VM
// exists for its owner while the store records a placement of it. A request
// with answer_nonce asks about one placement (binding_id) and is answered
// with a signed VMAnswer from the same store read: DELETED only from the VM
// tombstone of a completed Delete of that placement, never from the absence
// of a record.
func (p *providerService) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	s := p.s
	if s.opts.Operations == nil {
		return nil, status.Error(codes.Unimplemented, "this agent executes no operations")
	}
	if err := checkDescribe(req, s.opts.Credentials.Load().Identity.HostID()); err != nil {
		return nil, err
	}
	uid := req.GetOwner().GetUid()
	var binding []byte
	if len(req.GetAnswerNonce()) != 0 {
		binding = req.GetBindingId()
	}
	var view opstore.VMAnswerView
	var readErr error
	read, didRead := func() error {
		view, readErr = s.opts.Operations.AnswerVM(ctx, uid, binding, req.GetContinuitySeq())
		return readErr
	}, false
	blob := s.signAnswer(ctx, req.GetAnswerNonce(), func(h answer.Host) (proto.Message, error) {
		didRead = true
		if err := read(); err != nil {
			return nil, err
		}
		return answer.VMAnswer(answer.VM{Host: h, VMUID: uid, BindingID: binding, View: view})
	})
	if !didRead {
		_ = read()
	}
	if readErr != nil {
		s.logNoAnswer(readErr, "the operation store cannot be read")
		return nil, status.Error(codes.Unavailable, "the host's operation store cannot be read")
	}
	resp := &providerv1.DescribeResponse{HostAnswer: blob}
	switch view.Presence {
	case opstore.VMCreating, opstore.VMPresent, opstore.VMOtherBinding:
		resp.Exists = true
	default:
		return resp, nil
	}
	if st, ok, err := s.opts.Operations.Describe(ctx, uid); err == nil && ok && st.Exists {
		resp.PowerState = PowerOff
		if st.Running {
			resp.PowerState = PowerOn
		}
	}
	return resp, nil
}
