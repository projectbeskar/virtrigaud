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
	"os/exec"
	"os/user"
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
		// The audit lines are the durable record: journald must not drop them.
		"LogRateLimitIntervalSec": "0",
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

// TestUnitsRequireTheSetupUnit: the agent and the signer require the setup
// unit and start after it, so that their users and directories exist before
// they run and neither starts when it failed (a user id moved under the
// state files). The upheld units (the shutdown unit and the once-per-boot
// start unit) want it and start after it, and never require it: Upholds=
// starts a failed upheld unit again without end, so a Requires= on a failing
// setup unit would retry both forever.
func TestUnitsRequireTheSetupUnit(t *testing.T) {
	for _, name := range []string{shutdownUnit, bootStartUnit} {
		u := parseUnit(t, name)
		if slices.Contains(u.unitNames("Unit", "Requires"), setupUnit) || slices.Contains(u.unitNames("Unit", "BindsTo"), setupUnit) {
			t.Errorf("%s: an upheld unit requires the setup unit", name)
		}
		if !slices.Contains(u.unitNames("Unit", "Wants"), setupUnit) || !slices.Contains(u.unitNames("Unit", "After"), setupUnit) {
			t.Errorf("%s: Wants = %v, After = %v, want %s in both", name, u["Unit"]["Wants"], u["Unit"]["After"], setupUnit)
		}
	}
	for _, name := range []string{agentUnit, signerUnit} {
		u := parseUnit(t, name)
		if !slices.Contains(u.unitNames("Unit", "Requires"), setupUnit) {
			t.Errorf("%s: Requires = %v, want %s", name, u["Unit"]["Requires"], setupUnit)
		}
		if !slices.Contains(u.unitNames("Unit", "After"), setupUnit) {
			t.Errorf("%s: After = %v, want %s", name, u["Unit"]["After"], setupUnit)
		}
	}
}

// TestNoSystemCallLog: no unit sets SystemCallLog=. seccomp applies the
// action of highest precedence, so a call the filter refuses with EPERM is
// never logged; the setting would only suggest an audit that never happens.
func TestNoSystemCallLog(t *testing.T) {
	for _, name := range serviceUnits {
		if v, ok := parseUnit(t, name)["Service"]["SystemCallLog"]; ok {
			t.Errorf("%s: SystemCallLog = %v", name, v)
		}
	}
}

// unitShell returns the script of an ExecStart=/bin/sh -c '...' line, with
// systemd's escapes ($$, %%) undone.
func unitShell(t *testing.T, line string) string {
	t.Helper()
	script, ok := strings.CutPrefix(line, "/bin/sh -c '")
	if !ok || !strings.HasSuffix(script, "'") {
		t.Fatalf("not a /bin/sh -c line: %s", line)
	}
	return strings.NewReplacer("$$", "$", "%%", "%").Replace(strings.TrimSuffix(script, "'"))
}

