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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// attestedRecord is the store's attestation record as the host view reads
// it.
func attestedRecord(t *testing.T, h *harness) journal.StoreAttestation {
	t.Helper()
	v, err := h.s.HostView(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return v.Attested
}

func wantRecord(t *testing.T, got journal.StoreAttestation, firstSeq uint64, enrolment []byte, recordSeq uint64) {
	t.Helper()
	if !got.EverAttested || got.FirstSeq != firstSeq || !bytes.Equal(got.FirstEnrolmentID, enrolment) || got.RecordSeq != recordSeq {
		t.Fatalf("attestation record %+v, want first seq %d enrolment %x record seq %d", got, firstSeq, enrolment, recordSeq)
	}
}

// The store's first attestation commits its attestation record (one chain
// step, before the session exists); a re-attestation, and the first
// attestation after a restart, commit nothing; the record survives the
// restart unchanged.
func TestAttest_RecordsFirstAttestationOnce(t *testing.T) {
	h, _ := attestHarness(t, true)
	if r := attestedRecord(t, h); r.EverAttested {
		t.Fatalf("a store never attested records %+v", r)
	}
	tip := h.s.Position()
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip})
	if err != nil || res.Quarantined != "" || !res.First || !res.Recorded {
		t.Fatalf("first attestation: %+v %v", res, err)
	}
	after := h.s.Position()
	if after.Seq != tip.Seq+1 {
		t.Fatalf("the first attestation committed up to %d, want exactly one commit (%d)", after.Seq, tip.Seq+1)
	}
	wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, after.Seq)
	if res.Attestation.Mark != tip {
		t.Fatalf("mark %+v, want the attested position %+v", res.Attestation.Mark, tip)
	}

	res, err = h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: after, Reattest: true})
	if err != nil || res.Quarantined != "" || res.Recorded {
		t.Fatalf("re-attestation: %+v %v", res, err)
	}
	if p := h.s.Position(); p != after {
		t.Fatalf("a re-attestation committed: %+v, want %+v", p, after)
	}
	wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, after.Seq)

	h.restart()
	wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, after.Seq)
	at := h.s.Position()
	res, err = h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: at})
	if err != nil || res.Quarantined != "" || !res.First || res.Recorded {
		t.Fatalf("first attestation after a restart: %+v %v", res, err)
	}
	if p := h.s.Position(); p != at {
		t.Fatalf("the first attestation after a restart committed: %+v, want %+v", p, at)
	}
	wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, after.Seq)
	h.verifyChain()
}

// A store that records an attestation is attested before continuity is
// established without committing anything; one that records none gets
// ErrContinuityPending, no session and no commit, and is attested once
// continuity is established.
func TestAttest_RecordNeedsContinuity(t *testing.T) {
	h, _ := attestHarness(t, true)
	_ = h.s.Close()
	h.open()
	tip := h.s.Position()
	_, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip})
	if !errors.Is(err, opstore.ErrContinuityPending) {
		t.Fatalf("first attestation before continuity: %v, want ErrContinuityPending", err)
	}
	if _, ok := h.s.Attestation(); ok {
		t.Fatal("a session exists without the attestation record")
	}
	if st, _ := h.s.Journal().State(); st != journal.StateUnattested || h.s.Position() != tip || attestedRecord(t, h).EverAttested {
		t.Fatalf("the refused attestation changed the store: %v %+v %+v", st, h.s.Position(), attestedRecord(t, h))
	}
	if err := h.s.EstablishContinuity(t.Context()); err != nil {
		t.Fatal(err)
	}
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip})
	if err != nil || !res.First || !res.Recorded {
		t.Fatalf("first attestation after continuity: %+v %v", res, err)
	}
	recorded := h.s.Position()
	wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, recorded.Seq)

	// Attested once: a later start is attested while still unattested,
	// committing nothing.
	_ = h.s.Close()
	h.open()
	res, err = h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: recorded})
	if err != nil || !res.First || res.Recorded {
		t.Fatalf("attestation of a recorded store before continuity: %+v %v", res, err)
	}
	if st, _ := h.s.Journal().State(); st != journal.StateUnattested || h.s.Position() != recorded {
		t.Fatalf("the attestation of a recorded store committed: %v %+v", st, h.s.Position())
	}
}

