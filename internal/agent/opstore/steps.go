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
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Step evidence values recorded in a done step.
const (
	evidenceEffect     = "effect"
	evidenceInspection = "inspection"
	evidenceGoal       = "goal"
)

// State reasons.
const (
	// ReasonWitnessNoEffect: a once step's witness exists and its effect was
	// not found by inspection.
	ReasonWitnessNoEffect = "witness_no_effect"
	// ReasonInconsistentStore: a witness exists without its step intent.
	ReasonInconsistentStore = "inconsistent_store"
	// ReasonStepDeadline: a step ran past its deadline and inspection could
	// not decide.
	ReasonStepDeadline = "step_deadline"
	// ReasonEffectError: a step failed in a way that leaves its effect in
	// doubt, and inspection could not decide.
	ReasonEffectError = "effect_error"
	// ReasonGoalNotObserved: an irreversible step's intent is durable and its
	// goal state was not observed after a restart.
	ReasonGoalNotObserved = "goal_not_observed"
	// ReasonProbeError: an inspection or goal check failed.
	ReasonProbeError = "probe_error"
	// ReasonProviderChanged: the operation was frozen by a re-enrolment.
	ReasonProviderChanged = "provider_changed"
	// ReasonNotPerformed: the executor reported the step definitely not
	// performed.
	ReasonNotPerformed = "not_performed"
	// ReasonRunFailed: the operation's run failed repeatedly on errors that
	// are neither a quarantine nor a stop, and was given up (ResolveUnknown).
	ReasonRunFailed = "run_failed"
	// ReasonRunPanicked: the operation's runs panicked repeatedly, each one
	// stopping the agent, and the operation was given up without running it
	// again (ResolveUnknown).
	ReasonRunPanicked = "run_panicked"
)

// panicKeyPrefix starts the key of an operation's panic count in the meta
// bucket.
const panicKeyPrefix = "panic/"

// panicKey is the key of ref's panic count.
func panicKey(ref OpRef) []byte {
	return append([]byte(panicKeyPrefix), opKey(ref.EnrolmentID, ref.OpID)...)
}

// RecordPanic durably adds one to the number of runs of ref that panicked and
// returns the new count. The agent calls it from the run that panicked,
// before it stops, so that a run that always panics is given up after a few
// restarts instead of stopping the host's agent forever.
func (s *Store) RecordPanic(ctx context.Context, ref OpRef) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		b, err := tx.Get(bucketMeta, panicKey(ref))
		if err != nil {
			return err
		}
		if len(b) == 4 {
			n = int(binary.BigEndian.Uint32(b))
		}
		n++
		return tx.Put(bucketMeta, panicKey(ref), binary.BigEndian.AppendUint32(nil, uint32(n))) // #nosec G115 -- a small count
	})
	if err != nil {
		return 0, fmt.Errorf("record panic of %s: %w", ref, err)
	}
	return n, nil
}

// Panics returns the number of runs of ref that panicked (RecordPanic).
func (s *Store) Panics(ctx context.Context, ref OpRef) (int, error) {
	n := 0
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		b, err := tx.Get(bucketMeta, panicKey(ref))
		if err != nil {
			return err
		}
		if len(b) == 4 {
			n = int(binary.BigEndian.Uint32(b))
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("panics of %s: %w", ref, err)
	}
	return n, nil
}

// ResolveUnknown records a runnable operation's outcome as unknown with
// reason, for a run that cannot be completed: the operation keeps holding its
// VM and is never reported running again, and nothing is said about whether
// its effect happened. An operation that already left the runnable states is
// returned as it is. It refuses while the operation has a runner.
func (s *Store) ResolveUnknown(ctx context.Context, ref OpRef, reason string) (OpView, error) {
	s.mu.Lock()
	if !s.ready || s.freezing.Load() {
		s.mu.Unlock()
		return OpView{}, ErrNotReady
	}
	k := runKey(ref)
	if s.running[k] {
		s.mu.Unlock()
		return OpView{}, ErrRunning
	}
	s.running[k] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, k)
		s.mu.Unlock()
	}()
	rec, err := s.loadOp(ctx, ref)
	if err != nil {
		return OpView{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	if rec.GetState() != StateAccepted && rec.GetState() != StateExecuting {
		return viewOfRecord(rec), nil
	}
	if err := s.outcomeUnknown(ctx, rec, reason); err != nil {
		return OpView{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	return viewOfRecord(rec), nil
}

// Lost reports whether the store lost its persistence guarantee
// (quarantined): nothing more is committed until the host is enrolled again.
func (s *Store) Lost() bool {
	st, _ := s.j.State()
	return st == journal.StateLost
}

func operationOf(id *pb.OpIdentity, payload []byte) executor.Operation {
	return executor.Operation{
		OpID:      id.GetOpId(),
		VMUID:     id.GetVmUid(),
		BindingID: id.GetBindingId(),
		Kind:      providerv1.OperationKind(id.GetKind()),
		Attempt:   id.GetAttempt(),
		Payload:   payload,
	}
}

func stepOf(st *pb.Step) executor.Step {
	return executor.Step{Name: st.GetName(), Once: st.GetOnce(), Irreversible: st.GetIrreversible()}
}

// loadOp reads an operation record.
func (s *Store) loadOp(ctx context.Context, ref OpRef) (*pb.OpRecord, error) {
	rec := &pb.OpRecord{}
	var ok bool
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		ok, err = get(tx, bucketOps, opKey(ref.EnrolmentID, ref.OpID), rec)
		return err
	}); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUnknownOperation
	}
	return rec, nil
}

