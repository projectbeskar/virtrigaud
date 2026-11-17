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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

const (
	// testDiscardChunk and discardOps give a predecessor of 10 finished
	// operations (an op record, a tombstone and an attempt mark each) and a
	// panic count: about 31 records, removed in 8 chunks.
	testDiscardChunk = 4
	discardOps       = 10
)

// discardStore builds a store whose active enrolment holds discardOps
// finished operations and a panic count, re-enrols it, and returns the
// predecessor's id and operations; h then makes requests under the new
// enrolment.
func discardStore(t *testing.T, chunk int) (*harness, []byte, []opstore.Request) {
	t.Helper()
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.DiscardChunk = chunk }
	h.fresh()
	ops := finishOps(t, h, "vm")
	prev := reenrolNext(t, h)
	return h, prev, ops
}

// finishOps runs discardOps operations to completion under h's enrolment
// and records a panic count under it.
func finishOps(t *testing.T, h *harness, prefix string) []opstore.Request {
	t.Helper()
	var ops []opstore.Request
	for i := 0; i < discardOps; i++ {
		r := h.req(fmt.Sprintf("%s-%d", prefix, i), rid(), kCreate, 1)
		h.do(r)
		require.True(t, h.tombstoned(r))
		ops = append(ops, r)
	}
	_, err := h.s.RecordPanic(context.Background(), opstore.OpRef{EnrolmentID: h.enrol, OpID: rid()})
	require.NoError(t, err)
	return ops
}

// reenrolNext re-enrols h with a new enrolment and returns the previous one.
func reenrolNext(t *testing.T, h *harness) []byte {
	t.Helper()
	prev := h.enrol
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.NoError(t, h.s.Reenrol(context.Background(), next))
	h.enrol, h.start = next.ID, next.StartEpoch
	return prev
}

// rawRecords counts the records of enrolment prev that are still in the
// store, read directly from the journal (op records, tombstones, attempt
// marks, panic counts), and reports whether its enrolment record exists and
// whether the discard marker names it.
func rawRecords(t *testing.T, h *harness, prev []byte) (records int, enrolment, pending bool) {
	t.Helper()
	require.NoError(t, h.s.Journal().View(context.Background(), func(tx *journal.Tx) error {
		for _, b := range []struct {
			bucket string
			prefix []byte
		}{{"ops", prev}, {"tombstones", prev}, {"attempts", prev}, {"meta", append([]byte("panic/"), prev...)}} {
			if err := tx.ForEach(b.bucket, b.prefix, func(_, _ []byte) error {
				records++
				return nil
			}); err != nil {
				return err
			}
		}
		e, err := tx.Get("enrolments", prev)
		if err != nil {
			return err
		}
		m, err := tx.Get("meta", []byte("discard"))
		for i := 0; i+16 <= len(m); i += 16 {
			pending = pending || bytes.Equal(m[i:i+16], prev)
		}
		enrolment = e != nil
		return err
	}))
	return records, enrolment, pending
}

// wantGone checks that nothing of prev is left and the store reports no
// discard in progress.
func wantGone(t *testing.T, h *harness, prev []byte) {
	t.Helper()
	left, enrolment, pending := rawRecords(t, h, prev)
	assert.Zero(t, left, "records left")
	assert.False(t, enrolment, "enrolment record left")
	assert.False(t, pending, "still named by the marker")
}

// wantDiscarded checks that every operation of the predecessor reads as
// discarded: refused UNKNOWN_TO_AGENT (never as definitely not performed),
// not found, no tombstone, not held.
func wantDiscarded(t *testing.T, h *harness, ops []opstore.Request) {
	t.Helper()
	ctx := context.Background()
	for _, r := range ops {
		d := h.admit(r)
		assert.True(t, d.IsRefusal(opstore.RefusalUnknownToAgent), "re-delivery of a discarded op: %+v", d)
		_, found := h.lookup(r)
		assert.False(t, found, "a discarded op is found")
		tomb, err := h.s.HasTombstone(ctx, ref(r))
		require.NoError(t, err)
		assert.False(t, tomb, "a discarded op has a tombstone")
		v, err := h.s.AnswerOp(ctx, ref(r), 0)
		require.NoError(t, err)
		assert.False(t, v.Found, "a discarded op is answered as found")
		assert.False(t, v.EnrolmentHeld, "a discarded enrolment is answered as held")
	}
}

