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

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVMSnapshot_DeleteEventsNeverNameTheDialTarget (security review L1): a
// SnapshotDelete that failed in the gRPC transport — whose text names the
// provider Service's address — is reported in the SnapshotDeleteFailed and
// SnapshotLeftOnHypervisor events as "the provider could not be reached".
func TestVMSnapshot_DeleteEventsNeverNameTheDialTarget(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"held":         nil,
		"force-delete": {forceDeleteAnnotation: "true"},
	} {
		t.Run(name, func(t *testing.T) {
			r, spy, get, events := deletingSnapshot(t, annotations)
			spy.answer(transportFailure("snapshotDelete"))

			_, err := r.handleDeletion(context.Background(), get())
			require.NoError(t, err)
			evs := strings.Join(eventsOf(events), "\n")
			assert.Contains(t, evs, providerUnreachableDetail)
			assert.NotContains(t, evs, "10.96.12.7")
			assert.NotContains(t, evs, "dial tcp")
			if snap := get(); snap != nil {
				assert.NotContains(t, snap.Status.Message, "10.96.12.7")
			}
		})
	}
}
