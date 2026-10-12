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

package contracts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLibvirtCreateDomainNames(t *testing.T) {
	assert.Equal(t, []string{"team-a.web", "web"}, LibvirtCreateDomainNames("team-a", "web"))
	assert.Equal(t, []string{"team-a.web.v2"}, LibvirtCreateDomainNames("team-a", "web.v2"), "a dotted name has no legacy form")
	assert.Equal(t, []string{"team-a.12345"}, LibvirtCreateDomainNames("team-a", "12345"), "virsh would read it as a domain ID")
	long := strings.Repeat("a", 250)
	names := LibvirtCreateDomainNames("team-a", long)
	require.Len(t, names, 2)
	assert.LessOrEqual(t, len(names[0]), 200)
	assert.True(t, strings.HasPrefix(names[0], "team-a.aaa"))
	assert.Regexp(t, `_[0-9a-f]{16}$`, names[0])
	assert.Equal(t, names[0], LibvirtBoundDomainName("team-a", long), "deterministic")
}
