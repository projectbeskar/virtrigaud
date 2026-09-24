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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum A slice 1: routed
// Describe/Delete on a clustered provider (withHostConn over the leased host),
// the owner-checked clustered Delete, the always-failing single-host
// placeholder that no per-VM RPC may reach, the clustered capability set, and
// that single-host Describe/Delete are unchanged (they still run on
// p.virshProvider, ignoring any host).
//
// Hosts are real VirshProviders on LOCAL per-host URIs (qemu:///<host>), so the
// production cores run unmodified; a fake `virsh` on PATH answers per host (it
// routes on the -c URI) and logs every call as "<host> <args>". Host-shell
// commands ("!" escape: sudo rm / rm -rf) hit fake `sudo`/`rm` shims that only
// log, so nothing on the test machine is touched.

const routingDiskPath = "/var/lib/libvirt/images/web-disk.qcow2"

// routingDomainXML is a `virsh dumpxml` document for name carrying owner's stamp
// (none when owner is zero) and one file-backed disk.
func routingDomainXML(name string, owner contracts.ObjectIdentity) string {
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>11111111-2222-4333-8444-555555555555</uuid>\n%s"+
		"  <devices>\n    <disk type='file'>\n      <source file='%s'/>\n    </disk>\n  </devices>\n</domain>\n",
		name, renderOwnerMetadataXML(owner), routingDiskPath)
}

// routingFixture installs the per-host fake virsh (+ sudo/rm shims) and returns
// a function that reads the call log.
type routingFixture struct {
	t   *testing.T
	dir string
}

func newRoutingFixture(t *testing.T, hosts map[string]map[string]string) *routingFixture {
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
		}
		require.NoError(t, os.WriteFile(filepath.Join(hd, "list.txt"), []byte(list.String()), 0o600))
	}

	virsh := `#!/bin/sh
host=local
if [ "$1" = "-c" ]; then host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
d="$FAKE_VIRSH_DIR/$host"
case "$1" in
  list) cat "$d/list.txt" ;;
  dumpxml)
    f="$d/dom-$2.xml"
    if [ -f "$f" ]; then cat "$f"; else echo "error: failed to get domain '$2'" >&2; exit 1; fi ;;
  dominfo)
    if [ -f "$d/dom-$2.xml" ]; then
      printf 'Id:             -\nName:           %s\nState:          shut off\nCPU(s):         1\nMax memory:     1048576 KiB\n' "$2"
    else echo "error: failed to get domain '$2'" >&2; exit 1; fi ;;
  destroy|undefine) exit 0 ;;
  *) echo "fake virsh: unsupported: $*" >&2; exit 1 ;;
esac
`
	shim := "#!/bin/sh\nprintf 'local %s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$FAKE_VIRSH_DIR/calls.log\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(virsh), 0o755)) //nolint:gosec // test shim must be executable
	for _, name := range []string{"sudo", "rm"} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(shim), 0o755)) //nolint:gosec // test shim must be executable
	}
	t.Setenv("FAKE_VIRSH_DIR", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &routingFixture{t: t, dir: dir}
}

// calls returns the logged invocations ("<host> <args>").
func (f *routingFixture) calls() []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.log")) //nolint:gosec // test reads its own fixture log
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(f.t, err)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// hostsOf returns the set of host tags that appear in the virsh calls.
func hostsOf(calls []string) map[string]bool {
	out := map[string]bool{}
	for _, c := range calls {
		out[strings.SplitN(c, " ", 2)[0]] = true
	}
	return out
}

// localHostVP is a VirshProvider on the LOCAL URI qemu:///<host>.
func localHostVP(host string) *VirshProvider {
	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu:///" + host}})
	vp.uri = "qemu:///" + host
	return vp
}

// routedCluster builds a clustered provider over host-a and host-b, each a
// *virshConn on its own local VirshProvider, with the production always-failing
// placeholder as p.virshProvider. closes counts connection Closes per host (the
// registry closes a connection only once no lease on it is outstanding).
func routedCluster(t *testing.T) (*Provider, *hostconn.ClusterRegistry, map[string]*atomic.Int32) {
	t.Helper()
	closes := map[string]*atomic.Int32{"host-a": {}, "host-b": {}}
	conns := map[string]*virshConn{}
	for host := range closes {
		c := closes[host]
		conns[host] = newClusteredVirshConn(hostconn.HostID(host), localHostVP(host), func() { c.Add(1) })
	}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		c, ok := conns[h.ID]
		if !ok {
			return nil, fmt.Errorf("no conn for %q", h.ID)
		}
		return c, nil
	}
	p, reg := newClusteredProviderForTest(t, twoHostInventory(), dial)
	p.virshProvider = newUnroutableVirshProvider()
	return p, reg, closes
}

