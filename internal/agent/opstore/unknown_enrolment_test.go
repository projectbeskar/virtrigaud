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
	"os"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

func wantUnknown(t *testing.T, d opstore.Decision) {
	t.Helper()
	if !d.IsRefusal(opstore.RefusalUnknownToAgent) || d.Refusal.DefiniteNotPerformed() {
		t.Fatalf("decision %+v, want a non-definite UNKNOWN_TO_AGENT", d)
	}
}

// A fresh store made for a new enrolment (for example after the old store was
// lost) cannot say whether an op of the old enrolment ran: never definite.
func TestAdmit_OldEnrolmentOnFreshStoreIsUnknown(t *testing.T) {
	h := newHarness(t)
	oldEnrol := rid()
	h.boot()
	if err := h.s.Enrol(context.Background(), opstore.Enrolment{HostID: "host-1", ID: h.enrol, ProviderUID: h.provider,
		StartEpoch: h.start, Predecessors: [][]byte{oldEnrol}}); err != nil {
		t.Fatal(err)
	}
	r := h.req("vm", rid(), kCreate, 1)
	r.EnrolmentID = oldEnrol
	wantUnknown(t, h.admit(r))
	// A predecessor is never activated in this store.
	if err := h.s.Reenrol(context.Background(), opstore.Enrolment{HostID: "host-1", ID: oldEnrol, ProviderUID: h.provider,
		StartEpoch: h.start + 1}); !errors.Is(err, opstore.ErrEnrolment) {
		t.Fatalf("activating a predecessor: %v", err)
	}
	// Without the predecessor list the answer is the same.
	r.EnrolmentID = rid()
	wantUnknown(t, h.admit(r))
}

// After the whole state set is wiped, an op that ran under the old enrolment
// is answered UNKNOWN_TO_AGENT by the new store, never as not performed.
func TestAdmit_AfterWipeOldOpIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ran := h.req("vm", rid(), kCreate, 1)
	h.do(ran)
	_ = h.s.Close()
	if err := os.RemoveAll(h.dir); err != nil {
		t.Fatal(err)
	}
	h.mem = journaltest.NewMemFS()
	oldEnrol := h.enrol
	h.enrol = rid()
	h.fresh()
	ran.EnrolmentID = oldEnrol
	wantUnknown(t, h.admit(ran))
}

// Still definite: a new op_id for the active enrolment minted before it
// started, and one for a frozen enrolment the store holds.
func TestAdmit_DefiniteRefusalsRemain(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	early := h.req("vm", rid(), kCreate, 1)
	early.MintEpoch.Counter = h.start - 1
	if d := h.admit(early); !d.IsRefusal(opstore.RefusalGrantMismatch) || !d.Refusal.DefiniteNotPerformed() {
		t.Fatalf("early mint epoch: %+v", d)
	}
	frozen := h.enrol
	if err := h.s.Reenrol(context.Background(), opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider,
		StartEpoch: h.start + 1}); err != nil {
		t.Fatal(err)
	}
	r := h.req("vm", rid(), kCreate, 1)
	r.EnrolmentID = frozen
	if d := h.admit(r); !d.IsRefusal(opstore.RefusalGrantMismatch) || !d.Refusal.DefiniteNotPerformed() {
		t.Fatalf("frozen enrolment: %+v", d)
	}
}
