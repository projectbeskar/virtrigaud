//go:build virtrigaud_dev

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

package daemon_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// storeAttestation reads the store's attestation record from a signed
// GetHostInfo answer that asks for ledger version 2, as the manager decodes
// it, and returns the answer's enrolment.
func (r *running) storeAttestation() (hostidentity.StoreAttestation, []byte) {
	r.h.t.Helper()
	info, err := providerv1.NewProviderClient(r.conn()).GetHostInfo(callCtx(r.h.t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{0x52}, 16), LedgerVersion: 2})
	if err != nil {
		r.h.t.Fatal(err)
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(info.GetHostAnswer().GetBody(), a); err != nil {
		r.h.t.Fatal(err)
	}
	rec, err := hostidentity.StoreAttestationOf(a.GetLedger())
	if err != nil {
		r.h.t.Fatalf("the manager refuses the ledger state: %v", err)
	}
	return rec, a.GetContext().GetEnrolmentId()
}

// TestFirstAttestationAfterRestartRecordsTheStore: an enrolled store that
// was never attested and restarts awaits its attestation without continuity;
// its first attestation has the start-up goroutine prepare the store, then
// commits the attestation record before it is applied, and the start-up
// finishes. The record is reported from then on and survives a restart
// unchanged.
func TestFirstAttestationAfterRestartRecordsTheStore(t *testing.T) {
	h := newHost(t)
	h.noAttest = true
	r := h.start()
	r.awaitAwaiting()
	if rec, _ := r.storeAttestation(); !rec.Reported || rec.EverAttested {
		t.Fatalf("a store never attested: %+v", rec)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	r = h.start()
	r.awaitAwaiting()
	if rec, _ := r.storeAttestation(); !rec.Reported || rec.EverAttested {
		t.Fatalf("after a restart, never attested: %+v", rec)
	}
	before := r.chainPosition()
	go r.attestNow()
	if a := <-r.attested; a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED {
		t.Fatalf("attestation: %v\n%s", a, h.logs.text())
	}
	select {
	case s := <-r.started:
		if s != providerv1.AgentState_AGENT_STATE_READY {
			t.Fatalf("state after the attestation: %v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the start-up did not finish after the attestation\n%s", h.logs.text())
	}
	if !strings.Contains(h.logs.text(), "the store records no attestation") {
		t.Fatalf("the store was not prepared by the attestation:\n%s", h.logs.text())
	}
	rec, enrolment := r.storeAttestation()
	if !rec.EverAttested || rec.FirstSeq == 0 || rec.RecordSeq <= rec.FirstSeq || !bytes.Equal(rec.FirstEnrolmentID, enrolment) {
		t.Fatalf("after the first attestation: %+v (enrolment %x)", rec, enrolment)
	}
	if bytes.Equal(before, r.chainPosition()) {
		t.Fatal("the first attestation committed nothing")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	h.noAttest = false
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("restart: %v", state)
	}
	if again, _ := r.storeAttestation(); again.EverAttested != rec.EverAttested || again.FirstSeq != rec.FirstSeq ||
		again.RecordSeq != rec.RecordSeq || !bytes.Equal(again.FirstEnrolmentID, rec.FirstEnrolmentID) {
		t.Fatalf("after a restart: %+v, want %+v", again, rec)
	}
}
