/*
Copyright 2025.

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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestHandleCreatingPhase_NilDiskInfoFailsCleanly verifies the Creating-phase
// guard (Bug Y): a migration that reaches Creating without recorded disk info
// (status.diskInfo nil) must transition to Failed with a diagnosable message
// rather than dereference nil and panic in an endless recover/requeue loop.
func TestHandleCreatingPhase_NilDiskInfoFailsCleanly(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{} // not exercised on this path
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.DiskInfo = nil // import did not record disk info
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	// Must not panic; must fail cleanly.
	_, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)

	var got infrav1beta1.VMMigration
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(migration), &got))
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase,
		"a Creating migration with nil DiskInfo must transition to Failed")
	assert.Contains(t, got.Status.Message, "disk info",
		"the failure message must name the missing disk info")
}
