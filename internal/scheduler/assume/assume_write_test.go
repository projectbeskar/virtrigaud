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

package assume

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// These tests pin the write lifecycle of an assumption: it ends on evidence
// (a snapshot showing the record, a definitive rejection, an authoritative
// resolution), never on its TTL alone once a write was sent.

// absent and present are snapshot answers for Observe.
func absent(Assumption) Observation  { return Observation{} }
func present(Assumption) Observation { return Observation{Present: true} }

func TestExpiry_OnlyAPickWithNoWriteSentIsDropped(t *testing.T) {
	clk := newClock()
	c := New(time.Minute, clk.now)
	var stale []string
	c.SetStaleHook(func(_ string, a Assumption, age time.Duration) {
		stale = append(stale, a.UID)
		assert.GreaterOrEqual(t, age, time.Minute)
	})
	tNot, _ := c.Assume("p1", assumption("not-sent", "h1"))
	tFly, _ := c.Assume("p1", assumption("in-flight", "h1"))
	tSto, _ := c.Assume("p1", assumption("stored", "h1"))
	require.NotZero(t, tNot)
	require.True(t, c.BeginWrite("in-flight", tFly))
	require.True(t, c.BeginWrite("stored", tSto))
	c.WriteStored("stored", tSto)

	clk.advance(time.Minute)
	got := uids(c.Observe("p1", present))
	assert.Equal(t, []string{"in-flight", "stored"}, got, "a sent write keeps counting past the TTL")
	assert.ElementsMatch(t, []string{"in-flight", "stored"}, stale, "and is reported as stale")

	stale = nil
	c.Observe("p1", present)
	assert.Empty(t, stale, "reported once")
}

func TestAssume_UnresolvedPickIsNeverReplacedByAnotherHost(t *testing.T) {
	c := New(time.Minute, newClock().now)
	tok, ok := c.Assume("p1", assumption("vm", "h1"))
	require.True(t, ok)
	require.True(t, c.BeginWrite("vm", tok))
	c.WriteAmbiguous("vm", tok, "rv-1")

	_, ok = c.Assume("p1", assumption("vm", "h2"))
	assert.False(t, ok, "another host is refused while the write is unresolved")
	_, ok = c.Assume("p2", assumption("vm", "h1"))
	assert.False(t, ok, "another Provider too")

	bigger := assumption("vm", "h1")
	bigger.Resources.CPU = 4
	again, ok := c.Assume("p1", bigger)
	require.True(t, ok)
	assert.Equal(t, tok, again, "the same host continues the pick")
	a, _, _ := c.Lookup("vm")
	assert.Equal(t, WriteInFlight, a.Write, "with its write record")
	assert.EqualValues(t, 4, a.Resources.CPU, "at the larger size")
	assert.True(t, c.Pending("vm", tok, "rv-1"))
}

func TestAssume_PickWithNoWriteSentIsReplaced(t *testing.T) {
	c := New(time.Minute, newClock().now)
	first, _ := c.Assume("p1", assumption("vm", "h1"))
	second, ok := c.Assume("p1", assumption("vm", "h2"))
	require.True(t, ok)
	assert.NotEqual(t, first, second)
	assert.False(t, c.BeginWrite("vm", first), "the replaced pick's write is not sent")
	assert.False(t, c.ForgetIf("vm", first), "nor can it remove the newer pick")
	_, ok = c.TokenFor("vm", "h1")
	assert.False(t, ok)
	got, ok := c.TokenFor("vm", "h2")
	require.True(t, ok)
	assert.Equal(t, second, got)
}

func TestWriteRejected(t *testing.T) {
	t.Run("a first write refused releases the pick", func(t *testing.T) {
		c := New(time.Minute, newClock().now)
		tok, _ := c.Assume("p1", assumption("vm", "h1"))
		require.True(t, c.BeginWrite("vm", tok))
		assert.True(t, c.WriteRejected("vm", tok))
		assert.Zero(t, c.Len())
	})
	t.Run("an earlier ambiguous write keeps it", func(t *testing.T) {
		c := New(time.Minute, newClock().now)
		tok, _ := c.Assume("p1", assumption("vm", "h1"))
		require.True(t, c.BeginWrite("vm", tok))
		c.WriteAmbiguous("vm", tok, "rv-1")
		require.True(t, c.BeginWrite("vm", tok))
		assert.False(t, c.WriteRejected("vm", tok), "the conflict of a later write does not prove the first did not land")
		assert.True(t, c.Unresolved("vm"))
	})
	t.Run("a stored write keeps it", func(t *testing.T) {
		c := New(time.Minute, newClock().now)
		tok, _ := c.Assume("p1", assumption("vm", "h1"))
		require.True(t, c.BeginWrite("vm", tok))
		c.WriteStored("vm", tok)
		assert.False(t, c.WriteRejected("vm", tok))
		assert.True(t, c.Unresolved("vm"))
	})
	t.Run("another pick's token changes nothing", func(t *testing.T) {
		c := New(time.Minute, newClock().now)
		tok, _ := c.Assume("p1", assumption("vm", "h1"))
		assert.False(t, c.WriteRejected("vm", tok+1))
		assert.Equal(t, 1, c.Len())
	})
}

