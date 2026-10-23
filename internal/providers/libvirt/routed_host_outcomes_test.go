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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R25: a clustered provider's answer to a host that stops
// answering says how far the call got. The manager reads HOST_UNAVAILABLE as
// "nothing was done", so it is answered only before any command of the call
// was answered; while only reads ran the call is "not performed"; once a
// command that may change the host was sent (and its session dropped, or it
// was stopped by timeout(1) or killed) the outcome is unknown. A read
// (Describe) cannot half-do anything and stays HOST_UNAVAILABLE.
//
// Each case runs the real RPC through the gRPC Server on the shell-shim
// fixtures. A host is "dead" when its fake virsh dies without an exit status
// (as a dropped SSH session does), either for every call ("dead") or for one
// subcommand ("dead-<subcommand>"): list and dumpxml are the owner check every
// routed call starts with.

// hostOutcome is how a routed RPC's wire error reads to the manager.
type hostOutcome string

const (
	// outcomeHostUnavailable: Unavailable + HOST_UNAVAILABLE ("nothing done").
	outcomeHostUnavailable hostOutcome = "HOST_UNAVAILABLE"
	// outcomeNotPerformed: Unavailable + VM_OPERATION_FAILED, nothing done.
	outcomeNotPerformed hostOutcome = "not performed"
	// outcomeUnknown: Unknown + VM_OPERATION_FAILED, outcome unknown.
	outcomeUnknown hostOutcome = "unknown"
	// outcomeUnknownRetry: Unavailable + VM_OPERATION_FAILED, outcome unknown,
	// retry (idempotent RPCs).
	outcomeUnknownRetry hostOutcome = "unknown, retry"
)

// hostOutcomeOf reads err the way the manager's mapping does.
func hostOutcomeOf(t *testing.T, err error) hostOutcome {
	t.Helper()
	require.Error(t, err)
	st := status.Convert(err)
	vmOpFailed := contracts.HasErrorInfoReason(st.Err(), contracts.VMOperationFailedReason)
	switch {
	case hasHostUnavailableInfo(st):
		require.Equal(t, codes.Unavailable, st.Code())
		return outcomeHostUnavailable
	case vmOpFailed && st.Code() == codes.Unavailable && containsAll(st.Message(), "nothing was done"):
		return outcomeNotPerformed
	case vmOpFailed && st.Code() == codes.Unavailable && containsAll(st.Message(), "outcome is unknown", "retry"):
		return outcomeUnknownRetry
	case vmOpFailed && st.Code() == codes.Unknown &&
		(containsAll(st.Message(), "outcome is unknown") || containsAll(st.Message(), "was stopped")):
		// A host that stopped answering, or a command stopped by its budget
		// (timeout(1) 124 or 137: classifyRoutedFailure).
		return outcomeUnknown
	}
	return hostOutcome(fmt.Sprintf("other: %s %q (reason %q)", st.Code(), st.Message(), errorInfoReason(st)))
}

// containsAll reports whether s contains every one of parts.
func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// hostOutcomeCase is one RPC on one scripted host failure.
type hostOutcomeCase struct {
	name string
	// host files written in host-b's directory ("dead", "dead-dumpxml", ...).
	host []string
	// local files (the fake host-shell tools': "dead-qemu-img-convert", ...).
	local map[string]string
	want  hostOutcome
}

