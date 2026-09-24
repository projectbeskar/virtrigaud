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
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum A slice 2: Power and
// Reconfigure on a clustered provider are routed to the leased host
// (withHostConn), OWNER-CHECKED before anything is changed, and act on the
// checked domain by its UUID — including the online disk grow's guest-agent
// filesystem grow, which must run on the leased host. The fake virsh routes on
// the per-host local URI (qemu:///<host>), so the production cores run
// unmodified; see opsFakeVirsh.

// routingDomainUUID is the UUID routingDomainXML gives every domain.
const routingDomainUUID = "11111111-2222-4333-8444-555555555555"

// clusteredOpsFixture seeds host-a (empty) and host-b with domain "web" owned
// by ownerTeamA, and returns the clustered provider over them.
func clusteredOpsFixture(t *testing.T) (*routingFixture, *Provider) {
	t.Helper()
	t.Cleanup(func() { _ = os.Remove("/tmp/" + routingDomainUUID + "-sync.xml") })
	fx := newOpsFixture(t, map[string]map[string]string{
		"host-a": {},
		"host-b": {"web": routingDomainXML("web", ownerTeamA)},
	})
	p, _, _ := routedCluster(t)
	return fx, p
}

// webOnHostB is the routed reference the operator sends for "web".
var webOnHostB = contracts.VMRef{ID: "web", HostID: "host-b", Owner: ownerTeamA}

// ownerCheckCalls is the read-only ownership check every routed Power and
// Reconfigure starts with.
var ownerCheckCalls = []string{"host-b list --all", "host-b dumpxml web"}

// mutatingVerbs are the virsh subcommands that change a domain or its disks.
var mutatingVerbs = []string{"start", "destroy", "shutdown", "setvcpus", "setmem", "setmaxmem",
	"vol-resize", "blockresize", "define", "undefine", "qemu-agent-command"}

// assertNothingChanged fails if any call mutates a domain, its disks, or the
// guest, or touches a host file.
func assertNothingChanged(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		fields := strings.Fields(c)
		require.GreaterOrEqual(t, len(fields), 2, c)
		assert.NotContains(t, mutatingVerbs, fields[1], "a domain this VM does not own must never be changed: %q", c)
		assert.False(t, strings.HasPrefix(c, "local "), "no host file may be touched: %q", c)
	}
}

// ─── routed Power ─────────────────────────────────────────────────────────────

func TestClustered_Power_RoutedToLeasedHostOnTheCheckedDomain(t *testing.T) {
	uuid := routingDomainUUID
	sync := []string{
		"host-b dumpxml " + uuid,
		"local define /tmp/" + uuid + "-sync.xml",
		"local rm -f /tmp/" + uuid + "-sync.xml",
	}
	cases := map[contracts.PowerOp][]string{
		contracts.PowerOpOn:               append([]string{"host-b start " + uuid}, sync...),
		contracts.PowerOpOff:              {"host-b destroy " + uuid},
		contracts.PowerOpReboot:           append([]string{"host-b destroy " + uuid, "host-b start " + uuid}, sync...),
		contracts.PowerOpShutdownGraceful: {"host-b shutdown " + uuid},
	}
	for op, want := range cases {
		t.Run(string(op), func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			_, err := p.Power(context.Background(), webOnHostB, op)
			require.NoError(t, err)
			assert.Equal(t, append(append([]string{}, ownerCheckCalls...), want...), fx.calls(),
				"the owner check runs first, then the op on the checked domain's UUID, all on the bound host")
			assert.Zero(t, p.virshProvider.unroutableHits.Load(), "nothing reached the single-host placeholder")
		})
	}

	t.Run("graceful shutdown falls back to destroy on the same domain", func(t *testing.T) {
		fx, p := clusteredOpsFixture(t)
		fx.script("host-b", "fail-shutdown", "")
		_, err := p.Power(context.Background(), webOnHostB, contracts.PowerOpShutdownGraceful)
		require.NoError(t, err)
		assert.Equal(t, append(append([]string{}, ownerCheckCalls...), "host-b shutdown "+uuid, "host-b destroy "+uuid), fx.calls())
	})
}