// ─── the placeholder ──────────────────────────────────────────────────────────

func TestNewClusteredProvider_PlaceholderAlwaysFails(t *testing.T) {
	inv := filepath.Join(t.TempDir(), "hosts.json")
	require.NoError(t, os.WriteFile(inv, []byte(`{"schemaVersion":1,"hosts":[]}`), 0o600))
	p, err := newClusteredProvider(inv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.True(t, p.clustered())
	require.NotNil(t, p.virshProvider)
	_, err = p.virshProvider.getDomainState(context.Background(), "web")
	require.ErrorIs(t, err, errUnroutableCall, "an unrouted call fails instead of reaching any host")
	require.ErrorIs(t, p.virshProvider.writeRemoteFile(context.Background(), filepath.Join(t.TempDir(), "x"), []byte("x")), errUnroutableCall)
	require.ErrorIs(t, p.virshProvider.callLibvirt(context.Background(), nil), errUnroutableCall)
	_, err = p.virshProvider.ensureSSHClient(context.Background())
	require.ErrorIs(t, err, errUnroutableCall)
	assert.EqualValues(t, 4, p.virshProvider.unroutableHits.Load(), "every refused call is counted")
}

// TestClustered_EveryPerVMRPC_NeverReachesPlaceholder drives EVERY per-VM RPC
// (and the host-scoped / list RPCs) through the gRPC Server of a clustered
// provider and proves none of them reaches the single-host placeholder: Describe
// and Delete are routed to their host, everything else is an honest
// Unimplemented until its slice lands.
func TestClustered_EveryPerVMRPC_NeverReachesPlaceholder(t *testing.T) {
	fx := newRoutingFixture(t, map[string]map[string]string{
		"host-a": {"web": routingDomainXML("web", ownerTeamA)},
		"host-b": {},
	})
	p, _, _ := routedCluster(t)
	s := NewServer(p)
	ctx := context.Background()
	owner := &providerv1.ObjectIdentity{Uid: ownerTeamA.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name}

	d, err := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.True(t, d.Exists)

	_, err = s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: "host-a", Owner: owner})
	require.NoError(t, err)

	unimplemented := map[string]func() error{
		"Power": func() error {
			_, e := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-a"})
			return e
		},
		"Reconfigure": func() error {
			_, e := s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: "{}", TargetHostId: "host-a"})
			return e
		},
		"HardwareUpgrade": func() error {
			_, e := s.HardwareUpgrade(ctx, &providerv1.HardwareUpgradeRequest{Id: "web", TargetHostId: "host-a"})
			return e
		},
		"SnapshotCreate": func() error {
			_, e := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "web", TargetHostId: "host-a"})
			return e
		},
		"SnapshotDelete": func() error {
			_, e := s.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: "web", SnapshotId: "s", TargetHostId: "host-a"})
			return e
		},
		"SnapshotRevert": func() error {
			_, e := s.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: "web", SnapshotId: "s", TargetHostId: "host-a"})
			return e
		},
		"Clone": func() error {
			_, e := s.Clone(ctx, &providerv1.CloneRequest{SourceVmId: "web", SourceHostId: "host-a", TargetName: "copy"})
			return e
		},
		"ExportDisk(pvc)": func() error {
			_, e := s.ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-a", BackendType: "pvc"})
			return e
		},
		"ExportDisk(s3)": func() error {
			_, e := s.ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-a", BackendType: "s3"})
			return e
		},
		"ExportDisk(nfs)": func() error {
			_, e := s.ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-a", BackendType: "nfs"})
			return e
		},
		"GetDiskInfo": func() error {
			_, e := s.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{VmId: "web", TargetHostId: "host-a"})
			return e
		},
		"ImportDisk": func() error {
			_, e := s.ImportDisk(ctx, &providerv1.ImportDiskRequest{SourceUrl: "file:///tmp/x.qcow2"})
			return e
		},
		"ImagePrepare": func() error {
			_, e := s.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: `{"path":"/var/lib/libvirt/images/b.qcow2"}`, TargetName: "t"})
			return e
		},
		"ListVMs": func() error {
			_, e := s.ListVMs(ctx, &providerv1.ListVMsRequest{})
			return e
		},
	}
	for name, call := range unimplemented {
		err := call()
		require.Error(t, err, name)
		assert.Equal(t, codes.Unimplemented, status.Code(err), "%s must be an honest Unimplemented until its slice lands", name)
	}

	assert.Zero(t, p.virshProvider.unroutableHits.Load(), "no RPC may reach the single-host placeholder")
	assert.Equal(t, map[string]bool{"host-a": true, "local": true}, hostsOf(fx.calls()),
		"only the routed host (and its host-shell cleanup) was touched; host-b never was")
}

