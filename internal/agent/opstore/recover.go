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
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// Action is what recovery does with one step of a non-terminal operation.
type Action int

// The recovery actions.
const (
	// ActionSkip: the step is done.
	ActionSkip Action = iota + 1
	// ActionRun: the step never started (no intent); the runner runs it.
	ActionRun
	// ActionReRun: an idempotent step with its intent durable; the runner
	// runs it again.
	ActionReRun
	// ActionRunOnce: a once step with its intent durable and no witness: its
	// effect provably never started; the runner runs it, once.
	ActionRunOnce
	// ActionDoneInspection: a once step with its witness, whose effect
	// inspection found: done{inspection}.
	ActionDoneInspection
	// ActionDoneGoal: an irreversible step whose goal state holds:
	// done{goal}.
	ActionDoneGoal
	// ActionUnknownWitnessNoEffect: a once step with its witness and no
	// effect found: OutcomeUnknown{witness_no_effect}. Never run again, never
	// "not performed".
	ActionUnknownWitnessNoEffect
	// ActionUnknownGoalNotObserved: an irreversible step that may have
	// started and whose goal state does not hold: OutcomeUnknown.
	ActionUnknownGoalNotObserved
	// ActionUnknownProbe: an inspection or goal check failed or could not
	// decide: OutcomeUnknown{probe_error}. Recovery never guesses.
	ActionUnknownProbe
	// ActionInconsistent: a witness without its step intent:
	// OutcomeUnknown{inconsistent_store} and the store is quarantined.
	ActionInconsistent
)

// StepFacts are the facts recovery classifies a step by.
type StepFacts struct {
	// Once and Irreversible are the step's class.
	Once, Irreversible bool
	// Intent and Done are the step's journalled flags.
	Intent, Done bool
	// Witness reports whether the step's witness file exists.
	Witness bool
	// Evidence is the result of inspecting a once step with its witness.
	Evidence executor.Evidence
	// GoalReached is the result of an irreversible step's goal check.
	GoalReached bool
	// ProbeFailed reports that the inspection or goal check returned an
	// error.
	ProbeFailed bool
}

// NeedsInspection reports whether Classify needs Evidence for f.
func (f StepFacts) NeedsInspection() bool { return !f.Done && f.Intent && f.Once && f.Witness }

// NeedsGoal reports whether Classify needs GoalReached for f.
func (f StepFacts) NeedsGoal() bool { return !f.Done && f.Intent && !f.Once && f.Irreversible }

// Classify is the recovery table for one step:
//
//	done                                  ⇒ skip
//	witness, no intent                    ⇒ OutcomeUnknown{inconsistent_store} + quarantine
//	no intent                             ⇒ run (not started)
//	once, intent, no witness              ⇒ run once (provably not started)
//	once, intent, witness, effect found   ⇒ done{inspection}
//	once, intent, witness, effect absent  ⇒ OutcomeUnknown{witness_no_effect}
//	irreversible, intent, goal holds      ⇒ done{goal}
//	irreversible, intent, goal not seen   ⇒ OutcomeUnknown{goal_not_observed}
//	idempotent, intent                    ⇒ run again
//	any probe error or undecided probe    ⇒ OutcomeUnknown{probe_error}
func Classify(f StepFacts) Action {
	switch {
	case f.Done:
		return ActionSkip
	case f.Witness && !f.Intent:
		return ActionInconsistent
	case !f.Intent:
		return ActionRun
	case f.Once && !f.Witness:
		return ActionRunOnce
	case f.Once:
		switch {
		case f.ProbeFailed || f.Evidence == executor.EvidenceUnknown:
			return ActionUnknownProbe
		case f.Evidence == executor.EvidencePresent:
			return ActionDoneInspection
		default:
			return ActionUnknownWitnessNoEffect
		}
	case f.Irreversible:
		switch {
		case f.ProbeFailed:
			return ActionUnknownProbe
		case f.GoalReached:
			return ActionDoneGoal
		default:
			return ActionUnknownGoalNotObserved
		}
	}
	return ActionReRun
}

// ErrAlreadyRecovered is returned by a second Recover: recovery runs once,
// before any runner.
var ErrAlreadyRecovered = errors.New("operation store already recovered")

