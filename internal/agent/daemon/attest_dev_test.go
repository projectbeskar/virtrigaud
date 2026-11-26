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
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/attest/attesttest"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// chainPosition is the chain position a signed Hello answer names.
func (r *running) chainPosition() []byte {
	r.h.t.Helper()
	a := r.hello()
	if a == nil {
		r.h.t.Fatal("no signed answer")
	}
	return a.GetContext().GetChainPosition()
}

// awaitAwaiting waits until the agent awaits its attestation.
func (r *running) awaitAwaiting() {
	r.h.t.Helper()
	select {
	case <-r.awaiting:
	case s := <-r.started:
		r.h.t.Fatalf("the agent ended its start-up (%v) without an attestation\n%s", s, r.h.logs.text())
	case <-time.After(10 * time.Second):
		r.h.t.Fatalf("the agent never awaited its attestation\n%s", r.h.logs.text())
	}
}

// TestAwaitsAttestationBeforeRecovery: a development build that restarts on
// an enrolled store commits nothing and runs no recovery until it is
// attested; it says so in its status and refuses every mutation with the
// awaiting cause. The attestation lets the start-up finish.
func TestAwaitsAttestationBeforeRecovery(t *testing.T) {
	h := newHost(t)
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("first start: %v", state)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	var recovered atomic.Bool
	h.hooks.BeforeRecover = func(context.Context) { recovered.Store(true) }
	h.noAttest = true
	h.notes = &notifications{}
	r = h.start()
	r.awaitAwaiting()
	before := r.chainPosition()
	time.Sleep(100 * time.Millisecond) // several floor ticks: none may commit
	if after := r.chainPosition(); !bytes.Equal(before, after) {
		t.Fatalf("the store committed before the attestation: %x then %x", before, after)
	}
	if recovered.Load() {
		t.Fatal("recovery ran before the attestation")
	}
	if a := r.hello(); a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_UNATTESTED {
		t.Fatalf("state while awaiting: %v", a.GetContext().GetAgentState())
	}
	if code, reason := r.create(); code != "Unavailable" || reason != opstore.ReasonUnattested {
		t.Fatalf("Create while awaiting = %s %s", code, reason)
	}
	if h.notes.readyStarted() {
		t.Fatal("READY=1 for the end of the start-up before the attestation")
	}
	awaitingSent := false
	for _, n := range h.notes.snapshot() {
		if strings.HasPrefix(n, systemd.NotifyReady) && strings.Contains(n, "awaiting attestation") {
			awaitingSent = true
		}
	}
	if !awaitingSent {
		t.Fatalf("no READY=1 with the awaiting status: %v", h.notes.snapshot())
	}
	if !strings.Contains(h.logs.text(), server.CauseAwaitingAttestation) {
		t.Fatalf("the awaiting cause is not logged:\n%s", h.logs.text())
	}

	go r.attestNow()
	if a := <-r.attested; a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED {
		t.Fatalf("attestation: %v", a)
	}
	select {
	case s := <-r.started:
		if s != providerv1.AgentState_AGENT_STATE_READY {
			t.Fatalf("state after the attestation: %v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the start-up did not finish after the attestation\n%s", h.logs.text())
	}
	if !recovered.Load() {
		t.Fatal("recovery did not run after the attestation")
	}
	if !h.notes.readyStarted() {
		t.Fatal("no READY=1 after the attestation")
	}
}

// TestAttestationThatQuarantinesEndsTheStartUp: an attestation at a position
// beyond the store's chain quarantines it, and the start-up ends as a
// quarantine; the quarantine survives a restart.
func TestAttestationThatQuarantinesEndsTheStartUp(t *testing.T) {
	h := newHost(t)
	h.noAttest = true
	r := h.start()
	r.awaitAwaiting()

	a := attestAt(t, r, func(l *providerv1.LedgerPosition) { l.Seq += 10 })
	if a.GetOutcome() != providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_QUARANTINED ||
		a.GetOutcomeReason() != string(journal.LostLedgerAhead) {
		t.Fatalf("attestation: %v", a)
	}
	select {
	case s := <-r.started:
		if s != providerv1.AgentState_AGENT_STATE_QUARANTINED {
			t.Fatalf("state: %v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the start-up did not end\n%s", h.logs.text())
	}
	// A plain answer reads rolled_back; a ledger-aware one the reason.
	if got := r.hello().GetContext().GetStateReason(); got != string(journal.LostRolledBack) {
		t.Fatalf("Hello state reason %q", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	h.noAttest = false
	if _, s := h.run(); s != providerv1.AgentState_AGENT_STATE_QUARANTINED {
		t.Fatalf("restart state: %v", s)
	}
}

// attestAt runs a challenge and an attestation at the agent's own position
// changed by edit.
func attestAt(t *testing.T, r *running, edit func(*providerv1.LedgerPosition)) *providerv1.AuthorityAnswer {
	t.Helper()
	ha := providerv1.NewHostAuthorityClient(r.conn())
	n := bytes.Repeat([]byte{0x41}, 16)
	resp, err := ha.Challenge(callCtx(t), &providerv1.ChallengeRequest{TargetHostId: trusttest.HostName, AnswerNonce: n,
		Purpose: providerv1.ChallengePurpose_CHALLENGE_PURPOSE_ATTEST})
	if err != nil {
		t.Fatal(err)
	}
	ch := &providerv1.ChallengeAnswer{}
	if err := proto.Unmarshal(resp.GetHostAnswer().GetBody(), ch); err != nil {
		t.Fatal(err)
	}
	pos := ch.GetContext().GetChainPosition()
	var p journal.Position
	if err := p.UnmarshalBinary(pos); err != nil {
		t.Fatal(err)
	}
	l := &providerv1.LedgerPosition{StoreId: p.StoreID[:], Seq: p.Seq, Head: p.Head[:]}
	edit(l)
	c := ch.GetContext()
	g, err := r.h.grants.AttestGrant(attesttestTarget(c), ch.GetChallengeNonce(), l)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := ha.Attest(callCtx(t), &providerv1.AttestRequest{TargetHostId: trusttest.HostName, AnswerNonce: n, Grant: g})
	if err != nil {
		t.Fatal(err)
	}
	a := &providerv1.AuthorityAnswer{}
	if err := proto.Unmarshal(ar.GetHostAnswer().GetBody(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// attesttestTarget is the grant target an answer context names.
func attesttestTarget(c *providerv1.AnswerContext) attesttest.Target {
	return attesttest.Target{ProviderUID: c.GetProviderUid(), HostID: trusttest.HostName, InstallID: c.GetInstallId(),
		EnrolmentID: c.GetEnrolmentId()}
}
