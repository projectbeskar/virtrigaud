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
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

// runScript drives a Floor through a script of clock events and asserts the
// floor's monotonicity at every step: the floor never decreases, across
// restarts and reboots too, and the time Now returns (when it returns one) is
// never behind the floor or any earlier Now.
func runScript(t testing.TB, script []byte) {
	t.Helper()
	ctx := context.Background()
	c, s := newClocks(), &clockfloor.MemStore{}
	f := open(t, c, s)
	var maxFloor, lastNow time.Time
	bootN := 0
	check := func() {
		st := f.Status()
		if st.State == clockfloor.StateFailed {
			return
		}
		require.False(t, st.Floor.Before(maxFloor), "floor decreased: %s < %s", st.Floor, maxFloor)
		maxFloor = st.Floor
		if now := f.Now(); !now.IsZero() {
			require.False(t, now.Before(st.Floor), "Now behind the floor")
			require.False(t, now.Before(lastNow), "Now decreased")
			lastNow = now
		}
	}
	for i := 0; i+1 < len(script); i += 2 {
		arg := int64(script[i+1])
		switch script[i] % 7 {
		case 0: // time passes
			c.advance(time.Duration(arg) * time.Minute)
		case 1: // the wall clock is set anywhere in a 20-year window
			c.wall = t0.Add(time.Duration(arg-128) * 30 * 24 * time.Hour)
		case 2:
			require.NoError(t, f.Observe(ctx))
		case 3:
			require.NoError(t, f.Flush(ctx))
		case 4: // the process restarts in the same boot
			require.NoError(t, f.Flush(ctx))
			c.boot += time.Duration(arg) * time.Second
			var err error
			f, err = clockfloor.Open(ctx, cfgFor(c, s))
			require.NoError(t, err)
		case 5: // the host reboots
			require.NoError(t, f.Flush(ctx))
			bootN++
			c.reboot("boot-" + string(rune('a'+bootN%26)) + string(rune('a'+bootN/26)))
			c.boot += time.Duration(arg) * time.Hour // an arbitrary boot clock must earn nothing
			var err error
			f, err = clockfloor.Open(ctx, cfgFor(c, s))
			require.NoError(t, err)
		case 6: // a boot-time reading that moved a little, as scheduling can make it
			c.boot += time.Duration(arg) * time.Millisecond
		}
		check()
	}
}

// FuzzFloorMonotonic: for any script of clock events, the floor never moves
// backwards.
func FuzzFloorMonotonic(f *testing.F) {
	f.Add([]byte{0, 10, 1, 0, 2, 0, 5, 3, 1, 255, 0, 5, 4, 9})
	f.Add([]byte{1, 0, 3, 0, 5, 200, 1, 200, 0, 90})
	f.Fuzz(func(t *testing.T, script []byte) { runScript(t, script) })
}

// TestFloorMonotonic_Random runs the same property over seeded random
// scripts, so it holds in a plain test run.
func TestFloorMonotonic_Random(t *testing.T) {
	rng := rand.New(rand.NewSource(20261108))
	for i := 0; i < 2000; i++ {
		script := make([]byte, 2*(1+rng.Intn(60)))
		_, _ = rng.Read(script)
		runScript(t, script)
	}
}
