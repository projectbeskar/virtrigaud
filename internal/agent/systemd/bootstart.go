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

package systemd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// AgentUnit is the per-host agent's unit.
const AgentUnit = "vrtg-agent.service"

// SetupUnit is the unit that creates the agent's users and directories.
const SetupUnit = "vrtg-agent-setup.service"

// BootStartMarker is the file, in the boot-start unit's runtime directory,
// whose existence records that the decision to start the agent was made in
// the userspace started by the boot (no soft reboot since). After a soft
// reboot the marker is BootStartMarkerName of that soft reboot's time.
const BootStartMarker = "started"

// SoftRebootTarget is the target a soft reboot (systemctl soft-reboot,
// systemd 254 and later) reaches just before the new userspace starts.
const SoftRebootTarget = "soft-reboot.target"

// propActiveEnterMono and propInactiveExitMono are the unit properties read
// by LastSoftReboot and StartedSince.
const (
	propActiveEnterMono  = "ActiveEnterTimestampMonotonic"
	propInactiveExitMono = "InactiveExitTimestampMonotonic"
)

// BootStartMarkerName is the marker's file name for the userspace whose last
// soft reboot happened at softReboot (LastSoftReboot): BootStartMarker for 0
// (no soft reboot since the boot, the name earlier versions used), and
// BootStartMarker + "." + softReboot otherwise. A marker of an earlier
// userspace has another name, so it is ignored, and each userspace's marker
// is created exclusively.
func BootStartMarkerName(softReboot uint64) string {
	if softReboot == 0 {
		return BootStartMarker
	}
	return BootStartMarker + "." + strconv.FormatUint(softReboot, 10)
}

// ErrBootStartUndecided marks a BootStart error returned before this boot's
// decision was made: nothing was started by this run, and whether the agent
// was already started this boot is unknown to it (an earlier run, an
// operator or the agent's own links may have started it). An error without
// it came after this boot's marker was made: the agent was not started by
// boot-start in this boot.
var ErrBootStartUndecided = errors.New("nothing was decided")

// undecided wraps err as a failure before this boot's decision.
func undecided(err error) error {
	return fmt.Errorf("boot-start: %w (%w)", err, ErrBootStartUndecided)
}

// UnitStarter asks the service manager about a unit and starts it.
type UnitStarter interface {
	// Enabled reports whether unit is enabled (systemctl is-enabled
	// succeeds: enabled, enabled-runtime, static, ...).
	Enabled(ctx context.Context, unit string) (bool, error)
	// StartNoBlock queues a start job for unit without waiting for it.
	StartNoBlock(ctx context.Context, unit string) error
	// Active reports whether unit is active (systemctl is-active succeeds).
	Active(ctx context.Context, unit string) (bool, error)
	// LastSoftReboot reports when the current userspace began with a soft
	// reboot, in microseconds of the monotonic clock, or 0 when it is the
	// userspace the boot started (no soft reboot since, or a systemd without
	// soft reboot).
	LastSoftReboot(ctx context.Context) (uint64, error)
	// StartedSince reports whether unit was started (left the inactive
	// state) after the monotonic time since, in microseconds.
	StartedSince(ctx context.Context, unit string, since uint64) (bool, error)
}

// LastSoftReboot implements UnitStarter from SoftRebootTarget's
// ActiveEnterTimestampMonotonic. The monotonic clock does not restart at a
// soft reboot, and the manager keeps the timestamps of the units it keeps
// loaded across a soft reboot, a daemon-reexec and a daemon-reload. The
// target stays loaded (systemd-journald.service conflicts with it), so the
// value changes at each soft reboot and only then. It is 0 until the first
// soft reboot of the boot, and on a systemd without soft reboot (before
// 254), where the target does not exist and systemctl shows 0. Were the
// target ever unloaded, the value would read 0: this boot would then look
// like the boot's own, and StartedSince(0) holds for any agent started since
// the boot, so the error could only miss a start, never make a second one.
func (s Systemctl) LastSoftReboot(ctx context.Context) (uint64, error) {
	return s.showUint(ctx, SoftRebootTarget, propActiveEnterMono)
}

