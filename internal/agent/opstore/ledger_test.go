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
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// gateHarness is an enrolled store with the ledger gate, attested at its
// tip; mod adjusts its options.
func gateHarness(t *testing.T, mod func(*opstore.Options)) (*harness, []byte) {
	t.Helper()
	h := newHarness(t)
	h.opts = func(o *opstore.Options) {
		o.LedgerGate = true
		if mod != nil {
			mod(o)
		}
	}
	h.fresh()
	c := session()
	if res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: c, Ledger: h.s.Position()}); err != nil || res.Quarantined != "" {
		t.Fatalf("attest: %+v %v", res, err)
	}
	return h, c
}

// withRequest is r carrying request bytes.
func withRequest(r opstore.Request, payload []byte) opstore.Request {
	r.RequestBytes = payload
	return r
}

// mark raises the ledger mark of session c to l and expects outcome.
func mark(t *testing.T, h *harness, c []byte, l journal.Position, want opstore.LedgerMarkOutcome) {
	t.Helper()
	res, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c, Ledger: l})
	if err != nil || res.Outcome != want {
		t.Fatalf("ledger mark at %d: %+v %v, want outcome %v", l.Seq, res, err, want)
	}
}

// closed reports whether ch is closed.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestLedgerGate_ParksUntilMarked: an accepted operation commits nothing
// until a ledger mark covers its acceptance; the mark wakes it and it runs
// with the request bytes the store held.
func TestLedgerGate_ParksUntilMarked(t *testing.T) {
	h, c := gateHarness(t, nil)
	payload := []byte("the request")
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), payload)
	d := h.admit(r)
	if d.Outcome != opstore.OutcomeAccepted || d.Acceptance == nil {
		t.Fatalf("admit: %+v", d)
	}
	acc := d.Acceptance.Position
	if acc.Seq != d.Op.AcceptedSeq || acc != h.s.Position() || !bytes.Equal(d.Acceptance.Session, c) {
		t.Fatalf("acceptance %+v (accepted seq %d, tip %+v)", d.Acceptance, d.Op.AcceptedSeq, h.s.Position())
	}
	if b := h.s.Backlog(); b.PendingReceipts != 1 || b.ParkedRequestBytes != uint64(len(payload)) ||
		b.ParkedRequestBudgetBytes != opstore.DefaultParkedRequestBudget {
		t.Fatalf("backlog after accept: %+v", b)
	}
	before := h.s.Position()
	wake := h.s.LedgerWake()
	if _, err := h.s.Execute(t.Context(), ref(r), nil); !errors.Is(err, opstore.ErrAwaitingLedger) {
		t.Fatalf("execute before the mark: %v", err)
	}
	if v, _ := h.lookup(r); v.State != opstore.StateAccepted || h.s.Position() != before || h.exec.effects(r.OpID, "start") != 0 ||
		h.exec.effects(r.OpID, "prepare") != 0 {
		t.Fatalf("a parked operation committed or ran: %+v", v)
	}
	if closed(wake) {
		t.Fatal("woken without a mark")
	}
	// A mark below the acceptance changes nothing.
	mark(t, h, c, acc, opstore.LedgerMarkApplied)
	if !closed(wake) {
		t.Fatal("the mark did not wake the parked operations")
	}
	if b := h.s.Backlog(); b.PendingReceipts != 0 {
		t.Fatalf("backlog after the mark: %+v", b)
	}
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("after the mark: %v{%s}", v.State, v.Reason)
	}
	if got := h.exec.payload[effKey(r.OpID, "prepare")]; !bytes.Equal(got, payload) {
		t.Fatalf("the executor got request %q, want %q", got, payload)
	}
	if b := h.s.Backlog(); b.ParkedRequestBytes != 0 {
		t.Fatalf("the request bytes stayed parked: %+v", b)
	}
	// The same mark again is already applied.
	mark(t, h, c, acc, opstore.LedgerMarkAlreadyApplied)
}

// TestLedgerGate_NotAttested: a store with the gate that awaits its first
// attestation starts nothing.
func TestLedgerGate_NotAttested(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.LedgerGate = true }
	h.fresh()
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("x"))
	d := h.admit(r)
	if d.Outcome != opstore.OutcomeAccepted || d.Acceptance != nil {
		t.Fatalf("admit while not attested: %+v", d)
	}
	if _, err := h.s.Execute(t.Context(), ref(r), nil); !errors.Is(err, opstore.ErrAwaitingLedger) {
		t.Fatalf("execute while not attested: %v", err)
	}
	if h.exec.effects(r.OpID, "prepare") != 0 {
		t.Fatal("ran before any attestation")
	}
}

