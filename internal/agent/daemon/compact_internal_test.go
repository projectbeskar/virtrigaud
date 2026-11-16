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

package daemon

import (
	"testing"
	"time"
)

// TestCompactDue: a compaction runs at once, then every period, and earlier
// near the size threshold, at most once per check period.
func TestCompactDue(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name string
		last time.Time
		near bool
		want bool
	}{
		{"never ran", time.Time{}, false, true},
		{"within the period", now.Add(-30 * time.Minute), false, false},
		{"period passed", now.Add(-time.Hour), false, true},
		{"near the threshold, last one a check period ago", now.Add(-compactCheckEvery), true, true},
		{"near the threshold, last one just now", now.Add(-time.Second), true, false},
	} {
		if got := compactDue(now, c.last, time.Hour, c.near); got != c.want {
			t.Errorf("%s: compactDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNearCap: three quarters of the threshold.
func TestNearCap(t *testing.T) {
	for _, c := range []struct {
		live, max int64
		want      bool
	}{
		{0, 0, false}, {191, 256, false}, {192, 256, true}, {300, 256, true}, {256 << 20 / 4 * 3, 256 << 20, true},
	} {
		if got := nearCap(c.live, c.max); got != c.want {
			t.Errorf("nearCap(%d, %d) = %v, want %v", c.live, c.max, got, c.want)
		}
	}
}
