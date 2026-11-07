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

package fake

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdktasks "github.com/projectbeskar/virtrigaud/sdk/provider/tasks"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
)

// opTaskPrefix starts the inner part of the task reference of an operation
// accepted in identity mode: "op-" and the op_id in hex, so the host answers
// TaskStatus from the operation's record.
const opTaskPrefix = "op-"

// Identity-mode defaults. The epochs are identity-only stand-ins: the fake
// compares a first delivery's mint_epoch counter with the enrolment start
// counter and makes no other use of them.
const (
	// identityEpochCounter is the epoch counter the suite issues grants under.
	identityEpochCounter = 2
	// identityEnrolmentStart is the epoch counter at which the fake's hosts
	// were enrolled.
	identityEnrolmentStart = 2
	// identityProviderUID is the fake Provider's metadata.uid.
	identityProviderUID = "5f0c2a8e-1d3b-4e6f-9a7c-2b4d6e8f0a1c"
)

// envelopeField is the name of the operation_envelope field of every
// mutating request message.
const envelopeField protoreflect.Name = "operation_envelope"

// IdentityConfig configures the fake's operation-identity mode (NewIdentity).
//
// In that mode the fake reports supports_operation_identity, each of its
// hosts verifies the grant of a signed Create, Delete, Power or Reconfigure with the shared verifier
// (internal/operation) against the pinned anchor, and executes an operation
// at most once per op_id. Each host keeps, in memory, a record of every
// operation it accepted, and never drops it (a tombstone once the operation
// is over): a re-delivery of a known op_id is answered from it, with the
// answer the first delivery got, and a known op_id with other identity
// fields or another intent_digest is refused with OP_ID_CONFLICT. A new
// operation for a VM that has a non-terminal one is refused with VM_BUSY. An
// op_id minted before the host's enrolment start epoch is refused on its
// first delivery. The task reference of an accepted operation encodes its
// op_id, and TaskStatus reports a typed outcome, which a test can script
// (ScriptOutcome). The records live on the hosts, so they survive Restart.
//
// Its other calls are those of the ordinary fake; snapshot calls are not
// operation-identity calls here.
type IdentityConfig struct {
	// Anchor is the trust anchor the fake's hosts pin. It is never taken from
	// a grant.
	Anchor *x509.Certificate
	// Signer is handed to the suite (Target.OperationIdentity) to sign the
	// operations it sends. The fake never signs; it only reads InstallID.
	Signer operation.Signer
	// Required makes the fake report requires_operation_identity and refuse
	// a mutating call that carries no operation_envelope (IDENTITY_REQUIRED).
	// Without it the fake also accepts unsigned calls, as before.
	Required bool
}

// identityMode is the fake's operation-identity state that is fixed when the
// fake is made: the verifiers its hosts use and the identity they expect.
type identityMode struct {
	required   bool
	signer     operation.Signer
	verifiers  map[string]*operation.Verifier
	enrolments map[string][]byte
	epoch      operation.Epoch
	startEpoch uint64
}

// newIdentityMode builds the verifier of each host from cfg.
func newIdentityMode(cfg IdentityConfig, hosts []string) (*identityMode, error) {
	if cfg.Anchor == nil || cfg.Signer == nil {
		return nil, errors.New("fake: operation identity needs an anchor and a signer")
	}
	holder, err := operation.NewRandomID()
	if err != nil {
		return nil, fmt.Errorf("fake: epoch holder: %w", err)
	}
	m := &identityMode{
		required:   cfg.Required,
		signer:     cfg.Signer,
		verifiers:  map[string]*operation.Verifier{},
		enrolments: map[string][]byte{},
		epoch:      operation.Epoch{Counter: identityEpochCounter, Holder: holder},
		startEpoch: identityEnrolmentStart,
	}
	for _, h := range hosts {
		enrolment, err := operation.NewRandomID()
		if err != nil {
			return nil, fmt.Errorf("fake: enrolment id: %w", err)
		}
		v, err := operation.NewVerifier(operation.VerifierConfig{
			Anchors:     []*x509.Certificate{cfg.Anchor},
			InstallID:   cfg.Signer.InstallID(),
			ProviderUID: identityProviderUID,
			HostID:      h,
			EnrolmentID: enrolment,
		})
		if err != nil {
			return nil, fmt.Errorf("fake: verifier for %s: %w", h, err)
		}
		m.verifiers[h], m.enrolments[h] = v, enrolment
	}
	return m, nil
}

