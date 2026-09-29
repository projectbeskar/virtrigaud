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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 A6.2 (R4): the owner-filtered
// ListVMs of a clustered provider returns, from every host, only the domains
// it would name for one VirtualMachine ("<namespace>.<name>", whatever its
// stamp) and a legacy bare-named domain stamped for it; a candidate it cannot
// read is reported, never skipped; a host that stopped answering is
// unreachable; the answer is marked owner_filter_applied; a single-host
// provider ignores the filter byte for byte. The hosts are the slice 4 fake
// per-host virsh (listFixture); nothing on the test machine is touched.

// ownerPrev is a previous incarnation of team-a/web: same namespace and name,
// another UID.
var ownerPrev = contracts.ObjectIdentity{UID: "5d7e0c1a-2b3c-4d5e-8f60-718293a4b5c6", Namespace: "team-a", Name: "web"}

// ownerFilterTeamAWeb is the filter for team-a/web.
var ownerFilterTeamAWeb = &providerv1.ListVMsRequest{OwnerNamespace: "team-a", OwnerName: "web"}

// TestClustered_ListVMsOwnerFilter_ReturnsOnlyTheCandidates: across three
// hosts (one unreachable), only team-a/web's candidates come back — the
// namespaced domain on every host whatever its stamp (a previous incarnation
// on host-a, an unstamped one on host-b), and the legacy bare-named "web"
// only where its stamp records team-a/web — while other domains are never
// even read. The unreachable host is reported, and the answer is marked.
func TestClustered_ListVMsOwnerFilter_ReturnsOnlyTheCandidates(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {
			{name: "team-a.web", uuid: uuidWebA, state: "running", owner: ownerPrev},
			{name: "team-a.db", uuid: uuidDBA, owner: contracts.ObjectIdentity{UID: ownerTeamA.UID, Namespace: "team-a", Name: "db"}},
			{name: "web", uuid: "aaaaaaaa-0000-4000-8000-0000000000e0", owner: ownerTeamB},
		},
		"host-b": {
			{name: "team-a.web", uuid: uuidWebB},
			{name: "web", uuid: "bbbbbbbb-0000-4000-8000-0000000000e0", owner: ownerPrev},
		},
		"host-c": {{name: "team-a.web", uuid: "cccccccc-0000-4000-8000-00000000000c", owner: ownerPrev}},
	})
	p := clusterOf(t, []string{"host-a", "host-b", "host-c"}, "host-c")

	resp, err := NewServer(p).ListVMs(context.Background(), ownerFilterTeamAWeb)
	require.NoError(t, err, "an unreachable host never fails the call")
	assert.True(t, resp.GetOwnerFilterApplied())
	assert.Equal(t, []string{"host-c"}, resp.GetUnreachableHostIds())

	got := byHostAndID(resp.GetVms())
	require.Len(t, got, 3, "%v", got)
	prev := got[[2]string{"host-a", "team-a.web"}]
	require.NotNil(t, prev)
	assert.Equal(t, "team-a", prev.GetOwnerNamespace())
	assert.Equal(t, "web", prev.GetOwnerName())
	assert.Equal(t, ownerPrev.UID, prev.GetProviderRaw()[contracts.VMInfoOwnerUIDKey])
	assert.Equal(t, uuidWebA, prev.GetProviderRaw()[contracts.VMInfoUUIDKey])
	assert.Empty(t, prev.GetProviderRaw()[contracts.VMInfoOwnerStampStateKey])

	unstamped := got[[2]string{"host-b", "team-a.web"}]
	require.NotNil(t, unstamped, "the namespaced domain is returned whatever its stamp")
	assert.Empty(t, unstamped.GetOwnerNamespace())
	assert.Empty(t, unstamped.GetProviderRaw()[contracts.VMInfoOwnerUIDKey])

	legacy := got[[2]string{"host-b", "web"}]
	require.NotNil(t, legacy, "a legacy bare-named domain stamped for team-a/web is a candidate")
	assert.Equal(t, "team-a", legacy.GetOwnerNamespace())
	assert.NotContains(t, got, [2]string{"host-a", "web"}, "team-b's bare-named web is not team-a/web's")
	assert.NotContains(t, got, [2]string{"host-a", "team-a.db"})

	for _, c := range fx.calls() {
		assert.NotContains(t, c, "team-a.db", "a domain that is not a candidate is never read: %s", c)
		assert.False(t, strings.HasPrefix(c, "host-c "), "the unreachable host ran nothing: %s", c)
	}
	assert.Zero(t, p.virshProvider.unroutableHits.Load(), "the listing never reaches the single-host placeholder")
}

