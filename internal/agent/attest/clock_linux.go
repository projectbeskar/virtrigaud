//go:build linux

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

package attest

import (
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// RawClock is the Linux CLOCK_MONOTONIC_RAW clock: neither a wall-clock
// change nor NTP slewing moves it.
type RawClock struct{}

// Now returns CLOCK_MONOTONIC_RAW. A failed read returns the largest
// duration, which makes every outstanding challenge expired (fails closed).
func (RawClock) Now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts); err != nil {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ts.Nano())
}
