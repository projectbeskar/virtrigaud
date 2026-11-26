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
	"errors"
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// TestOriginEvidence: a fresh store's first enrolment is its origin; a
// re-enrolment counts and keeps the origin.
func TestOriginEvidence(t *testing.T) {
	h := newHarness(t)
	h.boot()
	if v, err := h.s.HostView(t.Context(), 0); err != nil || v.OriginEnrolmentID != nil || v.EnrolmentActivations != 0 {
		t.Fatalf("never enrolled: %+v %v", v, err)
	}
	if err := h.s.Enrol(t.Context(), opstore.Enrolment{HostID: "host-1", ID: h.enrol, ProviderUID: h.provider, InstallID: rid(), StartEpoch: h.start}); err != nil {
		t.Fatal(err)
	}
	v, err := h.s.HostView(t.Context(), 0)
	if err != nil || !bytes.Equal(v.OriginEnrolmentID, h.enrol) || v.EnrolmentActivations != 1 {
		t.Fatalf("after the first enrolment: origin %x count %d (%v)", v.OriginEnrolmentID, v.EnrolmentActivations, err)
	}
	next := rid()
	if err := h.s.Reenrol(t.Context(), opstore.Enrolment{HostID: "host-1", ID: next, ProviderUID: h.provider, InstallID: rid(), StartEpoch: h.start + 1}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	v, err = h.s.HostView(t.Context(), 0)
	if err != nil || !bytes.Equal(v.OriginEnrolmentID, h.enrol) || v.EnrolmentActivations != 2 || !bytes.Equal(v.Enrolment.ID, next) {
		t.Fatalf("after a re-enrolment: origin %x count %d active %+v (%v)", v.OriginEnrolmentID, v.EnrolmentActivations, v.Enrolment, err)
	}
}

// attestHarness is an enrolled store with some history and small checkpoint
// intervals, and the positions it passed through.
func attestHarness(t *testing.T, retention bool) (*harness, []journal.Position) {
	t.Helper()
	h := newHarness(t)
	h.opts = func(o *opstore.Options) {
		o.Journal.CheckpointEvery = 4
		o.Journal.LedgerRetention = retention
	}
	h.fresh()
	var seen []journal.Position
	for i := 0; i < 12; i++ {
		h.do(h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1))
		seen = append(seen, h.s.Position())
	}
	return h, seen
}

func session() []byte { return rid() }

func wantQuarantined(t *testing.T, h *harness, res opstore.AttestResult, err error, want journal.LostReason) {
	t.Helper()
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if res.Quarantined != want {
		t.Fatalf("quarantined %q, want %q", res.Quarantined, want)
	}
	if st, r := h.s.Journal().State(); st != journal.StateLost || r != want {
		t.Fatalf("journal %v{%s}, want lost{%s}", st, r, want)
	}
	if _, ok := h.s.Attestation(); ok {
		t.Fatal("a quarantining attestation set a session")
	}
}

// headAt is the store's head at seq, from a chain entry or a checkpoint.
func headAt(t *testing.T, h *harness, seq uint64) ([32]byte, bool) {
	t.Helper()
	v, err := h.s.HostView(t.Context(), seq)
	if err != nil {
		t.Fatal(err)
	}
	var head [32]byte
	if len(v.ContinuityHead) != 32 {
		return head, false
	}
	copy(head[:], v.ContinuityHead)
	return head, true
}

func TestAttest_FirstAndReattest(t *testing.T) {
	h, seen := attestHarness(t, true)
	if _, ok := h.s.Attestation(); ok {
		t.Fatal("attested before any attestation")
	}
	tip := h.s.Position()
	if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip, Reattest: true}); !errors.Is(err, opstore.ErrAttestState) {
		t.Fatalf("re-attestation while awaiting: %v", err)
	}
	c1 := session()
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: c1, Ledger: tip})
	if err != nil || res.Quarantined != "" || !res.First {
		t.Fatalf("first attestation: %+v %v", res, err)
	}
	a, ok := h.s.Attestation()
	if !ok || !bytes.Equal(a.Session, c1) || a.Mark != tip {
		t.Fatalf("attestation %+v", a)
	}
	if got := h.s.Journal().RetentionFloor(); got != tip.Seq {
		t.Fatalf("retention floor %d, want %d", got, tip.Seq)
	}
	if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip}); !errors.Is(err, opstore.ErrAttestState) {
		t.Fatalf("a second first attestation: %v", err)
	}

	// A re-attestation at a lower position (an older ledger) replaces the
	// session and keeps the higher mark.
	older := seen[len(seen)-3]
	c2 := session()
	res, err = h.s.Attest(t.Context(), opstore.AttestRequest{Session: c2, Ledger: older, Reattest: true})
	if err != nil || res.Quarantined != "" || res.First {
		t.Fatalf("re-attestation: %+v %v", res, err)
	}
	if a, _ := h.s.Attestation(); !bytes.Equal(a.Session, c2) || a.Mark != tip {
		t.Fatalf("after a lower re-attestation: %+v, want session %x mark %+v", a, c2, tip)
	}
	// A higher one raises it.
	h.do(h.req("vm-more", rid(), kCreate, 1))
	now := h.s.Position()
	if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: now, Reattest: true}); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.s.Attestation(); a.Mark != now || h.s.Journal().RetentionFloor() != now.Seq {
		t.Fatalf("after a higher re-attestation: %+v floor %d", a, h.s.Journal().RetentionFloor())
	}
	// A restart forgets the session.
	h.restart()
	if _, ok := h.s.Attestation(); ok {
		t.Fatal("the session survived a restart")
	}
}

