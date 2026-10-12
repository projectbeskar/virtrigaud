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

package vsphere

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The manager sends DeleteRequest.owner on single-host deletes too. vSphere
// does not owner-check a delete, so a VM it created for that owner, and an
// unstamped (pre-upgrade or adopted) VM, are deleted exactly as before.
func TestDelete_ToleratesOwnerOnTheRequest(t *testing.T) {
	ctx := context.Background()
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)

	own, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: own.Id, Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)
	assert.False(t, vmExists(t, p, own.Id))

	legacy, err := p.Create(ctx, templateCreateRequest("legacy", "", nil))
	require.NoError(t, err)
	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: legacy.Id, Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err, "an unstamped VM stays deletable with an owner on the request")
	assert.False(t, vmExists(t, p, legacy.Id))
}
