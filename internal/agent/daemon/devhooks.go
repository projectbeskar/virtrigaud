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
	"strings"
	"sync"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// The development-only crash hook environment. A development build reads
// them at start-up to kill its own process at a named point of the
// operation path, so that a test can prove that every crash point recovers
// with at most one effect per operation. A release build ignores them.
const (
	// DevCrashEnv names the crash points, comma separated. The process is
	// killed (SIGKILL) when the last point is reached after the earlier ones
	// were reached in order, for example "before-accept,after-store-commit"
	// for the commit of an admission. The point names are those of the
	// journal and the operation store (journal.Point, opstore.Point*).
	DevCrashEnv = "VRTG_AGENT_DEV_CRASH_AT"
	// DevAnchorRotateEnv sets the anchor log size that triggers a rotation,
	// in bytes, so that a test reaches the anchor rotation's crash point.
	DevAnchorRotateEnv = "VRTG_AGENT_DEV_ANCHOR_ROTATE_BYTES"
)

// crashSequence matches a sequence of crash points.
type crashSequence struct {
	mu     sync.Mutex
	points []journal.Point
	next   int
	kill   func()
}

// parseCrashSequence parses a DevCrashEnv value; empty gives nil.
func parseCrashSequence(v string, kill func()) *crashSequence {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	c := &crashSequence{kill: kill}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.points = append(c.points, journal.Point(p))
		}
	}
	if len(c.points) == 0 {
		return nil
	}
	return c
}

// at is the journal crash hook: it kills the process when the sequence is
// complete. It reports true then, so that a kill that has not taken effect
// yet still stops the store.
func (c *crashSequence) at(p journal.Point) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.points) || p != c.points[c.next] {
		return false
	}
	c.next++
	if c.next < len(c.points) {
		return false
	}
	c.kill()
	return true
}
