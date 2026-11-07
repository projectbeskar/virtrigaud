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
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Kinds used by the tests, with their test plans.
var (
	kCreate   = providerv1.OperationKind_OPERATION_KIND_CREATE
	kDelete   = providerv1.OperationKind_OPERATION_KIND_DELETE
	kPowerOn  = providerv1.OperationKind_OPERATION_KIND_POWER_ON
	kPowerOff = providerv1.OperationKind_OPERATION_KIND_POWER_OFF
	kReconfig = providerv1.OperationKind_OPERATION_KIND_RECONFIGURE
)

// testPlans: every class appears. Create: idempotent then once. Delete:
// irreversible then once+irreversible. PowerOn: once. PowerOff:
// irreversible. Reconfigure: idempotent.
var testPlans = map[providerv1.OperationKind][]executor.Step{
	kCreate:   {{Name: "prepare"}, {Name: "start", Once: true}},
	kDelete:   {{Name: "stop", Irreversible: true}, {Name: "remove", Once: true, Irreversible: true}},
	kPowerOn:  {{Name: "power", Once: true}},
	kPowerOff: {{Name: "off", Irreversible: true}},
	kReconfig: {{Name: "apply"}},
}

// testExec is the in-memory test executor. Its effects are the "world": they
// survive simulated crashes of the store.
type testExec struct {
	mu      sync.Mutex
	runs    map[string]int  // op/step -> successful Run calls
	present map[string]bool // effect exists (launch-identity view)
	fail    map[string]error
	probe   error
	block   chan struct{} // when set, Run waits on it (deadline tests)
	started chan struct{} // when set, Run sends on it before waiting on block
	payload map[string][]byte
}

func newTestExec() *testExec {
	return &testExec{runs: map[string]int{}, present: map[string]bool{}, fail: map[string]error{}, payload: map[string][]byte{}}
}

func effKey(opID []byte, step string) string { return fmt.Sprintf("%x/%s", opID, step) }

func (e *testExec) Plan(k providerv1.OperationKind) ([]executor.Step, error) {
	p, ok := testPlans[k]
	if !ok {
		return nil, fmt.Errorf("%w: %v", executor.ErrUnsupportedKind, k)
	}
	return p, nil
}

func (e *testExec) Run(ctx context.Context, op executor.Operation, st executor.Step) error {
	e.mu.Lock()
	block, started := e.block, e.started
	if err, ok := e.fail[st.Name]; ok {
		delete(e.fail, st.Name)
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	k := effKey(op.OpID, st.Name)
	e.runs[k]++
	e.present[k] = true
	if op.Payload != nil {
		e.payload[k] = op.Payload
	}
	return nil
}

func (e *testExec) Inspect(_ context.Context, op executor.Operation, st executor.Step) (executor.Evidence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.probe != nil {
		return executor.EvidenceUnknown, e.probe
	}
	if e.present[effKey(op.OpID, st.Name)] {
		return executor.EvidencePresent, nil
	}
	return executor.EvidenceAbsent, nil
}

func (e *testExec) Goal(_ context.Context, op executor.Operation, st executor.Step) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.probe != nil {
		return false, e.probe
	}
	return e.present[effKey(op.OpID, st.Name)], nil
}

// effects returns the number of successful runs of step of op.
func (e *testExec) effects(opID []byte, step string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[effKey(opID, step)]
}

// onceEffects is the total of runs of the once and irreversible steps of an
// operation: each must be at most 1.
func (e *testExec) onceEffects(opID []byte, kind providerv1.OperationKind) map[string]int {
	out := map[string]int{}
	for _, st := range testPlans[kind] {
		if st.Once || st.Irreversible {
			out[st.Name] = e.effects(opID, st.Name)
		}
	}
	return out
}

// harness is one store directory, reopened across simulated crashes.
type harness struct {
	t        testing.TB
	dir      string
	mem      *journaltest.MemFS
	exec     *testExec
	crash    *journaltest.Crasher
	s        *opstore.Store
	enrol    []byte
	provider string
	holder   []byte
	start    uint64
	opts     func(*opstore.Options)
	audit    []opstore.AuditEvent
	auditMu  sync.Mutex
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	h := &harness{t: t, dir: journaltest.FastTempDir(t), mem: journaltest.NewMemFS(), exec: newTestExec(),
		enrol: rid(), provider: "provider-uid-1", holder: rid(), start: 2}
	t.Cleanup(func() {
		if h.s != nil {
			_ = h.s.Close()
		}
	})
	return h
}

func rid() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return b
}

