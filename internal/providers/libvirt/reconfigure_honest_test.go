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
	stderrors "errors"
	"runtime"
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

// These tests pin the honest Reconfigure result: every CPU, memory and disk
// change a request asks for is either applied to the running domain AND its
// persistent definition, or applied to the persistent definition only and
// reported as restart-required, or the call fails. It never reports success
// for a change in none of these states (the false-success cases the reviewers
// found: a failed live change with nothing persisted, a failed --config change,
// an ignored setmaxmem error, a failed offline disk resize, a balloon-only
// memory shrink, and a config-only change to a paused or suspended domain).

// reconfigureVerbs are the virsh subcommands that change a domain's size.
var reconfigureVerbs = []string{"setvcpus", "setmem", "setmaxmem", "vol-resize", "blockresize"}

// sizeChanges returns the calls among calls that change a domain's CPU,
// memory or disk, in order, without their host tag.
func sizeChanges(calls []string) []string {
	var out []string
	for _, c := range calls {
		f := strings.Fields(c)
		if len(f) >= 2 && slices.Contains(reconfigureVerbs, f[1]) {
			out = append(out, strings.Join(f[1:], " "))
		}
	}
	return out
}

// honestCase is one single-host Reconfigure: the fake host's scripted state,
// the request, and what it must do and answer.
type honestCase struct {
	name    string
	script  map[string]string // per-host fake behavior files (see opsFakeVirsh)
	desired contracts.CreateRequest
	// changes are the size-changing calls, in order (sizeChanges).
	changes []string
	// restart is the expected RestartRequired; wantErr an expected failure.
	restart bool
	wantErr bool
}