func TestClustered_Power_ReleasesTheLease(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
	p, reg, closes := routedCluster(t)
	_, err := p.Power(context.Background(), webOnHostB, contracts.PowerOpOff)
	require.NoError(t, err)
	assert.Zero(t, closes["host-b"].Load())
	reg.Evict("host-b")
	assert.EqualValues(t, 1, closes["host-b"].Load(), "a leaked lease would keep the connection open")
	assert.Zero(t, closes["host-a"].Load(), "a host the VM is not bound to is never dialed")
}

// ─── routed Reconfigure ───────────────────────────────────────────────────────

// reconfigureReads is the domstate + dominfo (with its enrichment reads) the
// Reconfigure core starts with, addressed to the checked domain's UUID.
func reconfigureReads(uuid string) []string {
	return []string{
		"host-b domstate " + uuid,
		"host-b dominfo " + uuid,
		"host-b dommemstat " + uuid,
		"host-b cpu-stats " + uuid,
		"host-b domiflist " + uuid,
		"host-b domblklist " + uuid,
		"host-b domblkstat " + uuid + " vda",
		"host-b guestinfo " + uuid + " --os",
		"host-b guestinfo " + uuid + " --hostname",
	}
}

func TestClustered_Reconfigure_RoutedToLeasedHostOnTheCheckedDomain(t *testing.T) {
	uuid := routingDomainUUID
	agent := func(execArgs string) []string {
		return []string{
			`host-b qemu-agent-command --timeout 3 ` + uuid + ` {"execute":"guest-ping"}`,
			`host-b qemu-agent-command --timeout 3 ` + uuid + ` {"execute":"guest-exec","arguments":{"path":"/bin/sh","arg":["-c","` + execArgs + `"],"capture-output":true}}`,
			`host-b qemu-agent-command --timeout 3 ` + uuid + ` {"execute":"guest-exec-status","arguments":{"pid":7}}`,
		}
	}
	cases := []struct {
		name    string
		running bool
		desired contracts.CreateRequest
		want    []string
	}{
		{
			name: "offline CPU, memory and disk", desired: reconfigureTo(4, 4096, 20),
			want: []string{
				"host-b setvcpus " + uuid + " 4 --config",
				"host-b setmem " + uuid + " 4194304K --config",
				"host-b setmaxmem " + uuid + " 4194304K --config",
				"host-b vol-resize web-disk 20G --pool default",
			},
		},
		{
			name: "online CPU and memory", running: true, desired: reconfigureTo(4, 4096, 0),
			want: []string{
				"host-b setvcpus " + uuid + " 4 --live",
				"host-b setmem " + uuid + " 4194304K --live",
			},
		},
		{
			name: "online disk grow with the in-guest filesystem grow on the leased host", running: true, desired: reconfigureTo(0, 0, 20),
			want: append(append(append([]string{
				"host-b domblklist " + uuid,
				"host-b domblkinfo " + uuid + " vda",
				"host-b vol-resize web-disk 20G --pool default",
				"host-b blockresize " + uuid + " vda 20G",
				`host-b qemu-agent-command --timeout 3 ` + uuid + ` {"execute":"guest-ping"}`,
			}, agent("growpart /dev/vda 1")...), agent("resize2fs /dev/vda1")...), agent("xfs_growfs /")...),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			if tc.running {
				fx.script("host-b", "state", "running\n")
			}
			_, err := p.Reconfigure(context.Background(), webOnHostB, tc.desired)
			require.NoError(t, err)

			want := append(append(append([]string{}, ownerCheckCalls...), reconfigureReads(uuid)...), tc.want...)
			assert.Equal(t, want, fx.calls(), "every read, change and guest-agent call ran on the bound host, on the checked domain")
			assert.Zero(t, p.virshProvider.unroutableHits.Load(), "nothing (not even the guest agent) reached the single-host placeholder")
		})
	}
}

// ─── ownership ────────────────────────────────────────────────────────────────