// opRecord is a host's record of one operation it accepted. It is never
// dropped (unless the fake commits DropsTombstones).
type opRecord struct {
	kind      providerv1.OperationKind
	vmUID     string
	attempt   uint64
	digest    []byte
	bindingID []byte
	mintEpoch *providerv1.Epoch
	// task is the inner task id ("" for a synchronous operation).
	task string
	// resp is the answer the first delivery got.
	resp proto.Message
}

// sameIdentity reports whether b names the operation r records.
func (r *opRecord) sameIdentity(b *providerv1.GrantBody) bool {
	return r.kind == b.GetKind() && r.vmUID == b.GetVmUid() && r.attempt == b.GetAttempt() &&
		bytes.Equal(r.digest, b.GetIntentDigest()) && bytes.Equal(r.bindingID, b.GetBindingId()) &&
		proto.Equal(r.mintEpoch, b.GetMintEpoch())
}

// scripted is an outcome a test asked the next accepted operation to end with.
type scripted struct {
	outcome providerv1.TaskOutcome
	reason  string
}

// admission is a signed call the host admitted: the grant body and the
// request to act on.
type admission struct {
	body *providerv1.GrantBody
	opID string
}

// identityRefusal answers a verifier refusal as an agent would:
// INVALID_ARGUMENT for outer fields set beside the envelope, otherwise
// FAILED_PRECONDITION, with the refusal's reason. It never quotes the request.
func identityRefusal(r *operation.Refusal) error {
	c := codes.FailedPrecondition
	if r.InvalidArgument {
		c = codes.InvalidArgument
	}
	return withInfo(c, r.Error(), r.Reason, contracts.ErrorInfoDomain, nil)
}

// ignored reports whether the fake's violations make it act on a call the
// verifier refused under rule.
func (p *provider) ignored(rule operation.Rule) bool {
	switch rule {
	case operation.RuleChain, operation.RuleSignature:
		return p.v(AcceptsForgedGrant)
	case operation.RuleWrongRPC, operation.RuleFixedField:
		return p.v(AcceptsWrongKindGrant)
	case operation.RuleOuterFieldSet:
		return p.v(AcceptsOuterFields)
	case operation.RuleIntentDigest:
		return p.v(IgnoresDigest)
	case operation.RuleBinding:
		return p.v(IgnoresBinding)
	}
	return false
}

// envelopeOf returns the operation_envelope of outer, or nil.
func envelopeOf(outer proto.Message) *providerv1.OperationEnvelope {
	r := outer.ProtoReflect()
	fd := r.Descriptor().Fields().ByName(envelopeField)
	if fd == nil || !r.Has(fd) {
		return nil
	}
	env, _ := r.Get(fd).Message().Interface().(*providerv1.OperationEnvelope)
	return env
}

// admit is the host-side gate of a mutating call in identity mode, before the
// fake acts. It returns the request to act on and, for a signed call, its
// admission:
//
//   - not in identity mode: outer, as before (an envelope is ignored);
//   - no envelope: IDENTITY_REQUIRED when identity is required, otherwise
//     outer;
//   - an envelope: the call is routed by a decoded copy of the request, the
//     host's verifier checks it (operation.Verifier.Verify), and the decoded,
//     verified request is the only one acted on.
//
// It takes no lock: the verifiers are fixed and safe for concurrent use.
func (p *provider) admit(fullMethod string, outer proto.Message) (proto.Message, *admission, error) {
	if p.ident == nil {
		return outer, nil, nil
	}
	env := envelopeOf(outer)
	if env == nil {
		if p.ident.required && !p.v(AcceptsUnsigned) {
			return nil, nil, withInfo(codes.FailedPrecondition, "this provider requires an operation grant on every mutating call",
				contracts.IdentityRequiredReason, contracts.ErrorInfoDomain, nil)
		}
		return outer, nil, nil
	}
	if p.v(RoutesFromOuter) {
		// Acts on the outer request, which a signed call leaves empty.
		return outer, &admission{body: &providerv1.GrantBody{}}, nil
	}
	routing := outer.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(env.GetRequest(), routing); err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, "operation_envelope.request does not decode")
	}
	host := ""
	if r, ok := routing.(interface{ GetTargetHostId() string }); ok {
		host = r.GetTargetHostId()
	}
	verifier := p.ident.verifiers[host]
	if verifier == nil {
		return nil, nil, p.routeError(host)
	}
	verified, err := verifier.Verify(fullMethod, outer)
	if err == nil {
		return verified.Request, &admission{body: verified.Body, opID: hex.EncodeToString(verified.Body.GetOpId())}, nil
	}
	refusal, ok := operation.AsRefusal(err)
	if !ok {
		return nil, nil, status.Error(codes.Internal, "the grant could not be verified")
	}
	if !p.ignored(refusal.Rule) {
		return nil, nil, identityRefusal(refusal)
	}
	// A violation: act on what the grant and the envelope say, unverified.
	body := &providerv1.GrantBody{}
	_ = proto.Unmarshal(env.GetGrant().GetBody(), body)
	return routing, &admission{body: body, opID: hex.EncodeToString(body.GetOpId())}, nil
}

