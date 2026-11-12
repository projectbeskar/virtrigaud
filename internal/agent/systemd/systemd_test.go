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

package systemd_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// fakeManager is a fake systemd.
type fakeManager struct {
	state    string
	stateErr error
	jobs     []systemd.Job
	jobsErr  error
}

func (f fakeManager) SystemState(context.Context) (string, error) { return f.state, f.stateErr }
func (f fakeManager) Jobs(context.Context) ([]systemd.Job, error) { return f.jobs, f.jobsErr }

// TestShutdownGuardTruthTable: the VM shutdown command acts only while the
// system is stopping or a start job of a shutdown target is queued.
func TestShutdownGuardTruthTable(t *testing.T) {
	start := func(unit string) []systemd.Job { return []systemd.Job{{Unit: unit, Type: "start"}} }
	boom := errors.New("bus unreachable")
	for _, tc := range []struct {
		name    string
		m       fakeManager
		want    bool
		wantErr bool
	}{
		{"running, no jobs (maintenance stop)", fakeManager{state: "running"}, false, false},
		{"degraded, no jobs", fakeManager{state: "degraded"}, false, false},
		{"starting, no jobs", fakeManager{state: "starting"}, false, false},
		{"maintenance, no jobs", fakeManager{state: "maintenance"}, false, false},
		{"stopping", fakeManager{state: "stopping"}, true, false},
		{"reboot queued", fakeManager{state: "running", jobs: start("reboot.target")}, true, false},
		{"poweroff queued", fakeManager{state: "degraded", jobs: start("poweroff.target")}, true, false},
		{"halt queued", fakeManager{state: "running", jobs: start("halt.target")}, true, false},
		{"shutdown queued", fakeManager{state: "running", jobs: start("shutdown.target")}, true, false},
		{"kexec queued", fakeManager{state: "running", jobs: start("kexec.target")}, true, false},
		{"other target queued", fakeManager{state: "running", jobs: start("rescue.target")}, false, false},
		{"agent restart queued", fakeManager{state: "running", jobs: []systemd.Job{{Unit: "vrtg-agent.service", Type: "restart"}}}, false, false},
		{"stop job of a shutdown target", fakeManager{state: "running", jobs: []systemd.Job{{Unit: "reboot.target", Type: "stop"}}}, false, false},
		{"state unreadable", fakeManager{stateErr: boom, jobs: start("reboot.target")}, false, true},
		{"jobs unreadable", fakeManager{state: "running", jobsErr: boom}, false, true},
		{"stopping, jobs unreadable", fakeManager{state: "stopping", jobsErr: boom}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := systemd.ShutdownInProgress(context.Background(), tc.m)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestParseJobs(t *testing.T) {
	out := "123 reboot.target start waiting\n124 vrtg-vm-shutdown.service stop running\n\nshort line\n"
	jobs := systemd.ParseJobs(out)
	if len(jobs) != 2 || jobs[0] != (systemd.Job{Unit: "reboot.target", Type: "start"}) || jobs[1].Type != "stop" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestSocketNotifier(t *testing.T) {
	if err := (systemd.SocketNotifier{}).Notify(systemd.NotifyReady); err != nil {
		t.Fatalf("no socket: %v", err)
	}
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := (systemd.SocketNotifier{Addr: path}).Notify(systemd.NotifyReady); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != systemd.NotifyReady {
		t.Fatalf("received %q, %v", buf[:n], err)
	}
}
