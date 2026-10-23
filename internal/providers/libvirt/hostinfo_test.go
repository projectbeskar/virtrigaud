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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// --- Real virsh output fixtures (shapes captured from actual hosts) ----------

// nodeInfoFixture is real `virsh nodeinfo` output: several labels share the
// "CPU" prefix, so the parser must match "CPU(s)" exactly. Memory is in KiB.
const nodeInfoFixture = `CPU model:           x86_64
CPU(s):              8
CPU frequency:       3600 MHz
CPU socket(s):       1
Core(s) per socket:  4
Thread(s) per core:  2
NUMA cell(s):        1
Memory size:         16777216 KiB
`

// capabilitiesFixture is a trimmed-but-valid `virsh capabilities` document: the
// host CPU baseline model and its feature flags under <host><cpu>.
const capabilitiesFixture = `<capabilities>
  <host>
    <uuid>deadbeef-0000-0000-0000-000000000000</uuid>
    <cpu>
      <arch>x86_64</arch>
      <model>Skylake-Client-IBRS</model>
      <vendor>Intel</vendor>
      <topology sockets='1' dies='1' cores='4' threads='2'/>
      <feature name='ss'/>
      <feature name='vmx'/>
      <feature name='hypervisor'/>
    </cpu>
  </host>
  <guest>
    <os_type>hvm</os_type>
    <arch name='x86_64'>
      <wordsize>64</wordsize>
      <emulator>/usr/bin/qemu-system-x86_64</emulator>
      <machine canonical='pc-i440fx-7.2' maxCpus='255'>pc</machine>
      <machine maxCpus='288'>pc-q35-7.2</machine>
    </arch>
  </guest>
</capabilities>
`

// domCapabilitiesFixture is a trimmed-but-valid `virsh domcapabilities` document:
// the default machine type and the emulator binary path.
const domCapabilitiesFixture = `<domainCapabilities>
  <path>/usr/bin/qemu-system-x86_64</path>
  <domain>kvm</domain>
  <machine>pc-i440fx-7.2</machine>
  <arch>x86_64</arch>
  <vcpu max='255'/>
  <iothreads supported='yes'/>
</domainCapabilities>
`

// poolListFixture is real `virsh pool-list --all` output: two active pools and
// one inactive one, plus the header and dashed separator the parser skips.
const poolListFixture = ` Name      State      Autostart
--------------------------------------
 default   active     yes
 data      active     yes
 iso       inactive   no
`

// poolDumpXMLDirFixture is `virsh pool-dumpxml default` captured from a lab host
// (2026-10-22) whose dir pool sits on an NFS mount. Its <available> is
// libvirt's CACHED value (as of the last pool-refresh, and f_bfree-based) —
// 596772913152 — which the inventory must NOT report (R31).
const poolDumpXMLDirFixture = `<pool type='dir'>
  <name>default</name>
  <uuid>dbcbe54c-dd10-4aed-a333-c877dbe51ec6</uuid>
  <capacity unit='bytes'>1829413847040</capacity>
  <allocation unit='bytes'>1232640933888</allocation>
  <available unit='bytes'>596772913152</available>
  <source>
  </source>
  <target>
    <path>/var/lib/libvirt/images</path>
    <permissions>
      <mode>0755</mode>
      <owner>1000</owner>
      <group>1000</group>
    </permissions>
  </target>
</pool>
`

// poolDirFixture is the target directory of poolDumpXMLDirFixture.
const poolDirFixture = DefaultImageDir

// labPoolDumpXML is poolDumpXMLDirFixture with its pool at
// defaultPoolRequiredDir — the directory the inventory requires of the
// `default` pool, which this test binary roots in a fixture directory
// (TestMain) rather than the real /var/lib/libvirt/images.
func labPoolDumpXML() string {
	return strings.Replace(poolDumpXMLDirFixture, "<path>"+poolDirFixture+"</path>",
		"<path>"+defaultPoolRequiredDir+"</path>", 1)
}

// statfsKey is the fakeHostConn script key of the free-space measurement of
// the pool directory of labPoolDumpXML (a host command: "!"-prefixed, like
// runHost).
func statfsKey() string {
	return "! timeout --kill-after=1s 10s stat -f -c %a %S -- " + filepath.Clean(defaultPoolRequiredDir)
}

// statfsFixture is `stat -f -c "%a %S" /var/lib/libvirt/images` on the same lab
// host at the same time: 507261 blocks of 1 MiB available to non-root =
// 531901710336 bytes, exactly df's "Available" column.
const (
	statfsFixture      = "507261 1048576\n"
	statfsFixtureBytes = int64(531901710336)
)

// fullHostScript is the scripted command set for a fully-answering host.
func fullHostScript() map[string]fakeVirshResult {
	return map[string]fakeVirshResult{
		"nodeinfo":             {stdout: nodeInfoFixture},
		"capabilities":         {stdout: capabilitiesFixture},
		"domcapabilities":      {stdout: domCapabilitiesFixture},
		"pool-list --all":      {stdout: poolListFixture},
		"pool-dumpxml default": {stdout: labPoolDumpXML()},
		statfsKey():            {stdout: statfsFixture},
	}
}

// --- fakeHostConn: a hostconn.Conn whose Virsh returns scripted fixtures ------

type fakeVirshResult struct {
	stdout string
	err    error
}

// fakeHostConn is a hostconn.Conn that answers Virsh and RunHost from a
// per-command script (key = the space-joined args; a RunHost key is prefixed
// with "! ", as runHost spells host commands), records the commands it was
// asked to run (so a test can assert what was and was NOT queried), and counts
// Close calls (so a test can prove a lease was released and the underlying conn
// closed). runHost, when set, answers RunHost instead of the script.
type fakeHostConn struct {
	id      hostconn.HostID
	script  map[string]fakeVirshResult
	runHost func(ctx context.Context, argv []string) (*hostconn.Result, error)

	mu     sync.Mutex
	calls  []string
	closeN int
}

