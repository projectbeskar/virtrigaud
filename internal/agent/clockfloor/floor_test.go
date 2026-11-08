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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// fakeClocks is the wall clock, CLOCK_BOOTTIME and boot_id of a test host.
type fakeClocks struct {
	wall    time.Time
	boot    time.Duration
	bootID  string
	bootErr error
}

func (c *fakeClocks) Now() time.Time                   { return c.wall }
func (c *fakeClocks) BootTime() (time.Duration, error) { return c.boot, c.bootErr }
func (c *fakeClocks) BootID() (string, error)          { return c.bootID, nil }

// advance moves the wall clock and boot time together, as real time does.
func (c *fakeClocks) advance(d time.Duration) { c.wall, c.boot = c.wall.Add(d), c.boot+d }

// reboot starts a new boot: a new boot_id and a boot clock near zero. The wall
// clock is left to the caller.
func (c *fakeClocks) reboot(id string) { c.bootID, c.boot = id, 3*time.Second }

func newClocks() *fakeClocks { return &fakeClocks{wall: t0, boot: time.Hour, bootID: "boot-a"} }

func cfgFor(c *fakeClocks, s clockfloor.Store) clockfloor.Config {
	return clockfloor.Config{Store: s, Wall: c, Boot: c, BootID: c, FirstUse: true}
}

func open(t testing.TB, c *fakeClocks, s clockfloor.Store) *clockfloor.Floor {
	t.Helper()
	f, err := clockfloor.Open(context.Background(), cfgFor(c, s))
	require.NoError(t, err)
	return f
}

// TestFloor_ImplementsClock: the Floor is a grantverify.Clock.
func TestFloor_ImplementsClock(t *testing.T) {
	var _ grantverify.Clock = (*clockfloor.Floor)(nil)
}

// TestOpen_FirstUseAndMissing: an empty store opens only for a new enrolment;
// afterwards a deleted floor is refused, never recreated.
func TestOpen_FirstUseAndMissing(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	cfg := cfgFor(c, s)
	cfg.FirstUse = false
	_, err := clockfloor.Open(context.Background(), cfg)
	require.ErrorIs(t, err, clockfloor.ErrFloorMissing)
	assert.Equal(t, 0, s.Saves, "a refused Open writes nothing")

	f := open(t, c, s)
	assert.Equal(t, t0, f.Now())

	cfg.FirstUse = false
	_, err = clockfloor.Open(context.Background(), cfg)
	require.NoError(t, err, "a stored floor opens without FirstUse")
}

// TestFloor_WallRolledBackWithinABoot: the wall clock moves back; the floor
// keeps counting with the boot clock and never returns a time before it.
func TestFloor_WallRolledBackWithinABoot(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)

	// A small rollback: Now stays on the floor, which is ahead of the wall.
	c.advance(10 * time.Minute)
	assert.Equal(t, t0.Add(10*time.Minute), f.Now())
	c.wall = c.wall.Add(-2 * time.Minute)
	assert.Equal(t, t0.Add(10*time.Minute), f.Now(), "never earlier than the floor")
	assert.Equal(t, clockfloor.StateOK, f.Status().State)

	// A large rollback: refused as ahead, and recovered when the wall passes it.
	c.wall = t0.Add(-24 * time.Hour)
	assert.True(t, f.Now().IsZero())
	st := f.Status()
	assert.Equal(t, clockfloor.StateClockFloorAhead, st.State)
	assert.Equal(t, clockfloor.ReasonClockFloorAhead, string(st.State))
	assert.Greater(t, st.Ahead, clockfloor.DefaultMaxAhead)
}

