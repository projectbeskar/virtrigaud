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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestDelete_RefreshesDefaultPoolAfterRemovingDisks (R37): a VM delete that
// removed disk files refreshes the default pool once, after the removal, so
// `virsh vol-list default` no longer lists them; the refresh is best-effort
// (this fixture's virsh refuses pool-refresh, and the delete still succeeds).
// A delete that removed no disk does not refresh.
func TestDelete_RefreshesDefaultPoolAfterRemovingDisks(t *testing.T) {
	refreshes := func(calls []string) int {
		n := 0
		for _, c := range calls {
			if strings.HasSuffix(c, "pool-refresh default") {
				n++
			}
		}
		return n
	}

	t.Run("disks removed: one refresh, after the rm; a failed refresh does not fail the delete", func(t *testing.T) {
		fx := newRoutingFixture(t, map[string]map[string]string{
			"single": {"web": routingDomainXML("web", contracts.ObjectIdentity{})},
		})
		p := &Provider{virshProvider: localHostVP("single")}
		_, err := p.Delete(context.Background(), contracts.VMRef{ID: "web"})
		require.NoError(t, err)
		calls := fx.calls()
		require.Equal(t, 1, refreshes(calls), "%v", calls)
		assert.Equal(t, "single pool-refresh default", calls[len(calls)-1])
		assert.Contains(t, calls[len(calls)-2], "sudo rm -f -- ", "the refresh follows the removal")
	})

	t.Run("no disk removed: no refresh", func(t *testing.T) {
		fx := newRoutingFixture(t, map[string]map[string]string{
			"single": {"nodisk": "<domain type='kvm'>\n  <name>nodisk</name>\n  <uuid>11111111-2222-4333-8444-555555555556</uuid>\n" +
				"  <devices/>\n</domain>\n"},
		})
		p := &Provider{virshProvider: localHostVP("single")}
		_, err := p.Delete(context.Background(), contracts.VMRef{ID: "nodisk"})
		require.NoError(t, err)
		assert.Zero(t, refreshes(fx.calls()), "%v", fx.calls())
	})
}