func (f *fakeHostConn) HostID() hostconn.HostID { return f.id }

func (f *fakeHostConn) Virsh(ctx context.Context, args ...string) (*hostconn.Result, error) {
	cmd := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	// Like the real transport, a done context fails the command.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, ok := f.script[cmd]
	if !ok {
		return nil, fmt.Errorf("fakeHostConn: no script for %q", cmd)
	}
	if r.err != nil {
		return nil, r.err
	}
	return &hostconn.Result{Command: cmd, Stdout: r.stdout}, nil
}

func (f *fakeHostConn) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	cmd := "! " + strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	if f.runHost != nil {
		return f.runHost(ctx, argv)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, ok := f.script[cmd]
	if !ok {
		return nil, fmt.Errorf("fakeHostConn: no script for %q", cmd)
	}
	if r.err != nil {
		return nil, r.err
	}
	return &hostconn.Result{Command: cmd, Stdout: r.stdout}, nil
}

func (f *fakeHostConn) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	return nil, errors.New("fakeHostConn: Stream not used")
}

func (f *fakeHostConn) Libvirt(_ context.Context) (*golibvirt.Libvirt, error) {
	return nil, errors.New("fakeHostConn: Libvirt not used")
}

func (f *fakeHostConn) Close() error {
	f.mu.Lock()
	f.closeN++
	f.mu.Unlock()
	return nil
}

func (f *fakeHostConn) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHostConn) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeN
}

// --- Parse-function unit tests ------------------------------------------------

func TestParseNodeInfoCPUMem(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantCPU int32
		wantMem int64
		wantErr bool
	}{
		{
			name:    "real nodeinfo, KiB->MiB",
			in:      nodeInfoFixture,
			wantCPU: 8,
			wantMem: 16384, // 16777216 KiB / 1024
		},
		{
			name:    "KiB->MiB exact",
			in:      "CPU(s):              2\nMemory size:         2097152 KiB\n",
			wantCPU: 2,
			wantMem: 2048, // 2097152 / 1024
		},
		{
			name:    "missing CPU(s)",
			in:      "CPU model:           x86_64\nMemory size:         1048576 KiB\n",
			wantErr: true,
		},
		{
			name:    "missing Memory size",
			in:      "CPU(s):              4\n",
			wantErr: true,
		},
		{
			name:    "wrong memory unit is rejected, not mis-scaled",
			in:      "CPU(s):              4\nMemory size:         64 MiB\n",
			wantErr: true,
		},
		{
			name:    "CPU model line does not satisfy CPU(s)",
			in:      "CPU model:           x86_64\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mem, err := parseNodeInfoCPUMem(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCPU, cpu)
			assert.Equal(t, tt.wantMem, mem)
		})
	}
}

func TestParseHostCPUFromCaps(t *testing.T) {
	t.Run("model and features from real capabilities", func(t *testing.T) {
		model, features, err := parseHostCPUFromCaps(capabilitiesFixture)
		require.NoError(t, err)
		assert.Equal(t, "Skylake-Client-IBRS", model)
		assert.Equal(t, []string{"ss", "vmx", "hypervisor"}, features)
	})
	t.Run("no host cpu element is empty, not an error", func(t *testing.T) {
		model, features, err := parseHostCPUFromCaps("<capabilities><host></host></capabilities>")
		require.NoError(t, err)
		assert.Empty(t, model)
		assert.Empty(t, features)
	})
	t.Run("invalid XML errors", func(t *testing.T) {
		_, _, err := parseHostCPUFromCaps("<capabilities><host><cpu>")
		require.Error(t, err)
	})
}

func TestParseDomCapsMachineEmulator(t *testing.T) {
	t.Run("machine and emulator from real domcapabilities", func(t *testing.T) {
		machines, emulator, err := parseDomCapsMachineEmulator(domCapabilitiesFixture)
		require.NoError(t, err)
		assert.Equal(t, []string{"pc-i440fx-7.2"}, machines)
		assert.Equal(t, "/usr/bin/qemu-system-x86_64", emulator)
	})
	t.Run("absent machine yields nil slice", func(t *testing.T) {
		machines, emulator, err := parseDomCapsMachineEmulator(
			"<domainCapabilities><path>/usr/bin/qemu-kvm</path></domainCapabilities>")
		require.NoError(t, err)
		assert.Nil(t, machines)
		assert.Equal(t, "/usr/bin/qemu-kvm", emulator)
	})
	t.Run("invalid XML errors", func(t *testing.T) {
		_, _, err := parseDomCapsMachineEmulator("<domainCapabilities>")
		require.Error(t, err)
	})
}

func TestParseActivePoolNames(t *testing.T) {
	assert.Equal(t, []string{"default", "data"}, parseActivePoolNames(poolListFixture))
	assert.Nil(t, parseActivePoolNames(" Name   State   Autostart\n-------------------\n"))
	assert.Nil(t, parseActivePoolNames(""))
}

