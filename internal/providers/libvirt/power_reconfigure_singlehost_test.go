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
// virsh / host command sequence pinned in
// testdata/single_host_power_reconfigure.golden.json. The Power sequences were
// captured against origin/main at 5c4a335 (the commit before the
// Power/Reconfigure cores were refactored) and deliberately regenerated once
// since (delete-safety fix, review item 5): a successful start (power-on,
// power-on-define-fails, power-reboot, power-reboot-stop-fails) is followed by
// the read-only linked-clone dependents count — `dumpxml` of the domain and
// `list --all --uuid`. The Reconfigure sequences, errors and restart-required
// answers were deliberately re-captured (VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN=1)
// when Reconfigure stopped reporting success for changes it did not apply (the
// honest-result fix, see reconfigure.go). A change to any single-host sequence
// — which would restart the ADR-0008 D5 soak window — fails here.

// callSeqGoldenFile is the golden single-host call sequences (see the file
// comment for when each part was captured).
const callSeqGoldenFile = "testdata/single_host_power_reconfigure.golden.json"

// callSeqUpdateEnv, set to "1", rewrites callSeqGoldenFile from the current
// code instead of comparing against it.
const callSeqUpdateEnv = "VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN"

// opsDomainName is the domain every call-sequence scenario acts on. It is
// distinctive because single-host Power writes /tmp/<name>-sync.xml on a local
// connection (removed again by the test).
const opsDomainName = "vrcallseq-web"

// opsDiskPath is the primary disk the fake domblklist reports.
var opsDiskPath string // inside fixtureImagesDir (set by TestMain)