// StartedSince implements UnitStarter from the unit's
// InactiveExitTimestampMonotonic. The manager keeps that timestamp across a
// soft reboot while the unit stays loaded (the monotonic clock goes on), so a
// non-zero value alone does not mean "started in this userspace": only a
// value after since does. A unit the manager unloads (an inactive unit that
// nothing references, such as a disabled agent) reads 0, never started;
// BootStart does not start a disabled agent.
func (s Systemctl) StartedSince(ctx context.Context, unit string, since uint64) (bool, error) {
	v, err := s.showUint(ctx, unit, propInactiveExitMono)
	if err != nil {
		return false, err
	}
	return v > since, nil
}

// showUint reads one unsigned property of unit with systemctl show.
func (s Systemctl) showUint(ctx context.Context, unit, prop string) (uint64, error) {
	out, err := exec.CommandContext(ctx, s.bin(), "show", "--property="+prop, "--value", unit).Output() // #nosec G204 -- fixed binary, a unit and property name of ours
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %s %q: %w", unit, prop, strings.TrimSpace(string(out)), err)
	}
	return v, nil
}

// Active implements UnitStarter: systemctl is-active --quiet exits 0 for an
// active unit and non-zero otherwise; a systemctl that cannot be run is an
// error.
func (s Systemctl) Active(ctx context.Context, unit string) (bool, error) {
	err := exec.CommandContext(ctx, s.bin(), "is-active", "--quiet", unit).Run() // #nosec G204 -- fixed binary, a unit name of ours
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, fmt.Errorf("systemctl is-active %s: %w", unit, err)
}

var _ UnitStarter = Systemctl{}

// UnitFailures reports whether a unit is in the failed state.
type UnitFailures interface {
	// Failed reports whether unit failed (systemctl is-failed succeeds).
	Failed(ctx context.Context, unit string) (bool, error)
}

// Failed implements UnitFailures: systemctl is-failed --quiet exits 0 for a
// failed unit and non-zero otherwise; a systemctl that cannot be run is an
// error.
func (s Systemctl) Failed(ctx context.Context, unit string) (bool, error) {
	err := exec.CommandContext(ctx, s.bin(), "is-failed", "--quiet", unit).Run() // #nosec G204 -- fixed binary, a unit name of ours
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, fmt.Errorf("systemctl is-failed %s: %w", unit, err)
}

var _ UnitFailures = Systemctl{}

// Enabled implements UnitStarter: systemctl is-enabled --quiet exits 0 for an
// enabled unit and non-zero otherwise (disabled, masked, not found, ...); a
// systemctl that cannot be run is an error.
func (s Systemctl) Enabled(ctx context.Context, unit string) (bool, error) {
	err := exec.CommandContext(ctx, s.bin(), "is-enabled", "--quiet", unit).Run() // #nosec G204 -- fixed binary, a unit name of ours
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, fmt.Errorf("systemctl is-enabled %s: %w", unit, err)
}

// StartNoBlock implements UnitStarter.
func (s Systemctl) StartNoBlock(ctx context.Context, unit string) error {
	if out, err := exec.CommandContext(ctx, s.bin(), "--no-block", "start", unit).CombinedOutput(); err != nil { // #nosec G204 -- fixed binary, a unit name of ours
		return fmt.Errorf("systemctl --no-block start %s: %w: %s", unit, err, out)
	}
	return nil
}

// BootStartOutcome is what BootStart did.
type BootStartOutcome int

// The outcomes of BootStart. "This boot" is the current userspace: the one
// the boot started, or the one the last soft reboot started.
const (
	// BootStartAlreadyDecided: this boot's marker existed; the decision was
	// made before (the unit was restarted, by hand or by a package script),
	// so nothing is done.
	BootStartAlreadyDecided BootStartOutcome = iota
	// BootStartNotEnabled: the agent's unit is not enabled; it is not
	// started.
	BootStartNotEnabled
	// BootStartStarted: a start job for the agent was queued.
	BootStartStarted
	// BootStartStartedByOther: the agent was already started during this
	// boot (by hand, or by its own [Install] links): that is the boot's
	// decision; the marker is made and nothing is started, so a later
	// restart of the boot-start unit never starts an agent stopped since.
	BootStartStartedByOther
	// BootStartSetupNotActive: the setup unit is not active (it failed):
	// nothing was decided, no marker was made, so a later run in the same
	// boot (after the cause is fixed) still acts once.
	BootStartSetupNotActive
)

