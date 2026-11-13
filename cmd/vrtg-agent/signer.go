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
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/version"
)

// defaultSignerAgentUser is the user the signer serves.
const defaultSignerAgentUser = "virtrigaud-agent"

// signerCommand runs the signer: the process that holds the host private key
// and signs digests for the agent over a Unix socket (see package signer for
// the protocol and for what the separation does and does not protect). It
// runs as its own unprivileged user, never root. It is the same in a release
// and a development build: it has no development-only setting. Exit status 0
// after a graceful stop, 1 when the start is refused or the signer fails, 2
// for bad flags.
func signerCommand(args []string, log logr.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("signer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", config.DefaultSignerSocket, "the Unix socket to serve (its directory: owned by the signer's user, no access for others)")
	keyFile := fs.String("key-file", config.DefaultSignerKeyFile, "the host private key file (owned by the signer's user, mode 0600)")
	agentUser := fs.String("agent-user", defaultSignerAgentUser, "the only user served (name or uid)")
	perSecond := fs.Float64("max-signs-per-second", signer.DefaultSignsPerSecond, "the sustained rate limit of the answer budget (signed host answers)")
	burst := fs.Int("sign-burst", signer.DefaultSignBurst, "the answer budget's burst size")
	hsPerSecond := fs.Float64("max-handshake-signs-per-second", signer.DefaultHandshakeSignsPerSecond, "the sustained rate limit of the handshake budget (TLS handshakes, which unauthenticated peers can trigger)")
	hsBurst := fs.Int("handshake-sign-burst", signer.DefaultHandshakeSignBurst, "the handshake budget's burst size")
	every := fs.Duration("log-interval", signer.DefaultLogInterval, "how often the signature counters are logged")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	for name, p := range map[string]string{"socket": *socket, "key-file": *keyFile} {
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			log.Error(fmt.Errorf("--%s must be a clean absolute path", name), "invalid configuration")
			return 2
		}
	}
	if *perSecond <= 0 || *burst < 1 || *hsPerSecond <= 0 || *hsBurst < 1 || *every <= 0 {
		log.Error(errors.New("the rate limit and the log interval must be positive"), "invalid configuration")
		return 2
	}
	own := signer.EUID()
	if own == 0 {
		log.Error(errors.New("the signer runs as root"), "start refused; run it as its own unprivileged user")
		return 1
	}
	agentUID, err := signer.ResolveUID(*agentUser)
	if err != nil {
		log.Error(err, "agent user")
		return 1
	}
	if agentUID == own {
		log.Error(errors.New("the agent's user is the signer's user"), "start refused; the two must be different users")
		return 1
	}
	log.Info("starting signer", "version", version.String(), "agentUid", agentUID)

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := signer.Run(context.Background(), signer.RunOptions{
		SocketPath: *socket, KeyFile: *keyFile, OwnerUID: own, AgentUID: agentUID,
		SignsPerSecond: *perSecond, SignBurst: *burst, LogInterval: *every,
		HandshakeSignsPerSecond: *hsPerSecond, HandshakeSignBurst: *hsBurst,
		Notifier: systemd.NewNotifier(), Log: log,
	}, signals); err != nil {
		log.Error(err, "the signer stopped")
		return 1
	}
	return 0
}
