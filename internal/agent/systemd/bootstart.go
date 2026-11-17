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
)

// AgentUnit is the per-host agent's unit.
const AgentUnit = "vrtg-agent.service"

// SetupUnit is the unit that creates the agent's users and directories.
const SetupUnit = "vrtg-agent-setup.service"

// BootStartMarker is the file, in the boot-start unit's runtime directory,
// whose existence records that this boot's decision to start the agent was
// made.
const BootStartMarker = "started"

// UnitStarter asks the service manager about a unit and starts it.
type UnitStarter interface {
	// Enabled reports whether unit is enabled (systemctl is-enabled
	// succeeds: enabled, enabled-runtime, static, ...).
	Enabled(ctx context.Context, unit string) (bool, error)
	// StartNoBlock queues a start job for unit without waiting for it.
	StartNoBlock(ctx context.Context, unit string) error
	// Active reports whether unit is active (systemctl is-active succeeds).
	Active(ctx context.Context, unit string) (bool, error)
	// StartedThisBoot reports whether unit was started (left the inactive
	// state) at any time since this boot began.
	StartedThisBoot(ctx context.Context, unit string) (bool, error)
}

// StartedThisBoot implements UnitStarter from the unit's
// InactiveExitTimestampMonotonic: the monotonic clock restarts at each
// boot, so a non-zero value means the unit left the inactive state during
// this boot.
func (s Systemctl) StartedThisBoot(ctx context.Context, unit string) (bool, error) {
	out, err := exec.CommandContext(ctx, s.bin(), "show", "--property=InactiveExitTimestampMonotonic", "--value", unit).Output() // #nosec G204 -- fixed binary, a unit name of ours
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: InactiveExitTimestampMonotonic %q: %w", unit, strings.TrimSpace(string(out)), err)
	}
	return v != 0, nil
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

// The outcomes of BootStart.
const (
	// BootStartAlreadyDecided: the marker existed; this boot's decision was
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

// BootStart starts the agent at most once per boot, once the setup unit
// succeeded: while the setup unit is not active it does nothing and makes
// no marker (BootStartSetupNotActive). It then creates the marker in
// runtimeDir (a directory that survives restarts of the unit but
// not a reboot), exclusively: if it exists, the decision was made earlier in
// this boot and nothing is done, so a restart of the boot-start unit never
// starts an agent stopped for maintenance. The marker is created before the
// start, so an interruption between the two can only miss a start (the
// fail-safe direction: an operator can start the agent), never start it
// twice. Then, only while unit is enabled, it queues its start without
// waiting for it. Any error leaves the agent unstarted.
func BootStart(ctx context.Context, runtimeDir, unit string, s UnitStarter) (BootStartOutcome, error) {
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return 0, fmt.Errorf("boot-start: runtime directory %q is not an absolute path", runtimeDir)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, BootStartMarker)); err == nil {
		return BootStartAlreadyDecided, nil
	}
	// Any start of the agent during this boot is the boot's decision.
	if started, err := s.StartedThisBoot(ctx, unit); err != nil {
		return 0, fmt.Errorf("boot-start: %w", err)
	} else if started {
		if err := createMarker(runtimeDir); err != nil && !errors.Is(err, fs.ErrExist) {
			return 0, err
		}
		return BootStartStartedByOther, nil
	}
	if ok, err := s.Active(ctx, SetupUnit); err != nil {
		return 0, fmt.Errorf("boot-start: %w", err)
	} else if !ok {
		return BootStartSetupNotActive, nil
	}
	switch err := createMarker(runtimeDir); {
	case errors.Is(err, fs.ErrExist):
		return BootStartAlreadyDecided, nil
	case err != nil:
		return 0, err
	}
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

// createMarker creates the boot's marker in runtimeDir, exclusively: an
// error matching fs.ErrExist means it was there.
func createMarker(runtimeDir string) error {
	f, err := os.OpenFile(filepath.Join(runtimeDir, BootStartMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the unit's own runtime directory
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if err != nil {
		return fmt.Errorf("boot-start: create the marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("boot-start: close the marker: %w", err)
	}
	return nil
}
