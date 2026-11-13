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

package clockfloor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

// Open only reads. Until StartPersisting, the floor lives in memory: it is
// raised from the clocks, Observe and Flush save nothing, and the stored
// floor is untouched.
func TestOpen_WritesNothingBeforeStartPersisting(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f, err := clockfloor.Open(ctx, cfgFor(c, s))
	require.NoError(t, err)
	assert.Equal(t, 0, s.Saves, "Open writes nothing")
	assert.Equal(t, t0, f.Now())

	c.advance(time.Hour)
	require.NoError(t, f.Observe(ctx))
	require.NoError(t, f.Flush(ctx))
	assert.Equal(t, t0.Add(time.Hour), f.Now(), "the in-memory floor is raised")
	assert.Equal(t, 0, s.Saves, "Observe and Flush save nothing before StartPersisting")

	require.NoError(t, f.StartPersisting(ctx))
	assert.Equal(t, 1, s.Saves)
	st, err := s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, t0.Add(time.Hour), st.Floor, "the first write carries the raised floor")
}

// The in-memory floor starts from the stored one: before anything is
// written, a wall clock behind the stored floor does not lower it, and the
// first write never stores less than what was stored.
func TestOpen_FloorNeverBelowStoredBeforeFirstWrite(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	c.advance(3 * time.Hour)
	require.NoError(t, f.Flush(ctx))
	stored := f.Now()

	// A reboot with the wall clock a day behind.
	c.reboot("boot-b")
	c.wall = stored.Add(-24 * time.Hour)
	f2, err := clockfloor.Open(ctx, cfgFor(c, s))
	require.NoError(t, err)
	st := f2.Status()
	assert.Equal(t, stored, st.Floor, "the floor held")
	assert.Equal(t, clockfloor.StateClockFloorAhead, st.State)
	assert.True(t, f2.Now().IsZero())
	savesBefore := s.Saves

	require.NoError(t, f2.StartPersisting(ctx))
	assert.Equal(t, savesBefore+1, s.Saves)
	got, err := s.Load(ctx)
	require.NoError(t, err)
	assert.False(t, got.Floor.Before(stored), "the first write is not below the stored floor: %s < %s", got.Floor, stored)
}

// StopPersisting returns the Floor to memory only: it keeps raising, and
// keeps its health, without writing.
func TestFloor_StopPersisting(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	f.StopPersisting()
	saves := s.Saves
	c.advance(time.Hour)
	require.NoError(t, f.Observe(ctx))
	require.NoError(t, f.Flush(ctx))
	assert.Equal(t, saves, s.Saves)
	assert.Equal(t, t0.Add(time.Hour), f.Now())
	assert.Equal(t, clockfloor.StateOK, f.Status().State)
}

// The write rate is bounded by PersistInterval: with Observe every five
// seconds and a one-minute interval, ten minutes cost at most one write per
// minute, and a crash (the process dies, the host reboots with the wall clock
// behind) loses at most the interval and the Observe period of the raise,
// never a value that was saved.
func TestFloor_PersistRateAndCrashLoss(t *testing.T) {
	ctx := context.Background()
	const (
		step   = time.Minute
		period = 5 * time.Second
	)
	c, s := newClocks(), &clockfloor.MemStore{}
	cfg := cfgFor(c, s)
	cfg.PersistInterval = step
	f, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, f.StartPersisting(ctx))
	base := s.Saves

	var lastSaved time.Time
	for i := 0; i < 10*int(time.Minute/period); i++ {
		c.advance(period)
		require.NoError(t, f.Observe(ctx))
		st, err := s.Load(ctx)
		require.NoError(t, err)
		assert.False(t, st.Floor.Before(lastSaved), "a saved floor never goes down")
		lastSaved = st.Floor
		// A crash right now loses at most one step and one period.
		assert.LessOrEqual(t, f.Now().Sub(st.Floor), step+period, "unsaved raise bounded")
	}
	assert.LessOrEqual(t, s.Saves-base, 10, "at most one write per interval")
	assert.GreaterOrEqual(t, s.Saves-base, 9)

	// The crash: no Flush; the host reboots with the wall clock behind.
	inMemory := f.Now()
	c.reboot("boot-b")
	c.wall = t0
	f2, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, lastSaved, f2.Status().Floor)
	assert.LessOrEqual(t, inMemory.Sub(lastSaved), step+period)
}

