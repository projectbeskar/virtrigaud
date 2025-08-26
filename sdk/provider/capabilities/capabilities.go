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

// Package capabilities provides provider capability management and advertisement.
package capabilities

import (
	"context"

	providerv1 "github.com/projectbeskar/virtrigaud/internal/rpc/provider/v1"
)

// Capability represents a provider capability flag.
type Capability string

// Standard capability flags that providers can advertise.
const (
	// Core capabilities (all providers should support)
	CapabilityValidate        Capability = "validate"
	CapabilityCreate          Capability = "create"
	CapabilityDelete          Capability = "delete"
	CapabilityPower           Capability = "power"
	CapabilityDescribe        Capability = "describe"
	CapabilityGetCapabilities Capability = "get_capabilities"

	// Optional capabilities
	CapabilityReconfigure            Capability = "reconfigure"
	CapabilityReconfigureOnline      Capability = "reconfigure_online"
	CapabilityDiskExpansionOnline    Capability = "disk_expansion_online"
	CapabilitySnapshots              Capability = "snapshots"
	CapabilityMemorySnapshots        Capability = "memory_snapshots"
	CapabilityLinkedClones           Capability = "linked_clones"
	CapabilityImageImport            Capability = "image_import"
	CapabilityTaskStatus             Capability = "task_status"

	// Provider-specific capabilities
	CapabilityVSphere          Capability = "vsphere"
	CapabilityLibvirt          Capability = "libvirt"
	CapabilityFirecracker      Capability = "firecracker"
	CapabilityQEMU             Capability = "qemu"
	CapabilityMock             Capability = "mock"
)

// Profile represents a set of capabilities that form a functional profile.
type Profile string

// Standard capability profiles.
const (
	// ProfileCore includes basic VM lifecycle operations
	ProfileCore Profile = "core"

	// ProfileSnapshot includes snapshot operations
	ProfileSnapshot Profile = "snapshot"

	// ProfileClone includes clone operations
	ProfileClone Profile = "clone"

	// ProfileImagePrepare includes image preparation operations
	ProfileImagePrepare Profile = "image_prepare"

	// ProfileAdvanced includes advanced features like online reconfigure
	ProfileAdvanced Profile = "advanced"
)

// Manager manages provider capabilities.
type Manager struct {
	capabilities         map[Capability]bool
	supportedDiskTypes   []string
	supportedNetworkTypes []string
}

// NewManager creates a new capability manager.
func NewManager() *Manager {
	return &Manager{
		capabilities: make(map[Capability]bool),
	}
}

// AddCapability adds a capability to the manager.
func (m *Manager) AddCapability(cap Capability) *Manager {
	m.capabilities[cap] = true
	return m
}

// RemoveCapability removes a capability from the manager.
func (m *Manager) RemoveCapability(cap Capability) *Manager {
	delete(m.capabilities, cap)
	return m
}

// HasCapability checks if a capability is supported.
func (m *Manager) HasCapability(cap Capability) bool {
	return m.capabilities[cap]
}

// SetSupportedDiskTypes sets the supported disk types.
func (m *Manager) SetSupportedDiskTypes(types []string) *Manager {
	m.supportedDiskTypes = types
	return m
}

// SetSupportedNetworkTypes sets the supported network types.
func (m *Manager) SetSupportedNetworkTypes(types []string) *Manager {
	m.supportedNetworkTypes = types
	return m
}

// GetCapabilities returns the capabilities response for gRPC.
func (m *Manager) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:      m.HasCapability(CapabilityReconfigureOnline),
		SupportsDiskExpansionOnline:    m.HasCapability(CapabilityDiskExpansionOnline),
		SupportsSnapshots:              m.HasCapability(CapabilitySnapshots),
		SupportsMemorySnapshots:        m.HasCapability(CapabilityMemorySnapshots),
		SupportsLinkedClones:           m.HasCapability(CapabilityLinkedClones),
		SupportsImageImport:            m.HasCapability(CapabilityImageImport),
		SupportedDiskTypes:             m.supportedDiskTypes,
		SupportedNetworkTypes:          m.supportedNetworkTypes,
	}, nil
}

// GetProfileCapabilities returns the capabilities required for a profile.
func GetProfileCapabilities(profile Profile) []Capability {
	switch profile {
	case ProfileCore:
		return []Capability{
			CapabilityValidate,
			CapabilityCreate,
			CapabilityDelete,
			CapabilityPower,
			CapabilityDescribe,
			CapabilityGetCapabilities,
		}
	case ProfileSnapshot:
		return []Capability{
			CapabilitySnapshots,
		}
	case ProfileClone:
		return []Capability{
			CapabilityLinkedClones,
		}
	case ProfileImagePrepare:
		return []Capability{
			CapabilityImageImport,
		}
	case ProfileAdvanced:
		return []Capability{
			CapabilityReconfigure,
			CapabilityReconfigureOnline,
			CapabilityDiskExpansionOnline,
		}
	default:
		return nil
	}
}

// SupportsProfile checks if the manager supports all capabilities in a profile.
func (m *Manager) SupportsProfile(profile Profile) bool {
	required := GetProfileCapabilities(profile)
	for _, cap := range required {
		if !m.HasCapability(cap) {
			return false
		}
	}
	return true
}

// GetSupportedProfiles returns all profiles supported by the manager.
func (m *Manager) GetSupportedProfiles() []Profile {
	profiles := []Profile{ProfileCore, ProfileSnapshot, ProfileClone, ProfileImagePrepare, ProfileAdvanced}
	var supported []Profile

	for _, profile := range profiles {
		if m.SupportsProfile(profile) {
			supported = append(supported, profile)
		}
	}

	return supported
}

// Builder provides a fluent interface for building capability managers.
type Builder struct {
	manager *Manager
}

// NewBuilder creates a new capability builder.
func NewBuilder() *Builder {
	return &Builder{
		manager: NewManager(),
	}
}

// Core adds core capabilities (required for all providers).
func (b *Builder) Core() *Builder {
	caps := GetProfileCapabilities(ProfileCore)
	for _, cap := range caps {
		b.manager.AddCapability(cap)
	}
	return b
}

// Snapshots adds snapshot capabilities.
func (b *Builder) Snapshots() *Builder {
	b.manager.AddCapability(CapabilitySnapshots)
	return b
}

// MemorySnapshots adds memory snapshot capabilities.
func (b *Builder) MemorySnapshots() *Builder {
	b.manager.AddCapability(CapabilityMemorySnapshots)
	return b
}

// LinkedClones adds linked clone capabilities.
func (b *Builder) LinkedClones() *Builder {
	b.manager.AddCapability(CapabilityLinkedClones)
	return b
}

// ImageImport adds image import capabilities.
func (b *Builder) ImageImport() *Builder {
	b.manager.AddCapability(CapabilityImageImport)
	return b
}

// Reconfigure adds reconfiguration capabilities.
func (b *Builder) Reconfigure() *Builder {
	b.manager.AddCapability(CapabilityReconfigure)
	return b
}

// OnlineReconfigure adds online reconfiguration capabilities.
func (b *Builder) OnlineReconfigure() *Builder {
	b.manager.AddCapability(CapabilityReconfigureOnline)
	return b
}

// OnlineDiskExpansion adds online disk expansion capabilities.
func (b *Builder) OnlineDiskExpansion() *Builder {
	b.manager.AddCapability(CapabilityDiskExpansionOnline)
	return b
}

// TaskStatus adds task status checking capabilities.
func (b *Builder) TaskStatus() *Builder {
	b.manager.AddCapability(CapabilityTaskStatus)
	return b
}

// VSphere marks this as a vSphere provider.
func (b *Builder) VSphere() *Builder {
	b.manager.AddCapability(CapabilityVSphere)
	return b
}

// Libvirt marks this as a libvirt provider.
func (b *Builder) Libvirt() *Builder {
	b.manager.AddCapability(CapabilityLibvirt)
	return b
}

// Firecracker marks this as a Firecracker provider.
func (b *Builder) Firecracker() *Builder {
	b.manager.AddCapability(CapabilityFirecracker)
	return b
}

// QEMU marks this as a QEMU provider.
func (b *Builder) QEMU() *Builder {
	b.manager.AddCapability(CapabilityQEMU)
	return b
}

// Mock marks this as a mock provider.
func (b *Builder) Mock() *Builder {
	b.manager.AddCapability(CapabilityMock)
	return b
}

// DiskTypes sets supported disk types.
func (b *Builder) DiskTypes(types ...string) *Builder {
	b.manager.SetSupportedDiskTypes(types)
	return b
}

// NetworkTypes sets supported network types.
func (b *Builder) NetworkTypes(types ...string) *Builder {
	b.manager.SetSupportedNetworkTypes(types)
	return b
}

// Build returns the configured capability manager.
func (b *Builder) Build() *Manager {
	return b.manager
}

// Standard disk types that providers can support.
var (
	DiskTypeThin        = "thin"
	DiskTypeThick       = "thick"
	DiskTypeEagerZero   = "eager_zero"
	DiskTypeRaw         = "raw"
	DiskTypeQCOW2       = "qcow2"
	DiskTypeVMDK        = "vmdk"
	DiskTypeVHD         = "vhd"
	DiskTypeVDI         = "vdi"
)

// Standard network types that providers can support.
var (
	NetworkTypeBridge     = "bridge"
	NetworkTypeNAT        = "nat"
	NetworkTypeHostOnly   = "host_only"
	NetworkTypeDistributed = "distributed"
	NetworkTypeVLAN       = "vlan"
	NetworkTypeOVS        = "ovs"
)
