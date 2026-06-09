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

package libvirt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildCPUMemoryXML_HotAddOff verifies that with both hot-add flags off the
// rendered CPU/memory elements are byte-identical to the historical layout:
// no `current=` attribute on <vcpu>, and <memory>==<currentMemory>==initial. No
// regression for existing/most VMs (#203).
func TestBuildCPUMemoryXML_HotAddOff(t *testing.T) {
	out := buildCPUMemoryXML(2, 2048, false, false)

	assert.Equal(t, "<vcpu placement='static'>2</vcpu>", out.VCPU)
	assert.Equal(t, "<memory unit='MiB'>2048</memory>", out.Memory)
	assert.Equal(t, "<currentMemory unit='MiB'>2048</currentMemory>", out.CurrentMemory)

	// Explicitly assert no headroom leaked in.
	assert.NotContains(t, out.VCPU, "current=", "hot-add off must not emit a vcpu current= attribute")
	// <memory> and <currentMemory> must carry the same numeric value: stripping
	// the differing element names yields identical strings.
	memValue := strings.ReplaceAll(out.Memory, "memory", "")
	curValue := strings.ReplaceAll(out.CurrentMemory, "currentMemory", "")
	assert.Equal(t, memValue, curValue,
		"hot-add off must keep <memory> and <currentMemory> at the same value")
}

// TestBuildCPUMemoryXML_CPUHotAddOnly verifies CPU headroom is provisioned
// (vcpu carries current='<initial>' and a max of the ceiling) while memory is
// left at the historical equal-value layout when only CPU hot-add is enabled.
func TestBuildCPUMemoryXML_CPUHotAddOnly(t *testing.T) {
	out := buildCPUMemoryXML(2, 2048, true, false)

	// 2 vCPUs initial, 4× ceiling = 8 max, boots 2 online.
	assert.Equal(t, "<vcpu placement='static' current='2'>8</vcpu>", out.VCPU)

	// Memory untouched.
	assert.Equal(t, "<memory unit='MiB'>2048</memory>", out.Memory)
	assert.Equal(t, "<currentMemory unit='MiB'>2048</currentMemory>", out.CurrentMemory)
}

// TestBuildCPUMemoryXML_MemoryHotAddOnly verifies the memory balloon ceiling is
// the 4× headroom and currentMemory stays at the initial, while CPU is left at
// the historical no-current layout when only memory hot-add is enabled.
func TestBuildCPUMemoryXML_MemoryHotAddOnly(t *testing.T) {
	out := buildCPUMemoryXML(2, 2048, false, true)

	// CPU untouched.
	assert.Equal(t, "<vcpu placement='static'>2</vcpu>", out.VCPU)

	// 2048 MiB initial, 4× ceiling = 8192 max (balloon maximum), guest sees 2048.
	assert.Equal(t, "<memory unit='MiB'>8192</memory>", out.Memory)
	assert.Equal(t, "<currentMemory unit='MiB'>2048</currentMemory>", out.CurrentMemory)
}

// TestBuildCPUMemoryXML_BothHotAdd verifies that with both flags on, CPU carries
// current=<initial> with the ceiling max, and memory's <memory> ceiling is
// strictly greater than <currentMemory> (the initial), so live grow has
// headroom on both axes (#203).
func TestBuildCPUMemoryXML_BothHotAdd(t *testing.T) {
	out := buildCPUMemoryXML(4, 4096, true, true)

	assert.Equal(t, "<vcpu placement='static' current='4'>16</vcpu>", out.VCPU)
	assert.Equal(t, "<memory unit='MiB'>16384</memory>", out.Memory)
	assert.Equal(t, "<currentMemory unit='MiB'>4096</currentMemory>", out.CurrentMemory)
}

// TestComputeHotplugCeilingVCPUs covers the multiplier, the floor (ceiling must
// be strictly > initial even for a 1-vCPU VM), the hard cap at maxHotplugVCPUs,
// and the at/over-cap case where no headroom is possible.
func TestComputeHotplugCeilingVCPUs(t *testing.T) {
	cases := []struct {
		name    string
		initial int32
		want    int32
	}{
		{"single vcpu floored to >initial", 1, 4},     // 1*4 = 4, already > 1
		{"two vcpus", 2, 8},                           // 2*4 = 8
		{"four vcpus", 4, 16},                         // 4*4 = 16
		{"multiplier exceeds cap is clamped", 32, 64}, // 32*4 = 128 -> cap 64
		{"initial at cap yields no headroom", 64, 64}, // 64*4 = 256 -> cap 64 == initial
		{"initial over cap yields initial", 100, 100}, // ceiling clamps back to initial
		{"zero treated as one", 0, 4},                 // normalized to 1, then 1*4
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeHotplugCeilingVCPUs(tc.initial)
			assert.Equal(t, tc.want, got)
			// Invariant: a ceiling can never be below the initial.
			if tc.initial >= 1 {
				assert.GreaterOrEqual(t, got, tc.initial)
			}
		})
	}
}

// TestComputeHotplugCeilingVCPUs_AlwaysHasHeadroomBelowCap asserts that for any
// initial strictly below the cap, the ceiling is strictly greater than the
// initial (headroom actually exists).
func TestComputeHotplugCeilingVCPUs_AlwaysHasHeadroomBelowCap(t *testing.T) {
	for initial := int32(1); initial < maxHotplugVCPUs; initial++ {
		got := computeHotplugCeilingVCPUs(initial)
		assert.Greater(t, got, initial,
			"initial=%d below cap must provision headroom", initial)
		assert.LessOrEqual(t, got, int32(maxHotplugVCPUs),
			"initial=%d ceiling must never exceed the cap", initial)
	}
}

// TestComputeHotplugCeilingMemoryMiB covers the multiplier and the floor; memory
// has no hard cap because the guest only allocates currentMemory.
func TestComputeHotplugCeilingMemoryMiB(t *testing.T) {
	cases := []struct {
		name    string
		initial int64
		want    int64
	}{
		{"1 GiB", 1024, 4096},
		{"2 GiB", 2048, 8192},
		{"tiny floored to >initial", 1, 4},
		{"zero treated as one", 0, 4},
		{"large no cap", 65536, 262144}, // 64 GiB * 4 = 256 GiB, uncapped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeHotplugCeilingMemoryMiB(tc.initial)
			assert.Equal(t, tc.want, got)
			if tc.initial >= 1 {
				assert.Greater(t, got, tc.initial, "memory ceiling must exceed the initial")
			}
		})
	}
}
