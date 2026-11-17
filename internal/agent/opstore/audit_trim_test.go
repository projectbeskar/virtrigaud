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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// lifecycle runs n create, power and delete cycles and returns the requests.
func lifecycle(h *harness, n int) []opstore.Request { return lifecycleAs(h, "vm", n) }

// lifecycleAs is lifecycle on VMs named prefix-<i>.
func lifecycleAs(h *harness, prefix string, n int) []opstore.Request {
	var out []opstore.Request
	for i := 0; i < n; i++ {
		b := rid()
		vm := fmt.Sprintf("%s-%d", prefix, i)
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

// takeAudit returns the events emitted since the last call, and clears the
// record.
func takeAudit(h *harness) []opstore.AuditEvent {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	out := h.audit
	h.audit = nil
	return out
}

// TestReplayAudit_CappedWithOneSummary: with more events above the cursor
// than the retention window's event count (here a store that never
// persisted a cursor), the replay emits exactly the newest window, in commit
// order, with their original sequences, and one summary of the skipped
// older ones.
func TestReplayAudit_CappedWithOneSummary(t *testing.T) {
	const window = 10
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = window }
	h.fresh()
	lifecycle(h, 20)
	fresh := takeAudit(h)
	keys, err := opstore.AuditKeys(context.Background(), h.s)
	if err != nil || len(keys) <= window {
		t.Fatalf("%d keys, %v", len(keys), err)
	}
	for _, ev := range fresh {
		if ev.Seq == 0 && ev.Event != "known" && ev.Event != "refused" {
			t.Fatalf("a stored event was emitted without its sequence: %+v", ev)
		}
	}
	h.restart()
	var replayed []opstore.AuditEvent
	summaries := 0
	for _, ev := range takeAudit(h) {
		switch {
		case ev.Event == opstore.EventReplaySkipped:
			summaries++
			if !strings.Contains(ev.Reason, fmt.Sprintf("%d audit events", len(keys)-window)) {
				t.Fatalf("summary: %s", ev.Reason)
			}
		case ev.Replayed:
			replayed = append(replayed, ev)
		}
	}
	if summaries != 1 || len(replayed) != window {
		t.Fatalf("%d summaries, %d replayed, want 1 and %d", summaries, len(replayed), window)
	}
	for i, ev := range replayed {
		if want := opstore.AuditKeySeq(keys[len(keys)-window+i]); ev.Seq != want {
			t.Fatalf("replayed %d has seq %d, want the newest window's %d", i, ev.Seq, want)
		}
	}
}

// TestReplayAudit_NotAgainOnceTheCursorIsPersisted: what the replay emitted
// is not emitted again after the cursor is persisted (the agent does it once
// it is ready), even before any trim.
func TestReplayAudit_NotAgainOnceTheCursorIsPersisted(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	lifecycle(h, 3)
	h.restart()
	if n := countReplayed(takeAudit(h)); n == 0 {
		t.Fatal("a store without a cursor replayed nothing")
	}
	if err := h.s.PersistAuditCursor(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if n := countReplayed(takeAudit(h)); n != 0 {
		t.Fatalf("%d events replayed again after the cursor was persisted", n)
	}
}

// TestReplayAudit_OffReplaysNothing: without ReplayAudit (a release build)
// nothing is replayed.
func TestReplayAudit_OffReplaysNothing(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.ReplayAudit = false }
	h.fresh()
	lifecycle(h, 3)
	h.restart()
	if n := countReplayed(takeAudit(h)); n != 0 {
		t.Fatalf("%d events replayed with ReplayAudit off", n)
	}
}

func countReplayed(evs []opstore.AuditEvent) int {
	n := 0
	for _, ev := range evs {
		if ev.Replayed {
			n++
		}
	}
	return n
}

// TestReplayAudit_HoldsNothingPerEvent: the replay streams; neither it nor
// its count of events to skip collects events (no append, no make), so its
// memory does not grow with the number of events above the cursor.
func TestReplayAudit_HoldsNothingPerEvent(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "audit_trim.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "replayAudit" && fn.Name.Name != "replaySkip") {
			continue
		}
		found++
		ast.Inspect(fn, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "append" || id.Name == "make") {
					t.Errorf("%s calls %s at %s: the replay must not collect events", fn.Name.Name, id.Name, fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	if found != 2 {
		t.Fatalf("found %d of replayAudit and replaySkip", found)
	}
}

// replayFailures returns the EventAuditReplayFailed events of evs.
func replayFailures(evs []opstore.AuditEvent) []opstore.AuditEvent {
	var out []opstore.AuditEvent
	for _, ev := range evs {
		if ev.Event == opstore.EventAuditReplayFailed {
			out = append(out, ev)
		}
	}
	return out
}

// TestReplayAudit_FailedRecordPinsTheCursor: a replay that stops at an
// event it cannot decode reports it once, and for the rest of the process
// the audit cursor never passes the last transaction whose events were all
// emitted, however many new transactions follow, and nothing above it is
// trimmed.
func TestReplayAudit_FailedRecordPinsTheCursor(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = 1 }
	h.fresh()
	ctx := context.Background()
	lifecycle(h, 6)
	keys, err := opstore.AuditKeys(ctx, h.s)
	if err != nil {
		t.Fatal(err)
	}
	bad := opstore.AuditKeySeq(keys[len(keys)-1]) // the newest: within the one-event replay window
	if err := opstore.PutRawAudit(ctx, h.s, bad, 1<<20, []byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	takeAudit(h)
	h.restart()
	fails := replayFailures(takeAudit(h))
	if len(fails) != 1 || fails[0].Seq >= bad {
		t.Fatalf("replay failures %+v, want one with a ceiling below seq %d", fails, bad)
	}
	ceiling := fails[0].Seq
	lifecycleAs(h, "later", 4) // new transactions raise the high-water mark
	if err := h.s.PersistAuditCursor(ctx); err != nil {
		t.Fatal(err)
	}
	if cursor, _, _ := opstore.PersistedAuditCursor(ctx, h.s); cursor > ceiling {
		t.Fatalf("cursor %d persisted past the ceiling %d", cursor, ceiling)
	}
	if _, err := h.s.TrimAudit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	left, _ := opstore.AuditKeys(ctx, h.s)
	for _, k := range keys {
		if opstore.AuditKeySeq(k) > ceiling && !containsKey(left, k) {
			t.Fatalf("event of seq %d trimmed above the ceiling %d", opstore.AuditKeySeq(k), ceiling)
		}
	}
}

// TestReplayAudit_UnreadableCursorFreezes: a cursor that cannot be read
// (here, one of the wrong size) is reported once, and in that process the
// cursor is never written and nothing is trimmed.
func TestReplayAudit_UnreadableCursorFreezes(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.AuditRetainEvents = 1 }
	h.fresh()
	ctx := context.Background()
	lifecycle(h, 3)
	if err := opstore.PutRawAuditCursor(ctx, h.s, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	takeAudit(h)
	h.restart()
	if fails := replayFailures(takeAudit(h)); len(fails) != 1 {
		t.Fatalf("replay failures %+v, want one", fails)
	}
	before, _ := opstore.AuditKeys(ctx, h.s)
	lifecycleAs(h, "later", 2)
	if err := h.s.PersistAuditCursor(ctx); err == nil {
		t.Fatal("the cursor was written although it could not be read")
	}
	if n, err := h.s.TrimAudit(ctx, 0); err != nil || n != 0 {
		t.Fatalf("trimmed %d (%v) with a frozen cursor", n, err)
	}
	after, _ := opstore.AuditKeys(ctx, h.s)
	if len(after) < len(before) {
		t.Fatal("events were removed")
	}
}

// TestReplayAudit_FailedCountPinsTheCursorAtItsStart: a replay whose count
// fails emits nothing and pins the cursor where it was.
func TestReplayAudit_FailedCountPinsTheCursorAtItsStart(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	lifecycle(h, 3)
	if err := h.s.PersistAuditCursor(ctx); err != nil {
		t.Fatal(err)
	}
	start, _, _ := opstore.PersistedAuditCursor(ctx, h.s)
	lifecycleAs(h, "second", 2)
	restore := opstore.SetReplaySkipFault(errors.New("count failed"))
	takeAudit(h)
	h.restart()
	restore()
	if fails := replayFailures(takeAudit(h)); len(fails) != 1 || fails[0].Seq != start {
		t.Fatalf("replay failures %+v, want one pinned at %d", fails, start)
	}
	lifecycleAs(h, "third", 2)
	if err := h.s.PersistAuditCursor(ctx); err != nil {
		t.Fatal(err)
	}
	if cursor, _, _ := opstore.PersistedAuditCursor(ctx, h.s); cursor != start {
		t.Fatalf("cursor %d, want it pinned at %d", cursor, start)
	}
}

func containsKey(keys [][]byte, k []byte) bool {
	for _, x := range keys {
		if bytes.Equal(x, k) {
			return true
		}
	}
	return false
}

// TestNonStoredEventsAreLimited: re-deliveries answered "known" beyond the
// limit in one window are not emitted, a summary with their count is
// emitted at the first one of the next window, and stored events are never
// limited.
func TestNonStoredEventsAreLimited(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	opstore.SetNonStoredClock(h.s, func() time.Time { return now })
	r := h.req("vm-1", rid(), kCreate, 1)
	h.do(r)
	takeAudit(h)
	const extra = 7
	for i := 0; i < opstore.NonStoredBurst+extra; i++ {
		if d := h.admit(r); d.Outcome != opstore.OutcomeKnown {
			t.Fatalf("%+v", d)
		}
	}
	known := 0
	for _, ev := range takeAudit(h) {
		if ev.Event == "known" {
			known++
		}
	}
	if known != opstore.NonStoredBurst {
		t.Fatalf("%d known events emitted in one window, want %d", known, opstore.NonStoredBurst)
	}
	now = now.Add(2 * time.Second)
	h.admit(r)
	evs := takeAudit(h)
	if len(evs) != 2 || evs[0].Event != opstore.EventNonStoredSuppressed || !strings.Contains(evs[0].Reason, fmt.Sprintf("%d ", extra)) ||
		evs[1].Event != "known" {
		t.Fatalf("next window: %+v", evs)
	}
	// Stored events are not limited: a burst of operations emits all of them.
	lifecycleAs(h, "burst", opstore.NonStoredBurst)
	stored := 0
	for _, ev := range takeAudit(h) {
		if ev.Seq != 0 {
			stored++
		}
	}
	if stored < 3*opstore.NonStoredBurst {
		t.Fatalf("only %d stored events emitted", stored)
	}
	_ = ctx
}

// TestNonStoredFloodThatStopsIsFlushed: when a flood of re-deliveries stops,
// the store's flush summarises the last window once it has closed, naming
// the kinds and the actor.
func TestNonStoredFloodThatStopsIsFlushed(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	now := time.Unix(1_700_000_000, 0)
	opstore.SetNonStoredClock(h.s, func() time.Time { return now })
	r := h.req("vm-1", rid(), kCreate, 1)
	h.do(r)
	for i := 0; i < opstore.NonStoredBurst+5; i++ {
		h.admit(r)
	}
	takeAudit(h)
	h.s.FlushNonStored(false)
	if evs := takeAudit(h); len(evs) != 0 {
		t.Fatalf("flushed before the window closed: %+v", evs)
	}
	now = now.Add(2 * time.Second)
	h.s.FlushNonStored(false)
	evs := takeAudit(h)
	if len(evs) != 1 || evs[0].Event != opstore.EventNonStoredSuppressed || !strings.Contains(evs[0].Reason, "known=5") ||
		evs[0].Actor != r.Actor {
		t.Fatalf("flush after the flood stopped: %+v", evs)
	}
}
