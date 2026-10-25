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

package conformance

import (
	"sort"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Capability names a specification can list in requiredCapabilities.
const (
	// CapVMCreate: create a VirtualMachine. Every provider (core contract).
	CapVMCreate = "vm-create"
	// CapVMDelete: delete a VirtualMachine. Every provider (core contract).
	CapVMDelete = "vm-delete"
	// CapVMPower: power a VirtualMachine on and off. Every provider (core
	// contract).
	CapVMPower = "vm-power"
	// CapVMReconfigure: change a powered-off VirtualMachine's CPU and memory.
	// Every provider (core contract).
	CapVMReconfigure = "vm-reconfigure"
	// CapVMClone: full clone of a VirtualMachine (VMClone). Every provider
	// (core contract).
	CapVMClone = "vm-clone"
	// CapVMReconfigureOnline: change a running VM's CPU and memory
	// (status.reportedCapabilities.supportsReconfigureOnline).
	CapVMReconfigureOnline = "vm-reconfigure-online"
	// CapVMSnapshot: VMSnapshot (status.reportedCapabilities.supportsSnapshots).
	CapVMSnapshot = "vm-snapshot"
	// CapVMSnapshotMemory: memory-inclusive VMSnapshot
	// (status.reportedCapabilities.supportsMemorySnapshots).
	CapVMSnapshotMemory = "vm-snapshot-memory"
	// CapVMCloneLinked: linked clone
	// (status.reportedCapabilities.supportsLinkedClones).
	CapVMCloneLinked = "vm-clone-linked"
	// CapImageImport: VMImage import/preparation
	// (status.reportedCapabilities.supportsImageImport).
	CapImageImport = "image-import"
	// CapDiskExport: disk export, the migration source side
	// (status.reportedCapabilities.supportsDiskExport).
	CapDiskExport = "disk-export"
	// CapDiskImport: disk import, the migration target side
	// (status.reportedCapabilities.supportsDiskImport).
	CapDiskImport = "disk-import"
)

// coreCapabilities are part of the provider contract every Provider
// implements, so they are available whatever the Provider reports.
var coreCapabilities = []string{CapVMCreate, CapVMDelete, CapVMPower, CapVMReconfigure, CapVMClone}

// reportedCapabilities maps each optional capability to the field of
// Provider.status.reportedCapabilities that grants it.
var reportedCapabilities = map[string]func(*infrav1beta1.ReportedCapabilities) bool{
	CapVMReconfigureOnline: func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsReconfigureOnline },
	CapVMSnapshot:          func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsSnapshots },
	CapVMSnapshotMemory:    func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsMemorySnapshots },
	CapVMCloneLinked:       func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsLinkedClones },
	CapImageImport:         func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsImageImport },
	CapDiskExport:          func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsDiskExport },
	CapDiskImport:          func(r *infrav1beta1.ReportedCapabilities) bool { return r.SupportsDiskImport },
}

// KnownCapabilities returns every capability name a specification may
// require, sorted.
func KnownCapabilities() []string {
	names := append([]string{}, coreCapabilities...)
	for name := range reportedCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isKnownCapability reports whether name is a capability the runner can
// evaluate.
func isKnownCapability(name string) bool {
	if _, ok := reportedCapabilities[name]; ok {
		return true
	}
	for _, c := range coreCapabilities {
		if c == name {
			return true
		}
	}
	return false
}

// ProviderCapabilities returns the capabilities provider offers, sorted: the
// core capabilities plus each optional one its
// status.reportedCapabilities grants. A Provider that has not reported its
// capabilities yet offers the core capabilities only.
func ProviderCapabilities(provider *infrav1beta1.Provider) []string {
	caps := append([]string{}, coreCapabilities...)
	if reported := provider.Status.ReportedCapabilities; reported != nil {
		for name, granted := range reportedCapabilities {
			if granted(reported) {
				caps = append(caps, name)
			}
		}
	}
	sort.Strings(caps)
	return caps
}

// missingCapabilities returns the entries of required that are not in have.
func missingCapabilities(required, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[c] = true
	}
	var missing []string
	for _, c := range required {
		if !set[c] {
			missing = append(missing, c)
		}
	}
	return missing
}