// A ledger position that fails the checks quarantines a store that records
// no attestation without writing a record.
func TestAttest_RecordNotWrittenOnQuarantine(t *testing.T) {
	h, _ := attestHarness(t, true)
	tip := h.s.Position()
	bad := tip
	bad.Head[0] ^= 0xff
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: bad})
	wantQuarantined(t, h, res, err, journal.LostLedgerMismatch)
	if h.s.Position() != tip || attestedRecord(t, h).EverAttested {
		t.Fatalf("a quarantining attestation wrote the record: %+v %+v", h.s.Position(), attestedRecord(t, h))
	}
}

// A commit of the record that fails fails the attestation: no session, no
// record (the store is quarantined io_error, as for every failed commit).
func TestAttest_RecordCommitFailureFailsAttestation(t *testing.T) {
	failCommit := false
	h := newHarness(t)
	h.opts = func(o *opstore.Options) {
		o.Journal.Hooks.StoreCommitError = func() error {
			if failCommit {
				return errors.New("injected commit failure")
			}
			return nil
		}
	}
	h.fresh()
	tip := h.s.Position()
	failCommit = true
	res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip})
	if err == nil || res.First || res.Recorded {
		t.Fatalf("attestation with a failing record commit: %+v %v", res, err)
	}
	if !errors.Is(err, journal.ErrLost) {
		t.Fatalf("error %v, want a store loss", err)
	}
	if _, ok := h.s.Attestation(); ok {
		t.Fatal("a session exists although the record commit failed")
	}
	if st, r := h.s.Journal().State(); st != journal.StateLost || r != journal.LostIOError {
		t.Fatalf("journal %v{%s}, want lost{io_error}", st, r)
	}
	// The quarantined store answers reads after a restart: the record was
	// never written.
	failCommit = false
	_ = h.s.Close()
	h.mem.Crash()
	h.open()
	if attestedRecord(t, h).EverAttested {
		t.Fatal("the failed commit's record is in the store")
	}
}

// A crash around the record's commit leaves no session; after the restart
// the record is there exactly when the commit reached the store file (the
// one unacknowledged commit is accepted and re-anchored), and the next
// attestation commits nothing more.
func TestAttest_RecordCrashPoints(t *testing.T) {
	for _, p := range []journal.Point{journal.PointAfterStoreCommit, journal.PointAfterAnchor} {
		t.Run(string(p), func(t *testing.T) {
			h, _ := attestHarness(t, true)
			tip := h.s.Position()
			h.crash = &journaltest.Crasher{Point: p, N: 1}
			res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip})
			if !errors.Is(err, journal.ErrCrashed) || res.Recorded {
				t.Fatalf("attestation at the crash: %+v %v", res, err)
			}
			if !h.crash.Fired() {
				t.Fatalf("crash point %s never reached", p)
			}
			if _, ok := h.s.Attestation(); ok {
				t.Fatal("a session exists after a crash in the record commit")
			}
			h.restart()
			after := h.s.Position()
			if after.Seq != tip.Seq+1 {
				t.Fatalf("after the restart the store is at %d, want the record commit %d", after.Seq, tip.Seq+1)
			}
			wantRecord(t, attestedRecord(t, h), tip.Seq, h.enrol, after.Seq)
			res, err = h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: after})
			if err != nil || !res.First || res.Recorded || h.s.Position() != after {
				t.Fatalf("attestation after the restart: %+v %v at %+v", res, err, h.s.Position())
			}
			h.verifyChain()
		})
	}
}

// The record names the active enrolment of the first attestation; a later
// re-enrolment keeps it.
func TestAttest_RecordKeepsFirstEnrolment(t *testing.T) {
	h, _ := attestHarness(t, true)
	tip := h.s.Position()
	if res, err := h.s.Attest(t.Context(), opstore.AttestRequest{Session: session(), Ledger: tip}); err != nil || !res.Recorded {
		t.Fatalf("first attestation: %+v %v", res, err)
	}
	recordSeq := h.s.Position().Seq
	first := h.enrol
	if err := h.s.Reenrol(context.Background(), opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, InstallID: rid(), StartEpoch: h.start + 1}); err != nil {
		t.Fatal(err)
	}
	wantRecord(t, attestedRecord(t, h), tip.Seq, first, recordSeq)
}
