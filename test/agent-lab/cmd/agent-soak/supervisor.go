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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Agent launch modes.
const (
	// modeBinary runs the agent binary, `vrtg-agent run`.
	modeBinary = "binary"
	// modeWrapped runs the agent's daemon in this tool's own binary, with the
	// fake executor wrapped so that every step invocation is recorded outside
	// the agent's state before the executor is called.
	modeWrapped = "wrapped"
)

// agentConn is one run of the agent as the callers see it.
type agentConn struct {
	cc  *grpc.ClientConn
	gen int
}

// supervisor starts, watches and kills the agent, and tells the callers which
// run is serving.
type supervisor struct {
	lab    *lab
	mode   string
	agent  string // the agent binary (modeBinary) or this tool (modeWrapped)
	listen string
	extra  []string
	// memoryMax, when set (for example "256M"), runs each agent in a
	// transient systemd scope with MemoryMax set to it.
	memoryMax string

	mu      sync.Mutex
	gen     int
	cur     *agentConn
	ready   chan struct{} // closed while an agent is ready
	cmd     *exec.Cmd
	exited  chan struct{}
	upTotal time.Duration
	upSince time.Time

	// toReady records the time from spawn to READY of each start.
	toReady []time.Duration
}

func newSupervisor(l *lab, mode, agentBin, listen string, extra []string) *supervisor {
	return &supervisor{lab: l, mode: mode, agent: agentBin, listen: listen, extra: extra, ready: make(chan struct{})}
}

// args are the agent's command line.
func (s *supervisor) args() []string {
	d := s.lab.Dirs
	common := []string{
		"--state-dir", d.State, "--listen-address", s.listen,
		"--signer-socket", d.signerSocket(), "--signer-user", s.lab.Signer.Name, "--signer-key-file", d.signerKey(),
		"--executor", "fake", "--dev-trust-dir", d.Trust, "--dev-enrolment-file", d.DevEnrolment,
		"--scrub-interval", "1h", "--drain-timeout", "10s",
	}
	if s.mode == modeWrapped {
		return append(append([]string{"agent"}, common...), s.extra...)
	}
	return append(append([]string{"run"}, common...), s.extra...)
}

// dial opens a connection to the agent that reconnects quickly.
func (s *supervisor) dial() (*grpc.ClientConn, error) {
	return grpc.NewClient(s.listen, grpc.WithTransportCredentials(credentials.NewTLS(s.lab.tlsConfig())),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 20 * time.Millisecond, Multiplier: 1.3, MaxDelay: 300 * time.Millisecond},
			MinConnectTimeout: 2 * time.Second,
		}))
}

// helloState asks the agent for its state with a signed Hello and returns it.
func helloState(ctx context.Context, cc *grpc.ClientConn) (providerv1.AgentState, error) {
	resp, err := agentv1.NewAgentServiceClient(cc).Hello(ctx,
		&agentv1.HelloRequest{AnswerNonce: bytes.Repeat([]byte{7}, 16), CallerApiVersion: 1})
	if err != nil {
		return 0, err
	}
	if resp.GetHostAnswer() == nil {
		return 0, errors.New("the answer is not signed")
	}
	a := &providerv1.HostAnswer{}
	if err := proto.Unmarshal(resp.GetHostAnswer().GetBody(), a); err != nil {
		return 0, fmt.Errorf("decode host answer: %w", err)
	}
	return a.GetContext().GetAgentState(), nil
}

