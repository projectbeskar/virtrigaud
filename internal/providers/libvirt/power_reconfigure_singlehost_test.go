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

package libvirt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// This file pins D9 for Power and Reconfigure (ADR-0007 Addendum A, slice 2):
// on a SINGLE-HOST provider both run on p.virshProvider and emit exactly the
// virsh / host command sequence they emitted before routing. The expected
// sequences live in testdata/single_host_power_reconfigure.golden.json, which
// was captured by running this very test against origin/main at 5c4a335 (the
// commit before the Power/Reconfigure cores were refactored) with
// VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN=1. A change to any single-host sequence —
// which would restart the ADR-0008 D5 soak window — fails here.

// callSeqGoldenFile is the golden single-host call sequences, captured on
// origin/main before the slice 2 refactor.
const callSeqGoldenFile = "testdata/single_host_power_reconfigure.golden.json"

// callSeqUpdateEnv, set to "1", rewrites callSeqGoldenFile from the current
// code instead of comparing against it.
const callSeqUpdateEnv = "VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN"

// opsDomainName is the domain every call-sequence scenario acts on. It is
// distinctive because single-host Power writes /tmp/<name>-sync.xml on a local
// connection (removed again by the test).
const opsDomainName = "vrcallseq-web"

// opsDiskPath is the primary disk the fake domblklist reports.
const opsDiskPath = "/var/lib/libvirt/images/" + opsDomainName + "-disk.qcow2"

// opsFakeVirsh is a scriptable fake `virsh`. It routes on the -c URI (the host
// tag is the URI path, "local" without -c), logs every call as
// "<host> <args>", and answers the subcommands Power and Reconfigure use. Per
// host, a file named fail-<subcommand> makes that subcommand fail, "state"
// holds the domstate answer (default "shut off"), and dom-<name>.xml marks a
// domain (by name or UUID) as present; a domain command on anything else fails
// like libvirt's "failed to get domain".
const opsFakeVirsh = `#!/bin/sh
host=local
if [ "$1" = "-c" ]; then host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
d="$FAKE_VIRSH_DIR/$host"
fail() { if [ -f "$d/fail-$1" ]; then echo "error: scripted failure of $1" >&2; exit 1; fi; }
nodom() { echo "error: failed to get domain '$1'" >&2; exit 1; }
case "$1" in
  list) cat "$d/list.txt" ;;
  dumpxml) if [ -f "$d/dom-$2.xml" ]; then cat "$d/dom-$2.xml"; else nodom "$2"; fi ;;
  dominfo)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    uuid=11111111-2222-4333-8444-555555555555
    if [ -f "$d/uuid" ]; then uuid=$(cat "$d/uuid"); fi
    printf 'Id:             -\nName:           %s\nUUID:           %s\nState:          shut off\nCPU(s):         2\nMax memory:     2097152 KiB\n' "$2" "$uuid" ;;
  domstate)
    fail domstate
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    if [ -f "$d/state" ]; then cat "$d/state"; else echo "shut off"; fi ;;
  start|destroy|shutdown|setvcpus|setmem|setmaxmem|blockresize|undefine)
    fail "$1"
    [ -f "$d/dom-$2.xml" ] || nodom "$2" ;;
  vol-resize|define) fail "$1"; exit 0 ;;
  domblklist)
    fail domblklist
    printf ' Target   Source\n------------------------------------------------\n vda      %s\n' "$FAKE_DISK_PATH" ;;
  domblkinfo)
    fail domblkinfo
    printf 'Capacity:       10737418240\nAllocation:     1073741824\nPhysical:       1073741824\n' ;;
  qemu-agent-command)
    fail qemu-agent-command
    case "$*" in
      *guest-ping*) echo '{"return":{}}' ;;
      *guest-exec-status*) echo '{"return":{"exited":true,"exitcode":0,"out-data":""}}' ;;
      *guest-exec*) echo '{"return":{"pid":7}}' ;;
      *) echo "fake virsh: unsupported agent command: $*" >&2; exit 1 ;;
    esac ;;
  *) echo "fake virsh: unsupported: $*" >&2; exit 1 ;;
esac
`

