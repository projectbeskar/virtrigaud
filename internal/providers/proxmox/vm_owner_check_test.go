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

package proxmox

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin, against pvefake, the owner check of the per-VM calls other
// than Delete (vm_owner_check.go): a VirtualMachine whose VMID was reused —
// by another tenant's VirtRigaud VM (stamped with another UID) or by a VM made
// outside VirtRigaud (unstamped, another name) — can no longer read, power,
// resize, snapshot or revert the VM that took its VMID over.

const (
	// ownerCheckVMID is the VM every owner-check case puts at the
	// VirtualMachine's recorded VMID.
	ownerCheckVMID = 700
	// ownerCheckID is ownerCheckVMID as a provider id on the default node.
	ownerCheckID = "700"
	// ownerCheckSnapshot is the snapshot seeded on the VM for the snapshot
	// delete and revert calls.
	ownerCheckSnapshot = "snap-1"
)

// ownerCheckCall is one owner-checked per-VM call: run makes it for owner (no
// owner when nil); mutates says whether a successful call changes PVE state.
type ownerCheckCall struct {
	name    string
	mutates bool
	run     func(ctx context.Context, p *Provider, owner *providerv1.ObjectIdentity) error
}

// ownerCheckCalls are the calls the owner check guards.
func ownerCheckCalls() []ownerCheckCall {
	return []ownerCheckCall{
		{"Describe", false, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			resp, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: ownerCheckID, Owner: o})
			if err == nil && !resp.Exists {
				return status.Error(codes.Unknown, "test: an allowed describe must report the VM")
			}
			return err
		}},
		{"Power", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.Power(ctx, &providerv1.PowerRequest{Id: ownerCheckID, Op: providerv1.PowerOp_POWER_OP_OFF, Owner: o})
			return err
		}},
		{"Reconfigure", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.Reconfigure(ctx, &providerv1.ReconfigureRequest{Id: ownerCheckID,
				DesiredJson: `{"Class":{"CPU":4,"MemoryMiB":2048}}`, Owner: o})
			return err
		}},
		{"SnapshotCreate", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.SnapshotCreate(ctx, &providerv1.SnapshotCreateRequest{VmId: ownerCheckID, NameHint: "snap-2", Owner: o})
			return err
		}},
		{"SnapshotDelete", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.SnapshotDelete(ctx, &providerv1.SnapshotDeleteRequest{VmId: ownerCheckID, SnapshotId: ownerCheckSnapshot, Owner: o})
			return err
		}},
		{"SnapshotRevert", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.SnapshotRevert(ctx, &providerv1.SnapshotRevertRequest{VmId: ownerCheckID, SnapshotId: ownerCheckSnapshot, Owner: o})
			return err
		}},
		{"GetDiskInfo", false, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			resp, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{VmId: ownerCheckID, Owner: o})
			if err == nil && resp.VirtualSizeBytes <= 0 {
				return status.Error(codes.Unknown, "test: an allowed disk read must report the disk")
			}
			return err
		}},
		{"Clone", true, func(ctx context.Context, p *Provider, o *providerv1.ObjectIdentity) error {
			_, err := p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: ownerCheckID, TargetName: "copy", SourceOwner: o})
			return err
		}},
	}
}

// newOwnerCheckFixture starts a fake PVE holding vm at ownerCheckVMID
// (running, on node "pve", named like the test owner unless vm names it),
// with snapshot ownerCheckSnapshot taken by an owner-less call, and a
// provider for it.
func newOwnerCheckFixture(t *testing.T, vm pvefake.VM) (*Provider, *pvefake.Server) {
	t.Helper()
	p, srv := newStampTestProvider(t)
	vm.VMID, vm.Node, vm.Status = ownerCheckVMID, "pve", "running"
	if vm.Name == "" {
		vm.Name = testOwnerName
	}
	srv.PutVM(vm)
	_, err := p.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{VmId: ownerCheckID, NameHint: ownerCheckSnapshot})
	require.NoError(t, err)
	return p, srv
}

// requestsSince returns the requests srv received after the first n.
func requestsSince(srv *pvefake.Server, n int) []pvefake.RecordedRequest {
	return srv.Requests()[n:]
}