// ErrConcurrentChange is returned when an operation's stored record left the
// runnable states while a writer held an older copy of it.
var ErrConcurrentChange = errors.New("operation record changed concurrently")

// saveOp writes rec (and, through extra, more writes) in one transaction. The
// stored record must still be runnable (Accepted or Executing): every caller
// writes over a runnable record, so a record that recovery or a freeze moved
// on is never overwritten by a stale copy.
func (s *Store) saveOp(ctx context.Context, rec *pb.OpRecord, extra func(tx *journal.Tx) error) error {
	id := rec.GetIdentity()
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		key := opKey(id.GetEnrolmentId(), id.GetOpId())
		cur := &pb.OpRecord{}
		ok, err := get(tx, bucketOps, key, cur)
		if err != nil {
			return err
		}
		if !ok || (cur.GetState() != StateAccepted && cur.GetState() != StateExecuting) {
			return ErrConcurrentChange
		}
		if err := put(tx, bucketOps, key, rec); err != nil {
			return err
		}
		if extra != nil {
			return extra(tx)
		}
		return nil
	})
	return err
}

// Execute runs an admitted operation's remaining steps, in the caller's
// goroutine, and returns its final state. For each step: the intent (and,
// for a once or irreversible step, the irrev flag) is committed; a once step's
// witness is created with O_EXCL and fsynced with its directory; the executor
// performs the effect; done is committed. After the last step one
// transaction writes the terminal state, the tombstone, the VM record update
// and, for a completed Delete, the VM tombstone.
//
// A step that fails is resolved by its evidence: a definite not-performed
// fails the operation (dirty when irrev is set); otherwise the executor
// inspects, and the effect found means done, its definite absence a failure,
// and no evidence OutcomeUnknown. A once step whose witness already exists is
// never run again: it is resolved by inspection only.
//
// payload is the verified request, passed to the executor in memory and
// never written. Cancelling ctx stops between steps and leaves the operation
// to recovery; it never resolves a step.
func (s *Store) Execute(ctx context.Context, ref OpRef, payload []byte) (OpView, error) {
	s.mu.Lock()
	if !s.ready || s.freezing.Load() {
		s.mu.Unlock()
		return OpView{}, ErrNotReady
	}
	k := runKey(ref)
	if s.running[k] {
		s.mu.Unlock()
		return OpView{}, ErrRunning
	}
	s.running[k] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, k)
		s.mu.Unlock()
	}()

	rec, err := s.loadOp(ctx, ref)
	if err != nil {
		return OpView{}, fmt.Errorf("execute %s: %w", ref, err)
	}
	if rec.GetState() != StateAccepted && rec.GetState() != StateExecuting {
		return viewOfRecord(rec), nil
	}
	if len(rec.GetSteps()) == 0 {
		plan, err := s.exec.Plan(providerv1.OperationKind(rec.GetIdentity().GetKind()))
		if err != nil {
			return OpView{}, fmt.Errorf("execute %s: %w", ref, err)
		}
		if len(plan) == 0 {
			return OpView{}, fmt.Errorf("execute %s: executor returned an empty plan", ref)
		}
		for _, st := range plan {
			rec.Steps = append(rec.Steps, &pb.Step{Name: st.Name, Once: st.Once, Irreversible: st.Irreversible})
		}
	}
	op := operationOf(rec.GetIdentity(), payload)
	for i, st := range rec.GetSteps() {
		if st.GetDone() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return viewOfRecord(rec), fmt.Errorf("execute %s: %w", ref, err)
		}
		finished, err := s.runStep(ctx, rec, i, op)
		if err != nil {
			return viewOfRecord(rec), fmt.Errorf("execute %s: step %s: %w", ref, st.GetName(), err)
		}
		if finished {
			return viewOfRecord(rec), nil
		}
	}
	if err := s.j.CrashPoint(PointAfterLastDone); err != nil {
		return viewOfRecord(rec), err
	}
	if err := s.finish(ctx, rec, StateCompleted, ""); err != nil {
		return viewOfRecord(rec), fmt.Errorf("execute %s: terminal: %w", ref, err)
	}
	if err := s.j.CrashPoint(PointAfterTerminal); err != nil {
		return viewOfRecord(rec), err
	}
	return viewOfRecord(rec), nil
}