func TestParseStatfsAvailBytes(t *testing.T) {
	ok := []struct {
		name, in string
		want     int64
	}{
		{"lab nfs pool", statfsFixture, statfsFixtureBytes},
		{"4k blocks", "2621440 4096", 10737418240},
		{"full filesystem", "0 4096\n", 0},
		{"surrounding whitespace", "  \t12 512 \n\n", 6144},
		{"max without overflow", fmt.Sprintf("%d 1", int64(math.MaxInt64)), math.MaxInt64},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatfsAvailBytes(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// Every odd output is an error: the caller then reports 0 (fail closed),
	// never a misread number.
	bad := map[string]string{
		"empty":                "",
		"whitespace only":      " \n",
		"one field":            "507261\n",
		"three fields":         "507261 1048576 579243\n",
		"a second line":        "507261 1048576\n1 1\n",
		"non-numeric":          "? 4096",
		"stat error text":      "stat: cannot read file system information for '/x': No such file or directory",
		"negative blocks":      "-1 4096",
		"zero block size":      "100 0",
		"negative block size":  "100 -4096",
		"hex":                  "0x10 4096",
		"float":                "1.5 4096",
		"overflow":             fmt.Sprintf("%d 2", int64(math.MaxInt64)),
		"blocks beyond int64":  "99999999999999999999 4096",
		"block size beyond 64": "1 99999999999999999999",
	}
	for name, in := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			got, err := parseStatfsAvailBytes(in)
			require.Error(t, err)
			assert.Zero(t, got)
		})
	}
}

func TestParseFilesystemPoolDir(t *testing.T) {
	poolXML := func(typ, target string) string {
		return fmt.Sprintf("<pool type='%s'><name>default</name>%s</pool>", typ, target)
	}
	pathTarget := func(p string) string { return "<target><path>" + p + "</path></target>" }

	ok := []struct {
		name, in, wantType, wantDir string
	}{
		{"lab dir pool", poolDumpXMLDirFixture, "dir", poolDirFixture},
		{"netfs pool", poolXML("netfs", pathTarget("/var/lib/libvirt/images")), "netfs", "/var/lib/libvirt/images"},
		{"fs pool", poolXML("fs", pathTarget("/srv/vm")), "fs", "/srv/vm"},
		{"surrounding space trimmed, path kept as defined", poolXML("dir", pathTarget(" /var/lib/libvirt/images/ ")), "dir", "/var/lib/libvirt/images/"},
		{"unclean path kept (cleaned and resolved by isRequiredPoolDirOn)", poolXML("dir", pathTarget("/var/lib/../lib/libvirt/images")), "dir", "/var/lib/../lib/libvirt/images"},
		{"root directory", poolXML("dir", pathTarget("/")), "dir", "/"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			typ, dir, err := parseFilesystemPoolDir(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.wantType, typ)
			assert.Equal(t, tc.wantDir, dir)
		})
	}

	bad := []struct {
		name, in, wantType string
	}{
		{"logical (LVM) pool", poolXML("logical", pathTarget("/dev/vg0")), "logical"},
		{"rbd pool", poolXML("rbd", ""), "rbd"},
		{"iscsi pool", poolXML("iscsi", pathTarget("/dev/disk/by-path")), "iscsi"},
		{"zfs pool", poolXML("zfs", pathTarget("/dev/zvol/tank")), "zfs"},
		{"no type", "<pool><name>default</name>" + pathTarget("/x") + "</pool>", ""},
		{"no target", poolXML("dir", ""), "dir"},
		{"empty path", poolXML("dir", pathTarget("")), "dir"},
		{"relative path", poolXML("dir", pathTarget("images")), "dir"},
		{"not XML", "error: failed to get pool 'default'", ""},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			typ, dir, err := parseFilesystemPoolDir(tc.in)
			require.Error(t, err)
			assert.Empty(t, dir)
			assert.Equal(t, tc.wantType, typ)
		})
	}
}

// --- collectHostInfo: assembly + health gating + best-effort ------------------

func TestCollectHostInfo_FullRender(t *testing.T) {
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	labels := map[string]string{"zone": "rack-1"}

	got := collectHostInfo(context.Background(), conn, "host-a",
		"qemu+ssh://virt@host-a/system", labels, slog.Default(), nil)

	assert.Equal(t, "host-a", got.ID)
	assert.Equal(t, "qemu+ssh://virt@host-a/system", got.Address)
	assert.Equal(t, labels, got.Labels)
	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Equal(t, int32(8), got.AllocatableCPU)
	assert.Equal(t, int64(16384), got.AllocatableMemMiB)
	assert.Equal(t, "Skylake-Client-IBRS", got.CPUModel)
	assert.Equal(t, []string{"ss", "vmx", "hypervisor"}, got.CPUFeatures)
	assert.Equal(t, []string{"pc-i440fx-7.2"}, got.MachineTypes)
	assert.Equal(t, "/usr/bin/qemu-system-x86_64", got.EmulatorVersion)
	// The default pool's directory, measured fresh with statvfs (f_bavail *
	// f_frsize) — not libvirt's cached <available> (596772913152), and no other
	// pool (data, the inactive iso) is counted.
	assert.Equal(t, statfsFixtureBytes, got.AllocatableStorage)
	assert.Equal(t, []string{
		"nodeinfo", "capabilities", "domcapabilities",
		"pool-list --all", "pool-dumpxml default", statfsKey(),
	}, conn.commands(), "no pool-refresh, no pool-info, and no other pool is read")
}

func TestCollectHostInfo_NodeinfoError_NotReady(t *testing.T) {
	conn := &fakeHostConn{
		id:     "host-down",
		script: map[string]fakeVirshResult{"nodeinfo": {err: errors.New("connection refused")}},
	}
	labels := map[string]string{"zone": "rack-9"}

	got := collectHostInfo(context.Background(), conn, "host-down", "qemu+ssh://virt@host-down/system", labels, slog.Default(), nil)

	assert.Equal(t, contracts.HostHealthNotReady, got.Health)
	// Metadata the registry knows is still reported for a down host.
	assert.Equal(t, "host-down", got.ID)
	assert.Equal(t, "qemu+ssh://virt@host-down/system", got.Address)
	assert.Equal(t, labels, got.Labels)
	// Nothing else was populated.
	assert.Zero(t, got.AllocatableCPU)
	assert.Zero(t, got.AllocatableMemMiB)
	assert.Zero(t, got.AllocatableStorage)
	assert.Empty(t, got.CPUModel)
	// The richer queries must be skipped once the core query fails.
	assert.Equal(t, []string{"nodeinfo"}, conn.commands())
}