// TestRoutedHostOutcomes_OpsRPCs: Power, Reconfigure, Delete and Describe.
func TestRoutedHostOutcomes_OpsRPCs(t *testing.T) {
	ctx := context.Background()
	owner := ownerFromIdentity(ownerTeamA)
	desired, err := json.Marshal(reconfigureTo(4, 0, 0))
	require.NoError(t, err)

	rpcs := map[string]func(s *Server) error{
		"Power": func(s *Server) error {
			_, e := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_OFF, TargetHostId: "host-b", Owner: owner})
			return e
		},
		"Reconfigure": func(s *Server) error {
			_, e := s.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: "web", DesiredJson: string(desired), TargetHostId: "host-b", Owner: owner})
			return e
		},
		"Delete": func(s *Server) error {
			_, e := s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
			return e
		},
		"Describe": func(s *Server) error {
			_, e := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
			return e
		},
	}
	// The command that changes the host, per RPC.
	writeVerb := map[string]string{"Power": "destroy", "Reconfigure": "setvcpus", "Delete": "undefine"}

	for rpc, call := range rpcs {
		t.Run(rpc, func(t *testing.T) {
			cases := []hostOutcomeCase{
				{name: "every call dies: the host never answered", host: []string{"dead"}, want: outcomeHostUnavailable},
			}
			if rpc == "Describe" {
				// A read cannot half-do anything: HOST_UNAVAILABLE at any point.
				cases = append(cases, hostOutcomeCase{name: "the host drops after answering the list", host: []string{"dead-dumpxml"}, want: outcomeHostUnavailable})
			} else {
				cases = append(cases,
					hostOutcomeCase{name: "the host drops after answering the list: only reads ran", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
					hostOutcomeCase{name: "the " + writeVerb[rpc] + " is sent and its session drops", host: []string{"dead-" + writeVerb[rpc]}, want: outcomeUnknown},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					fx, p := clusteredOpsFixture(t)
					// Power off only changes a running domain.
					fx.script("host-b", "state", "running\n")
					for _, f := range tc.host {
						fx.script("host-b", f, "")
					}
					assert.Equal(t, tc.want, hostOutcomeOf(t, call(NewServer(p))))
				})
			}
		})
	}
}

// TestRoutedHostOutcomes_LeaseFailureIsHostUnavailable: a host that cannot be
// dialed answers HOST_UNAVAILABLE before any command ran, for every RPC kind.
func TestRoutedHostOutcomes_LeaseFailureIsHostUnavailable(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
	p := clusterOf(t, []string{"host-a", "host-b"}, "host-b")
	s := NewServer(p)
	ctx := context.Background()
	owner := ownerFromIdentity(ownerTeamA)
	for name, call := range map[string]func() error{
		"Power": func() error {
			_, e := s.Power(ctx, &providerv1.PowerRequest{Id: "web", Op: providerv1.PowerOp_POWER_OP_OFF, TargetHostId: "host-b", Owner: owner})
			return e
		},
		"Delete": func() error {
			_, e := s.Delete(ctx, &providerv1.DeleteRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
			return e
		},
		"SnapshotCreate": func() error {
			_, e := s.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: "web", TargetHostId: "host-b", Owner: owner, NameHint: "s"})
			return e
		},
		"ExportDisk": func() error {
			_, e := s.ExportDisk(ctx, &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-b", Owner: owner,
				BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2"})
			return e
		},
		"Clone": func() error {
			_, e := s.Clone(ctx, routedCloneReq())
			return e
		},
		"Describe": func() error {
			_, e := s.Describe(ctx, &providerv1.DescribeRequest{Id: "web", TargetHostId: "host-b", Owner: owner})
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, call()))
		})
	}
}

