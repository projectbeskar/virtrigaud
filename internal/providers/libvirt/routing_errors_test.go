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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the wire classification of a routed call's failure on a
// clustered provider (ADR-0007 Addendum A, slice 2 security review): a host
// that cannot be reached is HOST_UNAVAILABLE, a per-VM operation that reached
// its host and failed there carries VM_OPERATION_FAILED with the historical
// code and message — neither counts toward the manager's circuit breaker — and
// a provider-level failure still does. Single-host wire errors are unchanged.

// errorInfoReason returns the VirtRigaud ErrorInfo reason on st, or "".
func errorInfoReason(st *status.Status) string {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == contracts.ErrorInfoDomain {
			return info.GetReason()
		}
	}
	return ""
}

func TestClustered_DeadLeasedHostIsHostUnavailable(t *testing.T) {
	for name, marker := range map[string]string{
		"connection dropped (no exit status)": "dead",
		"libvirtd down on the host":           "nolibvirtd",
	} {
		t.Run(name, func(t *testing.T) {
			fx, p := clusteredOpsFixture(t)
			s := NewServer(p)
			ctx := context.Background()
			owner := ownerFromIdentity(ownerTeamA)
			desired, err := json.Marshal(reconfigureTo(4, 0, 0))
			require.NoError(t, err)

			// The first call dials and uses the host; the registry then reuses
			// that connection without a health probe.
			_, err = s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_OFF, TargetHostId: "host-b", Owner: owner})
			require.NoError(t, err)

			fx.script("host-b", marker, "")
			calls := map[string]func() error{
				"Describe": func() error {
					_, e := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
					return e
				},
				"Power": func() error {
					_, e := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-b", Owner: owner})
					return e
				},
				"Reconfigure": func() error {
					_, e := s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: "host-b", Owner: owner})
					return e
				},
				"Delete": func() error {
					_, e := s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
					return e
				},
			}
			for rpc, call := range calls {
				st, ok := status.FromError(call())
				require.True(t, ok, rpc)
				assert.Equal(t, codes.Unavailable, st.Code(), rpc)
				assert.True(t, hasHostUnavailableInfo(st), "%s: a host that died after first use is host-scoped Unavailable: %v", rpc, st.Message())
				assert.Contains(t, st.Message(), `host "host-b"`, rpc)
			}
		})
	}
}

// TestClustered_VMOperationFailureCarriesVMOperationFailed: an operation the
// host ran and rejected keeps the historical code (Unknown), carries the
// VM_OPERATION_FAILED detail, and — since the slice 3 security review — only
// the categorized message: the operation and the host id, never the virsh
// command line or its stderr.
func TestClustered_VMOperationFailureCarriesVMOperationFailed(t *testing.T) {
	fx, p := clusteredOpsFixture(t)
	fx.script("host-b", "fail-start", "")
	fx.script("host-b", "state", "running\n")
	fx.script("host-b", "fail-blockresize", "")
	s := NewServer(p)
	ctx := context.Background()
	owner := ownerFromIdentity(ownerTeamA)

	_, perr := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-b", Owner: owner})
	desired, err := json.Marshal(reconfigureTo(0, 0, 20))
	require.NoError(t, err)
	_, rerr := s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: "host-b", Owner: owner})

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"Power":       {perr, `failed to perform power operation on host "host-b"`},
		"Reconfigure": {rerr, `failed to reconfigure VM on host "host-b"`},
	} {
		st, ok := status.FromError(tc.err)
		require.True(t, ok, name)
		assert.Equal(t, codes.Unknown, st.Code(), name)
		assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), name)
		assert.Equal(t, tc.want, st.Message(), "%s sends only the categorized message", name)
		for _, leak := range []string{"virsh", "stderr", "scripted failure", "/var/lib", "qemu:///"} {
			assert.NotContains(t, st.Message(), leak, "%s leaks no command, stderr or path", name)
		}
	}
}

// TestClustered_CreateFailureOnHostIsClassified: a clustered Create's failure
// on its target host is classified the same way.
func TestClustered_CreateFailureOnHostIsClassified(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {}})
	p, _, _ := routedCluster(t)
	s := NewServer(p)

	p.createOnHostFn = func(context.Context, hostconn.Conn, contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to create VM", &VirshError{Command: "virsh define", ExitCode: 1, Stderr: "error: bad xml"})
	}
	_, err := s.Create(context.Background(), &providerv1.CreateRequest{Name: "web", TargetHostId: "host-b"})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st))
	assert.Equal(t, `failed to create VM on host "host-b"`, st.Message())
	assert.NotContains(t, st.Message(), "bad xml", "no stderr crosses the wire")

	p.createOnHostFn = func(context.Context, hostconn.Conn, contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to list existing domains",
			&VirshError{Command: "virsh list --all", ExitCode: -1, Cause: errors.New("ssh: handshake failed: dial ssh host 10.0.0.5:22: EOF")})
	}
	_, err = s.Create(context.Background(), &providerv1.CreateRequest{Name: "web", TargetHostId: "host-b"})
	st, ok = status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st))
	assert.Equal(t, `create VM: host "host-b" is unreachable`, st.Message())
	assert.NotContains(t, st.Message(), "10.0.0.5", "the host's SSH address is never on the wire")
}

