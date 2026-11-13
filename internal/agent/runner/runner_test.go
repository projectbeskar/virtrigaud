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

package runner_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// stubStore fails Execute a set number of times.
type stubStore struct {
	mu         sync.Mutex
	fails      int
	calls      int
	resolved   string
	lost       bool
	panics     bool
	block      chan struct{}
	panicCount int
}

func (s *stubStore) Execute(ctx context.Context, _ opstore.OpRef, _ []byte) (opstore.OpView, error) {
	s.mu.Lock()
	s.calls++
	fail, panics, block := s.calls <= s.fails, s.panics, s.block
	s.mu.Unlock()
	if panics {
		panic("executor bug")
	}
	if block != nil {
		<-block
	}
	if fail {
		return opstore.OpView{}, errors.New("store hiccup")
	}
	return opstore.OpView{State: opstore.StateCompleted}, nil
}

func (s *stubStore) ResolveUnknown(_ context.Context, _ opstore.OpRef, reason string) (opstore.OpView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = reason
	return opstore.OpView{State: opstore.StateOutcomeUnknown, Reason: reason}, nil
}

func (s *stubStore) RecordPanic(context.Context, opstore.OpRef) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panicCount++
	return s.panicCount, nil
}

func (s *stubStore) Panics(context.Context, opstore.OpRef) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panicCount, nil
}

func (s *stubStore) Lost() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

func (s *stubStore) snapshot() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.resolved
}

var fast = runner.Options{Resumes: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, GiveUpAfter: time.Millisecond}

func drain(t *testing.T, p *runner.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !p.Drain(ctx) {
		t.Fatal("the pool did not drain")
	}
}

// TestRunResumesThenSucceeds: a run that fails on store errors is resumed
// and ends when one resumption succeeds.
func TestRunResumesThenSucceeds(t *testing.T) {
	s := &stubStore{fails: 2}
	p := runner.New(context.Background(), s, logr.Discard(), fast)
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if calls, resolved := s.snapshot(); calls != 3 || resolved != "" {
		t.Fatalf("calls %d, resolved %q", calls, resolved)
	}
}

// TestRunGivesUpAsOutcomeUnknown: after the last failed resumption the
// operation's outcome is recorded as unknown, never as a definite outcome.
func TestRunGivesUpAsOutcomeUnknown(t *testing.T) {
	s := &stubStore{fails: 100}
	p := runner.New(context.Background(), s, logr.Discard(), fast)
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if calls, resolved := s.snapshot(); calls != 4 || resolved != opstore.ReasonRunFailed {
		t.Fatalf("calls %d, resolved %q", calls, resolved)
	}
}

// TestRunStopsOnQuarantine: a quarantined store is not resumed.
func TestRunStopsOnQuarantine(t *testing.T) {
	s := &stubStore{fails: 100, lost: true}
	p := runner.New(context.Background(), s, logr.Discard(), fast)
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if calls, resolved := s.snapshot(); calls != 1 || resolved != "" {
		t.Fatalf("calls %d, resolved %q", calls, resolved)
	}
}

// TestPanicIsRecoveredAndReported: a panicking run is recovered and reported
// once; the pool keeps working.
func TestPanicIsRecoveredAndReported(t *testing.T) {
	var reported atomic.Int32
	s := &stubStore{panics: true}
	p := runner.New(context.Background(), s, logr.Discard(), runner.Options{OnPanic: func(any) { reported.Add(1) }})
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if reported.Load() != 1 {
		t.Fatalf("panics reported %d", reported.Load())
	}
	if _, n := s.snapshot(); n != "" {
		t.Fatalf("resolved %q after one panic", n)
	}
	// Two more panics reach the limit; the next start gives the operation
	// up without running it.
	for range 2 {
		q := p2(t, s)
		if err := q.Start(opstore.OpRef{}, nil); err != nil {
			t.Fatal(err)
		}
		drain(t, q)
	}
	calls, _ := s.snapshot()
	q := p2(t, s)
	if err := q.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, q)
	after, resolved := s.snapshot()
	if resolved != opstore.ReasonRunPanicked || after != calls {
		t.Fatalf("after %d panics: resolved %q, runs %d then %d", s.panicCount, resolved, calls, after)
	}
}

// p2 is a new pool over s.
func p2(_ *testing.T, s *stubStore) *runner.Pool {
	p := runner.New(context.Background(), s, logr.Discard(), runner.Options{})
	return p
}

// TestDrainRefusesStartsAndIsBounded: once draining the pool refuses runs,
// and Drain gives up at its context's end on a run that does not stop.
func TestDrainRefusesStartsAndIsBounded(t *testing.T) {
	s := &stubStore{block: make(chan struct{})}
	p := runner.New(context.Background(), s, logr.Discard(), fast)
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if p.Drain(ctx) {
		t.Fatal("drained while a run was blocked")
	}
	if err := p.Start(opstore.OpRef{}, nil); !errors.Is(err, runner.ErrDraining) {
		t.Fatalf("start while draining: %v", err)
	}
	close(s.block)
	drain(t, p)
}