// ─── routed Describe / Delete ─────────────────────────────────────────────────

func TestClustered_Describe_RoutedToLeasedHostAndReleasesLease(t *testing.T) {
	fx := newRoutingFixture(t, map[string]map[string]string{
		"host-a": {},
		"host-b": {"db": routingDomainXML("db", ownerTeamA)},
	})
	p, reg, closes := routedCluster(t)

	resp, err := p.Describe(context.Background(), contracts.VMRef{ID: "db", HostID: "host-b"})
	require.NoError(t, err)
	assert.True(t, resp.Exists)
	assert.Equal(t, "db", resp.ProviderRaw["Name"])
	assert.Equal(t, map[string]bool{"host-b": true}, hostsOf(fx.calls()), "every virsh read ran on the bound host")

	// The lease was returned: evicting the idle host closes its connection now.
	assert.Zero(t, closes["host-b"].Load())
	reg.Evict("host-b")
	assert.EqualValues(t, 1, closes["host-b"].Load(), "a leaked lease would keep the connection open")
	assert.Zero(t, closes["host-a"].Load(), "a host the VM is not bound to is never dialed")
}

// TestClustered_Describe_AbsentDomainIsExistsFalse feeds A4: on a routed
// Describe, a domain that is not on the bound host is reported exists=false,
// not an opaque read error.
func TestClustered_Describe_AbsentDomainIsExistsFalse(t *testing.T) {
	newRoutingFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {}})
	p, _, _ := routedCluster(t)
	resp, err := p.Describe(context.Background(), contracts.VMRef{ID: "gone", HostID: "host-a"})
	require.NoError(t, err)
	assert.False(t, resp.Exists)
}

func TestClustered_RoutingErrors(t *testing.T) {
	newRoutingFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {}})
	p, _, _ := routedCluster(t)
	s := NewServer(p)
	ctx := context.Background()

	for _, host := range []string{"", "   "} {
		_, err := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: host})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "an empty target_host_id never defaults to a host")
		_, err = s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: host})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	}

	_, err := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-zzz"})
	assert.Equal(t, codes.Unavailable, status.Code(err), "an unknown host is a retryable Unavailable")
	_, err = s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: "host-zzz"})
	assert.Equal(t, codes.Unavailable, status.Code(err))

	// Create shares the class: an unknown target host is Unavailable (A2).
	_, err = s.Create(ctx, &providerv1.CreateRequest{Name: "web", TargetHostId: "host-zzz"})
	assert.Equal(t, codes.Unavailable, status.Code(err))

	assert.Zero(t, p.virshProvider.unroutableHits.Load())
}