// newOpsFixture installs opsFakeVirsh (and logging sudo/rm shims, so no host
// file is touched) for hosts: host -> domain name -> dumpxml document. Every
// domain is also addressable by the UUID in its document.
func newOpsFixture(t *testing.T, hosts map[string]map[string]string) *routingFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shims")
	}
	dir := t.TempDir()
	for host, domains := range hosts {
		hd := filepath.Join(dir, host)
		require.NoError(t, os.MkdirAll(hd, 0o700))
		var list strings.Builder
		list.WriteString(" Id   Name                 State\n------------------------------------\n")
		for name, xml := range domains {
			fmt.Fprintf(&list, " -    %-20s shut off\n", name)
			require.NoError(t, os.WriteFile(filepath.Join(hd, "dom-"+name+".xml"), []byte(xml), 0o600))
			if d, err := parseDomainLibvirtxml(xml); err == nil && strings.TrimSpace(d.UUID) != "" {
				require.NoError(t, os.WriteFile(filepath.Join(hd, "dom-"+strings.TrimSpace(d.UUID)+".xml"), []byte(xml), 0o600))
			}
		}
		require.NoError(t, os.WriteFile(filepath.Join(hd, "list.txt"), []byte(list.String()), 0o600))
	}
	shim := "#!/bin/sh\nprintf 'local %s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$FAKE_VIRSH_DIR/calls.log\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(opsFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	for _, name := range []string{"sudo", "rm"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755)) //nolint:gosec // test shim must be executable
	}
	t.Setenv("FAKE_VIRSH_DIR", dir)
	t.Setenv("FAKE_DISK_PATH", opsDiskPath)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &routingFixture{t: t, dir: dir}
}

// script writes a per-host behavior file (fail-<cmd>, state, uuid).
func (f *routingFixture) script(host, name, content string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, host, name), []byte(content), 0o600))
}

// callSeqScenario is one single-host Power or Reconfigure call whose command
// sequence is pinned.
type callSeqScenario struct {
	name  string
	setup func(fx *routingFixture)
	run   func(ctx context.Context, p *Provider) error
}

// callSeqResult is what a scenario produced: the logged commands and the
// returned error ("" on success).
type callSeqResult struct {
	Calls []string `json:"calls"`
	Err   string   `json:"err"`
}

// reconfigureTo is a desired state for Reconfigure: cpu/memMiB/diskGiB of 0
// leave that dimension alone.
func reconfigureTo(cpu, memMiB, diskGiB int32) contracts.CreateRequest {
	req := contracts.CreateRequest{Name: opsDomainName, Class: contracts.VMClass{CPU: cpu, MemoryMiB: memMiB}}
	if diskGiB > 0 {
		req.Class.DiskDefaults = &contracts.DiskDefaults{SizeGiB: diskGiB}
	}
	return req
}

// singleHostCallSeqScenarios covers every Power op (success and fallback /
// failure variants) and every Reconfigure branch: offline CPU/memory/disk,
// online CPU/memory (success and beyond-headroom failure), online disk grow
// with and without the guest agent, a failed blockresize, a no-op and a
// domstate failure.
func singleHostCallSeqScenarios() []callSeqScenario {
	vm := contracts.VMRef{ID: opsDomainName}
	power := func(op contracts.PowerOp) func(context.Context, *Provider) error {
		return func(ctx context.Context, p *Provider) error {
			_, err := p.Power(ctx, vm, op)
			return err
		}
	}
	reconfigure := func(desired contracts.CreateRequest) func(context.Context, *Provider) error {
		return func(ctx context.Context, p *Provider) error {
			_, err := p.Reconfigure(ctx, vm, desired)
			return err
		}
	}
	running := func(fx *routingFixture) { fx.script("single", "state", "running\n") }
	return []callSeqScenario{
		{name: "power-on", run: power(contracts.PowerOpOn)},
		{name: "power-on-start-fails", setup: func(fx *routingFixture) { fx.script("single", "fail-start", "") }, run: power(contracts.PowerOpOn)},
		{name: "power-on-define-fails", setup: func(fx *routingFixture) {
			require.NoError(fx.t, os.MkdirAll(filepath.Join(fx.dir, "local"), 0o700))
			fx.script("local", "fail-define", "")
		}, run: power(contracts.PowerOpOn)},
		{name: "power-off", run: power(contracts.PowerOpOff)},
		{name: "power-off-fails", setup: func(fx *routingFixture) { fx.script("single", "fail-destroy", "") }, run: power(contracts.PowerOpOff)},
		{name: "power-reboot", run: power(contracts.PowerOpReboot)},
		{name: "power-reboot-stop-fails", setup: func(fx *routingFixture) { fx.script("single", "fail-destroy", "") }, run: power(contracts.PowerOpReboot)},
		{name: "power-shutdown-graceful", run: power(contracts.PowerOpShutdownGraceful)},
		{name: "power-shutdown-graceful-fallback", setup: func(fx *routingFixture) { fx.script("single", "fail-shutdown", "") }, run: power(contracts.PowerOpShutdownGraceful)},
		{name: "power-invalid-op", run: power(contracts.PowerOp("Hibernate"))},
		{name: "reconfigure-offline-cpu-mem-disk", run: reconfigure(reconfigureTo(4, 4096, 20))},
		{name: "reconfigure-offline-failures", setup: func(fx *routingFixture) {
			fx.script("single", "fail-setvcpus", "")
			fx.script("single", "fail-setmem", "")
			fx.script("single", "fail-vol-resize", "")
		}, run: reconfigure(reconfigureTo(4, 4096, 20))},
		{name: "reconfigure-online-cpu-mem", setup: running, run: reconfigure(reconfigureTo(4, 4096, 0))},
		{name: "reconfigure-online-beyond-headroom", setup: func(fx *routingFixture) {
			running(fx)
			fx.script("single", "fail-setvcpus", "")
			fx.script("single", "fail-setmem", "")
		}, run: reconfigure(reconfigureTo(64, 65536, 0))},
		{name: "reconfigure-online-disk-grow-guest-agent", setup: running, run: reconfigure(reconfigureTo(2, 2048, 20))},
		{name: "reconfigure-online-disk-grow-no-agent", setup: func(fx *routingFixture) {
			running(fx)
			fx.script("single", "fail-qemu-agent-command", "")
		}, run: reconfigure(reconfigureTo(0, 0, 20))},
		{name: "reconfigure-online-disk-already-large", setup: running, run: reconfigure(reconfigureTo(0, 0, 10))},
		{name: "reconfigure-online-blockresize-fails", setup: func(fx *routingFixture) {
			running(fx)
			fx.script("single", "fail-blockresize", "")
		}, run: reconfigure(reconfigureTo(0, 0, 20))},
		{name: "reconfigure-no-change", setup: running, run: reconfigure(reconfigureTo(2, 2048, 0))},
		{name: "reconfigure-domstate-fails", setup: func(fx *routingFixture) { fx.script("single", "fail-domstate", "") }, run: reconfigure(reconfigureTo(4, 0, 0))},
	}
}

