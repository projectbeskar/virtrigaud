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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestLibvirtDomainNamesMatchContracts is the drift guard of the libvirt
// domain naming rule: the manager looks for a domain a Create may have made
// with contracts.LibvirtCreateDomainNames, which must name exactly the domains
// this provider's own rule makes (domainNameFor) and re-binds by their legacy
// name (legacyNameOf).
func TestLibvirtDomainNamesMatchContracts(t *testing.T) {
	cases := []struct{ namespace, name string }{
		{"team-a", "web"},
		{"default", "db-01"},
		{"team-a", "web.v2"},
		{"team-a", "12345"},
		{"team-a", "0123456789abcdef0123456789abcdef"},
		{"team-a", "01234567-89ab-cdef-0123-456789abcdef"},
		{"team-a", strings.Repeat("a", 190)},
		{strings.Repeat("n", 63), strings.Repeat("b", 253)},
		{"team-a", strings.Repeat("a", 175) + "." + strings.Repeat("b", 30)}, // the cut lands on the dot
	}
	for _, tc := range cases {
		owner := contracts.ObjectIdentity{UID: "uid-1", Namespace: tc.namespace, Name: tc.name}
		bound, err := domainNameFor(owner, tc.name)
		require.NoError(t, err, "%s/%s", tc.namespace, tc.name)
		assert.Equal(t, bound, boundDomainName(tc.namespace, tc.name))
		assert.Equal(t, bound, contracts.LibvirtBoundDomainName(tc.namespace, tc.name), "%s/%s", tc.namespace, tc.name)
		want := []string{bound}
		if legacy, ok := legacyNameOf(owner, tc.name, bound); ok {
			want = append(want, legacy)
		}
		assert.Equal(t, want, contracts.LibvirtCreateDomainNames(tc.namespace, tc.name), "%s/%s", tc.namespace, tc.name)
	}
}