// TestClustered_HostUnavailableSendsNoDialAddress: a host that cannot be
// leased (its dial fails) is reported by id only; the dial error — which
// names the SSH address — stays in the provider's log.
func TestClustered_HostUnavailableSendsNoDialAddress(t *testing.T) {
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{
		{ID: "host-a", Endpoint: "qemu+ssh://virt@10.0.0.5/system"},
	}}
	dial := func(context.Context, hostsecret.Host) (hostconn.Conn, error) {
		return nil, errors.New("dial ssh host 10.0.0.5:22: connection refused")
	}
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	_, err := NewServer(p).Describe(context.Background(), &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-a",
		Owner: ownerFromIdentity(ownerTeamA)})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st))
	assert.Equal(t, `connect to host "host-a"`, st.Message())
}

func TestRoutedRPCError_Classification(t *testing.T) {
	onHost := func(err error) error { return &hostOpError{host: "host-b", err: err} }
	cases := []struct {
		name   string
		err    error
		code   codes.Code
		reason string
	}{
		{"provider-level unavailable still counts", contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil), codes.Unavailable, ""},
		{"error not from a host keeps the historical form", errors.New("boom"), codes.Unknown, ""},
		{"not found on the host", onHost(contracts.NewNotFoundError("gone", nil)), codes.NotFound, ""},
		{"command the host rejected", onHost(contracts.NewRetryableError("x", &VirshError{ExitCode: 1, Stderr: "error: nope"})), codes.Unknown, contracts.VMOperationFailedReason},
		{"host unreachable", onHost(contracts.NewRetryableError("x", &VirshError{ExitCode: -1})), codes.Unavailable, contracts.HostUnavailableReason},
		{"libvirtd down", onHost(&VirshError{ExitCode: 1, Stderr: "error: failed to connect to the hypervisor"}), codes.Unavailable, contracts.HostUnavailableReason},
		{"call context expired is not a host failure", onHost(&VirshError{ExitCode: -1, Cause: context.DeadlineExceeded}), codes.Unknown, contracts.VMOperationFailedReason},
		{"non-virsh failure on the host", onHost(errors.New("resolve primary disk: no disk")), codes.Unknown, contracts.VMOperationFailedReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// status.Convert is what gRPC does with a handler's error on the
			// wire: a plain Go error becomes codes.Unknown.
			st := status.Convert(routedRPCError("do it", tc.err))
			assert.Equal(t, tc.code, st.Code())
			assert.Equal(t, tc.reason, errorInfoReason(st))
		})
	}
}

// TestSingleHost_WireErrorsCarryNoErrorInfo pins that a single-host Power
// failure keeps its historical wire form exactly (codes.Unknown, no detail).
func TestSingleHost_WireErrorsCarryNoErrorInfo(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	fx.script("single", "fail-start", "")
	s := NewServer(&Provider{virshProvider: localHostVP("single")})

	_, err := s.Power(context.Background(), &providerv1.PowerRequest{Id: opsDomainName, Op: providerv1.PowerOp_POWER_OP_ON})
	require.Error(t, err)
	_, isStatus := status.FromError(err)
	assert.False(t, isStatus, "single-host Power still returns a plain wrapped error (codes.Unknown on the wire)")
	st := status.Convert(err)
	assert.Equal(t, codes.Unknown, st.Code())
	assert.Empty(t, st.Details(), "no ErrorInfo on a single-host Power")
}

// TestSingleHost_ReconfigureFailureKeptOutOfTheBreaker (review H2): a
// single-host Reconfigure that reached the host and failed there keeps its
// historical code and message and carries VM_OPERATION_FAILED, which the
// manager keeps out of the Provider's circuit breaker; a host that could not
// be reached keeps the plain error, which the breaker counts.
func TestSingleHost_ReconfigureFailureKeptOutOfTheBreaker(t *testing.T) {
	cases := map[string]struct {
		script map[string]string
		reason string // "" = no ErrorInfo (counted by the breaker)
	}{
		"the host refused the change":  {map[string]string{"fail-domstate": ""}, contracts.VMOperationFailedReason},
		"a paused domain is refused":   {map[string]string{"state": "paused\n", "id": "7"}, contracts.VMOperationFailedReason},
		"the host's libvirtd is down":  {map[string]string{"nolibvirtd": ""}, ""},
		"the connection to it dropped": {map[string]string{"dead": ""}, ""},
	}
	desired, err := json.Marshal(reconfigureTo(4, 0, 0))
	require.NoError(t, err)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newOpsFixture(t, map[string]map[string]string{
				"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
			})
			for f, v := range tc.script {
				fx.script("single", f, v)
			}
			s := NewServer(&Provider{virshProvider: localHostVP("single")})
			_, rerr := s.Reconfigure(context.Background(), &providerv1.ReconfigureRequest{Id: opsDomainName, DesiredJson: string(desired)})
			require.Error(t, rerr)
			st := status.Convert(rerr)
			assert.Equal(t, codes.Unknown, st.Code(), "the historical code")
			assert.True(t, strings.HasPrefix(st.Message(), "failed to reconfigure VM: "), "the historical message: %s", st.Message())
			assert.Equal(t, tc.reason, errorInfoReason(st))
		})
	}
}