// TestFloor_SurvivesRestart: the persisted floor holds across a restart in
// the same boot, with the boot time that passed credited.
func TestFloor_SurvivesRestart(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	c.advance(30 * time.Minute)
	require.NoError(t, f.Flush(context.Background()))
	floor := f.Now()

	// The process restarts 20 minutes later, and the wall clock was reset.
	c.boot += 20 * time.Minute
	c.wall = t0.Add(-time.Hour)
	f2, err := clockfloor.Open(context.Background(), cfgFor(c, s))
	require.NoError(t, err)
	assert.Equal(t, floor.Add(20*time.Minute), f2.Status().Floor)
	assert.Equal(t, clockfloor.StateClockFloorAhead, f2.Status().State)
}

// TestFloor_RebootWithClockBehind: after a reboot the floor holds although the
// wall clock restarted behind it, and grants are refused until the clock
// passes the floor.
func TestFloor_RebootWithClockBehind(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	c.advance(time.Hour)
	require.NoError(t, f.Flush(context.Background()))
	floor := f.Now()
	require.Equal(t, t0.Add(time.Hour), floor)

	c.reboot("boot-b")
	c.wall = t0 // the clock restarted an hour behind
	f2, err := clockfloor.Open(context.Background(), cfgFor(c, s))
	require.NoError(t, err)
	assert.True(t, f2.Now().IsZero(), "refused while behind the floor")
	assert.Equal(t, clockfloor.StateClockFloorAhead, f2.Status().State)

	// Time passing does not help: the floor keeps counting with the boot
	// clock, so a clock that is an hour behind stays an hour behind.
	c.advance(59 * time.Minute)
	assert.True(t, f2.Now().IsZero(), "still refused while the clock is wrong")
	// The clock is corrected (stepped) past the floor.
	c.wall = c.wall.Add(2 * time.Hour)
	assert.Equal(t, clockfloor.StateOK, f2.Status().State)
	assert.Equal(t, c.wall, f2.Now())
	assert.False(t, f2.Now().Before(floor))
}

// TestFloor_BootCreditIsKeyedOnBootID: a boot-time reading from another boot is
// never replayed as elapsed time.
func TestFloor_BootCreditIsKeyedOnBootID(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	require.NoError(t, s.Save(context.Background(), clockfloor.State{Floor: t0, BootID: "boot-a", BootTime: 100 * time.Second}))

	// A different boot whose boot clock reads far above the persisted value.
	c.bootID, c.boot = "boot-b", 10_000*time.Hour
	f := open(t, c, s)
	assert.Equal(t, t0, f.Status().Floor, "no credit across a boot_id change")

	// The same boot_id does earn the credit, as a control.
	require.NoError(t, s.Save(context.Background(), clockfloor.State{Floor: t0, BootID: "boot-b", BootTime: 100 * time.Second}))
	c.boot = 100*time.Second + 2*time.Hour
	c.wall = t0
	f2 := open(t, c, s)
	assert.Equal(t, t0.Add(2*time.Hour), f2.Status().Floor)
}

// TestOpen_BootTimeRegressionFailsClosed: a boot clock that is lower than the
// persisted one in the same boot is refused.
func TestOpen_BootTimeRegressionFailsClosed(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	require.NoError(t, s.Save(context.Background(), clockfloor.State{Floor: t0, BootID: "boot-a", BootTime: 2 * time.Hour}))
	_, err := clockfloor.Open(context.Background(), cfgFor(c, s))
	require.ErrorIs(t, err, clockfloor.ErrBootTimeRegressed)

	// And at run time.
	c2, s2 := newClocks(), &clockfloor.MemStore{}
	f := open(t, c2, s2)
	c2.boot -= time.Minute
	assert.True(t, f.Now().IsZero())
	assert.Equal(t, clockfloor.StateFailed, f.Status().State)
	require.ErrorIs(t, f.Observe(context.Background()), clockfloor.ErrBootTimeRegressed)
}

