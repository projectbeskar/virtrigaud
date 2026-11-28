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
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// retire asks h's store to retire r in session c.
func retire(t *testing.T, h *harness, c []byte, r opstore.Request) (opstore.RetireResult, error) {
	t.Helper()
	return h.s.RetireIfUnknown(t.Context(), opstore.RetireRequest{Session: c, Op: r})
}

// wantRetire expects retire to return outcome want.
func wantRetire(t *testing.T, h *harness, c []byte, r opstore.Request, want opstore.RetireOutcome) opstore.RetireResult {
	t.Helper()
	res, err := retire(t, h, c, r)
	if err != nil || res.Outcome != want {
		t.Fatalf("retire: %+v %v, want outcome %v", res, err, want)
	}
	return res
}

// wantStoreLost expects the store lost with reason.
func wantStoreLost(t *testing.T, h *harness, reason journal.LostReason) {
	t.Helper()
	if st, r := h.s.Journal().State(); st != journal.StateLost || r != reason {
		t.Fatalf("store state %v{%s}, want lost{%s}", st, r, reason)
	}
}

// reqP is a new request for vm that carries its request bytes.
func reqP(h *harness, vm string, binding []byte, kind providerv1.OperationKind, attempt uint64) opstore.Request {
	return withRequest(h.req(vm, binding, kind, attempt), []byte("the request"))
}

// markAndRun covers r's acceptance with a ledger mark and runs it.
func markAndRun(t *testing.T, h *harness, c []byte, r opstore.Request) opstore.OpView {
	t.Helper()
	mark(t, h, c, h.s.Position(), opstore.LedgerMarkApplied)
	return h.run(r)
}

// An operation the store never received is retired: its RETIRED tombstone
// (never_received) is committed, a delivery of it is answered RETIRED and
// never run, the same op_id with other fields is a conflict, and the
// attempt mark of its VM and kind rose, so a lower or equal attempt of
// another op_id is refused while a higher one is accepted.
func TestRetire_NeverReceived(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 3)
	res := wantRetire(t, h, c, r, opstore.RetireApplied)
	if res.State != opstore.StateRetired {
		t.Fatalf("state %v", res.State)
	}
	v, ok := h.lookup(r)
	if !ok || v.State != opstore.StateRetired || v.Reason != opstore.ReasonNeverReceived || !v.Tombstoned {
		t.Fatalf("after the retirement: %+v %v", v, ok)
	}
	if d := h.admit(r); d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateRetired {
		t.Fatalf("a delivery after the retirement: %+v", d)
	}
	if h.exec.effects(r.OpID, "prepare")+h.exec.effects(r.OpID, "start") != 0 {
		t.Fatal("a retired operation ran")
	}
	other := r
	other.Attempt = 4
	if d := h.admit(other); !d.IsRefusal(opstore.RefusalOpIDConflict) {
		t.Fatalf("the same op_id with other fields: %+v", d)
	}
	same := reqP(h, "vm-1", r.BindingID, kCreate, 3)
	if d := h.admit(same); !d.IsRefusal(opstore.RefusalOpIDConflict) {
		t.Fatalf("an attempt not above the retired one: %+v", d)
	}
	h.mustAccept(reqP(h, "vm-1", r.BindingID, kCreate, 4))
	h.verifyChain()
}

// A second retirement of the same operation is already applied; a
// retirement of the same op_id with other fields is op_known.
func TestRetire_AlreadyAppliedAndOpKnown(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	wantRetire(t, h, c, r, opstore.RetireApplied)
	before := h.s.Position()
	if res := wantRetire(t, h, c, r, opstore.RetireAlreadyApplied); res.State != opstore.StateRetired {
		t.Fatalf("state %v", res.State)
	}
	other := r
	other.Attempt = 2
	wantRetire(t, h, c, other, opstore.RetireOpKnown)
	if h.s.Position() != before {
		t.Fatal("a retirement that changed nothing committed")
	}
}

// An operation the store accepted is never retired: the retirement is not
// applicable, with the record's state, before and after it ran; with other
// fields it is op_known.
func TestRetire_NotApplicableForAKnownOperation(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	h.mustAccept(r)
	if res := wantRetire(t, h, c, r, opstore.RetireNotApplicable); res.State != opstore.StateAccepted {
		t.Fatalf("state %v", res.State)
	}
	if v := markAndRun(t, h, c, r); v.State != opstore.StateCompleted {
		t.Fatalf("ran: %v", v.State)
	}
	if res := wantRetire(t, h, c, r, opstore.RetireNotApplicable); res.State != opstore.StateCompleted {
		t.Fatalf("state %v", res.State)
	}
	other := r
	other.Attempt = 9
	wantRetire(t, h, c, other, opstore.RetireOpKnown)
	if st, _ := h.s.Journal().State(); st == journal.StateLost {
		t.Fatal("a known operation's effects quarantined the store")
	}
}

