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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

const (
	// testDiscardChunk and discardOps give a predecessor of 10 finished
	// operations (an op record, a tombstone and an attempt mark each): about
	// 30 records, removed in 8 chunks.
	testDiscardChunk = 4
	discardOps       = 10
)

// discardStore builds a store whose predecessor enrolment holds discardOps
// finished operations, re-enrols it, and returns the predecessor's id and
// operations; h then makes requests under the new enrolment.
func discardStore(t *testing.T, chunk int) (*harness, []byte, []opstore.Request) {
	t.Helper()
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.DiscardChunk = chunk }
	h.fresh()
	var ops []opstore.Request
	for i := 0; i < discardOps; i++ {
		r := h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1)
		h.do(r)
		require.True(t, h.tombstoned(r))
		ops = append(ops, r)
	}
	prev := h.enrol
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.NoError(t, h.s.Reenrol(context.Background(), next))
	h.enrol, h.start = next.ID, next.StartEpoch
	return h, prev, ops
}

// rawRecords counts the records of enrolment prev that are still in the
// store, read directly from the journal, and reports whether its enrolment
// record and the discard marker exist.
func rawRecords(t *testing.T, h *harness, prev []byte) (records int, enrolment, marker bool) {
	t.Helper()
	require.NoError(t, h.s.Journal().View(context.Background(), func(tx *journal.Tx) error {
		for _, b := range []string{"ops", "tombstones", "attempts"} {
			if err := tx.ForEach(b, prev, func(_, _ []byte) error {
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
		enrolment, marker = e != nil, m != nil
		return err
	}))
	return records, enrolment, marker
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

// A predecessor larger than one chunk is removed in several transactions.
// A crash between them leaves it retired: every operation reads as discarded
// although some of its records remain, and the new enrolment works. The
// next compaction removes the rest, with the enrolment record and the
// marker.
func TestConfirmFreeze_ChunksAndCrashResumeByCompaction(t *testing.T) {
	h, prev, ops := discardStore(t, testDiscardChunk)
	before, _, _ := rawRecords(t, h, prev)
	require.Greater(t, before, 3*testDiscardChunk, "the predecessor must need several chunks")

	// Crash after the third chunk: the op records and some tombstones are
	// gone, the other tombstones and the attempt marks remain.
	h.crash = &journaltest.Crasher{Point: opstore.PointMidDiscard, N: 3}
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), prev), journal.ErrCrashed)
	h.crash = nil
	h.restart()

	left, enrolment, marker := rawRecords(t, h, prev)
	assert.Equal(t, before-3*testDiscardChunk, left, "each chunk removes at most the chunk's records")
	assert.True(t, enrolment, "the enrolment record goes last")
	assert.True(t, marker, "the marker names the discard to finish")
	wantDiscarded(t, h, ops)

	fresh := h.req("vm-new", rid(), kCreate, 1)
	h.do(fresh)
	v, ok := h.lookup(fresh)
	require.True(t, ok)
	assert.Equal(t, opstore.StateCompleted, v.State)

	n, err := h.s.Compact(context.Background(), opstore.DefaultCompactBatch)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, left, "compaction reports the records the discard removed")
	left, enrolment, marker = rawRecords(t, h, prev)
	assert.Zero(t, left)
	assert.False(t, enrolment)
	assert.False(t, marker)
	wantDiscarded(t, h, ops)
	assert.True(t, h.tombstoned(fresh), "the new enrolment's tombstone is kept")
	h.verifyChain()
}

// A relayed confirmation of the same predecessor after a crash finishes the
// discard.
func TestConfirmFreeze_CrashResumeByRelay(t *testing.T) {
	h, prev, ops := discardStore(t, testDiscardChunk)
	h.crash = &journaltest.Crasher{Point: opstore.PointMidDiscard, N: 1}
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), prev), journal.ErrCrashed)
	h.crash = nil
	h.restart()
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), prev))
	left, enrolment, marker := rawRecords(t, h, prev)
	assert.Zero(t, left)
	assert.False(t, enrolment)
	assert.False(t, marker)
	wantDiscarded(t, h, ops)
	// Confirming it again is refused as not frozen (it is gone).
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), prev), opstore.ErrEnrolment)
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
}

// A predecessor that fits in one chunk is discarded in exactly one
// transaction, as before chunking.
func TestConfirmFreeze_SmallPredecessorOneTransaction(t *testing.T) {
	h, prev, ops := discardStore(t, 0)
	before := h.s.Position().Seq
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), prev))
	assert.Equal(t, before+1, h.s.Position().Seq, "one transaction")
	left, enrolment, marker := rawRecords(t, h, prev)
	assert.Zero(t, left)
	assert.False(t, enrolment)
	assert.False(t, marker)
	wantDiscarded(t, h, ops)
}
