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
	"context"
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// The recovery table, every row.
func TestClassify(t *testing.T) {
	type F = opstore.StepFacts
	rows := []struct {
		name string
		f    F
		want opstore.Action
	}{
		{"done", F{Intent: true, Done: true}, opstore.ActionSkip},
		{"done once with witness", F{Once: true, Intent: true, Done: true, Witness: true}, opstore.ActionSkip},
		{"witness without intent", F{Once: true, Witness: true}, opstore.ActionInconsistent},
		{"witness without intent on an idempotent step", F{Witness: true}, opstore.ActionInconsistent},
		{"no intent", F{}, opstore.ActionRun},
		{"no intent once", F{Once: true}, opstore.ActionRun},
		{"no intent irreversible", F{Irreversible: true}, opstore.ActionRun},
		{"idempotent with intent", F{Intent: true}, opstore.ActionReRun},
		{"once with intent, no witness", F{Once: true, Intent: true}, opstore.ActionRunOnce},
		{"once with witness, effect present", F{Once: true, Intent: true, Witness: true, Evidence: executor.EvidencePresent}, opstore.ActionDoneInspection},
		{"once with witness, effect absent", F{Once: true, Intent: true, Witness: true, Evidence: executor.EvidenceAbsent}, opstore.ActionUnknownWitnessNoEffect},
		{"once with witness, inspection undecided", F{Once: true, Intent: true, Witness: true, Evidence: executor.EvidenceUnknown}, opstore.ActionUnknownProbe},
		{"once with witness, probe error", F{Once: true, Intent: true, Witness: true, Evidence: executor.EvidencePresent, ProbeFailed: true}, opstore.ActionUnknownProbe},
		{"once+irreversible with witness, effect present", F{Once: true, Irreversible: true, Intent: true, Witness: true, Evidence: executor.EvidencePresent}, opstore.ActionDoneInspection},
		{"once+irreversible, no witness", F{Once: true, Irreversible: true, Intent: true}, opstore.ActionRunOnce},
		{"irreversible, goal holds", F{Irreversible: true, Intent: true, GoalReached: true}, opstore.ActionDoneGoal},
		{"irreversible, goal not observed", F{Irreversible: true, Intent: true}, opstore.ActionUnknownGoalNotObserved},
		{"irreversible, probe error", F{Irreversible: true, Intent: true, GoalReached: true, ProbeFailed: true}, opstore.ActionUnknownProbe},
	}
	for _, r := range rows {
		if got := opstore.Classify(r.f); got != r.want {
			t.Errorf("%s: Classify = %v, want %v", r.name, got, r.want)
		}
	}
	if !(opstore.StepFacts{Once: true, Intent: true, Witness: true}).NeedsInspection() {
		t.Error("a once step with its witness needs inspection")
	}
	if !(opstore.StepFacts{Irreversible: true, Intent: true}).NeedsGoal() {
		t.Error("an irreversible step with its intent needs a goal check")
	}
}

// A witness without its intent at recovery: OutcomeUnknown and quarantine.
func TestRecover_WitnessWithoutIntentQuarantines(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.mem.Put(witnessPathFor(h, r, 1), nil)
	_ = h.s.Close()
	h.mem.Crash()
	s := h.open()
	if err := s.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(context.Background()); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("recover: %v", err)
	}
	if st, reason := s.Journal().State(); st != journal.StateLost || reason != journal.LostInconsistentStore {
		t.Fatalf("state %v{%s}", st, reason)
	}
	v, _ := h.lookup(r)
	if v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonInconsistentStore {
		t.Fatalf("op %v{%s}", v.State, v.Reason)
	}
	if h.exec.effects(r.OpID, "start") != 0 {
		t.Fatal("effect ran")
	}
}

// A probe error at recovery gives OutcomeUnknown, never a guess.
func TestRecover_ProbeErrorIsOutcomeUnknown(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterEffect, N: 2}
	_, _ = h.s.Execute(context.Background(), ref(r), nil)
	h.exec.probe = errors.New("cannot list processes")
	h.restart()
	v, _ := h.lookup(r)
	if v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonProbeError {
		t.Fatalf("op %v{%s}", v.State, v.Reason)
	}
}

// Recovery never commits before continuity is established.
func TestRecover_RefusedBeforeContinuity(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterEffect, N: 2}
	_, _ = h.s.Execute(context.Background(), ref(r), nil)
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	s := h.open()
	before := s.Journal().Position()
	if _, err := s.Recover(context.Background()); !errors.Is(err, journal.ErrUnattested) {
		t.Fatalf("recover before continuity: %v", err)
	}
	if s.Journal().Position() != before {
		t.Fatal("recovery committed before continuity")
	}
	if _, err := s.Execute(context.Background(), ref(r), nil); !errors.Is(err, opstore.ErrNotReady) {
		t.Fatalf("execute before recovery: %v", err)
	}
}

// A rolled-back store is never recovered.
func TestRecover_LostStoreIsNotRecovered(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	h.s.Journal().Quarantine(journal.LostRolledBack, "test")
	if _, err := h.s.Recover(context.Background()); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("recover on a lost store: %v", err)
	}
}

// A record is reported Executing only while a runner owns it: after a
// restart, every open operation is resolved or resumed before the store is
// ready.
func TestRecover_NoOperationLeftExecuting(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	r := h.req("vm", b, kReconfig, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterIntent, N: 1}
	_, _ = h.s.Execute(context.Background(), ref(r), nil)
	refs := h.restart()
	if len(refs) != 1 {
		t.Fatalf("runnable after restart: %d", len(refs))
	}
	if v, _ := h.lookup(r); v.State != opstore.StateCompleted {
		t.Fatalf("state %v", v.State)
	}
}
