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

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
)

// TestInflight_RestartGuardSeesBothNamelessForms (H-A): /cluster/resources may
// list a guest whose config has no name with an empty name or, as PVE's
// status daemon does, as "VM <vmid>". A restarted provider holds a re-sent
// Create on the in-progress clone under either form.
func TestInflight_RestartGuardSeesBothNamelessForms(t *testing.T) {
	for name, empty := range map[string]bool{`"VM <vmid>"`: false, "empty name": true} {
		t.Run(name, func(t *testing.T) {
			srv, endpoint := startSlowFake(t)
			srv.SetEmptyResourceNames(empty)
			ctx := context.Background()

			_, err := slowTaskProvider(t, endpoint).Create(ctx, createReq(testOwner(), templateImageJSON))
			require.Equal(t, classOutcomeUnknown, classOf(t, err))

			_, err = slowTaskProvider(t, endpoint).Create(ctx, createReq(testOwner(), templateImageJSON))
			assert.Equal(t, classOutcomeUnknown, classOf(t, err))
			assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"), "no second clone after a restart")
		})
	}
}

func TestNameless(t *testing.T) {
	assert.True(t, nameless(pveapi.ClusterVM{VMID: 105}))
	assert.True(t, nameless(pveapi.ClusterVM{VMID: 105, Name: "VM 105"}))
	assert.False(t, nameless(pveapi.ClusterVM{VMID: 105, Name: "VM 106"}))
	assert.False(t, nameless(pveapi.ClusterVM{VMID: 105, Name: "web"}))
}