// start spawns the next run of the agent and waits until it answers READY. On
// success the run is published to the callers. maxWait bounds the wait.
func (s *supervisor) start(ctx context.Context, maxWait time.Duration) (int, error) {
	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	logf, err := os.OpenFile(s.lab.Dirs.agentLog(gen), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- a path under the lab directory
	if err != nil {
		return gen, fmt.Errorf("open agent log: %w", err)
	}
	defer func() { _ = logf.Close() }()
	var cmd *exec.Cmd
	if s.memoryMax != "" {
		// The agent runs as its own user inside a scope with a memory limit.
		a := append([]string{"--scope", "--quiet", "--unit=" + s.scopeName(gen), "-p", "MemoryMax=" + s.memoryMax,
			"--uid=" + s.lab.Agent.Name, "--gid=" + fmt.Sprint(s.lab.Agent.GID), "--", s.agent}, s.args()...)
		cmd = exec.Command("systemd-run", a...) // #nosec G204 -- the lab's own binary
	} else {
		cmd = exec.Command(s.agent, s.args()...) // #nosec G204 -- the lab's own binary
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: s.lab.Agent.credential()}
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	spawned := time.Now()
	if err := cmd.Start(); err != nil {
		return gen, fmt.Errorf("start agent: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	s.mu.Lock()
	s.cmd, s.exited = cmd, exited
	s.mu.Unlock()

	cc, err := s.dial()
	if err != nil {
		return gen, fmt.Errorf("dial agent: %w", err)
	}
	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-exited:
			_ = cc.Close()
			return gen, fmt.Errorf("agent run %d exited during start (see %s)", gen, s.lab.Dirs.agentLog(gen))
		case <-ctx.Done():
			_ = cc.Close()
			return gen, ctx.Err()
		default:
		}
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, herr := helloState(hctx, cc)
		cancel()
		if herr == nil && st == providerv1.AgentState_AGENT_STATE_READY {
			break
		}
		if herr == nil && st == providerv1.AgentState_AGENT_STATE_QUARANTINED {
			_ = cc.Close()
			return gen, fmt.Errorf("agent run %d is quarantined (see %s)", gen, s.lab.Dirs.agentLog(gen))
		}
		if time.Now().After(deadline) {
			_ = cc.Close()
			return gen, fmt.Errorf("agent run %d not ready after %s (last: state %v, err %v)", gen, maxWait, st, herr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	s.cur = &agentConn{cc: cc, gen: gen}
	s.toReady = append(s.toReady, time.Since(spawned))
	s.upSince = time.Now()
	close(s.ready)
	s.mu.Unlock()
	return gen, nil
}

// pid is the running agent's process id, 0 when none.
func (s *supervisor) pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// kill sends SIGKILL to the agent, waits for it to be gone and withdraws the
// run from the callers. It reports whether a process was killed.
func (s *supervisor) kill() bool {
	s.mu.Lock()
	cmd, exited, cur := s.cmd, s.exited, s.cur
	if cmd == nil || cmd.Process == nil {
		s.mu.Unlock()
		return false
	}
	if cur != nil {
		s.upTotal += time.Since(s.upSince)
		s.cur = nil
		s.ready = make(chan struct{})
	}
	s.cmd = nil
	s.mu.Unlock()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-exited
	if cur != nil {
		_ = cur.cc.Close()
	}
	return true
}

// stop asks the agent to stop (SIGTERM) and kills it when it does not within
// wait. It reports whether the stop was graceful.
func (s *supervisor) stop(wait time.Duration) bool {
	s.mu.Lock()
	cmd, exited, cur := s.cmd, s.exited, s.cur
	if cmd == nil {
		s.mu.Unlock()
		return true
	}
	if cur != nil {
		s.upTotal += time.Since(s.upSince)
		s.cur = nil
		s.ready = make(chan struct{})
	}
	s.cmd = nil
	s.mu.Unlock()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	graceful := true
	select {
	case <-exited:
	case <-time.After(wait):
		graceful = false
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-exited
	}
	if cur != nil {
		_ = cur.cc.Close()
	}
	return graceful
}

// current returns the serving run, if any, and the channel that closes when
// one is ready.
func (s *supervisor) current() (*agentConn, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.ready
}

// wait blocks until an agent run is serving.
func (s *supervisor) wait(ctx context.Context) (*agentConn, error) {
	for {
		c, ready := s.current()
		if c != nil {
			return c, nil
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// uptime is the total time an agent has been serving, across runs.
func (s *supervisor) uptime() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.upTotal
	if s.cur != nil {
		d += time.Since(s.upSince)
	}
	return d
}

// startTimes returns the spawn-to-ready times so far.
func (s *supervisor) startTimes() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.toReady...)
}

// scopeName is the transient scope's name for run gen.
func (s *supervisor) scopeName(gen int) string { return fmt.Sprintf("soakagent-%d.scope", gen) }

// scopeStats reads the memory statistics of run gen's scope while it exists.
func (s *supervisor) scopeStats(gen int) map[string]string {
	out := map[string]string{}
	dir := "/sys/fs/cgroup/system.slice/" + s.scopeName(gen)
	for _, f := range []string{"memory.peak", "memory.current", "memory.max", "memory.events"} {
		if b, err := os.ReadFile(dir + "/" + f); err == nil { // #nosec G304 -- a cgroup file
			out[f] = strings.Join(strings.Fields(string(b)), " ")
		}
	}
	return out
}

// logCount counts the lines of run gen's log that contain substr.
func (s *supervisor) logCount(gen int, substr string) int {
	b, err := os.ReadFile(s.lab.Dirs.agentLog(gen)) // #nosec G304 -- the lab's log
	if err != nil {
		return 0
	}
	return strings.Count(string(b), substr)
}

// currentGen is the number of the latest run.
func (s *supervisor) currentGen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}