// TestSetupUnitRefusesMovedIDs: the setup unit's owner check decides by
// content: it passes when the state directories are missing, empty, or hold
// only files, directories and links owned by their users' uids, whatever the
// owners of the directories tmpfiles manages (the state directories and
// pki/: a bind mount root recreated as root at boot, or directories a
// boot-time tmpfiles run already re-owned); it fails, naming the entries,
// when a file, a subdirectory or a link is owned by another uid, or its user
// does not exist. A symbolic link is not followed.
func TestSetupUnitRefusesMovedIDs(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if me.Uid == "0" {
		t.Skip("needs a user other than root")
	}
	script := unitShell(t, parseUnit(t, setupUnit)["Service"]["ExecStart"][1])
	run := func(agentUser, signerUser string, mk ...string) (string, error) {
		t.Helper()
		root := t.TempDir()
		for _, p := range mk {
			full := filepath.Join(root, p)
			switch {
			case strings.HasSuffix(p, "/"):
				if err := os.MkdirAll(full, 0o700); err != nil {
					t.Fatal(err)
				}
				continue
			case strings.HasSuffix(p, "@"):
				full = strings.TrimSuffix(full, "@")
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/passwd", full); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s := strings.NewReplacer(
			"/var/lib/", root+"/var/lib/",
			" virtrigaud-agent ", " "+agentUser+" ",
		).Replace(script)
		s = strings.TrimSuffix(s, " virtrigaud-signer") + " " + signerUser
		cmd := exec.Command("sh", "-c", s)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stderr.String(), err
	}
	files := []string{"var/lib/virtrigaud-agent/host-id", "var/lib/virtrigaud-agent/pki/host.crt", "var/lib/virtrigaud-signer/host.key"}
	if _, err := run(me.Username, me.Username); err != nil {
		t.Errorf("nothing exists yet: %v", err)
	}
	if _, err := run(me.Username, me.Username, files...); err != nil {
		t.Errorf("files owned by the users' uids: %v", err)
	}
	// The directories' owner (here the test's user, and the users root) is
	// not a reason: empty state directories pass.
	if out, err := run("root", "root", "var/lib/virtrigaud-agent/pki/", "var/lib/virtrigaud-signer/"); err != nil {
		t.Errorf("empty state directories of another owner were refused: %v %s", err, out)
	}
	// A symbolic link is not followed (its target is root's /etc/passwd);
	// its own owner counts.
	if out, err := run(me.Username, me.Username, "var/lib/virtrigaud-agent/link@"); err != nil {
		t.Errorf("a symbolic link was followed: %v %s", err, out)
	}
	if out, err := run("root", me.Username, "var/lib/virtrigaud-agent/link@"); err == nil || !strings.Contains(out, "link") {
		t.Errorf("a link of another owner was accepted: %v %s", err, out)
	}
	// A subdirectory (not one tmpfiles manages) of another owner fails.
	if out, err := run("root", me.Username, "var/lib/virtrigaud-agent/witness/"); err == nil || !strings.Contains(out, "witness") {
		t.Errorf("a subdirectory of another owner was accepted: %v %s", err, out)
	}
	out, err := run(me.Username, "root", files...)
	if err == nil || !strings.Contains(out, "host.key") {
		t.Errorf("the signer's key owned by another uid was accepted, or not named: %v %s", err, out)
	}
	out, err = run("root", me.Username, files...)
	if err == nil || !strings.Contains(out, "host-id") || !strings.Contains(out, "pki/host.crt") {
		t.Errorf("the agent's files owned by another uid were accepted, or not named: %v %s", err, out)
	}
	if _, err := run(me.Username, "no-such-user-for-this-test", files...); err == nil {
		t.Error("a missing user was accepted")
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
	if got := u["Service"]["ExecStart"]; len(got) != 3 || got[0] != "/usr/bin/systemd-sysusers virtrigaud-agent.conf" ||
		!strings.HasPrefix(got[1], "/bin/sh -c 'check() ") || got[2] != "/usr/bin/systemd-tmpfiles --create virtrigaud-agent.conf" {
		t.Errorf("ExecStart = %v: sysusers, the owner check, then tmpfiles", got)
	}
	// Writable: /etc for sysusers, the state and socket parents for tmpfiles;
	// the capabilities those two need; nothing else.
	for key, want := range map[string]string{
		"ProtectSystem": "strict", "ReadWritePaths": "/etc /var/lib /run", "ProtectProc": "invisible", "PrivateIPC": "yes",
		"CapabilityBoundingSet": "CAP_CHOWN CAP_FOWNER CAP_FSETID CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH", "AmbientCapabilities": "",
	} {
		if got := u.one(t, "Service", key); got != want {
			t.Errorf("[Service] %s = %q, want %q", key, got, want)
		}
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
		"ExecStart": binaryPath + " boot-start", "ProtectSystem": "strict",
		// The once-per-boot marker's directory survives restarts of the unit
		// (a package's try-restart included) and goes with the reboot.
		"RuntimeDirectory": "vrtg-agent-boot-start", "RuntimeDirectoryPreserve": "yes", "RuntimeDirectoryMode": "0700",
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
	// Enablement is checked in the action, never as a condition: a skipped
	// unit would be inactive, and Upholds= would start it again and again.
	for _, k := range []string{"ExecCondition"} {
		if v, ok := u["Service"][k]; ok {
			t.Errorf("[Service] %s = %v", k, v)
		}
	}
	for k := range u["Unit"] {
		if strings.HasPrefix(k, "Condition") || strings.HasPrefix(k, "Assert") {
			t.Errorf("[Unit] %s is set", k)
		}
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

// TestStartLimits: the shutdown, boot-start and setup units are never
// start-limited (StartLimitIntervalSec=0, no StartLimitBurst: leaving the key
// out would fall back to the manager's default limit). Their stops, starts,
// restarts and Upholds= re-arms all count towards a start limit, so routine
// maintenance put them in start-limit-hit. For the shutdown unit a reboot
// inside the window then runs no stop action. For the setup unit the agent
// cannot start and the shutdown unit skips its graceful stop, so a reboot
// stops no VM. For the boot-start unit the host reports itself degraded
// although nothing failed. A start limit cannot bound Upholds= retries
// anyway, and nothing retries the setup unit by itself. The agent and the
// signer keep theirs: they restart themselves, and the limit ends a crash
// loop.
func TestStartLimits(t *testing.T) {
	for _, name := range []string{shutdownUnit, bootStartUnit, setupUnit} {
		u := parseUnit(t, name)
		if got := u.one(t, "Unit", "StartLimitIntervalSec"); got != "0" {
			t.Errorf("%s: StartLimitIntervalSec = %q, want 0: routine maintenance could leave it in start-limit-hit", name, got)
		}
		if _, ok := u["Unit"]["StartLimitBurst"]; ok {
			t.Errorf("%s: StartLimitBurst is set", name)
		}
	}
	for _, name := range []string{agentUnit, signerUnit} {
		u := parseUnit(t, name)
		if got := u.one(t, "Service", "Restart"); got != "always" {
			t.Errorf("%s: Restart = %q", name, got)
		}
		if got := u.one(t, "Unit", "StartLimitIntervalSec"); got == "0" {
			t.Errorf("%s: StartLimitIntervalSec = 0: a crash loop would never end", name)
		}
	}
}
