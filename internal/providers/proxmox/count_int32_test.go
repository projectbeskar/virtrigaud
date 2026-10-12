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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCountToInt32_Clamps(t *testing.T) {
	assert.Equal(t, int32(0), countToInt32(-1))
	assert.Equal(t, int32(0), countToInt32(0))
	assert.Equal(t, int32(8), countToInt32(8))
	assert.Equal(t, int32(math.MaxInt32), countToInt32(math.MaxInt32))
	assert.Equal(t, int32(math.MaxInt32), countToInt32(math.MaxInt32+1))
}
