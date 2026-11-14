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
	"bufio"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// startTimeout bounds an agent process's start-up.
const startTimeout = 30 * time.Second

// stateDBName is the operation store's database file in the state directory.
const stateDBName = "state.db"

// Host is one per-host agent process.
type Host struct {
	// Name is the host id.
	Name string
	// Enrolment is the host's enrolment id.
	Enrolment []byte

	root       string
	stateDir   string
	cfgPath    string
	logPath    string
	clientTLS  *tls.Config
	snapshot   string
	mu         sync.Mutex
	cmd        *exec.Cmd
	exited     chan struct{}
	exitErr    error
	addr       string
	conn       *grpc.ClientConn
	state      providerv1.AgentState
	extraEnv   []string
	lastStatus *os.ProcessState
}

// FakeDir is the host's fake executor directory.
func (h *Host) FakeDir() string { return filepath.Join(h.stateDir, config.FakeExecutorDirName) }

// StateDir is the host's state directory.
func (h *Host) StateDir() string { return h.stateDir }

// LogPath is the file the agent process logs to.
func (h *Host) LogPath() string { return h.logPath }

// State is the trust state the agent reported when its start-up ended.
func (h *Host) State() providerv1.AgentState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// Conn is the Provider's mutual TLS connection to the agent.
func (h *Host) Conn() *grpc.ClientConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

// Client is a Provider client of the agent.
func (h *Host) Client() providerv1.ProviderClient { return providerv1.NewProviderClient(h.Conn()) }

// writePEM writes one PEM block.
func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode)
}

// newHost lays out a host's directories: the state directory with its key
// material and provisioning marker, the trust configuration, the signer's
// key and the child configuration.
func newHost(root, name, hostURI string, enrolment []byte, hostCertPEM []byte, hostKey *ecdsa.PrivateKey,
	trustFiles map[string][]byte, clientTLS *tls.Config, startEpoch uint64, stepDeadline time.Duration) (*Host, error) {
	h := &Host{Name: name, Enrolment: enrolment, root: root, stateDir: filepath.Join(root, "state"),
		cfgPath: filepath.Join(root, "child.json"), logPath: filepath.Join(root, "agent.log"), clientTLS: clientTLS,
		snapshot: filepath.Join(root, "snapshot.db")}
	trustDir := filepath.Join(root, "trust")
	keyDir := filepath.Join(root, "signer")
	for _, d := range []string{h.stateDir, filepath.Join(h.stateDir, config.PKIDirName), keyDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(trustDir, 0o750); err != nil {
		return nil, err
	}
	for n, data := range trustFiles {
		if err := os.WriteFile(filepath.Join(trustDir, n), data, 0o644); err != nil { // #nosec G306 -- a test trust configuration
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(h.stateDir, config.PKIDirName, trust.FileHostCert), hostCertPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(h.stateDir, hostcheck.HostMarkerFile), []byte(hostURI+"\n"), 0o600); err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(hostKey)
	if err != nil {
		return nil, err
	}
	keyFile := filepath.Join(keyDir, trust.FileHostKey)
	if err := writePEM(keyFile, "PRIVATE KEY", der, 0o600); err != nil {
		return nil, err
	}
	// A Unix socket path is short; the test's directories may not be.
	sockDir, err := os.MkdirTemp("", "vat")
	if err != nil {
		return nil, err
	}
	cfg, err := json.Marshal(childConfig{StateDir: h.stateDir, TrustDir: trustDir, SignerSocket: filepath.Join(sockDir, "s.sock"),
		SignerKeyFile: keyFile, HostURI: hostURI, StartEpoch: startEpoch, StepDeadline: stepDeadline.Milliseconds()})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(h.cfgPath, cfg, 0o600); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(h.FakeDir(), 0o700); err != nil {
		return nil, err
	}
	return h, nil
}

// Start starts the agent process with extra environment variables and
// waits until its start-up sequence ends (or the process dies).
func (h *Host) Start(extraEnv ...string) error {
	logf, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- the test's own log file
	if err != nil {
		return err
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$") // #nosec G204 -- the test binary itself
	cmd.Env = append(append(os.Environ(), childEnv+"="+h.cfgPath), extraEnv...)
	cmd.Stderr = logf
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = logf.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	exited := make(chan struct{})
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	h.mu.Lock()
	h.cmd, h.exited, h.extraEnv = cmd, exited, extraEnv
	h.mu.Unlock()
	go func() {
		err := cmd.Wait()
		_ = logf.Close()
		h.mu.Lock()
		h.exitErr, h.lastStatus = err, cmd.ProcessState
		h.mu.Unlock()
		close(exited)
	}()
	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				<-exited
				return fmt.Errorf("the agent %s exited during start-up: %v\n%s", h.Name, h.exitErr, h.tail())
			}
			kind, val, _ := strings.Cut(line, " ")
			switch kind {
			case lineAddr:
				if err := h.dial(val); err != nil {
					return err
				}
			case lineStarted:
				h.mu.Lock()
				h.state = providerv1.AgentState(providerv1.AgentState_value[val])
				h.mu.Unlock()
				go func() {
					for range lines { //nolint:revive // drain the agent's output
					}
				}()
				return nil
			}
		case <-timer.C:
			_ = cmd.Process.Kill()
			return fmt.Errorf("the agent %s did not start in %s\n%s", h.Name, startTimeout, h.tail())
		}
	}
}