// running scripts a running domain on top of more.
func running(more map[string]string) map[string]string {
	out := map[string]string{"state": "running\n", "id": "7"}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// honestSingleHostCases is the single-host matrix. The domain is
// opsDomainName; the fake's defaults are 2 vCPUs (max 2) and 2 GiB (max 2
// GiB), live and persistent, and a 10 GiB disk.
func honestSingleHostCases() []honestCase {
	h := opsDomainName
	return []honestCase{
		{
			name:    "live CPU grow within the vCPU maximum is applied live and persisted",
			script:  running(map[string]string{"cfg-maxvcpus": "8"}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config", "setvcpus " + h + " 4 --live"},
		},
		{
			name:    "live CPU grow beyond the running maximum is persisted and needs a restart",
			script:  running(map[string]string{"fail-setvcpus-live": ""}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config --maximum", "setvcpus " + h + " 4 --config", "setvcpus " + h + " 4 --live"},
			restart: true,
		},
		{
			name: "live CPU shrink without hotpluggable vCPUs is persisted and needs a restart",
			script: running(map[string]string{"vcpus": "4", "cfg-vcpus": "4", "cfg-maxvcpus": "4",
				"fail-setvcpus-live": ""}),
			desired: reconfigureTo(2, 0, 0),
			changes: []string{"setvcpus " + h + " 2 --config", "setvcpus " + h + " 2 --live"},
			restart: true,
		},
		{
			name:    "a failed --config vCPU change on a running domain fails, and nothing is tried live",
			script:  running(map[string]string{"cfg-maxvcpus": "8", "fail-setvcpus-config": ""}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config"},
			wantErr: true,
		},
		{
			name:    "a failed --config vCPU change on a stopped domain fails",
			script:  map[string]string{"cfg-maxvcpus": "8", "fail-setvcpus-config": ""},
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config"},
			wantErr: true,
		},
		{
			name:    "a vCPU count only the running domain has is persisted (config drift)",
			script:  running(map[string]string{"vcpus": "4", "cfg-maxvcpus": "8"}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config"},
		},
		{
			name:    "live memory grow within the balloon maximum is applied live and persisted",
			script:  running(map[string]string{"maxmem": "8388608", "cfg-maxmem": "8388608"}),
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmem " + h + " 4194304K --config", "setmem " + h + " 4194304K --live"},
		},
		{
			name: "live memory grow within the maximum whose balloon change fails needs a restart",
			script: running(map[string]string{"maxmem": "8388608", "cfg-maxmem": "8388608",
				"fail-setmem-live": ""}),
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmem " + h + " 4194304K --config", "setmem " + h + " 4194304K --live"},
			restart: true,
		},
		{
			name:    "live memory grow beyond the maximum is persisted and needs a restart",
			script:  running(nil),
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmaxmem " + h + " 4194304K --config", "setmem " + h + " 4194304K --config"},
			restart: true,
		},
		{
			name: "live memory shrink is persisted (maximum lowered) and needs a restart, the balloon is not trusted",
			script: running(map[string]string{"maxmem": "4194304", "usedmem": "4194304",
				"cfg-mem": "4194304", "cfg-maxmem": "4194304"}),
			desired: reconfigureTo(0, 2048, 0),
			changes: []string{"setmaxmem " + h + " 2097152K --config", "setmem " + h + " 2097152K --config"},
			restart: true,
		},
		{
			name: "live memory shrink of a hot-add VM lowers its balloon maximum and needs a restart",
			script: running(map[string]string{"maxmem": "8388608", "usedmem": "4194304",
				"cfg-mem": "4194304", "cfg-maxmem": "8388608"}),
			desired: reconfigureTo(0, 2048, 0),
			changes: []string{"setmaxmem " + h + " 2097152K --config", "setmem " + h + " 2097152K --config"},
			restart: true,
		},
		{
			name:    "a failed setmaxmem fails the call",
			script:  map[string]string{"fail-setmaxmem": ""},
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmaxmem " + h + " 4194304K --config"},
			wantErr: true,
		},
		{
			name:    "a failed setmaxmem of an offline shrink fails the call",
			script:  map[string]string{"cfg-mem": "4194304", "cfg-maxmem": "4194304", "fail-setmaxmem": ""},
			desired: reconfigureTo(0, 2048, 0),
			changes: []string{"setmaxmem " + h + " 2097152K --config"},
			wantErr: true,
		},
		{
			name:    "a failed --config memory change fails the call",
			script:  map[string]string{"fail-setmem-config": ""},
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmaxmem " + h + " 4194304K --config", "setmem " + h + " 4194304K --config"},
			wantErr: true,
		},
		{
			name:    "a stopped domain gets every change persistently: the disk first, the vCPU maximum raised first",
			desired: reconfigureTo(4, 4096, 20),
			changes: []string{
				"vol-resize " + h + "-disk 20G --pool default",
				"setvcpus " + h + " 4 --config --maximum", "setvcpus " + h + " 4 --config",
				"setmaxmem " + h + " 4194304K --config", "setmem " + h + " 4194304K --config",
			},
		},
		{
			// Review H1: a disk grow the host refuses fails the call before the
			// CPU or memory of the domain changes, so no grow is left applied.
			name:    "a failed disk grow changes no CPU or memory (stopped)",
			script:  map[string]string{"fail-vol-resize": ""},
			desired: reconfigureTo(4, 4096, 20),
			changes: []string{"vol-resize " + h + "-disk 20G --pool default"},
			wantErr: true,
		},
		{
			name:    "a failed disk grow changes no CPU or memory (running)",
			script:  running(map[string]string{"cfg-maxvcpus": "8", "maxmem": "8388608", "cfg-maxmem": "8388608", "fail-blockresize": ""}),
			desired: reconfigureTo(4, 4096, 20),
			changes: []string{"vol-resize " + h + "-disk 20G --pool default", "blockresize " + h + " vda 20G"},
			wantErr: true,
		},
		{
			name:    "a stopped domain's memory shrink lowers the maximum first",
			script:  map[string]string{"cfg-mem": "4194304", "cfg-maxmem": "4194304"},
			desired: reconfigureTo(0, 2048, 0),
			changes: []string{"setmaxmem " + h + " 2097152K --config", "setmem " + h + " 2097152K --config"},
		},
		{
			name:    "a failed offline disk resize fails the call",
			script:  map[string]string{"fail-vol-resize": ""},
			desired: reconfigureTo(0, 0, 20),
			changes: []string{"vol-resize " + h + "-disk 20G --pool default"},
			wantErr: true,
		},
		{
			name:    "an offline disk already at the requested size is not resized",
			script:  map[string]string{"capacity": "21474836480"},
			desired: reconfigureTo(0, 0, 20),
		},
		{
			name:    "a crashed domain that is inactive takes the persistent path",
			script:  map[string]string{"state": "crashed\n", "cfg-maxvcpus": "8"},
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + h + " 4 --config"},
		},
		{
			name:    "nothing to change",
			script:  running(nil),
			desired: reconfigureTo(2, 2048, 0),
		},
	}
}

// runSingleHostReconfigure runs one single-host Reconfigure on a fresh fixture.
func runSingleHostReconfigure(t *testing.T, script map[string]string, desired contracts.CreateRequest) (contracts.ReconfigureResult, []string, error) {
	t.Helper()
	fx := newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	for name, content := range script {
		fx.script("single", name, content)
	}
	p := &Provider{virshProvider: localHostVP("single")}
	res, err := p.Reconfigure(context.Background(), contracts.VMRef{ID: opsDomainName}, desired)
	return res, fx.calls(), err
}

func TestReconfigure_SingleHost_HonestResult(t *testing.T) {
	for _, tc := range honestSingleHostCases() {
		t.Run(tc.name, func(t *testing.T) {
			res, calls, err := runSingleHostReconfigure(t, tc.script, tc.desired)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, contracts.IsRetryable(err), "a host-side failure is retryable: %v", err)
				assertTenantSafe(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.restart, res.RestartRequired, "restart required")
			assert.Equal(t, tc.changes, sizeChanges(calls), "the size-changing calls")
		})
	}
}

// assertTenantSafe fails if err's text — which reaches the VirtualMachine's
// status — carries the virsh command line, a connection URI or a host path.
func assertTenantSafe(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	for _, leak := range []string{"virsh", "qemu:", "ssh", "/var/lib", "/tmp", "stderr="} {
		assert.NotContains(t, msg, leak, "the requester-facing error must not carry %q: %s", leak, msg)
	}
}

// TestReconfigure_ActiveButNotRunningDomainIsRefused (review R1): a paused,
// PM-suspended, shutting-down, crashed-but-active or unknown-state domain is
// still active, so a --config change would not be what it runs — and a guest
// suspended to RAM wakes at its old size. Nothing is changed and the call
// fails retryably; it is never reported as applied.
func TestReconfigure_ActiveButNotRunningDomainIsRefused(t *testing.T) {
	shrink := reconfigureTo(1, 1024, 0)
	for _, state := range []string{"paused", "pmsuspended", "in shutdown", "crashed", "no state", "something-new"} {
		t.Run(state, func(t *testing.T) {
			res, calls, err := runSingleHostReconfigure(t, map[string]string{"state": state + "\n", "id": "7"}, shrink)
			require.Error(t, err)
			assert.True(t, contracts.IsRetryable(err), "%v", err)
			assert.False(t, res.RestartRequired)
			assert.Empty(t, sizeChanges(calls), "a domain that is active but not running is never changed")
			assertTenantSafe(t, err)
		})
	}
}

// TestReconfigure_FailureKeepsHostClassification: the requester-facing text
// drops the virsh detail, but the error chain still carries the *VirshError,
// so a host that could not be reached is still classified as such.
func TestReconfigure_FailureKeepsHostClassification(t *testing.T) {
	_, _, err := runSingleHostReconfigure(t, map[string]string{"fail-setvcpus-config": "", "cfg-maxvcpus": "8"}, reconfigureTo(4, 0, 0))
	require.Error(t, err)
	var ve *VirshError
	assert.True(t, stderrors.As(err, &ve), "the virsh failure stays in the chain for classification")
	assertTenantSafe(t, err)
}

// TestReconfigure_Server_ReportsRestartRequired: the gRPC server puts the
// provider's restart-required answer on the wire (single-host).
func TestReconfigure_Server_ReportsRestartRequired(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	for name, content := range running(map[string]string{"fail-setvcpus-live": ""}) {
		fx.script("single", name, content)
	}
	s := NewServer(&Provider{virshProvider: localHostVP("single")})
	desired, err := json.Marshal(reconfigureTo(4, 0, 0))
	require.NoError(t, err)
	resp, err := s.Reconfigure(context.Background(), &providerv1.ReconfigureRequest{Id: opsDomainName, DesiredJson: string(desired)})
	require.NoError(t, err)
	assert.True(t, resp.GetRestartRequired())
}

// ─── clustered (routed) ───────────────────────────────────────────────────────

// TestClustered_Reconfigure_HonestResult runs the cases whose routed path
// differs — the checked domain addressed by UUID, the error class on the wire
// — through the gRPC server of a clustered provider.
func TestClustered_Reconfigure_HonestResult(t *testing.T) {
	uuid := routingDomainUUID
	owner := ownerFromIdentity(ownerTeamA)
	cases := []struct {
		name    string
		script  map[string]string
		desired contracts.CreateRequest
		changes []string
		restart bool
		// failed: the call fails with VM_OPERATION_FAILED (never HOST_UNAVAILABLE:
		// the host answered).
		failed bool
	}{
		{
			name:    "live CPU grow applied live and persisted",
			script:  running(map[string]string{"cfg-maxvcpus": "8"}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + uuid + " 4 --config", "setvcpus " + uuid + " 4 --live"},
		},
		{
			name: "live CPU shrink without hotpluggable vCPUs needs a restart",
			script: running(map[string]string{"vcpus": "4", "cfg-vcpus": "4", "cfg-maxvcpus": "4",
				"fail-setvcpus-live": ""}),
			desired: reconfigureTo(2, 0, 0),
			changes: []string{"setvcpus " + uuid + " 2 --config", "setvcpus " + uuid + " 2 --live"},
			restart: true,
		},
		{
			name:    "live memory shrink needs a restart",
			script:  running(map[string]string{"maxmem": "4194304", "usedmem": "4194304", "cfg-mem": "4194304", "cfg-maxmem": "4194304"}),
			desired: reconfigureTo(0, 2048, 0),
			changes: []string{"setmaxmem " + uuid + " 2097152K --config", "setmem " + uuid + " 2097152K --config"},
			restart: true,
		},
		{
			name:    "a failed --config change is a VM operation failure",
			script:  running(map[string]string{"cfg-maxvcpus": "8", "fail-setvcpus-config": ""}),
			desired: reconfigureTo(4, 0, 0),
			changes: []string{"setvcpus " + uuid + " 4 --config"},
			failed:  true,
		},
		{
			name:    "a failed setmaxmem is a VM operation failure",
			script:  map[string]string{"fail-setmaxmem": ""},
			desired: reconfigureTo(0, 4096, 0),
			changes: []string{"setmaxmem " + uuid + " 4194304K --config"},
			failed:  true,
		},
		{
			name:    "a stopped domain applies persistently, the disk first",
			desired: reconfigureTo(4, 4096, 20),
			changes: []string{
				"vol-resize --vol " + opsDiskPath + " --capacity 20G",
				"setvcpus " + uuid + " 4 --config --maximum", "setvcpus " + uuid + " 4 --config",
				"setmaxmem " + uuid + " 4194304K --config", "setmem " + uuid + " 4194304K --config",
			},
		},
		{
			// Review H1: a disk grow the tenant's VMClass asks for and the host
			// cannot give fails before any CPU or memory change.
			name:    "a failed disk grow changes no CPU or memory",
			script:  map[string]string{"fail-vol-resize": ""},
			desired: reconfigureTo(4, 4096, 20),
			changes: []string{"vol-resize --vol " + opsDiskPath + " --capacity 20G"},
			failed:  true,
		},
		{
			name:    "a PM-suspended domain is refused as a VM operation failure",
			script:  map[string]string{"state": "pmsuspended\n", "id": "7"},
			desired: reconfigureTo(1, 1024, 0),
			failed:  true,
		},
		{
			// A network disk has no host path: a grow it needs cannot be
			// applied, and that is now reported instead of logged.
			name:    "an offline grow of a network disk fails",
			script:  map[string]string{"disktype": "network", "disksource": "pool/web"},
			desired: reconfigureTo(0, 0, 20),
			failed:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			for name, content := range tc.script {
				fx.script("host-b", name, content)
			}
			s := NewServer(p)
			desired, err := json.Marshal(tc.desired)
			require.NoError(t, err)
			resp, err := s.Reconfigure(context.Background(), &providerv1.ReconfigureRequest{
				Id: "web", DesiredJson: string(desired), TargetHostId: "host-b", Owner: owner,
			})
			if tc.failed {
				st, ok := status.FromError(err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, codes.Unknown, st.Code())
				assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "the host answered: never counted against the Provider")
				for _, leak := range []string{"virsh", "qemu:", "/var/lib", "stderr="} {
					assert.NotContains(t, st.Message(), leak)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.restart, resp.GetRestartRequired())
			}
			assert.Equal(t, tc.changes, sizeChanges(fx.calls()))
			assert.Equal(t, map[string]bool{"host-b": true}, hostsOf(fx.calls()), "every call ran on the bound host")
		})
	}
}

// TestDescribe_ReportsMaxMemory (review R2): Describe reports the domain's
// memory maximum (dominfo "Max memory", rounded up to MiB) on the wire, so the
// manager can record a clustered VM's balloon ceiling from the provider.
func TestDescribe_ReportsMaxMemory(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	fx.script("single", "maxmem", "8388609") // 8 GiB + 1 KiB: rounds up
	s := NewServer(&Provider{virshProvider: localHostVP("single")})
	resp, err := s.Describe(context.Background(), &providerv1.DescribeRequest{Id: opsDomainName})
	require.NoError(t, err)
	assert.EqualValues(t, 8193, resp.GetMaxMemoryMib())
}

// TestGenerateDomainXML_ClusteredDisablesGuestSuspend (review R1c): a clustered
// provider's new domain may not suspend to RAM or disk; a single-host domain's
// XML is unchanged (no <pm> element).
func TestGenerateDomainXML_ClusteredDisablesGuestSuspend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: the /dev/kvm probe execs test(1)")
	}
	ctx := context.Background()
	req := contracts.CreateRequest{Name: "web", Owner: ownerTeamA}

	single, err := (&Provider{}).generateDomainXMLWithStorage(ctx, localTestVirshProvider(), req, "web", "/var/lib/libvirt/images/web-disk.qcow2", "")
	require.NoError(t, err)
	assert.NotContains(t, single, "<pm>", "single-host domain XML is unchanged")
	assert.Contains(t, single, "  <on_crash>destroy</on_crash>\n  <devices>\n", "single-host layout is byte-identical around the insertion point")

	clustered, _, _ := routedCluster(t)
	x, err := clustered.generateDomainXMLWithStorage(ctx, localTestVirshProvider(), req, "web", "/var/lib/libvirt/images/web-disk.qcow2", "")
	require.NoError(t, err)
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err, "the clustered domain XML stays well-formed")
	require.NotNil(t, d.PM)
	require.NotNil(t, d.PM.SuspendToMem)
	require.NotNil(t, d.PM.SuspendToDisk)
	assert.Equal(t, "no", d.PM.SuspendToMem.Enabled)
	assert.Equal(t, "no", d.PM.SuspendToDisk.Enabled)
}