// TestClustered_PowerAndReconfigure_OwnerChecked is slice 2's safety property:
// a routed Power or Reconfigure changes a domain only when its owner stamp is
// the requester's. Anything else is NotFound, and only the read-only ownership
// check ever ran.
func TestClustered_PowerAndReconfigure_OwnerChecked(t *testing.T) {
	cases := []struct {
		name      string
		domainXML string // "" = no such domain
		owner     contracts.ObjectIdentity
	}{
		{"owned by another tenant", routingDomainXML("web", ownerTeamA), ownerTeamB},
		{"unstamped (legacy / foreign)", routingDomainXML("web", contracts.ObjectIdentity{}), ownerTeamA},
		{"request without owner", routingDomainXML("web", ownerTeamA), contracts.ObjectIdentity{}},
		{"unreadable stamp", "<domain><name>web", ownerTeamA},
		{"absent", "", ownerTeamA},
	}
	calls := map[string]func(p *Provider, vm contracts.VMRef) error{
		"Power": func(p *Provider, vm contracts.VMRef) error {
			_, err := p.Power(context.Background(), vm, contracts.PowerOpOn)
			return err
		},
		"Reconfigure": func(p *Provider, vm contracts.VMRef) error {
			_, err := p.Reconfigure(context.Background(), vm, reconfigureTo(4, 4096, 20))
			return err
		},
	}
	for _, tc := range cases {
		for rpc, call := range calls {
			t.Run(rpc+"/"+tc.name, func(t *testing.T) {
				domains := map[string]string{}
				if tc.domainXML != "" {
					domains["web"] = tc.domainXML
				}
				fx := newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": domains})
				fx.script("host-b", "state", "running\n")
				p, _, _ := routedCluster(t)

				err := call(p, contracts.VMRef{ID: "web", HostID: "host-b", Owner: tc.owner})
				require.Error(t, err)
				assert.True(t, contracts.IsNotFound(err), "a domain this VM does not own is reported not-found: %v", err)
				assert.NotContains(t, err.Error(), ownerTeamA.UID, "the message never discloses the other owner")
				assertNothingChanged(t, fx.calls())
				assert.Subset(t, ownerCheckCalls, fx.calls(), "only the read-only ownership check ran")
			})
		}
	}
}

// TestClustered_PowerAndReconfigure_ActOnlyOnTheCheckedDomain closes the
// window between the ownership check and the change (Delete included): the change addresses the
// checked domain's UUID, never its name, so a domain that is no longer the one
// whose stamp was checked is not acted on — the command fails instead.
func TestClustered_PowerAndReconfigure_ActOnlyOnTheCheckedDomain(t *testing.T) {
	fx, p := clusteredOpsFixture(t)
	// The name still resolves (to a document stamped for ownerTeamA), but the
	// domain with the checked UUID is gone.
	require.NoError(t, os.Remove(filepath.Join(fx.dir, "host-b", "dom-"+routingDomainUUID+".xml")))

	_, err := p.Power(context.Background(), webOnHostB, contracts.PowerOpOn)
	require.Error(t, err)
	_, err = p.Reconfigure(context.Background(), webOnHostB, reconfigureTo(4, 4096, 20))
	require.Error(t, err)
	_, err = p.Delete(context.Background(), webOnHostB)
	require.Error(t, err, "the undefine of a domain that is no longer the checked one fails")

	calls := fx.calls()
	assert.Contains(t, calls, "host-b start "+routingDomainUUID, "the start addressed the checked UUID")
	assert.Contains(t, calls, "host-b domstate "+routingDomainUUID, "the reconfigure read addressed the checked UUID")
	assert.Contains(t, calls, "host-b undefine "+routingDomainUUID, "the delete addressed the checked UUID")
	for _, c := range calls {
		f := strings.Fields(c)
		if len(f) >= 3 && slices.Contains(mutatingVerbs, f[1]) {
			assert.NotEqual(t, "web", f[2], "a change never addresses the domain by name: %q", c)
		}
	}
	for _, verb := range []string{"setvcpus", "setmem", "vol-resize", "blockresize", "define"} {
		for _, c := range calls {
			assert.NotContains(t, c, " "+verb+" ", "nothing was changed once the checked domain could not be found")
		}
	}
}