// onlyConfigReadOfVM asserts that, of reqs, the only ones addressing the
// owner-check VM are GETs of its config: nothing of its state was read for
// the answer, and nothing was changed.
func onlyConfigReadOfVM(t *testing.T, reqs []pvefake.RecordedRequest) {
	t.Helper()
	vmPath := "/qemu/" + ownerCheckID
	for _, r := range reqs {
		if !strings.Contains(r.Path, vmPath) {
			continue
		}
		assert.Equal(t, http.MethodGet, r.Method, "nothing of the VM may be changed: %s %s", r.Method, r.Path)
		assert.True(t, strings.HasSuffix(r.Path, vmPath+"/config"), "only the VM's config may be read: %s %s", r.Method, r.Path)
	}
}

// TestOwnerCheck_PerVMCalls: with an owner on the request, every guarded call
// acts on the VM at the VMID only when it is the requester's (stamped with its
// UID, or unstamped and named like it); a VM of another owner, or an
// unstamped VM with another name (a reused VMID), is NotFound + VM_NOT_OWNED,
// and a stamp that cannot be relied on is FailedPrecondition +
// OWNER_STAMP_UNREADABLE — neither read nor changed. A request without an
// owner (an older manager) is unchanged.
func TestOwnerCheck_PerVMCalls(t *testing.T) {
	mine := ownerProto(testOwner())
	other := testStamp(testOtherUID, ownerStampPhaseReady).line()
	cases := []struct {
		name       string
		vm         pvefake.VM
		owner      *providerv1.ObjectIdentity
		wantCode   codes.Code
		wantReason string
	}{
		{"own VM", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()}, mine, codes.OK, ""},
		{"own VM still being created", pvefake.VM{Description: "notes\n" + testStamp(testOwnerUID, ownerStampPhaseCreating).line()}, mine, codes.OK, ""},
		{"unstamped VM named like the VirtualMachine (created before owner stamps)", pvefake.VM{Description: "notes"}, mine, codes.OK, ""},
		{"unstamped VM under the name adoption gave it", pvefake.VM{Name: "Web"}, mine, codes.OK, ""},
		{"VMID reused by another tenant's VM", pvefake.VM{Name: "db", Description: other}, mine, codes.NotFound, contracts.VMNotOwnedReason},
		{"VMID reused by a VM of the same name of another tenant", pvefake.VM{Description: other}, mine, codes.NotFound, contracts.VMNotOwnedReason},
		{"VMID reused by a VM made outside VirtRigaud", pvefake.VM{Name: "someone-else", Description: "hand made"}, mine, codes.NotFound, contracts.VMNotOwnedReason},
		{"template of another owner", pvefake.VM{Template: 1, Description: other}, mine, codes.NotFound, contracts.VMNotOwnedReason},
		{"unreadable stamp", pvefake.VM{Description: "virtrigaud.owner v1 tampered"}, mine, codes.FailedPrecondition, contracts.OwnerStampUnreadableReason},
		{"two owners", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line() + "\n" + other}, mine,
			codes.FailedPrecondition, contracts.OwnerStampUnreadableReason},
		{"own stamp repeated", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line() + "\n" +
			testStamp(testOwnerUID, ownerStampPhaseReady).line()}, mine, codes.FailedPrecondition, contracts.OwnerStampUnreadableReason},
		{"no owner on the request (older manager): unchanged", pvefake.VM{Name: "db", Description: other}, nil, codes.OK, ""},
		{"owner without a UID: unchecked", pvefake.VM{Name: "db", Description: other},
			&providerv1.ObjectIdentity{Namespace: testNamespace, Name: testOwnerName}, codes.OK, ""},
	}
	for _, call := range ownerCheckCalls() {
		for _, tc := range cases {
			t.Run(call.name+"/"+tc.name, func(t *testing.T) {
				p, srv := newOwnerCheckFixture(t, tc.vm)
				before := len(srv.Requests())
				beforeMutating := len(srv.MutatingRequests())

				err := call.run(context.Background(), p, tc.owner)

				assert.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
				if tc.wantCode == codes.OK {
					if call.mutates {
						assert.Greater(t, len(srv.MutatingRequests()), beforeMutating, "an allowed %s acts on the VM", call.name)
					}
					return
				}
				assert.Equal(t, []string{tc.wantReason}, deleteErrorReasons(err))
				assert.Equal(t, beforeMutating, len(srv.MutatingRequests()), "a refused %s changes nothing", call.name)
				onlyConfigReadOfVM(t, requestsSince(srv, before))
				vm, ok := srv.GetVM(ownerCheckVMID)
				require.True(t, ok)
				assert.Equal(t, "running", vm.Status, "a refused %s leaves the VM as it was", call.name)
				msg := status.Convert(err).Message()
				assert.NotContains(t, msg, testOtherUID, "the answer never names the other owner")
				assert.NotContains(t, msg, "someone-else", "the answer never names the other VM")
			})
		}
	}
}

