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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The reference-model test drives the store and a small, independent model of
// the operation state machine with the same randomized schedule (deliveries,
// duplicates, mutated re-deliveries, reordered attempts, executions, scripted
// failures, crashes at every crash point, compaction, re-enrolment and freeze
// confirmation), and compares them after every step. It also checks the
// invariants directly on the executor's ground truth:
//
//   - no once or irreversible step runs more than once per operation;
//   - a retired operation never had a step intent nor an effect;
//   - a terminal operation never changes state again;
//   - at most one operation holds a VM;
//   - the irrev flag survives compaction and re-enrolment.
//
// Runs: VIRTRIGAUD_MODEL_RUNS (default 100000; 10000 with -short).
// One seed: VIRTRIGAUD_MODEL_SEED. Long run: make test-agent-model-long.

const (
	modelRunsEnv     = "VIRTRIGAUD_MODEL_RUNS"
	modelSeedEnv     = "VIRTRIGAUD_MODEL_SEED"
	modelDefaultRuns = 100000
	modelShortRuns   = 10000
	modelEvents      = 14
)

// modelStats counts what the runs exercised, to show the schedule reaches
// every path; it is logged at the end of the test.
var modelStats struct {
	sync.Mutex
	n map[string]int
}

func stat(name string) {
	modelStats.Lock()
	if modelStats.n == nil {
		modelStats.n = map[string]int{}
	}
	modelStats.n[name]++
	modelStats.Unlock()
}

var modelVMs = []string{"vm-a", "vm-b"}

var modelKinds = []providerv1.OperationKind{kCreate, kPowerOn, kPowerOff, kReconfig, kDelete}

var modelPoints = []journal.Point{
	opstore.PointBeforeAccept, journal.PointAfterStoreCommit, journal.PointAfterAnchor, opstore.PointAfterIntent,
	opstore.PointAfterWitness, opstore.PointAfterEffect, opstore.PointAfterLastDone, opstore.PointAfterTerminal,
	opstore.PointMidReenrolment,
}

func TestReferenceModel(t *testing.T) {
	runs := modelDefaultRuns
	if testing.Short() {
		runs = modelShortRuns
	}
	if v, err := strconv.Atoi(os.Getenv(modelRunsEnv)); err == nil && v > 0 {
		runs = v
	}
	if v, err := strconv.ParseUint(os.Getenv(modelSeedEnv), 10, 64); err == nil {
		if err := runModel(v); err != nil {
			t.Fatalf("seed %d: %v", v, err)
		}
		return
	}
	// The runs allocate heavily and briefly; a higher GC target halves the
	// wall time without changing what is tested.
	defer debug.SetGCPercent(debug.SetGCPercent(800))
	workers := runtime.GOMAXPROCS(0)
	if workers > runs {
		workers = runs
	}
	start := time.Now()
	for w := 0; w < workers; w++ {
		t.Run(fmt.Sprintf("worker-%d", w), func(t *testing.T) {
			t.Parallel()
			for seed := uint64(w); seed < uint64(runs); seed += uint64(workers) {
				if err := runModel(seed); err != nil {
					t.Fatalf("seed %d (reproduce with %s=%d): %v", seed, modelSeedEnv, seed, err)
				}
			}
		})
	}
	t.Cleanup(func() {
		modelStats.Lock()
		defer modelStats.Unlock()
		t.Logf("%d seeded runs in %v; exercised: %v", runs, time.Since(start), modelStats.n)
	})
}

// modelFailure stops a run.
type modelFailure struct{ msg string }

func (f *modelFailure) Error() string { return f.msg }

// tb adapts the harness's testing.TB use inside a model run: Fatal stops the
// run with an error.
type tb struct {
	testing.TB
}

func (t *tb) Helper() {}
func (t *tb) Fatalf(format string, args ...any) {
	panic(&modelFailure{msg: fmt.Sprintf(format, args...)})
}
func (t *tb) Fatal(args ...any)            { panic(&modelFailure{msg: fmt.Sprint(args...)}) }
func (t *tb) Cleanup(func())               {}
func (t *tb) Errorf(f string, args ...any) { t.Fatalf(f, args...) }

type mStep struct{ once, irr, intent, done, witness bool }

type mOp struct {
	r        opstore.Request
	recorded bool
	state    opstore.OpState
	reason   string
	irrev    bool
	steps    []mStep
	compact  bool
	runs     map[int]int
	present  map[int]bool
	gone     bool // discarded by a confirmed freeze
}