// A retirement is refused, changing nothing, for another session, for a
// store that awaits its first attestation, and for an enrolment that is
// not the active one.
func TestRetire_Refusals(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	before := h.s.Position()
	if _, err := retire(t, h, session(), r); !errors.Is(err, opstore.ErrStaleAttestation) {
		t.Fatalf("another session: %v", err)
	}
	other := r
	other.EnrolmentID = rid()
	if _, err := retire(t, h, c, other); !errors.Is(err, opstore.ErrEnrolmentNotActive) {
		t.Fatalf("another enrolment: %v", err)
	}
	otherProvider := r
	otherProvider.ProviderUID = "another-provider"
	if _, err := retire(t, h, c, otherProvider); !errors.Is(err, opstore.ErrEnrolmentNotActive) {
		t.Fatalf("another Provider: %v", err)
	}
	if h.s.Position() != before {
		t.Fatal("a refused retirement committed")
	}
	if _, ok := h.lookup(r); ok {
		t.Fatal("a refused retirement left a trace")
	}

	u := newHarness(t)
	u.opts = func(o *opstore.Options) { o.LedgerGate = true }
	u.fresh()
	if _, err := retire(t, u, c, reqP(u, "vm-1", rid(), kCreate, 1)); !errors.Is(err, opstore.ErrAttestState) {
		t.Fatalf("an unattested store: %v", err)
	}
}

// A trace of the operation among the executor's objects that no record
// explains quarantines the store (unknown_op_trace) and retires nothing.
func TestRetire_TraceFoundQuarantines(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kPowerOn, 1)
	h.exec.mu.Lock()
	h.exec.present[effKey(r.OpID, "power")] = true
	h.exec.mu.Unlock()
	res := wantRetire(t, h, c, r, opstore.RetireTraceFound)
	if res.Quarantined != journal.LostUnknownOpTrace {
		t.Fatalf("quarantined %q", res.Quarantined)
	}
	wantStoreLost(t, h, journal.LostUnknownOpTrace)
	if h.tombstoned(r) {
		t.Fatal("a retirement that found a trace committed a tombstone")
	}
}

// A Create's placement among the executor's objects is a trace unless the
// store records the placement.
func TestRetire_PlacementTrace(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	h.exec.mu.Lock()
	h.exec.placements[placementKey(r.VMUID, r.BindingID)] = true
	h.exec.mu.Unlock()
	wantRetire(t, h, c, r, opstore.RetireTraceFound)
	wantStoreLost(t, h, journal.LostUnknownOpTrace)

	k, kc := gateHarness(t, nil)
	created := reqP(k, "vm-2", rid(), kCreate, 1)
	k.mustAccept(created)
	markAndRun(t, k, kc, created)
	k.exec.mu.Lock()
	k.exec.placements[placementKey(created.VMUID, created.BindingID)] = true
	k.exec.mu.Unlock()
	// A Create of the same placement the store records (made by another
	// op_id) is explained by the VM record: not a trace of this one.
	later := reqP(k, "vm-2", created.BindingID, kCreate, 2)
	wantRetire(t, k, kc, later, opstore.RetireApplied)
	if st, _ := k.s.Journal().State(); st == journal.StateLost {
		t.Fatal("a recorded placement quarantined the store")
	}
}

// A trace explained by the operation's record is not a loss: the
// retirement is not applicable and the store stays healthy.
func TestRetire_TraceExplainedByRecord(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	h.mustAccept(r)
	markAndRun(t, h, c, r)
	wantRetire(t, h, c, r, opstore.RetireNotApplicable)
	if st, _ := h.s.Journal().State(); st == journal.StateLost {
		t.Fatal("an explained trace quarantined the store")
	}
}

// A witness file of the operation, or a VM record naming it busy, without
// its record quarantines the store (inconsistent_store).
func TestRetire_WitnessOrBusyVMQuarantines(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kPowerOn, 1)
	dir := filepath.Join(h.dir, "witness", fmt.Sprintf("%x", r.EnrolmentID))
	if err := h.mem.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := h.mem.OpenFile(filepath.Join(dir, fmt.Sprintf("%x.0", r.OpID)), 0o101, 0o600) // O_WRONLY|O_CREATE
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res := wantRetire(t, h, c, r, opstore.RetireQuarantined)
	if res.Quarantined != journal.LostInconsistentStore {
		t.Fatalf("quarantined %q", res.Quarantined)
	}
	wantStoreLost(t, h, journal.LostInconsistentStore)

	b, bc := gateHarness(t, nil)
	busy := reqP(b, "vm-9", rid(), kPowerOn, 1)
	if err := opstore.PutBusyVM(t.Context(), b.s, busy.VMUID, busy.BindingID, busy.EnrolmentID, busy.OpID); err != nil {
		t.Fatal(err)
	}
	wantRetire(t, b, bc, busy, opstore.RetireQuarantined)
	wantStoreLost(t, b, journal.LostInconsistentStore)
}