// TestClustered_ListVMsOwnerFilter_CostsOneLookupPerHost: each host runs one
// `virsh list --all` and reads only the candidates (a running candidate's
// persistent definition too), addressed with --domain.
func TestClustered_ListVMsOwnerFilter_CostsOneLookupPerHost(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {
			{name: "team-a.web", uuid: uuidWebA, state: "running", owner: ownerPrev},
			{name: "other-1", uuid: "aaaaaaaa-0000-4000-8000-000000000101"},
			{name: "other-2", uuid: "aaaaaaaa-0000-4000-8000-000000000102"},
		},
		"host-b": {{name: "other-3", uuid: "bbbbbbbb-0000-4000-8000-000000000103"}},
	})
	p := clusterOf(t, []string{"host-a", "host-b"})

	list, err := p.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	assert.True(t, list.OwnerFilterApplied)
	require.Len(t, list.VMs, 1)
	assert.Equal(t, "host-a", list.VMs[0].HostID)
	assert.Empty(t, list.UnreachableHostIDs)

	perHost := map[string][]string{}
	for _, c := range fx.calls() {
		host, args, _ := strings.Cut(c, " ")
		perHost[host] = append(perHost[host], args)
	}
	assert.Equal(t, []string{"list --all", "dumpxml --domain team-a.web", "dumpxml --inactive --domain " + uuidWebA}, perHost["host-a"])
	assert.Equal(t, []string{"list --all"}, perHost["host-b"], "a host without a candidate reads no definition")
}

// TestClustered_ListVMsOwnerFilter_ReportsTheCurrentSize (security review of
// A6.2, item 4): a candidate's current vCPUs and memory are reported beside
// its maxima — from <vcpu current=…> and <currentMemory> when the domain has
// hot-add headroom, and equal to the maxima when it has none — so the manager
// can size a re-attach from the domain itself.
func TestClustered_ListVMsOwnerFilter_ReportsTheCurrentSize(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "team-a.web", uuid: uuidWebA, owner: ownerPrev}},
		"host-b": {{name: "team-a.web", uuid: uuidWebB, owner: ownerPrev}},
	})
	hotAdd := strings.Replace(strings.Replace(fx.read("host-a", "dom-team-a.web.xml"),
		"<memory unit='KiB'>2097152</memory>", "<memory unit='KiB'>8388608</memory>\n  <currentMemory unit='KiB'>2097152</currentMemory>", 1),
		"<vcpu placement='static'>2</vcpu>", "<vcpu placement='static' current='2'>8</vcpu>", 1)
	require.Contains(t, hotAdd, "currentMemory")
	fx.write("host-a", "dom-team-a.web.xml", hotAdd)
	p := clusterOf(t, []string{"host-a", "host-b"})

	list, err := p.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	got := map[string]contracts.VMInfo{}
	for _, v := range list.VMs {
		got[v.HostID] = v
	}
	require.Len(t, got, 2)
	a := got["host-a"]
	assert.EqualValues(t, 8, a.CPU, "maxima as in an unfiltered listing")
	assert.EqualValues(t, 8192, a.MemoryMiB)
	assert.Equal(t, "2", a.ProviderRaw[contracts.VMInfoCurrentVCPUsKey])
	assert.Equal(t, "2048", a.ProviderRaw[contracts.VMInfoCurrentMemoryMiBKey])
	b := got["host-b"]
	assert.Equal(t, "2", b.ProviderRaw[contracts.VMInfoCurrentVCPUsKey], "no current attribute: the count")
	assert.Equal(t, "2048", b.ProviderRaw[contracts.VMInfoCurrentMemoryMiBKey], "no <currentMemory>: <memory>")
}

