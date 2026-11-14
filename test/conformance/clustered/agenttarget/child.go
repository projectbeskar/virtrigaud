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

package agenttarget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// childEnv names the configuration file of an agent process: the test binary
// runs as an agent when it is set.
const childEnv = "VRTG_AGENTTARGET_CHILD"

// The lines an agent process writes on its standard output.
const (
	lineAddr    = "ADDR"
	lineStarted = "STARTED"
)

// childConfig is what an agent process is started with.
type childConfig struct {
	StateDir      string `json:"stateDir"`
	TrustDir      string `json:"trustDir"`
	SignerSocket  string `json:"signerSocket"`
	SignerKeyFile string `json:"signerKeyFile"`
	HostURI       string `json:"hostURI"`
	StartEpoch    uint64 `json:"startEpoch"`
	StepDeadline  int64  `json:"stepDeadlineMillis"`
}

// RunChildIfRequested runs the test binary as an agent process and exits
// when it was started as one; otherwise it returns false. Call it first in
// TestMain.
func RunChildIfRequested() bool {
	path := os.Getenv(childEnv)
	if path == "" {
		return false
	}
	os.Exit(runChild(path, os.Stdout, os.Stderr))
	return true
}

// noNotify discards systemd notifications.
type noNotify struct{}

func (noNotify) Notify(string) error { return nil }

// runChild runs one agent until SIGTERM: the host key signer in its own
// goroutine on a real socket, and the agent's daemon with the development
// fake executor. The probes of the host checks are a healthy fake, and the
// agent's user is the test's user (the test overrides a development build
// allows).
func runChild(path string, stdout, stderr io.Writer) int {
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own configuration file
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read child config: %v\n", err)
		return 2
	}
	var c childConfig
	if err := json.Unmarshal(b, &c); err != nil {
		_, _ = fmt.Fprintf(stderr, "decode child config: %v\n", err)
		return 2
	}
	log := logr.FromSlogHandler(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	keyPEM, err := os.ReadFile(c.SignerKeyFile) // #nosec G304 -- the test's own key file
	if err != nil {
		log.Error(err, "read the host key")
		return 2
	}
	key, err := trust.ParseHostKey(keyPEM)
	if err != nil {
		log.Error(err, "parse the host key")
		return 2
	}
	uid := signer.EUID()
	srv, err := signer.NewServer(signer.ServerOptions{Key: key, AllowedUID: uid, Log: logr.Discard()})
	if err != nil {
		log.Error(err, "signer")
		return 2
	}
	_ = os.Remove(c.SignerSocket)
	slis, err := signer.Listen(c.SignerSocket)
	if err != nil {
		log.Error(err, "signer socket")
		return 2
	}
	go func() { _ = srv.Serve(ctx, slis) }()

	cfg := &config.Config{StateDir: c.StateDir, ListenAddress: "127.0.0.1:0", DrainTimeout: 5 * time.Second,
		Executor: config.ExecutorFake, SignerSocket: c.SignerSocket, SignerUser: "signer", SignerKeyFile: c.SignerKeyFile}
	exec, err := fake.New(filepath.Join(c.StateDir, config.FakeExecutorDirName))
	if err != nil {
		log.Error(err, "fake executor")
		return 2
	}
	probe := hostchecktest.Healthy(hostcheck.Config{StateDir: c.StateDir, PKIDir: cfg.PKIDir(), TrustDir: c.TrustDir, UID: uid,
		SignerKeyFile: c.SignerKeyFile, SignerSocketDir: filepath.Dir(c.SignerSocket), SignerUID: uid}, c.HostURI)
	probe.Edit(func(f *hostchecktest.Fake) {
		if uid == 0 {
			f.UID = 4242
		}
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Error(err, "listen")
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "%s %s\n", lineAddr, lis.Addr().String())
	deadline := time.Duration(c.StepDeadline) * time.Millisecond
	deadlines := map[providerv1.OperationKind]time.Duration{}
	for k := providerv1.OperationKind_OPERATION_KIND_CREATE; k <= providerv1.OperationKind_OPERATION_KIND_RECONFIGURE; k++ {
		deadlines[k] = deadline
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	ownUID := uid
	err = daemon.Run(ctx, daemon.Options{
		Config: cfg, Executor: exec, DevEnrolment: &config.DevEnrolment{StartEpoch: c.StartEpoch}, Notifier: noNotify{},
		Log: log, Version: "agenttarget", Probe: probe, UID: &ownUID, SignerUID: &ownUID, AllowSignerSameUser: true,
		Listener: lis, TrustDir: c.TrustDir, TrustFileOwner: &ownUID, StepDeadlines: deadlines,
		Hooks: daemon.Hooks{Started: func(s providerv1.AgentState) { _, _ = fmt.Fprintf(stdout, "%s %s\n", lineStarted, s) }},
	}, signals)
	if err != nil {
		log.Error(err, "the agent stopped")
		return 1
	}
	return 0
}