// TestRoutedHostOutcomes_SnapshotCloneExport: the snapshot family, Clone and
// ExportDisk (nfs and s3), on the fixture whose host-shell tools can be
// scripted to die or to end with timeout(1)'s exit statuses.
func TestRoutedHostOutcomes_SnapshotCloneExport(t *testing.T) {
	ctx := context.Background()
	snapshotCreate := func(fx *routedSCD) error {
		_, e := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d", RequestToken: "uid-snap-1"})
		return e
	}
	snapshotCreateNoToken := func(fx *routedSCD) error {
		_, e := NewServer(fx.p).SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "d"})
		return e
	}
	snapshotDelete := func(fx *routedSCD) error {
		_, e := NewServer(fx.p).SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{
			VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner})
		return e
	}
	snapshotRevert := func(fx *routedSCD) error {
		_, e := NewServer(fx.p).SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{
			VmId: "web", SnapshotId: "s1", TargetHostId: "host-b", Owner: teamAOwner})
		return e
	}
	clone := func(fx *routedSCD) error {
		_, e := NewServer(fx.p).Clone(ctx, routedCloneReq())
		return e
	}
	exportNFS := func(fx *routedSCD) error {
		fx.p.hostDiskTransportFn = anyTransport
		_, e := NewServer(fx.p).ExportDisk(ctx, &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "nfs", DestinationUrl: "nfs://nas/e/web.qcow2"})
		return e
	}
	exportS3 := func(fx *routedSCD) error {
		fx.p.hostDiskTransportFn = anyTransport
		_, e := NewServer(fx.p).ExportDisk(ctx, &providerv1.ExportDiskRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "s3",
			DestinationUrl:     "s3://bucket/web.qcow2",
			StorageOptionsJson: `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`,
			Credentials:        map[string]string{"accessKeyID": "a", "secretAccessKey": "s"},
		})
		return e
	}

	tests := []struct {
		rpc   string
		run   func(fx *routedSCD) error
		cases []hostOutcomeCase
	}{
		{"SnapshotCreate", snapshotCreate, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "the name lookup drops: only reads ran", host: []string{"dead-snapshot-list"}, want: outcomeNotPerformed},
			{name: "snapshot-create-as is sent and its session drops: retried by request token", host: []string{"dead-snapshot-create-as"}, want: outcomeUnknownRetry},
		}},
		{"SnapshotCreate without a request token", snapshotCreateNoToken, []hostOutcomeCase{
			{name: "snapshot-create-as is sent and its session drops: a retry could take a second snapshot", host: []string{"dead-snapshot-create-as"}, want: outcomeUnknown},
		}},
		{"SnapshotDelete", snapshotDelete, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "snapshot-delete is sent and its session drops", host: []string{"dead-snapshot-delete"}, want: outcomeUnknown},
		}},
		{"SnapshotRevert", snapshotRevert, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "snapshot-revert is sent and its session drops", host: []string{"dead-snapshot-revert"}, want: outcomeUnknown},
		}},
		{"Clone", clone, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list: before the copy", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "the copy is sent and its session drops: retried on the same host", local: map[string]string{"dead-qemu-img-convert": ""}, want: outcomeUnknownRetry},
		}},
		{"ExportDisk nfs", exportNFS, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "the convert is sent and its session drops", local: map[string]string{"dead-qemu-img-convert": ""}, want: outcomeUnknown},
			{name: "the convert is killed (137)", local: map[string]string{"exit-qemu-img-convert": "137"}, want: outcomeUnknown},
			{name: "the convert is stopped by timeout(1) (124)", local: map[string]string{"timeout-expire": ""}, want: outcomeUnknown},
		}},
		{"ExportDisk s3", exportS3, []hostOutcomeCase{
			{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
			{name: "the owner check drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
			{name: "the flatten is sent and its session drops", local: map[string]string{"dead-qemu-img-convert": ""}, want: outcomeUnknown},
			{name: "the flatten is killed (137)", local: map[string]string{"exit-qemu-img-convert": "137"}, want: outcomeUnknown},
			{name: "the flatten is stopped by timeout(1) (124)", local: map[string]string{"timeout-expire": ""}, want: outcomeUnknown},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.rpc, func(t *testing.T) {
			for _, tc := range tt.cases {
				t.Run(tc.name, func(t *testing.T) {
					fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
					for _, f := range tc.host {
						fx.script("host-b", f, "")
					}
					for f, content := range tc.local {
						fx.script("local", f, content)
					}
					assert.Equal(t, tc.want, hostOutcomeOf(t, tt.run(fx)))
				})
			}
		})
	}
}

// TestRoutedHostOutcomes_TransferOwner: the owner re-stamp is a write.
func TestRoutedHostOutcomes_TransferOwner(t *testing.T) {
	for _, tc := range []hostOutcomeCase{
		{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
		{name: "the definition read drops after the list", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
		{name: "the stamp is sent and its session drops", host: []string{"dead-metadata"}, want: outcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dom := listDomain{name: "web", uuid: uuidWebA}
			fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
			fx.scriptStamp("host-a", dom, adopter)
			for _, f := range tc.host {
				fx.write("host-a", f, "")
			}
			p := clusterOf(t, []string{"host-a"})
			_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
			assert.Equal(t, tc.want, hostOutcomeOf(t, err))
		})
	}
}

// TestRoutedHostOutcomes_Create: a clustered create's host commands run on
// the lease with the create's progress record, so the same rules apply to its
// failure (createRPCError). The create pipeline itself is replaced by a
// sequence of reads and a define, run on the leased connection.
func TestRoutedHostOutcomes_Create(t *testing.T) {
	for _, tc := range []hostOutcomeCase{
		{name: "every call dies", host: []string{"dead"}, want: outcomeHostUnavailable},
		{name: "a read drops after the list: only reads ran", host: []string{"dead-dumpxml"}, want: outcomeNotPerformed},
		{name: "the define is sent and its session drops", host: []string{"dead-define"}, want: outcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
			p, _, _ := routedCluster(t)
			p.createOnHostFn = func(ctx context.Context, c hostconn.Conn, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
				for _, args := range [][]string{{"list", "--all"}, {"dumpxml", "web"}, {"define", "/tmp/web.xml"}} {
					if _, err := c.Virsh(ctx, args...); err != nil {
						return contracts.CreateResponse{}, contracts.NewRetryableError("failed to create VM", err)
					}
				}
				return contracts.CreateResponse{ID: "web"}, nil
			}
			for _, f := range tc.host {
				fx.script("host-b", f, "")
			}
			_, err := NewServer(p).Create(context.Background(), &providerv1.CreateRequest{Name: "web", TargetHostId: "host-b"})
			assert.Equal(t, tc.want, hostOutcomeOf(t, err))
		})
	}
}

// TestRoutedHostOutcomes_GetDiskInfoStaysHostUnavailable: GetDiskInfo only
// reads; a host that stops answering after it answered is still
// HOST_UNAVAILABLE.
func TestRoutedHostOutcomes_GetDiskInfoStaysHostUnavailable(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.script("host-b", "dead-dumpxml", "")
	_, err := NewServer(fx.p).GetDiskInfo(context.Background(), &providerv1.GetDiskInfoRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner})
	assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, err))
}