// TestOwnerCheck_ConfigReadFailureHoldsTheCall: a config that cannot be read
// is never taken as "owned": the call fails retryably and changes nothing.
func TestOwnerCheck_ConfigReadFailureHoldsTheCall(t *testing.T) {
	for _, call := range ownerCheckCalls() {
		t.Run(call.name, func(t *testing.T) {
			p, srv := newOwnerCheckFixture(t, pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()})
			srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/" + ownerCheckID + "/config",
				Status: http.StatusInternalServerError, Body: "boom"})
			beforeMutating := len(srv.MutatingRequests())

			err := call.run(context.Background(), p, ownerProto(testOwner()))

			require.Error(t, err)
			want := codes.Unavailable
			if call.name == "Reconfigure" || call.name == "GetDiskInfo" {
				// Both read the config for their own use and keep that
				// read's historical answer.
				want = codes.Internal
			}
			assert.Equal(t, want, status.Code(err), "error: %v", err)
			if call.name == "Clone" {
				assert.ElementsMatch(t, []string{contracts.CreateNotPerformedReason, contracts.VMOperationFailedReason},
					deleteErrorReasons(err), "a clone whose source cannot be checked made nothing: retry is safe")
			}
			assert.Equal(t, beforeMutating, len(srv.MutatingRequests()), "nothing is changed on an unknown owner")
		})
	}
}

// TestOwnerCheck_DescribeOfAGoneVMIsUnchanged: with an owner on the request,
// a VMID no node of the cluster has is still Exists=false (the VM is gone, and
// there is no VM there to check), so the manager re-creates it as before.
func TestOwnerCheck_DescribeOfAGoneVMIsUnchanged(t *testing.T) {
	p, _ := newStampTestProvider(t)
	resp, err := p.Describe(context.Background(), &providerv1.DescribeRequest{Id: "799", Owner: ownerProto(testOwner())})
	require.NoError(t, err)
	assert.False(t, resp.Exists)
}

// TestOwnerCheck_DescribeOfAReusedVMIDIsNotExistsFalse pins that a VM that is
// not the requester's is never reported Exists=false (which would make the
// manager create the VirtualMachine's VM again): the provider cannot prove the
// requester's VM gone — an adopted VM can still carry an earlier owner's stamp
// — so it answers VM_NOT_OWNED and the manager holds the VirtualMachine.
func TestOwnerCheck_DescribeOfAReusedVMIDIsNotExistsFalse(t *testing.T) {
	p, _ := newOwnerCheckFixture(t, pvefake.VM{Name: "db", Description: testStamp(testOtherUID, ownerStampPhaseReady).line()})
	resp, err := p.Describe(context.Background(), &providerv1.DescribeRequest{Id: ownerCheckID, Owner: ownerProto(testOwner())})
	require.Error(t, err)
	assert.Nil(t, resp, "none of the other VM's state is returned")
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Equal(t, []string{contracts.VMNotOwnedReason}, deleteErrorReasons(err))
}

// TestOwnerCheck_SnapshotDeleteChecksTheVMOnItsActualNode: the snapshot
// delete resolves the node the VM is on now (an HA move), and checks the
// owner of the VM there.
func TestOwnerCheck_SnapshotDeleteChecksTheVMOnItsActualNode(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.SetNodes("pve", "pve2")
	srv.PutVM(pvefake.VM{VMID: ownerCheckVMID, Node: "pve", Status: "running", Name: "db",
		Description: testStamp(testOtherUID, ownerStampPhaseReady).line()})
	_, err := p.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{VmId: ownerCheckID, NameHint: ownerCheckSnapshot})
	require.NoError(t, err)
	vm, _ := srv.GetVM(ownerCheckVMID)
	vm.Node = "pve2"
	srv.PutVM(vm)
	beforeMutating := len(srv.MutatingRequests())

	_, err = p.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: ownerCheckID,
		SnapshotId: ownerCheckSnapshot, Owner: ownerProto(testOwner())})

	assert.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
	assert.Equal(t, []string{contracts.VMNotOwnedReason}, deleteErrorReasons(err))
	assert.Equal(t, beforeMutating, len(srv.MutatingRequests()))
}

