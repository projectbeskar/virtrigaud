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
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/version"
)

// runCommand runs the agent until SIGTERM or SIGINT. Exit status 0 after a
// graceful stop, 1 when the start is refused or the agent fails, 2 for bad
// flags.
func runCommand(args []string, log logr.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config.Register(fs)
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
	dev, err := cfg.LoadDevEnrolment()
	if err != nil {
		log.Error(err, "development enrolment")
		return 1
	}
	log.Info("starting", "version", version.String(), "developmentBuild", config.DevBuild, "executor", cfg.Executor)

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	err = daemon.Run(context.Background(), daemon.Options{
		Config: cfg, Executor: exec, DevEnrolment: dev, Notifier: systemd.NewNotifier(),
		Log: log, Version: version.Version,
	}, signals)
	if err != nil {
		if !errors.Is(err, daemon.ErrStartRefused) {
			log.Error(err, "the agent stopped")
		}
		return 1
	}
	return 0
}
