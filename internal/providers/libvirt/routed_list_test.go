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
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin ADR-0007 Addendum A slice 4 on the provider side: the
// clustered ListVMs fan-out (every routable host, bounded concurrency, a
// per-host deadline, host-tagged VMs, unreachable hosts reported — never
// dropped — and never failing the call) and TransferOwner, the
// compare-and-swap owner re-stamp adoption uses.
//
// Hosts are real VirshProviders on LOCAL per-host URIs (qemu:///<host>); a
// fake `virsh` on PATH answers per host from files in a temp directory and
// logs every call as "<host> <args>". Nothing on the test machine (least of
// all /var/lib/libvirt) is read or written.

// listFakeVirsh is the per-host fake virsh the slice 4 tests install. Per
// host directory: list.txt (`virsh list --all`), dom-<name|uuid>.xml
// (dumpxml, with or without --inactive), uuid-<name> (dominfo's UUID),
// stamped-<uuid>.xml + name-<uuid> (what `virsh metadata ... --set` turns the
// domain into), fail-<cmd> (fail that command), hang-list (list never
// answers: exec sleep, so killing it closes its pipes).
const listFakeVirsh = `#!/bin/sh
host=local
if [ "$1" = "-c" ]; then host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_LIST_DIR/calls.log"
d="$FAKE_LIST_DIR/$host"
fail() { if [ -f "$d/fail-$1" ]; then echo "error: scripted failure of $1" >&2; exit 1; fi; }
case "$1" in
  list)
    if [ -f "$d/hang-list" ]; then exec sleep 30; fi
    fail list
    cat "$d/list.txt" ;;
  dumpxml)
    fail dumpxml
    n="$2"; if [ "$n" = "--inactive" ]; then n="$3"; fi
    if [ -f "$d/dom-$n.xml" ]; then cat "$d/dom-$n.xml"; else echo "error: failed to get domain '$n'" >&2; exit 1; fi ;;
  dominfo)
    if [ -f "$d/dom-$2.xml" ]; then
      printf 'Id:             -\nName:           %s\nUUID:           %s\nState:          shut off\nCPU(s):         2\nMax memory:     2097152 KiB\n' "$2" "$(cat "$d/uuid-$2")"
    else echo "error: failed to get domain '$2'" >&2; exit 1; fi ;;
  domstate) echo "shut off" ;;
  metadata)
    fail metadata
    if [ -f "$d/stamped-$2.xml" ]; then
      n=$(cat "$d/name-$2")
      cp "$d/stamped-$2.xml" "$d/dom-$n.xml"
      cp "$d/stamped-$2.xml" "$d/dom-$2.xml"
    fi ;;
  destroy|start|shutdown) exit 0 ;;
  *) echo "fake virsh: unsupported: $*" >&2; exit 1 ;;
esac
`

// listDomain is one domain seeded on a fake host.
type listDomain struct {
	name, uuid, state string
	owner             contracts.ObjectIdentity
}

// listDomainDoc renders dom's `virsh dumpxml` document.
func listDomainDoc(dom listDomain) string {
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n%s"+
		"  <memory unit='KiB'>2097152</memory>\n  <vcpu placement='static'>2</vcpu>\n"+
		"  <devices>\n    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n"+
		"      <source file='/var/lib/libvirt/images/%s.qcow2'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n"+
		"  </devices>\n</domain>\n", dom.name, dom.uuid, renderOwnerMetadataXML(dom.owner), dom.name)
}

// listFixture is the fake per-host environment of the slice 4 tests.
type listFixture struct {
	t   *testing.T
	dir string
}