// TestOwnerCheck_ExportDisk: the disk of a VM that is not the requester's is
// never read or exported, on either backend; the check runs before the
// backend path, so an allowed request goes on to it (here: no SSH data plane
// configured, which that path reports).
func TestOwnerCheck_ExportDisk(t *testing.T) {
	other := testStamp(testOtherUID, ownerStampPhaseReady).line()
	cases := []struct {
		name       string
		vm         pvefake.VM
		owner      *providerv1.ObjectIdentity
		wantCode   codes.Code
		wantReason string
	}{
		{"own VM", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()}, ownerProto(testOwner()), codes.Unavailable, ""},
		{"unstamped VM named like the VirtualMachine", pvefake.VM{}, ownerProto(testOwner()), codes.Unavailable, ""},
		{"VMID reused by another tenant's VM", pvefake.VM{Name: "db", Description: other}, ownerProto(testOwner()), codes.NotFound, contracts.VMNotOwnedReason},
		{"VMID reused by a VM made outside VirtRigaud", pvefake.VM{Name: "someone-else"}, ownerProto(testOwner()), codes.NotFound, contracts.VMNotOwnedReason},
		{"unreadable stamp", pvefake.VM{Description: "virtrigaud.owner v1 tampered"}, ownerProto(testOwner()), codes.FailedPrecondition, contracts.OwnerStampUnreadableReason},
		{"no owner on the request (older manager): unchanged", pvefake.VM{Name: "db", Description: other}, nil, codes.Unavailable, ""},
	}
	for _, backend := range []string{"s3", "nfs"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				p, srv := newOwnerCheckFixture(t, tc.vm)
				before := len(srv.Requests())
				_, err := p.ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
					VmId: ownerCheckID, BackendType: backend, TransferMode: "relay",
					DestinationUrl: "nfs://nfs.example/export/x.qcow2", Owner: tc.owner,
				})
				assert.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
				if tc.wantReason == "" {
					assert.Contains(t, status.Convert(err).Message(), "SSH data plane", "an allowed export reaches the backend path")
					return
				}
				assert.Equal(t, []string{tc.wantReason}, deleteErrorReasons(err))
				onlyConfigReadOfVM(t, requestsSince(srv, before))
			})
		}
	}
}

// TestOwnerCheck_ExportPathsCheckTheConfigTheyCopyFrom: each export path's own
// disk read — the config that yields the volume it copies — is owner-checked
// too, not only ExportDisk's pre-check, so a VM that took the VMID over after
// the pre-check is never copied. The paths are called directly here (past the
// pre-check); nothing may run on the node.
func TestOwnerCheck_ExportPathsCheckTheConfigTheyCopyFrom(t *testing.T) {
	paths := map[string]func(p *Provider, req *providerv1.ExportDiskRequest) error{
		"s3": func(p *Provider, req *providerv1.ExportDiskRequest) error {
			_, err := p.exportDiskToS3(context.Background(), req)
			return err
		},
		"nfs": func(p *Provider, req *providerv1.ExportDiskRequest) error {
			_, err := p.exportDiskToNFS(context.Background(), req)
			return err
		},
	}
	for name, export := range paths {
		t.Run(name, func(t *testing.T) {
			p, srv := newOwnerCheckFixture(t, pvefake.VM{Name: "db", Description: testStamp(testOtherUID, ownerStampPhaseReady).line()})
			p.ssh = &sshTransport{host: "pve", logger: slog.Default(),
				runOverride: func(_ context.Context, cmd string) (string, string, error) {
					t.Errorf("nothing may run on the node for a VM that is not the requester's: %q", cmd)
					return "", "", nil
				}}
			before := len(srv.Requests())
			err := export(p, &providerv1.ExportDiskRequest{
				VmId: ownerCheckID, BackendType: name, TransferMode: "relay",
				DestinationUrl:     "nfs://nfs.example/export/web.qcow2",
				StorageOptionsJson: `{"server":"nfs.example","export":"/export"}`,
				Owner:              ownerProto(testOwner()),
			})
			assert.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
			assert.Equal(t, []string{contracts.VMNotOwnedReason}, deleteErrorReasons(err))
			onlyConfigReadOfVM(t, requestsSince(srv, before))
		})
	}
}