// routeError is the answer for a signed call whose decoded request names a
// host the fake has no verifier for.
func (p *provider) routeError(host string) error {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if _, _, err := p.route(host, false); err != nil {
		return err
	}
	return status.Error(codes.InvalidArgument, "target_host_id names no enrolled host")
}

// ops returns the operation records of host: on the host, or in the
// provider's memory when the fake commits TombstonesInProviderMemory. The
// caller holds w.mu.
func (p *provider) ops(host string, hs *hostState) map[string]*opRecord {
	if p.v(TombstonesInProviderMemory) {
		if p.memOps[host] == nil {
			p.memOps[host] = map[string]*opRecord{}
		}
		return p.memOps[host]
	}
	return hs.ops
}

// taskOf returns the task inner on host, wherever the fake keeps it. The
// caller holds w.mu.
func (p *provider) taskOf(host string, hs *hostState, inner string) *task {
	if p.v(TasksInMemory) {
		return p.memTasks[host+"/"+inner]
	}
	return hs.tasks[inner]
}

// nonTerminal reports whether rec's operation is still running or its
// outcome is unknown. The caller holds w.mu.
func (p *provider) nonTerminal(host string, hs *hostState, rec *opRecord) bool {
	if rec.task == "" {
		return false
	}
	k := p.taskOf(host, hs, rec.task)
	return k != nil && (!k.done || k.outcome == providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN)
}

// agentStep is the host's single-execution check of a signed call, after
// routing and before the fake acts. When it answers the call (a known op_id,
// or a refusal) done is true and the handler returns resp, err. Otherwise the
// call goes on and is remembered on success. The caller holds w.mu.
func (p *provider) agentStep(host string, hs *hostState, adm *admission) (resp proto.Message, done bool, err error) {
	if adm == nil || adm.opID == "" {
		return nil, false, nil
	}
	b := adm.body
	ops := p.ops(host, hs)
	if rec := ops[adm.opID]; rec != nil && !p.v(ExecutesDuplicateOpID) {
		if !rec.sameIdentity(b) && !p.v(IgnoresOpIDConflict) {
			return nil, true, withInfo(codes.FailedPrecondition, "this host knows the op_id with other identity fields",
				contracts.OpIDConflictReason, contracts.ErrorInfoDomain, nil)
		}
		return proto.Clone(rec.resp), true, nil
	}
	if b.GetMintEpoch().GetCounter() < p.ident.startEpoch && !p.v(IgnoresMintEpoch) {
		return nil, true, status.Error(codes.FailedPrecondition, "the op_id was minted before this host's enrolment")
	}
	if !p.v(IgnoresVMBusy) {
		for _, rec := range ops {
			if rec.vmUID == b.GetVmUid() && p.nonTerminal(host, hs, rec) {
				return nil, true, withInfo(codes.Aborted, "the host runs another operation for this VM",
					contracts.VMBusyReason, contracts.ErrorInfoDomain, nil)
			}
		}
	}
	p.cur = adm
	return nil, false, nil
}