// TestLedgerGate_Off: without the gate nothing is parked and no acceptance
// is reported.
func TestLedgerGate_Off(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("x"))
	d := h.admit(r)
	if d.Acceptance != nil || h.s.LedgerGate() {
		t.Fatalf("acceptance without the gate: %+v", d)
	}
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("without the gate: %v", v.State)
	}
	if b := h.s.Backlog(); b.PendingReceipts != 0 || b.ParkedRequestBytes != 0 {
		t.Fatalf("backlog without the gate: %+v", b)
	}
}

// TestLedgerGate_AwaitingRequest: a covered operation without its request
// bytes waits for a delivery of the same op_id, unless the executor runs its
// kind without them.
func TestLedgerGate_AwaitingRequest(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := h.req("vm-1", rid(), kCreate, 1)
	d := h.admit(r)
	mark(t, h, c, d.Acceptance.Position, opstore.LedgerMarkApplied)
	wake := h.s.LedgerWake()
	if _, err := h.s.Execute(t.Context(), ref(r), nil); !errors.Is(err, opstore.ErrAwaitingRequest) {
		t.Fatalf("execute without a request: %v", err)
	}
	v, err := h.s.AnswerOp(t.Context(), ref(r), 0)
	if err != nil || !v.AwaitingRequest || v.Op.State != opstore.StateAccepted || len(v.AcceptedHead) != 32 {
		t.Fatalf("answer: %+v %v", v, err)
	}
	if h.exec.effects(r.OpID, "prepare") != 0 {
		t.Fatal("ran without its request")
	}
	h.s.DepositRequest(ref(r), []byte("again"))
	if !closed(wake) {
		t.Fatal("a deposit did not wake the parked operations")
	}
	if v, _ := h.s.AnswerOp(t.Context(), ref(r), 0); v.AwaitingRequest {
		t.Fatal("still awaiting its request after a deposit")
	}
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("after the deposit: %v", v.State)
	}
	if got := h.exec.payload[effKey(r.OpID, "prepare")]; string(got) != "again" {
		t.Fatalf("the executor got %q", got)
	}
	// A deposit for an operation that is not parked is ignored.
	h.s.DepositRequest(ref(r), []byte("late"))
	if b := h.s.Backlog(); b.ParkedRequestBytes != 0 {
		t.Fatalf("a late deposit was held: %+v", b)
	}

	// An executor that runs the kind without its request starts it.
	h2, c2 := gateHarness(t, func(o *opstore.Options) { o.Executor = requestFree{o.Executor} })
	r2 := h2.req("vm-2", rid(), kCreate, 1)
	d2 := h2.admit(r2)
	mark(t, h2, c2, d2.Acceptance.Position, opstore.LedgerMarkApplied)
	if v, _ := h2.s.AnswerOp(t.Context(), ref(r2), 0); v.AwaitingRequest {
		t.Fatal("awaiting a request the executor does not need")
	}
	if v := h2.run(r2); v.State != opstore.StateCompleted {
		t.Fatalf("request-free kind: %v", v.State)
	}
}

// requestFree runs every kind without its request.
type requestFree struct{ executor.Executor }

func (requestFree) RunsWithoutRequest(providerv1.OperationKind) bool { return true }