// After an unclean end (another boot_id, last save not a clean drain) the
// floor is raised by NewBootSlack, the unsaved advance a crash may have lost,
// even when the wall clock is behind; the raise stops at wall + MaxAhead and
// never goes below the stored floor.
func TestOpen_NewBootSlack(t *testing.T) {
	ctx := context.Background()
	const slack = 65 * time.Second
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	c.advance(time.Hour)
	require.NoError(t, f.Flush(ctx))
	stored := f.Now()
	cfg := cfgFor(c, s)
	cfg.NewBootSlack = slack
	cfg.MaxAhead = 5 * time.Minute

	// Same boot: the boot-time credit restores the advance; no slack.
	c.boot += 10 * time.Second
	c.wall = stored.Add(-time.Hour)
	same, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored.Add(10*time.Second), same.Status().Floor)

	// A new boot with the wall clock behind the stored floor by more than the
	// slack: the full slack still applies.
	c.reboot("boot-b")
	c.wall = stored.Add(-2 * time.Minute)
	f2, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored.Add(slack), f2.Status().Floor)
	assert.Equal(t, clockfloor.StateOK, f2.Status().State)

	// A wall clock at the stored floor: the full slack.
	c.reboot("boot-c")
	c.wall = stored
	f3, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored.Add(slack), f3.Status().Floor)

	// A wall clock so far behind that wall + MaxAhead is below the stored
	// floor: the stored floor is kept, nothing is added.
	c.reboot("boot-d")
	c.wall = stored.Add(-time.Hour)
	f4, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored, f4.Status().Floor)

	// A wall clock whose wall + MaxAhead falls between stored and stored +
	// slack: the raise stops there.
	c.reboot("boot-f")
	c.wall = stored.Add(-5*time.Minute + 20*time.Second)
	f6, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored.Add(20*time.Second), f6.Status().Floor)

	// Without the setting nothing is added.
	c.reboot("boot-e")
	c.wall = stored.Add(-10 * time.Second)
	f5, err := clockfloor.Open(ctx, cfgFor(c, s))
	require.NoError(t, err)
	assert.Equal(t, stored, f5.Status().Floor)
}

// After a clean drain the next boot adds nothing: no advance is unsaved.
func TestOpen_NoSlackAfterCleanDrain(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	c.advance(time.Hour)
	require.NoError(t, f.Drain(ctx))
	st, err := s.Load(ctx)
	require.NoError(t, err)
	assert.True(t, st.Drained)
	stored := st.Floor

	c.reboot("boot-b")
	c.wall = stored
	cfg := cfgFor(c, s)
	cfg.NewBootSlack = 65 * time.Second
	f2, err := clockfloor.Open(ctx, cfg)
	require.NoError(t, err)
	assert.Equal(t, stored, f2.Status().Floor, "no slack after a clean drain")

	// The first write of the new run clears the flag, so a crash after it is
	// unclean.
	require.NoError(t, f2.StartPersisting(ctx))
	st, err = s.Load(ctx)
	require.NoError(t, err)
	assert.False(t, st.Drained)
}

