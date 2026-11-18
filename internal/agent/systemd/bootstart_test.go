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
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// fakeStarter is a service manager as the lab showed it: a monotonic clock
// that goes on across soft reboots, the agent's InactiveExitTimestampMonotonic
// kept across a soft reboot, and soft-reboot.target's
// ActiveEnterTimestampMonotonic set at each soft reboot. It records calls and
// whether this boot's marker existed at each start.
type fakeStarter struct {
	dir            string
	enabled        bool
	enabledErr     error
	startErr       error
	softRebootErr  error
	starts         int
	markerAtStart  bool
	enabledChecked int
	setupFailed    bool
	// now is the monotonic clock (µs), advanced by each event.
	now uint64
	// softReboot is soft-reboot.target's ActiveEnterTimestampMonotonic: 0
	// until the first soft reboot (and always on systemd 252).
	softReboot uint64
	// agentStarted is the agent's InactiveExitTimestampMonotonic.
	agentStarted uint64
}

func newFake(t *testing.T) *fakeStarter {
	return &fakeStarter{dir: t.TempDir(), enabled: true, now: 1000}
}

func (f *fakeStarter) tick() uint64 { f.now += 10; return f.now }

// startByHand: an operator (or the agent's [Install] links) starts the agent.
func (f *fakeStarter) startByHand() { f.agentStarted = f.tick() }

// doSoftReboot: everything stops (the agent's timestamps are kept), the
// new userspace begins; the runtime directory stays.
func (f *fakeStarter) doSoftReboot() { f.softReboot = f.tick() }

// markerPath is the current boot's marker.
func (f *fakeStarter) markerPath() string {
	return filepath.Join(f.dir, systemd.BootStartMarkerName(f.softReboot))
}

func (f *fakeStarter) run() (systemd.BootStartOutcome, error) {
	return systemd.BootStart(context.Background(), f.dir, systemd.AgentUnit, f)
}

func (f *fakeStarter) LastSoftReboot(context.Context) (uint64, error) {
	return f.softReboot, f.softRebootErr
}

func (f *fakeStarter) StartedSince(_ context.Context, unit string, since uint64) (bool, error) {
	if unit != systemd.AgentUnit {
		panic("unexpected unit " + unit)
	}
	return f.agentStarted > since, nil
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
	_, err := os.Stat(f.markerPath())
	f.markerAtStart = err == nil
	if f.startErr != nil {
		return f.startErr
	}
	f.agentStarted = f.tick()
	return nil
}

// TestBootStartOncePerBoot: the first run creates the marker, then starts
// the enabled agent (the marker exists before the start); every later run
// in the same boot (a restart of the unit, by hand or by a package script)
// finds the marker and starts nothing; a disabled agent is not started, and
// the decision still holds for the boot.
func TestBootStartOncePerBoot(t *testing.T) {
	f := newFake(t)
	got, err := f.run()
	if err != nil || got != systemd.BootStartStarted || f.starts != 1 || !f.markerAtStart {
		t.Fatalf("first run: %v %v, starts %d, marker before the start %v", got, err, f.starts, f.markerAtStart)
	}
	if _, err := os.Stat(filepath.Join(f.dir, systemd.BootStartMarker)); err != nil {
		t.Fatalf("the boot's marker is not %q: %v", systemd.BootStartMarker, err)
	}
	for i := 0; i < 3; i++ {
		got, err = f.run()
		if err != nil || got != systemd.BootStartAlreadyDecided {
			t.Fatalf("restart %d: %v %v", i, got, err)
		}
	}
	if f.starts != 1 || f.enabledChecked != 1 {
		t.Fatalf("starts %d, enablement checks %d: a restart must start nothing", f.starts, f.enabledChecked)
	}

	disabled := newFake(t)
	disabled.enabled = false
	if got, err := disabled.run(); err != nil || got != systemd.BootStartNotEnabled || disabled.starts != 0 {
		t.Fatalf("disabled: %v %v starts %d", got, err, disabled.starts)
	}
	disabled.enabled = true
	if got, _ := disabled.run(); got != systemd.BootStartAlreadyDecided || disabled.starts != 0 {
		t.Fatal("enabling later in the boot and restarting the unit started the agent")
	}
}