// TestClustered_ListVMsOwnerFilter_UnreadableCandidateIsReported: a candidate
// the host lists but whose definition cannot be read is returned with
// owner_stamp_state "unreadable" — never skipped, so the manager holds on it —
// and its host is not reported unreachable.
func TestClustered_ListVMsOwnerFilter_UnreadableCandidateIsReported(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "team-a.web", uuid: uuidWebA, owner: ownerPrev}},
	})
	fx.write("host-a", "fail-dumpxml", "")
	p := clusterOf(t, []string{"host-a"})

	list, err := p.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	require.Len(t, list.VMs, 1)
	assert.Equal(t, contracts.OwnerStampUnreadable, list.VMs[0].ProviderRaw[contracts.VMInfoOwnerStampStateKey])
	assert.Empty(t, list.VMs[0].OwnerNamespace)
	assert.Empty(t, list.UnreachableHostIDs)
}

// TestClustered_ListVMsOwnerFilter_HostDownMidReadIsUnreachable: a host whose
// libvirtd stops answering while a candidate is read is unknown, not a host
// without a candidate.
func TestClustered_ListVMsOwnerFilter_HostDownMidReadIsUnreachable(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "team-a.web", uuid: uuidWebA, owner: ownerPrev}},
		"host-b": {},
	})
	installHostDownVirsh(t)
	fx.write("host-a", "down-dumpxml", "")
	p := clusterOf(t, []string{"host-a", "host-b"})

	list, err := p.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	assert.Empty(t, list.VMs)
	assert.Equal(t, []string{"host-a"}, list.UnreachableHostIDs)
	assert.True(t, list.OwnerFilterApplied)
}

// installHostDownVirsh replaces the fixture's fake virsh with one where a
// down-<cmd> file makes that command fail as a host whose libvirtd cannot be
// reached (a transport failure, isHostTransportFailure).
func installHostDownVirsh(t *testing.T) {
	t.Helper()
	script := strings.Replace(listFakeVirsh, `fail() { `,
		`fail() { if [ -f "$d/down-$1" ]; then echo "error: failed to connect to the hypervisor" >&2; exit 1; fi; `, 1)
	require.NotEqual(t, listFakeVirsh, script)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestClustered_ListVMsOwnerFilter_InvalidFilterIsRefused: an incomplete
// filter, or a namespace or name Kubernetes would not admit (they reach virsh
// arguments), is INVALID_ARGUMENT, and no host is contacted.
func TestClustered_ListVMsOwnerFilter_InvalidFilterIsRefused(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{"host-a": {{name: "team-a.web", uuid: uuidWebA}}})
	p := clusterOf(t, []string{"host-a"})
	for _, req := range []*providerv1.ListVMsRequest{
		{OwnerNamespace: "team-a"},
		{OwnerName: "web"},
		{OwnerNamespace: "Team_A", OwnerName: "web"},
		{OwnerNamespace: "team-a", OwnerName: "--web;rm"},
	} {
		_, err := NewServer(p).ListVMs(context.Background(), req)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%+v: %v", req, err)
	}
	assert.Empty(t, fx.calls(), "an invalid filter contacts no host")
}

// TestClustered_ListVMsOwnerFilter_OverGRPCKeepsTheBreakerClosed: through the
// manager's transport client, with its breaker at a failure threshold of 1,
// the filtered listing with an unreachable host succeeds, is marked, and
// leaves the breaker closed.
func TestClustered_ListVMsOwnerFilter_OverGRPCKeepsTheBreakerClosed(t *testing.T) {
	newListFixture(t, map[string][]listDomain{
		"host-a": {{name: "team-a.web", uuid: uuidWebA, owner: ownerTeamA}},
		"host-b": {},
	})
	p := clusterOf(t, []string{"host-a", "host-b"}, "host-b")
	cb := resilience.NewCircuitBreaker("a62", "libvirt", "clustered", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
	})
	c := startBreakerGRPC(t, p, cb)

	for i := 0; i < 3; i++ {
		list, err := c.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
		require.NoError(t, err)
		assert.True(t, list.OwnerFilterApplied)
		assert.Equal(t, []string{"host-b"}, list.UnreachableHostIDs)
		require.Len(t, list.VMs, 1)
		assert.Equal(t, []string{ownerTeamA.UID}, contracts.OwnerUIDs(list.VMs[0]))
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState())

	caps, err := c.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.True(t, caps.SupportsListOwnerFilter)
}

