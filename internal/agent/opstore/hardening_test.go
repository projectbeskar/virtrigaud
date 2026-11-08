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
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The definite not-performed classification is the provider contract's.
func TestRefusalClassificationMatchesContract(t *testing.T) {
	for r := opstore.RefusalQuarantined; r <= opstore.RefusalStoreFull; r++ {
		reason := r.Reason()
		if reason == "" {
			if r != opstore.RefusalStoreFull {
				t.Errorf("%v has no reason", r)
			}
			continue
		}
		if got, want := r.DefiniteNotPerformed(), contracts.IsDefiniteNotPerformedReason(reason); got != want {
			t.Errorf("%s: definite %v, contract %v", reason, got, want)
		}
	}
	if opstore.ReasonUnknownToAgent != contracts.UnknownToAgentReason {
		t.Error("UNKNOWN_TO_AGENT differs from the contract")
	}
}

// During a re-enrolment, with no enrolment active, a known op_id is still
// answered from its record.
func TestAdmit_KnownOpAnsweredWithNoActiveEnrolment(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	h.do(r)
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	h.crash = &journaltest.Crasher{Point: opstore.PointMidReenrolment, N: 1}
	if err := h.s.Reenrol(context.Background(), next); !errors.Is(err, journal.ErrCrashed) {
		t.Fatal(err)
	}
	h.restart()
	if d := h.admit(r); d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateCompleted {
		t.Fatalf("known op with no active enrolment: %+v", d)
	}
}

func TestAdmit_CreateOnExistingPlacementIsStale(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	d := h.admit(h.req("vm", b, kCreate, 2))
	if !d.IsRefusal(opstore.RefusalVMBindingStale) || d.Refusal.DefiniteNotPerformed() {
		t.Fatalf("%+v", d)
	}
}

func TestAdmit_BoundedFields(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	r := h.req("vm", rid(), kCreate, 1)
	r.Actor = strings.Repeat("a", 1000)
	if _, err := h.s.Admit(context.Background(), r); !errors.Is(err, opstore.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

// VM tombstones outlive a confirmed freeze.
func TestVMTombstone_KeptAcrossReenrolment(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	h.do(h.req("vm", b, kDelete, 1))
	old := h.enrol
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	if err := h.s.Reenrol(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ConfirmFreeze(ctx, old); err != nil {
		t.Fatal(err)
	}
	h.enrol, h.start = next.ID, next.StartEpoch
	if _, ok, _ := h.s.VMDeleted(ctx, "vm", b); !ok {
		t.Fatal("VM tombstone discarded with the enrolment")
	}
	wantRefusal(t, h.admit(h.req("vm", b, kCreate, 1)), opstore.RefusalVMBindingStale)
}

// The store-full threshold uses the live size: compaction clears it.
func TestAdmit_StoreFullClearsAfterCompaction(t *testing.T) {
	h := newHarness(t)
	var limit int64 = 1 << 40
	h.opts = func(o *opstore.Options) { o.MaxStoreBytes = limit }
	h.fresh()
	for i := 0; i < 300; i++ {
		h.do(h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1))
	}
	live := h.s.Journal().LiveSize()
	limit = live - 1
	_ = h.s.Close()
	h.boot()
	wantRefusal(t, h.admit(h.req("vm-new", rid(), kCreate, 1)), opstore.RefusalStoreFull)
	if _, err := h.s.Compact(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	h.mustAccept(h.req("vm-new", rid(), kCreate, 1))
}

func TestRecover_RunsOnce(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	if _, err := h.s.Recover(context.Background()); !errors.Is(err, opstore.ErrAlreadyRecovered) {
		t.Fatalf("second recover: %v", err)
	}
}

// A witness of an operation the store has no record of (the store lost
// records its witnesses outlived) quarantines the store at recovery.
func TestRecover_UnknownWitnessQuarantines(t *testing.T) {
	cases := map[string]func(h *harness) string{
		"unknown operation": func(h *harness) string {
			return filepath.Join(h.dir, "witness", fmt.Sprintf("%x", h.enrol), fmt.Sprintf("%x.1", rid()))
		},
		"unknown enrolment": func(h *harness) string {
			return filepath.Join(h.dir, "witness", fmt.Sprintf("%x", rid()), fmt.Sprintf("%x.0", rid()))
		},
		"malformed name": func(h *harness) string {
			return filepath.Join(h.dir, "witness", fmt.Sprintf("%x", h.enrol), "not-a-witness")
		},
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fresh()
			h.do(h.req("vm", rid(), kCreate, 1)) // a legitimate witness exists
			h.mem.Put(path(h), nil)
			_ = h.s.Close()
			s := h.open()
			if err := s.EstablishContinuity(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Recover(context.Background()); !errors.Is(err, journal.ErrLost) {
				t.Fatalf("recover: %v", err)
			}
			if st, reason := s.Journal().State(); st != journal.StateLost || reason != journal.LostInconsistentStore {
				t.Fatalf("%v{%s}", st, reason)
			}
		})
	}
}

// The store marker binds the store to its enrolment.
func TestMarker_BindingChecks(t *testing.T) {
	t.Run("another enrolment quarantines", func(t *testing.T) {
		h := newHarness(t)
		h.fresh()
		if err := h.s.Journal().BindMarker(journal.MarkerBinding{HostID: "host-1", EnrolmentID: rid()}); err != nil {
			t.Fatal(err)
		}
		_ = h.s.Close()
		s := h.open()
		if st, reason := s.Journal().State(); st != journal.StateLost || reason != journal.LostInconsistentStore {
			t.Fatalf("%v{%s}", st, reason)
		}
	})
	t.Run("another host quarantines", func(t *testing.T) {
		h := newHarness(t)
		h.fresh()
		if err := h.s.Journal().BindMarker(journal.MarkerBinding{HostID: "host-2", EnrolmentID: h.enrol}); err != nil {
			t.Fatal(err)
		}
		_ = h.s.Close()
		if st, _ := h.open().Journal().State(); st != journal.StateLost {
			t.Fatal("marker of another host accepted")
		}
	})
	t.Run("crash after the activation commit is completed", func(t *testing.T) {
		h := newHarness(t)
		h.fresh()
		if err := h.s.Journal().BindMarker(journal.MarkerBinding{HostID: "host-1", PendingEnrolmentID: h.enrol}); err != nil {
			t.Fatal(err)
		}
		_ = h.s.Close()
		h.mem.Crash()
		s := h.open()
		if st, r := s.Journal().State(); st != journal.StateUnattested {
			t.Fatalf("%v{%s}", st, r)
		}
		if mk := s.Journal().Marker(); string(mk.EnrolmentID) != string(h.enrol) || len(mk.PendingEnrolmentID) != 0 {
			t.Fatalf("marker not completed: %+v", mk)
		}
	})
	t.Run("crash before the first activation commit", func(t *testing.T) {
		h := newHarness(t)
		h.boot()
		if err := h.s.Journal().BindMarker(journal.MarkerBinding{HostID: "host-1", PendingEnrolmentID: rid()}); err != nil {
			t.Fatal(err)
		}
		_ = h.s.Close()
		if st, r := h.open().Journal().State(); st != journal.StateUnattested {
			t.Fatalf("%v{%s}", st, r)
		}
	})
}