// newListFixture installs listFakeVirsh and seeds each host with domains.
func newListFixture(t *testing.T, hosts map[string][]listDomain) *listFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shims")
	}
	dir := t.TempDir()
	f := &listFixture{t: t, dir: dir}
	for host, doms := range hosts {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, host), 0o700))
		var list strings.Builder
		list.WriteString(" Id   Name                  State\n-------------------------------------\n")
		for _, dom := range doms {
			state := dom.state
			if state == "" {
				state = domainStateShutOff
			}
			fmt.Fprintf(&list, " -    %-21s %s\n", dom.name, state)
			f.write(host, "dom-"+dom.name+".xml", listDomainDoc(dom))
			f.write(host, "dom-"+dom.uuid+".xml", listDomainDoc(dom))
			f.write(host, "uuid-"+dom.name, dom.uuid)
		}
		f.write(host, "list.txt", list.String())
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(listFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_LIST_DIR", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// write writes one per-host fixture file.
func (f *listFixture) write(host, name, content string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, host, name), []byte(content), 0o600))
}

// read returns one per-host fixture file ("" when absent).
func (f *listFixture) read(host, name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, host, name)) //nolint:gosec // test reads its own fixture
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(f.t, err)
	return string(b)
}

// calls returns the logged virsh invocations ("<host> <args>").
func (f *listFixture) calls() []string {
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

// scriptStamp makes `virsh metadata <uuid> ... --set` on host turn dom into
// the same domain stamped with owner — what libvirt does — built with the
// production splice (stampOwnerMetadata) from an unstamped copy.
func (f *listFixture) scriptStamp(host string, dom listDomain, owner contracts.ObjectIdentity) {
	f.t.Helper()
	bare := dom
	bare.owner = contracts.ObjectIdentity{}
	stamped, err := stampOwnerMetadata(listDomainDoc(bare), owner)
	require.NoError(f.t, err)
	f.write(host, "stamped-"+dom.uuid+".xml", stamped)
	f.write(host, "name-"+dom.uuid, dom.name)
}

// clusterOf builds a clustered provider over hosts, each a *virshConn on its
// own local VirshProvider, with the always-failing single-host placeholder;
// a host in down fails its dial (unreachable).
func clusterOf(t *testing.T, hosts []string, down ...string) *Provider {
	t.Helper()
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	conns := map[string]*virshConn{}
	for _, h := range hosts {
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
		conns[h] = newClusteredVirshConn(hostconn.HostID(h), localHostVP(h), nil)
	}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		for _, d := range down {
			if d == h.ID {
				return nil, fmt.Errorf("dial %s: connection refused", h.Endpoint)
			}
		}
		return conns[h.ID], nil
	}
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	return p
}

// byHostAndID indexes a proto list by (host_id, id).
func byHostAndID(vms []*providerv1.VMInfo) map[[2]string]*providerv1.VMInfo {
	out := map[[2]string]*providerv1.VMInfo{}
	for _, v := range vms {
		out[[2]string{v.GetHostId(), v.GetId()}] = v
	}
	return out
}

const (
	uuidWebA = "aaaaaaaa-0000-4000-8000-00000000000a"
	uuidWebB = "bbbbbbbb-0000-4000-8000-00000000000b"
	uuidDBA  = "aaaaaaaa-0000-4000-8000-0000000000db"
)