// TestHostOpRPCErrorFor_Matrix pins the answer for every kind of RPC at every
// stage of a call, and that a failure that is not a host failure is unaffected.
func TestHostOpRPCErrorFor_Matrix(t *testing.T) {
	transport := &VirshError{Command: "virsh x", ExitCode: -1, Stderr: "ssh: handshake failed"}
	rejected := &VirshError{Command: "virsh x", ExitCode: 1, Stderr: "error: nope"}
	progressAt := func(stage hostDownStage) *hostProgress {
		p := &hostProgress{}
		switch stage {
		case hostDownReadsOnly:
			p.answered = true
		case hostDownWriteSent:
			p.answered, p.wrote = true, true
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		kind  hostOpKind
		stage hostDownStage
		err   error
		want  hostOutcome
	}{
		{"write, nothing ran", hostOpWrite, hostDownNothingRan, transport, outcomeHostUnavailable},
		{"write, reads answered", hostOpWrite, hostDownReadsOnly, transport, outcomeNotPerformed},
		{"write, write sent", hostOpWrite, hostDownWriteSent, transport, outcomeUnknown},
		{"retry, nothing ran", hostOpWriteRetry, hostDownNothingRan, transport, outcomeHostUnavailable},
		{"retry, reads answered", hostOpWriteRetry, hostDownReadsOnly, transport, outcomeNotPerformed},
		{"retry, write sent", hostOpWriteRetry, hostDownWriteSent, transport, outcomeUnknownRetry},
		{"read, nothing ran", hostOpRead, hostDownNothingRan, transport, outcomeHostUnavailable},
		{"read, reads answered", hostOpRead, hostDownReadsOnly, transport, outcomeHostUnavailable},
		{"the dependency guard's host failure is fail-closed HOST_UNAVAILABLE", hostOpWrite, hostDownReadsOnly,
			&guardHostUnreachableError{op: guardOpDelete, domain: "d", cause: transport}, outcomeHostUnavailable},
		{"a command the host rejected is the operation's failure", hostOpWrite, hostDownWriteSent, rejected, "other: Unknown " +
			`"failed to do it on host \"host-b\"" (reason "VM_OPERATION_FAILED")`},
		{"a cancelled call is not a host failure", hostOpWrite, hostDownNothingRan,
			&VirshError{ExitCode: -1, Cause: context.Canceled}, "other: Unknown " +
				`"failed to do it on host \"host-b\"" (reason "VM_OPERATION_FAILED")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ho := &hostOpError{host: "host-b", err: tc.err, progress: progressAt(tc.stage)}
			assert.Equal(t, tc.want, hostOutcomeOf(t, hostOpRPCErrorFor(tc.kind, "do it", ho, tc.err)))
		})
	}

	t.Run("an untracked error assumes a write was sent", func(t *testing.T) {
		ho := &hostOpError{host: "host-b", err: transport}
		assert.Equal(t, outcomeUnknown, hostOutcomeOf(t, hostOpRPCErrorFor(hostOpWrite, "do it", ho, transport)))
	})
	t.Run("the wire text names the operation and host only", func(t *testing.T) {
		secret := &VirshError{Command: "virsh -c qemu+ssh://10.0.0.5/system x", ExitCode: -1, Stderr: "dial 10.0.0.5:22 /var/lib/x"}
		for _, stage := range []hostDownStage{hostDownNothingRan, hostDownReadsOnly, hostDownWriteSent} {
			ho := &hostOpError{host: "host-b", err: secret, progress: progressAt(stage)}
			msg := status.Convert(hostOpRPCErrorFor(hostOpWrite, "do it", ho, secret)).Message()
			for _, leak := range []string{"10.0.0.5", "/var/lib", "virsh", "qemu+ssh"} {
				assert.NotContains(t, msg, leak)
			}
		}
	})
	t.Run("routedRPCError variants choose the kind", func(t *testing.T) {
		ho := &hostOpError{host: "host-b", err: transport, progress: progressAt(hostDownWriteSent)}
		assert.Equal(t, outcomeUnknown, hostOutcomeOf(t, routedRPCError("do it", ho)))
		assert.Equal(t, outcomeUnknownRetry, hostOutcomeOf(t, routedIdempotentRPCError("do it", ho)))
		assert.Equal(t, outcomeHostUnavailable, hostOutcomeOf(t, routedReadRPCError("do it", ho)))
	})
}

// TestRoutedHostOutcomes_DeleteLandedDisk: the routed landed-disk delete
// answers a host that stops answering by how far it got: reads that were
// answered are "could not be checked or removed" (retried), the removal that
// was sent is "not known whether it was removed" (retried; the removal is
// idempotent), and only a host that never answered is HOST_UNAVAILABLE.
func TestRoutedHostOutcomes_DeleteLandedDisk(t *testing.T) {
	transport := virshFailure(-1, "reset")
	for name, tc := range map[string]struct {
		failOn  string
		wantMsg string
		hostDow bool
	}{
		"the very first command finds the host dead": {failOn: "virsh pool-dumpxml", hostDow: true},
		"a read drops after the host answered":       {failOn: "sh kind", wantMsg: landedDiskFailedMessage},
		"the rm is sent and its session drops":       {failOn: "rm", wantMsg: landedDiskRemovalUnknownMessage},
	} {
		t.Run(name, func(t *testing.T) {
			f := newImportFixtureOf(t, []string{"host-b"})
			f.rec.failOn(tc.failOn, 0, transport)
			_, err := f.server().DeleteLandedDisk(context.Background(), deleteLandedRequest("host-b"))
			st := status.Convert(err)
			require.Equal(t, codes.Unavailable, st.Code(), "%v", err)
			if tc.hostDow {
				assert.True(t, hasHostUnavailableInfo(st), "%v", err)
				return
			}
			assert.False(t, hasHostUnavailableInfo(st), "%v", err)
			assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason))
			assert.Equal(t, tc.wantMsg, st.Message())
		})
	}
}