func TestAttest_Quarantines(t *testing.T) {
	flip := func(b [32]byte) [32]byte { b[0] ^= 1; return b }
	t.Run("another store", func(t *testing.T) {
		h, _ := attestHarness(t, false)
		l := h.s.Position()
		l.StoreID[0] ^= 1
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: l})
		wantQuarantined(t, h, res, err, journal.LostLedgerStoreMismatch)
	})
	t.Run("ahead of the chain", func(t *testing.T) {
		h, _ := attestHarness(t, false)
		l := h.s.Position()
		l.Seq++
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: l})
		wantQuarantined(t, h, res, err, journal.LostLedgerAhead)
	})
	t.Run("another head", func(t *testing.T) {
		h, _ := attestHarness(t, false)
		l := h.s.Position()
		l.Head = flip(l.Head)
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: l})
		wantQuarantined(t, h, res, err, journal.LostLedgerMismatch)
	})
	t.Run("pruned head without a checkpoint", func(t *testing.T) {
		h, seen := attestHarness(t, false)
		old := prunedPosition(t, h, seen)
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: old})
		wantQuarantined(t, h, res, err, journal.LostLedgerUnverifiable)
	})
	t.Run("pruned head with a checkpoint the store does not hold", func(t *testing.T) {
		h, seen := attestHarness(t, false)
		old := prunedPosition(t, h, seen)
		cp := journal.Position{Seq: old.Seq - 1, Head: old.Head}
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: old, Checkpoint: &cp})
		wantQuarantined(t, h, res, err, journal.LostLedgerUnverifiable)
	})
	t.Run("pruned head with another checkpoint head", func(t *testing.T) {
		h, seen := attestHarness(t, false)
		old := prunedPosition(t, h, seen)
		cpSeq := old.Seq - old.Seq%4
		head, ok := headAt(t, h, cpSeq)
		if !ok {
			t.Fatalf("no checkpoint at %d", cpSeq)
		}
		cp := journal.Position{Seq: cpSeq, Head: flip(head)}
		res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: old, Checkpoint: &cp})
		wantQuarantined(t, h, res, err, journal.LostLedgerMismatch)
	})
	t.Run("a lost store refuses", func(t *testing.T) {
		h, _ := attestHarness(t, false)
		h.s.Journal().Quarantine(journal.LostRolledBack, "test")
		if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: h.s.Position()}); !errors.Is(err, journal.ErrLost) {
			t.Fatalf("attest on a lost store: %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		h, _ := attestHarness(t, false)
		if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: []byte{1}, Ledger: h.s.Position()}); err == nil {
			t.Fatal("a short session was accepted")
		}
		if _, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session()}); err == nil {
			t.Fatal("a zero ledger position was accepted")
		}
	})
}

// TestAttest_CheckpointPath: a position whose head was pruned passes with a
// held checkpoint below it whose head matches.
func TestAttest_CheckpointPath(t *testing.T) {
	h, seen := attestHarness(t, false)
	old := prunedPosition(t, h, seen)
	cpSeq := old.Seq - old.Seq%4
	head, ok := headAt(t, h, cpSeq)
	if !ok {
		t.Fatalf("no checkpoint at %d", cpSeq)
	}
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: old,
		Checkpoint: &journal.Position{Seq: cpSeq, Head: head}})
	if err != nil || res.Quarantined != "" {
		t.Fatalf("checkpoint path: %+v %v", res, err)
	}
	if a, ok := h.s.Attestation(); !ok || a.Mark != old {
		t.Fatalf("attestation %+v", a)
	}
}

// prunedPosition returns a position the store passed whose head it no
// longer holds (not a checkpoint position).
func prunedPosition(t *testing.T, h *harness, seen []journal.Position) journal.Position {
	t.Helper()
	for _, p := range seen {
		if p.Seq%4 == 0 {
			continue
		}
		if _, ok := headAt(t, h, p.Seq); !ok {
			return p
		}
	}
	t.Fatal("no pruned position")
	return journal.Position{}
}