// TestClustered_ListVMs_FansOutTagsHostsAndReportsUnreachable is the A3
// contract end to end through the gRPC Server: three hosts, one unreachable.
// The reachable hosts' VMs come back tagged with their host — the same domain
// name on two hosts is two distinct entries — and the unreachable host is in
// unreachable_host_ids, not dropped, and does not fail the call.
func TestClustered_ListVMs_FansOutTagsHostsAndReportsUnreachable(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA, state: "running"}, {name: "team-a.db", uuid: uuidDBA, owner: ownerTeamA}},
		"host-b": {{name: "web", uuid: uuidWebB}},
		"host-c": {{name: "never-seen", uuid: "cccccccc-0000-4000-8000-00000000000c"}},
	})
	p := clusterOf(t, []string{"host-a", "host-b", "host-c"}, "host-c")

	resp, err := NewServer(p).ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err, "an unreachable host never fails the call")

	got := byHostAndID(resp.GetVms())
	require.Len(t, got, 3)
	require.Contains(t, got, [2]string{"host-a", "web"})
	require.Contains(t, got, [2]string{"host-b", "web"}, "the same name on two hosts is two distinct VMs")
	require.Contains(t, got, [2]string{"host-a", "team-a.db"})
	assert.Equal(t, uuidWebA, got[[2]string{"host-a", "web"}].GetProviderRaw()[contracts.VMInfoUUIDKey])
	assert.Equal(t, uuidWebB, got[[2]string{"host-b", "web"}].GetProviderRaw()[contracts.VMInfoUUIDKey])
	assert.Equal(t, "On", got[[2]string{"host-a", "web"}].GetPowerState())
	assert.Equal(t, ownerTeamA.UID, got[[2]string{"host-a", "team-a.db"}].GetProviderRaw()[contracts.VMInfoOwnerUIDKey])
	// The owner stamp's namespace and name (A6's R4 looks incarnations up by
	// them); empty for an unstamped domain.
	assert.Equal(t, ownerTeamA.Namespace, got[[2]string{"host-a", "team-a.db"}].GetOwnerNamespace())
	assert.Equal(t, ownerTeamA.Name, got[[2]string{"host-a", "team-a.db"}].GetOwnerName())
	assert.Empty(t, got[[2]string{"host-a", "web"}].GetOwnerNamespace())
	assert.Empty(t, got[[2]string{"host-a", "web"}].GetOwnerName())
	assert.Equal(t, []string{"host-c"}, resp.GetUnreachableHostIds())

	assert.Zero(t, p.virshProvider.unroutableHits.Load(), "the listing never reaches the single-host placeholder")
	for _, c := range fx.calls() {
		assert.False(t, strings.HasPrefix(c, "host-c "), "the unreachable host ran nothing: %s", c)
	}
}

// TestClustered_ListVMs_HostWhoseListFailsIsUnreachable: a host that answers
// but whose `virsh list` fails is unknown, not empty.
func TestClustered_ListVMs_HostWhoseListFailsIsUnreachable(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA}},
		"host-b": {{name: "web", uuid: uuidWebB}},
	})
	fx.write("host-b", "fail-list", "")
	p := clusterOf(t, []string{"host-a", "host-b"})

	list, err := p.ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, list.VMs, 1)
	assert.Equal(t, "host-a", list.VMs[0].HostID)
	assert.Equal(t, []string{"host-b"}, list.UnreachableHostIDs)
	assert.True(t, list.Unreachable("host-b"))
	assert.False(t, list.Unreachable("host-a"))
}

// TestClustered_ListVMs_PerHostDeadline: a host that never answers is cut off
// at its own deadline and reported unreachable, while the other host's VMs are
// returned — one dead host cannot starve the rest.
func TestClustered_ListVMs_PerHostDeadline(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA}},
		"host-b": {{name: "web", uuid: uuidWebB}},
	})
	fx.write("host-b", "hang-list", "")
	p := clusterOf(t, []string{"host-a", "host-b"})
	p.listHostTimeout = 300 * time.Millisecond

	start := time.Now()
	list, err := p.ListVMs(context.Background())
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 10*time.Second, "the hung host is cut off at its deadline, not after its 30s sleep")
	require.Len(t, list.VMs, 1)
	assert.Equal(t, "host-a", list.VMs[0].HostID)
	assert.Equal(t, []string{"host-b"}, list.UnreachableHostIDs)
}