// TestOpen_UnreadableFloorFailsClosed: a corrupt, truncated, unreadable or
// invalid floor makes Open fail; none of them reads as a fresh floor.
func TestOpen_UnreadableFloorFailsClosed(t *testing.T) {
	c := newClocks()
	good := &clockfloor.MemStore{}
	open(t, c, good)
	raw := good.Raw()
	require.NotEmpty(t, raw)

	flip := append([]byte(nil), raw...)
	flip[10] ^= 0xFF
	tests := []struct {
		name string
		prep func(s *clockfloor.MemStore)
		want error
	}{
		{"bit flip", func(s *clockfloor.MemStore) { s.SetRaw(flip) }, clockfloor.ErrCorrupt},
		{"truncated", func(s *clockfloor.MemStore) { s.SetRaw(raw[:len(raw)-3]) }, clockfloor.ErrCorrupt},
		{"extended", func(s *clockfloor.MemStore) { s.SetRaw(append(append([]byte(nil), raw...), 0)) }, clockfloor.ErrCorrupt},
		{"garbage", func(s *clockfloor.MemStore) { s.SetRaw([]byte("not a floor record at all")) }, clockfloor.ErrCorrupt},
		{"read error", func(s *clockfloor.MemStore) { s.LoadErr = errors.New("EIO") }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &clockfloor.MemStore{}
			s.SetRaw(raw)
			tt.prep(s)
			cfg := cfgFor(newClocks(), s)
			cfg.FirstUse = true // even a first-use start must not paper over a bad floor
			f, err := clockfloor.Open(context.Background(), cfg)
			require.Error(t, err)
			assert.Nil(t, f)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
			assert.NotErrorIs(t, err, clockfloor.ErrNotFound)
		})
	}
}

// TestFloor_SaveFailureIsSticky: a floor that cannot be persisted fails and
// stays failed.
func TestFloor_SaveFailureIsSticky(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	s.SaveErr = errors.New("EIO")
	c.advance(2 * clockfloor.DefaultPersistInterval)
	require.Error(t, f.Observe(context.Background()))
	s.SaveErr = nil
	assert.Error(t, f.Observe(context.Background()), "no retry-and-continue")
	assert.True(t, f.Now().IsZero())
	assert.Equal(t, clockfloor.StateFailed, f.Status().State)

	s2 := &clockfloor.MemStore{SaveErr: errors.New("EIO")}
	_, err := clockfloor.Open(context.Background(), cfgFor(newClocks(), s2))
	require.Error(t, err)
}

// TestFloor_PersistInterval: Observe saves only after the floor advanced a
// PersistInterval; Flush always saves.
func TestFloor_PersistInterval(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	base := s.Saves
	c.advance(clockfloor.DefaultPersistInterval / 2)
	require.NoError(t, f.Observe(context.Background()))
	assert.Equal(t, base, s.Saves)
	c.advance(clockfloor.DefaultPersistInterval)
	require.NoError(t, f.Observe(context.Background()))
	assert.Equal(t, base+1, s.Saves)
	require.NoError(t, f.Flush(context.Background()))
	assert.Equal(t, base+2, s.Saves)
}

// TestFloor_NeverRaisedFromAGrantTimestamp: the Floor has no input but its
// clocks; a far-future wall reading raises it (own clock) and nothing else
// can. This pins the one input path.
func TestFloor_OnlyOwnClocksRaiseIt(t *testing.T) {
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	before := f.Status().Floor
	for i := 0; i < 5; i++ {
		_ = f.Now()
		_ = f.Status()
	}
	assert.Equal(t, before, f.Status().Floor, "reading never raises the floor without time passing")
}

// TestOpen_SourceFailures: a failing boot clock or boot_id source refuses the
// start.
func TestOpen_SourceFailures(t *testing.T) {
	c := newClocks()
	c.bootErr = clockfloor.ErrUnsupported
	_, err := clockfloor.Open(context.Background(), cfgFor(c, &clockfloor.MemStore{}))
	require.ErrorIs(t, err, clockfloor.ErrUnsupported)

	_, err = clockfloor.Open(context.Background(), clockfloor.Config{})
	require.Error(t, err, "no store")
}