// scanWitnesses checks every witness file before recovery: each must belong to
// a known enrolment and to an operation the store has a record or tombstone
// of. A witness of an unknown operation means the store lost records the
// witnesses outlived (for example a store restored with its anchor log), so
// the store is quarantined (inconsistent_store). Directories of discarded
// enrolments are left-overs of an interrupted discard and are removed.
func (s *Store) scanWitnesses(ctx context.Context) error {
	if guard.Off(guard.WitnessScan) {
		return nil
	}
	root := filepath.Join(s.j.Dir(), witnessDirName)
	dirs, err := s.fsys.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list witnesses: %w", err)
	}
	anomaly := ""
	for _, d := range dirs {
		enrol, err := hex.DecodeString(d)
		if err != nil || len(enrol) != idLen {
			anomaly = "witness directory with a malformed name"
			break
		}
		var known, retired bool
		if err := s.j.View(ctx, func(tx *journal.Tx) error {
			var err error
			if known, err = has(tx, bucketEnrolments, enrol); err != nil {
				return err
			}
			retired, err = isRetired(tx, enrol)
			return err
		}); err != nil {
			return err
		}
		if retired {
			if err := s.fsys.RemoveAll(filepath.Join(root, d)); err != nil {
				return fmt.Errorf("remove discarded witnesses: %w", err)
			}
			continue
		}
		if !known {
			anomaly = "witness directory of an unknown enrolment"
			break
		}
		files, err := s.fsys.ReadDir(filepath.Join(root, d))
		if err != nil {
			return fmt.Errorf("list witnesses: %w", err)
		}
		for _, f := range files {
			opHex, _, ok := strings.Cut(f, ".")
			op, err := hex.DecodeString(opHex)
			if !ok || err != nil || len(op) != idLen {
				anomaly = "witness file with a malformed name"
				break
			}
			var found bool
			if err := s.j.View(ctx, func(tx *journal.Tx) error {
				k := opKey(enrol, op)
				for _, b := range []string{bucketOps, bucketTombstones} {
					ok, err := has(tx, b, k)
					if err != nil {
						return err
					}
					if ok {
						found = true
						break
					}
				}
				return nil
			}); err != nil {
				return err
			}
			if !found {
				anomaly = "witness of an operation the store has no record of"
				break
			}
		}
		if anomaly != "" {
			break
		}
	}
	if anomaly == "" {
		return nil
	}
	s.j.Quarantine(journal.LostInconsistentStore, anomaly)
	s.emit(AuditEvent{Event: "quarantined", Reason: string(journal.LostInconsistentStore)})
	return &journal.LostError{Reason: journal.LostInconsistentStore}
}

// Recover resolves every non-terminal operation's in-doubt steps by the
// recovery table, then makes the store ready and returns the operations that
// still need a runner (call Execute for each). It refuses to run before
// continuity is established: a recovery commit first would hide a rollback.
// A witness without its intent quarantines the store; Recover then returns
// the lost error.
func (s *Store) Recover(ctx context.Context) ([]OpRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch st, reason := s.j.State(); {
	case st == journal.StateLost:
		return nil, fmt.Errorf("recover: %w", &journal.LostError{Reason: reason})
	case st != journal.StateReady && !guard.Off(guard.CommitAfterContinuity):
		return nil, fmt.Errorf("recover: %w", journal.ErrUnattested)
	case s.ready:
		return nil, fmt.Errorf("recover: %w", ErrAlreadyRecovered)
	}
	if s.freezing.Load() && !guard.Off(guard.FreezeResumeRecover) {
		// A crash left a re-enrolment's freeze incomplete: finish it before any
		// operation of the frozen enrolment is classified, so that each one is
		// frozen once, as provider_changed.
		var fm *freezeMarker
		if err := s.j.View(ctx, func(tx *journal.Tx) error {
			var err error
			fm, err = readFreeze(tx)
			return err
		}); err != nil {
			return nil, fmt.Errorf("recover: %w", err)
		}
		if fm != nil {
			if err := s.freezeLocked(ctx, fm.prev); err != nil {
				return nil, fmt.Errorf("recover: resume freeze: %w", err)
			}
		}
	}
	if err := s.scanWitnesses(ctx); err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	var open []*pb.OpRecord
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		return tx.ForEach(bucketOps, nil, func(_, v []byte) error {
			rec := &pb.OpRecord{}
			if err := unmarshalRecord(v, rec); err != nil {
				return err
			}
			if rec.GetState() == StateAccepted || rec.GetState() == StateExecuting {
				open = append(open, rec)
			}
			return nil
		})
	}); err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	var runnable []OpRef
	var active []byte
	_ = s.j.View(ctx, func(tx *journal.Tx) error {
		if e, err := activeEnrolment(tx); err == nil && e != nil {
			active = e.GetEnrolmentId()
		}
		return nil
	})
	for _, rec := range open {
		still, err := s.recoverOp(ctx, rec)
		if err != nil {
			return nil, fmt.Errorf("recover %x: %w", rec.GetIdentity().GetOpId(), err)
		}
		if still && sameBytes(rec.GetIdentity().GetEnrolmentId(), active) {
			runnable = append(runnable, OpRef{EnrolmentID: rec.GetIdentity().GetEnrolmentId(), OpID: rec.GetIdentity().GetOpId()})
		}
	}
	s.ready = true
	s.recovered.Store(true)
	return runnable, nil
}

