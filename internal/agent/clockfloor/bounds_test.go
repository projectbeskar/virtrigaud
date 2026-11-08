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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

var maxFloor = time.Date(clockfloor.MaxFloorYear, time.January, 1, 0, 0, 0, 0, time.UTC)

// TestState_BoundedCodec: a floor at the bound round-trips; a floor beyond it,
// including years at which a nanosecond encoding would wrap, is not encoded.
func TestState_BoundedCodec(t *testing.T) {
	at := clockfloor.State{Floor: maxFloor, BootID: "a", BootTime: time.Second}
	b, err := at.MarshalBinary()
	require.NoError(t, err)
	var out clockfloor.State
	require.NoError(t, out.UnmarshalBinary(b))
	assert.True(t, maxFloor.Equal(out.Floor))

	for name, f := range map[string]time.Time{
		"one nanosecond past":  maxFloor.Add(time.Nanosecond),
		"2300":                 time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC),
		"2600 (wraps to 2015)": time.Date(2600, 1, 1, 0, 0, 0, 0, time.UTC),
		"2800":                 time.Date(2800, 1, 1, 0, 0, 0, 0, time.UTC),
		"epoch":                time.Unix(0, 0),
		"before the epoch":     time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		_, err := (clockfloor.State{Floor: f, BootID: "a"}).MarshalBinary()
		assert.Error(t, err, name)
	}
}

// TestFloor_ClockStepFarForwardFailsClosed: the wall clock is stepped to the
// year 2600 and then the host reboots with the clock set behind. The floor is
// never raised to 2600 (a nanosecond encoding would wrap it back to 2015), so
// after the reboot it still holds and grants are refused.
func TestFloor_ClockStepFarForwardFailsClosed(t *testing.T) {
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	require.NoError(t, f.Flush(ctx))
	saves := s.Saves

	c.wall = time.Date(2600, 1, 1, 0, 0, 0, 0, time.UTC)
	require.ErrorIs(t, f.Observe(ctx), clockfloor.ErrClockOutOfRange)
	assert.True(t, f.Now().IsZero())
	assert.Equal(t, clockfloor.StateFailed, f.Status().State)
	assert.Equal(t, saves, s.Saves, "nothing beyond the bound is saved")
	st, err := s.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, t0, st.Floor)

	// Reboot with the wall clock an hour behind the saved floor.
	c.reboot("boot-b")
	c.wall = t0.Add(-time.Hour)
	f2, err := clockfloor.Open(ctx, cfgFor(c, s))
	require.NoError(t, err)
	assert.True(t, f2.Now().IsZero(), "the floor held; the clock is behind it")
	assert.Equal(t, clockfloor.StateClockFloorAhead, f2.Status().State)
	assert.Equal(t, t0, f2.Status().Floor)

	// Opening while the clock is still out of range is refused outright.
	c.wall = time.Date(2600, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = clockfloor.Open(ctx, cfgFor(c, s))
	require.ErrorIs(t, err, clockfloor.ErrClockOutOfRange)
}

// TestOpen_TimeNamespace: a non-zero time namespace offset refuses the start;
// zero offsets and a kernel without the file are accepted.
func TestOpen_TimeNamespace(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, "timens_offsets")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	check := func(p string) error { return clockfloor.ProcTimeNS{Path: p}.CheckTimeNamespace() }

	require.NoError(t, check(write("monotonic 0 0\nboottime 0 0\n")))
	require.NoError(t, check(filepath.Join(dir, "missing")))
	require.ErrorIs(t, check(write("monotonic 0 0\nboottime 315360000 0\n")), clockfloor.ErrTimeNamespace)
	require.ErrorIs(t, check(write("monotonic 0 5\nboottime 0 0\n")), clockfloor.ErrTimeNamespace)
	require.Error(t, check(write("garbage\n")))

	c, s := newClocks(), &clockfloor.MemStore{}
	cfg := cfgFor(c, s)
	cfg.TimeNS = clockfloor.ProcTimeNS{Path: write("boottime 1 0\n")}
	_, err := clockfloor.Open(context.Background(), cfg)
	require.ErrorIs(t, err, clockfloor.ErrTimeNamespace)
	assert.Equal(t, 0, s.Saves)
}

// blockingStore blocks Save until released.
type blockingStore struct {
	clockfloor.MemStore
	entered chan struct{}
	release chan struct{}
	block   bool
}

func (b *blockingStore) Save(ctx context.Context, s clockfloor.State) error {
	if b.block {
		b.entered <- struct{}{}
		<-b.release
	}
	return b.MemStore.Save(ctx, s)
}

// TestFloor_NowDoesNotWaitForSave: a slow Save does not block Now.
func TestFloor_NowDoesNotWaitForSave(t *testing.T) {
	c := newClocks()
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	f := open(t, c, st)
	st.block = true
	c.advance(time.Hour)
	done := make(chan error)
	go func() { done <- f.Flush(context.Background()) }()
	<-st.entered
	nowed := make(chan time.Time)
	go func() { nowed <- f.Now() }()
	select {
	case got := <-nowed:
		assert.Equal(t, c.wall, got)
	case <-time.After(5 * time.Second):
		t.Fatal("Now blocked behind Save")
	}
	close(st.release)
	require.NoError(t, <-done)
}