type mVM struct {
	binding  []byte
	busy     string
	present  bool
	creating bool
}

type model struct {
	h         *harness
	rnd       *rand.Rand
	active    []byte
	start     uint64
	lastStart uint64
	frozen    [][]byte
	ops       map[string]*mOp
	order     []string
	vms       map[string]*mVM
	vmTomb    map[string]bool
	attempts  map[string]uint64
	bindings  map[string][]byte
	crash     *journaltest.Crasher // the model's own counter, armed like the store's
	crashed   bool
	ids       uint64
}

func (m *model) fail(format string, args ...any) {
	panic(&modelFailure{msg: fmt.Sprintf(format, args...)})
}

func (m *model) newID() []byte {
	m.ids++
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, m.rnd.Uint64())
	binary.BigEndian.PutUint64(b[8:], m.ids)
	return b
}

func runModel(seed uint64) (err error) {
	dir, derr := os.MkdirTemp(fastDir(), "vrtg-model-")
	if derr != nil {
		return derr
	}
	defer func() { _ = os.RemoveAll(dir) }()
	t := &tb{}
	h := &harness{t: t, dir: dir, mem: journaltest.NewMemFS(), exec: newTestExec(), provider: "provider-model"}
	m := &model{h: h, rnd: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), ops: map[string]*mOp{},
		vms: map[string]*mVM{}, vmTomb: map[string]bool{}, attempts: map[string]uint64{}, bindings: map[string][]byte{}}
	h.enrol, h.holder, h.start = m.newID(), m.newID(), 2
	defer func() {
		if h.s != nil {
			_ = h.s.Close()
		}
		if r := recover(); r != nil {
			if f, ok := r.(*modelFailure); ok {
				err = f
				return
			}
			panic(r)
		}
	}()
	h.fresh()
	m.active, m.start, m.lastStart = h.enrol, h.start, h.start
	for i := 0; i < modelEvents; i++ {
		m.step()
		m.check()
	}
	h.verifyChain()
	return nil
}

func fastDir() string {
	if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
		return "/dev/shm"
	}
	return os.TempDir()
}

// arm arms a crash, for the store and the model alike, with probability p.
func (m *model) arm(p float64) {
	m.h.crash, m.crash, m.crashed = nil, nil, false
	if m.rnd.Float64() >= p {
		return
	}
	pt := modelPoints[m.rnd.IntN(len(modelPoints))]
	n := 1 + m.rnd.IntN(3)
	m.h.crash = &journaltest.Crasher{Point: pt, N: n}
	m.crash = &journaltest.Crasher{Point: pt, N: n}
}

// point is a crash point in the model's own walk of an action.
func (m *model) point(p journal.Point) bool {
	if m.crashed {
		return true
	}
	if m.crash.At(p) {
		m.crashed = true
	}
	return m.crashed
}

// commit applies a durable change and walks the commit's two journal points.
func (m *model) commit(apply func()) bool {
	if m.crashed {
		return true
	}
	apply()
	if m.point(journal.PointAfterStoreCommit) {
		return true
	}
	return m.point(journal.PointAfterAnchor)
}

// settle restarts both after a crash, or disarms.
func (m *model) settle() {
	storeCrashed, modelCrashed := m.h.crash.Fired(), m.crashed
	if storeCrashed != modelCrashed {
		m.fail("crash at %v #%d: store crashed %v, model %v", m.h.crash.Point, m.h.crash.N, storeCrashed, modelCrashed)
	}
	m.h.exec.fail = map[string]error{}
	if storeCrashed {
		stat("crash:" + string(m.h.crash.Point))
	}
	m.h.crash, m.crash = nil, nil
	if !storeCrashed {
		return
	}
	m.crashed = false
	m.h.restart()
	m.recoverAll()
}

func (m *model) step() {
	switch x := m.rnd.IntN(100); {
	case x < 45:
		m.deliver()
	case x < 85:
		if !m.execute() {
			m.deliver()
		}
	case x < 92:
		m.compact()
	case x < 97:
		m.reenrol()
	default:
		m.confirm()
	}
}

func key(enrol, op []byte) string { return string(enrol) + string(op) }

func (m *model) tombKey(enrol []byte, vm string, binding []byte) string {
	return string(enrol) + "|" + vm + "|" + string(binding)
}