// ConfirmFreeze commits one transaction and returns: the predecessor is
// retired and every operation of it reads as discarded, though records
// remain. Compaction removes the rest in chunks; a crash between them
// leaves the discard pending across a restart, and the next compaction
// finishes it.
func TestConfirmFreeze_OneTransactionThenCompaction(t *testing.T) {
	h, prev, ops := discardStore(t, testDiscardChunk)
	before, _, _ := rawRecords(t, h, prev)
	require.Greater(t, before, 4*testDiscardChunk, "the predecessor must need several chunks")
	seq := h.s.Position().Seq

	require.NoError(t, h.s.ConfirmFreeze(context.Background(), prev))
	assert.Equal(t, seq+1, h.s.Position().Seq, "ConfirmFreeze commits one transaction")
	left, enrolment, pending := rawRecords(t, h, prev)
	assert.Equal(t, before-testDiscardChunk, left, "the first transaction removes one chunk")
	assert.True(t, enrolment, "the enrolment record goes last")
	assert.True(t, pending, "the marker names the discard to finish")
	assert.True(t, h.s.Discarding())
	wantDiscarded(t, h, ops)

	// A confirmation of the same enrolment changes nothing.
	seq = h.s.Position().Seq
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), prev))
	assert.Equal(t, seq, h.s.Position().Seq, "a repeated confirmation writes nothing")

	// Crash after two more chunks.
	h.crash = &journaltest.Crasher{Point: opstore.PointMidDiscard, N: 2}
	_, err := h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	require.ErrorIs(t, err, journal.ErrCrashed)
	h.crash = nil
	h.restart()
	assert.True(t, h.s.Discarding(), "the pending discard is known after a restart")
	left, enrolment, pending = rawRecords(t, h, prev)
	assert.Equal(t, before-3*testDiscardChunk, left, "each transaction removes at most one chunk")
	assert.True(t, enrolment)
	assert.True(t, pending)
	wantDiscarded(t, h, ops)

	fresh := h.req("vm-new", rid(), kCreate, 1)
	h.do(fresh)
	v, ok := h.lookup(fresh)
	require.True(t, ok)
	assert.Equal(t, opstore.StateCompleted, v.State)

	n, err := h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, left, "compaction reports the records the discard removed")
	wantGone(t, h, prev)
	assert.False(t, h.s.Discarding())
	wantDiscarded(t, h, ops)
	assert.True(t, h.tombstoned(fresh), "the new enrolment's tombstone is kept")
	h.restart()
	assert.False(t, h.s.Discarding(), "no discard is pending after a restart")
	h.verifyChain()
}

// A crash right after the first discard transaction, before the witness
// directory is removed, does not quarantine the store at the next start:
// the predecessor is retired, so its witnesses are removed, not taken for
// witnesses of operations the store has no record of.
func TestConfirmFreeze_CrashBeforeWitnessRemoval(t *testing.T) {
	h, prev, ops := discardStore(t, testDiscardChunk)
	ok, err := h.mem.Exists(witnessPathFor(h, ops[0], 1))
	require.NoError(t, err)
	require.True(t, ok, "the predecessor has witnesses")
	h.crash = &journaltest.Crasher{Point: journal.PointAfterStoreCommit, N: 1}
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), prev), journal.ErrCrashed)
	h.crash = nil
	h.restart()
	assert.False(t, h.s.Lost(), "the store is not quarantined")
	ok, err = h.mem.Exists(witnessPathFor(h, ops[0], 1))
	require.NoError(t, err)
	assert.False(t, ok, "the retired enrolment's witnesses are removed at start")
	wantDiscarded(t, h, ops)
	_, err = h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	require.NoError(t, err)
	wantGone(t, h, prev)
}

// A predecessor that fits in one chunk is discarded in exactly one
// transaction, and no discard is left pending.
func TestConfirmFreeze_SmallPredecessorOneTransaction(t *testing.T) {
	h, prev, ops := discardStore(t, 0)
	before := h.s.Position().Seq
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), prev))
	assert.Equal(t, before+1, h.s.Position().Seq, "one transaction")
	wantGone(t, h, prev)
	assert.False(t, h.s.Discarding())
	wantDiscarded(t, h, ops)
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), prev), opstore.ErrEnrolment, "gone: not frozen any more")
}

// A second predecessor confirmed while the first one's records are still
// being removed is retired at once too; compaction removes both.
func TestConfirmFreeze_SecondWhileOnePending(t *testing.T) {
	h, first, firstOps := discardStore(t, testDiscardChunk)
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), first))
	secondOps := finishOps(t, h, "vm2")
	second := reenrolNext(t, h)
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), second))
	_, _, p1 := rawRecords(t, h, first)
	_, _, p2 := rawRecords(t, h, second)
	assert.True(t, p1 && p2, "both are pending")
	wantDiscarded(t, h, firstOps)
	wantDiscarded(t, h, secondOps)
	_, err := h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	require.NoError(t, err)
	wantGone(t, h, first)
	wantGone(t, h, second)
	assert.False(t, h.s.Discarding())
}

