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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Units and maxima for converting VMClass quantities into the provider
// contract's int32 fields (contracts.VMClass.MemoryMiB,
// contracts.DiskDefaults.SizeGiB).
const (
	bytesPerMiB = int64(1) << 20
	bytesPerGiB = int64(1) << 30

	// maxVMClassMemoryBytes is the exclusive VMClass memory maximum (100Ti).
	// It matches the CRD's validation rules on spec.memory (vmclass_types.go);
	// the operator enforces it too, for objects stored before those rules.
	maxVMClassMemoryBytes = int64(100) << 40
	// maxVMClassDiskBytes is the exclusive VMClass diskDefaults.size maximum
	// (1Pi), matching the CRD's validation rules on it.
	maxVMClassDiskBytes = int64(1) << 50
	// maxVMClassCPU is the CRD maximum for both VMClass.Spec.CPU and
	// VirtualMachineResources.CPU (vmclass_types.go / virtualmachine_types.go
	// both cap at 128 vCPUs via kubebuilder Maximum). The operator enforces it
	// again in effectiveResources, for a spec.resources override stored before
	// that CRD validation existed.
	maxVMClassCPU = int32(128)
	// minVMClassCPU is the CRD minimum for both fields (kubebuilder Minimum=1).
	minVMClassCPU = int32(1)
	// minVMResourceOverrideMemoryMiB is the CRD minimum for
	// VirtualMachineResources.MemoryMiB (kubebuilder Minimum=128).
	minVMResourceOverrideMemoryMiB = int64(128)
)

// vmClassQuantityUnits converts a VMClass quantity into whole units of
// unitBytes for an int32 field of the provider contract. A negative quantity,
// or one at or above maxBytes (exclusive), is refused with an InvalidSpec error
// naming field — it is never wrapped around or silently clamped into a
// different size. Both maxima keep the unit count far inside the int32 range
// (100Ti is 104857600 MiB; 1Pi is 1048576 GiB).
func vmClassQuantityUnits(field string, q resource.Quantity, unitBytes, maxBytes int64) (int32, error) {
	if q.Sign() < 0 {
		return 0, contracts.NewInvalidSpecError(fmt.Sprintf("VMClass %s must not be negative (got %s)", field, q.String()), nil)
	}
	if q.Cmp(*resource.NewQuantity(maxBytes, resource.BinarySI)) >= 0 {
		return 0, contracts.NewInvalidSpecError(fmt.Sprintf("VMClass %s %s is not below the maximum %s",
			field, q.String(), resource.NewQuantity(maxBytes, resource.BinarySI).String()), nil)
	}
	return int32(q.Value() / unitBytes), nil // #nosec G115 -- bounded above: maxBytes/unitBytes < MaxInt32
}

// effectiveResources computes the CPU and memory (MiB) a VirtualMachine
// actually asks a provider for: the VMClass values with any spec.resources
// override applied, field by field. It is the single place that computes
// this — used to build a Create/Reconfigure request, to decide in
// needsReconfigure whether one is needed, and to record what a confirmed
// Create/Reconfigure applied in status.currentResources — so a
// spec.resources override can no longer be compared against and recorded in
// status.currentResources (needsReconfigure, updateCurrentResources) while
// never actually being sent to the provider (buildCreateRequest), which is
// what let a VM report a size it never had.
//
// An override is validated against the same bounds a VMClass value is
// validated against: vmClassQuantityUnits for memory (so it can never
// overflow the int32 the wire contract carries), and [minVMClassCPU,
// maxVMClassCPU] for CPU — matching the CRD's own kubebuilder bounds on both
// VMClass and VirtualMachineResources, enforced again here for an object
// stored before that CRD validation existed. An out-of-bounds override
// returns an InvalidSpec error naming the field: never a clamped, wrapped, or
// silently-ignored value, and never a provider call.
func effectiveResources(
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	vmClass *infravirtrigaudiov1beta1.VMClass,
) (cpu int32, memoryMiB int32, err error) {
	cpu = vmClass.Spec.CPU
	memoryMiB, err = vmClassQuantityUnits("memory", vmClass.Spec.Memory, bytesPerMiB, maxVMClassMemoryBytes)
	if err != nil {
		return 0, 0, err
	}

	res := vm.Spec.Resources
	if res == nil {
		return cpu, memoryMiB, nil
	}

	if res.CPU != nil {
		overrideCPU := *res.CPU
		if overrideCPU < minVMClassCPU || overrideCPU > maxVMClassCPU {
			return 0, 0, contracts.NewInvalidSpecError(
				fmt.Sprintf("spec.resources.cpu %d is out of range [%d, %d]", overrideCPU, minVMClassCPU, maxVMClassCPU), nil)
		}
		cpu = overrideCPU
	}

	if res.MemoryMiB != nil {
		overrideMiB := *res.MemoryMiB
		maxMemoryMiB := maxVMClassMemoryBytes / bytesPerMiB
		if overrideMiB < minVMResourceOverrideMemoryMiB || overrideMiB >= maxMemoryMiB {
			return 0, 0, contracts.NewInvalidSpecError(
				fmt.Sprintf("spec.resources.memoryMiB %d is not in range [%d, %d)", overrideMiB, minVMResourceOverrideMemoryMiB, maxMemoryMiB), nil)
		}
		memoryMiB = int32(overrideMiB) // #nosec G115 -- bounded above by maxMemoryMiB, itself far below MaxInt32
	}

	return cpu, memoryMiB, nil
}