func (m *model) attemptKey(enrol []byte, vm string, k providerv1.OperationKind) string {
	return fmt.Sprintf("%x|%s|%d", enrol, vm, k)
}

// request generates a delivery: a new operation or a re-delivery.
func (m *model) request() opstore.Request {
	if len(m.order) > 0 && m.rnd.IntN(100) < 40 {
		op := m.ops[m.order[m.rnd.IntN(len(m.order))]]
		r := op.r
		if m.rnd.IntN(100) < 15 {
			r.IntentDigest = append([]byte{^r.IntentDigest[0]}, r.IntentDigest[1:]...)
		}
		return r
	}
	vm := modelVMs[m.rnd.IntN(len(modelVMs))]
	rec := m.vms[vm]
	kind := kCreate
	if rec != nil && m.rnd.IntN(100) >= 10 || rec == nil && m.rnd.IntN(100) >= 70 {
		kind = modelKinds[1+m.rnd.IntN(len(modelKinds)-1)]
	}
	binding := m.bindings[vm]
	switch {
	case binding == nil || m.rnd.IntN(100) < 20:
		binding = m.newID()
	case rec != nil && m.rnd.IntN(100) < 80:
		binding = rec.binding
	}
	m.bindings[vm] = binding
	enrol := m.h.enrol
	maxA := m.attempts[m.attemptKey(enrol, vm, kind)]
	attempt := maxA + 1
	switch x := m.rnd.IntN(100); {
	case x < 15 && maxA > 0:
		attempt = 1 + m.rnd.Uint64N(maxA)
	case x < 25:
		attempt = maxA + 2
	}
	mint := m.h.start
	if m.rnd.IntN(100) < 8 {
		mint = m.h.start - 1
	}
	d := sha256.Sum256(m.newID())
	r := opstore.Request{ProviderUID: m.h.provider, EnrolmentID: enrol, OpID: m.newID(), VMUID: vm, BindingID: binding,
		Kind: kind, Attempt: attempt, IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: mint, Holder: m.h.holder}}
	return r
}

func sameRequest(a, b opstore.Request) bool {
	return a.ProviderUID == b.ProviderUID && bytes.Equal(a.EnrolmentID, b.EnrolmentID) && bytes.Equal(a.OpID, b.OpID) &&
		a.VMUID == b.VMUID && bytes.Equal(a.BindingID, b.BindingID) && a.Kind == b.Kind && a.Attempt == b.Attempt &&
		bytes.Equal(a.IntentDigest, b.IntentDigest) && a.MintEpoch.Counter == b.MintEpoch.Counter &&
		bytes.Equal(a.MintEpoch.Holder, b.MintEpoch.Holder)
}

// expect is the model's admission table.
func (m *model) expect(r opstore.Request) (opstore.Outcome, opstore.Refusal, *mOp) {
	if m.active == nil || !bytes.Equal(m.active, r.EnrolmentID) {
		return opstore.OutcomeRefused, opstore.RefusalGrantMismatch, nil
	}
	if op := m.ops[key(r.EnrolmentID, r.OpID)]; op != nil && op.recorded && !op.gone {
		if !sameRequest(op.r, r) {
			return opstore.OutcomeRefused, opstore.RefusalOpIDConflict, nil
		}
		return opstore.OutcomeKnown, opstore.RefusalNone, op
	}
	if r.MintEpoch.Counter < m.start {
		return opstore.OutcomeRefused, opstore.RefusalGrantMismatch, nil
	}
	vm := m.vms[r.VMUID]
	if vm != nil && vm.busy != "" {
		return opstore.OutcomeRefused, opstore.RefusalVMBusy, nil
	}
	if r.Kind == kCreate {
		if vm != nil && !bytes.Equal(vm.binding, r.BindingID) || m.vmTomb[m.tombKey(r.EnrolmentID, r.VMUID, r.BindingID)] {
			return opstore.OutcomeRefused, opstore.RefusalVMBindingStale, nil
		}
	} else {
		if vm == nil {
			return opstore.OutcomeRefused, opstore.RefusalVMNotOwned, nil
		}
		if !bytes.Equal(vm.binding, r.BindingID) {
			return opstore.OutcomeRefused, opstore.RefusalVMBindingStale, nil
		}
	}
	if r.Attempt <= m.attempts[m.attemptKey(r.EnrolmentID, r.VMUID, r.Kind)] {
		return opstore.OutcomeRefused, opstore.RefusalOpIDConflict, nil
	}
	return opstore.OutcomeAccepted, opstore.RefusalNone, nil
}

