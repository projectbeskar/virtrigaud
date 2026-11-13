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
	testFreezeChunk = 3
	// chunkedIntentOps and chunkedPlainOps give 13 open operations: more than
	// four chunks of testFreezeChunk.
	chunkedIntentOps = 3
	chunkedPlainOps  = 10
)

// chunkedOps is a store with many open operations of the active enrolment:
// chunkedIntentOps with a durable step intent (frozen as OutcomeUnknown) and
// chunkedPlainOps with none (retired).
type chunkedOps struct {
	intent, plain []opstore.Request
}

func (c chunkedOps) all() []opstore.Request {
	return append(append([]opstore.Request(nil), c.intent...), c.plain...)
}

// openChunkedStore builds a store with chunkedIntentOps+chunkedPlainOps open
// operations and a freeze chunk of testFreezeChunk. Operations with an intent
// are left Executing by crashing after the intent and restarting without
// running the runnable list.
func openChunkedStore(t *testing.T) (*harness, chunkedOps) {
	t.Helper()
	h := newHarness(t)
	h.opts = func(o *opstore.Options) { o.FreezeChunk = testFreezeChunk }
	h.fresh()
	ctx := context.Background()
	var ops chunkedOps
	for i := 0; i < chunkedIntentOps; i++ {
		r := h.req(fmt.Sprintf("vm-intent-%d", i), rid(), kCreate, 1)
		h.mustAccept(r)
		h.crash = &journaltest.Crasher{Point: opstore.PointAfterIntent, N: 1}
		_, _ = h.s.Execute(ctx, ref(r), nil)
		require.True(t, h.crash.Fired())
		_ = h.s.Close()
		h.mem.Crash()
		h.crash = nil
		h.open()
		require.NoError(t, h.s.EstablishContinuity(ctx))
		_, err := h.s.Recover(ctx) // the runnable list is deliberately not run
		require.NoError(t, err)
		ops.intent = append(ops.intent, r)
	}
	for i := 0; i < chunkedPlainOps; i++ {
		r := h.req(fmt.Sprintf("vm-plain-%d", i), rid(), kCreate, 1)
		h.mustAccept(r)
		ops.plain = append(ops.plain, r)
	}
	return h, ops
}

// wantFrozenOnce checks every operation's end state and that none was frozen
// twice: at most one audit event with the provider_changed reason per
// operation, and exactly one when exact (the sink is told after a commit
// returns, so a crash inside a chunk's commit loses that chunk's events).
func wantFrozenOnce(t *testing.T, h *harness, ops chunkedOps, exact bool) {
	t.Helper()
	for _, r := range ops.intent {
		v, ok := h.lookup(r)
		require.True(t, ok)
		assert.Equal(t, opstore.StateOutcomeUnknown, v.State, "op with an intent")
		assert.Equal(t, opstore.ReasonProviderChanged, v.Reason)
	}
	for _, r := range ops.plain {
		v, ok := h.lookup(r)
		require.True(t, ok)
		assert.Equal(t, opstore.StateRetired, v.State, "op with no intent")
		assert.Equal(t, opstore.ReasonProviderChanged, v.Reason)
		assert.True(t, h.tombstoned(r))
	}
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	count := map[string]int{}
	for _, ev := range h.audit {
		if ev.Reason == opstore.ReasonProviderChanged && (ev.Event == "frozen" || ev.Event == "terminal") {
			count[ev.Ref.String()]++
		}
	}
	for _, r := range ops.all() {
		if exact {
			assert.Equal(t, 1, count[ref(r).String()], "freeze events of %s", ref(r))
		} else {
			assert.LessOrEqual(t, count[ref(r).String()], 1, "freeze events of %s", ref(r))
		}
	}
}