func TestCollectHostInfo_BestEffortDegrades(t *testing.T) {
	// nodeinfo succeeds (host is READY) but every richer query fails: the host
	// still renders with cpu/mem and empty richer fields — never NotReady.
	conn := &fakeHostConn{
		id: "host-partial",
		script: map[string]fakeVirshResult{
			"nodeinfo":        {stdout: nodeInfoFixture},
			"capabilities":    {err: errors.New("boom")},
			"domcapabilities": {err: errors.New("boom")},
			"pool-list --all": {err: errors.New("boom")},
		},
	}

	got := collectHostInfo(context.Background(), conn, "host-partial", "addr", nil, slog.Default(), nil)

	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Equal(t, int32(8), got.AllocatableCPU)
	assert.Equal(t, int64(16384), got.AllocatableMemMiB)
	assert.Empty(t, got.CPUModel)
	assert.Empty(t, got.CPUFeatures)
	assert.Empty(t, got.MachineTypes)
	assert.Empty(t, got.EmulatorVersion)
	assert.Zero(t, got.AllocatableStorage)
}

// TestCollectStorageBytes_FailsClosed pins R31's fail-closed contract: every
// way the default pool's free space cannot be measured reports 0 — which the
// scheduler's disk-space filter reads as "no room" — and leaves the host
// READY (storage never gates health). A non-filesystem pool, or one whose
// definition cannot be read, is never stat'ed.
func TestCollectStorageBytes_FailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		override  map[string]fakeVirshResult
		wantNoRun bool // the statvfs must not even be attempted
	}{
		{name: "stat fails", override: map[string]fakeVirshResult{
			statfsKey(): {err: errors.New("stat: cannot read file system information: Stale file handle")},
		}},
		{name: "stat prints an error text", override: map[string]fakeVirshResult{
			statfsKey(): {stdout: "stat: cannot read file system information for '/var/lib/libvirt/images'"},
		}},
		{name: "stat prints too many fields", override: map[string]fakeVirshResult{
			statfsKey(): {stdout: "507261 1048576 579243 1744665"},
		}},
		{name: "stat prints nothing", override: map[string]fakeVirshResult{statfsKey(): {stdout: ""}}},
		{name: "stat overflows", override: map[string]fakeVirshResult{
			statfsKey(): {stdout: "9223372036854775807 4096"},
		}},
		{name: "pool-dumpxml fails", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {err: errors.New("error: failed to get pool 'default'")},
		}},
		{name: "pool-dumpxml unparseable", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {stdout: "<pool type='dir'"},
		}},
		{name: "logical (LVM) pool", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {stdout: "<pool type='logical'><name>default</name><available unit='bytes'>99999999999</available>" +
				"<target><path>/dev/vg0</path></target></pool>"},
		}},
		{name: "rbd pool", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {stdout: "<pool type='rbd'><name>default</name><available unit='bytes'>99999999999</available></pool>"},
		}},
		{name: "relative target path", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {stdout: "<pool type='dir'><name>default</name><target><path>images</path></target></pool>"},
		}},
		// A default pool that is not at the required directory, even once
		// resolved on the host: creates and imports there are refused
		// (errDefaultPoolMisplaced, HOST_STORAGE_MISCONFIGURED), so it has no
		// allocatable space — even though its own directory has room.
		{name: "pool misplaced", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default":                      {stdout: poolDumpXMLAt("/srv/other-images")},
			realpathKey("/srv/other-images"):            {stdout: nulList("/srv/other-images", defaultPoolRequiredDir)},
			"! " + statProbeKeyFor("/srv/other-images"): {stdout: statfsFixture},
		}},
		{name: "pool at a parent of the required directory", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default": {stdout: poolDumpXMLAt(filepath.Dir(defaultPoolRequiredDir))},
			realpathKey(filepath.Dir(defaultPoolRequiredDir)): {
				stdout: nulList(filepath.Dir(defaultPoolRequiredDir), defaultPoolRequiredDir)},
		}},
		{name: "pool directory cannot be resolved", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-dumpxml default":      {stdout: poolDumpXMLAt("/srv/vm-link")},
			realpathKey("/srv/vm-link"): {err: errors.New("ssh: connection lost")},
		}},
		{name: "pool-list fails", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-list --all": {err: errors.New("boom")},
		}},
		{name: "default pool inactive", wantNoRun: true, override: map[string]fakeVirshResult{
			"pool-list --all": {stdout: " Name    State    Autostart\n-------\n default inactive no\n"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := fullHostScript()
			for k, v := range tc.override {
				script[k] = v
			}
			conn := &fakeHostConn{id: "host-x", script: script}

			got := collectHostInfo(context.Background(), conn, "host-x", "addr", nil, slog.Default(), nil)

			assert.Equal(t, contracts.HostHealthReady, got.Health, "storage never gates health")
			assert.Zero(t, got.AllocatableStorage, "an unmeasurable pool reports 0 (fail closed), never libvirt's cached value")
			if tc.wantNoRun {
				for _, cmd := range conn.commands() {
					assert.NotContains(t, cmd, "stat -f", "the free space is not measured")
				}
			}
		})
	}
}

// poolDumpXMLAt is the lab capture with its pool at dir.
func poolDumpXMLAt(dir string) string {
	return strings.Replace(poolDumpXMLDirFixture, "<path>"+poolDirFixture+"</path>", "<path>"+dir+"</path>", 1)
}

// realpathKey is the fakeHostConn script key of isRequiredPoolDirOn's
// resolution of the pool directory dir and the required directory.
func realpathKey(dir string) string {
	return "! realpath -m -z -- " + dir + " " + defaultPoolRequiredDir
}

