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

package systemd_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// fakeStarter records calls and whether the marker existed at each.
type fakeStarter struct {
	marker         string
	enabled        bool
	enabledErr     error
	startErr       error
	starts         int
	markerAtStart  bool
	enabledChecked int
	setupFailed    bool
	// startedThisBoot is the agent's InactiveExitTimestampMonotonic != 0.
	startedThisBoot bool
}

func (f *fakeStarter) StartedThisBoot(_ context.Context, unit string) (bool, error) {
	if unit != systemd.AgentUnit {
		panic("unexpected unit " + unit)
	}
	return f.startedThisBoot, nil
}

func (f *fakeStarter) Active(_ context.Context, unit string) (bool, error) {
	if unit != systemd.SetupUnit {
		panic("unexpected unit " + unit)
	}
	return !f.setupFailed, nil
}

func (f *fakeStarter) Enabled(context.Context, string) (bool, error) {
	f.enabledChecked++
	return f.enabled, f.enabledErr
}

func (f *fakeStarter) StartNoBlock(context.Context, string) error {
	f.starts++
	_, err := os.Stat(f.marker)
	f.markerAtStart = err == nil
	return f.startErr
}

// TestBootStartOncePerBoot: the first run creates the marker, then starts
// the enabled agent (the marker exists before the start); every later run
// in the same boot (a restart of the unit, by hand or by a package script)
// finds the marker and starts nothing; a disabled agent is not started, and
// the decision still holds for the boot.
func TestBootStartOncePerBoot(t *testing.T) {
	dir := t.TempDir()
	f := &fakeStarter{marker: filepath.Join(dir, systemd.BootStartMarker), enabled: true}
	got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f)
	if err != nil || got != systemd.BootStartStarted || f.starts != 1 || !f.markerAtStart {
		t.Fatalf("first run: %v %v, starts %d, marker before the start %v", got, err, f.starts, f.markerAtStart)
	}
	for i := 0; i < 3; i++ {
		got, err = systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f)
		if err != nil || got != systemd.BootStartAlreadyDecided {
			t.Fatalf("restart %d: %v %v", i, got, err)
		}
	}
	if f.starts != 1 || f.enabledChecked != 1 {
		t.Fatalf("starts %d, enablement checks %d: a restart must start nothing", f.starts, f.enabledChecked)
	}

	disabled := &fakeStarter{marker: filepath.Join(t.TempDir(), systemd.BootStartMarker)}
	dir = filepath.Dir(disabled.marker)
	if got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, disabled); err != nil || got != systemd.BootStartNotEnabled || disabled.starts != 0 {
		t.Fatalf("disabled: %v %v starts %d", got, err, disabled.starts)
	}
	disabled.enabled = true
	if got, _ := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, disabled); got != systemd.BootStartAlreadyDecided || disabled.starts != 0 {
		t.Fatal("enabling later in the boot and restarting the unit started the agent")
	}
}

// TestBootStartWaitsForSetup: while the setup unit is not active the
// command decides nothing (no marker, no start); once setup succeeds, a
// later run in the same boot acts, once.
func TestBootStartWaitsForSetup(t *testing.T) {
	dir := t.TempDir()
	f := &fakeStarter{marker: filepath.Join(dir, systemd.BootStartMarker), enabled: true, setupFailed: true}
	got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f)
	if err != nil || got != systemd.BootStartSetupNotActive || f.starts != 0 {
		t.Fatalf("setup failed: %v %v, starts %d", got, err, f.starts)
	}
	if _, err := os.Stat(f.marker); err == nil {
		t.Fatal("a marker was made although setup failed")
	}
	f.setupFailed = false
	if got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f); err != nil || got != systemd.BootStartStarted || f.starts != 1 {
		t.Fatalf("after setup: %v %v, starts %d", got, err, f.starts)
	}
	if got, _ := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f); got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatal("a second run started the agent again")
	}
}