// failingPlan is an executor whose Plan works for admission and then fails,
// an error of the store's run that neither quarantines nor stops it.
type failingPlan struct{ calls atomic.Int32 }

func (f *failingPlan) Plan(providerv1.OperationKind) ([]executor.Step, error) {
	if f.calls.Add(1) == 1 {
		return []executor.Step{{Name: "power"}}, nil
	}
	return nil, errors.New("the executor's plan is unavailable")
}
func (f *failingPlan) Run(context.Context, executor.Operation, executor.Step) error { return nil }
func (f *failingPlan) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidenceUnknown, nil
}
func (f *failingPlan) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return false, nil
}

func rid() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// TestRealStoreRunThatKeepsFailingEndsUnknown: with the real store, an
// operation whose run fails on every resumption is recorded OUTCOME_UNKNOWN
// (run_failed), so it is never left running without a runner.
func TestRealStoreRunThatKeepsFailingEndsUnknown(t *testing.T) {
	ctx := context.Background()
	exec := &failingPlan{}
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: journaltest.FastTempDir(t), FS: journaltest.NewMemFS()}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	enrol := rid()
	if err := st.Enrol(ctx, opstore.Enrolment{ID: enrol, ProviderUID: "p", HostID: "h", InstallID: rid(), StartEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	b := rid()
	create := func(kind providerv1.OperationKind) opstore.Request {
		d := sha256.Sum256(rid())
		return opstore.Request{ProviderUID: "p", EnrolmentID: enrol, OpID: rid(), VMUID: "vm", BindingID: b, Kind: kind,
			Attempt: 1, IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: 1, Holder: rid()}}
	}
	r := create(providerv1.OperationKind_OPERATION_KIND_CREATE)
	if d, err := st.Admit(ctx, r); err != nil || d.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit: %+v %v", d, err)
	}
	ref := opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}
	p := runner.New(ctx, st, logr.Discard(), fast)
	if err := p.Start(ref, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	v, ok, err := st.Lookup(ctx, ref)
	if err != nil || !ok || v.State != opstore.StateOutcomeUnknown || v.Reason != opstore.ReasonRunFailed {
		t.Fatalf("after the runs: %+v %v %v", v, ok, err)
	}
	if st.Lost() {
		t.Fatal("the store was quarantined")
	}
}

// TestRealStoreWitnessFailureQuarantinesAndStops: a witness that cannot be
// written quarantines the store; the run stops at once, is not resumed and
// records nothing more.
func TestRealStoreWitnessFailureQuarantinesAndStops(t *testing.T) {
	ctx := context.Background()
	mem := journaltest.NewMemFS()
	exec := &onceExec{}
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: journaltest.FastTempDir(t), FS: mem}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	enrol := rid()
	if err := st.Enrol(ctx, opstore.Enrolment{ID: enrol, ProviderUID: "p", HostID: "h", InstallID: rid(), StartEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(rid())
	r := opstore.Request{ProviderUID: "p", EnrolmentID: enrol, OpID: rid(), VMUID: "vm", BindingID: rid(),
		Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1, IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: 1, Holder: rid()}}
	if dec, err := st.Admit(ctx, r); err != nil || dec.Outcome != opstore.OutcomeAccepted {
		t.Fatalf("admit: %+v %v", dec, err)
	}
	mem.Fail(journaltest.OpCreate, func(path string) bool { return containsWitness(path) }, 0)
	p := runner.New(ctx, st, logr.Discard(), fast)
	if err := p.Start(opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if !st.Lost() {
		t.Fatal("a witness write failure must quarantine the store")
	}
	if exec.runs.Load() != 0 {
		t.Fatal("the step ran without its witness")
	}
}

// onceExec plans a once step and counts its runs.
type onceExec struct{ runs atomic.Int32 }

func (o *onceExec) Plan(providerv1.OperationKind) ([]executor.Step, error) {
	return []executor.Step{{Name: "define", Once: true}}, nil
}
func (o *onceExec) Run(context.Context, executor.Operation, executor.Step) error {
	o.runs.Add(1)
	return nil
}
func (o *onceExec) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidenceUnknown, nil
}
func (o *onceExec) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return false, nil
}

// containsWitness reports whether path is under a witness directory.
func containsWitness(path string) bool { return strings.Contains(path, "/witness/") }

// TestGiveUpWaitsForTheWindow: a run keeps being resumed until the give-up
// window has passed, however many resumptions failed before.
func TestGiveUpWaitsForTheWindow(t *testing.T) {
	s := &stubStore{fails: 1000}
	window := 200 * time.Millisecond
	p := runner.New(context.Background(), s, logr.Discard(), runner.Options{Resumes: 1, InitialBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond, GiveUpAfter: window})
	start := time.Now()
	if err := p.Start(opstore.OpRef{}, nil); err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	calls, resolved := s.snapshot()
	if resolved != opstore.ReasonRunFailed || time.Since(start) < window || calls < 10 {
		t.Fatalf("gave up after %s and %d calls (%q), want at least %s", time.Since(start), calls, resolved, window)
	}
}
