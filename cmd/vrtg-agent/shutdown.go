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
	exec, err := cfg.NewExecutor()
	if err != nil {
		log.Error(err, "executor")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return shutdownVMs(ctx, systemd.Systemctl{}, exec, log)
}

// shutdownVMs is the guarded action of shutdown-vms.
func shutdownVMs(ctx context.Context, m systemd.Manager, s executor.Stopper, log logr.Logger) int {
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
