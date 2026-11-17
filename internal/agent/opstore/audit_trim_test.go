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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
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
	// meta gains the persisted audit cursor; nothing else changes.
	for _, b := range []string{"tombstones", "vm_tombstones", "attempts", "ops", "vms", "enrolments"} {
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

// TestAudit_CrashBetweenCommitAndSinkIsReplayed: an event committed when the
// agent stopped before the sink received it is emitted again at the next
// open, marked Replayed, so it is not lost; trimming then may remove it.
func TestAudit_CrashBetweenCommitAndSinkIsReplayed(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	r := h.req("vm-crash", rid(), kCreate, 1)
	h.crash = &journaltest.Crasher{Point: journal.PointAfterStoreCommit, N: 1}
	if _, err := h.s.Admit(ctx, r); err == nil {
		t.Fatal("no crash after the commit")
	}
	accepted := func(replayed bool) int {
		h.auditMu.Lock()
		defer h.auditMu.Unlock()
		n := 0
		for _, ev := range h.audit {
			if ev.Event == "accepted" && bytes.Equal(ev.Ref.OpID, r.OpID) && ev.Replayed == replayed {
				n++
			}
		}
		return n
	}
	if accepted(false) != 0 {
		t.Fatal("the sink got the event although the agent stopped before telling it")
	}
	h.restart()
	if accepted(true) != 1 {
		t.Fatalf("the committed, unemitted event was not replayed at open: %d", accepted(true))
	}
	// The cursor now covers it: a trim persists the cursor first and may
	// remove it; the next open replays nothing at or below the cursor.
	if _, err := h.s.TrimAudit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	cursor, found, err := opstore.PersistedAuditCursor(ctx, h.s)
	if err != nil || !found {
		t.Fatalf("cursor %d %v %v", cursor, found, err)
	}
	keys, err := opstore.AuditKeys(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	if last := opstore.AuditKeySeq(keys[len(keys)-1]); cursor < last {
		t.Fatalf("cursor %d below the last emitted event %d", cursor, last)
	}
	h.auditMu.Lock()
	h.audit = nil
	h.auditMu.Unlock()
	h.restart()
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	for _, ev := range h.audit {
		if ev.Replayed {
			t.Fatalf("an event at or below the persisted cursor was replayed: %+v", ev)
		}
	}
}

// TestTrimAudit_NeverRemovesAboveTheCursor: while a committed transaction's
// events are not emitted yet, the cursor stays below it, it is persisted
// there, and no event at or after it is removed, whatever the bounds.
func TestTrimAudit_NeverRemovesAboveTheCursor(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = 1 }
	h.fresh()
	ctx := context.Background()
	lifecycle(h, 10)
	keys, err := opstore.AuditKeys(ctx, h.s)
	if err != nil || len(keys) < 10 {
		t.Fatalf("%d keys, %v", len(keys), err)
	}
	held := opstore.AuditKeySeq(keys[len(keys)/2])
	release := opstore.HoldAuditPending(h.s, held)
	if _, err := h.s.TrimAudit(ctx, 3); err != nil {
		t.Fatal(err)
	}
	cursor, _, err := opstore.PersistedAuditCursor(ctx, h.s)
	if err != nil || cursor != held-1 {
		t.Fatalf("persisted cursor %d (%v), want %d", cursor, err, held-1)
	}
	after, err := opstore.AuditKeys(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, k := range after {
		kept[string(k)] = true
		if opstore.AuditKeySeq(k) < held {
			t.Fatalf("an event below the held transaction was kept although the window is 1: seq %d", opstore.AuditKeySeq(k))
		}
	}
	for _, k := range keys {
		if opstore.AuditKeySeq(k) >= held && !kept[string(k)] {
			t.Fatalf("event of seq %d removed above the cursor %d", opstore.AuditKeySeq(k), cursor)
		}
	}
	release()
	if _, err := h.s.TrimAudit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if left, _ := opstore.AuditKeys(ctx, h.s); len(left) != 1 {
		t.Fatalf("after the release %d events are kept, want the window of 1", len(left))
	}
}