// TestBootStartWaitsForSetup: while the setup unit is not active the
// command decides nothing (no marker, no start); once setup succeeds, a
// later run in the same boot acts, once.
func TestBootStartWaitsForSetup(t *testing.T) {
	f := newFake(t)
	f.setupFailed = true
	got, err := f.run()
	if err != nil || got != systemd.BootStartSetupNotActive || f.starts != 0 {
		t.Fatalf("setup failed: %v %v, starts %d", got, err, f.starts)
	}
	if _, err := os.Stat(f.markerPath()); err == nil {
		t.Fatal("a marker was made although setup failed")
	}
	f.setupFailed = false
	if got, err := f.run(); err != nil || got != systemd.BootStartStarted || f.starts != 1 {
		t.Fatalf("after setup: %v %v, starts %d", got, err, f.starts)
	}
	if got, _ := f.run(); got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatal("a second run started the agent again")
	}
}

// TestBootStartAfterAManualStartThisBoot: setup fails at boot (nothing is
// decided), the operator fixes it, starts the agent by hand and stops it for
// maintenance; a package's try-restart of the boot-start unit then finds
// that the agent was already started this boot, makes the marker and starts
// nothing. The first boot of an agent never started yet starts it once.
func TestBootStartAfterAManualStartThisBoot(t *testing.T) {
	f := newFake(t)
	f.setupFailed = true
	if got, _ := f.run(); got != systemd.BootStartSetupNotActive {
		t.Fatalf("boot with setup failed: %v", got)
	}
	f.setupFailed = false
	f.startByHand() // fixed, started by hand, stopped again
	got, err := f.run()
	if err != nil || got != systemd.BootStartStartedByOther || f.starts != 0 {
		t.Fatalf("try-restart after a manual start: %v %v, starts %d", got, err, f.starts)
	}
	if _, err := os.Stat(f.markerPath()); err != nil {
		t.Fatal("the boot's decision was not recorded")
	}
	if got, _ := f.run(); got != systemd.BootStartAlreadyDecided || f.starts != 0 {
		t.Fatal("a further restart started the agent")
	}

	g := newFake(t)
	if got, err := g.run(); err != nil || got != systemd.BootStartStarted || g.starts != 1 {
		t.Fatalf("first boot: %v %v, starts %d", got, err, g.starts)
	}
}

// TestBootStartSoftReboot: a soft reboot keeps the runtime directory and the
// agent's start timestamp, and re-arms the start exactly as a reboot does:
// once per soft reboot, whether the agent was running or stopped for
// maintenance before it, and restarts of the unit after it start nothing.
func TestBootStartSoftReboot(t *testing.T) {
	f := newFake(t)
	if got, err := f.run(); err != nil || got != systemd.BootStartStarted || f.starts != 1 {
		t.Fatalf("boot: %v %v, starts %d", got, err, f.starts)
	}
	for i := 1; i <= 2; i++ { // two soft reboots in a row
		f.doSoftReboot()
		got, err := f.run()
		if err != nil || got != systemd.BootStartStarted || f.starts != 1+i || !f.markerAtStart {
			t.Fatalf("soft reboot %d: %v %v, starts %d, marker before the start %v", i, got, err, f.starts, f.markerAtStart)
		}
		for j := 0; j < 2; j++ {
			if got, _ := f.run(); got != systemd.BootStartAlreadyDecided || f.starts != 1+i {
				t.Fatalf("restart %d after soft reboot %d: %v, starts %d", j, i, got, f.starts)
			}
		}
	}
	for _, name := range []string{systemd.BootStartMarker, systemd.BootStartMarkerName(f.softReboot)} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("marker %s: %v", name, err)
		}
	}

	// A maintenance stop before the soft reboot: the soft reboot starts the
	// agent, as a reboot would.
	m := newFake(t)
	if got, _ := m.run(); got != systemd.BootStartStarted {
		t.Fatalf("boot: %v", got)
	}
	// systemctl stop vrtg-agent: nothing changes for boot-start.
	if got, _ := m.run(); got != systemd.BootStartAlreadyDecided || m.starts != 1 {
		t.Fatal("a restart after the maintenance stop started the agent")
	}
	m.doSoftReboot()
	if got, err := m.run(); err != nil || got != systemd.BootStartStarted || m.starts != 2 {
		t.Fatalf("soft reboot after a maintenance stop: %v %v, starts %d", got, err, m.starts)
	}

	// Disabled across the soft reboot: not started, and enabling it later
	// in the soft boot does not make a restart start it.
	d := newFake(t)
	d.enabled = false
	d.doSoftReboot()
	if got, _ := d.run(); got != systemd.BootStartNotEnabled || d.starts != 0 {
		t.Fatalf("disabled: %v, starts %d", got, d.starts)
	}
	d.enabled = true
	if got, _ := d.run(); got != systemd.BootStartAlreadyDecided || d.starts != 0 {
		t.Fatal("enabling after the soft reboot and restarting the unit started the agent")
	}
}