// The freeze of many open operations commits in chunks of at most
// FreezeChunk: one transaction per chunk plus the activation, however many
// operations were open.
func TestReenrol_FreezeIsChunked(t *testing.T) {
	h, ops := openChunkedStore(t)
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	before := h.s.Position().Seq
	require.NoError(t, h.s.Reenrol(context.Background(), next))
	total := chunkedIntentOps + chunkedPlainOps
	chunks := (total + testFreezeChunk - 1) / testFreezeChunk
	assert.Equal(t, before+uint64(chunks)+1, h.s.Position().Seq, "%d chunks and the activation", chunks)
	wantFrozenOnce(t, h, ops, true)
	v, err := h.s.HostView(context.Background(), 0)
	require.NoError(t, err)
	require.NotNil(t, v.Enrolment)
	assert.Equal(t, next.ID, v.Enrolment.ID)
	h.verifyChain()
}

// With a default chunk, a freeze of fewer operations than a chunk is one
// transaction, like the unchunked freeze it replaces.
func TestReenrol_SmallFreezeIsOneTransaction(t *testing.T) {
	h := newHarness(t)
	h.fresh()
	for i := 0; i < 5; i++ {
		h.mustAccept(h.req(fmt.Sprintf("vm-%d", i), rid(), kCreate, 1))
	}
	before := h.s.Position().Seq
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.NoError(t, h.s.Reenrol(context.Background(), next))
	assert.Equal(t, before+2, h.s.Position().Seq, "one freeze transaction and the activation")
}

// A crash between chunks (or in a chunk's commit) leaves a freeze-in-progress
// marker. The next start finishes the freeze in recovery, then Reenrol
// activates; every operation is frozen exactly once, and the new enrolment is
// not active before the freeze is complete.
func TestReenrol_CrashBetweenChunksResumesAtRecovery(t *testing.T) {
	points := []struct {
		point journal.Point
		ns    []int
	}{
		{opstore.PointMidFreeze, []int{1, 2, 3, 4}},
		{journal.PointAfterStoreCommit, []int{1, 2, 3, 4, 5}},
		{journal.PointAfterAnchor, []int{1, 3, 5}},
		{opstore.PointMidReenrolment, []int{1}},
	}
	for _, p := range points {
		for _, n := range p.ns {
			t.Run(fmt.Sprintf("%s#%d", p.point, n), func(t *testing.T) {
				h, ops := openChunkedStore(t)
				ctx := context.Background()
				next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
				h.crash = &journaltest.Crasher{Point: p.point, N: n}
				err := h.s.Reenrol(ctx, next)
				require.ErrorIs(t, err, journal.ErrCrashed)
				h.restart() // continuity and recovery
				v, err := h.s.HostView(ctx, 0)
				require.NoError(t, err)
				assert.Nil(t, v.Enrolment, "the new enrolment is not active before Reenrol")
				wantRefusal(t, h.admit(h.req("vm-new", rid(), kCreate, 1)), opstore.RefusalGrantMismatch)
				for _, r := range ops.all() {
					got, ok := h.lookup(r)
					require.True(t, ok)
					assert.NotEqual(t, opstore.StateAccepted, got.State, "recovery finished the freeze")
					assert.NotEqual(t, opstore.StateExecuting, got.State, "recovery finished the freeze")
				}
				require.NoError(t, h.s.Reenrol(ctx, next))
				wantFrozenOnce(t, h, ops, false)
				h.verifyChain()
			})
		}
	}
}

