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

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// vmsNamedIn counts the mock's VMs named name.
func vmsNamedIn(p *Provider, name string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, vm := range p.vms {
		if vm.Name == name {
			n++
		}
	}
	return n
}

// TestCreate_ResendRebindsAndCapabilityIsReported: the mock reports
// supports_owner_rebind and, like the real providers, a Create sent again for
// the same owner uid returns the VM it made (rc2 R4).
func TestCreate_ResendRebindsAndCapabilityIsReported(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()
	caps, err := p.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerRebind())

	req := &providerv1.CreateRequest{Name: "web", Owner: &providerv1.ObjectIdentity{Uid: "uid-web", Namespace: "default", Name: "web"}}
	first, err := p.Create(ctx, req)
	require.NoError(t, err)
	second, err := p.Create(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first.Id, second.Id)
	assert.Equal(t, 1, vmsNamedIn(p, "web"))

	_, err = p.Create(ctx, &providerv1.CreateRequest{Name: "web"})
	require.NoError(t, err)
	assert.Equal(t, 2, vmsNamedIn(p, "web"), "without an owner, a Create makes a VM as before")
}

// TestClone_ResendRebindsTheStampedClone: as on the real providers (rc2 R4),
// a re-sent Clone for the same target VirtualMachine UID returns the clone it
// made; another target UID under that name is refused; a request without a
// UID clones as before.
func TestClone_ResendRebindsTheStampedClone(t *testing.T) {
	ctx := context.Background()
	p := NewProvider()
	target := &providerv1.ObjectIdentity{Uid: "uid-target", Namespace: "default", Name: "copy"}
	req := &providerv1.CloneRequest{SourceVmId: "vm-1", TargetName: "copy", TargetVm: target}

	first, err := p.Clone(ctx, req)
	require.NoError(t, err)
	second, err := p.Clone(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first.TargetVmId, second.TargetVmId)
	assert.Equal(t, 1, vmsNamedIn(p, "copy"), "no second clone")

	_, err = p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: "vm-1", TargetName: "copy",
		TargetVm: &providerv1.ObjectIdentity{Uid: "uid-other", Namespace: "default", Name: "copy"}})
	assert.Equal(t, codes.AlreadyExists, status.Code(err), "%v", err)

	_, err = p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: "vm-1", TargetName: "copy"})
	require.NoError(t, err, "a request without a UID clones as before")
	assert.Equal(t, 2, vmsNamedIn(p, "copy"))
}