// TestLedgerGate_CrashBetweenAcceptAndMark: an operation accepted and lost
// to a crash before any mark is recovered, counted as pending again, kept
// parked under an attestation below its acceptance, and run (once its
// request comes again) only after a mark covers it.
func TestLedgerGate_CrashBetweenAcceptAndMark(t *testing.T) {
	h, _ := gateHarness(t, nil)
	before := h.s.Position()
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("req"))
	acc := h.admit(r).Acceptance.Position
	// Crash: the session, the mark and the parked request are memory only.
	_ = h.s.Close()
	h.mem.Crash()
	s := h.open()
	if err := s.EstablishContinuity(t.Context()); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Recover(t.Context())
	if err != nil || len(refs) != 1 || !bytes.Equal(refs[0].OpID, r.OpID) {
		t.Fatalf("recover: %v %v", refs, err)
	}
	if _, err := s.Execute(t.Context(), refs[0], nil); !errors.Is(err, opstore.ErrAwaitingLedger) {
		t.Fatalf("execute before the attestation: %v", err)
	}
	// The manager's ledger is below the acceptance (the receipt was lost).
	c := session()
	if res, err := s.Attest(t.Context(), opstore.AttestRequest{Session: c, Ledger: before}); err != nil || res.Quarantined != "" {
		t.Fatalf("attest: %+v %v", res, err)
	}
	if b := s.Backlog(); b.PendingReceipts != 1 || b.ParkedRequestBytes != 0 {
		t.Fatalf("backlog after the restart: %+v", b)
	}
	if _, err := s.Execute(t.Context(), refs[0], nil); !errors.Is(err, opstore.ErrAwaitingLedger) {
		t.Fatalf("execute under a mark below the acceptance: %v", err)
	}
	v, err := s.AnswerOp(t.Context(), refs[0], 0)
	if err != nil || v.Op.AcceptedSeq != acc.Seq || !bytes.Equal(v.AcceptedHead, acc.Head[:]) || v.AwaitingRequest {
		t.Fatalf("answer: %+v %v", v, err)
	}
	if h.exec.effects(r.OpID, "prepare") != 0 {
		t.Fatal("ran before its acceptance was ledgered")
	}
	mark(t, h, c, acc, opstore.LedgerMarkApplied)
	if _, err := s.Execute(t.Context(), refs[0], nil); !errors.Is(err, opstore.ErrAwaitingRequest) {
		t.Fatalf("execute without the request lost by the restart: %v", err)
	}
	if _, err := s.Execute(t.Context(), refs[0], []byte("req")); err != nil {
		t.Fatalf("execute with the request again: %v", err)
	}
	if got, _ := h.lookup(r); got.State != opstore.StateCompleted || h.exec.effects(r.OpID, "start") != 1 {
		t.Fatalf("after the mark: %+v", got)
	}
}

// TestLedgerGate_StartedNotGated: an operation whose first step intent is
// durable is resumed whatever the mark.
func TestLedgerGate_StartedNotGated(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	before := h.s.Position()
	r := h.req("vm-1", rid(), kCreate, 1)
	h.mustAccept(r)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterIntent, N: 1}
	if _, err := h.s.Execute(t.Context(), ref(r), nil); err == nil {
		t.Fatal("no crash after the intent")
	}
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	h.opts = func(o *opstore.Options) { o.LedgerGate = true }
	s := h.open()
	if err := s.EstablishContinuity(t.Context()); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Recover(t.Context())
	if err != nil || len(refs) != 1 {
		t.Fatalf("recover: %v %v", refs, err)
	}
	if _, err := s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: before}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Execute(t.Context(), refs[0], nil); err != nil || v.State != opstore.StateCompleted {
		t.Fatalf("a started operation was gated: %+v %v", v, err)
	}
}