// TestClustered_ListVMsOwnerFilter_BusyFailsClosed (security review of A6.2,
// item 5): at most ownerListConcurrency owner-filtered listings run at once
// per provider process; one that gets no slot in time contacts no host and
// fails closed as RESOURCE_EXHAUSTED, which the manager's circuit breaker
// never counts. Once a slot frees, the listing runs.
func TestClustered_ListVMsOwnerFilter_BusyFailsClosed(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{"host-a": {{name: "team-a.web", uuid: uuidWebA, owner: ownerPrev}}})
	p := clusterOf(t, []string{"host-a"})
	p.ownerListSlotWait = 50 * time.Millisecond
	require.NoError(t, p.ownerListSemaphore().Acquire(context.Background(), ownerListConcurrency))

	_, err := NewServer(p).ListVMs(context.Background(), ownerFilterTeamAWeb)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err), "%v", err)
	assert.Empty(t, fx.calls(), "a listing without a slot contacts no host")

	cb := resilience.NewCircuitBreaker("a62-busy", "libvirt", "clustered", &resilience.Config{
		FailureThreshold: 1, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1,
	})
	c := startBreakerGRPC(t, p, cb)
	for i := 0; i < 3; i++ {
		_, err := c.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
		require.Error(t, err)
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "a busy provider never trips the breaker")

	p.ownerListSemaphore().Release(ownerListConcurrency)
	list, err := c.ListVMsForOwner(context.Background(), contracts.OwnerFilter{Namespace: "team-a", Name: "web"})
	require.NoError(t, err)
	assert.True(t, list.OwnerFilterApplied)
	require.Len(t, list.VMs, 1)
}

// TestCapabilities_ListOwnerFilterIsClusteredOnly: a clustered provider
// advertises supports_list_owner_filter; a single-host provider does not.
func TestCapabilities_ListOwnerFilterIsClusteredOnly(t *testing.T) {
	caps, err := NewServer(clusterOf(t, []string{"host-a"})).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsListOwnerFilter())

	single := &Provider{virshProvider: localHostVP("single")}
	caps, err = NewServer(single).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.False(t, caps.GetSupportsListOwnerFilter())
}

// TestSingleHost_ListVMs_IgnoresOwnerFilter: a single-host provider ignores
// the owner filter — every golden ListVMs scenario, re-run WITH a filter,
// produces exactly the pinned command sequence, response (no
// owner_filter_applied) and error of testdata/single_host_listvms.golden.json.
func TestSingleHost_ListVMs_IgnoresOwnerFilter(t *testing.T) {
	raw, err := os.ReadFile(listGoldenFile)
	require.NoError(t, err)
	want := map[string]scdResult{}
	require.NoError(t, json.Unmarshal(raw, &want))

	scenarios := singleHostListScenarios()
	require.Len(t, scenarios, len(want))
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			fx := newSCDFixture(t, sc.domains)
			listPath := filepath.Join(fx.dir, "single", "list.txt")
			if sc.list == "" {
				require.NoError(t, os.Remove(listPath))
			} else {
				require.NoError(t, os.WriteFile(listPath, []byte(sc.list), 0o600))
			}
			if sc.setup != nil {
				sc.setup(fx)
			}
			require.False(t, fx.p.clustered())
			res := scdResult{}
			resp, err := NewServer(fx.p).ListVMs(context.Background(), ownerFilterTeamAWeb)
			if err != nil {
				res.Err = fx.normalize(err.Error())
			}
			if resp != nil {
				assert.False(t, resp.GetOwnerFilterApplied(), "single-host never marks its answer")
				b, merr := json.Marshal(resp)
				require.NoError(t, merr)
				res.Resp = fx.normalize(string(b))
			}
			res.Calls = fx.calls()
			w := want[sc.name]
			assert.Equal(t, w.Calls, res.Calls, "the single-host command sequence changed")
			assert.Equal(t, w.Resp, res.Resp, "the single-host response changed")
			assert.Equal(t, w.Err, res.Err, "the single-host error changed")
		})
	}
}
