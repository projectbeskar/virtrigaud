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
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/version"
)

// countingExec is the fake executor with a record of every step invocation,
// appended to a file outside the agent's state before the step is called.
// The fake executor itself logs only effects it actually performs: a second
// invocation of a once or irreversible step that finds its effect present
// leaves no trace there, so the invocation record is the witness that the
// store never calls such a step twice.
type countingExec struct {
	*fake.Executor
	log *os.File
}

// Run records the invocation, then runs the step on the fake executor.
func (c *countingExec) Run(ctx context.Context, op executor.Operation, step executor.Step) error {
	if _, err := fmt.Fprintf(c.log, "%s %s %s\n", hex.EncodeToString(op.OpID), step.Name, op.VMUID); err != nil {
		return fmt.Errorf("record step invocation: %w", err)
	}
	return c.Executor.Run(ctx, op, step)
}

// runWrappedAgent runs the agent's daemon in this process, as `vrtg-agent
// run` does, with the counting executor. It returns the exit status.
func runWrappedAgent(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config.Register(fs)
	stepLog := fs.String("step-log", "", "file the step invocations are appended to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := logr.FromSlogHandler(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid configuration")
		return 2
	}
	if *stepLog == "" {
		log.Error(errors.New("--step-log is required"), "invalid configuration")
		return 2
	}
	fx, err := fake.New(filepath.Join(cfg.StateDir, config.FakeExecutorDirName))
	if err != nil {
		log.Error(err, "executor")
		return 1
	}
	f, err := os.OpenFile(*stepLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- a path the lab chose
	if err != nil {
		log.Error(err, "step log")
		return 1
	}
	defer func() { _ = f.Close() }()
	dev, err := cfg.LoadDevEnrolment()
	if err != nil {
		log.Error(err, "development enrolment")
		return 1
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := daemon.Run(context.Background(), daemon.Options{
		Config: cfg, Executor: &countingExec{Executor: fx, log: f}, DevEnrolment: dev, Notifier: systemd.NewNotifier(),
		Log: log, Version: version.Version,
	}, signals); err != nil {
		if !errors.Is(err, daemon.ErrStartRefused) {
			log.Error(err, "the agent stopped")
		}
		return 1
	}
	return 0
}