// TestLedgerMark_Rules: a mark needs an attested store and its session;
// one that is not on the chain quarantines; a lower one is already applied.
func TestLedgerMark_Rules(t *testing.T) {
	flip := func(b [32]byte) [32]byte { b[0] ^= 1; return b }
	t.Run("not attested", func(t *testing.T) {
		h, _ := attestHarness(t, true)
		if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: session(), Ledger: h.s.Position()}); !errors.Is(err, opstore.ErrAttestState) {
			t.Fatalf("mark while awaiting: %v", err)
		}
	})
	attested := func(t *testing.T) (*harness, []byte, []journal.Position) {
		h, seen := attestHarness(t, true)
		c := session()
		if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: c, Ledger: seen[5]}); err != nil {
			t.Fatal(err)
		}
		return h, c, seen
	}
	wantLost := func(t *testing.T, h *harness, l journal.Position, c []byte, want journal.LostReason) {
		t.Helper()
		res, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c, Ledger: l})
		if err != nil || res.Outcome != opstore.LedgerMarkQuarantined || res.Quarantined != want {
			t.Fatalf("mark: %+v %v, want quarantine %s", res, err, want)
		}
		if st, r := h.s.Journal().State(); st != journal.StateLost || r != want {
			t.Fatalf("journal %v{%s}", st, r)
		}
	}
	t.Run("stale session", func(t *testing.T) {
		h, _, seen := attested(t)
		if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: session(), Ledger: seen[8]}); !errors.Is(err, opstore.ErrStaleAttestation) {
			t.Fatalf("stale mark: %v", err)
		}
		if a, _ := h.s.Attestation(); a.Mark != seen[5] || h.s.Lost() {
			t.Fatalf("a stale mark changed the store: %+v", a)
		}
	})
	t.Run("raises and keeps", func(t *testing.T) {
		h, c, seen := attested(t)
		mark(t, h, c, seen[8], opstore.LedgerMarkApplied)
		if a, _ := h.s.Attestation(); a.Mark != seen[8] || h.s.Journal().RetentionFloor() != seen[8].Seq {
			t.Fatalf("after the mark: %+v floor %d", a, h.s.Journal().RetentionFloor())
		}
		mark(t, h, c, seen[6], opstore.LedgerMarkAlreadyApplied)
		mark(t, h, c, seen[8], opstore.LedgerMarkAlreadyApplied)
		if a, _ := h.s.Attestation(); a.Mark != seen[8] {
			t.Fatalf("a lower mark lowered it: %+v", a)
		}
	})
	t.Run("another store", func(t *testing.T) {
		h, c, seen := attested(t)
		l := seen[8]
		l.StoreID[0] ^= 1
		wantLost(t, h, l, c, journal.LostLedgerStoreMismatch)
	})
	t.Run("ahead of the chain", func(t *testing.T) {
		h, c, _ := attested(t)
		l := h.s.Position()
		l.Seq++
		wantLost(t, h, l, c, journal.LostLedgerAhead)
	})
	t.Run("another head above the mark", func(t *testing.T) {
		h, c, seen := attested(t)
		l := seen[8]
		l.Head = flip(l.Head)
		wantLost(t, h, l, c, journal.LostLedgerMismatch)
	})
	t.Run("another head below the mark", func(t *testing.T) {
		h, c, seen := attested(t)
		l := seen[4]
		l.Head = flip(l.Head)
		wantLost(t, h, l, c, journal.LostLedgerMismatch)
	})
	t.Run("a head above the mark the store does not hold", func(t *testing.T) {
		h, seen := attestHarness(t, false)
		old := prunedPosition(t, h, seen)
		cpSeq := old.Seq - old.Seq%4
		head, ok := headAt(t, h, cpSeq)
		if !ok || cpSeq == 0 {
			t.Fatalf("no checkpoint at %d", cpSeq)
		}
		c := session()
		cp := journal.Position{StoreID: old.StoreID, Seq: cpSeq, Head: head}
		if res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: c, Ledger: cp}); err != nil || res.Quarantined != "" {
			t.Fatalf("attest: %+v %v", res, err)
		}
		wantLost(t, h, old, c, journal.LostLedgerUnverifiable)
	})
	t.Run("a lost store refuses", func(t *testing.T) {
		h, c, seen := attested(t)
		h.s.Journal().Quarantine(journal.LostRolledBack, "test")
		if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c, Ledger: seen[8]}); !errors.Is(err, journal.ErrLost) {
			t.Fatalf("mark on a lost store: %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		h, c, _ := attested(t)
		if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: []byte{1}, Ledger: h.s.Position()}); err == nil {
			t.Fatal("a short session was accepted")
		}
		if _, err := h.s.LedgerMark(t.Context(), opstore.LedgerMarkRequest{Session: c}); err == nil {
			t.Fatal("a zero position was accepted")
		}
	})
}

// TestLedgerBehind_Count: new operations are refused, definitely, while the
// accepted operations above the mark are at their bound; known op_ids are
// still answered, and a mark lifts the refusal.
func TestLedgerBehind_Count(t *testing.T) {
	h, c := gateHarness(t, func(o *opstore.Options) { o.MaxPendingReceipts = 2 })
	r1 := h.req("vm-1", rid(), kCreate, 1)
	h.mustAccept(r1)
	r2 := h.req("vm-2", rid(), kCreate, 1)
	h.mustAccept(r2)
	r3 := h.req("vm-3", rid(), kCreate, 1)
	d := h.admit(r3)
	if !d.IsRefusal(opstore.RefusalLedgerBehind) || !d.Refusal.DefiniteNotPerformed() || d.Refusal.String() != "LEDGER_BEHIND" {
		t.Fatalf("third operation: %+v", d)
	}
	if _, found := h.lookup(r3); found {
		t.Fatal("a refused operation was recorded")
	}
	if d := h.admit(r1); d.Outcome != opstore.OutcomeKnown {
		t.Fatalf("a known op_id while behind: %+v", d)
	}
	mark(t, h, c, h.s.Position(), opstore.LedgerMarkApplied)
	h.mustAccept(r3)
}

// TestLedgerBehind_Bytes: the parked request bytes never exceed their
// budget.
func TestLedgerBehind_Bytes(t *testing.T) {
	h, _ := gateHarness(t, func(o *opstore.Options) { o.ParkedRequestBudget = 10 })
	h.mustAccept(withRequest(h.req("vm-1", rid(), kCreate, 1), make([]byte, 6)))
	d := h.admit(withRequest(h.req("vm-2", rid(), kCreate, 1), make([]byte, 6)))
	if !d.IsRefusal(opstore.RefusalLedgerBehind) {
		t.Fatalf("over the budget: %+v", d)
	}
	h.mustAccept(withRequest(h.req("vm-3", rid(), kCreate, 1), make([]byte, 4)))
	if b := h.s.Backlog(); b.ParkedRequestBytes != 10 || b.ParkedRequestBudgetBytes != 10 {
		t.Fatalf("backlog: %+v", b)
	}
}

