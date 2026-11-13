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