func (m *model) track(r opstore.Request) *mOp {
	k := key(r.EnrolmentID, r.OpID)
	op := m.ops[k]
	if op == nil {
		op = &mOp{r: r, runs: map[int]int{}, present: map[int]bool{}}
		m.ops[k] = op
		m.order = append(m.order, k)
	}
	return op
}

func (m *model) deliver() {
	r := m.request()
	outcome, refusal, known := m.expect(r)
	m.arm(0.3)
	d, err := m.h.s.Admit(context.Background(), r)
	// The model's walk of the admission.
	if outcome == opstore.OutcomeAccepted && !m.point(opstore.PointBeforeAccept) {
		m.commit(func() { m.accept(r) })
	} else if outcome == opstore.OutcomeAccepted {
		_ = m.track(r) // crashed before recording: tracked as unrecorded
	}
	if m.crashed {
		if err == nil {
			m.fail("admission: model crashed, store answered %+v", d)
		}
		m.settle()
		return
	}
	if err != nil {
		m.fail("admit: %v", err)
	}
	m.settle()
	if d.Outcome != outcome || d.Refusal != refusal {
		m.fail("admit %v attempt %d: store %v/%v, model %v/%v", r.Kind, r.Attempt, d.Outcome, d.Refusal, outcome, refusal)
	}
	if outcome == opstore.OutcomeKnown && (d.Op.State != known.state || d.Op.Reason != known.reason) {
		m.fail("known op: store %v{%s}, model %v{%s}", d.Op.State, d.Op.Reason, known.state, known.reason)
	}
	switch outcome {
	case opstore.OutcomeRefused:
		m.track(r)
		stat("refused:" + refusal.String())
	case opstore.OutcomeKnown:
		stat("known:" + known.state.String())
	default:
		stat("accepted")
	}
}

// accept is the accept transaction.
func (m *model) accept(r opstore.Request) {
	op := m.track(r)
	op.r, op.recorded, op.state, op.reason = r, true, opstore.StateAccepted, ""
	op.steps = nil
	for _, st := range testPlans[r.Kind] {
		op.steps = append(op.steps, mStep{once: st.Once, irr: st.Irreversible})
	}
	vm := m.vms[r.VMUID]
	if vm == nil {
		vm = &mVM{}
		m.vms[r.VMUID] = vm
	}
	vm.binding, vm.busy = r.BindingID, key(r.EnrolmentID, r.OpID)
	if r.Kind == kCreate && !vm.present {
		vm.creating = true
	}
	m.attempts[m.attemptKey(r.EnrolmentID, r.VMUID, r.Kind)] = r.Attempt
}

func terminal(s opstore.OpState) bool { return opstore.Terminal(s) }

func holds(s opstore.OpState) bool { return !terminal(s) || s == opstore.StateFailedDirty }

// finish is the terminal transaction's writes.
func (m *model) finish(op *mOp, state opstore.OpState, reason string) {
	op.state, op.reason = state, reason
	k := key(op.r.EnrolmentID, op.r.OpID)
	vm := m.vms[op.r.VMUID]
	if vm == nil || vm.busy != k {
		return
	}
	if !holds(state) {
		vm.busy = ""
	}
	switch {
	case op.r.Kind == kDelete && state == opstore.StateCompleted:
		delete(m.vms, op.r.VMUID)
		m.vmTomb[m.tombKey(op.r.EnrolmentID, op.r.VMUID, op.r.BindingID)] = true
	case op.r.Kind == kCreate && state == opstore.StateCompleted:
		vm.present, vm.creating = true, false
	case op.r.Kind == kCreate && vm.creating && (state == opstore.StateFailedClean || state == opstore.StateRetired):
		delete(m.vms, op.r.VMUID)
	}
}

func (m *model) openOps() []*mOp {
	var out []*mOp
	for _, k := range m.order {
		op := m.ops[k]
		if op.recorded && !op.gone && (op.state == opstore.StateAccepted || op.state == opstore.StateExecuting) {
			out = append(out, op)
		}
	}
	return out
}