// remember records the operation adm on host with the answer resp, and ends
// the admission. The caller holds w.mu.
func (p *provider) remember(host string, hs *hostState, adm *admission, resp proto.Message, task *providerv1.TaskRef) {
	p.cur = nil
	if adm == nil || adm.opID == "" {
		return
	}
	b := adm.body
	rec := &opRecord{
		kind: b.GetKind(), vmUID: b.GetVmUid(), attempt: b.GetAttempt(),
		digest: append([]byte(nil), b.GetIntentDigest()...), bindingID: append([]byte(nil), b.GetBindingId()...),
		mintEpoch: proto.Clone(b.GetMintEpoch()).(*providerv1.Epoch), resp: proto.Clone(resp),
	}
	if ref := task.GetId(); ref != "" {
		rec.task = opTaskPrefix + adm.opID
	}
	if rec.task == "" && p.v(DropsTombstones) {
		return
	}
	p.ops(host, hs)[adm.opID] = rec
}

// opTask names and scripts the task of the operation being admitted, if any.
// The caller holds w.mu.
func (p *provider) opTask(hs *hostState, t *task) (string, bool) {
	if p.cur == nil || p.cur.opID == "" {
		return "", false
	}
	t.typed, t.outcome = true, providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED
	if hs.script != nil {
		t.outcome, t.reason = hs.script.outcome, hs.script.reason
		hs.script = nil
	}
	return opTaskPrefix + p.cur.opID, true
}

// typedStatus is the TaskStatus answer of an operation-identity task.
func (p *provider) typedStatus(t *task) *providerv1.TaskStatusResponse {
	if !t.done {
		return sdktasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_RUNNING, "", "")
	}
	o := t.outcome
	switch {
	case o == providerv1.TaskOutcome_TASK_OUTCOME_RETIRED && p.v(RetiredAsSucceeded),
		o == providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN && p.v(UnknownAsDone):
		o = providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED
	case o == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY && p.v(DirtyAsClean):
		o = providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN
	}
	msg := ""
	if o == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN || o == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY {
		msg = "the operation failed on the host"
	}
	if o == providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED {
		return sdktasks.Status(o, "", "")
	}
	return sdktasks.Status(o, t.reason, msg)
}

// appliesEffect reports whether a typed task that ended with outcome had its
// effect: a success did, and a dirty failure may have; a clean failure, a
// retired, cancelled or unknown operation did not.
func appliesEffect(outcome providerv1.TaskOutcome) bool {
	return outcome == providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED || outcome == providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY
}

// OperationIdentity returns how the suite signs operations for the fake. It
// is valid only for a fake made WithOperationIdentity.
func (t *Target) OperationIdentity() clustered.OperationIdentityConfig {
	m := t.p.ident
	if m == nil {
		return clustered.OperationIdentityConfig{}
	}
	enrolments := map[string][]byte{}
	for h, id := range m.enrolments {
		enrolments[h] = append([]byte(nil), id...)
	}
	return clustered.OperationIdentityConfig{
		Signer:              m.signer,
		ProviderUID:         identityProviderUID,
		Enrolments:          enrolments,
		Epoch:               operation.Epoch{Counter: m.epoch.Counter, Holder: append([]byte(nil), m.epoch.Holder...)},
		EnrolmentStartEpoch: m.startEpoch,
	}
}

// ScriptOutcome makes the next operation host accepts end with outcome.
func (t *Target) ScriptOutcome(host string, outcome providerv1.TaskOutcome, reason string) error {
	switch outcome {
	case providerv1.TaskOutcome_TASK_OUTCOME_RETIRED, providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN,
		providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY, providerv1.TaskOutcome_TASK_OUTCOME_CANCELLED,
		providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN:
	default:
		return fmt.Errorf("fake: outcome %s cannot be scripted", outcome)
	}
	return t.withHost(host, func(hs *hostState) { hs.script = &scripted{outcome: outcome, reason: reason} })
}

// requestOf asserts the request admit returned to the handler's type.
func requestOf[M proto.Message](m proto.Message) (M, error) {
	r, ok := m.(M)
	if !ok {
		var zero M
		return zero, status.Error(codes.InvalidArgument, "the operation's request is not of this call's type")
	}
	return r, nil
}
