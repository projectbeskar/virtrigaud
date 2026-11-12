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

package daemon_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const testUID uint32 = 4242

// notifications records systemd notifications.
type notifications struct {
	mu   sync.Mutex
	sent []string
}

func (n *notifications) Notify(state string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, state)
	return nil
}

func (n *notifications) has(prefix string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.sent {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// fixed clock sources for the clock floor.
type wall struct{}

func (wall) Now() time.Time { return time.Now() }

type bootClock struct{ start time.Time }

func (b bootClock) BootTime() (time.Duration, error) { return time.Since(b.start) + time.Hour, nil }

type bootID struct{}

func (bootID) BootID() (string, error) { return "boot-1", nil }

type timeNS struct{}

func (timeNS) CheckTimeNamespace() error { return nil }

// logBuffer collects log lines.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuffer) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 1})
}

func (l *logBuffer) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// host is one agent host: a state directory, a trust directory, a PKI and a
// fake probe on which every check passes.
type host struct {
	t        *testing.T
	pki      *trusttest.PKI
	stateDir string
	trustDir string
	probe    *hostchecktest.Fake
	cfg      *config.Config
	dev      *config.DevEnrolment
	exec     config.Executor
	notes    *notifications
	logs     *logBuffer
	hooks    daemon.Hooks
	boot     bootClock
}

func newHost(t *testing.T) *host {
	t.Helper()
	daemon.AllowTestOverrides(t)
	root := t.TempDir()
	h := &host{t: t, pki: trusttest.New(t), stateDir: filepath.Join(root, "state"), trustDir: filepath.Join(root, "trust"),
		dev: &config.DevEnrolment{StartEpoch: 2}, exec: none.Executor{}, notes: &notifications{}, logs: &logBuffer{},
		boot: bootClock{start: time.Now()}}
	if err := os.MkdirAll(h.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.pki.WriteTrust(t, h.trustDir)
	h.pki.WritePKI(t, filepath.Join(h.stateDir, config.PKIDirName))
	if err := os.WriteFile(filepath.Join(h.stateDir, hostcheck.HostMarkerFile), []byte(h.pki.HostURI+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfg = &config.Config{StateDir: h.stateDir, ListenAddress: "127.0.0.1:0", DrainTimeout: 5 * time.Second, Executor: config.ExecutorNone}
	h.probe = hostchecktest.Healthy(hostcheck.Config{StateDir: h.stateDir, PKIDir: h.cfg.PKIDir(), TrustDir: h.trustDir, UID: testUID}, h.pki.HostURI)
	return h
}

// running is one run of the agent.
type running struct {
	h       *host
	addr    string
	signals chan os.Signal
	done    chan error
	exited  chan struct{}
	started chan providerv1.AgentState
}

// start runs the agent; it does not wait for the start-up sequence.
func (h *host) start() *running {
	h.t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	r := &running{h: h, addr: lis.Addr().String(), signals: make(chan os.Signal, 4), done: make(chan error, 1),
		exited: make(chan struct{}), started: make(chan providerv1.AgentState, 1)}
	hooks := h.hooks
	hooks.Started = func(s providerv1.AgentState) { r.started <- s }
	uid := testUID
	opts := daemon.Options{
		Config: h.cfg, Probe: h.probe, UID: &uid, Executor: h.exec, DevEnrolment: h.dev, Notifier: h.notes,
		Log: h.logs.logger(), Version: "test", Listener: lis, TrustDir: h.trustDir, Hooks: hooks,
		Floor: daemon.FloorSources{Wall: wall{}, Boot: h.boot, BootID: bootID{}, TimeNS: timeNS{}},
	}
	go func() {
		r.done <- daemon.Run(context.Background(), opts, r.signals)
		close(r.exited)
	}()
	h.t.Cleanup(func() {
		select {
		case r.signals <- syscall.SIGTERM:
		default:
		}
		select {
		case <-r.exited:
		case <-time.After(10 * time.Second):
			h.t.Error("the agent did not stop")
		}
	})
	return r
}

// run starts the agent and waits for the start-up sequence to end.
func (h *host) run() (*running, providerv1.AgentState) {
	h.t.Helper()
	r := h.start()
	select {
	case s := <-r.started:
		return r, s
	case err := <-r.done:
		h.t.Fatalf("the agent stopped during start-up: %v\n%s", err, h.logs.text())
	case <-time.After(10 * time.Second):
		h.t.Fatalf("the agent did not finish starting\n%s", h.logs.text())
	}
	return nil, 0
}

// stop sends SIGTERM and waits for Run to return.
func (r *running) stop() error {
	r.h.t.Helper()
	r.signals <- syscall.SIGTERM
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		r.h.t.Fatal("the agent did not stop")
	}
	return nil
}

// conn dials the agent as the allowed Provider.
func (r *running) conn() *grpc.ClientConn {
	r.h.t.Helper()
	cert := r.h.pki.ProviderCert(r.h.t, nil, trusttest.ProviderURI(trusttest.ProviderUID))
	roots := r.h.pki.HostCAPool()
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // #nosec G402 -- verified below against the host identity issuer
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = leaf.Verify(x509.VerifyOptions{Roots: roots})
			return err
		},
	}
	cc, err := grpc.NewClient(r.addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		r.h.t.Fatal(err)
	}
	r.h.t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func callCtx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// hello calls Hello and returns the decoded HostAnswer, or nil when the
// answer is unsigned.
func (r *running) hello() *providerv1.HostAnswer {
	r.h.t.Helper()
	resp, err := agentv1.NewAgentServiceClient(r.conn()).Hello(callCtx(r.h.t),
		&agentv1.HelloRequest{AnswerNonce: bytes.Repeat([]byte{1}, 16), CallerApiVersion: 1})
	if err != nil {
		r.h.t.Fatalf("Hello: %v", err)
	}
	if resp.GetHostAnswer() == nil {
		return nil
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(resp.GetHostAnswer().GetBody(), a); err != nil {
		r.h.t.Fatal(err)
	}
	return a
}

// create calls Create and returns the refusal's code and reason.
func (r *running) create() (string, string) {
	r.h.t.Helper()
	_, err := providerv1.NewProviderClient(r.conn()).Create(callCtx(r.h.t), &providerv1.CreateRequest{})
	st, _ := status.FromError(err)
	reason := ""
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reason = info.GetReason()
		}
	}
	return st.Code().String(), reason
}

var errNotExist = os.ErrNotExist

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, errNotExist) {
		t.Fatal(err)
	}
	return err == nil
}
