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
	"strconv"
	"strings"
	"testing"
)

// unitDir holds the reference systemd units.
const unitDir = "../../packaging/systemd"

// binaryPath is where the units expect the agent binary: under /usr/lib,
// which every system extension hierarchy merges (/usr/libexec is not merged
// on some immutable hosts).
const binaryPath = "/usr/lib/virtrigaud/vrtg-agent"

// Unit names the tests refer to.
const (
	setupUnit     = "vrtg-agent-setup.service"
	bootStartUnit = "vrtg-agent-boot-start.service"
	agentUnit     = "vrtg-agent.service"
	signerUnit    = "vrtg-agent-signer.service"
	shutdownUnit  = "vrtg-vm-shutdown.service"
	upholdsDropIn = "multi-user.target.d/virtrigaud-agent.conf"
)

// serviceUnits are every reference service unit.
var serviceUnits = []string{agentUnit, signerUnit, shutdownUnit, setupUnit, bootStartUnit}

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
		"SystemCallErrorNumber": "EPERM", "TimeoutStartSec": "5min",
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
	if u.one(t, "Service", "ExecStart") != binaryPath+" run" {
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
		"Group": "virtrigaud-signer", "LimitCORE": "0", "MemorySwapMax": "0",
		"ReadWritePaths": "/var/lib/virtrigaud-signer /run/virtrigaud-signer", "UMask": "0077", "SystemCallErrorNumber": "EPERM", "ProtectSystem": "strict", "ProtectHome": "yes",
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
	if u.one(t, "Service", "ExecStart") != binaryPath+" signer" {
		t.Errorf("ExecStart = %q", u.one(t, "Service", "ExecStart"))
	}
	// A certificate renewal writes the pending key beside the host key and
	// renames it over it: the key directory must not be read-only.
	if v, ok := u["Service"]["ReadOnlyPaths"]; ok {
		t.Errorf("[Service] ReadOnlyPaths = %v: a renewal cannot write the pending key", v)
	}
	// The signer is not the agent's user and shares nothing of its.
	a := parseUnit(t, "vrtg-agent.service")
	for _, key := range []string{"User", "Group"} {
		if u.one(t, "Service", key) == a.one(t, "Service", key) {
			t.Errorf("[Service] %s is the agent's: the two must be separate users", key)
		}
	}
	// The key directory is made by tmpfiles.d with its owner and mode; it is
	// not one of the service's own state directories, which systemd would
	// create and re-own on its own terms.
	if _, ok := u["Service"]["StateDirectory"]; ok {
		t.Error("[Service] StateDirectory is set: the key directory comes from tmpfiles.d")
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
				if strings.Contains(v, agentUnit) || strings.Contains(v, bootStartUnit) || strings.Contains(v, "vrtg-vm-shutdown") {
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
	if u.one(t, "Service", "ExecStop") != binaryPath+" shutdown-vms" {
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
				for _, name := range strings.Fields(v) {
					if strings.HasPrefix(name, "vrtg-agent") && name != setupUnit {
						t.Errorf("[%s] %s=%s refers to an agent unit other than the setup unit", section, k, v)
					}
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

// unitNames returns the unit names a dependency key's values list.
func (u unit) unitNames(section, key string) []string {
	var out []string
	for _, v := range u[section][key] {
		out = append(out, strings.Fields(v)...)
	}
	return out
}

// TestUnitsFailFilteredCallsWithEPERM: every unit that filters system calls
// makes a filtered call fail with EPERM instead of killing the process (the
// default). The agent's host inventory probes open_by_handle_at, which the
// filter's ~@privileged denies; on a kernel that reaches it, the default
// would kill the agent after it became ready. The inventory package's
// TestProbeSyscallsAreAccountedFor checks that the probe reads EPERM as "not
// supported" and that every call it makes is classified against this filter.
func TestUnitsFailFilteredCallsWithEPERM(t *testing.T) {
	for _, name := range serviceUnits {
		u := parseUnit(t, name)
		if len(u["Service"]["SystemCallFilter"]) == 0 {
			t.Errorf("%s: no SystemCallFilter", name)
			continue
		}
		if got := u.one(t, "Service", "SystemCallErrorNumber"); got != "EPERM" {
			t.Errorf("%s: SystemCallErrorNumber = %q, want EPERM", name, got)
		}
	}
	a := parseUnit(t, agentUnit)
	if got := a["Service"]["SystemCallFilter"]; !slices.Equal(got, []string{"@system-service", "~@privileged"}) {
		t.Errorf("agent SystemCallFilter = %v: the inventory package classifies the probe's calls against this filter", got)
	}
}

// TestUnitsUseTheLibBinaryPath: no unit runs a binary from /usr/libexec,
// which some immutable hosts do not merge from a system extension.
func TestUnitsUseTheLibBinaryPath(t *testing.T) {
	for _, name := range serviceUnits {
		u := parseUnit(t, name)
		for _, key := range []string{"ExecStart", "ExecStop", "ExecReload", "ExecStartPre"} {
			for _, v := range u["Service"][key] {
				if strings.Contains(v, "/usr/libexec/") {
					t.Errorf("%s: %s=%s runs a binary under /usr/libexec", name, key, v)
				}
				if strings.Contains(v, "vrtg-agent ") && !strings.HasPrefix(v, binaryPath+" ") {
					t.Errorf("%s: %s=%s does not run %s", name, key, v, binaryPath)
				}
			}
		}
	}
}

// TestUnitsWantTheSetupUnit: the agent, the signer, the shutdown unit and
// the once-per-boot start unit want the setup unit and start after it, so
// that their users and directories exist before they run.
func TestUnitsWantTheSetupUnit(t *testing.T) {
	for _, name := range []string{agentUnit, signerUnit, shutdownUnit, bootStartUnit} {
		u := parseUnit(t, name)
		if !slices.Contains(u.unitNames("Unit", "Wants"), setupUnit) {
			t.Errorf("%s: Wants = %v, want %s", name, u["Unit"]["Wants"], setupUnit)
		}
		if !slices.Contains(u.unitNames("Unit", "After"), setupUnit) {
			t.Errorf("%s: After = %v, want %s", name, u["Unit"]["After"], setupUnit)
		}
	}
}

// TestSetupUnitProperties: the setup unit is a oneshot that stays active,
// runs systemd-sysusers and systemd-tmpfiles on the agent's fragments only
// (by file name, so an /etc fragment of that name wins), runs after the
// system extensions are merged and the boot's own sysusers pass, and before
// the agent's units; it has no default dependencies (it is ordered before
// units that may start early) and wants or requires no agent unit.
func TestSetupUnitProperties(t *testing.T) {
	u := parseUnit(t, setupUnit)
	for key, want := range map[string]string{"Type": "oneshot", "RemainAfterExit": "yes", "NoNewPrivileges": "yes"} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
	}
	if got := u["Service"]["ExecStart"]; !slices.Equal(got, []string{
		"/usr/bin/systemd-sysusers virtrigaud-agent.conf",
		"/usr/bin/systemd-tmpfiles --create virtrigaud-agent.conf",
	}) {
		t.Errorf("ExecStart = %v", got)
	}
	if got := u.one(t, "Unit", "DefaultDependencies"); got != "no" {
		t.Errorf("DefaultDependencies = %q", got)
	}
	after := u.unitNames("Unit", "After")
	for _, want := range []string{"systemd-sysext.service", "systemd-sysusers.service", "local-fs.target"} {
		if !slices.Contains(after, want) {
			t.Errorf("After = %v, want %s", after, want)
		}
	}
	before := u.unitNames("Unit", "Before")
	for _, want := range []string{agentUnit, signerUnit, shutdownUnit} {
		if !slices.Contains(before, want) {
			t.Errorf("Before = %v, want %s", before, want)
		}
	}
	for _, k := range []string{"Wants", "Requires", "Requisite", "BindsTo", "Upholds", "PartOf"} {
		for _, v := range u.unitNames("Unit", k) {
			if strings.HasPrefix(v, "vrtg-") {
				t.Errorf("[Unit] %s=%s: the setup unit pulls in no agent unit", k, v)
			}
		}
	}
}

// TestBootStartUnitProperties: the once-per-boot start unit is a oneshot
// that stays active after it ran, so that Upholds= does not run it again in
// the same boot; it asks systemd to start the agent without waiting; and it
// has no dependency of any kind on the agent's unit, so that stopping the
// agent for maintenance never involves it.
func TestBootStartUnitProperties(t *testing.T) {
	u := parseUnit(t, bootStartUnit)
	for key, want := range map[string]string{
		"Type": "oneshot", "RemainAfterExit": "yes", "NoNewPrivileges": "yes", "CapabilityBoundingSet": "",
		"ExecStart": "/usr/bin/systemctl --no-block start " + agentUnit, "ProtectSystem": "strict",
		"RestrictAddressFamilies": "AF_UNIX",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
	}
	for section, keys := range u {
		for _, k := range dependencyKeys {
			for _, v := range keys[k] {
				if slices.Contains(strings.Fields(v), agentUnit) || slices.Contains(strings.Fields(v), signerUnit) {
					t.Errorf("[%s] %s=%s: the start unit must not depend on the agent", section, k, v)
				}
			}
		}
	}
	if got := u["Service"]["ExecStop"]; len(got) != 0 {
		t.Errorf("ExecStop = %v: stopping the start unit must not touch the agent", got)
	}
}

// TestUpholdsDropIn: multi-user.target upholds the shutdown unit and the
// once-per-boot start unit, which reach a system extension merged during
// the boot, and never the agent or the signer: a unit upheld is started
// again whenever it stops, so a maintenance stop would be undone.
func TestUpholdsDropIn(t *testing.T) {
	u := parseUnit(t, upholdsDropIn)
	if len(u) != 1 || len(u["Unit"]) != 1 {
		t.Fatalf("the drop-in sets more than [Unit] Upholds=: %v", u)
	}
	got := u.unitNames("Unit", "Upholds")
	slices.Sort(got)
	if want := []string{bootStartUnit, shutdownUnit}; !slices.Equal(got, want) {
		t.Errorf("Upholds = %v, want %v", got, want)
	}
	// Nothing upholds or binds the agent anywhere else either.
	for _, name := range serviceUnits {
		for section, keys := range parseUnit(t, name) {
			for _, k := range []string{"Upholds", "BindsTo", "Requires", "PartOf"} {
				if slices.Contains(strings.Fields(strings.Join(keys[k], " ")), agentUnit) {
					t.Errorf("%s [%s] %s names the agent's unit", name, section, k)
				}
			}
		}
	}
}

// TestSysusersFixedIDs: both users have fixed ids in the system range, so
// that they keep the ids owning their persistent state directories where
// /etc/passwd is rebuilt at every boot; the two ids differ.
func TestSysusersFixedIDs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(unitDir, "virtrigaud-agent.sysusers.conf"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "u" {
			continue
		}
		id, err := strconv.Atoi(f[2])
		if err != nil {
			t.Errorf("user %s: id %q is not a fixed number", f[1], f[2])
			continue
		}
		if id < 100 || id > 999 {
			t.Errorf("user %s: id %d is outside the system range", f[1], id)
		}
		ids[f[1]] = id
	}
	if len(ids) != 2 || ids["virtrigaud-agent"] == 0 || ids["virtrigaud-signer"] == 0 || ids["virtrigaud-agent"] == ids["virtrigaud-signer"] {
		t.Errorf("ids = %v, want two distinct fixed ids for both users", ids)
	}
}
