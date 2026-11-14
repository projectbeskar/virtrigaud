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

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// TestCrashSequence: the hook fires once, at the last point of the
// sequence, only after the earlier points were reached in order.
func TestCrashSequence(t *testing.T) {
	kills := 0
	c := parseCrashSequence(" before-accept , after-store-commit ", func() { kills++ })
	if c == nil || len(c.points) != 2 {
		t.Fatalf("parsed %+v", c)
	}
	for _, p := range []journal.Point{"after-store-commit", "after-anchor", "before-accept", "after-anchor"} {
		if c.at(p) {
			t.Fatalf("fired early at %s", p)
		}
	}
	if !c.at("after-store-commit") || kills != 1 {
		t.Fatalf("did not fire at the end of the sequence (kills %d)", kills)
	}
	if c.at("after-store-commit") || kills != 1 {
		t.Fatal("fired twice")
	}
	if parseCrashSequence("  ", nil) != nil || parseCrashSequence(",", nil) != nil {
		t.Fatal("an empty sequence arms a hook")
	}
}

// TestReleaseBuildIgnoresTheCrashEnvironment: only a development build arms
// the crash hook.
func TestReleaseBuildIgnoresTheCrashEnvironment(t *testing.T) {
	t.Setenv(DevCrashEnv, "before-accept")
	t.Setenv(DevAnchorRotateEnv, "128")
	hooks, rotate := devJournalHooks(logr.Discard())
	if config.DevBuild != (hooks.Crash != nil) || config.DevBuild != (rotate == 128) {
		t.Fatalf("development build %v: crash hook %v, rotation %d", config.DevBuild, hooks.Crash != nil, rotate)
	}
}