// TestClustered_ListVMs_DeadlineAndConcurrencyBounds drives the fan-out with a
// scripted per-host listing: at most listHostConcurrency hosts are listed at
// once, a host that blocks is cut off at the per-host deadline, and every
// other host is still listed.
func TestClustered_ListVMs_DeadlineAndConcurrencyBounds(t *testing.T) {
	hosts := []string{"host-a", "host-b", "host-c", "host-d", "host-e", "host-f"}
	p := clusterOf(t, hosts)
	p.listHostTimeout = 200 * time.Millisecond
	p.listHostConcurrency = 2

	var inFlight, peak atomic.Int32
	p.listHostVMsFn = func(ctx context.Context, c libvirtConn) ([]contracts.VMInfo, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		if c.HostID() == "host-c" {
			<-ctx.Done() // never answers on its own
			return nil, ctx.Err()
		}
		time.Sleep(20 * time.Millisecond)
		return []contracts.VMInfo{{ID: "vm", Name: "vm", HostID: string(c.HostID())}}, nil
	}

	list, err := p.ListVMs(context.Background())
	require.NoError(t, err)
	assert.LessOrEqual(t, peak.Load(), int32(2), "at most listHostConcurrency hosts are listed at once")
	assert.Equal(t, []string{"host-c"}, list.UnreachableHostIDs)
	var listed []string
	for _, v := range list.VMs {
		listed = append(listed, v.HostID)
	}
	sort.Strings(listed)
	assert.Equal(t, []string{"host-a", "host-b", "host-d", "host-e", "host-f"}, listed)
}

// TestClustered_ListVMs_BudgetFitsTheCallerDeadline: hosts not listed before
// the caller's deadline (less the response margin) are reported unreachable,
// and the call answers before the caller's deadline.
func TestClustered_ListVMs_BudgetFitsTheCallerDeadline(t *testing.T) {
	p := clusterOf(t, []string{"host-a", "host-b", "host-c"})
	p.listHostConcurrency = 1
	p.listHostVMsFn = func(ctx context.Context, c libvirtConn) ([]contracts.VMInfo, error) {
		if c.HostID() == "host-a" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []contracts.VMInfo{{ID: "vm", HostID: string(c.HostID())}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	list, err := p.ListVMs(ctx)
	require.NoError(t, err, "the answer is sent before the caller's deadline")
	assert.NoError(t, ctx.Err())
	assert.Contains(t, list.UnreachableHostIDs, "host-a", "the hung host used the rest of the budget")
	assert.Len(t, list.VMs, 3-len(list.UnreachableHostIDs),
		"every host is accounted for: listed before host-a started, or never started and reported unknown, not empty")
}

// TestClustered_ListVMs_CallerGoneFailsTheCall: when the caller's context
// ends, the partial answer is not returned as if it were complete.
func TestClustered_ListVMs_CallerGoneFailsTheCall(t *testing.T) {
	p := clusterOf(t, []string{"host-a"})
	ctx, cancel := context.WithCancel(context.Background())
	p.listHostVMsFn = func(context.Context, libvirtConn) ([]contracts.VMInfo, error) {
		cancel()
		return nil, nil
	}
	_, err := p.ListVMs(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestClustered_ListVMs_UnreachableHostDoesNotTripTheBreaker: over a real
// gRPC hop with the manager's circuit breaker at a failure threshold of 1, a
// ListVMs with an unreachable host — and a TransferOwner to that host
// (HOST_UNAVAILABLE) — leave the breaker closed.
func TestClustered_ListVMs_UnreachableHostDoesNotTripTheBreaker(t *testing.T) {
	newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA}},
		"host-b": {},
	})
	p := clusterOf(t, []string{"host-a", "host-b"}, "host-b")
	cb := resilience.NewCircuitBreaker("slice4", "libvirt", "clustered", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
	})
	c := startBreakerGRPC(t, p, cb)

	for i := 0; i < 3; i++ {
		list, err := c.ListVMs(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"host-b"}, list.UnreachableHostIDs)
		require.Len(t, list.VMs, 1)
		assert.Equal(t, "host-a", list.VMs[0].HostID)
	}
	err := c.TransferOwner(context.Background(), contracts.TransferOwnerRequest{
		VM:           contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA},
		ExpectedUUID: uuidWebB,
	})
	require.Error(t, err)
	assert.True(t, contracts.IsHostUnavailable(err), "got %v", err)
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "host-scoped failures never count toward the Provider's breaker")
}

