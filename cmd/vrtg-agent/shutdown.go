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
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// defaultShutdownTimeout bounds shutdown-vms.
const defaultShutdownTimeout = 5 * time.Minute

// shutdownCommand stops every VM, and only while the host is shutting down:
// the shutdown unit's stop action runs it, and so does a maintenance stop of
// that unit, which must never stop VMs. Exit status 0 when it acted or
// correctly did nothing, 1 when stopping failed or the host's state could not
// be read (no VM is stopped then), 2 for bad flags.
func shutdownCommand(args []string, log logr.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("shutdown-vms", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config.Register(fs)
	timeout := fs.Duration("timeout", defaultShutdownTimeout, "how long stopping the VMs may take")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid configuration")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return shutdownVMs(ctx, systemd.Systemctl{}, systemd.Systemctl{}, func() (executor.Stopper, error) {
		return cfg.NewExecutor()
	}, log)
}

// shutdownVMs is the guarded action of shutdown-vms. When the setup unit
// failed (the agent's user is missing or its uid moved), it does nothing and
// exits 0: the agent's state cannot be trusted, nothing is stopped, and the
// failure is the setup unit's to report. This check is in the action, not a
// unit condition: the shutdown unit is upheld, and a skipped upheld unit is
// started again and again. A setup unit that is merely inactive, or whose
// state cannot be read, does not skip: the shutdown guard and the executor
// then decide, fail-closed, as before.
func shutdownVMs(ctx context.Context, m systemd.Manager, units systemd.UnitFailures, newStopper func() (executor.Stopper, error), log logr.Logger) int {
	failed, err := units.Failed(ctx, systemd.SetupUnit)
	switch {
	case err != nil:
		log.Info("cannot tell whether the setup unit failed; going on", "unit", systemd.SetupUnit, "error", err.Error())
	case failed:
		slog.New(logr.ToSlogHandler(log)).Warn("skipping: the agent's users and directories could not be set up ("+systemd.SetupUnit+
			" failed), so the agent's state cannot be used; no VM is stopped", "unit", systemd.SetupUnit)
		return 0
	}
	s, err := newStopper()
	if err != nil {
		log.Error(err, "executor")
		return 1
	}
	down, err := systemd.ShutdownInProgress(ctx, m)
	if err != nil {
		log.Error(err, "cannot tell whether the host is shutting down; no VM is stopped")
		return 1
	}
	if !down {
		log.Info("the host is not shutting down; no VM is stopped")
		return 0
	}
	if err := s.StopAll(ctx); err != nil {
		log.Error(err, "stopping the VMs failed")
		return 1
	}
	log.Info("every VM stopped for the host shutdown")
	return 0
}