// TestClustered_Delete_OwnerChecked is A2's safety property: a clustered delete
// destroys a domain only when its owner stamp is the requester's; anything else
// is NotFound and never touched (only the read-only list/dumpxml run).
func TestClustered_Delete_OwnerChecked(t *testing.T) {
	cases := []struct {
		name      string
		domainXML string // "" = no such domain
		owner     contracts.ObjectIdentity
		destroyed bool
	}{
		{"owned by the requester", routingDomainXML("web", ownerTeamA), ownerTeamA, true},
		{"owned by another tenant", routingDomainXML("web", ownerTeamA), ownerTeamB, false},
		{"unstamped (legacy / foreign)", routingDomainXML("web", contracts.ObjectIdentity{}), ownerTeamA, false},
		{"request without owner", routingDomainXML("web", ownerTeamA), contracts.ObjectIdentity{}, false},
		{"unreadable stamp", "<domain><name>web", ownerTeamA, false},
		{"absent", "", ownerTeamA, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			domains := map[string]string{}
			if tc.domainXML != "" {
				domains["web"] = tc.domainXML
			}
			fx := newRoutingFixture(t, map[string]map[string]string{"host-a": domains, "host-b": {}})
			p, _, _ := routedCluster(t)

			_, err := p.Delete(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a"}, tc.owner)
			calls := fx.calls()
			if tc.destroyed {
				require.NoError(t, err)
				assert.Contains(t, calls, "host-a destroy web")
				assert.Contains(t, calls, "host-a undefine web")
				assert.Contains(t, calls, "local sudo rm -f "+routingDiskPath)
				return
			}
			require.Error(t, err)
			assert.True(t, contracts.IsNotFound(err), "a domain this VM does not own is reported not-found: %v", err)
			for _, c := range calls {
				assert.NotContains(t, c, "destroy")
				assert.NotContains(t, c, "undefine")
				assert.False(t, strings.HasPrefix(c, "local "), "no host file may be removed: %q", c)
			}
			if tc.domainXML != "" {
				assert.NotContains(t, err.Error(), ownerTeamA.UID, "the message never discloses the other owner")
			}
		})
	}
}