// The VMs the predecessor's operations held are released by the first
// transaction, before its records are removed.
func TestConfirmFreeze_ReleasesHeldVMsFirst(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.DiscardChunk = testDiscardChunk }
	h.fresh()
	ctx := context.Background()
	held := h.req("vm-held", rid(), kCreate, 1)
	h.mustAccept(held)
	h.crash = &journaltest.Crasher{Point: opstore.PointAfterIntent, N: 1}
	_, _ = h.s.Execute(ctx, ref(held), nil)
	require.True(t, h.crash.Fired())
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	h.open()
	require.NoError(t, h.s.EstablishContinuity(ctx))
	_, err := h.s.Recover(ctx) // the runnable list is deliberately not run
	require.NoError(t, err)
	finishOps(t, h, "vm")
	prev := reenrolNext(t, h)
	v, ok := h.lookup(held)
	require.True(t, ok)
	require.Equal(t, opstore.StateOutcomeUnknown, v.State, "the frozen op holds its VM")

	power := h.req("vm-held", held.BindingID, kPowerOn, 1)
	require.True(t, h.admit(power).IsRefusal(opstore.RefusalVMBusy), "held before the confirmation")
	require.NoError(t, h.s.ConfirmFreeze(ctx, prev))
	_, _, pending := rawRecords(t, h, prev)
	require.True(t, pending, "the discard needs more chunks")
	d := h.admit(power)
	assert.Equal(t, opstore.OutcomeAccepted, d.Outcome, "released by the first transaction: %+v", d)
}

// ConfirmFreeze waits for a discard transaction already running only as
// long as its context allows.
func TestConfirmFreeze_WaitIsBoundedByContext(t *testing.T) {
	h, prev, _ := discardStore(t, testDiscardChunk)
	opstore.HoldDiscard(h.s)
	defer opstore.ReleaseDiscard(h.s)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.s.ConfirmFreeze(ctx, prev)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
	_, _, pending := rawRecords(t, h, prev)
	assert.False(t, pending, "nothing was started")
}

// A discard marker naming an enrolment that is not retired (damage: only a
// confirmation writes the marker, and it retires the enrolment first) never
// makes compaction remove that enrolment's records: the store is
// quarantined at once.
func TestCompact_MarkerOfANonRetiredEnrolmentRemovesNothing(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	ops := finishOps(t, h, "vm")
	before, _, _ := rawRecords(t, h, h.enrol)
	_, err := h.s.Journal().Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put("meta", []byte("discard"), append([]byte(nil), h.enrol...))
	})
	require.NoError(t, err)
	_, err = h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	var lost *journal.LostError
	require.ErrorAs(t, err, &lost, "compaction refuses the marker")
	assert.True(t, h.s.Lost(), "the store is quarantined")
	after, enrolment, _ := rawRecords(t, h, h.enrol)
	assert.Equal(t, before, after, "nothing of the active enrolment was removed")
	assert.True(t, enrolment)
	for _, r := range ops {
		assert.True(t, h.tombstoned(r))
	}
}

// At most MaxPendingDiscards discards are pending; a further confirmation
// is refused with ErrTooManyDiscards (reported by ConfirmFrozen, not
// dropped) until compaction finishes them, and then succeeds.
func TestConfirmFreeze_TooManyPending(t *testing.T) {
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.DiscardChunk = testDiscardChunk }
	h.fresh()
	ctx := context.Background()
	var last []byte
	for i := 0; i <= opstore.MaxPendingDiscards; i++ {
		finishOps(t, h, fmt.Sprintf("vm%d", i))
		last = reenrolNext(t, h)
		if i < opstore.MaxPendingDiscards {
			require.NoError(t, h.s.ConfirmFreeze(ctx, last))
		}
	}
	err := h.s.ConfirmFreeze(ctx, last)
	require.ErrorIs(t, err, opstore.ErrTooManyDiscards)
	_, err = h.s.ConfirmFrozen(ctx, h.enrol, [][]byte{last})
	require.ErrorIs(t, err, opstore.ErrTooManyDiscards, "the relay reports the refusal")
	_, err = h.s.Compact(ctx, opstore.DefaultCompactBatch)
	require.NoError(t, err)
	assert.False(t, h.s.Discarding())
	require.NoError(t, h.s.ConfirmFreeze(ctx, last))
}
