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
}

func (s *startRecorder) Enabled(context.Context, string) (bool, error) { return s.enabled, nil }

func (s *startRecorder) Active(context.Context, string) (bool, error) { return !s.setupFailed, nil }

func (s *startRecorder) StartedThisBoot(context.Context, string) (bool, error) { return false, nil }

func (s *startRecorder) StartNoBlock(_ context.Context, unit string) error {
	if unit != systemd.AgentUnit {
		panic("unexpected unit " + unit)
	}
	s.starts++
	_, err := os.Stat(filepath.Join(s.dir, systemd.BootStartMarker))
	s.markerAtStart = err == nil
	return nil
}

// TestBootStartCommand: with no marker the command creates it and then
// queues the agent's start; with the marker present (a restart of the unit
// in the same boot) it starts nothing; the runtime directory comes from
// systemd's $RUNTIME_DIRECTORY.
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
	other := t.TempDir()
	s2 := &startRecorder{dir: other}
	if code := bootStart([]string{"--runtime-dir", other}, logr.Discard(), &stderr, s2); code != 0 || s2.starts != 0 {
		t.Fatalf("disabled: exit %d, starts %d", code, s2.starts)
	}
	var logs bytes.Buffer
	if code := bootStart([]string{"--runtime-dir", filepath.Join(other, "missing")}, newLogger(&logs), &stderr, &startRecorder{enabled: true}); code != 1 {
		t.Fatalf("missing runtime directory: exit %d", code)
	}
	// A failure is a warning that tells the operator what to do.
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "start it by hand: systemctl start vrtg-agent.service") {
		t.Fatalf("failure log: %s", logs.String())
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