// TestClustered_Server_DeleteForeignIsNotFound checks the wire code the manager
// turns into "already gone" (finalizer released, domain untouched).
func TestClustered_Server_DeleteForeignIsNotFound(t *testing.T) {
	newRoutingFixture(t, map[string]map[string]string{"host-a": {"web": routingDomainXML("web", ownerTeamA)}, "host-b": {}})
	p, _, _ := routedCluster(t)
	_, err := NewServer(p).Delete(context.Background(), &providerv1.DeleteRequest{
		Id: "web", TargetHostId: "host-a",
		Owner: &providerv1.ObjectIdentity{Uid: ownerTeamB.UID},
	})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// TestClustered_ShadowDescribeHoldsTheSameLease proves the ADR-0008 shadow read
// of a routed Describe runs on the SAME host connection, and keeps its lease
// until it finishes: a drain that starts meanwhile does not close the
// connection under it.
func TestClustered_ShadowDescribeHoldsTheSameLease(t *testing.T) {
	newRoutingFixture(t, map[string]map[string]string{"host-a": {"web": routingDomainXML("web", ownerTeamA)}, "host-b": {}})
	p, reg, closes := routedCluster(t)
	cfg, _ := parseNativeConfig("shadow:describe")
	p.nativeCfg = cfg
	p.shadowSampler = &sampler{n: 1}
	p.logger = slog.Default()

	release := make(chan struct{})
	var mu sync.Mutex
	var shadowHost hostconn.HostID
	p.describeNativeFn = func(_ context.Context, c libvirtConn, _ string) (contracts.DescribeResponse, error) {
		mu.Lock()
		shadowHost = c.HostID()
		mu.Unlock()
		<-release
		return contracts.DescribeResponse{Exists: true}, nil
	}

	_, err := p.Describe(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a"})
	require.NoError(t, err)

	reg.Evict("host-a") // drain while the shadow still holds the lease
	assert.Zero(t, closes["host-a"].Load(), "the shadow's lease keeps the drained connection open")
	close(release)
	p.shadowWG.Wait()
	assert.EqualValues(t, 1, closes["host-a"].Load(), "the connection closes once the shadow returns its lease")
	mu.Lock()
	assert.Equal(t, hostconn.HostID("host-a"), shadowHost, "the shadow read ran on the virsh read's host")
	mu.Unlock()
}

// ─── clustered capabilities ───────────────────────────────────────────────────

func TestClustered_GetCapabilities_HidesUnroutedPerVMCapabilities(t *testing.T) {
	p, _, _ := routedCluster(t)
	caps, err := NewServer(p).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.SupportsClustering)
	assert.False(t, caps.SupportsImageImport, "image prepare is host-scoped and not routed")
	for name, v := range map[string]bool{
		"reconfigure online":    caps.SupportsReconfigureOnline,
		"disk expansion online": caps.SupportsDiskExpansionOnline,
		"snapshots":             caps.SupportsSnapshots,
		"memory snapshots":      caps.SupportsMemorySnapshots,
		"linked clones":         caps.SupportsLinkedClones,
		"disk export":           caps.SupportsDiskExport,
		"disk import":           caps.SupportsDiskImport,
		"export compression":    caps.SupportsExportCompression,
	} {
		assert.False(t, v, "%s must be hidden until its slice routes it", name)
	}
	assert.Empty(t, caps.SupportedExportBackends)
	assert.Empty(t, caps.SupportedImportBackends)
}

// ─── single-host: unchanged ───────────────────────────────────────────────────

// TestSingleHost_DescribeAndDelete_UnchangedOnVirshProvider proves D9: on a
// single-host provider Describe and Delete still run on p.virshProvider — no
// registry is involved (none is configured here), any HostID is ignored, the
// owner is not checked (a legacy unstamped domain stays deletable), and the
// virsh/host command sequence is the historical one.
func TestSingleHost_DescribeAndDelete_UnchangedOnVirshProvider(t *testing.T) {
	for _, hostID := range []string{"", "host-ignored"} {
		t.Run(fmt.Sprintf("hostID=%q", hostID), func(t *testing.T) {
			fx := newRoutingFixture(t, map[string]map[string]string{
				"single": {"web": routingDomainXML("web", contracts.ObjectIdentity{})}, // unstamped legacy domain
			})
			p := &Provider{virshProvider: localHostVP("single")}
			require.False(t, p.clustered())

			resp, err := p.Describe(context.Background(), contracts.VMRef{ID: "web", HostID: hostID})
			require.NoError(t, err)
			assert.True(t, resp.Exists)
			assert.Equal(t, "Off", resp.PowerState)
			describeCalls := fx.calls()
			require.NotEmpty(t, describeCalls)
			assert.Equal(t, "single dominfo web", describeCalls[0], "Describe starts with the historical dominfo")

			_, err = p.Delete(context.Background(), contracts.VMRef{ID: "web", HostID: hostID}, ownerTeamB)
			require.NoError(t, err, "single-host delete ignores the owner")
			assert.Equal(t, []string{
				"single list --all",
				"single dumpxml web",
				"single dumpxml web",
				"single destroy web",
				"single undefine web",
				"local sudo rm -f " + routingDiskPath,
			}, fx.calls()[len(describeCalls):], "the historical delete sequence, unchanged")
		})
	}
}

// TestSingleHost_NativeDescribeStillResolvesThroughRegistry pins that the
// single-host shadow read keeps resolving its connection through the registry,
// exactly as before routing (the D5 soak path): with no registry configured it
// fails the same way it always did, instead of using the connection it is handed.
func TestSingleHost_NativeDescribeStillResolvesThroughRegistry(t *testing.T) {
	p := &Provider{virshProvider: localHostVP("single")}
	_, err := p.describeNative(context.Background(), p.singleHostConn(), "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "libvirt provider not initialized")
}

// TestSingleHost_DeleteAbsentStillCleansOrphans pins that the single-host
// absent-domain path keeps its name-based orphan cleanup (the clustered path
// deliberately does not).
func TestSingleHost_DeleteAbsentStillCleansOrphans(t *testing.T) {
	fx := newRoutingFixture(t, map[string]map[string]string{"single": {}})
	p := &Provider{virshProvider: localHostVP("single")}
	_, err := p.Delete(context.Background(), contracts.VMRef{ID: "web"}, contracts.ObjectIdentity{})
	require.NoError(t, err)
	calls := fx.calls()
	assert.Equal(t, "single list --all", calls[0])
	assert.Contains(t, calls, "local sudo rm -f /var/lib/libvirt/images/web-disk.qcow2")
	assert.Contains(t, calls, "local rm -rf /tmp/virtrigaud-cloudinit/web")
}

// TestSingleHost_ServerErrorsKeepLegacyWireForm pins that the routed error
// mapping is clustered-only: a single-host Describe failure keeps the
// historical wrapped (codes.Unknown) form.
func TestSingleHost_ServerErrorsKeepLegacyWireForm(t *testing.T) {
	newRoutingFixture(t, map[string]map[string]string{"single": {}})
	p := &Provider{virshProvider: localHostVP("single")}
	_, err := NewServer(p).Describe(context.Background(), &providerv1.DescribeRequest{Id: "gone"})
	require.Error(t, err)
	assert.Equal(t, codes.Unknown, status.Code(err))
	assert.Contains(t, err.Error(), "failed to describe VM")
}