// statProbeKeyFor is the inventory's statvfs probe of dir, without the "! ".
func statProbeKeyFor(dir string) string {
	return "timeout --kill-after=1s 10s stat -f -c %a %S -- " + dir
}

// nulList is realpath -z output for paths.
func nulList(paths ...string) string {
	return strings.Join(paths, "\x00") + "\x00"
}

// TestCollectStorageBytes_PoolDirDecidedLikeCreate pins #417 M1: the pool
// directory is accepted exactly when creates and imports accept it
// (isRequiredPoolDirOn) — lexically after cleaning (no host command), or once
// resolved on the host (a symbolic link) — and the required directory is
// what is measured.
func TestCollectStorageBytes_PoolDirDecidedLikeCreate(t *testing.T) {
	parent, base := filepath.Dir(defaultPoolRequiredDir), filepath.Base(defaultPoolRequiredDir)

	t.Run("unclean path naming the required directory", func(t *testing.T) {
		script := fullHostScript()
		script["pool-dumpxml default"] = fakeVirshResult{stdout: poolDumpXMLAt(parent + "/x/../" + base + "/")}
		conn := &fakeHostConn{id: "h", script: script}

		assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), conn, "h", slog.Default(), nil))
		assert.Equal(t, []string{"pool-list --all", "pool-dumpxml default", statfsKey()}, conn.commands(),
			"a lexical match needs no host command")
	})

	t.Run("symbolic link resolving to the required directory", func(t *testing.T) {
		script := fullHostScript()
		script["pool-dumpxml default"] = fakeVirshResult{stdout: poolDumpXMLAt("/srv/vm-link")}
		script[realpathKey("/srv/vm-link")] = fakeVirshResult{stdout: nulList(defaultPoolRequiredDir, defaultPoolRequiredDir)}
		conn := &fakeHostConn{id: "h", script: script}

		assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), conn, "h", slog.Default(), nil))
		assert.Equal(t, []string{"pool-list --all", "pool-dumpxml default", realpathKey("/srv/vm-link"), statfsKey()},
			conn.commands(), "resolved on the host, then the required directory is measured")
	})
}

// TestCollectStorageBytes_BudgetIsHostStorageProbeTimeout pins #417 M2: with
// no caller deadline the whole probe runs under exactly
// hostStorageProbeTimeout.
func TestCollectStorageBytes_BudgetIsHostStorageProbeTimeout(t *testing.T) {
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	var deadline time.Time
	var hasDeadline bool
	conn.runHost = func(ctx context.Context, _ []string) (*hostconn.Result, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, errors.New("stat: interrupted")
	}

	start := time.Now()
	got := collectStorageBytes(context.Background(), conn, "host-a", slog.Default(), nil)
	end := time.Now()

	assert.Zero(t, got)
	require.True(t, hasDeadline, "the statvfs must run under a deadline")
	assert.True(t, deadline.After(start.Add(hostStorageProbeTimeout-time.Second)),
		"deadline %v is not about hostStorageProbeTimeout after %v", deadline, start)
	assert.False(t, deadline.After(end.Add(hostStorageProbeTimeout)),
		"deadline %v is more than hostStorageProbeTimeout after %v", deadline, end)
}

// TestCollectStorageBytes_TimeoutFailsClosed: a statvfs that hangs — statfs on
// a dead hard-mounted NFS export blocks — is abandoned when the budget ends,
// reporting 0. Under a short caller deadline the budget is half of it (#417
// L4), so the probe really expires here.
func TestCollectStorageBytes_TimeoutFailsClosed(t *testing.T) {
	conn := &fakeHostConn{id: "host-hung", script: fullHostScript()}
	var deadline time.Time
	var hasDeadline bool
	conn.runHost = func(ctx context.Context, _ []string) (*hostconn.Result, error) {
		deadline, hasDeadline = ctx.Deadline()
		<-ctx.Done() // a hung statfs: returns only when the probe is cancelled
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	got := collectStorageBytes(ctx, conn, "host-hung", slog.Default(), nil)

	assert.Zero(t, got)
	require.True(t, hasDeadline, "the statvfs must run under a deadline")
	assert.True(t, deadline.Before(callerDeadline.Add(-90*time.Millisecond)),
		"the budget is about half of the caller's remaining 200ms (probe %v, caller %v)", deadline, callerDeadline)
	assert.NoError(t, ctx.Err(), "the probe gave up before the caller's own deadline")
}

// TestStorageProbeBudget pins #417 L4: hostStorageProbeTimeout, capped at half
// of the caller's remaining time.
func TestStorageProbeBudget(t *testing.T) {
	assert.Equal(t, hostStorageProbeTimeout, storageProbeBudget(context.Background()))

	long, cancelLong := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelLong()
	assert.Equal(t, hostStorageProbeTimeout, storageProbeBudget(long))

	// GetHostInfo's 30 s client timeout: the probe takes at most half.
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShort()
	b := storageProbeBudget(short)
	assert.LessOrEqual(t, b, 10*time.Second)
	assert.Greater(t, b, 9*time.Second)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	assert.LessOrEqual(t, storageProbeBudget(expired), time.Duration(0))
}

// TestCollectStorageBytes_CancelledCallerFailsClosed (#417 L5): a caller that
// gave up (cancelled, not expired) gets 0, not a measurement.
func TestCollectStorageBytes_CancelledCallerFailsClosed(t *testing.T) {
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Zero(t, collectStorageBytes(ctx, conn, "host-a", slog.Default(), nil))
	assert.NotContains(t, conn.commands(), statfsKey())
}

// TestCollectStorageBytes_InFlightGuard pins #417 M3: while one storage probe
// of a host has not returned, another probe of that host is not started
// (reports 0, runs no command) — other hosts are not held up — and each
// problem is logged at Warn when it starts for a host, at Debug while it
// lasts.
func TestCollectStorageBytes_InFlightGuard(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	probes := &hostStorageProbes{}

	entered := make(chan struct{})
	release := make(chan struct{})
	first := &fakeHostConn{id: "h", script: fullHostScript()}
	first.runHost = func(ctx context.Context, _ []string) (*hostconn.Result, error) {
		close(entered)
		select {
		case <-release:
			return &hostconn.Result{Stdout: statfsFixture}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan int64, 1)
	go func() { done <- collectStorageBytes(context.Background(), first, "h", logger, probes) }()
	<-entered

	for range 3 {
		second := &fakeHostConn{id: "h", script: fullHostScript()}
		assert.Zero(t, collectStorageBytes(context.Background(), second, "h", logger, probes))
		assert.Empty(t, second.commands(), "a host whose probe is in flight is not probed again")
	}
	other := &fakeHostConn{id: "h2", script: fullHostScript()}
	assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), other, "h2", logger, probes),
		"another host is not held up")

	close(release)
	assert.Equal(t, statfsFixtureBytes, <-done)

	third := &fakeHostConn{id: "h", script: fullHostScript()}
	assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), third, "h", logger, probes),
		"once the probe returned, the host is probed again")

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=WARN"), "one Warn for the in-flight skips:\n%s", out)
	assert.Equal(t, 3, strings.Count(out, "has not returned"), "one Warn, then Debug:\n%s", out)
	assert.Equal(t, 1, strings.Count(out, "measured again"), "the recovery is logged once:\n%s", out)
}

