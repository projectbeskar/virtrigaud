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
	"sync"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

func wantRefusal(t *testing.T, d opstore.Decision, r opstore.Refusal) {
	t.Helper()
	if !d.IsRefusal(r) {
		t.Fatalf("decision = outcome %v refusal %v, want refusal %v", d.Outcome, d.Refusal, r)
	}
}

// One test per row of the admission table.

func TestAdmit_QuarantinedStoreRefuses(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	h.s.Journal().Quarantine(journal.LostRolledBack, "test")
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 1)), opstore.RefusalQuarantined)
}

func TestAdmit_UnattestedStoreRefuses(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	_ = h.s.Close()
	s := h.open() // continuity not established
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 1)), opstore.RefusalUnattested)
	if err := s.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Continuity established, recovery not yet run: still unattested.
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 1)), opstore.RefusalUnattested)
}

func TestAdmit_EnrolmentAndProviderBinding(t *testing.T) {
	h := newHarness(t)
	h.boot()
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 1)), opstore.RefusalGrantMismatch) // no enrolment
	if err := h.s.Enrol(context.Background(), opstore.Enrolment{ID: h.enrol, ProviderUID: h.provider, StartEpoch: h.start}); err != nil {
		t.Fatal(err)
	}
	r := h.req("vm", rid(), kCreate, 1)
	r.EnrolmentID = rid()
	wantRefusal(t, h.admit(r), opstore.RefusalGrantMismatch)
	r = h.req("vm", rid(), kCreate, 1)
	r.ProviderUID = "another-provider"
	wantRefusal(t, h.admit(r), opstore.RefusalProviderMismatch)
}

func TestAdmit_TombstoneSameIdentityAnswersTerminalNeverReruns(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.do(r)
	if _, err := h.s.Compact(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	d := h.admit(r)
	if d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateCompleted || !d.Op.Tombstoned {
		t.Fatalf("redelivery after compaction: %+v", d)
	}
	if got := h.exec.effects(r.OpID, "start"); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}
}

func TestAdmit_TombstoneOtherIdentityIsConflict(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.do(r)
	if _, err := h.s.Compact(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	other := r
	other.IntentDigest = append([]byte(nil), r.IntentDigest...)
	other.IntentDigest[0] ^= 1
	wantRefusal(t, h.admit(other), opstore.RefusalOpIDConflict)
}

func TestAdmit_RecordSameIdentityIsKnown(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.mustAccept(r)
	r.Actor = "another actor" // not an identity field
	d := h.admit(r)
	if d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateAccepted {
		t.Fatalf("duplicate: %+v", d)
	}
}

func TestAdmit_RecordOtherIdentityIsConflict(t *testing.T) {
	mutations := map[string]func(*opstore.Request){
		"provider-scoped vm": func(r *opstore.Request) { r.VMUID = "other-vm" },
		"binding":            func(r *opstore.Request) { r.BindingID = rid() },
		"kind":               func(r *opstore.Request) { r.Kind = kPowerOn },
		"attempt":            func(r *opstore.Request) { r.Attempt++ },
		"digest":             func(r *opstore.Request) { r.IntentDigest = append([]byte{1}, r.IntentDigest[1:]...) },
		"mint epoch counter": func(r *opstore.Request) { r.MintEpoch.Counter++ },
		"mint epoch holder":  func(r *opstore.Request) { r.MintEpoch.Holder = rid() },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fresh()
			r := h.req("vm", rid(), kCreate, 1)
			h.mustAccept(r)
			m := r
			mutate(&m)
			wantRefusal(t, h.admit(m), opstore.RefusalOpIDConflict)
		})
	}
}

func TestAdmit_MintEpochBeforeEnrolmentStartRefusedOnFirstDelivery(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	r.MintEpoch.Counter = h.start - 1
	wantRefusal(t, h.admit(r), opstore.RefusalGrantMismatch)
	r.MintEpoch.Counter = h.start
	h.mustAccept(r)
}

func TestAdmit_VMBusy(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	h.mustAccept(h.req("vm", b, kPowerOn, 1))
	wantRefusal(t, h.admit(h.req("vm", b, kPowerOff, 1)), opstore.RefusalVMBusy)
}

func TestAdmit_NonCreateWithoutVMRecordIsNotOwned(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	wantRefusal(t, h.admit(h.req("vm", rid(), kPowerOn, 1)), opstore.RefusalVMNotOwned)
}

