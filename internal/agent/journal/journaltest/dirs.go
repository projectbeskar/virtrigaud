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
package journaltest

import (
	"os"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// fastRoot is a memory-backed directory used, when present, for stores in
// tests, so that bbolt's fsync costs little.
const fastRoot = "/dev/shm"

// FastTempDir returns a temporary directory for a bbolt store, on a
// memory-backed file system when one is available, removed at the end of the
// test.
func FastTempDir(t testing.TB) string {
	t.Helper()
	if st, err := os.Stat(fastRoot); err == nil && st.IsDir() {
		d, err := os.MkdirTemp(fastRoot, "vrtg-journal-test-")
		if err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(d) })
			return d
		}
	}
	return t.TempDir()
}

// Crasher is a journal crash hook that fires once, at the n-th time (from 1)
// the named point is reached.
type Crasher struct {
	// Point is the crash point to fire at.
	Point journal.Point
	// N is the occurrence to fire at, from 1.
	N     int
	seen  int
	fired bool
}

// At implements the hook: it reports whether to crash at p.
func (c *Crasher) At(p journal.Point) bool {
	if c == nil || c.fired || p != c.Point {
		return false
	}
	c.seen++
	if c.seen == c.N {
		c.fired = true
		return true
	}
	return false
}

// Fired reports whether the crash happened.
func (c *Crasher) Fired() bool { return c != nil && c.fired }
