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

// Hotplug headroom (#203). A VM whose VMClass enables CPU or memory hot-add is
// created with a ceiling above its initial allocation, so a live resize can
// grow it without a power cycle. The libvirt provider provisions it; the
// clustered scheduler counts a memory hot-add VM at its memory ceiling, because
// a guest can deflate its balloon up to it (ADR-0007 Addendum A,
// scheduler-accuracy amendment). The rule lives here so both read the same
// numbers.
const (
	// HotplugResourceMultiplier is the multiple of the initial allocation
	// provisioned as the hot-add ceiling.
	HotplugResourceMultiplier = 4
	// MaxHotplugVCPUs caps the provisioned vCPU ceiling, whatever the
	// multiplier gives.
	MaxHotplugVCPUs = 64
)

// HotplugCeilingVCPUs returns the vCPU ceiling provisioned for a VM created
// with CPU hot-add: HotplugResourceMultiplier × initial, at least initial+1 so
// headroom exists, at most MaxHotplugVCPUs, and never below initial.
func HotplugCeilingVCPUs(initial int32) int32 {
	if initial < 1 {
		initial = 1
	}
	ceiling := initial * HotplugResourceMultiplier
	if ceiling <= initial {
		ceiling = initial + 1
	}
	if ceiling > MaxHotplugVCPUs {
		ceiling = MaxHotplugVCPUs
	}
	if ceiling < initial {
		ceiling = initial
	}
	return ceiling
}

// HotplugCeilingMemoryMiB returns the memory ceiling (the balloon maximum,
// libvirt's <memory>) provisioned for a VM created with memory hot-add:
// HotplugResourceMultiplier × initial, at least initial+1. It has no cap: the
// guest starts at the initial allocation and the ceiling is what the balloon
// can reach.
func HotplugCeilingMemoryMiB(initial int64) int64 {
	if initial < 1 {
		initial = 1
	}
	ceiling := initial * HotplugResourceMultiplier
	if ceiling <= initial {
		ceiling = initial + 1
	}
	return ceiling
}