func TestAdmit_NonCreateWithOtherBindingIsStale(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	h.do(h.req("vm", rid(), kCreate, 1))
	wantRefusal(t, h.admit(h.req("vm", rid(), kPowerOn, 1)), opstore.RefusalVMBindingStale)
}

func TestAdmit_CreateWithOtherBindingIsStale(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	h.do(h.req("vm", rid(), kCreate, 1))
	wantRefusal(t, h.admit(h.req("vm", rid(), kCreate, 2)), opstore.RefusalVMBindingStale)
}

func TestAdmit_CreateAgainstVMTombstoneIsStale(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	h.do(h.req("vm", b, kDelete, 1))
	if _, ok, _ := h.s.VMDeleted(context.Background(), h.enrol, "vm", b); !ok {
		t.Fatal("no VM tombstone after a completed Delete")
	}
	wantRefusal(t, h.admit(h.req("vm", b, kCreate, 2)), opstore.RefusalVMBindingStale)
	// A new placement of the same VM is a Create with a new binding.
	h.mustAccept(h.req("vm", rid(), kCreate, 3))
}

func TestAdmit_AttemptMustIncrease(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	h.do(h.req("vm", b, kPowerOn, 5))
	wantRefusal(t, h.admit(h.req("vm", b, kPowerOn, 5)), opstore.RefusalOpIDConflict)
	wantRefusal(t, h.admit(h.req("vm", b, kPowerOn, 4)), opstore.RefusalOpIDConflict) // reordered n after n+1
	h.do(h.req("vm", b, kPowerOn, 6))
	// Attempts are per kind.
	h.do(h.req("vm", b, kPowerOff, 1))
}

func TestAdmit_StoreFull(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.MaxStoreBytes = 1 }
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	wantRefusal(t, h.admit(r), opstore.RefusalStoreFull)
}

func TestAdmit_Accept(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	before := h.s.Journal().Position()
	d := h.admit(r)
	if d.Outcome != opstore.OutcomeAccepted || d.Op.State != opstore.StateAccepted {
		t.Fatalf("%+v", d)
	}
	if h.s.Journal().Position().Seq != before.Seq+1 {
		t.Fatal("accept is not exactly one chained commit")
	}
}

func TestAdmit_RefusalsWriteNothing(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	before := h.s.Journal().Position()
	h.admit(h.req("vm", rid(), kPowerOn, 1))
	r := h.req("vm", rid(), kCreate, 1)
	r.MintEpoch.Counter = 1
	h.admit(r)
	if h.s.Journal().Position() != before {
		t.Fatal("a refusal committed")
	}
}

// Twelve concurrent deliveries of one operation: one record, one effect.
func TestAdmit_ConcurrentDuplicates(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	var wg sync.WaitGroup
	decisions := make([]opstore.Decision, 12)
	for i := range decisions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := h.s.Admit(context.Background(), r)
			if err != nil {
				t.Error(err)
			}
			decisions[i] = d
			if d.Outcome == opstore.OutcomeAccepted || d.Outcome == opstore.OutcomeKnown {
				_, _ = h.s.Execute(context.Background(), ref(r), nil) // concurrent runners: one wins
			}
		}(i)
	}
	wg.Wait()
	accepted := 0
	for _, d := range decisions {
		switch d.Outcome {
		case opstore.OutcomeAccepted:
			accepted++
		case opstore.OutcomeKnown:
		default:
			t.Fatalf("unexpected decision %+v", d)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d times, want 1", accepted)
	}
	h.run(r)
	if v, _ := h.lookup(r); v.State != opstore.StateCompleted {
		t.Fatalf("state %v", v.State)
	}
	if got := h.exec.effects(r.OpID, "start"); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}
}

// The reason strings equal the provider contract's.
func TestReasonsMatchContract(t *testing.T) {
	pairs := map[string]string{
		opstore.ReasonQuarantined:      contracts.QuarantinedReason,
		opstore.ReasonUnattested:       contracts.UnattestedReason,
		opstore.ReasonProviderMismatch: contracts.ProviderMismatchReason,
		opstore.ReasonGrantMismatch:    contracts.GrantMismatchReason,
		opstore.ReasonOpIDConflict:     contracts.OpIDConflictReason,
		opstore.ReasonVMBusy:           contracts.VMBusyReason,
		opstore.ReasonVMBindingStale:   contracts.VMBindingStaleReason,
		opstore.ReasonVMNotOwned:       contracts.VMNotOwnedReason,
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("reason %q, contract %q", got, want)
		}
	}
}
