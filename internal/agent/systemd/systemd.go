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

// Package systemd is the per-host agent's small interface to systemd: the
// readiness notification protocol (sd_notify) and the shutdown guard that
// lets the VM shutdown command act only while the host is really going down.
//
// It speaks the notification protocol directly (a datagram to
// $NOTIFY_SOCKET) and reads the manager's state with systemctl, so it adds no
// dependency. It imports no Kubernetes package.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Notification states the agent sends.
const (
	// NotifyReady tells systemd the service finished starting.
	NotifyReady = "READY=1"
	// NotifyStopping tells systemd the service is stopping.
	NotifyStopping = "STOPPING=1"
	// NotifyReloading tells systemd the service is reloading its
	// configuration; NotifyReady follows when it is done.
	NotifyReloading = "RELOADING=1"
	// notifySocketEnv names the notification socket.
	notifySocketEnv = "NOTIFY_SOCKET"
)

// Notifier sends service state notifications.
type Notifier interface {
	// Notify sends state, one or more newline-separated assignments.
	Notify(state string) error
}

// SocketNotifier sends notifications to a notification socket. With no
// socket (the process is not run by systemd with Type=notify) it sends
// nothing.
type SocketNotifier struct {
	// Addr is the socket address; a leading '@' names an abstract socket.
	Addr string
}

// NewNotifier returns a SocketNotifier for $NOTIFY_SOCKET.
func NewNotifier() SocketNotifier { return SocketNotifier{Addr: os.Getenv(notifySocketEnv)} }

// Notify implements Notifier.
func (n SocketNotifier) Notify(state string) error {
	if n.Addr == "" {
		return nil
	}
	addr := n.Addr
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	return nil
}

// Job is a job queued in systemd.
type Job struct {
	// Unit is the job's unit.
	Unit string
	// Type is the job type (start, stop, restart, ...).
	Type string
}

// Manager reads systemd's state.
type Manager interface {
	// SystemState returns the system state systemctl is-system-running
	// prints (running, degraded, starting, stopping, ...).
	SystemState(ctx context.Context) (string, error)
	// Jobs returns the queued jobs.
	Jobs(ctx context.Context) ([]Job, error)
}

// SystemStateStopping is the system state while the host shuts down.
const SystemStateStopping = "stopping"

// ShutdownTargets are the targets whose queued start job means the host is
// going down: shutdown, reboot, halt, power-off and kexec.
var ShutdownTargets = []string{"shutdown.target", "reboot.target", "halt.target", "poweroff.target", "kexec.target"}

// ShutdownInProgress reports whether the host is shutting down: systemd's
// system state is stopping, or a start job of a shutdown target is queued.
// When the state cannot be read it returns the error and false: the caller
// then does nothing, so that stopping the agent's shutdown unit for
// maintenance never stops VMs.
func ShutdownInProgress(ctx context.Context, m Manager) (bool, error) {
	state, err := m.SystemState(ctx)
	if err != nil {
		return false, fmt.Errorf("read the system state: %w", err)
	}
	if state == SystemStateStopping {
		return true, nil
	}
	jobs, err := m.Jobs(ctx)
	if err != nil {
		return false, fmt.Errorf("read the queued jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Type == "start" && slices.Contains(ShutdownTargets, j.Unit) {
			return true, nil
		}
	}
	return false, nil
}

// Systemctl is the Manager of the running host, read with systemctl.
type Systemctl struct {
	// Path is the systemctl binary; empty means "systemctl" on $PATH.
	Path string
}

var _ Manager = Systemctl{}

func (s Systemctl) bin() string {
	if s.Path == "" {
		return "systemctl"
	}
	return s.Path
}

// SystemState implements Manager. systemctl exits non-zero for any state but
// running, so its output decides, not its exit status.
func (s Systemctl) SystemState(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, s.bin(), "is-system-running").Output() // #nosec G204 -- fixed arguments
	state := strings.TrimSpace(string(out))
	if state == "" {
		if err == nil {
			err = errors.New("empty output")
		}
		return "", fmt.Errorf("systemctl is-system-running: %w", err)
	}
	return state, nil
}

// Jobs implements Manager.
func (s Systemctl) Jobs(ctx context.Context) ([]Job, error) {
	out, err := exec.CommandContext(ctx, s.bin(), "list-jobs", "--no-legend", "--no-pager", "--plain", "--full").Output() // #nosec G204 -- fixed arguments
	if err != nil {
		return nil, fmt.Errorf("systemctl list-jobs: %w", err)
	}
	return ParseJobs(string(out)), nil
}

// ParseJobs parses systemctl list-jobs output without its legend: one job per
// line, "<id> <unit> <type> <state>".
func ParseJobs(out string) []Job {
	var jobs []Job
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		jobs = append(jobs, Job{Unit: f[1], Type: f[2]})
	}
	return jobs
}