// TestCollectStorageBytes_WarnOncePerProblem (#417 L3): a problem that lasts
// logs one Warn, a different problem another, and a measurement in between
// re-arms it.
func TestCollectStorageBytes_WarnOncePerProblem(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	probes := &hostStorageProbes{}
	probe := func(override map[string]fakeVirshResult) int64 {
		script := fullHostScript()
		for k, v := range override {
			script[k] = v
		}
		return collectStorageBytes(context.Background(), &fakeHostConn{id: "h", script: script}, "h", logger, probes)
	}
	misplaced := map[string]fakeVirshResult{
		"pool-dumpxml default":           {stdout: poolDumpXMLAt("/srv/other-images")},
		realpathKey("/srv/other-images"): {stdout: nulList("/srv/other-images", defaultPoolRequiredDir)},
	}
	statFails := map[string]fakeVirshResult{statfsKey(): {err: errors.New("exit status 124")}}

	for range 3 {
		assert.Zero(t, probe(misplaced))
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"))
	for range 2 {
		assert.Zero(t, probe(statFails))
	}
	assert.Equal(t, 2, strings.Count(logs.String(), "level=WARN"), "a different problem warns again")
	assert.Equal(t, statfsFixtureBytes, probe(nil))
	assert.Zero(t, probe(statFails))
	assert.Equal(t, 3, strings.Count(logs.String(), "level=WARN"), "a problem after a measurement warns again")
	assert.Equal(t, 3, strings.Count(logs.String(), "path=/srv/other-images"), "Debug repeats carry the attributes too")
}

// TestParseStatfsAvailBytes_ClipsQuotedOutput (#417 L3): host output quoted
// in an error is clipped.
func TestParseStatfsAvailBytes_ClipsQuotedOutput(t *testing.T) {
	_, err := parseStatfsAvailBytes(strings.Repeat("x", 10_000) + " y z")
	require.Error(t, err)
	assert.Less(t, len(err.Error()), maxLoggedHostOutputBytes+100)
	assert.Contains(t, err.Error(), "...")

	assert.Equal(t, "short", clipForLog("short"))
	clipped := clipForLog(strings.Repeat("é", maxLoggedHostOutputBytes))
	assert.True(t, utf8.ValidString(clipped), "clipped on a rune boundary")
}

// staticRunner is a hostCommandRunner answering every command with stdout.
type staticRunner struct{ stdout string }

func (r staticRunner) runVirshCommand(_ context.Context, _ ...string) (*VirshResult, error) {
	return &VirshResult{Stdout: r.stdout}, nil
}

// TestHostFreeBytes_Strict (#417 L5): the import-time free-space check shares
// the strict parser — trailing junk and an overflowing product are refused.
func TestHostFreeBytes_Strict(t *testing.T) {
	got, err := hostFreeBytes(context.Background(), staticRunner{stdout: statfsFixture}, "/x")
	require.NoError(t, err)
	assert.Equal(t, statfsFixtureBytes, got)

	for name, out := range map[string]string{
		"trailing junk": "507261 1048576 junk\n",
		"overflow":      "9223372036854775807 4096\n",
		"negative":      "-1 4096\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := hostFreeBytes(context.Background(), staticRunner{stdout: out}, "/x")
			require.Error(t, err)
			assert.Zero(t, got)
		})
	}
}

// TestCollectStorageBytes_NetfsAndFsPoolsMeasured: every filesystem-backed pool
// type is measured with statvfs of its target directory.
func TestCollectStorageBytes_NetfsAndFsPoolsMeasured(t *testing.T) {
	for _, typ := range filesystemPoolTypes {
		t.Run(typ, func(t *testing.T) {
			script := fullHostScript()
			script["pool-dumpxml default"] = fakeVirshResult{stdout: "<pool type='" + typ + "'><name>default</name>" +
				"<available unit='bytes'>1</available><target><path>" + defaultPoolRequiredDir + "/</path></target></pool>"}
			conn := &fakeHostConn{id: "host-fs", script: script}

			assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), conn, "host-fs", slog.Default(), nil))
		})
	}
}

