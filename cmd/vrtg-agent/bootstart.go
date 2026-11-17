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
// (RuntimeDirectory=vrtg-agent-boot-start, kept until reboot).
const defaultBootStartDir = "/run/vrtg-agent-boot-start"

// bootStartFailedMessage is logged when boot-start did not start the agent.
const bootStartFailedMessage = "the agent was not started at boot; start it by hand: systemctl start " + systemd.AgentUnit

// bootStartTimeout bounds the two systemctl calls.
const bootStartTimeout = 30 * time.Second

// bootStartCommand is the action of vrtg-agent-boot-start.service: start the
// agent at most once per boot, only while its unit is enabled (see
// systemd.BootStart). Exit status 0 when it started the agent or correctly
// did nothing, 1 when it failed (the agent is then not started), 2 for bad
// flags.
func bootStartCommand(args []string, log logr.Logger, stderr io.Writer) int {
	return bootStart(args, log, stderr, systemd.Systemctl{})
}

// bootStart is bootStartCommand with the service manager given.
func bootStart(args []string, log logr.Logger, stderr io.Writer, s systemd.UnitStarter) int {
	fs := flag.NewFlagSet("boot-start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("runtime-dir", runtimeDir(), "directory of the once-per-boot marker; survives restarts of the unit, not a reboot")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootStartTimeout)
	defer cancel()
	got, err := systemd.BootStart(ctx, *dir, systemd.AgentUnit, s)
	switch {
	case err != nil:
		// A warning, not an error: the host is fine, the agent is merely not
		// started this boot (the unit fails, and Upholds= runs it again a
		// bounded number of times; the marker keeps a second start out).
		slog.New(logr.ToSlogHandler(log)).Warn(bootStartFailedMessage, "error", err.Error())
		return 1
	case got == systemd.BootStartAlreadyDecided:
		log.Info("the agent's start at boot was already decided in this boot; nothing is done (a reboot re-arms it)")
	case got == systemd.BootStartStartedByOther:
		log.Info("the agent was already started during this boot; that is this boot's decision, nothing is done (a reboot re-arms it)")
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
