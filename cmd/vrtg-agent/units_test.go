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
		"ProtectSystem": "strict", "RestartPreventExitStatus": "2", "ProtectProc": "invisible", "PrivateIPC": "yes",
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
	for _, key := range []string{"LimitNOFILE", "TasksMax", "MemoryMax"} {
		if u.one(t, "Service", key) == "" {
			t.Errorf("[Service] %s is empty", key)
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

// TestAgentUnitOrdersAfterTheSigner: the agent wants the signer and starts
// after it, but does not require it: a signer restart must not restart the
// agent, which fails closed on its own while the signer is away.
func TestAgentUnitOrdersAfterTheSigner(t *testing.T) {
	const signerUnit = "vrtg-agent-signer.service"
	u := parseUnit(t, "vrtg-agent.service")
	if !slices.ContainsFunc(u["Unit"]["Wants"], func(v string) bool { return slices.Contains(strings.Fields(v), signerUnit) }) {
		t.Errorf("[Unit] Wants = %v, want %s", u["Unit"]["Wants"], signerUnit)
	}
	if !slices.ContainsFunc(u["Unit"]["After"], func(v string) bool { return slices.Contains(strings.Fields(v), signerUnit) }) {
		t.Errorf("[Unit] After = %v, want %s", u["Unit"]["After"], signerUnit)
	}
	for section, keys := range u {
		for _, k := range []string{"Requires", "Requisite", "BindsTo", "PartOf", "PropagatesStopTo", "StopPropagatedFrom"} {
			if slices.ContainsFunc(keys[k], func(v string) bool { return strings.Contains(v, "signer") }) {
				t.Errorf("[%s] %s refers to the signer: its restart would restart the agent", section, k)
			}
		}
	}
}

// TestSignerUnitProperties: the signer's unit runs as its own user, not the
// agent's; is a notify service restarted always with a bounded start rate;
// has no new privileges, no capability, no network (private network
// namespace and AF_UNIX only) and a syscall filter; reaches only its own
// state and the socket directory; and neither delegates a cgroup nor creates
// a scope.
func TestSignerUnitProperties(t *testing.T) {
	u := parseUnit(t, "vrtg-agent-signer.service")
	for key, want := range map[string]string{
		"Type": "notify", "NotifyAccess": "main", "NoNewPrivileges": "yes", "Restart": "always", "RestartSec": "2",
		"KillMode": "process", "CapabilityBoundingSet": "", "AmbientCapabilities": "", "User": "virtrigaud-signer",
		"Group": "virtrigaud-signer", "ReadOnlyPaths": "/var/lib/virtrigaud-signer", "LimitCORE": "0", "MemorySwapMax": "0",
		"ReadWritePaths": "/run/virtrigaud-signer", "UMask": "0077", "ProtectSystem": "strict", "ProtectHome": "yes",
		"RestartPreventExitStatus": "2", "ProtectProc": "invisible", "PrivateIPC": "yes", "PrivateNetwork": "yes",
		"PrivateTmp": "yes", "PrivateDevices": "yes", "RestrictAddressFamilies": "AF_UNIX", "MemoryDenyWriteExecute": "yes",
		"SystemCallArchitectures": "native", "RestrictNamespaces": "yes", "LockPersonality": "yes", "RestrictSUIDSGID": "yes",
		"ProtectKernelTunables": "yes", "ProtectKernelModules": "yes", "ProtectKernelLogs": "yes", "ProtectControlGroups": "yes",
		"ProtectClock": "yes", "ProtectHostname": "yes", "RestrictRealtime": "yes",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
	}
	if got := u["Service"]["SystemCallFilter"]; !slices.Equal(got, []string{"@system-service", "~@privileged"}) {
		t.Errorf("SystemCallFilter = %v", got)
	}
	for key, want := range map[string]string{"StartLimitIntervalSec": "300", "StartLimitBurst": "10"} {
		if got := u.one(t, "Unit", key); got != want {
			t.Errorf("[Unit] %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"LimitNOFILE", "TasksMax", "MemoryMax"} {
		if u.one(t, "Service", key) == "" {
			t.Errorf("[Service] %s is empty", key)
		}
	}
	if !strings.HasSuffix(u.one(t, "Service", "ExecStart"), "/vrtg-agent signer") {
		t.Errorf("ExecStart = %q", u.one(t, "Service", "ExecStart"))
	}
	// The signer is not the agent's user and shares nothing of its.
	a := parseUnit(t, "vrtg-agent.service")
	for _, key := range []string{"User", "Group"} {
		if u.one(t, "Service", key) == a.one(t, "Service", key) {
			t.Errorf("[Service] %s is the agent's: the two must be separate users", key)
		}
	}
	// The signer never writes its state directory: systemd must not make it
	// one of the service's own (read-write) state directories.
	if _, ok := u["Service"]["StateDirectory"]; ok {
		t.Error("[Service] StateDirectory is set: the key directory is read-only to the signer")
	}
	// PrivateUsers would hide the agent's uid from SO_PEERCRED.
	for _, key := range []string{"Delegate", "Slice", "DelegateSubgroup", "PrivateUsers"} {
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
	// It reaches no unit of the agent's.
	for section, keys := range u {
		for _, k := range dependencyKeys {
			for _, v := range keys[k] {
				if strings.Contains(v, "vrtg-agent.service") || strings.Contains(v, "vrtg-vm-shutdown") {
					t.Errorf("[%s] %s=%s: the signer depends on nothing of the agent", section, k, v)
				}
			}
		}
	}
}

// TestSysusersAndTmpfilesCoverTheSigner: the signer's user, its state
// directory and the socket directory (group the agent's, setgid, nothing for
// others) are declared.
func TestSysusersAndTmpfilesCoverTheSigner(t *testing.T) {
	sysusers, err := os.ReadFile(filepath.Join(unitDir, "virtrigaud-agent.sysusers.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sysusers), "u virtrigaud-signer ") || !strings.Contains(string(sysusers), "u virtrigaud-agent ") {
		t.Errorf("sysusers does not declare both users:\n%s", sysusers)
	}
	tmpfiles, err := os.ReadFile(filepath.Join(unitDir, "virtrigaud-agent.tmpfiles.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"d /var/lib/virtrigaud-signer     0700 virtrigaud-signer virtrigaud-signer -",
		"d /run/virtrigaud-signer         2750 virtrigaud-signer virtrigaud-agent -",
	} {
		if !strings.Contains(string(tmpfiles), want) {
			t.Errorf("tmpfiles lacks %q", want)
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
	// ProtectProc=invisible would hide /proc/1 from the guard's systemctl.
	if _, ok := u["Service"]["ProtectProc"]; ok {
		t.Error("ProtectProc is set on the shutdown unit")
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
