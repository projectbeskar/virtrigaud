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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Real PVE answers a per-VM call for a VM that is not on the node asked with
// HTTP 500 "Configuration file 'nodes/<node>/qemu-server/<vmid>.conf' does
// not exist" — for a deleted VM and for one that moved to another node alike.
// The fake answers the same way; these tests pin that "missing here" is
// treated as gone only once the VMID is free cluster-wide.

func TestPVEAPI_MissingVMIsErrVMNotFound(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	srv.PutVM(pvefake.VM{VMID: 700, Name: "elsewhere", Node: "pve2", Status: "stopped"})

	_, err := p.client.GetVMConfig(ctx, "pve", 700)
	assert.ErrorIs(t, err, pveapi.ErrVMNotFound, "on another node")
	_, err = p.client.GetVM(ctx, "pve", 701)
	assert.ErrorIs(t, err, pveapi.ErrVMNotFound, "deleted")
	_, err = p.client.DeleteVM(ctx, "pve", 701, true)
	assert.ErrorIs(t, err, pveapi.ErrVMNotFound)

	free, err := p.client.VMIDFree(ctx, 700)
	require.NoError(t, err)
	assert.False(t, free)
	free, err = p.client.VMIDFree(ctx, 701)
	require.NoError(t, err)
	assert.True(t, free)

	// A 500 that is not "does not exist" is a plain error.
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/700/config", Status: http.StatusInternalServerError, Body: "got timeout"})
	_, err = p.client.GetVMConfig(ctx, "pve2", 700)
	require.Error(t, err)
	assert.NotErrorIs(t, err, pveapi.ErrVMNotFound)
}

func TestDescribe_MissingOnlyFromThisNodeIsNotGone(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	srv.PutVM(pvefake.VM{VMID: 702, Name: "moved", Node: "pve2", Status: "running"})

	_, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: "702"})
	require.Error(t, err, "a VM on another node is never reported as gone")
	assert.Equal(t, codes.Unavailable, status.Code(err))

	resp, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: "703"})
	require.NoError(t, err)
	assert.False(t, resp.Exists)

	// The VMID is in use although the token cannot see the VM in
	// /cluster/resources: /cluster/nextid?vmid= still says it is taken.
	srv.HideFromResources(702)
	_, err = p.Describe(ctx, &providerv1.DescribeRequest{Id: "702"})
	require.Error(t, err)
}

func TestDelete_MissingOnlyFromThisNodeIsHeld(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	srv.PutVM(pvefake.VM{VMID: 704, Name: "moved", Node: "pve2", Status: "stopped"})

	_, err := p.Delete(ctx, &providerv1.DeleteRequest{Id: "704"})
	require.Error(t, err, "success would release the finalizer of a VM that still exists")
	assert.NotEqual(t, codes.NotFound, status.Code(err))
	_, exists := srv.GetVM(704)
	assert.True(t, exists)

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: "705"})
	require.NoError(t, err, "a VMID free cluster-wide is already deleted")

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: "pve2:704"})
	require.NoError(t, err)
	_, exists = srv.GetVM(704)
	assert.False(t, exists)
}

func TestRollback_MovedVMIsNotNothingRemains(t *testing.T) {
	p, srv := newStampTestProvider(t)
	mine := testStamp(testOwnerUID, ownerStampPhaseCreating)
	srv.PutVM(pvefake.VM{VMID: 706, Name: testOwnerName, Node: "pve2", Status: "stopped", Description: mine.line()})
	require.Error(t, p.rollbackCreatedVM(context.Background(), "pve", 706, testOwnerName, &mine, true))
	require.NoError(t, p.rollbackCreatedVM(context.Background(), "pve", 707, testOwnerName, &mine, true))
}

func TestClone_SourceOnAnotherNodeIsNotNotFound(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 708, Name: "src", Node: "pve2", Status: "stopped"})
	_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "708", TargetName: "c"})
	require.Error(t, err)
	assert.NotEqual(t, codes.NotFound, status.Code(err))

	_, err = p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "709", TargetName: "c"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
