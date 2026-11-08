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

package grantverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
)

// TestDecodeRequestFor_ZeroRowDoesNotPanic: a zero KindRow, as RowFor returns
// for an unknown kind, is refused and never panics.
func TestDecodeRequestFor_ZeroRowDoesNotPanic(t *testing.T) {
	var row grantverify.KindRow
	assert.NotPanics(t, func() {
		m, refusal := grantverify.DecodeRequestFor(row, []byte{0x0a, 0x01, 'x'})
		assert.Nil(t, m)
		require.NotNil(t, refusal)
		assert.Equal(t, grantverify.ReasonGrantMismatch, refusal.Reason)
		assert.Equal(t, grantverify.RuleUnknownEnum, refusal.Rule)
	})
	assert.Nil(t, row.NewRequest())
	assert.Empty(t, string(row.RequestType()))
}