// startBreakerGRPC serves a libvirt Server on loopback gRPC and returns a
// manager transport client with circuit breaker cb.
func startBreakerGRPC(t *testing.T, backend providerBackend, cb *resilience.CircuitBreaker) *transportgrpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, NewServer(backend))
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)
	c, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "libvirt", "slice4", cb, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClustered_ListVMs_ShadowComparesPerHostOnTheSameLease: in shadow mode
// each host's native list runs on that host's lease and is joined on
// (host id, VM id), so the same domain name on two hosts is never
// cross-matched.
func TestClustered_ListVMs_ShadowComparesPerHostOnTheSameLease(t *testing.T) {
	newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA}},
		"host-b": {{name: "web", uuid: uuidWebB}},
	})
	p := clusterOf(t, []string{"host-a", "host-b"})
	cfg, _ := parseNativeConfig("shadow:list")
	p.nativeCfg = cfg
	p.shadowSampler = &sampler{n: 1}

	var mu sync.Mutex
	seen := map[string]int{}
	p.listNativeFn = func(_ context.Context, c libvirtConn) ([]contracts.VMInfo, error) {
		if !assert.NotNil(t, c, "a clustered shadow runs on the host's lease, never the single-host connection") {
			return nil, errors.New("no lease")
		}
		mu.Lock()
		seen[string(c.HostID())]++
		mu.Unlock()
		return nil, errors.New("scripted: native not wired")
	}

	_, err := p.ListVMs(context.Background())
	require.NoError(t, err)
	p.shadowWG.Wait()
	assert.Equal(t, map[string]int{"host-a": 1, "host-b": 1}, seen)
}

// TestCompareList_JoinsOnHostAndID: two hosts each have "web". The native
// answer for host-b's "web" is missing and host-a's has another vCPU count:
// the join on (host id, VM id) reports membership (host-b's) and vcpu
// (host-a's), and never matches host-a's native "web" against host-b's.
func TestCompareList_JoinsOnHostAndID(t *testing.T) {
	onHost := func(v contracts.VMInfo, host string) contracts.VMInfo { v.HostID = host; return v }
	virsh := []contracts.VMInfo{
		onHost(listVM("web", "On", "aaa", 2, 2048), "host-a"),
		onHost(listVM("web", "Off", "bbb", 4, 4096), "host-b"),
	}
	native := []contracts.VMInfo{onHost(listVM("web", "On", "aaa", 8, 2048), "host-a")}
	assert.ElementsMatch(t, []string{membershipField, "vcpu"}, compareList(virsh, native))

	// The same answers without host ids (single-host) join on the id alone,
	// exactly as before slice 4.
	assert.ElementsMatch(t, []string{"vcpu"}, compareList(
		[]contracts.VMInfo{listVM("web", "On", "aaa", 2, 2048)},
		[]contracts.VMInfo{listVM("web", "On", "aaa", 8, 2048)}))
	assert.Empty(t, compareList(
		[]contracts.VMInfo{onHost(listVM("web", "On", "aaa", 2, 2048), "host-a")},
		[]contracts.VMInfo{onHost(listVM("web", "On", "aaa", 2, 2048), "host-a")}))
}

// TestClustered_Capabilities_AdvertiseRoutedAdoption: a clustered provider
// advertises supports_routed_adoption now that ListVMs and TransferOwner are
// routed; a single-host provider does not.
func TestClustered_Capabilities_AdvertiseRoutedAdoption(t *testing.T) {
	clustered := clusterOf(t, []string{"host-a"})
	caps, err := NewServer(clustered).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsRoutedAdoption())

	single := &Provider{virshProvider: localHostVP("single")}
	caps, err = NewServer(single).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.False(t, caps.GetSupportsRoutedAdoption())

	_, err = NewServer(single).TransferOwner(context.Background(), &providerv1.TransferOwnerRequest{Id: "web"})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "single-host adoption does not stamp")
}

