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

package v1beta1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ProviderMaxDiskSizeAnnotation is the Provider annotation with which the
// Provider's owner caps the size of every disk VirtRigaud creates or grows
// through that Provider: a VM's disks (VMClass spec.diskDefaults.size and
// spec.disks[].sizeGiB), a clone's and a migration's. The value is a resource
// quantity of at least 1Gi (e.g. "500Gi"). It can only LOWER the manager-wide
// ceiling (the manager's --max-disk-size): the limit that applies is the
// smaller of the two (EffectiveMaxDiskSize). On a Provider whose consumers live
// in other namespaces (spec.consumerNamespaceSelector) the Provider's owner is
// the administrator of those consumers, and this is their cap.
const ProviderMaxDiskSizeAnnotation = "infra.virtrigaud.io/max-disk-size"

// bytesPerDiskGiB is the size of one GiB, the unit of DiskSpec.SizeGiB.
const bytesPerDiskGiB = int64(1) << 30

// ParseMaxDiskSize parses a disk-size limit (the manager's --max-disk-size or
// a ProviderMaxDiskSizeAnnotation value): a resource quantity of at least 1Gi.
// Disk sizes are whole GiB, so a smaller limit would refuse every disk — most
// likely a missing unit ("500" is 500 bytes); it is refused, not applied. Use a
// binary suffix (Gi, Ti): "500G" is 500*10^9 bytes, about 465.7Gi, so a 500 GiB
// disk exceeds it.
func ParseMaxDiskSize(value string) (*resource.Quantity, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("invalid disk-size limit %q: want a quantity such as 500Gi: %w", value, err)
	}
	if q.Cmp(*resource.NewQuantity(bytesPerDiskGiB, resource.BinarySI)) < 0 {
		return nil, fmt.Errorf("invalid disk-size limit %q: must be at least 1Gi (disk sizes are whole GiB; use a unit such as Gi or Ti)", value)
	}
	return &q, nil
}

// DiskSizeGiBQuantity is the quantity of a disk size given in whole GiB
// (DiskSpec.SizeGiB), for comparison with a disk-size limit.
func DiskSizeGiBQuantity(gib int32) resource.Quantity {
	return *resource.NewQuantity(int64(gib)*bytesPerDiskGiB, resource.BinarySI)
}

// EffectiveMaxDiskSize is the disk-size limit for disks created or grown
// through provider: the smaller of ceiling (the manager's --max-disk-size; nil
// for none) and the provider's ProviderMaxDiskSizeAnnotation (absent for none).
// nil means no limit beyond the CRD's own bounds. An annotation that
// ParseMaxDiskSize refuses (the Provider webhook refuses it too, but the
// webhook may be off) is an error: callers must fail closed — create and grow
// nothing through the Provider — rather than ignore the owner's cap.
func EffectiveMaxDiskSize(ceiling *resource.Quantity, provider *Provider) (*resource.Quantity, error) {
	limit := ceiling
	if provider == nil {
		return limit, nil
	}
	value, ok := provider.Annotations[ProviderMaxDiskSizeAnnotation]
	if !ok {
		return limit, nil
	}
	own, err := ParseMaxDiskSize(value)
	if err != nil {
		return nil, fmt.Errorf("provider %s/%s annotation %s: %w", provider.Namespace, provider.Name, ProviderMaxDiskSizeAnnotation, err)
	}
	if limit == nil || own.Cmp(*limit) < 0 {
		return own, nil
	}
	return limit, nil
}

// DiskSizeRequest is one disk size a VirtualMachine, VMClone or VMMigration
// asks a provider to create or grow, and where it comes from. It is a helper
// value, not an API object, so it is excluded from DeepCopy generation.
// +kubebuilder:object:generate=false
type DiskSizeRequest struct {
	// Field names where the size is set, for messages (e.g.
	// "spec.disks[data].sizeGiB", or "VMClass web spec.diskDefaults.size").
	Field string
	// Size is the requested size.
	Size resource.Quantity
}

// ClassDiskSizeRequest is the primary-disk size class asks for
// (spec.diskDefaults.size: providers create, or grow an imported disk, to it),
// and false when the class sets none.
func ClassDiskSizeRequest(class *VMClass) (DiskSizeRequest, bool) {
	if class == nil || class.Spec.DiskDefaults == nil || class.Spec.DiskDefaults.Size.IsZero() {
		return DiskSizeRequest{}, false
	}
	return DiskSizeRequest{
		Field: fmt.Sprintf("VMClass %s spec.diskDefaults.size", class.Name),
		Size:  class.Spec.DiskDefaults.Size,
	}, true
}

// SpecDiskSizeRequests are the sizes of disks, the additional disks of a
// VirtualMachine (spec.disks), each named by its field.
func SpecDiskSizeRequests(disks []DiskSpec) []DiskSizeRequest {
	out := make([]DiskSizeRequest, 0, len(disks))
	for _, d := range disks {
		out = append(out, DiskSizeRequest{
			Field: fmt.Sprintf("spec.disks[%s].sizeGiB", d.Name),
			Size:  DiskSizeGiBQuantity(d.SizeGiB),
		})
	}
	return out
}

// FirstDiskOverLimit returns the first of disks whose size is above limit,
// and false when none is (or limit is nil).
func FirstDiskOverLimit(disks []DiskSizeRequest, limit *resource.Quantity) (DiskSizeRequest, bool) {
	if limit == nil {
		return DiskSizeRequest{}, false
	}
	for _, d := range disks {
		if d.Size.Cmp(*limit) > 0 {
			return d, true
		}
	}
	return DiskSizeRequest{}, false
}