func (m *model) execute() bool {
	open := m.openOps()
	if len(open) == 0 {
		return false
	}
	op := open[m.rnd.IntN(len(open))]
	failAt := -1
	if m.rnd.IntN(100) < 10 {
		failAt = m.rnd.IntN(len(op.steps))
		m.h.exec.fail[testPlans[op.r.Kind][failAt].Name] = errDefinite
	}
	m.arm(0.35)
	_, err := m.h.s.Execute(context.Background(), ref(op.r), nil)
	m.run(op, failAt)
	if m.crashed != (err != nil && errors.Is(err, journal.ErrCrashed)) {
		m.fail("execute: model crashed %v, store err %v", m.crashed, err)
	}
	if err != nil && !m.crashed {
		m.fail("execute: %v", err)
	}
	m.settle()
	return true
}

// run is the model's walk of Execute.
func (m *model) run(op *mOp, failAt int) {
	if op.state != opstore.StateAccepted && op.state != opstore.StateExecuting {
		return
	}
	for i := range op.steps {
		st := &op.steps[i]
		if st.done {
			continue
		}
		if st.intent && st.irr && !st.once {
			if op.present[i] {
				m.commit(func() { st.done = true })
			} else {
				m.commit(func() { m.finish(op, failState(op), opstore.ReasonGoalNotObserved) })
				return
			}
			continue
		}
		if !st.intent {
			if m.commit(func() {
				st.intent = true
				if st.once || st.irr {
					op.irrev = true
				}
				op.state = opstore.StateExecuting
			}) {
				return
			}
			if m.point(opstore.PointAfterIntent) {
				return
			}
		}
		if st.once {
			if st.witness {
				if op.present[i] {
					m.commit(func() { st.done = true })
					continue
				}
				m.commit(func() { op.state, op.reason = opstore.StateOutcomeUnknown, opstore.ReasonWitnessNoEffect })
				return
			}
			st.witness = true
			if m.point(opstore.PointAfterWitness) {
				return
			}
		}
		ok := i != failAt
		if ok {
			op.runs[i]++
			op.present[i] = true
		}
		if m.point(opstore.PointAfterEffect) {
			return
		}
		if !ok {
			m.commit(func() { m.finish(op, failState(op), opstore.ReasonNotPerformed) })
			return
		}
		if m.commit(func() { st.done = true }) {
			return
		}
	}
	if m.point(opstore.PointAfterLastDone) {
		return
	}
	if m.commit(func() { m.finish(op, opstore.StateCompleted, "") }) {
		return
	}
	m.point(opstore.PointAfterTerminal)
}

func failState(op *mOp) opstore.OpState {
	if op.irrev {
		return opstore.StateFailedDirty
	}
	return opstore.StateFailedClean
}

// recoverAll is the model's recovery, then the runners for the active
// enrolment (the harness's boot runs them).
func (m *model) recoverAll() {
	var runnable []*mOp
	for _, op := range m.openOps() {
		still := true
	steps:
		for i := range op.steps {
			st := &op.steps[i]
			switch {
			case st.done, !st.intent:
				continue
			case st.once && !st.witness:
				continue // run once
			case st.once && op.present[i]:
				st.done = true
			case st.once:
				op.state, op.reason, still = opstore.StateOutcomeUnknown, opstore.ReasonWitnessNoEffect, false
				break steps
			case st.irr && op.present[i]:
				st.done = true
			case st.irr:
				op.state, op.reason, still = opstore.StateOutcomeUnknown, opstore.ReasonGoalNotObserved, false
				break steps
			}
		}
		if still && bytes.Equal(op.r.EnrolmentID, m.active) {
			runnable = append(runnable, op)
		}
	}
	for _, op := range runnable {
		m.run(op, -1)
	}
}

func (m *model) compact() {
	any := false
	for _, op := range m.ops {
		if op.recorded && !op.gone && !op.compact && terminal(op.state) && !holds(op.state) {
			any = true
		}
	}
	m.arm(0.3)
	_, err := m.h.s.Compact(context.Background(), 1<<20)
	if any {
		m.commit(func() {
			for _, op := range m.ops {
				if op.recorded && !op.gone && terminal(op.state) && !holds(op.state) {
					op.compact = true
				}
			}
		})
	}
	if err != nil && !m.crashed {
		m.fail("compact: %v", err)
	}
	m.settle()
}

