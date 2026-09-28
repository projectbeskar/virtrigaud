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

package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSaturatingAdd (review L2): a committed-capacity sum saturates at
// math.MaxInt64 instead of wrapping negative; a negative operand counts as 0.
func TestSaturatingAdd(t *testing.T) {
	assert.Equal(t, int64(7), SaturatingAdd(3, 4))
	assert.Equal(t, int64(math.MaxInt64), SaturatingAdd(math.MaxInt64, 1))
	assert.Equal(t, int64(math.MaxInt64), SaturatingAdd(math.MaxInt64/2+1, math.MaxInt64/2+1))
	assert.Equal(t, int64(3), SaturatingAdd(3, -10), "a malformed demand never frees capacity")
	assert.Equal(t, int64(4), SaturatingAdd(-3, 4))
}

// TestIndexPlaced_CommittedSumSaturates (review L2): VMs whose sizes would
// overflow the host's committed sum make it saturate, so the host has no free
// capacity — never a wrapped-negative sum that would look free.
func TestIndexPlaced_CommittedSumSaturates(t *testing.T) {
	_, committedByHost := indexPlaced([]PlacedVM{
		{Name: "a", HostID: "h", Resources: ResourceRequest{CPU: 4, MemoryMiB: math.MaxInt64}},
		{Name: "b", HostID: "h", Resources: ResourceRequest{CPU: 4, MemoryMiB: math.MaxInt64}},
		{Name: "c", HostID: "h", Resources: ResourceRequest{CPU: 4, MemoryMiB: 1}},
	}, "")
	c := committedByHost["h"]
	assert.Equal(t, int64(math.MaxInt64), c.memMiB)
	assert.Equal(t, int64(12), c.cpu)
}