// TestBootStartSoftRebootStartedByOther: after a soft reboot the previous
// userspace's marker is still there; when the agent was already started in
// the new userspace (by hand, or by its own [Install] links on a classic
// host), that is the soft boot's decision: nothing is started, now or at a
// later restart of the unit.
func TestBootStartSoftRebootStartedByOther(t *testing.T) {
	f := newFake(t)
	if got, _ := f.run(); got != systemd.BootStartStarted {
		t.Fatalf("boot: %v", got)
	}
	f.doSoftReboot()
	f.startByHand()
	got, err := f.run()
	if err != nil || got != systemd.BootStartStartedByOther || f.starts != 1 {
		t.Fatalf("soft reboot, agent already started: %v %v, starts %d", got, err, f.starts)
	}
	if _, err := os.Stat(f.markerPath()); err != nil {
		t.Fatal("the soft boot's decision was not recorded")
	}
	if got, _ := f.run(); got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatal("a later restart started the agent")
	}
}

// TestBootStartNoSoftReboot: on a systemd without soft reboot (252: no
// soft-reboot.target, LastSoftReboot is always 0) the behaviour is the
// once-per-boot one, with the marker named BootStartMarker; a marker of that
// name left by an earlier version in this boot is this boot's decision.
func TestBootStartNoSoftReboot(t *testing.T) {
	f := newFake(t)
	if got, _ := f.run(); got != systemd.BootStartStarted || f.starts != 1 {
		t.Fatalf("boot: %v, starts %d", got, f.starts)
	}
	f.agentStarted = 0 // whatever the agent's timestamp, the marker decides
	if got, _ := f.run(); got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatalf("restart: %v, starts %d", got, f.starts)
	}

	old := newFake(t)
	if err := os.WriteFile(filepath.Join(old.dir, systemd.BootStartMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := old.run(); got != systemd.BootStartAlreadyDecided || old.starts != 0 {
		t.Fatalf("marker of an earlier version: %v, starts %d", got, old.starts)
	}
}

// TestBootStartCrashBetweenMarkerAndStart: an interruption after the marker
// and before the start misses this boot's start (an operator starts the
// agent by hand) and never starts the agent twice; the next soft reboot
// starts it again.
func TestBootStartCrashBetweenMarkerAndStart(t *testing.T) {
	for _, soft := range []bool{false, true} {
		f := newFake(t)
		if soft {
			if got, _ := f.run(); got != systemd.BootStartStarted {
				t.Fatalf("boot: %v", got)
			}
			f.doSoftReboot()
		}
		// The run that crashed: it made this boot's marker, nothing more.
		if err := os.WriteFile(f.markerPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		before := f.starts
		for i := 0; i < 2; i++ {
			if got, err := f.run(); err != nil || got != systemd.BootStartAlreadyDecided || f.starts != before {
				t.Fatalf("soft %v, run %d after the crash: %v %v, starts %d", soft, i, got, err, f.starts)
			}
		}
		f.doSoftReboot()
		if got, _ := f.run(); got != systemd.BootStartStarted || f.starts != before+1 {
			t.Fatalf("soft %v, next soft reboot: %v, starts %d", soft, got, f.starts)
		}
	}
}

// TestBootStartFailsSafe: any failure leaves the agent unstarted, and the
// marker created before a failed start is kept (a later run starts nothing:
// at most a missed start, never a second one).
func TestBootStartFailsSafe(t *testing.T) {
	ctx := context.Background()
	if _, err := systemd.BootStart(ctx, "relative", systemd.AgentUnit, newFake(t)); err == nil {
		t.Fatal("a relative runtime directory was accepted")
	}
	f := newFake(t)
	if _, err := systemd.BootStart(ctx, filepath.Join(f.dir, "missing"), systemd.AgentUnit, f); err == nil || f.starts != 0 {
		t.Fatalf("no runtime directory: %v, starts %d", err, f.starts)
	}
	f = newFake(t)
	f.softRebootErr = errors.New("no systemctl")
	if _, err := f.run(); err == nil || f.starts != 0 {
		t.Fatalf("soft reboot time unknown: %v, starts %d", err, f.starts)
	}
	f = newFake(t)
	f.enabledErr = errors.New("no systemctl")
	if _, err := f.run(); err == nil || f.starts != 0 {
		t.Fatalf("enablement unknown: %v, starts %d", err, f.starts)
	}
	f = newFake(t)
	f.startErr = errors.New("refused")
	if _, err := f.run(); err == nil {
		t.Fatal("a failed start was not reported")
	}
	if got, err := f.run(); err != nil || got != systemd.BootStartAlreadyDecided || f.starts != 1 {
		t.Fatalf("after a failed start: %v %v starts %d", got, err, f.starts)
	}
}

// fakeSystemctl writes a systemctl that answers "show --property=P --value
// U" with the value given for P (any unit), and fails for anything else.
func fakeSystemctl(t *testing.T, values map[string]string) systemd.Systemctl {
	t.Helper()
	script := "#!/bin/sh\n[ \"$1\" = show ] || exit 9\ncase \"$2\" in\n"
	for p, v := range values {
		script += "--property=" + p + ") echo '" + v + "' ;;\n"
	}
	script += "*) exit 8 ;;\nesac\n"
	bin := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	return systemd.Systemctl{Path: bin}
}

// TestSystemctlStartedSince: the unit counts as started only when its
// InactiveExitTimestampMonotonic is after since (0: never started; a value
// kept from before the last soft reboot: not started since); output that is
// not a number is an error (and the agent is then not started).
func TestSystemctlStartedSince(t *testing.T) {
	for _, c := range []struct {
		out   string
		since uint64
		want  bool
	}{{"0", 0, false}, {"123456789", 0, true}, {"123456789", 200000000, false}, {"123456789", 123456789, false}, {"300000000", 200000000, true}} {
		s := fakeSystemctl(t, map[string]string{"InactiveExitTimestampMonotonic": c.out})
		got, err := s.StartedSince(context.Background(), systemd.AgentUnit, c.since)
		if err != nil || got != c.want {
			t.Fatalf("%q since %d: %v %v, want %v", c.out, c.since, got, err, c.want)
		}
	}
	if _, err := fakeSystemctl(t, map[string]string{"InactiveExitTimestampMonotonic": "n/a"}).StartedSince(context.Background(), systemd.AgentUnit, 0); err == nil {
		t.Fatal("a value that is not a number was accepted")
	}
}

// TestSystemctlLastSoftReboot: soft-reboot.target's
// ActiveEnterTimestampMonotonic is the soft reboot's time; 0 (no soft reboot
// yet, or systemd 252 where the target does not exist) is the boot; output
// that is not a number, or a systemctl that fails, is an error.
func TestSystemctlLastSoftReboot(t *testing.T) {
	for out, want := range map[string]uint64{"0": 0, "252678181": 252678181} {
		got, err := fakeSystemctl(t, map[string]string{"ActiveEnterTimestampMonotonic": out}).LastSoftReboot(context.Background())
		if err != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", out, got, err, want)
		}
	}
	for _, s := range []systemd.Systemctl{
		fakeSystemctl(t, map[string]string{"ActiveEnterTimestampMonotonic": ""}),
		fakeSystemctl(t, map[string]string{"Other": "1"}),
	} {
		if _, err := s.LastSoftReboot(context.Background()); err == nil {
			t.Fatal("no soft reboot time was accepted")
		}
	}
}

// TestBootStartWithSystemctl runs BootStart against a systemctl that shows a
// soft reboot after the agent's last start: the agent is started again.
func TestBootStartWithSystemctl(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, systemd.BootStartMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho \"$*\" >>" + log + "\ncase \"$*\" in\n" +
		"'show --property=ActiveEnterTimestampMonotonic --value soft-reboot.target') echo 252678181 ;;\n" +
		"'show --property=InactiveExitTimestampMonotonic --value vrtg-agent.service') echo 203840084 ;;\n" +
		"'is-active --quiet vrtg-agent-setup.service'|'is-enabled --quiet vrtg-agent.service'|'--no-block start vrtg-agent.service') ;;\n" +
		"*) exit 9 ;;\nesac\n"
	bin := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	got, err := systemd.BootStart(context.Background(), dir, systemd.AgentUnit, systemd.Systemctl{Path: bin})
	if err != nil || got != systemd.BootStartStarted {
		t.Fatalf("after a soft reboot: %v %v", got, err)
	}
	calls, err := os.ReadFile(log) // #nosec G304 -- the test's own file
	if err != nil || !strings.Contains(string(calls), "--no-block start vrtg-agent.service") {
		t.Fatalf("calls: %s %v", calls, err)
	}
	if _, err := os.Stat(filepath.Join(dir, systemd.BootStartMarkerName(252678181))); err != nil {
		t.Fatal(err)
	}
}