// noScanExec is an executor that cannot scan its objects.
type noScanExec struct{ executor.Executor }

// A store whose executor cannot scan for traces (ErrNoTraceScanner), or
// whose scan fails (ErrTraceScanFailed), retires nothing; the two are told
// apart.
func TestRetire_NoTraceScanRetiresNothing(t *testing.T) {
	h, c := gateHarness(t, func(o *opstore.Options) { o.Executor = noScanExec{o.Executor} })
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	if _, err := retire(t, h, c, r); !errors.Is(err, opstore.ErrNoTraceScanner) || errors.Is(err, opstore.ErrTraceScanFailed) {
		t.Fatalf("no scanner: %v", err)
	}
	f, fc := gateHarness(t, nil)
	f.exec.traceErr = errors.New("scan failed")
	if _, err := retire(t, f, fc, r); !errors.Is(err, opstore.ErrTraceScanFailed) || errors.Is(err, opstore.ErrNoTraceScanner) {
		t.Fatalf("a failed scan: %v", err)
	}
	if h.tombstoned(r) || f.tombstoned(r) {
		t.Fatal("retired without a scan")
	}
}

// delivery is one way a delivery of the retired op_id can race the
// retirement.
type delivery struct {
	// at is where the delivery happens: "before", "in-scan" (inside the
	// trace scan, before it reads), "scanned", "locking" or "after".
	at string
	// run is true when the delivery is also run to its end (an effect).
	run bool
	// sameIdentity is false for a delivery of the op_id with other fields.
	sameIdentity bool
}

// raceOnce runs one interleaving of a retirement of r with a delivery d
// and checks the invariants: never both executed and retired; exactly one
// of {retired, the delivery answered from the tombstone or refused} or
// {the delivery accepted, the retirement not applicable or op_known}.
func raceOnce(t *testing.T, d delivery) {
	t.Helper()
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kPowerOn, 1)
	cr := reqP(h, "vm-1", r.BindingID, kCreate, 1) // the VM exists
	h.mustAccept(cr)
	markAndRun(t, h, c, cr)
	sent := r
	if !d.sameIdentity {
		sent.Attempt = 7
	}
	var accepted bool
	var deliveryErr error
	deliver := func() {
		dd, err := h.s.Admit(t.Context(), sent)
		if err != nil {
			deliveryErr = err
			return
		}
		accepted = dd.Outcome == opstore.OutcomeAccepted
		if accepted && d.run {
			if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c, Ledger: h.s.Position()}); err != nil {
				deliveryErr = err
				return
			}
			if _, err := h.s.Execute(t.Context(), ref(sent), nil); err != nil {
				deliveryErr = err
			}
		}
	}
	switch d.at {
	case "before":
		deliver()
	case "in-scan":
		h.exec.traceHook = deliver
	default:
		opstore.SetRetireHook(h.s, func(point string) {
			if point == d.at {
				deliver()
			}
		})
	}
	res, err := retire(t, h, c, r)
	if d.at == "after" {
		deliver()
	}
	if err != nil || deliveryErr != nil {
		t.Fatalf("%+v: retire %v, delivery %v", d, err, deliveryErr)
	}
	executed := h.exec.effects(r.OpID, "power") + h.exec.effects(sent.OpID, "power")
	tomb := h.tombstoned(r)
	retired := res.Outcome == opstore.RetireApplied
	switch {
	case retired && executed > 0:
		t.Fatalf("%+v: both retired and executed", d)
	case retired && accepted:
		t.Fatalf("%+v: both retired and accepted", d)
	case retired && !tomb:
		t.Fatalf("%+v: retired without a tombstone", d)
	case !retired && !accepted:
		t.Fatalf("%+v: neither retired nor accepted (%v)", d, res.Outcome)
	case !retired && res.Outcome != opstore.RetireNotApplicable && res.Outcome != opstore.RetireOpKnown:
		t.Fatalf("%+v: a known operation's retirement ended %v", d, res.Outcome)
	}
	if st, _ := h.s.Journal().State(); st == journal.StateLost {
		t.Fatalf("%+v: a race quarantined a healthy store", d)
	}
}