// TestBootStartAfterAManualStartThisBoot: setup fails at boot (nothing is
// decided), the operator fixes it, starts the agent by hand and stops it for
// maintenance; a package's try-restart of the boot-start unit then finds
// that the agent was already started this boot, makes the marker and starts
// nothing. The first boot of an agent never started yet starts it once.
func TestBootStartAfterAManualStartThisBoot(t *testing.T) {
	dir := t.TempDir()
	f := &fakeStarter{marker: filepath.Join(dir, systemd.BootStartMarker), enabled: true, setupFailed: true}
	if got, _ := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f); got != systemd.BootStartSetupNotActive {
		t.Fatalf("boot with setup failed: %v", got)
	}
	f.setupFailed, f.startedThisBoot = false, true // fixed, started by hand, stopped again
	got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f)
	if err != nil || got != systemd.BootStartStartedByOther || f.starts != 0 {
		t.Fatalf("try-restart after a manual start: %v %v, starts %d", got, err, f.starts)
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatal("the boot's decision was not recorded")
	}
	if got, _ := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, f); got != systemd.BootStartAlreadyDecided || f.starts != 0 {
		t.Fatal("a further restart started the agent")
	}

	first := t.TempDir()
	g := &fakeStarter{marker: filepath.Join(first, systemd.BootStartMarker), enabled: true}
	if got, err := systemd.BootStart(context.Background(), first, systemd.AgentUnit, g); err != nil || got != systemd.BootStartStarted || g.starts != 1 {
		t.Fatalf("first boot: %v %v, starts %d", got, err, g.starts)
	}
}

// TestBootStartFailsSafe: any failure leaves the agent unstarted, and the
// marker created before a failed start is kept (a later run starts nothing:
// at most a missed start, never a second one).
func TestBootStartFailsSafe(t *testing.T) {
	ctx := context.Background()
	if _, err := systemd.BootStart(ctx, "relative", systemd.AgentUnit, &fakeStarter{enabled: true}); err == nil {
		t.Fatal("a relative runtime directory was accepted")
	}
	f := &fakeStarter{enabled: true}
	if _, err := systemd.BootStart(ctx, filepath.Join(t.TempDir(), "missing"), systemd.AgentUnit, f); err == nil || f.starts != 0 {
		t.Fatalf("no runtime directory: %v, starts %d", err, f.starts)
	}
	dir := t.TempDir()
	f = &fakeStarter{marker: filepath.Join(dir, systemd.BootStartMarker), enabledErr: errors.New("no systemctl")}
	if _, err := systemd.BootStart(ctx, dir, systemd.AgentUnit, f); err == nil || f.starts != 0 {
		t.Fatalf("enablement unknown: %v, starts %d", err, f.starts)
	}
	dir = t.TempDir()
	f = &fakeStarter{marker: filepath.Join(dir, systemd.BootStartMarker), enabled: true, startErr: errors.New("refused")}
	if _, err := systemd.BootStart(ctx, dir, systemd.AgentUnit, f); err == nil {
		t.Fatal("a failed start was not reported")
	}
	if got, err := systemd.BootStart(ctx, dir, systemd.AgentUnit, f); err != nil || got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatalf("after a failed start: %v %v starts %d", got, err, f.starts)
	}
}

// TestSystemctlStartedThisBoot: InactiveExitTimestampMonotonic 0 means the
// unit was not started this boot, any other number that it was; output that
// is not a number is an error (and the agent is then not started).
func TestSystemctlStartedThisBoot(t *testing.T) {
	for out, want := range map[string]bool{"0": false, "123456789": true} {
		bin := filepath.Join(t.TempDir(), "systemctl")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho "+out+"\n"), 0o700); err != nil { // #nosec G306 -- a test script
			t.Fatal(err)
		}
		got, err := systemd.Systemctl{Path: bin}.StartedThisBoot(context.Background(), systemd.AgentUnit)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", out, got, err, want)
		}
	}
	bin := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho n/a\n"), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	if _, err := (systemd.Systemctl{Path: bin}).StartedThisBoot(context.Background(), systemd.AgentUnit); err == nil {
		t.Fatal("a value that is not a number was accepted")
	}
}