// Reenrol alone finishes a freeze that a crash interrupted, even when the
// store was not recovered since.
func TestReenrol_ResumesAnInterruptedFreeze(t *testing.T) {
	h, ops := openChunkedStore(t)
	ctx := context.Background()
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	h.crash = &journaltest.Crasher{Point: opstore.PointMidFreeze, N: 2}
	require.ErrorIs(t, h.s.Reenrol(ctx, next), journal.ErrCrashed)
	_ = h.s.Close()
	h.mem.Crash()
	h.crash = nil
	h.open()
	require.NoError(t, h.s.EstablishContinuity(ctx)) // no Recover
	// Some operations are frozen, the rest are still open.
	frozen, open := 0, 0
	for _, r := range ops.all() {
		v, _ := h.lookup(r)
		if v.State == opstore.StateAccepted || v.State == opstore.StateExecuting {
			open++
		} else {
			frozen++
		}
	}
	assert.Equal(t, 2*testFreezeChunk, frozen, "two chunks were committed")
	assert.Equal(t, chunkedIntentOps+chunkedPlainOps-2*testFreezeChunk, open)
	require.NoError(t, h.s.Reenrol(ctx, next))
	wantFrozenOnce(t, h, ops, false)
	// A second Reenrol with the same id is a used id and freezes nothing.
	before := h.s.Position().Seq
	require.ErrorIs(t, h.s.Reenrol(ctx, next), opstore.ErrEnrolment)
	assert.Equal(t, before, h.s.Position().Seq)
}

// A freeze that fails without a crash leaves the store refusing every
// operation, and the next Reenrol completes it.
func TestReenrol_FailedFreezeRefusesEverything(t *testing.T) {
	h, ops := openChunkedStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after the first chunk is audited: the next chunk fails.
	h.opts = func(o *opstore.Options) {
		o.FreezeChunk = testFreezeChunk
		inner := o.Audit
		o.Audit = func(ev opstore.AuditEvent) {
			inner(ev)
			if ev.Reason == opstore.ReasonProviderChanged {
				cancel()
			}
		}
	}
	require.NoError(t, h.s.Close())
	h.open()
	require.NoError(t, h.s.EstablishContinuity(context.Background()))
	_, err := h.s.Recover(context.Background())
	require.NoError(t, err)
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.Error(t, h.s.Reenrol(ctx, next))

	// The store is ready, yet the freeze is incomplete: nothing runs, nothing
	// is confirmed, and nothing is admitted.
	d := h.admit(h.req("vm-new", rid(), kCreate, 1))
	assert.True(t, d.IsRefusal(opstore.RefusalUnattested), "%+v", d)
	_, err = h.s.Execute(context.Background(), ref(ops.plain[len(ops.plain)-1]), nil)
	require.ErrorIs(t, err, opstore.ErrNotReady)
	require.ErrorIs(t, h.s.ConfirmFreeze(context.Background(), h.enrol), opstore.ErrEnrolment)

	require.NoError(t, h.s.Reenrol(context.Background(), next))
	wantFrozenOnce(t, h, ops, true)
}

// A used enrolment id is refused before anything is frozen.
func TestReenrol_UsedIDFreezesNothing(t *testing.T) {
	h, ops := openChunkedStore(t)
	before := h.s.Position().Seq
	used := opstore.Enrolment{HostID: "host-1", ID: h.enrol, ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.ErrorIs(t, h.s.Reenrol(context.Background(), used), opstore.ErrEnrolment)
	assert.Equal(t, before, h.s.Position().Seq)
	for _, r := range ops.all() {
		v, _ := h.lookup(r)
		assert.Contains(t, []opstore.OpState{opstore.StateAccepted, opstore.StateExecuting}, v.State)
	}
}

func TestFreezeMarkerNeverLeftBehind(t *testing.T) {
	// After a completed freeze the marker is gone: a restart finds the store
	// ready and recovery freezes nothing again.
	h, _ := openChunkedStore(t)
	next := opstore.Enrolment{HostID: "host-1", ID: rid(), ProviderUID: h.provider, StartEpoch: h.start + 1}
	require.NoError(t, h.s.Reenrol(context.Background(), next))
	before := h.s.Position().Seq
	h.restart()
	assert.Equal(t, before, h.s.Position().Seq, "recovery wrote nothing")
	require.NoError(t, h.s.ConfirmFreeze(context.Background(), h.enrol), "the freeze can be confirmed once it is complete")
}
