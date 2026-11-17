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
)

// AgentUnit is the per-host agent's unit.
const AgentUnit = "vrtg-agent.service"

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
}

var _ UnitStarter = Systemctl{}

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
)

// BootStart starts the agent at most once per boot. It first creates the
// marker in runtimeDir (a directory that survives restarts of the unit but
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
	f, err := os.OpenFile(filepath.Join(runtimeDir, BootStartMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the unit's own runtime directory
	switch {
	case errors.Is(err, fs.ErrExist):
		return BootStartAlreadyDecided, nil
	case err != nil:
		return 0, fmt.Errorf("boot-start: create the marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("boot-start: close the marker: %w", err)
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
