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

package contracts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOwnerTransferDecision pins the compare-and-swap decision every
// provider's TransferOwner shares.
func TestOwnerTransferDecision(t *testing.T) {
	adopter := ObjectIdentity{UID: "11111111-1111-4111-8111-111111111111", Namespace: "team-a", Name: "web"}
	stale := ObjectIdentity{UID: "22222222-2222-4222-8222-222222222222", Namespace: "team-a", Name: "old"}
	live := ObjectIdentity{UID: "33333333-3333-4333-8333-333333333333", Namespace: "team-b", Name: "db"}
	edited := adopter
	edited.Namespace = "team-b"

	for _, tc := range []struct {
		name     string
		recorded []ObjectIdentity
		replace  []string
		already  bool
		refused  bool
	}{
		{name: "unstamped"},
		{name: "already exactly the owner's", recorded: []ObjectIdentity{adopter}, already: true},
		{name: "owner's uid under another namespace (edited)", recorded: []ObjectIdentity{edited}, refused: true},
		{name: "stale and replaceable", recorded: []ObjectIdentity{stale}, replace: []string{stale.UID}},
		{name: "stamped, not replaceable", recorded: []ObjectIdentity{stale}, refused: true},
		{name: "live owner, another uid replaceable", recorded: []ObjectIdentity{live}, replace: []string{stale.UID}, refused: true},
		{name: "two stamps", recorded: []ObjectIdentity{stale, adopter}, replace: []string{stale.UID}, refused: true},
		{name: "stamp without uid", recorded: []ObjectIdentity{{Namespace: "x", Name: "y"}}, replace: []string{""}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			already, err := OwnerTransferDecision(adopter, tc.recorded, tc.replace)
			if tc.refused {
				require.Error(t, err)
				assert.False(t, already)
				assert.NotContains(t, err.Error(), live.Namespace, "the refusal names no other owner")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.already, already)
		})
	}
}