// recoverOp classifies and resolves one operation's steps. still is true when
// the operation remains runnable. The caller holds s.mu.
func (s *Store) recoverOp(ctx context.Context, rec *pb.OpRecord) (still bool, err error) {
	id := rec.GetIdentity()
	op := operationOf(id, nil)
	changed := false
	steps := rec.GetSteps()
	if len(steps) == 0 {
		// No intent yet: only the plan's witnesses can be inconsistent.
		plan, err := s.exec.Plan(op.Kind)
		if err != nil {
			return false, err
		}
		for _, p := range plan {
			steps = append(steps, &pb.Step{Name: p.Name, Once: p.Once, Irreversible: p.Irreversible})
		}
	}
	for i, st := range steps {
		w, err := s.witnessExists(id, i)
		if err != nil {
			return false, err
		}
		f := StepFacts{Once: st.GetOnce(), Irreversible: st.GetIrreversible(), Intent: st.GetIntent(), Done: st.GetDone(), Witness: w}
		if f.NeedsInspection() {
			ev, perr := s.exec.Inspect(ctx, op, stepOf(st))
			f.Evidence, f.ProbeFailed = ev, perr != nil
		}
		if f.NeedsGoal() {
			reached, perr := s.exec.Goal(ctx, op, stepOf(st))
			f.GoalReached, f.ProbeFailed = reached, perr != nil
		}
		switch Classify(f) {
		case ActionSkip, ActionRun, ActionReRun, ActionRunOnce:
			continue
		case ActionDoneInspection:
			st.Done, st.Evidence, changed = true, evidenceInspection, true
		case ActionDoneGoal:
			st.Done, st.Evidence, changed = true, evidenceGoal, true
		case ActionUnknownWitnessNoEffect:
			return false, s.recoverUnknown(ctx, rec, ReasonWitnessNoEffect)
		case ActionUnknownGoalNotObserved:
			return false, s.recoverUnknown(ctx, rec, ReasonGoalNotObserved)
		case ActionUnknownProbe:
			return false, s.recoverUnknown(ctx, rec, ReasonProbeError)
		case ActionInconsistent:
			rec.Steps = steps
			if err := s.recoverUnknown(ctx, rec, ReasonInconsistentStore); err != nil {
				return false, err
			}
			s.j.Quarantine(journal.LostInconsistentStore, "witness without its step intent")
			return false, &journal.LostError{Reason: journal.LostInconsistentStore}
		}
	}
	if changed {
		if err := s.saveOp(ctx, rec, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// recoverUnknown records OutcomeUnknown during recovery. The caller holds
// s.mu.
func (s *Store) recoverUnknown(ctx context.Context, rec *pb.OpRecord, reason string) error {
	rec.State, rec.Reason = StateOutcomeUnknown, reason
	id := rec.GetIdentity()
	buf := &auditBuf{}
	err := s.saveOp(ctx, rec, func(tx *journal.Tx) error {
		return buf.write(tx, AuditEvent{Event: "outcome_unknown", Ref: OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
			VMUID: id.GetVmUid(), Reason: reason, State: StateOutcomeUnknown})
	})
	if err == nil {
		s.flush(buf)
	}
	return err
}
