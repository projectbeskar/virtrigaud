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
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

type stopper struct{ calls int }

func (s *stopper) StopAll(context.Context) error {
	s.calls++
	return nil
}

type manager struct {
	state string
	jobs  []systemd.Job
	err   error
}

func (m manager) SystemState(context.Context) (string, error) { return m.state, m.err }
func (m manager) Jobs(context.Context) ([]systemd.Job, error) { return m.jobs, nil }

// TestShutdownVMsGuard: shutdown-vms stops the VMs only while the host shuts
// down, and never when it cannot tell.
func TestShutdownVMsGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		m     manager
		calls int
		code  int
	}{
		{"maintenance stop", manager{state: "running"}, 0, 0},
		{"system stopping", manager{state: "stopping"}, 1, 0},
		{"reboot queued", manager{state: "running", jobs: []systemd.Job{{Unit: "reboot.target", Type: "start"}}}, 1, 0},
		{"soft reboot queued", manager{state: "running", jobs: []systemd.Job{{Unit: "soft-reboot.target", Type: "start"}}}, 1, 0},
		{"state unreadable", manager{err: errors.New("no bus")}, 0, 1},
	} {
		s := &stopper{}
		if code := shutdownVMs(context.Background(), tc.m, setupState{}, stopperOf(s), logr.Discard()); code != tc.code || s.calls != tc.calls {
			t.Errorf("%s: exit %d, StopAll called %d times; want exit %d, %d calls", tc.name, code, s.calls, tc.code, tc.calls)
		}
	}
}

// setupState is the setup unit's state as systemctl is-failed reports it.
type setupState struct {
	failed bool
	err    error
}

func (u setupState) Failed(_ context.Context, unit string) (bool, error) {
	if unit != systemd.SetupUnit {
		return false, errors.New("asked about " + unit)
	}
	return u.failed, u.err
}

// stopperOf returns s as the executor shutdown-vms builds.
func stopperOf(s *stopper) func() (executor.Stopper, error) {
	return func() (executor.Stopper, error) { return s, nil }
}

// TestShutdownVMsSkipsWhenSetupFailed: when the setup unit failed (the
// agent's user missing or its uid moved), the stop action logs a warning and
// exits 0 without building an executor or stopping anything, even during a
// host shutdown; an unreadable setup state does not skip, and the guard and
// the executor decide as before.
func TestShutdownVMsSkipsWhenSetupFailed(t *testing.T) {
	down := manager{state: "stopping"}
	var logs bytes.Buffer
	built := false
	code := shutdownVMs(context.Background(), down, setupState{failed: true}, func() (executor.Stopper, error) {
		built = true
		return nil, errors.New("must not be built")
	}, newLogger(&logs))
	if code != 0 || built {
		t.Fatalf("setup failed: exit %d, executor built %v; want exit 0, not built", code, built)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "skipping") ||
		!strings.Contains(logs.String(), systemd.SetupUnit) {
		t.Fatalf("setup-failed log: %s", logs.String())
	}

	s := &stopper{}
	if code := shutdownVMs(context.Background(), down, setupState{err: errors.New("no bus")}, stopperOf(s), logr.Discard()); code != 0 || s.calls != 1 {
		t.Fatalf("setup state unreadable: exit %d, %d calls; want the guard to decide (exit 0, 1 call)", code, s.calls)
	}
	if code := shutdownVMs(context.Background(), down, setupState{}, func() (executor.Stopper, error) {
		return nil, errors.New("state unreadable")
	}, logr.Discard()); code != 1 {
		t.Fatalf("executor fails: exit %d, want 1 (fail-closed)", code)
	}
}

func TestCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, 2}, {[]string{"nope"}, 2}, {[]string{"version"}, 0}, {[]string{"help"}, 0}, {[]string{"enroll"}, enrollNoStepStatus},
		{[]string{"run", "--state-dir=relative"}, 2}, {[]string{"run", "--no-such-flag"}, 2},
		{[]string{"shutdown-vms", "--executor=other"}, 2},
	} {
		if code := realMain(tc.args, &out, &errOut); code != tc.code {
			t.Errorf("%v: exit %d, want %d\n%s", tc.args, code, tc.code, errOut.String())
		}
	}
}