// opsFakeVirsh is a scriptable fake `virsh`. It routes on the -c URI (the host
// tag is the URI path, "local" without -c), logs every call as
// "<host> <args>", and answers the subcommands Power and Reconfigure use. Per
// host, a file named fail-<subcommand> makes that subcommand fail
// (fail-<subcommand>-live / -config / -maximum only its call with that flag,
// fail-inactive the `dumpxml --inactive` read), "state"
// holds the domstate answer (default "shut off"), "id" / "vcpus" / "maxmem" /
// "usedmem" the dominfo Id, CPU(s), Max memory and Used memory (KiB; default
// "-", 2, 2097152, 2097152), "cfg-vcpus" / "cfg-maxvcpus" / "cfg-mem" /
// "cfg-maxmem" the persistent definition `dumpxml --inactive` reports (same
// defaults), "capacity" the domblkinfo Capacity in bytes (default 10 GiB),
// "disktype" / "disksource"
// override the primary disk `domblklist --details` reports (default: a file
// disk at $FAKE_DISK_PATH), "dead" makes every call die without an exit status
// (the host dropped the connection), "nolibvirtd" makes every call fail as
// virsh does when the host's libvirtd is down, and dom-<name>.xml marks a
// domain (by name or UUID) as present; a domain command on anything else fails
// like libvirt's "failed to get domain".
const opsFakeVirsh = `#!/bin/sh
host=local
if [ "$1" = "-c" ]; then host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
d="$FAKE_VIRSH_DIR/$host"
fail() { if [ -f "$d/fail-$1" ]; then echo "error: scripted failure of $1" >&2; exit 1; fi; }
nodom() { echo "error: failed to get domain '$1'" >&2; exit 1; }
if [ -f "$d/dead" ]; then kill -9 $$; fi
if [ -f "$d/nolibvirtd" ]; then echo "error: failed to connect to the hypervisor" >&2; exit 1; fi
if [ "$1" = "dumpxml" ] && [ "$3" = "--inactive" ]; then
  fail inactive
  [ -f "$d/dom-$2.xml" ] || nodom "$2"
  vc=2; mvc=2; mem=2097152; mmem=2097152
  if [ -f "$d/cfg-vcpus" ]; then vc=$(cat "$d/cfg-vcpus"); fi
  if [ -f "$d/cfg-maxvcpus" ]; then mvc=$(cat "$d/cfg-maxvcpus"); fi
  if [ -f "$d/cfg-mem" ]; then mem=$(cat "$d/cfg-mem"); fi
  if [ -f "$d/cfg-maxmem" ]; then mmem=$(cat "$d/cfg-maxmem"); fi
  printf "<domain type='kvm'>\n  <name>%s</name>\n  <memory unit='KiB'>%s</memory>\n  <currentMemory unit='KiB'>%s</currentMemory>\n  <vcpu placement='static' current='%s'>%s</vcpu>\n</domain>\n" "$2" "$mmem" "$mem" "$vc" "$mvc"
  exit 0
fi
case "$1" in
  list) if [ "$3" = "--uuid" ]; then cat "$d/uuids.txt" 2>/dev/null; else cat "$d/list.txt"; fi ;;
  pool-dumpxml) printf "<pool type='dir'><name>default</name><target><path>%s</path></target></pool>\n" "$FAKE_POOL_DIR" ;;
  dumpxml) if [ -f "$d/dom-$2.xml" ]; then cat "$d/dom-$2.xml"; else nodom "$2"; fi ;;
  dominfo)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    uuid=11111111-2222-4333-8444-555555555555
    if [ -f "$d/uuid" ]; then uuid=$(cat "$d/uuid"); fi
    id=-; cpus=2; maxmem=2097152; usedmem=2097152
    if [ -f "$d/id" ]; then id=$(cat "$d/id"); fi
    if [ -f "$d/vcpus" ]; then cpus=$(cat "$d/vcpus"); fi
    if [ -f "$d/maxmem" ]; then maxmem=$(cat "$d/maxmem"); fi
    if [ -f "$d/usedmem" ]; then usedmem=$(cat "$d/usedmem"); fi
    printf 'Id:             %s\nName:           %s\nUUID:           %s\nState:          shut off\nCPU(s):         %s\nMax memory:     %s KiB\nUsed memory:    %s KiB\n' "$id" "$2" "$uuid" "$cpus" "$maxmem" "$usedmem" ;;
  domstate)
    fail domstate
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    if [ -f "$d/state" ]; then cat "$d/state"; else echo "shut off"; fi ;;
  start|destroy|shutdown|setvcpus|setmem|setmaxmem|blockresize|undefine)
    fail "$1"
    case "$*" in *--live*) fail "$1-live" ;; esac
    case "$*" in *--config*) fail "$1-config" ;; esac
    case "$*" in *--maximum*) fail "$1-maximum" ;; esac
    [ -f "$d/dom-$2.xml" ] || nodom "$2" ;;
  vol-resize|define) fail "$1"; exit 0 ;;
  domblklist)
    fail domblklist
    case "$*" in
      *--details*)
        t=file; if [ -f "$d/disktype" ]; then t=$(cat "$d/disktype"); fi
        src="$FAKE_DISK_PATH"; if [ -f "$d/disksource" ]; then src=$(cat "$d/disksource"); fi
        printf ' Type   Device   Target   Source\n------------------------------------------------\n %s   disk     vda      %s\n file   cdrom    hda      %s/web-cidata.iso\n' "$t" "$src" "$FAKE_POOL_DIR" ;;
      *) printf ' Target   Source\n------------------------------------------------\n vda      %s\n' "$FAKE_DISK_PATH" ;;
    esac ;;
  domblkinfo)
    fail domblkinfo
    capacity=10737418240
    if [ -f "$d/capacity" ]; then capacity=$(cat "$d/capacity"); fi
    printf 'Capacity:       %s\nAllocation:     1073741824\nPhysical:       1073741824\n' "$capacity" ;;
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
		writeFixtureUUIDs(t, hd, domains)
	}
	shim := "#!/bin/sh\nprintf 'local %s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$FAKE_VIRSH_DIR/calls.log\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(opsFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	for _, name := range []string{"sudo", "rm"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755)) //nolint:gosec // test shim must be executable
	}
	installQemuImgShim(t, bin)
	t.Setenv("FAKE_VIRSH_DIR", dir)
	useFixtureImages(t)
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
// sequence is pinned. run reports whether a Reconfigure left a change pending
// a restart (always false for Power).
type callSeqScenario struct {
	name  string
	setup func(fx *routingFixture)
	run   func(ctx context.Context, p *Provider) (restartRequired bool, err error)
}

// callSeqResult is what a scenario produced: the logged commands, the
// returned error ("" on success) and, for a Reconfigure, whether it reported a
// restart required (omitted when false, so the Power entries are unchanged).
type callSeqResult struct {
	Calls           []string `json:"calls"`
	Err             string   `json:"err"`
	RestartRequired bool     `json:"restartRequired,omitempty"`
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
// failure variants) and every Reconfigure branch: offline CPU/memory/disk
// (applied, a failed persistent change, a failed setmaxmem, a disk already
// large enough), online CPU/memory (applied live within the hot-add ceilings,
// beyond them and shrinks — restart required), online disk grow with and
// without the guest agent, a failed blockresize, a no-op, a domstate failure,
// and the refusal of a paused or PM-suspended domain.
func singleHostCallSeqScenarios() []callSeqScenario {
	vm := contracts.VMRef{ID: opsDomainName}
	power := func(op contracts.PowerOp) func(context.Context, *Provider) (bool, error) {
		return func(ctx context.Context, p *Provider) (bool, error) {
			_, err := p.Power(ctx, vm, op)
			return false, err
		}
	}
	reconfigure := func(desired contracts.CreateRequest) func(context.Context, *Provider) (bool, error) {
		return func(ctx context.Context, p *Provider) (bool, error) {
			res, err := p.Reconfigure(ctx, vm, desired)
			return res.RestartRequired, err
		}
	}
	running := func(fx *routingFixture) {
		fx.script("single", "state", "running\n")
		fx.script("single", "id", "7")
	}
	// hotAdd scripts a running VM created with CPU and memory hot-add: an 8
	// vCPU and 8 GiB ceiling above its 2 vCPUs and 2 GiB.
	hotAdd := func(fx *routingFixture) {
		running(fx)
		fx.script("single", "cfg-maxvcpus", "8")
		fx.script("single", "maxmem", "8388608")
		fx.script("single", "cfg-maxmem", "8388608")
	}
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
		{name: "reconfigure-offline-setmaxmem-fails", setup: func(fx *routingFixture) {
			fx.script("single", "fail-setmaxmem", "")
		}, run: reconfigure(reconfigureTo(0, 4096, 0))},
		{name: "reconfigure-offline-disk-already-large", setup: func(fx *routingFixture) {
			fx.script("single", "capacity", "21474836480")
		}, run: reconfigure(reconfigureTo(0, 0, 20))},
		{name: "reconfigure-online-cpu-mem", setup: hotAdd, run: reconfigure(reconfigureTo(4, 4096, 0))},
		{name: "reconfigure-online-beyond-headroom", setup: func(fx *routingFixture) {
			running(fx)
			fx.script("single", "fail-setvcpus-live", "")
		}, run: reconfigure(reconfigureTo(64, 65536, 0))},
		{name: "reconfigure-online-shrink", setup: func(fx *routingFixture) {
			running(fx)
			for f, v := range map[string]string{"vcpus": "4", "cfg-vcpus": "4", "cfg-maxvcpus": "4",
				"maxmem": "4194304", "usedmem": "4194304", "cfg-mem": "4194304", "cfg-maxmem": "4194304"} {
				fx.script("single", f, v)
			}
			fx.script("single", "fail-setvcpus-live", "")
		}, run: reconfigure(reconfigureTo(2, 2048, 0))},
		{name: "reconfigure-paused-refused", setup: func(fx *routingFixture) {
			fx.script("single", "state", "paused\n")
			fx.script("single", "id", "7")
		}, run: reconfigure(reconfigureTo(1, 1024, 0))},
		{name: "reconfigure-pmsuspended-refused", setup: func(fx *routingFixture) {
			fx.script("single", "state", "pmsuspended\n")
			fx.script("single", "id", "7")
		}, run: reconfigure(reconfigureTo(1, 1024, 0))},
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
			restart, err := sc.run(context.Background(), p)
			if err != nil {
				res.Err = err.Error()
			}
			res.RestartRequired = restart
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
		assert.Equal(t, w.RestartRequired, g.RestartRequired, "%s: the single-host restart-required answer changed", name)
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
