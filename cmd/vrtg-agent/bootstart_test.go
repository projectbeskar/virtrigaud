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

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// startRecorder is a UnitStarter that records whether the marker existed
// when the start was asked.
type startRecorder struct {
	dir           string
	enabled       bool
	starts        int
	markerAtStart bool
	setupFailed   bool
	// softReboot is soft-reboot.target's ActiveEnterTimestampMonotonic.
	softReboot uint64
	// agentStarted is the agent's InactiveExitTimestampMonotonic.
	agentStarted uint64
	// softRebootErr and startErr make LastSoftReboot and StartNoBlock fail.
	softRebootErr error
	startErr      error
}

func (s *startRecorder) Enabled(context.Context, string) (bool, error) { return s.enabled, nil }

func (s *startRecorder) Active(context.Context, string) (bool, error) { return !s.setupFailed, nil }

func (s *startRecorder) LastSoftReboot(context.Context) (uint64, error) {
	return s.softReboot, s.softRebootErr
}

func (s *startRecorder) StartedSince(_ context.Context, _ string, since uint64) (bool, error) {
	return s.agentStarted > since, nil
}

func (s *startRecorder) StartNoBlock(_ context.Context, unit string) error {
	if unit != systemd.AgentUnit {
		panic("unexpected unit " + unit)
	}
	s.starts++
	_, err := os.Stat(filepath.Join(s.dir, systemd.BootStartMarkerName(s.softReboot)))
	s.markerAtStart = err == nil
	if s.startErr != nil {
		return s.startErr
	}
	s.agentStarted = s.softReboot + 1
	return nil
}

// TestBootStartCommand: with no marker the command creates it and then
// queues the agent's start; with the marker present (a restart of the unit
// in the same boot) it starts nothing; a soft reboot re-arms it once; the
// runtime directory comes from systemd's $RUNTIME_DIRECTORY.
func TestBootStartCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", dir)
	s := &startRecorder{dir: dir, enabled: true}
	var stderr bytes.Buffer
	if code := bootStart(nil, logr.Discard(), &stderr, s); code != 0 || s.starts != 1 || !s.markerAtStart {
		t.Fatalf("first run: exit %d, starts %d, marker before the start %v: %s", code, s.starts, s.markerAtStart, stderr.String())
	}
	if code := bootStart(nil, logr.Discard(), &stderr, s); code != 0 || s.starts != 1 {
		t.Fatalf("restart: exit %d, starts %d", code, s.starts)
	}
	// A soft reboot keeps the runtime directory and re-arms the start, once.
	s.softReboot = 5000
	if code := bootStart(nil, logr.Discard(), &stderr, s); code != 0 || s.starts != 2 || !s.markerAtStart {
		t.Fatalf("soft reboot: exit %d, starts %d, marker before the start %v", code, s.starts, s.markerAtStart)
	}
	if code := bootStart(nil, logr.Discard(), &stderr, s); code != 0 || s.starts != 2 {
		t.Fatalf("restart after the soft reboot: exit %d, starts %d", code, s.starts)
	}
	other := t.TempDir()
	s2 := &startRecorder{dir: other}
	if code := bootStart([]string{"--runtime-dir", other}, logr.Discard(), &stderr, s2); code != 0 || s2.starts != 0 {
		t.Fatalf("disabled: exit %d, starts %d", code, s2.starts)
	}
	var logs bytes.Buffer
	if code := bootStart([]string{"--runtime-dir", filepath.Join(other, "missing")}, newLogger(&logs), &stderr, &startRecorder{enabled: true}); code != 1 {
		t.Fatalf("missing runtime directory: exit %d", code)
	}
	// A failure is a warning that tells the operator what to do. Before the
	// decision it does not claim a missed start: it says to check the agent.
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "could not determine whether the agent was started this boot") ||
		!strings.Contains(logs.String(), "systemctl status vrtg-agent.service") || strings.Contains(logs.String(), "was not started at boot") {
		t.Fatalf("failure log before the decision: %s", logs.String())
	}
	// The soft reboot time is read before the marker: a run can fail after
	// the agent was started this boot. Same warning, nothing started.
	logs.Reset()
	s.softRebootErr = errors.New("systemctl show failed")
	if code := bootStart(nil, newLogger(&logs), &stderr, s); code != 1 || s.starts != 2 {
		t.Fatalf("soft reboot time unknown: exit %d, starts %d", code, s.starts)
	}
	if !strings.Contains(logs.String(), "could not determine whether the agent was started this boot") || strings.Contains(logs.String(), "was not started at boot") {
		t.Fatalf("failure log with the marker present: %s", logs.String())
	}
	s.softRebootErr = nil
	// A failure after the decision (the start refused): this boot's start
	// was missed, and the warning says to start the agent by hand.
	logs.Reset()
	refused := t.TempDir()
	if code := bootStart([]string{"--runtime-dir", refused}, newLogger(&logs), &stderr, &startRecorder{dir: refused, enabled: true, startErr: errors.New("refused")}); code != 1 {
		t.Fatalf("start refused: exit %d", code)
	}
	if !strings.Contains(logs.String(), "the agent was not started at boot; start it by hand: systemctl start vrtg-agent.service") {
		t.Fatalf("failure log after the decision: %s", logs.String())
	}
	// Setup failed: exit 0 (the upheld unit must not fail), no marker, a
	// warning with what to do.
	third := t.TempDir()
	logs.Reset()
	if code := bootStart([]string{"--runtime-dir", third}, newLogger(&logs), &stderr, &startRecorder{dir: third, enabled: true, setupFailed: true}); code != 0 {
		t.Fatalf("setup failed: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(third, systemd.BootStartMarker)); err == nil {
		t.Fatal("a marker was made although setup failed")
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "systemctl start vrtg-agent.service") {
		t.Fatalf("setup-failed log: %s", logs.String())
	}
	if code := bootStart([]string{"extra"}, logr.Discard(), &stderr, s); code != 2 {
		t.Fatalf("bad arguments: exit %d", code)
	}
}
