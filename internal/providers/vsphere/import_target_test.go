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
	"github.com/vmware/govmomi/find"
)

// TestResolveImportTarget_FreshProvider (lab finding): a provider that has just
// started has a finder with no datacenter, as createVSphereClient builds it.
// ImportDisk used to resolve the DefaultDatastore before scoping the finder to
// the datacenter, so the first import after a provider restart failed with
// "datastore/StoragePod not found". resolveImportTarget scopes the finder
// first, so the same fresh finder resolves the datastore and the placement.
func TestResolveImportTarget_FreshProvider(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	// A finder with no datacenter, as a freshly started provider used to have it.
	unscoped := find.NewFinder(p.client.Client, true)
	_, err := p.resolveImportDatastore(ctx, unscoped, "")
	require.Error(t, err, "precondition: an unscoped finder cannot resolve a relative datastore name")

	target, err := p.resolveImportTarget(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, simDatastore, target.datastore.Name())
	assert.NotNil(t, target.datacenter)
	assert.NotNil(t, target.pool)
	assert.NotNil(t, target.folder)
	assert.NotNil(t, target.host)
}