// Fast reboots: clean ones add nothing; unclean ones that never commit do not
// stack (the raise lives in memory); unclean ones that each commit are capped
// at wall + MaxAhead.
func TestOpen_FastRebootsAreBounded(t *testing.T) {
	const (
		slack    = 65 * time.Second
		maxAhead = 5 * time.Minute
		reboots  = 50
	)
	newCfg := func(c *fakeClocks, s clockfloor.Store) clockfloor.Config {
		cfg := cfgFor(c, s)
		cfg.NewBootSlack, cfg.MaxAhead = slack, maxAhead
		return cfg
	}
	ctx := context.Background()
	t.Run("clean", func(t *testing.T) {
		c, s := newClocks(), &clockfloor.MemStore{}
		f := open(t, c, s)
		for i := 0; i < reboots; i++ {
			require.NoError(t, f.Drain(ctx))
			c.reboot(fmt.Sprintf("boot-%d", i))
			c.wall = c.wall.Add(time.Second)
			var err error
			f, err = clockfloor.Open(ctx, newCfg(c, s))
			require.NoError(t, err)
			require.NoError(t, f.StartPersisting(ctx))
			assert.LessOrEqual(t, f.Status().Floor.Sub(c.wall), time.Duration(0)+time.Nanosecond, "reboot %d: nothing added after a clean drain", i)
		}
	})
	t.Run("unclean without a commit", func(t *testing.T) {
		c, s := newClocks(), &clockfloor.MemStore{}
		f := open(t, c, s)
		require.NoError(t, f.Flush(ctx))
		stored, err := s.Load(ctx)
		require.NoError(t, err)
		for i := 0; i < reboots; i++ {
			c.reboot(fmt.Sprintf("boot-%d", i))
			c.wall = c.wall.Add(time.Second)
			f, err = clockfloor.Open(ctx, newCfg(c, s)) // killed before any commit
			require.NoError(t, err)
			assert.Equal(t, stored.Floor.Add(slack), f.Status().Floor, "reboot %d: the slack does not stack", i)
		}
	})
	t.Run("unclean with commits", func(t *testing.T) {
		c, s := newClocks(), &clockfloor.MemStore{}
		f := open(t, c, s)
		for i := 0; i < reboots; i++ {
			require.NoError(t, f.Flush(ctx))
			c.reboot(fmt.Sprintf("boot-%d", i))
			c.wall = c.wall.Add(time.Second)
			var err error
			f, err = clockfloor.Open(ctx, newCfg(c, s))
			require.NoError(t, err)
			require.NoError(t, f.StartPersisting(ctx))
			lead := f.Status().Floor.Sub(c.wall)
			assert.LessOrEqual(t, lead, maxAhead, "reboot %d lead %s", i, lead)
			assert.Equal(t, clockfloor.StateOK, f.Status().State)
		}
	})
}

// A save that fails because the store can no longer commit stops persisting;
// it does not fail the floor.
func TestFloor_StoreLostStopsPersisting(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	s.SaveErr = clockfloor.ErrStoreLost
	c.advance(2 * clockfloor.DefaultPersistInterval)
	require.ErrorIs(t, f.Observe(ctx), clockfloor.ErrStoreLost)
	assert.Equal(t, clockfloor.StateOK, f.Status().State)
	assert.False(t, f.Now().IsZero())
	s.SaveErr = nil
	saves := s.Saves
	c.advance(2 * clockfloor.DefaultPersistInterval)
	require.NoError(t, f.Observe(ctx))
	assert.Equal(t, saves, s.Saves, "no further saves")
	assert.Equal(t, c.wall, f.Now())
}

// Leaving the ahead state needs the lead to fall AheadHysteresis below
// MaxAhead, so a clock that jitters at the edge does not flip the state.
func TestFloor_AheadHasHysteresis(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	cfg := cfgFor(c, s)
	cfg.MaxAhead = 5 * time.Minute
	f, err := clockfloor.Open(context.Background(), cfg)
	require.NoError(t, err)
	floor := f.Status().Floor
	state := func(lead time.Duration) clockfloor.StateKind {
		c.wall = floor.Add(-lead)
		return f.Status().State
	}
	assert.Equal(t, clockfloor.StateOK, state(cfg.MaxAhead-time.Second))
	assert.Equal(t, clockfloor.StateClockFloorAhead, state(cfg.MaxAhead+time.Second), "enters beyond MaxAhead")
	for i := 0; i < 5; i++ {
		assert.Equal(t, clockfloor.StateClockFloorAhead, state(cfg.MaxAhead-5*time.Second), "jitter inside the band stays ahead")
		assert.Equal(t, clockfloor.StateClockFloorAhead, state(cfg.MaxAhead+time.Second))
	}
	assert.True(t, f.Now().IsZero())
	assert.Equal(t, clockfloor.StateOK, state(cfg.MaxAhead-clockfloor.AheadHysteresis-time.Second), "leaves below the band")
	assert.False(t, f.Now().IsZero())
	assert.Equal(t, clockfloor.StateOK, state(cfg.MaxAhead-5*time.Second), "once OK, the plain bound applies again")
}
