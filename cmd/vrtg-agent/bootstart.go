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
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// defaultBootStartDir is the boot-start unit's runtime directory
// (RuntimeDirectory=vrtg-agent-boot-start, kept until reboot; a soft reboot
// keeps it too).
const defaultBootStartDir = "/run/vrtg-agent-boot-start"

// bootStartFailedMessage is logged when boot-start failed after this boot's
// decision was made: it did not start the agent, and no later run will.
const bootStartFailedMessage = "the agent was not started at boot; start it by hand: systemctl start " + systemd.AgentUnit

// bootStartUndecidedMessage is logged when boot-start failed before this
// boot's decision: it started nothing, and cannot tell whether the agent was
// already started this boot.
const bootStartUndecidedMessage = "boot-start failed before deciding: it started nothing and could not determine whether the agent was started this boot; check it: systemctl status " + systemd.AgentUnit

// bootStartTimeout bounds the two systemctl calls.
const bootStartTimeout = 30 * time.Second

// bootStartCommand is the action of vrtg-agent-boot-start.service: start the
// agent at most once per boot, a soft reboot counting as a boot, only while
// its unit is enabled (see systemd.BootStart). Exit status 0 when it started
// the agent or correctly did nothing, 1 when it failed (this run started
// nothing; the warning says whether this boot's start was missed or could
// not be determined), 2 for bad flags.
func bootStartCommand(args []string, log logr.Logger, stderr io.Writer) int {
	return bootStart(args, log, stderr, systemd.Systemctl{})
}

// bootStart is bootStartCommand with the service manager given.
func bootStart(args []string, log logr.Logger, stderr io.Writer, s systemd.UnitStarter) int {
	fs := flag.NewFlagSet("boot-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("runtime-dir", runtimeDir(), "directory of the once-per-boot markers; survives restarts of the unit and a soft reboot, not a reboot")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootStartTimeout)
	defer cancel()
	got, err := systemd.BootStart(ctx, *dir, systemd.AgentUnit, s)
	switch {
	case err != nil:
		// A warning, not an error: this run started nothing. The unit fails
		// and Upholds= runs it again; the marker keeps a second start out.
		// Before the decision, whether the agent was started this boot is
		// unknown (the time of the last soft reboot is read before the
		// marker, so this can happen after a start); after it, this boot's
		// start was missed.
		msg := bootStartFailedMessage
		if errors.Is(err, systemd.ErrBootStartUndecided) {
			msg = bootStartUndecidedMessage
		}
		slog.New(logr.ToSlogHandler(log)).Warn(msg, "error", err.Error())
		return 1
	case got == systemd.BootStartAlreadyDecided:
		log.Info("the agent's start at boot was already decided in this boot; nothing is done (a reboot or a soft reboot re-arms it)")
	case got == systemd.BootStartStartedByOther:
		log.Info("the agent was already started during this boot; that is this boot's decision, nothing is done (a reboot or a soft reboot re-arms it)")
	case got == systemd.BootStartSetupNotActive:
		slog.New(logr.ToSlogHandler(log)).Warn("the agent's users and directories could not be set up (" + systemd.SetupUnit +
			" is not active): the agent is not started at boot; fix the cause (systemctl status " + systemd.SetupUnit +
			"), then start it by hand: systemctl start " + systemd.AgentUnit)
	case got == systemd.BootStartNotEnabled:
		log.Info("the agent's unit is not enabled; it is not started at boot", "unit", systemd.AgentUnit)
	default:
		log.Info("the agent's start is queued", "unit", systemd.AgentUnit)
	}
	return 0
}

// runtimeDir is the unit's runtime directory: the first of systemd's
// $RUNTIME_DIRECTORY, or defaultBootStartDir.
func runtimeDir() string {
	if d, _, _ := strings.Cut(os.Getenv("RUNTIME_DIRECTORY"), ":"); d != "" {
		return d
	}
	return defaultBootStartDir
}