func TestPending_EqualityWithAnAmbiguousPreconditionOnly(t *testing.T) {
	c := New(time.Minute, newClock().now)
	tok, _ := c.Assume("p1", assumption("vm", "h1"))
	require.True(t, c.BeginWrite("vm", tok))
	assert.False(t, c.Pending("vm", tok, "rv-1"), "no ambiguous write yet")
	c.WriteAmbiguous("vm", tok, "rv-1")
	assert.True(t, c.Pending("vm", tok, "rv-1"), "the stored version is still the precondition: the write may land")
	assert.False(t, c.Pending("vm", tok, "rv-2"), "any other stored version: it never can")
	assert.False(t, c.Pending("vm", tok+1, "rv-1"), "another pick")
	c.WriteStored("vm", tok)
	assert.False(t, c.Pending("vm", tok, "rv-1"), "a stored write supersedes the ambiguous one")
}

func TestObserve_AbsenceSettlesOnlyAnObservedVM(t *testing.T) {
	c := New(time.Minute, newClock().now)
	tok, _ := c.Assume("p1", assumption("fresh", "h1")) // not in the snapshot yet
	require.True(t, c.BeginWrite("fresh", tok))
	assert.Equal(t, []string{"fresh"}, uids(c.Observe("p1", absent)), "a VM not seen yet is not taken as gone")
	assert.Equal(t, []string{"fresh"}, uids(c.Observe("p1", present)), "now seen")
	assert.Empty(t, c.Observe("p1", absent), "and its absence after that settles it")

	seen := assumption("seen", "h1")
	seen.Observed = true
	c.Assume("p1", seen)
	assert.Empty(t, c.Observe("p1", absent), "a VM the pick's snapshot showed is settled by its absence")
}

func TestObserve_ASnapshotNotShowingTheRecordKeepsAStoredWrite(t *testing.T) {
	c := New(time.Minute, newClock().now)
	tok, _ := c.Assume("p1", assumption("vm", "h1"))
	require.True(t, c.BeginWrite("vm", tok))
	c.WriteStored("vm", tok)
	assert.Equal(t, []string{"vm"}, uids(c.Observe("p1", present)), "an older snapshot without the record keeps it")
	settled := func(Assumption) Observation { return Observation{Present: true, Settled: true} }
	assert.Empty(t, c.Observe("p1", settled), "the snapshot showing the record settles it")
}

func TestSettleVisible(t *testing.T) {
	c := New(time.Minute, newClock().now)
	tok, _ := c.Assume("p1", assumption("vm", "h1"))
	require.True(t, c.BeginWrite("vm", tok))
	c.SettleVisible("vm", []string{"h2"})
	assert.Equal(t, 1, c.Len(), "a record of another host does not settle it")
	c.SettleVisible("vm", []string{"h1"})
	assert.Zero(t, c.Len())
}

func TestBeginWrite_AnExpiredPickDroppedByADecisionIsNeverWritten(t *testing.T) {
	clk := newClock()
	c := New(time.Minute, clk.now)
	dropped, _ := c.Assume("p1", assumption("dropped", "h1"))
	kept, _ := c.Assume("p1", assumption("kept", "h1"))

	clk.advance(time.Minute)
	require.True(t, c.BeginWrite("kept", kept), "expired but not yet dropped: every decision so far counted it")
	assert.Equal(t, []string{"kept"}, uids(c.Observe("p1", present)), "so from now on it counts until resolved")
	assert.False(t, c.BeginWrite("dropped", dropped), "the decision dropped the other one: its write is refused")
}

func TestNarrow_LowersOnlyItsOwnPick(t *testing.T) {
	c := New(time.Minute, newClock().now)
	big := assumption("vm", "h1")
	big.Resources = scheduler.ResourceRequest{CPU: 8, MemoryMiB: 16384}
	tok, _ := c.Assume("p1", big)
	c.Narrow("vm", tok+1, scheduler.ResourceRequest{CPU: 1, MemoryMiB: 1})
	a, _, _ := c.Lookup("vm")
	assert.Equal(t, int32(8), a.Resources.CPU, "another pick's token changes nothing")
	c.Narrow("vm", tok, scheduler.ResourceRequest{CPU: 4, MemoryMiB: 32768})
	a, _, _ = c.Lookup("vm")
	assert.Equal(t, scheduler.ResourceRequest{CPU: 4, MemoryMiB: 16384}, a.Resources, "lowered per resource, never raised")
}
