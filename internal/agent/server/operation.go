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
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Operations is what the server's operation path needs from the agent: the
// operation store's admission and single-read answers, and a runner for each
// accepted operation. A server without Operations refuses every mutating
// method once ready (Unimplemented), as a release build does.
type Operations interface {
	// Admit decides on one delivery (opstore.Store.Admit).
	Admit(ctx context.Context, r opstore.Request) (opstore.Decision, error)
	// AnswerOp reads an operation and the host view in one read
	// (opstore.Store.AnswerOp).
	AnswerOp(ctx context.Context, ref opstore.OpRef, continuitySeq uint64) (opstore.OpAnswerView, error)
	// AnswerVM reads a VM placement and the host view in one read
	// (opstore.Store.AnswerVM).
	AnswerVM(ctx context.Context, vmUID string, binding []byte, continuitySeq uint64) (opstore.VMAnswerView, error)
	// Supports reports whether the executor implements kind.
	Supports(kind providerv1.OperationKind) bool
	// Start starts a runner for an admitted operation; payload is the
	// verified request, held in memory only.
	Start(ref opstore.OpRef, payload []byte)
	// Describe reads a VM's state on the host; ok is false when the executor
	// cannot read it.
	Describe(ctx context.Context, vmUID string) (state executor.VMState, ok bool, err error)
}

// TaskRefPrefix starts the task reference of an operation the agent accepted:
// "op-", then the enrolment id and the op_id in lower-case hexadecimal.
const TaskRefPrefix = "op-"

// taskRefHexLen is the length of a task reference after its prefix.
const taskRefHexLen = 4 * idLen

// idLen is the length of every random identifier on the operation path.
const idLen = 16

// TaskRef is the task reference of an operation: the agent answers TaskStatus
// for it from the operation's record or tombstone.
func TaskRef(ref opstore.OpRef) string {
	return TaskRefPrefix + hex.EncodeToString(ref.EnrolmentID) + hex.EncodeToString(ref.OpID)
}

// ParseTaskRef is the inverse of TaskRef.
func ParseTaskRef(s string) (opstore.OpRef, error) {
	rest, ok := strings.CutPrefix(s, TaskRefPrefix)
	if !ok || len(rest) != taskRefHexLen || strings.ToLower(rest) != rest {
		return opstore.OpRef{}, errors.New("not a task reference this agent issued")
	}
	b, err := hex.DecodeString(rest)
	if err != nil {
		return opstore.OpRef{}, errors.New("not a task reference this agent issued")
	}
	return opstore.OpRef{EnrolmentID: b[:idLen], OpID: b[idLen:]}, nil
}

// verifierCache holds the grant verifier of the current credentials.
type verifierCache struct {
	mu    sync.Mutex
	creds *Credentials
	v     *grantverify.Verifier
}

// verifier returns the grant verifier for the current credentials: the
// pinned grant anchors, the installation, Provider, host and enrolment the
// host certificate names, and a clock that is never behind the agent's clock
// floor.
func (s *Server) verifier() (*grantverify.Verifier, error) {
	c := s.opts.Credentials.Load()
	s.vc.mu.Lock()
	defer s.vc.mu.Unlock()
	if s.vc.creds == c && s.vc.v != nil {
		return s.vc.v, nil
	}
	v, err := grantverify.NewVerifier(grantverify.VerifierConfig{
		Anchors:     c.Trust.GrantAnchors,
		InstallID:   c.Identity.Record.InstallID,
		ProviderUID: c.Identity.Record.ProviderUID,
		HostID:      c.Identity.HostID(),
		EnrolmentID: c.Identity.Record.EnrolmentID,
		Clock:       grantverify.ClockFunc(s.grantNow),
	})
	if err != nil {
		return nil, fmt.Errorf("grant verifier: %w", err)
	}
	s.vc.creds, s.vc.v = c, v
	return v, nil
}

// grantNow is the time grants are judged at: the later of the host clock and
// the clock floor while the floor is healthy, and otherwise the zero time,
// which the verifier refuses as untrustworthy (GRANT_UNAUTHENTICATED). Unlike
// the client certificate check (verifyNow), which falls back to the host
// clock so that reads keep answering, the operation path never judges a
// grant at a clock the floor does not vouch for, even if the floor's state
// changes between the readiness gate and the verification.
func (s *Server) grantNow() time.Time {
	if s.opts.Agent.ClockFloorState() != clockfloor.StateOK {
		return time.Time{}
	}
	return s.verifyNow()
}

