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
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// unitDir holds the reference systemd units.
const unitDir = "../../packaging/systemd"

// unit is a parsed systemd unit: section -> key -> values in order.
type unit map[string]map[string][]string

func parseUnit(t *testing.T, name string) unit {
	t.Helper()
	f, err := os.Open(filepath.Join(unitDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	u := unit{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.Trim(line, "[]")
			u[section] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("%s: malformed line %q", name, line)
			}
			u[section][k] = append(u[section][k], v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return u
}

func (u unit) one(t *testing.T, section, key string) string {
	t.Helper()
	v := u[section][key]
	if len(v) != 1 {
		t.Fatalf("[%s] %s = %v, want exactly one value", section, key, v)
	}
	return v[0]
}

// dependencyKeys are the keys that pull in, order after, or keep another unit
// running.
var dependencyKeys = []string{"Wants", "Requires", "Requisite", "BindsTo", "PartOf", "Upholds", "After", "Before", "Also", "PropagatesStopTo", "StopPropagatedFrom"}

// TestAgentUnitProperties: the agent's unit is a notify service that is
// restarted always with a bounded start rate, has no new privileges and no
// capability, kills only its main process, and delegates no cgroup and
// creates no scope.
func TestAgentUnitProperties(t *testing.T) {
	u := parseUnit(t, "vrtg-agent.service")
	for key, want := range map[string]string{
		"Type": "notify", "NoNewPrivileges": "yes", "Restart": "always", "RestartSec": "2", "KillMode": "process",
		"CapabilityBoundingSet": "", "AmbientCapabilities": "", "User": "virtrigaud-agent", "StateDirectoryMode": "0700",
		"ProtectSystem": "strict",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{"StartLimitIntervalSec": "300", "StartLimitBurst": "10"} {
		if got := u.one(t, "Unit", key); got != want {
			t.Errorf("[Unit] %s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasSuffix(u.one(t, "Service", "ExecStart"), "/vrtg-agent run") {
		t.Errorf("ExecStart = %q", u.one(t, "Service", "ExecStart"))
	}
	for _, key := range []string{"Delegate", "Slice", "DelegateSubgroup", "PrivateUsers", "PrivateNetwork"} {
		if _, ok := u["Service"][key]; ok {
			t.Errorf("[Service] %s is set", key)
		}
	}
	for _, s := range u {
		for k, vs := range s {
			for _, v := range vs {
				if strings.Contains(v, ".scope") || strings.Contains(k, "Scope") {
					t.Errorf("%s=%s names a scope", k, v)
				}
			}
		}
	}
}

// TestShutdownUnitProperties: the shutdown unit runs shutdown-vms as its
// stop action, has no new privileges and no capability, and neither wants,
// requires, binds to nor orders itself against the agent's unit, so it never
// starts an agent stopped for maintenance.
func TestShutdownUnitProperties(t *testing.T) {
	u := parseUnit(t, "vrtg-vm-shutdown.service")
	for key, want := range map[string]string{
		"Type": "oneshot", "RemainAfterExit": "yes", "NoNewPrivileges": "yes", "CapabilityBoundingSet": "",
		"AmbientCapabilities": "", "User": "virtrigaud-agent",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasSuffix(u.one(t, "Service", "ExecStop"), "/vrtg-agent shutdown-vms") {
		t.Errorf("ExecStop = %q", u.one(t, "Service", "ExecStop"))
	}
	if _, ok := u["Service"]["Delegate"]; ok {
		t.Error("Delegate is set")
	}
	for section, keys := range u {
		for _, k := range dependencyKeys {
			for _, v := range keys[k] {
				if strings.Contains(v, "vrtg-agent") {
					t.Errorf("[%s] %s=%s refers to the agent's unit", section, k, v)
				}
			}
		}
	}
	// And the agent's unit does not pull the shutdown unit in either.
	a := parseUnit(t, "vrtg-agent.service")
	for section, keys := range a {
		for _, k := range dependencyKeys {
			if slices.ContainsFunc(keys[k], func(v string) bool { return strings.Contains(v, "vrtg-vm-shutdown") }) {
				t.Errorf("agent unit [%s] %s refers to the shutdown unit", section, k)
			}
		}
	}
}
