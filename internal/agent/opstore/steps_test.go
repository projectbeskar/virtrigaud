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
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestExecute_DefiniteFailureBeforeIrrevIsCleanAndFreesTheVM(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	r := h.req("vm", b, kCreate, 1)
	h.mustAccept(r)
	h.exec.fail["prepare"] = errDefinite
	if v := h.run(r); v.State != opstore.StateFailedClean {
		t.Fatalf("state %v", v.State)
	}
	if !h.tombstoned(r) {
		t.Fatal("no tombstone")
	}
	// No VM was made: a retry with a new attempt is admitted.
	h.do(h.req("vm", b, kCreate, 2))
}

func TestExecute_DefiniteFailureAfterOnceIntentIsDirtyAndHoldsTheVM(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	r := h.req("vm", b, kPowerOn, 1)
	h.mustAccept(r)
	h.exec.fail["power"] = errDefinite
	if v := h.run(r); v.State != opstore.StateFailedDirty {
		t.Fatalf("state %v", v.State)
	}
	wantRefusal(t, h.admit(h.req("vm", b, kPowerOff, 1)), opstore.RefusalVMBusy)
	if n, _ := h.s.Compact(context.Background(), 0); n != 1 { // only the Create
		t.Fatalf("compacted %d records; a dirty failure must be kept", n)
	}
	if v, _ := h.lookup(r); v.Tombstoned {
		t.Fatal("dirty failure record compacted")
	}
}

func TestExecute_StepDeadlineResolvedByInspection(t *testing.T) {
	cases := []struct {
		name   string
		kind   providerv1.OperationKind
		probe  error
		want   opstore.OpState
		reason string
	}{
		// Idempotent step, no effect found: a definite failure, clean.
		{name: "idempotent absent", kind: kReconfig, want: opstore.StateFailedClean, reason: opstore.ReasonStepDeadline},
		// Once step, no effect found: it may still appear; unknown.
		{name: "once absent", kind: kPowerOn, want: opstore.StateOutcomeUnknown, reason: opstore.ReasonStepDeadline},
		// Irreversible step, goal not reached: a definite failure, dirty.
		{name: "irreversible goal not reached", kind: kPowerOff, want: opstore.StateFailedDirty, reason: opstore.ReasonStepDeadline},
		// The probe fails: unknown, never a guess.
		{name: "probe error", kind: kReconfig, probe: errors.New("probe failed"), want: opstore.StateOutcomeUnknown, reason: opstore.ReasonProbeError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts = func(o *opstore.Options) {
				o.StepDeadlines = map[providerv1.OperationKind]time.Duration{c.kind: 20 * time.Millisecond}
			}
			h.fresh()
			b := rid()
			h.do(h.req("vm", b, kCreate, 1))
			r := h.req("vm", b, c.kind, 1)
			h.mustAccept(r)
			h.exec.block = make(chan struct{})
			h.exec.probe = c.probe
			v := h.run(r)
			if v.State != c.want || v.Reason != c.reason {
				t.Fatalf("state %v{%s}, want %v{%s}", v.State, v.Reason, c.want, c.reason)
			}
		})
	}
}

// An effect found by inspection after a failed run is done.
func TestExecute_IndefiniteFailureWithEffectPresentIsDone(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	r := h.req("vm", b, kReconfig, 1)
	h.mustAccept(r)
	h.exec.present[effKey(r.OpID, "apply")] = true
	h.exec.fail["apply"] = errors.New("connection reset")
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("state %v{%s}", v.State, v.Reason)
	}
}

// Cancelling the runner leaves the step to recovery; it never resolves it.
func TestExecute_CancelledRunnerLeavesStepToRecovery(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	r := h.req("vm", b, kPowerOn, 1)
	h.mustAccept(r)
	h.exec.block = make(chan struct{})
	h.exec.started = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.s.Execute(ctx, ref(r), nil)
		done <- err
	}()
	select {
	case <-h.exec.started:
	case err := <-done:
		t.Fatalf("runner ended early: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := h.lookup(r); v.State != opstore.StateExecuting {
		t.Fatalf("state %v: a cancelled step was resolved", v.State)
	}
	h.exec.block = nil
	h.restart()
	v, _ := h.lookup(r)
	if v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonWitnessNoEffect {
		t.Fatalf("after restart: %v{%s}", v.State, v.Reason)
	}
	if h.exec.effects(r.OpID, "power") != 0 {
		t.Fatal("the once step ran after its witness was durable")
	}
}

func TestExecute_OneRunnerPerOperation(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.exec.block = make(chan struct{})
	h.exec.started = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		_, _ = h.s.Execute(context.Background(), ref(r), nil)
		close(done)
	}()
	<-h.exec.started
	_, err := h.s.Execute(context.Background(), ref(r), nil)
	close(h.exec.block)
	<-done
	if !errors.Is(err, opstore.ErrRunning) {
		t.Fatalf("second runner: %v", err)
	}
}

func TestExecute_WitnessFsyncErrorQuarantines(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.mem.Fail(journaltest.OpSync, journaltest.Contains("/witness/"), 0)
	if _, err := h.s.Execute(context.Background(), ref(r), nil); !errors.Is(err, journal.ErrLost) {
		t.Fatalf("err = %v", err)
	}
	if h.exec.effects(r.OpID, "start") != 0 {
		t.Fatal("effect ran without a durable witness")
	}
	_ = h.s.Close()
	h.mem.Crash()
	s := h.open()
	if st, reason := s.Journal().State(); st != journal.StateLost || reason != journal.LostIOError {
		t.Fatalf("after reopen: %v{%s}", st, reason)
	}
	wantRefusal(t, h.admit(h.req("vm2", rid(), kCreate, 1)), opstore.RefusalQuarantined)
}

// The witness is durable before the effect: a power cut right after the
// effect still finds it.
func TestExecute_WitnessDurableBeforeEffect(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterEffect, N: 2}
	if _, err := h.s.Execute(context.Background(), ref(r), nil); !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("err = %v", err)
	}
	_ = h.s.Close()
	h.mem.Crash()
	if ok, _ := h.mem.Exists(witnessPathFor(h, r, 1)); !ok {
		t.Fatal("witness not durable after a power cut")
	}
}

func TestExecute_UnknownKindRefusedBeforeRecording(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), providerv1.OperationKind_OPERATION_KIND_CLONE, 1)
	if _, err := h.s.Admit(context.Background(), r); !errors.Is(err, executor.ErrUnsupportedKind) {
		t.Fatalf("err = %v", err)
	}
}

func TestAdmit_InvalidRequest(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	r.OpID = r.OpID[:3]
	if _, err := h.s.Admit(context.Background(), r); !errors.Is(err, opstore.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}