// operate is the operation path of one mutating call, after the peer check
// and the readiness gate, in this order:
//
//  1. the grant is verified (signature, chain and validity at the clock
//     floor, body rules, kind against the method and request type, request
//     decoding, intent digest, installation and host);
//  2. the grant's VM and Provider UIDs have the shape of a UID, and the
//     decoded request names this host and, as its owner, the grant's VM
//     (GRANT_MISMATCH:
//     nothing is recorded, so the refusal is definite);
//  3. a kind the executor does not implement is Unimplemented;
//  4. a new op_id (one the store has no trace of) needs the store's active
//     enrolment to be the one the host certificate names (UNATTESTED
//     otherwise, never definite);
//  5. the operation store decides: a known op_id is answered from its record
//     or tombstone before the grant's Provider and enrolment are compared
//     with the active enrolment; an enrolment the store has no record of is
//     UNKNOWN_TO_AGENT, never a definite refusal;
//  6. an accepted operation gets a runner, and the call is answered with its
//     task reference once its record is durable.
//
// Every decision is logged as one audit line with identity fields only.
func (s *Server) operate(ctx context.Context, method string, req any) (any, error) {
	outer, ok := req.(proto.Message)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "not a request message")
	}
	actor := peerIdentity(ctx)
	v, err := s.verifier()
	if err != nil {
		s.opts.Log.Error(err, "operation refused: no grant verifier", "method", method)
		return nil, status.Error(codes.Unavailable, "the host cannot verify grants")
	}
	verify := v.VerifyHostBound
	if guard.Off(guard.LookupBeforeEnrolment) {
		verify = v.Verify
	}
	verified, err := verify(method, outer)
	if err != nil {
		return nil, s.verifyRefusal(method, actor, err)
	}
	body := verified.Body
	opHex := hex.EncodeToString(body.GetOpId())
	audit := func(decision, reason string) {
		s.opts.Log.Info("operation decision", "decision", decision, "reason", reason, "method", method,
			"op", opHex, "enrolment", hex.EncodeToString(body.GetEnrolmentId()), "vm", body.GetVmUid(),
			"kind", body.GetKind().String(), "attempt", body.GetAttempt(), "actor", actor)
	}
	if !uidRE.MatchString(body.GetVmUid()) || !uidRE.MatchString(body.GetProviderUid()) {
		// A grant whose UIDs the host could never name in an answer is
		// refused before admission; nothing was recorded or performed.
		audit("refused", grantverify.ReasonGrantMismatch)
		return nil, refusal(codes.FailedPrecondition, grantverify.ReasonGrantMismatch, "the grant's VM or Provider UID is not a UID")
	}
	if err := checkInner(verified, s.opts.Credentials.Load().Identity.HostID()); err != nil {
		audit("refused", grantverify.ReasonGrantMismatch)
		return nil, refusal(codes.FailedPrecondition, grantverify.ReasonGrantMismatch, err.Error())
	}
	if !s.opts.Operations.Supports(verified.Kind) {
		audit("refused", "unimplemented")
		return nil, status.Errorf(codes.Unimplemented, "this agent does not perform %s", verified.Kind)
	}
	r := opstore.Request{
		ProviderUID: body.GetProviderUid(), EnrolmentID: body.GetEnrolmentId(), OpID: body.GetOpId(),
		VMUID: body.GetVmUid(), BindingID: body.GetBindingId(), Kind: verified.Kind, Attempt: body.GetAttempt(),
		IntentDigest: body.GetIntentDigest(),
		MintEpoch:    opstore.Epoch{Counter: body.GetMintEpoch().GetCounter(), Holder: body.GetMintEpoch().GetHolder()},
		Actor:        truncate(actor, actorMax),
	}
	ref := opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}
	if err := s.checkNewAdmission(ctx, ref); err != nil {
		audit("refused", opstore.ReasonUnattested)
		return nil, err
	}
	d, err := s.opts.Operations.Admit(ctx, r)
	if errors.Is(err, opstore.ErrInvalidRequest) {
		// The store refused a malformed field before reading or writing
		// anything: the request was not, and will never be, performed.
		audit("refused", grantverify.ReasonGrantMismatch)
		return nil, refusal(codes.FailedPrecondition, grantverify.ReasonGrantMismatch, "the grant carries a field the host cannot record")
	}
	if err != nil {
		// The store could not decide; nothing may be said about the op_id.
		audit("error", "store")
		s.opts.Log.Error(err, "the operation store could not decide", "op", opHex)
		return nil, status.Error(codes.Unavailable, "the host's operation store could not decide; ask again with the same op_id")
	}
	switch d.Outcome {
	case opstore.OutcomeAccepted:
		s.opts.Operations.Start(ref, verified.RequestBytes)
	case opstore.OutcomeKnown:
		if d.Op.State == opstore.StateAccepted || d.Op.State == opstore.StateExecuting {
			// A run that ended early (a store error) is resumed; a live
			// run makes this a no-op.
			s.opts.Operations.Start(ref, verified.RequestBytes)
		}
	default:
		audit("refused", d.Refusal.String())
		return nil, admissionRefusal(d.Refusal)
	}
	audit(map[opstore.Outcome]string{opstore.OutcomeAccepted: "accepted", opstore.OutcomeKnown: "known"}[d.Outcome], d.Op.State.String())
	task := &providerv1.TaskRef{Id: TaskRef(ref)}
	if verified.Kind == providerv1.OperationKind_OPERATION_KIND_CREATE {
		return &providerv1.CreateResponse{Id: r.VMUID, Task: task}, nil
	}
	return &providerv1.TaskResponse{Task: task}, nil
}