// TestCollectStorageBytes_ProductionPoolDir runs the unmodified lab capture
// against the production required directory (DefaultImageDir, which this test
// binary otherwise replaces with a fixture directory): a default pool at
// /var/lib/libvirt/images is measured; one that resolves elsewhere reports 0.
func TestCollectStorageBytes_ProductionPoolDir(t *testing.T) {
	prev := defaultPoolRequiredDir
	defaultPoolRequiredDir = DefaultImageDir
	t.Cleanup(func() { defaultPoolRequiredDir = prev })

	script := fullHostScript()
	script["pool-dumpxml default"] = fakeVirshResult{stdout: poolDumpXMLDirFixture}
	conn := &fakeHostConn{id: "hypervisor-3", script: script}
	assert.Equal(t, statfsFixtureBytes, collectStorageBytes(context.Background(), conn, "hypervisor-3", slog.Default(), nil))
	assert.Contains(t, conn.commands(), "! timeout --kill-after=1s 10s stat -f -c %a %S -- /var/lib/libvirt/images")

	script["pool-dumpxml default"] = fakeVirshResult{stdout: poolDumpXMLAt("/var/lib/libvirt/images-nfs")}
	script["! realpath -m -z -- /var/lib/libvirt/images-nfs /var/lib/libvirt/images"] = fakeVirshResult{
		stdout: nulList("/var/lib/libvirt/images-nfs", "/var/lib/libvirt/images")}
	moved := &fakeHostConn{id: "hypervisor-3", script: script}
	assert.Zero(t, collectStorageBytes(context.Background(), moved, "hypervisor-3", slog.Default(), nil))
	assert.Equal(t, []string{"pool-list --all", "pool-dumpxml default",
		"! realpath -m -z -- /var/lib/libvirt/images-nfs /var/lib/libvirt/images"}, moved.commands())
}

// TestCollectHostInfo_OnlyDefaultPoolCounts pins #416 M3: space in any other
// pool cannot hold a disk VirtRigaud lands in "default", so a host whose
// default pool is not active reports no allocatable storage.
func TestCollectHostInfo_OnlyDefaultPoolCounts(t *testing.T) {
	script := map[string]fakeVirshResult{
		"nodeinfo":          {stdout: nodeInfoFixture},
		"capabilities":      {stdout: capabilitiesFixture},
		"domcapabilities":   {stdout: domCapabilitiesFixture},
		"pool-list --all":   {stdout: " Name    State    Autostart\n-------\n default inactive no\n data   active   yes\n"},
		"pool-dumpxml data": {stdout: poolDumpXMLDirFixture},
	}
	conn := &fakeHostConn{id: "host-data", script: script}

	got := collectHostInfo(context.Background(), conn, "host-data", "addr", nil, slog.Default(), nil)

	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Zero(t, got.AllocatableStorage)
	assert.Equal(t, []string{"nodeinfo", "capabilities", "domcapabilities", "pool-list --all"}, conn.commands(),
		"neither another pool nor an inactive default pool's directory is read")
}

// sharedFilesystem is one filesystem (an NFS export) that several fake hosts
// mount as their default pool directory: each host's statvfs reads its CURRENT
// free space, like the kernel does.
type sharedFilesystem struct {
	mu          sync.Mutex
	availBlocks int64
}

func (fs *sharedFilesystem) setAvailBlocks(n int64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.availBlocks = n
}

func (fs *sharedFilesystem) statfs(_ context.Context, argv []string) (*hostconn.Result, error) {
	if strings.Join(argv, " ") != strings.TrimPrefix(statfsKey(), "! ") {
		return nil, fmt.Errorf("sharedFilesystem: unexpected host command %q", argv)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return &hostconn.Result{Stdout: fmt.Sprintf("%d 1048576\n", fs.availBlocks)}, nil
}

// TestProvider_ListHosts_SharedPoolSameFreshValue reproduces the 2026-10-22 lab
// finding (R31): two hosts mount the same NFS export as their default pool and
// libvirt's cached pool values disagree (220.8 GiB on one, 558.5 GiB on the
// other, never refreshed). Both hosts must report the SAME value — the
// filesystem's current free space — and follow it when it changes, with no
// pool-refresh.
func TestProvider_ListHosts_SharedPoolSameFreshValue(t *testing.T) {
	staleDumpXML := func(availBytes int64) string {
		return strings.Replace(labPoolDumpXML(), "<available unit='bytes'>596772913152</available>",
			fmt.Sprintf("<available unit='bytes'>%d</available>", availBytes), 1)
	}
	fs := &sharedFilesystem{availBlocks: 507261}
	hostScript := func(cachedAvail int64) map[string]fakeVirshResult {
		s := fullHostScript()
		s["pool-dumpxml default"] = fakeVirshResult{stdout: staleDumpXML(cachedAvail)}
		return s
	}
	conns := map[hostconn.HostID]*fakeHostConn{
		"hypervisor-2": {id: "hypervisor-2", script: hostScript(237086629478), runHost: fs.statfs}, // libvirt cached 220.8 GiB
		"hypervisor-3": {id: "hypervisor-3", script: hostScript(599684438835), runHost: fs.statfs}, // libvirt cached 558.5 GiB
	}
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts: []hostsecret.Host{
			{ID: "hypervisor-2", Endpoint: "qemu+ssh://virt@hypervisor-2/system"},
			{ID: "hypervisor-3", Endpoint: "qemu+ssh://virt@hypervisor-3/system"},
		},
	}
	p, _ := newClusteredProviderForTest(t, inv, fakeDialer(conns, nil))

	hosts, err := p.ListHosts(context.Background())
	require.NoError(t, err)
	require.Len(t, hosts, 2)
	assert.Equal(t, statfsFixtureBytes, hosts[0].AllocatableStorage)
	assert.Equal(t, hosts[0].AllocatableStorage, hosts[1].AllocatableStorage,
		"two hosts sharing one pool filesystem report the same value")

	// 100 GiB is written to the export (by either host, or anything else):
	// the next sync of both hosts sees it, without any pool-refresh.
	fs.setAvailBlocks(507261 - 100*1024)
	hosts, err = p.ListHosts(context.Background())
	require.NoError(t, err)
	want := statfsFixtureBytes - 100<<30
	assert.Equal(t, want, hosts[0].AllocatableStorage)
	assert.Equal(t, want, hosts[1].AllocatableStorage)

	for id, c := range conns {
		for _, cmd := range c.commands() {
			assert.NotContains(t, cmd, "pool-refresh", "host %s: the inventory never refreshes a pool", id)
			assert.NotContains(t, cmd, "pool-info", "host %s: libvirt's cached pool values are not read", id)
		}
	}
}