// TestSoleOwner: the owner namespace and name are reported only for a domain
// with exactly one readable stamp.
func TestSoleOwner(t *testing.T) {
	stamped := listDomainDoc(listDomain{name: "web", uuid: uuidWebA, owner: ownerTeamA})
	assert.Equal(t, ownerTeamA, soleOwner(stamped))
	assert.Equal(t, contracts.ObjectIdentity{}, soleOwner(listDomainDoc(listDomain{name: "web", uuid: uuidWebA})))
	two := strings.Replace(listDomainDoc(listDomain{name: "web", uuid: uuidWebA}), "<memory",
		"<metadata>"+renderOwnerElementXML(ownerTeamA)+renderOwnerElementXML(ownerTeamB)+"</metadata>\n  <memory", 1)
	assert.Equal(t, contracts.ObjectIdentity{}, soleOwner(two), "two stamps are ambiguous")
	assert.Equal(t, contracts.ObjectIdentity{}, soleOwner("<domain"), "unreadable")
}

// TestClustered_ListVMs_HostOverTheDomainCapIsUnreachable: a host with more
// domains than one listing reads is reported unreachable (unknown) before any
// definition is read; single-host has no cap.
func TestClustered_ListVMs_HostOverTheDomainCapIsUnreachable(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "web", uuid: uuidWebA}},
		"host-b": {{name: "web", uuid: uuidWebB}},
	})
	var list strings.Builder
	list.WriteString(" Id   Name                  State\n-------------------------------------\n")
	for i := 0; i <= clusteredListMaxDomainsPerHost; i++ {
		fmt.Fprintf(&list, " -    d%-20d shut off\n", i)
	}
	fx.write("host-b", "list.txt", list.String())
	p := clusterOf(t, []string{"host-a", "host-b"})

	got, err := p.ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, got.VMs, 1)
	assert.Equal(t, "host-a", got.VMs[0].HostID)
	assert.Equal(t, []string{"host-b"}, got.UnreachableHostIDs)
	for _, c := range fx.calls() {
		assert.False(t, strings.HasPrefix(c, "host-b dumpxml"), "no definition of the over-cap host is read: %s", c)
	}
}

// TestClustered_ListVMs_RotatesTheStartingHost: successive calls start the
// fan-out at different hosts (so a spent budget does not always starve the
// same tail), while the answer stays in host order.
func TestClustered_ListVMs_RotatesTheStartingHost(t *testing.T) {
	hosts := []string{"host-a", "host-b", "host-c"}
	p := clusterOf(t, hosts)
	p.listHostConcurrency = 1
	var mu sync.Mutex
	var firsts []string
	var seen int
	p.listHostVMsFn = func(_ context.Context, c libvirtConn) ([]contracts.VMInfo, error) {
		mu.Lock()
		if seen%len(hosts) == 0 {
			firsts = append(firsts, string(c.HostID()))
		}
		seen++
		mu.Unlock()
		return []contracts.VMInfo{{ID: "vm", HostID: string(c.HostID())}}, nil
	}
	for i := 0; i < 3; i++ {
		got, err := p.ListVMs(context.Background())
		require.NoError(t, err)
		require.Len(t, got.VMs, 3)
		assert.Equal(t, []string{"host-a", "host-b", "host-c"},
			[]string{got.VMs[0].HostID, got.VMs[1].HostID, got.VMs[2].HostID}, "the answer stays in host order")
	}
	assert.ElementsMatch(t, hosts, firsts, "each call started at another host")
}

// TestCappedBuffer: output past the bound is discarded (still drained) and
// flagged.
func TestCappedBuffer(t *testing.T) {
	b := cappedBuffer{max: 5}
	n, err := b.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.False(t, b.exceeded)
	n, err = b.Write([]byte("defgh"))
	require.NoError(t, err)
	assert.Equal(t, 5, n, "reported written so the remote side is drained")
	assert.True(t, b.exceeded)
	assert.Equal(t, "abcde", b.String())
}
