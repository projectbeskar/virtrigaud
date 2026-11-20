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

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestCompactDue: a compaction runs at once, then every period, and earlier
// near the size threshold, at most once per check period.
func TestCompactDue(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name       string
		last       time.Time
		near       bool
		discarding bool
		want       bool
	}{
		{"never ran", time.Time{}, false, false, true},
		{"within the period", now.Add(-30 * time.Minute), false, false, false},
		{"period passed", now.Add(-time.Hour), false, false, true},
		{"near the threshold, last one a check period ago", now.Add(-compactCheckEvery), true, false, true},
		{"near the threshold, last one just now", now.Add(-time.Second), true, false, false},
		{"a confirmed discard has records left, whatever the backoff", now.Add(-time.Second), true, true, true},
		{"a confirmed discard has records left, within the period", now.Add(-30 * time.Minute), false, true, true},
	} {
		if got := compactDue(now, c.last, time.Hour, 0, c.near, c.discarding); got != c.want {
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

// TestNearCapIsTheAnswerWarning: compaction is early exactly when a signed
// host answer reports the store at WARNING or FULL, so the operator sees the
// warning no later than the agent starts compacting early.
func TestNearCapIsTheAnswerWarning(t *testing.T) {
	const limit = 1003
	for live := int64(0); live <= 2*limit; live++ {
		level := hostidentity.StoreLevelOf(uint64(live), limit)
		if got, want := nearCap(live, limit), level != providerv1.StoreLevel_STORE_LEVEL_OK; got != want {
			t.Fatalf("live %d: nearCap = %v, answer level %s", live, got, level)
		}
	}
}

// TestNearBackoff: near the threshold, each early compaction that dropped
// nothing doubles the wait up to the period; one that dropped records, or
// growth of the store by a sixty-fourth of the threshold, resets it; a
// compaction is not due before the wait.
func TestNearBackoff(t *testing.T) {
	const maxBytes = 256 << 20
	every := time.Hour
	var b nearBackoff
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		b.after(0, maxBytes-1, every)
		if b.wait != w {
			t.Fatalf("after %d empty compactions wait = %v, want %v", i+1, b.wait, w)
		}
	}
	now := time.Now()
	if compactDue(now, now.Add(-30*time.Minute), every, b.wait, true, false) {
		t.Fatal("due before the backed-off wait")
	}
	b.grew(maxBytes-1+maxBytes/compactGrowthDen-1, maxBytes, every)
	if b.wait != time.Hour {
		t.Fatal("reset by growth below a sixty-fourth of the threshold")
	}
	b.grew(maxBytes-1+maxBytes/compactGrowthDen, maxBytes, every)
	if b.wait != compactCheckEvery {
		t.Fatalf("wait after growth = %v", b.wait)
	}
	b.after(0, maxBytes, every)
	b.after(3, maxBytes, every)
	if b.wait != compactCheckEvery {
		t.Fatalf("wait after a compaction that dropped records = %v", b.wait)
	}
	if !compactDue(now, now.Add(-compactCheckEvery), every, b.wait, true, false) {
		t.Fatal("not due after the reset wait")
	}
}