// --- Provider.ListHosts / GetHostInfo against a real ClusterRegistry ----------

// fakeDialer builds a hostconn.Dialer that returns the scripted fakeHostConn for
// a host id, or a dial error when the id is in dialErr. The real ClusterRegistry
// drives it, so lease accounting in these tests is the production code path.
func fakeDialer(conns map[hostconn.HostID]*fakeHostConn, dialErr map[hostconn.HostID]error) hostconn.Dialer {
	return func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		id := hostconn.HostID(h.ID)
		if err := dialErr[id]; err != nil {
			return nil, err
		}
		c, ok := conns[id]
		if !ok {
			return nil, fmt.Errorf("fakeDialer: no conn for %q", id)
		}
		return c, nil
	}
}

func newClusteredProviderForTest(t *testing.T, inv hostsecret.Inventory, dial hostconn.Dialer) (*Provider, *hostconn.ClusterRegistry) {
	t.Helper()
	reg, err := hostconn.NewClusterRegistry(inv, dial, slog.Default())
	require.NoError(t, err)
	p := &Provider{registry: reg, clusterReg: reg, logger: slog.Default()}
	t.Cleanup(func() { _ = reg.Close() })
	return p, reg
}

func TestProvider_ListHosts_ClusteredSiblingsRenderWhenOneIsDown(t *testing.T) {
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts: []hostsecret.Host{
			{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system", Labels: map[string]string{"zone": "a"}},
			{ID: "host-b", Endpoint: "qemu+ssh://virt@host-b/system"},
		},
	}
	conns := map[hostconn.HostID]*fakeHostConn{
		"host-a": {id: "host-a", script: fullHostScript()},
	}
	dialErr := map[hostconn.HostID]error{
		"host-b": errors.New("dial: no route to host"),
	}
	p, _ := newClusteredProviderForTest(t, inv, fakeDialer(conns, dialErr))

	hosts, err := p.ListHosts(context.Background())
	require.NoError(t, err)
	require.Len(t, hosts, 2)

	// Registry.Hosts() sorts, so host-a precedes host-b.
	assert.Equal(t, "host-a", hosts[0].ID)
	assert.Equal(t, contracts.HostHealthReady, hosts[0].Health)
	assert.Equal(t, int32(8), hosts[0].AllocatableCPU)
	assert.Equal(t, map[string]string{"zone": "a"}, hosts[0].Labels)

	// host-b could not be dialed: NotReady, but still reported with its address.
	assert.Equal(t, "host-b", hosts[1].ID)
	assert.Equal(t, contracts.HostHealthNotReady, hosts[1].Health)
	assert.Equal(t, "qemu+ssh://virt@host-b/system", hosts[1].Address)
}

func TestProvider_ListHosts_LeaseReleasedEvenOnQueryError(t *testing.T) {
	// The host dials fine but nodeinfo errors. The lease MUST still be released,
	// so a subsequent graceful-drain (Evict) closes the underlying connection
	// inline (inUse == 0). A leaked lease would park the entry in draining and
	// leave the conn open — Close would never be called.
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	conn := &fakeHostConn{
		id:     "host-a",
		script: map[string]fakeVirshResult{"nodeinfo": {err: errors.New("boom")}},
	}
	p, reg := newClusteredProviderForTest(t, inv,
		fakeDialer(map[hostconn.HostID]*fakeHostConn{"host-a": conn}, nil))

	hosts, err := p.ListHosts(context.Background())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	assert.Equal(t, contracts.HostHealthNotReady, hosts[0].Health)

	require.Equal(t, 0, conn.closeCount(), "lease Close must not close the shared connection")
	reg.Evict("host-a")
	assert.Equal(t, 1, conn.closeCount(),
		"after Evict the idle (lease-released) connection must be closed inline; a leaked lease would keep it open")
}

func TestProvider_GetHostInfo_SingleHostRefresh(t *testing.T) {
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	p, _ := newClusteredProviderForTest(t, inv,
		fakeDialer(map[hostconn.HostID]*fakeHostConn{"host-a": conn}, nil))

	got, err := p.GetHostInfo(context.Background(), "host-a")
	require.NoError(t, err)
	assert.Equal(t, "host-a", got.ID)
	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Equal(t, int32(8), got.AllocatableCPU)
}

func TestProvider_GetHostInfo_UnknownHostNotFound(t *testing.T) {
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	p, _ := newClusteredProviderForTest(t, inv,
		fakeDialer(map[hostconn.HostID]*fakeHostConn{"host-a": conn}, nil))

	_, err := p.GetHostInfo(context.Background(), "host-nope")
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestProvider_HostInventory_SingleHostUnimplemented(t *testing.T) {
	// A single-host provider (no clusterReg) must report the clustered-only RPCs
	// as Unimplemented — matching supports_clustering = false there (D9).
	p := &Provider{}

	_, lerr := p.ListHosts(context.Background())
	require.Error(t, lerr)
	assert.Equal(t, codes.Unimplemented, status.Code(lerr))

	_, gerr := p.GetHostInfo(context.Background(), "host-a")
	require.Error(t, gerr)
	assert.Equal(t, codes.Unimplemented, status.Code(gerr))
}
