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