// checkNewAdmission refuses a new operation while the store's active
// enrolment is not the one the host certificate names (UNATTESTED with the
// cause CauseEnrolmentMismatch, never definite): such a host signs no answer,
// so it must not take on operations it could not answer for. An op_id the
// store already knows is answered from its record or tombstone as always.
func (s *Server) checkNewAdmission(ctx context.Context, ref opstore.OpRef) error {
	v, err := s.opts.Operations.AnswerOp(ctx, ref, 0)
	if err != nil {
		s.opts.Log.Error(err, "the operation store could not be read", "op", ref.String())
		return status.Error(codes.Unavailable, "the host's operation store could not decide; ask again with the same op_id")
	}
	if v.Found {
		return nil
	}
	c := s.opts.Credentials.Load()
	e := v.Host.Enrolment
	if e != nil && bytes.Equal(e.ID, c.Identity.Record.EnrolmentID) && e.ProviderUID == c.Identity.Record.ProviderUID &&
		bytes.Equal(e.InstallID, c.Identity.Record.InstallID) && e.HostID == c.Identity.HostID() {
		return nil
	}
	return refusalWith(codes.Unavailable, opstore.ReasonUnattested,
		"the host refuses operations: "+CauseEnrolmentMismatch, map[string]string{MetadataCause: CauseEnrolmentMismatch})
}

// actorMax bounds the actor recorded for an operation.
const actorMax = 256

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// checkInner checks the decoded request against the grant: it names this
// host, and its owner, which every executable kind's request carries, is set
// and is the grant's VM.
func checkInner(v *grantverify.Verified, hostID string) error {
	if r, ok := v.Request.(interface{ GetTargetHostId() string }); !ok || r.GetTargetHostId() != hostID {
		return errors.New("the request names another host than the grant")
	}
	r, ok := v.Request.(interface {
		GetOwner() *providerv1.ObjectIdentity
	})
	if !ok || r.GetOwner().GetUid() == "" {
		return errors.New("the request names no owner")
	}
	if r.GetOwner().GetUid() != v.Body.GetVmUid() {
		return errors.New("the request names another owner than the grant's VM")
	}
	return nil
}

// verifyRefusal answers a refused grant as the verifier classified it:
// INVALID_ARGUMENT for outer fields beside the envelope, otherwise
// FAILED_PRECONDITION, with the refusal's reason. It never quotes the
// request.
func (s *Server) verifyRefusal(method, actor string, err error) error {
	r, ok := grantverify.AsRefusal(err)
	if !ok {
		s.opts.Log.Error(err, "operation refused: the grant could not be verified", "method", method)
		return status.Error(codes.Internal, "the grant could not be verified")
	}
	s.opts.Log.Info("operation decision", "decision", "refused", "reason", r.Reason, "rule", string(r.Rule),
		"method", method, "actor", actor)
	c := codes.FailedPrecondition
	if r.InvalidArgument {
		c = codes.InvalidArgument
	}
	return refusal(c, r.Reason, r.Error())
}

// admissionRefusal answers a refusal of the operation store. A grant naming
// another Provider than the enrolled one is a grant that does not fit this
// host (GRANT_MISMATCH): the caller itself is the enrolled Provider, which
// the peer check established.
func admissionRefusal(r opstore.Refusal) error {
	switch r {
	case opstore.RefusalQuarantined:
		return refusal(codes.FailedPrecondition, opstore.ReasonQuarantined, "the host's operation store is quarantined")
	case opstore.RefusalUnattested:
		return refusal(codes.Unavailable, opstore.ReasonUnattested, "the host's operation store has not established its continuity")
	case opstore.RefusalProviderMismatch, opstore.RefusalGrantMismatch:
		return refusal(codes.FailedPrecondition, grantverify.ReasonGrantMismatch, "the grant does not fit this host's enrolment")
	case opstore.RefusalOpIDConflict:
		return refusal(codes.FailedPrecondition, opstore.ReasonOpIDConflict, "this host knows the op_id with other identity fields, or the attempt does not increase")
	case opstore.RefusalVMBusy:
		return refusal(codes.Aborted, opstore.ReasonVMBusy, "the host runs another operation for this VM")
	case opstore.RefusalVMBindingStale:
		return refusal(codes.FailedPrecondition, opstore.ReasonVMBindingStale, "the grant's binding is not the VM's placement on this host")
	case opstore.RefusalVMNotOwned:
		return refusal(codes.NotFound, opstore.ReasonVMNotOwned, "this host has no placement of the VM")
	case opstore.RefusalUnknownToAgent:
		return refusal(codes.NotFound, opstore.ReasonUnknownToAgent, "this host holds no record of the grant's enrolment")
	case opstore.RefusalStoreFull:
		return status.Error(codes.ResourceExhausted, "the host's operation store is full")
	}
	return status.Error(codes.Internal, "the operation was not admitted")
}
