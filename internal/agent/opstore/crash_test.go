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
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// crashCase is one crash point in the life of one operation. The harness
// arms the crash, delivers and runs the operation until the crash, simulates
// a power cut, boots (continuity, recovery, runners), lets the manager
// re-deliver the same operation, and checks the end state.
type crashCase struct {
	name  string
	kind  providerv1.OperationKind
	point journal.Point
	n     int // occurrence of point, counted from the delivery
	// want is the end state after recovery and re-delivery.
	want opstore.OpState
	// wantReason qualifies want.
	wantReason string
	// wantEffects is the number of runs of every once or irreversible step.
	wantEffects int
}

func crashCases() []crashCase {
	return []crashCase{
		// Admission.
		{name: "before accept", kind: kCreate, point: opstore.PointBeforeAccept, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		{name: "accept: after store commit before anchor", kind: kCreate, point: journal.PointAfterStoreCommit, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		{name: "accept: after anchor before reply", kind: kCreate, point: journal.PointAfterAnchor, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		// Steps of a once step (Create: prepare, start[once]).
		{name: "after idempotent intent", kind: kCreate, point: opstore.PointAfterIntent, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		{name: "after once intent before witness", kind: kCreate, point: opstore.PointAfterIntent, n: 2, want: opstore.StateCompleted, wantEffects: 1},
		// The witness is durable and the effect never happened: the store
		// cannot prove it did not, so it never runs the step again.
		{name: "after witness before effect", kind: kCreate, point: opstore.PointAfterWitness, n: 1, want: opstore.StateOutcomeUnknown,
			wantReason: opstore.ReasonWitnessNoEffect, wantEffects: 0},
		{name: "after once effect before done", kind: kCreate, point: opstore.PointAfterEffect, n: 2, want: opstore.StateCompleted, wantEffects: 1},
		{name: "once intent commit: after store commit before anchor", kind: kCreate, point: journal.PointAfterStoreCommit, n: 4, want: opstore.StateCompleted, wantEffects: 1},
		{name: "done commit: after anchor", kind: kCreate, point: journal.PointAfterAnchor, n: 5, want: opstore.StateCompleted, wantEffects: 1},
		{name: "after last done before terminal", kind: kCreate, point: opstore.PointAfterLastDone, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		{name: "terminal: after store commit before anchor", kind: kCreate, point: journal.PointAfterStoreCommit, n: 6, want: opstore.StateCompleted, wantEffects: 1},
		{name: "after terminal before reply", kind: kCreate, point: opstore.PointAfterTerminal, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		// Irreversible steps (Delete: stop[irreversible], remove[once,irreversible]).
		{name: "irreversible: after intent before effect", kind: kDelete, point: opstore.PointAfterIntent, n: 1, want: opstore.StateOutcomeUnknown,
			wantReason: opstore.ReasonGoalNotObserved, wantEffects: 0},
		{name: "irreversible: after effect before done", kind: kDelete, point: opstore.PointAfterEffect, n: 1, want: opstore.StateCompleted, wantEffects: 1},
		{name: "once+irreversible: after effect before done", kind: kDelete, point: opstore.PointAfterEffect, n: 2, want: opstore.StateCompleted, wantEffects: 1},
		{name: "once+irreversible: after witness before effect", kind: kDelete, point: opstore.PointAfterWitness, n: 1, want: opstore.StateOutcomeUnknown,
			wantReason: opstore.ReasonWitnessNoEffect},
		{name: "delete terminal: after terminal before reply", kind: kDelete, point: opstore.PointAfterTerminal, n: 1, want: opstore.StateCompleted, wantEffects: 1},
	}
}

func TestCrashPoints(t *testing.T) {
	for _, c := range crashCases() {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.fresh()
			b := rid()
			if c.kind != kCreate {
				h.do(h.req("vm", b, kCreate, 1))
			}
			r := h.req("vm", b, c.kind, 2)
			h.crash = &journaltest.Crasher{Point: c.point, N: c.n}
			if d, err := h.s.Admit(context.Background(), r); err == nil && d.Outcome == opstore.OutcomeAccepted {
				_, err = h.s.Execute(context.Background(), ref(r), nil)
				if err == nil {
					t.Fatalf("no crash at %s #%d", c.point, c.n)
				}
			}
			if !h.crash.Fired() {
				t.Fatalf("crash point %s #%d never reached", c.point, c.n)
			}
			h.restart()
			checkAfterCrash(t, h, r, c)
		})
	}
}

// checkAfterCrash re-delivers r (as the manager does with the same op_id)
// and checks: one effect, the expected answer, the tombstone and the chain.
func checkAfterCrash(t *testing.T, h *harness, r opstore.Request, c crashCase) {
	t.Helper()
	for round := 0; round < 2; round++ {
		d := h.admit(r)
		switch d.Outcome {
		case opstore.OutcomeAccepted, opstore.OutcomeKnown:
		default:
			t.Fatalf("re-delivery refused: %v", d.Refusal)
		}
		if d.Op.State == opstore.StateAccepted || d.Op.State == opstore.StateExecuting {
			h.run(r)
		}
	}
	v, ok := h.lookup(r)
	if !ok || v.State != c.want || (c.wantReason != "" && v.Reason != c.wantReason) {
		t.Fatalf("answer = %v{%s} (found %v), want %v{%s}", v.State, v.Reason, ok, c.want, c.wantReason)
	}
	for step, n := range h.exec.onceEffects(r.OpID, c.kind) {
		if n > 1 {
			t.Fatalf("step %s ran %d times", step, n)
		}
		if c.want == opstore.StateCompleted && n != c.wantEffects {
			t.Fatalf("step %s ran %d times, want %d", step, n, c.wantEffects)
		}
	}
	if got := h.tombstoned(r); got != opstore.Terminal(c.want) {
		t.Fatalf("tombstone present = %v, want %v", got, opstore.Terminal(c.want))
	}
	h.verifyChain()
}

// A crash between compaction batches loses nothing: tombstones and witnesses
// stay, and no operation runs again.
func TestCrashPoint_MidCompaction(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	var reqs []opstore.Request
	for i := 0; i < 5; i++ {
		r := h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1)
		h.do(r)
		reqs = append(reqs, r)
	}
	h.crash = &journaltest.Crasher{Point: opstore.PointMidCompaction, N: 2}
	if _, err := h.s.Compact(context.Background(), 1); !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("compact: %v", err)
	}
	h.restart()
	if _, err := h.s.Compact(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	for _, r := range reqs {
		d := h.admit(r)
		if d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateCompleted {
			t.Fatalf("after compaction crash: %+v", d)
		}
		if n := h.exec.effects(r.OpID, "start"); n != 1 {
			t.Fatalf("effects %d", n)
		}
		if !h.tombstoned(r) {
			t.Fatal("tombstone lost")
		}
		if ok, _ := h.mem.Exists(witnessPathFor(h, r, 1)); !ok {
			t.Fatal("witness lost")
		}
	}
	h.verifyChain()
}

// A crash in the middle of an anchor rotation, during an operation.
func TestCrashPoint_MidAnchorRotation(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.Journal.AnchorRotateBytes = 3 * journal.AnchorRecordSize }
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.crash = &journaltest.Crasher{Point: journal.PointMidAnchorRotation, N: 1}
	if d, err := h.s.Admit(context.Background(), r); err == nil && d.Outcome == opstore.OutcomeAccepted {
		_, _ = h.s.Execute(context.Background(), ref(r), nil)
	}
	if !h.crash.Fired() {
		t.Fatal("rotation crash point never reached")
	}
	h.restart()
	checkAfterCrash(t, h, r, crashCase{kind: kCreate, want: opstore.StateCompleted, wantEffects: 1})
}

// A crash between the freeze and the activation of a re-enrolment.
func TestCrashPoint_MidReenrolment(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	open := h.req("vm", b, kPowerOn, 1)
	h.mustAccept(open)
	next := opstore.Enrolment{ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	h.crash = &journaltest.Crasher{Point: opstore.PointMidReenrolment, N: 1}
	if err := h.s.Reenrol(context.Background(), next); !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("reenrol: %v", err)
	}
	h.restart()
	// No enrolment is active: every operation is refused.
	wantRefusal(t, h.admit(h.req("vm", b, kPowerOff, 1)), opstore.RefusalGrantMismatch)
	if err := h.s.Reenrol(context.Background(), next); err != nil {
		t.Fatalf("resume re-enrolment: %v", err)
	}
	v, _ := h.lookup(open)
	if v.State != opstore.StateRetired || v.Reason != opstore.ReasonProviderChanged {
		t.Fatalf("open op after re-enrolment: %v{%s}", v.State, v.Reason)
	}
	if h.exec.effects(open.OpID, "power") != 0 {
		t.Fatal("a retired operation ran")
	}
	h.verifyChain()
}

func witnessPathFor(h *harness, r opstore.Request, step int) string {
	return fmt.Sprintf("%s/witness/%x/%x.%d", h.dir, r.EnrolmentID, r.OpID, step)
}