// runStep runs step i. finished is true when the operation reached a final
// state (terminal or OutcomeUnknown) in this step.
func (s *Store) runStep(ctx context.Context, rec *pb.OpRecord, i int, op executor.Operation) (finished bool, err error) {
	st := rec.GetSteps()[i]
	step := stepOf(st)
	if st.GetIntent() && st.GetIrreversible() && !st.GetOnce() {
		// An irreversible step that may have started is never run again;
		// only its goal state decides.
		return s.resolveByInspection(ctx, rec, i, op, ReasonGoalNotObserved, false)
	}
	if !st.GetIntent() {
		st.Intent = true
		if st.GetOnce() || st.GetIrreversible() {
			rec.Irrev = true
		}
		rec.State = StateExecuting
		s.mu.Lock()
		err := s.saveOp(ctx, rec, nil)
		s.mu.Unlock()
		if err != nil {
			return false, err
		}
		if err := s.j.CrashPoint(PointAfterIntent); err != nil {
			return false, err
		}
	}
	if st.GetOnce() {
		created, err := s.createWitness(rec.GetIdentity(), i)
		if err != nil {
			return false, err
		}
		if !created {
			// The witness exists: the effect may have happened. Never run
			// again; decide by inspection only.
			return s.resolveByInspection(ctx, rec, i, op, ReasonWitnessNoEffect, true)
		}
		if err := s.j.CrashPoint(PointAfterWitness); err != nil {
			return false, err
		}
	}

	stepCtx, cancel := context.WithTimeout(ctx, s.deadline(op.Kind))
	runErr := s.exec.Run(stepCtx, op, step)
	deadlineHit := stepCtx.Err() != nil && ctx.Err() == nil
	cancel()
	if err := s.j.CrashPoint(PointAfterEffect); err != nil {
		return false, err
	}
	if runErr == nil {
		return false, s.markDone(ctx, rec, i, evidenceEffect)
	}
	if ctx.Err() != nil {
		// Shutting down: leave the step to recovery.
		return false, ctx.Err()
	}
	if errors.Is(runErr, executor.ErrNotPerformed) {
		return true, s.fail(ctx, rec, ReasonNotPerformed)
	}
	reason := ReasonEffectError
	if deadlineHit {
		reason = ReasonStepDeadline
	}
	return s.resolveByInspection(ctx, rec, i, op, reason, st.GetOnce())
}

// resolveByInspection decides a step whose effect is in doubt. onceStep says
// the step is guarded by a witness: then the effect's definite absence still
// means OutcomeUnknown, because the effect may yet appear.
func (s *Store) resolveByInspection(ctx context.Context, rec *pb.OpRecord, i int, op executor.Operation,
	unknownReason string, onceStep bool) (bool, error) {
	st := rec.GetSteps()[i]
	step := stepOf(st)
	var ev executor.Evidence
	var perr error
	evidence := evidenceInspection
	if st.GetIrreversible() && !st.GetOnce() {
		var reached bool
		reached, perr = s.exec.Goal(ctx, op, step)
		evidence = evidenceGoal
		switch {
		case perr != nil:
		case reached:
			ev = executor.EvidencePresent
		default:
			ev = executor.EvidenceAbsent
		}
	} else {
		ev, perr = s.exec.Inspect(ctx, op, step)
	}
	switch {
	case perr != nil:
		return true, s.outcomeUnknown(ctx, rec, ReasonProbeError)
	case ev == executor.EvidencePresent:
		return false, s.markDone(ctx, rec, i, evidence)
	case ev == executor.EvidenceAbsent && !onceStep:
		return true, s.fail(ctx, rec, unknownReason)
	default:
		return true, s.outcomeUnknown(ctx, rec, unknownReason)
	}
}

func (s *Store) deadline(k providerv1.OperationKind) time.Duration {
	if v, ok := s.opts.StepDeadlines[k]; ok && v > 0 {
		return v
	}
	return DefaultStepDeadline
}

