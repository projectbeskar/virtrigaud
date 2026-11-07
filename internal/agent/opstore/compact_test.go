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
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Compaction drops the payload only: tombstones, witnesses and VM tombstones
// survive, and a re-delivery is answered without running again.
func TestCompact_KeepsTombstonesWitnessesAndVMTombstones(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	create := h.req("vm", b, kCreate, 1)
	h.do(create)
	power := h.req("vm", b, kPowerOn, 1)
	h.do(power)
	del := h.req("vm", b, kDelete, 1)
	h.do(del)
	n, err := h.s.Compact(context.Background(), 2)
	if err != nil || n != 3 {
		t.Fatalf("compacted %d, err %v", n, err)
	}
	for _, r := range []opstore.Request{create, power, del} {
		if !h.tombstoned(r) {
			t.Fatalf("%v: tombstone dropped", r.Kind)
		}
		v, ok := h.lookup(r)
		if !ok || !v.Tombstoned || v.State != opstore.StateCompleted {
			t.Fatalf("%v: lookup after compaction %+v", r.Kind, v)
		}
		d := h.admit(r)
		if d.Outcome != opstore.OutcomeKnown || d.Op.State != opstore.StateCompleted {
			t.Fatalf("%v: re-delivery %+v", r.Kind, d)
		}
	}
	for path, label := range map[string]string{
		witnessPathFor(h, create, 1): "create start",
		witnessPathFor(h, power, 0):  "power",
		witnessPathFor(h, del, 1):    "delete remove",
	} {
		if ok, _ := h.mem.Exists(path); !ok {
			t.Fatalf("witness of %s removed by compaction", label)
		}
	}
	if _, ok, _ := h.s.VMDeleted(context.Background(), h.enrol, "vm", b); !ok {
		t.Fatal("VM tombstone dropped by compaction")
	}
	if h.exec.effects(power.OpID, "power") != 1 || h.exec.effects(create.OpID, "start") != 1 {
		t.Fatal("an operation ran again")
	}
	h.verifyChain()
}

// Compaction never drops a record whose tombstone is missing, nor one that
// still holds its VM.
func TestCompact_KeepsOpenAndUnknownRecords(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	b := rid()
	h.do(h.req("vm", b, kCreate, 1))
	open := h.req("vm", b, kPowerOn, 1)
	h.mustAccept(open)
	if n, _ := h.s.Compact(context.Background(), 0); n != 1 {
		t.Fatalf("compacted %d, want only the finished Create", n)
	}
	if v, _ := h.lookup(open); v.Tombstoned || v.State != opstore.StateAccepted {
		t.Fatalf("open record compacted: %+v", v)
	}
}

func TestCompact_ManyOperations(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	var reqs []opstore.Request
	for i := 0; i < 40; i++ {
		r := h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1)
		h.do(r)
		reqs = append(reqs, r)
	}
	n, err := h.s.Compact(context.Background(), 7)
	if err != nil || n != 40 {
		t.Fatalf("compacted %d, err %v", n, err)
	}
	h.restart()
	for _, r := range reqs {
		if d := h.admit(r); d.Outcome != opstore.OutcomeKnown {
			t.Fatalf("%+v", d)
		}
	}
}