// Retirement/delivery races: every interleaving point, with and without
// the delivery running, with the same and with other identity fields,
// never ends with the operation both executed and retired, and never
// quarantines a healthy store. Forty seeded schedules also pick the
// combination at random.
func TestRetire_DeliveryRaces(t *testing.T) {
	points := []string{"before", "in-scan", "scanned", "locking", "after"}
	var all []delivery
	for _, at := range points {
		for _, run := range []bool{false, true} {
			for _, same := range []bool{true, false} {
				all = append(all, delivery{at: at, run: run, sameIdentity: same})
			}
		}
	}
	for _, d := range all {
		t.Run(fmt.Sprintf("%s/run=%v/same=%v", d.at, d.run, d.sameIdentity), func(t *testing.T) { raceOnce(t, d) })
	}
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, seed^0x5eed))
		d := all[rng.IntN(len(all))]
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { raceOnce(t, d) })
	}
}

// Concurrent retirements and deliveries of the same op_ids, on real
// goroutines (run with -race): never both executed and retired.
func TestRetire_ConcurrentDeliveries(t *testing.T) {
	h, c := gateHarness(t, nil)
	var reqs []opstore.Request
	for i := range 20 {
		vm := fmt.Sprintf("vm-%d", i)
		cr := reqP(h, vm, rid(), kCreate, 1)
		h.mustAccept(cr)
		markAndRun(t, h, c, cr)
		reqs = append(reqs, reqP(h, vm, cr.BindingID, kPowerOn, 1))
	}
	retired := make([]bool, len(reqs))
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, err := h.s.RetireIfUnknown(t.Context(), opstore.RetireRequest{Session: c, Op: r})
			retired[i] = err == nil && res.Outcome == opstore.RetireApplied
		}()
		go func() {
			defer wg.Done()
			if d, err := h.s.Admit(t.Context(), r); err == nil && d.Outcome == opstore.OutcomeAccepted {
				_, _ = h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c, Ledger: h.s.Position()})
				_, _ = h.s.Execute(t.Context(), ref(r), nil)
			}
		}()
	}
	wg.Wait()
	for i, r := range reqs {
		if retired[i] && h.exec.effects(r.OpID, "power") > 0 {
			t.Fatalf("op %d both retired and executed", i)
		}
	}
	if st, _ := h.s.Journal().State(); st == journal.StateLost {
		t.Fatal("concurrent races quarantined a healthy store")
	}
	h.verifyChain()
}

// scans is the number of trace scans h's executor ran.
func scans(h *harness) int {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	return h.exec.traceCalls
}

// An operation the store already holds a record or tombstone of is not
// scanned for: a replayed retirement grant costs one read, never a scan.
func TestRetire_KnownOperationIsNotScanned(t *testing.T) {
	h, c := gateHarness(t, nil)
	known := reqP(h, "vm-1", rid(), kCreate, 1)
	h.mustAccept(known)
	wantRetire(t, h, c, known, opstore.RetireNotApplicable)
	if n := scans(h); n != 0 {
		t.Fatalf("a known operation was scanned for (%d scans)", n)
	}
	lost := reqP(h, "vm-2", rid(), kCreate, 1)
	wantRetire(t, h, c, lost, opstore.RetireApplied)
	if n := scans(h); n != 1 {
		t.Fatalf("an unknown operation: %d scans, want 1", n)
	}
	for range 5 {
		wantRetire(t, h, c, lost, opstore.RetireAlreadyApplied)
	}
	if n := scans(h); n != 1 {
		t.Fatalf("replayed retirements scanned again (%d scans)", n)
	}
}

// The session is checked again under the store lock: a re-attestation
// between the scan and the lock makes the retirement stale, and nothing is
// retired.
func TestRetire_SessionRecheckedUnderTheLock(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	opstore.SetRetireHook(h.s, func(point string) {
		if point != "locking" {
			return
		}
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: h.s.Position(), Reattest: true})
		if err != nil || res.Quarantined != "" {
			t.Errorf("re-attest: %+v %v", res, err)
		}
	})
	if _, err := retire(t, h, c, r); !errors.Is(err, opstore.ErrStaleAttestation) {
		t.Fatalf("a session replaced before the lock: %v, want ErrStaleAttestation", err)
	}
	if h.tombstoned(r) {
		t.Fatal("a stale retirement committed a tombstone")
	}
}

// A store above its size threshold commits no tombstone, as admission
// commits no record then.
func TestRetire_StoreFullWritesNothing(t *testing.T) {
	h, c := gateHarness(t, func(o *opstore.Options) { o.MaxStoreBytes = 1 })
	r := reqP(h, "vm-1", rid(), kCreate, 1)
	before := h.s.Position()
	if _, err := retire(t, h, c, r); !errors.Is(err, opstore.ErrStoreFull) {
		t.Fatalf("a full store: %v, want ErrStoreFull", err)
	}
	if h.tombstoned(r) || h.s.Position() != before {
		t.Fatal("a full store committed a tombstone")
	}
}
