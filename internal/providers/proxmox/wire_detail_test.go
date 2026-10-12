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
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestWireDetail_CutsAtARuneBoundary (L-c): a long PVE error text is cut to
// maxWireDetailLen bytes without splitting a multi-byte character.
func TestWireDetail_CutsAtARuneBoundary(t *testing.T) {
	assert.Equal(t, "short", wireDetail("  short \n"))

	s := "x" + strings.Repeat("é", maxWireDetailLen) // byte maxWireDetailLen falls inside an "é"
	got := wireDetail(s)
	assert.True(t, utf8.ValidString(got))
	assert.True(t, strings.HasSuffix(got, "..."))
	assert.LessOrEqual(t, len(got), maxWireDetailLen+len("..."))
	assert.Equal(t, maxWireDetailLen-1+len("..."), len(got), "cut back one byte to the rune start")
}