func (h *harness) options() opstore.Options {
	o := opstore.Options{
		Journal: journal.Options{Dir: h.dir, FS: h.mem, OpenTimeout: 200 * time.Millisecond,
			Hooks: journal.Hooks{Crash: func(p journal.Point) bool { return h.crash.At(p) }}},
		Executor: h.exec,
		Audit: func(ev opstore.AuditEvent) {
			h.auditMu.Lock()
			h.audit = append(h.audit, ev)
			h.auditMu.Unlock()
		},
	}
	if h.opts != nil {
		h.opts(&o)
	}
	return o
}

// open opens the store without establishing continuity.
func (h *harness) open() *opstore.Store {
	h.t.Helper()
	s, err := opstore.Open(context.Background(), h.options())
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	h.s = s
	return s
}

// boot opens, establishes continuity, recovers and runs every runnable
// operation to its end. It returns the runnable list.
func (h *harness) boot() []opstore.OpRef {
	h.t.Helper()
	s := h.open()
	if st, r := s.Journal().State(); st != journal.StateUnattested {
		h.t.Fatalf("journal state at open = %v{%s}", st, r)
	}
	if err := s.EstablishContinuity(context.Background()); err != nil {
		h.t.Fatalf("continuity: %v", err)
	}
	refs, err := s.Recover(context.Background())
	if err != nil {
		h.t.Fatalf("recover: %v", err)
	}
	for _, r := range refs {
		if _, err := s.Execute(context.Background(), r, nil); err != nil {
			h.t.Fatalf("resume %s: %v", r, err)
		}
	}
	return refs
}

// fresh boots a new store and enrols it.
func (h *harness) fresh() {
	h.t.Helper()
	h.boot()
	if err := h.s.Enrol(context.Background(), opstore.Enrolment{ID: h.enrol, ProviderUID: h.provider, InstallID: rid(), StartEpoch: h.start}); err != nil {
		h.t.Fatalf("enrol: %v", err)
	}
}

// restart simulates a crash or power cut and boots again.
func (h *harness) restart() []opstore.OpRef {
	h.t.Helper()
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	return h.boot()
}

// req returns a new request for vm.
func (h *harness) req(vm string, binding []byte, kind providerv1.OperationKind, attempt uint64) opstore.Request {
	d := sha256.Sum256(rid())
	return opstore.Request{ProviderUID: h.provider, EnrolmentID: h.enrol, OpID: rid(), VMUID: vm, BindingID: binding,
		Kind: kind, Attempt: attempt, IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: h.start, Holder: h.holder},
		Actor: "provider/test"}
}

func (h *harness) admit(r opstore.Request) opstore.Decision {
	h.t.Helper()
	d, err := h.s.Admit(context.Background(), r)
	if err != nil {
		h.t.Fatalf("admit: %v", err)
	}
	return d
}

func (h *harness) mustAccept(r opstore.Request) {
	h.t.Helper()
	if d := h.admit(r); d.Outcome != opstore.OutcomeAccepted {
		h.t.Fatalf("admit %v: outcome %v refusal %v, want accepted", r.Kind, d.Outcome, d.Refusal)
	}
}

func (h *harness) run(r opstore.Request) opstore.OpView {
	h.t.Helper()
	v, err := h.s.Execute(context.Background(), ref(r), nil)
	if err != nil {
		h.t.Fatalf("execute: %v", err)
	}
	return v
}

// do admits and runs r to its end and expects Completed.
func (h *harness) do(r opstore.Request) {
	h.t.Helper()
	h.mustAccept(r)
	if v := h.run(r); v.State != opstore.StateCompleted {
		h.t.Fatalf("%v ended %v{%s}", r.Kind, v.State, v.Reason)
	}
}

func (h *harness) lookup(r opstore.Request) (opstore.OpView, bool) {
	h.t.Helper()
	v, ok, err := h.s.Lookup(context.Background(), ref(r))
	if err != nil {
		h.t.Fatalf("lookup: %v", err)
	}
	return v, ok
}

func (h *harness) tombstoned(r opstore.Request) bool {
	h.t.Helper()
	ok, err := h.s.HasTombstone(context.Background(), ref(r))
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

func (h *harness) verifyChain() {
	h.t.Helper()
	if err := h.s.Journal().VerifyChain(context.Background()); err != nil {
		h.t.Fatalf("chain: %v", err)
	}
}

func ref(r opstore.Request) opstore.OpRef {
	return opstore.OpRef{EnrolmentID: r.EnrolmentID, OpID: r.OpID}
}

var errDefinite = fmt.Errorf("refused by the hypervisor: %w", executor.ErrNotPerformed)