func (m *model) reenrol() {
	next := opstore.Enrolment{ID: m.newID(), ProviderUID: m.h.provider, StartEpoch: m.lastStart + 1}
	m.arm(0.3)
	err := m.h.s.Reenrol(context.Background(), next)
	prev := m.active
	crashed := false
	if prev != nil {
		crashed = m.commit(func() { m.freeze(prev) }) || m.point(opstore.PointMidReenrolment)
	}
	if !crashed {
		m.commit(func() {
			m.active, m.start, m.lastStart = next.ID, next.StartEpoch, next.StartEpoch
			m.h.enrol, m.h.start = next.ID, next.StartEpoch
		})
	}
	if err != nil && !m.crashed {
		m.fail("re-enrol: %v", err)
	}
	m.settle()
}

func (m *model) freeze(prev []byte) {
	for _, op := range m.openOps() {
		if !bytes.Equal(op.r.EnrolmentID, prev) {
			continue
		}
		intent := false
		for _, st := range op.steps {
			intent = intent || st.intent
		}
		if intent {
			op.state, op.reason = opstore.StateOutcomeUnknown, opstore.ReasonProviderChanged
			continue
		}
		m.finish(op, opstore.StateRetired, opstore.ReasonProviderChanged)
	}
	m.frozen = append(m.frozen, prev)
	m.active = nil
}

func (m *model) confirm() {
	if len(m.frozen) == 0 || m.active == nil {
		return
	}
	prev := m.frozen[0]
	m.arm(0.3)
	err := m.h.s.ConfirmFreeze(context.Background(), prev)
	m.commit(func() {
		m.frozen = m.frozen[1:]
		for _, op := range m.ops {
			if bytes.Equal(op.r.EnrolmentID, prev) {
				op.gone = true
			}
		}
		for vmUID, vm := range m.vms {
			if vm.busy != "" && m.ops[vm.busy] != nil && bytes.Equal(m.ops[vm.busy].r.EnrolmentID, prev) {
				m.vms[vmUID].busy = ""
			}
		}
		pfx := string(prev) + "|"
		for k := range m.vmTomb {
			if len(k) >= len(pfx) && k[:len(pfx)] == pfx {
				delete(m.vmTomb, k)
			}
		}
		for k := range m.attempts {
			if len(k) > 32 && k[:32] == fmt.Sprintf("%x", prev) {
				delete(m.attempts, k)
			}
		}
	})
	if err != nil && !m.crashed {
		m.fail("confirm freeze: %v", err)
	}
	m.settle()
}

// check compares every operation and the invariants.
func (m *model) check() {
	holders := map[string]int{}
	for _, k := range m.order {
		op := m.ops[k]
		v, found, err := m.h.s.Lookup(context.Background(), ref(op.r))
		if err != nil {
			m.fail("lookup: %v", err)
		}
		want := op.recorded && !op.gone
		if found != want {
			m.fail("op %x %v: found %v, model %v", op.r.OpID, op.r.Kind, found, want)
		}
		if found {
			if v.State != op.state || v.Reason != op.reason || v.Irrev != op.irrev || v.Tombstoned != op.compact {
				m.fail("op %x %v: store %v{%s} irrev=%v tomb=%v, model %v{%s} irrev=%v tomb=%v", op.r.OpID, op.r.Kind,
					v.State, v.Reason, v.Irrev, v.Tombstoned, op.state, op.reason, op.irrev, op.compact)
			}
			if holds(v.State) {
				holders[op.r.VMUID]++
			}
		}
		for i, st := range testPlans[op.r.Kind] {
			got := m.h.exec.effects(op.r.OpID, st.Name)
			if got != op.runs[i] {
				m.fail("op %x step %s: %d runs, model %d", op.r.OpID, st.Name, got, op.runs[i])
			}
			if (st.Once || st.Irreversible) && got > 1 {
				m.fail("op %x step %s ran %d times", op.r.OpID, st.Name, got)
			}
			if op.state == opstore.StateRetired && got > 0 {
				m.fail("retired op %x had an effect", op.r.OpID)
			}
		}
		if op.state == opstore.StateRetired {
			for _, st := range op.steps {
				if st.intent {
					m.fail("retired op %x had a step intent", op.r.OpID)
				}
			}
		}
	}
	if len(m.order) > 0 {
		last := m.ops[m.order[len(m.order)-1]]
		if last.recorded {
			stat("state:" + last.state.String() + "/" + last.reason)
		}
	}
	for vm, n := range holders {
		if n > 1 {
			m.fail("%d operations hold %s", n, vm)
		}
	}
}