// TestExpectKnown: an op_id the store knows nothing of is refused, not
// accepted, when its grant says the manager holds a ledgered receipt.
func TestExpectKnown(t *testing.T) {
	h, _ := gateHarness(t, nil)
	r := h.req("vm-1", rid(), kCreate, 1)
	r.ExpectKnown = true
	before := h.s.Position()
	d := h.admit(r)
	if !d.IsRefusal(opstore.RefusalExpectKnownUnknown) || d.Refusal.DefiniteNotPerformed() ||
		d.Refusal.Reason() != opstore.ReasonUnknownToAgent {
		t.Fatalf("expect_known for an unknown op_id: %+v", d)
	}
	if _, found := h.lookup(r); found || h.s.Position() != before {
		t.Fatal("an expect_known refusal wrote")
	}
	found := false
	for _, ev := range h.audit {
		if ev.Event == "refused" && ev.Reason == opstore.AuditExpectKnownUnknown && bytes.Equal(ev.Ref.OpID, r.OpID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no expect_known_unknown audit event: %+v", h.audit)
	}
	// A known op_id is answered from its record whatever expect_known says.
	r.ExpectKnown = false
	h.mustAccept(r)
	r.ExpectKnown = true
	if d := h.admit(r); d.Outcome != opstore.OutcomeKnown || d.Acceptance == nil {
		t.Fatalf("expect_known for a known op_id: %+v", d)
	}
}

// TestAcceptance_Known: a re-delivery of an accepted operation reports its
// acceptance again; one that has started does not.
func TestAcceptance_Known(t *testing.T) {
	h, c := gateHarness(t, nil)
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("x"))
	first := h.admit(r).Acceptance
	again := h.admit(r)
	if again.Outcome != opstore.OutcomeKnown || again.Acceptance == nil || again.Acceptance.Position != first.Position {
		t.Fatalf("re-delivery: %+v (first %+v)", again, first)
	}
	// A re-attestation changes the session the next receipt names.
	c2 := session()
	if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: c2, Ledger: h.s.Position(), Reattest: true}); err != nil {
		t.Fatal(err)
	}
	if d := h.admit(r); !bytes.Equal(d.Acceptance.Session, c2) || bytes.Equal(c, c2) {
		t.Fatalf("session after a re-attestation: %+v", d.Acceptance)
	}
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("run: %v", v.State)
	}
	if d := h.admit(r); d.Outcome != opstore.OutcomeKnown || d.Acceptance != nil {
		t.Fatalf("a completed operation reported an acceptance: %+v", d)
	}
}

// TestLedgerGate_ReattestWakesAndRecounts: a re-attestation above the
// pending operations wakes them and they no longer count.
func TestLedgerGate_ReattestWakesAndRecounts(t *testing.T) {
	h, _ := gateHarness(t, nil)
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("x"))
	h.mustAccept(r)
	wake := h.s.LedgerWake()
	if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: h.s.Position(), Reattest: true}); err != nil {
		t.Fatal(err)
	}
	if !closed(wake) || h.s.Backlog().PendingReceipts != 0 {
		t.Fatalf("after the re-attestation: woken %v backlog %+v", closed(wake), h.s.Backlog())
	}
	if v := h.run(r); v.State != opstore.StateCompleted {
		t.Fatalf("run: %v", v.State)
	}
}

// TestLedgerGate_ReenrolForgetsParked: a re-enrolment retires the parked
// operations of its predecessor; they no longer count or hold bytes.
func TestLedgerGate_ReenrolForgetsParked(t *testing.T) {
	h, _ := gateHarness(t, nil)
	r := withRequest(h.req("vm-1", rid(), kCreate, 1), []byte("held"))
	h.mustAccept(r)
	wake := h.s.LedgerWake()
	if err := h.s.Reenrol(context.Background(), opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider,
		InstallID: rid(), StartEpoch: h.start + 1}); err != nil {
		t.Fatal(err)
	}
	if b := h.s.Backlog(); b.PendingReceipts != 0 || b.ParkedRequestBytes != 0 || !closed(wake) {
		t.Fatalf("after the re-enrolment: %+v woken %v", b, closed(wake))
	}
	if v, err := h.s.Execute(t.Context(), ref(r), nil); err != nil || v.State != opstore.StateRetired {
		t.Fatalf("the retired operation: %+v %v", v, err)
	}
}
