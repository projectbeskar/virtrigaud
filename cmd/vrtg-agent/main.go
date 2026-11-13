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

// Command vrtg-agent is the per-host agent: one per host, it serves the
// Provider that routes to the host. It is for development builds only; no
// release artefact ships it. A build without the virtrigaud_dev build tag runs
// the no-op executor and refuses every development-only flag.
//
// Subcommands:
//
//	run           run the agent (systemd unit vrtg-agent.service)
//	signer        run the host key signer, a separate process and user that
//	              holds the host private key (vrtg-agent-signer.service)
//	shutdown-vms  stop every VM, only while the host is shutting down
//	              (systemd unit vrtg-vm-shutdown.service)
//	enroll        not available yet: hosts are enrolled with the development
//	              enrolment stand-in
//	version       print the version
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/version"
)

const usage = `usage: vrtg-agent <command> [flags]

commands:
  run           run the per-host agent
  signer        run the host key signer (holds the host private key)
  shutdown-vms  stop every VM, only while the host is shutting down
  enroll        enrol the host (not available yet)
  version       print the version
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain runs a subcommand and returns the exit status.
func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	log := newLogger(stderr)
	switch args[0] {
	case "run":
		return runCommand(args[1:], log, stderr)
	case "signer":
		return signerCommand(args[1:], log, stderr)
	case "shutdown-vms":
		return shutdownCommand(args[1:], log, stderr)
	case "enroll":
		return enrollCommand(args[1:], stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
	return 2
}

// newLogger returns a structured JSON logger on w, for the system journal.
func newLogger(w io.Writer) logr.Logger {
	return logr.FromSlogHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