// BootStart starts the agent at most once per boot, a soft reboot counting
// as a boot, once the setup unit succeeded. It first reads when the current
// userspace began (LastSoftReboot: 0 for the boot, the soft reboot's time
// otherwise); this boot's marker is BootStartMarkerName of that time, in
// runtimeDir (a directory that survives restarts of the unit and a soft
// reboot, but not a reboot). If the marker exists, the decision was made
// earlier in this boot and nothing is done, so a restart of the boot-start
// unit never starts an agent stopped for maintenance. A marker of an earlier
// userspace has another name and does not count: a soft reboot re-arms the
// start as a reboot does. If the agent was started since this boot began,
// that is the boot's decision: the marker is made and nothing is started.
// While the setup unit is not active it does nothing and makes no marker
// (BootStartSetupNotActive). Otherwise it creates the marker exclusively
// (if another run made it first, nothing is done), and the marker is
// created before the start, so an interruption between the two can only
// miss a start (the fail-safe direction: an operator can start the agent),
// never start it twice. Then, only while unit is enabled, it queues its start
// without waiting for it. Any error leaves the agent unstarted by this run;
// an error before this boot's marker was made wraps ErrBootStartUndecided
// (the time of the last soft reboot is read first, so a run can fail that
// way even when the marker exists).
func BootStart(ctx context.Context, runtimeDir, unit string, s UnitStarter) (BootStartOutcome, error) {
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return 0, undecided(fmt.Errorf("runtime directory %q is not an absolute path", runtimeDir))
	}
	softReboot, err := s.LastSoftReboot(ctx)
	if err != nil {
		return 0, undecided(err)
	}
	marker := filepath.Join(runtimeDir, BootStartMarkerName(softReboot))
	if _, err := os.Lstat(marker); err == nil && !guard.Off(guard.BootStartMarker) {
		return BootStartAlreadyDecided, nil
	}
	// Any start of the agent during this boot is the boot's decision.
	if started, err := s.StartedSince(ctx, unit, softReboot); err != nil {
		return 0, undecided(err)
	} else if started && !guard.Off(guard.BootStartStartedSince) {
		if err := createMarker(marker); err != nil && !errors.Is(err, fs.ErrExist) {
			return 0, undecided(err)
		}
		return BootStartStartedByOther, nil
	}
	if ok, err := s.Active(ctx, SetupUnit); err != nil {
		return 0, undecided(err)
	} else if !ok {
		return BootStartSetupNotActive, nil
	}
	// Another run may have made the marker since the check above: the
	// exclusive create decides which run acts.
	switch err := createMarker(marker); {
	case errors.Is(err, fs.ErrExist):
		if !guard.Off(guard.BootStartMarkerExcl) {
			return BootStartAlreadyDecided, nil
		}
	case err != nil:
		return 0, undecided(err)
	}
	// This boot's marker exists: from here, a failure misses this boot's
	// start (no later run starts the agent).
	enabled, err := s.Enabled(ctx, unit)
	if err != nil {
		return 0, fmt.Errorf("boot-start: %w", err)
	}
	if !enabled {
		return BootStartNotEnabled, nil
	}
	if err := s.StartNoBlock(ctx, unit); err != nil {
		return 0, fmt.Errorf("boot-start: %w", err)
	}
	return BootStartStarted, nil
}

// createMarker creates the marker file path, exclusively: an error matching
// fs.ErrExist means it was there.
func createMarker(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- in the unit's own runtime directory
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if err != nil {
		return fmt.Errorf("create the marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close the marker: %w", err)
	}
	return nil
}
