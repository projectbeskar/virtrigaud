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

import "fmt"

// Hotplug headroom policy for online CPU/memory reconfigure (#203).
//
// When a VMClass opts into CPU/MemoryHotAddEnabled, the create-path provisions
// a *ceiling* above the boot/initial allocation so that the live grow path
// (`virsh setvcpus --live` / `virsh setmem --live`) can raise the running
// allocation up to that ceiling without a power cycle. The guest boots with the
// initial allocation online; the headroom is the difference between the ceiling
// and the initial.
//
// These constants are deliberately conservative and tunable:
//   - hotplugResourceMultiplier: the ceiling is this multiple of the initial
//     allocation (e.g. 4× initial CPUs / 4× initial memory).
//   - maxHotplugVCPUs: a hard cap on the provisioned vCPU ceiling so an
//     unusually large initial CPU count cannot provision an unbootable or
//     host-hostile maximum. Memory has no hard cap because the guest only
//     allocates currentMemory; the <memory> ceiling is merely the balloon
//     maximum.
const (
	// hotplugResourceMultiplier is the multiple of the initial allocation used
	// as the hotplug ceiling when CPU/memory hot-add is enabled.
	hotplugResourceMultiplier = 4

	// maxHotplugVCPUs caps the provisioned vCPU ceiling. The ceiling is never
	// allowed to exceed this value, regardless of the multiplier.
	maxHotplugVCPUs = 64
)

// CPUMemoryXML holds the three domain-XML lines that govern CPU and memory
// allocation, already rendered. The create-path substitutes these directly into
// the domain template so the headroom decision lives in one testable place.
type CPUMemoryXML struct {
	// VCPU is the full `<vcpu .../>` element.
	VCPU string
	// Memory is the full `<memory .../>` element (the balloon maximum).
	Memory string
	// CurrentMemory is the full `<currentMemory .../>` element (the initial,
	// guest-visible allocation).
	CurrentMemory string
}

// computeHotplugCeilingVCPUs returns the vCPU ceiling for a VM created with CPU
// hot-add enabled. The ceiling is hotplugResourceMultiplier × initial, floored
// so that it is strictly greater than the initial (so headroom always exists
// even for a 1-vCPU VM where the multiplier would otherwise be exact), and
// capped at maxHotplugVCPUs. If the initial already meets or exceeds the cap,
// the ceiling equals the initial (no headroom is possible).
func computeHotplugCeilingVCPUs(initial int32) int32 {
	if initial < 1 {
		initial = 1
	}
	ceiling := initial * hotplugResourceMultiplier
	// Floor: ensure the ceiling is strictly greater than the initial so that
	// live grow has somewhere to go.
	if ceiling <= initial {
		ceiling = initial + 1
	}
	// Hard cap.
	if ceiling > maxHotplugVCPUs {
		ceiling = maxHotplugVCPUs
	}
	// If the initial is already at/over the cap, no headroom is possible.
	if ceiling < initial {
		ceiling = initial
	}
	return ceiling
}

// computeHotplugCeilingMemoryMiB returns the memory balloon-maximum ceiling for
// a VM created with memory hot-add enabled. The ceiling is
// hotplugResourceMultiplier × initial, floored so that it is strictly greater
// than the initial. There is no hard cap: the guest only allocates
// currentMemory (the initial); <memory> is just the balloon maximum that
// `setmem --live` can inflate up to.
func computeHotplugCeilingMemoryMiB(initial int64) int64 {
	if initial < 1 {
		initial = 1
	}
	ceiling := initial * hotplugResourceMultiplier
	if ceiling <= initial {
		ceiling = initial + 1
	}
	return ceiling
}

// buildCPUMemoryXML renders the `<vcpu>`, `<memory>`, and `<currentMemory>`
// domain-XML elements for the create-path, provisioning hotplug headroom when
// the corresponding hot-add flag is set (#203).
//
// Behavior:
//
//   - CPU hot-add OFF (default): `<vcpu placement='static'>N</vcpu>` — byte
//     identical to the historical layout, no `current=` attribute.
//
//   - CPU hot-add ON: `<vcpu placement='static' current='I'>C</vcpu>` where I is
//     the initial (boots online) and C is the ceiling (the extra vCPUs start
//     offline and are brought online by `setvcpus --live`).
//
//   - Memory hot-add OFF (default): `<memory>` == `<currentMemory>` == initial —
//     byte identical to the historical layout.
//
//   - Memory hot-add ON: `<memory>` = ceiling (the balloon maximum) and
//     `<currentMemory>` = initial, so `setmem --live` can inflate the balloon
//     from initial up to the ceiling.
//
// The cpuCount and memMiB arguments are the initial (boot) allocations.
func buildCPUMemoryXML(cpuCount int32, memMiB int64, cpuHotAdd, memHotAdd bool) CPUMemoryXML {
	var out CPUMemoryXML

	// --- CPU ---
	if cpuHotAdd {
		ceiling := computeHotplugCeilingVCPUs(cpuCount)
		out.VCPU = fmt.Sprintf("<vcpu placement='static' current='%d'>%d</vcpu>", cpuCount, ceiling)
	} else {
		out.VCPU = fmt.Sprintf("<vcpu placement='static'>%d</vcpu>", cpuCount)
	}

	// --- Memory ---
	if memHotAdd {
		ceiling := computeHotplugCeilingMemoryMiB(memMiB)
		out.Memory = fmt.Sprintf("<memory unit='MiB'>%d</memory>", ceiling)
		out.CurrentMemory = fmt.Sprintf("<currentMemory unit='MiB'>%d</currentMemory>", memMiB)
	} else {
		out.Memory = fmt.Sprintf("<memory unit='MiB'>%d</memory>", memMiB)
		out.CurrentMemory = fmt.Sprintf("<currentMemory unit='MiB'>%d</currentMemory>", memMiB)
	}

	return out
}