// TestClustered_PowerAndReconfigure_NoUUIDIsNotActedOn: an owned domain (also for Delete) whose
// definition carries no UUID cannot be pinned, so nothing is changed and the
// error is retryable (not NotFound — the VM may well exist).
func TestClustered_PowerAndReconfigure_NoUUIDIsNotActedOn(t *testing.T) {
	noUUID := strings.Replace(routingDomainXML("web", ownerTeamA), "  <uuid>"+routingDomainUUID+"</uuid>\n", "", 1)
	require.NotContains(t, noUUID, "<uuid>")
	fx := newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": noUUID}})
	p, _, _ := routedCluster(t)

	_, perr := p.Power(context.Background(), webOnHostB, contracts.PowerOpOn)
	_, rerr := p.Reconfigure(context.Background(), webOnHostB, reconfigureTo(4, 0, 0))
	_, derr := p.Delete(context.Background(), webOnHostB)
	for name, err := range map[string]error{"Power": perr, "Reconfigure": rerr, "Delete": derr} {
		require.Error(t, err, name)
		assert.False(t, contracts.IsNotFound(err), name)
		assert.True(t, contracts.IsRetryable(err), "%s: %v", name, err)
	}
	assertNothingChanged(t, fx.calls())
}

// ─── wire errors ──────────────────────────────────────────────────────────────

func TestClustered_PowerAndReconfigure_RoutingErrors(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
	p, _, _ := routedCluster(t)
	s := NewServer(p)
	ctx := context.Background()
	owner := ownerFromIdentity(ownerTeamA)
	desired, err := json.Marshal(reconfigureTo(4, 4096, 0))
	require.NoError(t, err)

	power := func(host string) error {
		_, e := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: host, Owner: owner})
		return e
	}
	reconfigure := func(host string) error {
		_, e := s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: host, Owner: owner})
		return e
	}

	for _, host := range []string{"", "   "} {
		assert.Equal(t, codes.InvalidArgument, status.Code(power(host)), "an empty target_host_id never defaults to a host")
		assert.Equal(t, codes.InvalidArgument, status.Code(reconfigure(host)))
	}
	for name, err := range map[string]error{"Power": power("host-zzz"), "Reconfigure": reconfigure("host-zzz")} {
		st, ok := status.FromError(err)
		require.True(t, ok, name)
		assert.Equal(t, codes.Unavailable, st.Code(), name)
		assert.True(t, hasHostUnavailableInfo(st), "%s must mark the unavailability as host-scoped", name)
	}

	// An unsupported power operation is refused before any host is leased.
	_, err = s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_UNSPECIFIED, TargetHostId: "host-b", Owner: owner})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = p.Power(ctx, webOnHostB, contracts.PowerOp("Hibernate"))
	assert.True(t, contracts.IsInvalidSpec(err), "%v", err)

	// A malformed desired state is the caller's error, not a host's.
	_, err = s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: "{", TargetHostId: "host-b", Owner: owner})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// A domain this VM does not own is NotFound on the wire (the manager's A4).
	_, err = s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_OFF, TargetHostId: "host-b",
		Owner: ownerFromIdentity(ownerTeamB)})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: "host-b"})
	assert.Equal(t, codes.NotFound, status.Code(err), "a request without an owner authorizes nothing")

	assert.Zero(t, p.virshProvider.unroutableHits.Load())
}

// TestClustered_Server_PowerAndReconfigureSucceedOnOwnedDomain drives both
// RPCs through the gRPC Server with the owner on the wire.
func TestClustered_Server_PowerAndReconfigureSucceedOnOwnedDomain(t *testing.T) {
	fx, p := clusteredOpsFixture(t)
	s := NewServer(p)
	owner := ownerFromIdentity(ownerTeamA)
	desired, err := json.Marshal(reconfigureTo(4, 0, 0))
	require.NoError(t, err)

	_, err = s.Power(context.Background(), &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_OFF, TargetHostId: "host-b", Owner: owner})
	require.NoError(t, err)
	_, err = s.Reconfigure(context.Background(), &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: "host-b", Owner: owner})
	require.NoError(t, err)

	calls := fx.calls()
	assert.Contains(t, calls, "host-b destroy "+routingDomainUUID)
	assert.Contains(t, calls, "host-b setvcpus "+routingDomainUUID+" 4 --config")
	assert.Equal(t, map[string]bool{"host-b": true}, hostsOf(calls))
}
