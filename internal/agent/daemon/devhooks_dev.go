//go:build virtrigaud_dev

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
	"os"
	"strconv"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// devJournalHooks returns the crash hook and the anchor rotation size of
// DevCrashEnv and DevAnchorRotateEnv (development builds only).
func devJournalHooks(log logr.Logger) (journal.Hooks, int64) {
	var hooks journal.Hooks
	if c := parseCrashSequence(os.Getenv(DevCrashEnv), killSelf); c != nil {
		log.Info("development crash hook armed", "points", c.points)
		hooks.Crash = c.at
	}
	var rotate int64
	if v := os.Getenv(DevAnchorRotateEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			rotate = n
		}
	}
	return hooks, rotate
}

// killSelf kills the process as a crash would: no deferred call, no flush.
func killSelf() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }
