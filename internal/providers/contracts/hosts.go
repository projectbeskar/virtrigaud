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

// HostHealth is the manager-side, transport-agnostic health of a hypervisor
// host in a clustered provider's inventory. It mirrors the provider.v1
// HostHealth enum (ADR-0007 P1). The probe that produces it is per-hypervisor
// (e.g. libvirtd reachability); the enum itself is provider-agnostic.
type HostHealth string

const (
	// HostHealthUnspecified means the provider did not report a health state.
	HostHealthUnspecified HostHealth = "Unspecified"
	// HostHealthReady means the host is reachable and available for placement.
	HostHealthReady HostHealth = "Ready"
	// HostHealthNotReady means the host is known but not currently usable.
	HostHealthNotReady HostHealth = "NotReady"
)

// HostInfo describes one hypervisor host fronted by a clustered provider. It is
// the manager-side view of the provider.v1 HostInfo message (ADR-0007 P1). The
// shape is hypervisor-agnostic; the source of each value is per-hypervisor (for
// libvirt: `virsh nodeinfo` / `pool-info` / `domcapabilities`). No VM or disk
// bytes are carried here — this is inventory metadata only.
type HostInfo struct {
	// ID is the operator-assigned host id (equal to the Host CR name).
	ID string
	// Address is the SSH endpoint / agent address of the host.
	Address string
	// AllocatableCPU is the number of allocatable vCPUs on the host.
	AllocatableCPU int32
	// AllocatableMemMiB is the allocatable memory on the host, in MiB.
	AllocatableMemMiB int64
	// AllocatableStorage is the allocatable storage on the host, in bytes.
	AllocatableStorage int64
	// Health is the host's most recent health-probe result.
	Health HostHealth
	// Labels carry scheduling constraints (storage-pool / network visibility,
	// zone/rack). Constraints for placement live here.
	Labels map[string]string
	// CPUModel is the host's baseline CPU model (per-hypervisor interpretation).
	CPUModel string
	// CPUFeatures lists the host's CPU feature flags.
	CPUFeatures []string
	// MachineTypes lists the machine types the host supports.
	MachineTypes []string
	// EmulatorVersion is the host's emulator/hypervisor version.
	EmulatorVersion string
}
