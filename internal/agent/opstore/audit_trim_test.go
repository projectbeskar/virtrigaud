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
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// lifecycle runs n create, power and delete cycles and returns the requests.
func lifecycle(h *harness, n int) []opstore.Request {
	var out []opstore.Request
	for i := 0; i < n; i++ {
		b := rid()
		vm := fmt.Sprintf("vm-%d", i)
		for _, r := range []opstore.Request{h.req(vm, b, kCreate, 1), h.req(vm, b, kPowerOn, 1), h.req(vm, b, kDelete, 1)} {
			h.do(r)
			out = append(out, r)
		}
	}
	return out
}

// TestTrimAudit_KeepsTheNewestWindowOnly: retention removes the oldest audit
// events beyond the event bound, in key (commit) order, and nothing else:
// every op_id tombstone, VM tombstone and attempt mark stays, a re-delivery
// is still answered from its tombstone and never runs again, and the store
// verifies after a reopen (the chain commits to the state digest, not to
// the removed events).
func TestTrimAudit_KeepsTheNewestWindowOnly(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = 25 }
	h.fresh()
	ctx := context.Background()
	reqs := lifecycle(h, 20)
	before, err := opstore.AuditKeys(ctx, h.s)
	if err != nil || len(before) <= 25 {
		t.Fatalf("audit events %d, %v", len(before), err)
	}
	usage, err := opstore.BucketUsages(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	n, err := h.s.TrimAudit(ctx, 7)
	if err != nil || n != len(before)-25 {
		t.Fatalf("trimmed %d (%v), want %d", n, err, len(before)-25)
	}
	after, err := opstore.AuditKeys(ctx, h.s)
	if err != nil || len(after) != 25 {
		t.Fatalf("kept %d, %v", len(after), err)
	}
	for i, k := range after {
		if !bytes.Equal(k, before[len(before)-25+i]) {
			t.Fatalf("kept key %d is not among the newest", i)
		}
	}
	usage2, err := opstore.BucketUsages(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"tombstones", "vm_tombstones", "attempts", "ops", "vms", "enrolments", "meta"} {
		if usage2[b] != usage[b] {
			t.Errorf("%s changed: %+v -> %+v", b, usage[b], usage2[b])
		}
	}
	for _, r := range reqs {
		if !h.tombstoned(r) {
			t.Fatalf("%v: tombstone removed by audit retention", r.Kind)
		}
		if d := h.admit(r); d.Outcome != opstore.OutcomeKnown {
			t.Fatalf("re-delivery after audit retention: %+v", d)
		}
	}
	if n, err := h.s.TrimAudit(ctx, 0); err != nil || n != 0 {
		t.Fatalf("a second trim removed %d, %v", n, err)
	}
	h.verifyChain()
	if err := h.s.Journal().Scrub(ctx); err != nil {
		t.Fatalf("scrub after trim: %v", err)
	}
	h.restart()
	h.verifyChain()
	if err := h.s.Journal().Scrub(ctx); err != nil {
		t.Fatalf("scrub after reopen: %v", err)
	}
	for _, r := range reqs {
		if d := h.admit(r); d.Outcome != opstore.OutcomeKnown {
			t.Fatalf("re-delivery after reopen: %+v", d)
		}
	}
}

// TestTrimAudit_ByteBound: the byte bound applies as well; the store keeps
// the newest events whose record bytes fit.
func TestTrimAudit_ByteBound(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainBytes = 2000 }
	h.fresh()
	ctx := context.Background()
	lifecycle(h, 10)
	if _, err := h.s.TrimAudit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	u, err := opstore.BucketUsages(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	if u["audit"].Bytes > 2000 || u["audit"].Keys == 0 {
		t.Fatalf("audit after trim: %+v", u["audit"])
	}
}

// TestTrimAudit_WithinBoundsDoesNothing: with the defaults a small store
// keeps every event.
func TestTrimAudit_WithinBoundsDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	lifecycle(h, 5)
	if n, err := h.s.TrimAudit(context.Background(), 0); err != nil || n != 0 {
		t.Fatalf("trimmed %d, %v", n, err)
	}
}