func (s *Store) markDone(ctx context.Context, rec *pb.OpRecord, i int, evidence string) error {
	rec.GetSteps()[i].Done = true
	rec.GetSteps()[i].Evidence = evidence
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveOp(ctx, rec, nil)
}

// fail ends the operation as a definite failure: dirty when irrev is set.
func (s *Store) fail(ctx context.Context, rec *pb.OpRecord, reason string) error {
	state := StateFailedClean
	if rec.GetIrrev() {
		state = StateFailedDirty
	}
	return s.finish(ctx, rec, state, reason)
}

// outcomeUnknown records OutcomeUnknown. The record keeps holding its VM.
func (s *Store) outcomeUnknown(ctx context.Context, rec *pb.OpRecord, reason string) error {
	rec.State, rec.Reason = StateOutcomeUnknown, reason
	buf := &auditBuf{}
	id := rec.GetIdentity()
	s.mu.Lock()
	err := s.saveOp(ctx, rec, func(tx *journal.Tx) error {
		return buf.write(tx, AuditEvent{Event: "outcome_unknown", Ref: OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
			VMUID: id.GetVmUid(), Kind: providerv1.OperationKind(id.GetKind()), Reason: reason, State: StateOutcomeUnknown})
	})
	s.mu.Unlock()
	if err == nil {
		s.flush(buf)
	}
	return err
}

// finish writes a terminal state in one transaction: the record, its
// tombstone, the VM record update and, for a completed Delete, the VM
// tombstone.
func (s *Store) finish(ctx context.Context, rec *pb.OpRecord, state OpState, reason string) error {
	rec.State, rec.Reason = state, reason
	buf := &auditBuf{}
	s.mu.Lock()
	err := s.saveOp(ctx, rec, func(tx *journal.Tx) error { return terminalWrites(tx, rec, buf) })
	s.mu.Unlock()
	if err == nil {
		s.flush(buf)
	}
	return err
}

// terminalWrites are the writes that go with a terminal record, in the same
// transaction.
func terminalWrites(tx *journal.Tx, rec *pb.OpRecord, buf *auditBuf) error {
	id := rec.GetIdentity()
	key := opKey(id.GetEnrolmentId(), id.GetOpId())
	if err := put(tx, bucketTombstones, key, &pb.Tombstone{Identity: id, State: rec.GetState(), Irrev: rec.GetIrrev(),
		Reason: rec.GetReason()}); err != nil {
		return err
	}
	vm := &pb.VMRecord{}
	hasVM, err := get(tx, bucketVMs, []byte(id.GetVmUid()), vm)
	if err != nil {
		return err
	}
	kind := providerv1.OperationKind(id.GetKind())
	if hasVM && sameBytes(vm.GetBusyOpId(), id.GetOpId()) && sameBytes(vm.GetBusyEnrolmentId(), id.GetEnrolmentId()) {
		if !holdsVM(rec.GetState()) {
			vm.BusyOpId, vm.BusyEnrolmentId = nil, nil
		}
		switch {
		case kind == providerv1.OperationKind_OPERATION_KIND_DELETE && rec.GetState() == StateCompleted:
			if err := tx.Delete(bucketVMs, []byte(id.GetVmUid())); err != nil {
				return err
			}
			if err := put(tx, bucketVMTombstones, vmTombstoneKey(id.GetVmUid(), id.GetBindingId()),
				&pb.VMTombstone{DeleteOpId: id.GetOpId(), TerminalSeq: tx.Seq(), EnrolmentId: id.GetEnrolmentId()}); err != nil {
				return err
			}
			hasVM = false
		case kind == providerv1.OperationKind_OPERATION_KIND_CREATE && rec.GetState() == StateCompleted:
			vm.Lifecycle = pb.VMLifecycle_VM_LIFECYCLE_PRESENT
		case kind == providerv1.OperationKind_OPERATION_KIND_CREATE && vm.GetLifecycle() == pb.VMLifecycle_VM_LIFECYCLE_CREATING &&
			(rec.GetState() == StateFailedClean || rec.GetState() == StateRetired):
			// Provably no VM was made: forget the placement.
			if err := tx.Delete(bucketVMs, []byte(id.GetVmUid())); err != nil {
				return err
			}
			hasVM = false
		}
		if hasVM {
			if err := put(tx, bucketVMs, []byte(id.GetVmUid()), vm); err != nil {
				return err
			}
		}
	}
	return buf.write(tx, AuditEvent{Event: "terminal", Ref: OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
		VMUID: id.GetVmUid(), Kind: kind, Reason: rec.GetReason(), State: rec.GetState()})
}
