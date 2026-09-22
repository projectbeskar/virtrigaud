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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

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

// poolInfoDefaultFixture / poolInfoDataFixture are `virsh pool-info --bytes`
// output: Available is a raw byte count with --bytes.
const poolInfoDefaultFixture = `Name:           default
UUID:           bb1e0000-0000-0000-0000-000000000000
State:          running
Persistent:     yes
Autostart:      yes
Capacity:       107374182400
Allocation:     53687091200
Available:      53687091200
`

const poolInfoDataFixture = `Name:           data
UUID:           cc2f0000-0000-0000-0000-000000000000
State:          running
Persistent:     yes
Autostart:      yes
Capacity:       53687091200
Allocation:     32212254720
Available:      21474836480
`

// fullHostScript is the scripted virsh command set for a fully-answering host.
func fullHostScript() map[string]fakeVirshResult {
	return map[string]fakeVirshResult{
		"nodeinfo":                  {stdout: nodeInfoFixture},
		"capabilities":              {stdout: capabilitiesFixture},
		"domcapabilities":           {stdout: domCapabilitiesFixture},
		"pool-list --all":           {stdout: poolListFixture},
		"pool-info --bytes default": {stdout: poolInfoDefaultFixture},
		"pool-info --bytes data":    {stdout: poolInfoDataFixture},
	}
}

// --- fakeHostConn: a hostconn.Conn whose Virsh returns scripted fixtures ------

type fakeVirshResult struct {
	stdout string
	err    error
}

// fakeHostConn is a hostconn.Conn that answers Virsh from a per-command script
// (key = the space-joined args), records the commands it was asked to run (so a
// test can assert what was and was NOT queried), and counts Close calls (so a
// test can prove a lease was released and the underlying conn closed).
type fakeHostConn struct {
	id     hostconn.HostID
	script map[string]fakeVirshResult

	mu     sync.Mutex
	calls  []string
	closeN int
}

func (f *fakeHostConn) HostID() hostconn.HostID { return f.id }

func (f *fakeHostConn) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
	cmd := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	r, ok := f.script[cmd]
	if !ok {
		return nil, fmt.Errorf("fakeHostConn: no script for %q", cmd)
	}
	if r.err != nil {
		return nil, r.err
	}
	return &hostconn.Result{Command: cmd, Stdout: r.stdout}, nil
}

func (f *fakeHostConn) RunHost(_ context.Context, _ ...string) (*hostconn.Result, error) {
	return nil, errors.New("fakeHostConn: RunHost not used")
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

func TestParsePoolAvailableBytes(t *testing.T) {
	t.Run("raw bytes", func(t *testing.T) {
		got, ok := parsePoolAvailableBytes(poolInfoDefaultFixture)
		require.True(t, ok)
		assert.Equal(t, int64(53687091200), got)
	})
	t.Run("bytes suffix tolerated", func(t *testing.T) {
		got, ok := parsePoolAvailableBytes("Available:      12345 bytes\n")
		require.True(t, ok)
		assert.Equal(t, int64(12345), got)
	})
	t.Run("missing Available line", func(t *testing.T) {
		_, ok := parsePoolAvailableBytes("Name: p\nState: running\n")
		assert.False(t, ok)
	})
	t.Run("non-numeric Available", func(t *testing.T) {
		_, ok := parsePoolAvailableBytes("Available:      unknown\n")
		assert.False(t, ok)
	})
}

// --- collectHostInfo: assembly + health gating + best-effort ------------------

func TestCollectHostInfo_FullRender(t *testing.T) {
	conn := &fakeHostConn{id: "host-a", script: fullHostScript()}
	labels := map[string]string{"zone": "rack-1"}

	got := collectHostInfo(context.Background(), conn, "host-a",
		"qemu+ssh://virt@host-a/system", labels, slog.Default())

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
	// 53687091200 (default) + 21474836480 (data); iso is inactive and skipped.
	assert.Equal(t, int64(75161927680), got.AllocatableStorage)
}

func TestCollectHostInfo_NodeinfoError_NotReady(t *testing.T) {
	conn := &fakeHostConn{
		id:     "host-down",
		script: map[string]fakeVirshResult{"nodeinfo": {err: errors.New("connection refused")}},
	}
	labels := map[string]string{"zone": "rack-9"}

	got := collectHostInfo(context.Background(), conn, "host-down", "qemu+ssh://virt@host-down/system", labels, slog.Default())

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

	got := collectHostInfo(context.Background(), conn, "host-partial", "addr", nil, slog.Default())

	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Equal(t, int32(8), got.AllocatableCPU)
	assert.Equal(t, int64(16384), got.AllocatableMemMiB)
	assert.Empty(t, got.CPUModel)
	assert.Empty(t, got.CPUFeatures)
	assert.Empty(t, got.MachineTypes)
	assert.Empty(t, got.EmulatorVersion)
	assert.Zero(t, got.AllocatableStorage)
}

func TestCollectHostInfo_OddPoolInfoSkipped(t *testing.T) {
	// One active pool answers with no Available line; storage stays 0 and the
	// host is still READY (storage is best-effort).
	script := map[string]fakeVirshResult{
		"nodeinfo":              {stdout: nodeInfoFixture},
		"capabilities":          {stdout: capabilitiesFixture},
		"domcapabilities":       {stdout: domCapabilitiesFixture},
		"pool-list --all":       {stdout: " Name    State    Autostart\n-------\n odd   active   yes\n"},
		"pool-info --bytes odd": {stdout: "Name: odd\nState: running\n"}, // no Available
	}
	conn := &fakeHostConn{id: "host-odd", script: script}

	got := collectHostInfo(context.Background(), conn, "host-odd", "addr", nil, slog.Default())

	assert.Equal(t, contracts.HostHealthReady, got.Health)
	assert.Zero(t, got.AllocatableStorage)
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