// dial opens the Provider's connection to the agent at addr.
func (h *Host) dial(addr string) error {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(h.clientTLS)))
	if err != nil {
		return err
	}
	h.mu.Lock()
	old := h.conn
	h.conn, h.addr = cc, addr
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// tail is the end of the agent's log, for error messages.
func (h *Host) tail() string {
	b, err := os.ReadFile(h.logPath)
	if err != nil {
		return ""
	}
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}

// Running reports whether the agent process runs.
func (h *Host) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exited == nil {
		return false
	}
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// Wait waits up to d for the agent process to exit and reports whether it
// was killed by SIGKILL.
func (h *Host) Wait(d time.Duration) (killed bool, err error) {
	h.mu.Lock()
	exited := h.exited
	h.mu.Unlock()
	if exited == nil {
		return false, errors.New("never started")
	}
	select {
	case <-exited:
	case <-time.After(d):
		return false, fmt.Errorf("the agent %s did not exit in %s", h.Name, d)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ws, ok := h.lastStatus.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return true, nil
	}
	return false, nil
}

// signal sends sig to the agent process and waits for it to exit.
func (h *Host) signal(sig syscall.Signal) error {
	h.mu.Lock()
	cmd := h.cmd
	h.mu.Unlock()
	if cmd == nil || !h.Running() {
		return nil
	}
	if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	_, err := h.Wait(startTimeout)
	return err
}

// Stop stops the agent gracefully (SIGTERM) and waits for it.
func (h *Host) Stop() error { return h.signal(syscall.SIGTERM) }

// Kill kills the agent (SIGKILL), as a crash would, and waits for it.
func (h *Host) Kill() error { return h.signal(syscall.SIGKILL) }

// Restart starts the agent again with no extra environment.
func (h *Host) Restart() error { return h.Start() }

// Snapshot copies the operation store's database as it is now; the agent
// must be stopped.
func (h *Host) Snapshot() error {
	return copyFile(filepath.Join(h.stateDir, stateDBName), h.snapshot)
}

// RollBack puts the last snapshot back in place of the database, keeping
// the anchor log; the agent must be stopped.
func (h *Host) RollBack() error {
	return copyFile(h.snapshot, filepath.Join(h.stateDir, stateDBName))
}

func copyFile(from, to string) error {
	in, err := os.Open(from) // #nosec G304 -- the test's own files
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the test's own files
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Effects reads the fake executor's effects log.
func (h *Host) Effects() ([]fake.Effect, error) { return fake.Effects(h.FakeDir()) }

// Addr is the address the agent process listens on now; it changes when the
// process restarts.
func (h *Host) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}
