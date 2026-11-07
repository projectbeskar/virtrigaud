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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// The re-enrolment freeze: an open operation with no step intent is retired;
// one with an intent becomes OutcomeUnknown and keeps holding its VM; the old
// enrolment's grants are refused; its tombstones stay until the freeze is
// confirmed.
func TestReenrol_FreezeRetiresOrHolds(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	b1, b2 := rid(), rid()
	h.do(h.req("vm1", b1, kCreate, 1))
	h.do(h.req("vm2", b2, kCreate, 1))
	done := h.req("vm1", b1, kReconfig, 1)
	h.do(done)
	withIntent := h.req("vm2", b2, kPowerOn, 1)
	h.mustAccept(withIntent)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterIntent, N: 1}
	_, _ = h.s.Execute(ctx, ref(withIntent), nil)
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	h.open() // continuity and recovery deliberately not run: the op stays Executing with its intent
	if err := h.s.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	noIntent := h.req("vm1", b1, kPowerOn, 1)
	// Admission needs a recovered store; recover without running anything.
	if _, err := h.s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	h.mustAccept(noIntent)
	if v, _ := h.lookup(withIntent); v.State != opstore.StateExecuting {
		t.Fatalf("setup: intent op %v", v.State)
	}

	oldEnrol := h.enrol
	next := opstore.Enrolment{ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	if err := h.s.Reenrol(ctx, opstore.Enrolment{ID: rid(), ProviderUID: h.provider, StartEpoch: h.start}); !errors.Is(err, opstore.ErrEnrolment) {
		t.Fatalf("start epoch reused: %v", err)
	}
	if err := h.s.Reenrol(ctx, next); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.lookup(noIntent); v.State != opstore.StateRetired || v.Reason != opstore.ReasonProviderChanged || !h.tombstoned(noIntent) {
		t.Fatalf("no-intent op: %+v", v)
	}
	if v, _ := h.lookup(withIntent); v.State != opstore.StateOutcomeUnknown {
		t.Fatalf("intent op: %+v", v)
	}
	if h.exec.effects(noIntent.OpID, "power") != 0 {
		t.Fatal("a retired op ran")
	}

	// Old grants are refused; the old tombstones are still readable.
	wantRefusal(t, h.admit(done), opstore.RefusalGrantMismatch)
	if !h.tombstoned(done) {
		t.Fatal("old tombstone dropped before the freeze was confirmed")
	}

	// New enrolment: vm1 is free (its op was retired); vm2 is held.
	h.enrol = next.ID
	h.start = next.StartEpoch
	h.do(h.req("vm1", b1, kPowerOn, 1)) // attempt marks are per enrolment
	wantRefusal(t, h.admit(h.req("vm2", b2, kPowerOff, 1)), opstore.RefusalVMBusy)

	if err := h.s.ConfirmFreeze(ctx, next.ID); !errors.Is(err, opstore.ErrEnrolment) {
		t.Fatalf("confirm on the active enrolment: %v", err)
	}
	if err := h.s.ConfirmFreeze(ctx, oldEnrol); err != nil {
		t.Fatal(err)
	}
	if h.tombstoned(done) {
		t.Fatal("old tombstone kept after the confirmed freeze")
	}
	if ok, _ := h.mem.Exists(witnessPathFor(h, withIntent, 0)); ok {
		t.Fatal("old witness kept after the confirmed freeze")
	}
	// The manager confirmed it holds the frozen operations: vm2 is released.
	h.do(h.req("vm2", b2, kPowerOff, 1))
	h.verifyChain()
}

func TestReenrol_RefusedWhileRunning(t *testing.T) {
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
	err := h.s.Reenrol(context.Background(), opstore.Enrolment{ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1})
	close(h.exec.block)
	<-done
	if !errors.Is(err, opstore.ErrRunning) {
		t.Fatalf("re-enrol with a runner: %v", err)
	}
}

func TestEnrol_OnlyOnce(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	if err := h.s.Enrol(context.Background(), opstore.Enrolment{ID: rid(), ProviderUID: h.provider, StartEpoch: 9}); !errors.Is(err, opstore.ErrEnrolment) {
		t.Fatalf("second enrol: %v", err)
	}
}