// TestSingleHost_PowerAndReconfigure_CallSequencesUnchanged is the single-host
// equivalence proof for slice 2: every scenario's virsh/host command sequence
// and returned error equal what origin/main produced before the refactor.
func TestSingleHost_PowerAndReconfigure_CallSequencesUnchanged(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove("/tmp/" + opsDomainName + "-sync.xml") })

	got := map[string]callSeqResult{}
	for _, sc := range singleHostCallSeqScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			fx := newOpsFixture(t, map[string]map[string]string{
				"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
			})
			if sc.setup != nil {
				sc.setup(fx)
			}
			p := &Provider{virshProvider: localHostVP("single")}
			require.False(t, p.clustered())
			res := callSeqResult{}
			if err := sc.run(context.Background(), p); err != nil {
				res.Err = err.Error()
			}
			res.Calls = fx.calls()
			got[sc.name] = res
		})
	}

	if os.Getenv(callSeqUpdateEnv) == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(callSeqGoldenFile), 0o750))
		require.NoError(t, os.WriteFile(callSeqGoldenFile, append(b, '\n'), 0o600))
		t.Logf("rewrote %s", callSeqGoldenFile)
		return
	}

	raw, err := os.ReadFile(callSeqGoldenFile)
	require.NoError(t, err)
	want := map[string]callSeqResult{}
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Len(t, got, len(want), "every golden scenario is still exercised")
	for name, w := range want {
		g, ok := got[name]
		require.True(t, ok, "scenario %s not run", name)
		assert.Equal(t, w.Calls, g.Calls, "%s: the single-host command sequence changed", name)
		assert.Equal(t, w.Err, g.Err, "%s: the single-host error changed", name)
	}
}

// TestSingleHost_PowerAndReconfigure_IgnoreHostAndOwner pins that a single-host
// provider ignores vm.HostID and vm.Owner on Power and Reconfigure (D9): no
// registry is consulted, the owner is not checked (an unstamped legacy domain
// stays manageable even for a foreign owner), and the domain is addressed by
// its name — the same sequence as a bare-id call.
func TestSingleHost_PowerAndReconfigure_IgnoreHostAndOwner(t *testing.T) {
	t.Cleanup(func() { _ = os.Remove("/tmp/" + opsDomainName + "-sync.xml") })
	run := func(vm contracts.VMRef) []string {
		fx := newOpsFixture(t, map[string]map[string]string{
			"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
		})
		fx.script("single", "state", "running\n")
		p := &Provider{virshProvider: localHostVP("single")}
		_, err := p.Power(context.Background(), vm, contracts.PowerOpOn)
		require.NoError(t, err)
		_, err = p.Reconfigure(context.Background(), vm, reconfigureTo(4, 4096, 0))
		require.NoError(t, err)
		return fx.calls()
	}
	bare := run(contracts.VMRef{ID: opsDomainName})
	routedLooking := run(contracts.VMRef{ID: opsDomainName, HostID: "host-ignored", Owner: ownerTeamB})
	assert.Equal(t, bare, routedLooking)
	for _, c := range routedLooking {
		assert.NotContains(t, c, routingDomainUUID, "single-host never addresses a domain by UUID")
	}
}
