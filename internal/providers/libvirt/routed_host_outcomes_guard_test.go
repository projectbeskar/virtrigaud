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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestRoutedHostOutcomes_CreateGuardUnreachableAfterSetupWrites (R25 L1): the
// cluster-wide disk guard's HOST_UNAVAILABLE concerns ANOTHER host and may
// follow idempotent setup writes on the leased host (the seed directory, the
// default storage pool in a Create; the varstore check in a Clone). Those
// writes leave nothing a retry does not redo, and the guard says nothing was
// created, so the answer stays the fail-closed HOST_UNAVAILABLE even though a
// command that may change the leased host was sent.
func TestRoutedHostOutcomes_CreateGuardUnreachableAfterSetupWrites(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {"web": routingDomainXML("web", ownerTeamA)}})
	p, _, _ := routedCluster(t)
	var progress *hostProgress
	p.createOnHostFn = func(ctx context.Context, c hostconn.Conn, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
		progress = hostProgressFrom(ctx)
		// An idempotent setup write on the leased host, then the guard finds
		// another host of the Provider unreachable.
		if _, err := c.Virsh(ctx, "define", "/tmp/setup.xml"); err != nil {
			return contracts.CreateResponse{}, err
		}
		return contracts.CreateResponse{}, &clusterGuardIncompleteError{op: guardOpCreate, domain: "team-a.web", unreachable: true}
	}
	_, err := NewServer(p).Create(context.Background(), &providerv1.CreateRequest{Name: "web", TargetHostId: "host-b"})
	require.Error(t, err)
	require.NotNil(t, progress)
	assert.Equal(t, hostDownWriteSent, progress.stage(), "the setup write was sent")
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st), "fail closed, nothing created: %v", err)
}

// TestRoutedHostOutcomes_DeleteDestroyDrops: a Delete continues after a failed
// destroy (the domain may already be off). A clustered Delete does not when the
// host stopped answering during the destroy: the domain may still be running,
// and undefining it would leave it running as a transient domain whose disks
// are then removed. The destroy is a write that was sent, so the answer is
// unknown, and the delete is retried.
func TestRoutedHostOutcomes_DeleteDestroyDrops(t *testing.T) {
	del := func(p *Provider) error {
		_, err := NewServer(p).Delete(context.Background(), &providerv1.DeleteRequest{
			Id: "web", TargetHostId: "host-b", Owner: ownerFromIdentity(ownerTeamA)})
		return err
	}
	t.Run("the host stops answering during the destroy: unknown, nothing undefined", func(t *testing.T) {
		fx, p := clusteredOpsFixture(t)
		fx.script("host-b", "state", "running\n")
		fx.script("host-b", "dead-destroy", "")
		err := del(p)
		assert.Equal(t, outcomeUnknown, hostOutcomeOf(t, err))
		assert.True(t, contracts.IsHostConnectionLost(err))
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "undefine", "the definition is not removed under a domain that may run: %q", c)
		}
	})
	t.Run("the destroy fails with an exit status: the delete goes on (historical)", func(t *testing.T) {
		fx, p := clusteredOpsFixture(t)
		fx.script("host-b", "fail-destroy", "")
		require.NoError(t, del(p))
		assert.Contains(t, fx.calls(), "host-b undefine "+routingDomainUUID)
	})
}
