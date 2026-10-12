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
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The disk-size webhooks refuse, at admission, a disk size above the
// manager-wide ceiling (--max-disk-size) on a VirtualMachine (spec.disks) or a
// VMClass (spec.diskDefaults.size). They are registered, and the chart renders
// their configuration, only when that ceiling is set: without one they have
// nothing to check and add no admission dependency. They check only the
// administrator's ceiling, not a Provider's own lower
// ProviderMaxDiskSizeAnnotation (a VMClass is not tied to a Provider): the
// controllers enforce both before any provider call, and they are the
// enforcement that holds when webhooks are off.
//
// Only GROWTH beyond the ceiling is refused: an object stored with a larger
// size before the ceiling was set (or lowered) can still be updated as long as
// that size does not grow, so its finalizers, labels and other fields stay
// editable and nothing is shrunk.

// +kubebuilder:webhook:path=/validate-infra-virtrigaud-io-v1beta1-virtualmachine,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.virtrigaud.io,resources=virtualmachines,verbs=create;update,versions=v1beta1,name=vvirtualmachine-disksize.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-infra-virtrigaud-io-v1beta1-vmclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=infra.virtrigaud.io,resources=vmclasses,verbs=create;update,versions=v1beta1,name=vvmclass-disksize.kb.io,admissionReviewVersions=v1

// VirtualMachineDiskSizeValidator refuses a VirtualMachine whose spec.disks
// asks for a disk above MaxDiskSize, or grows one beyond it. It never mutates
// and returns no warnings.
// +kubebuilder:object:generate=false
type VirtualMachineDiskSizeValidator struct {
	// MaxDiskSize is the manager-wide disk-size ceiling (--max-disk-size).
	MaxDiskSize resource.Quantity
}

// VMClassDiskSizeValidator refuses a VMClass whose spec.diskDefaults.size is
// above MaxDiskSize, or grows beyond it. It never mutates and returns no
// warnings.
// +kubebuilder:object:generate=false
type VMClassDiskSizeValidator struct {
	// MaxDiskSize is the manager-wide disk-size ceiling (--max-disk-size).
	MaxDiskSize resource.Quantity
}

var (
	_ webhook.CustomValidator = &VirtualMachineDiskSizeValidator{}
	_ webhook.CustomValidator = &VMClassDiskSizeValidator{}
)

// SetupDiskSizeWebhooksWithManager registers the VirtualMachine and VMClass
// disk-size validating webhooks with ceiling maxDiskSize. Callers register them
// only when webhook serving is configured AND a ceiling is set (see
// cmd/manager/main.go); the chart renders their configuration under the same
// two conditions.
func SetupDiskSizeWebhooksWithManager(mgr ctrl.Manager, maxDiskSize resource.Quantity) error {
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&VirtualMachine{}).
		WithValidator(&VirtualMachineDiskSizeValidator{MaxDiskSize: maxDiskSize}).
		Complete(); err != nil {
		return fmt.Errorf("register the VirtualMachine disk-size webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&VMClass{}).
		WithValidator(&VMClassDiskSizeValidator{MaxDiskSize: maxDiskSize}).
		Complete(); err != nil {
		return fmt.Errorf("register the VMClass disk-size webhook: %w", err)
	}
	return nil
}

// diskSizeLimitDetail is the field error detail of a size above the ceiling.
func diskSizeLimitDetail(limit resource.Quantity) string {
	return fmt.Sprintf("exceeds the disk-size limit %s set by the cluster administrator (--max-disk-size); "+
		"a disk can be created or grown only up to the limit", limit.String())
}

// ValidateCreate refuses a VirtualMachine with a spec.disks entry above the
// ceiling.
func (v *VirtualMachineDiskSizeValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	vm, ok := obj.(*VirtualMachine)
	if !ok {
		return nil, fmt.Errorf("expected a VirtualMachine object but got %T", obj)
	}
	return nil, v.validate(nil, vm)
}

// ValidateUpdate refuses a VirtualMachine update that adds a spec.disks entry
// above the ceiling, or grows one to a size above it. A disk already above it
// that keeps (or lowers) its size is admitted.
func (v *VirtualMachineDiskSizeValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	vm, ok := newObj.(*VirtualMachine)
	if !ok {
		return nil, fmt.Errorf("expected a VirtualMachine object but got %T", newObj)
	}
	old, ok := oldObj.(*VirtualMachine)
	if !ok {
		return nil, fmt.Errorf("expected a VirtualMachine object but got %T", oldObj)
	}
	return nil, v.validate(old, vm)
}

// ValidateDelete always admits.
func (v *VirtualMachineDiskSizeValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks vm's spec.disks against the ceiling; old (nil on create)
// supplies the sizes a disk of the same name already had.
func (v *VirtualMachineDiskSizeValidator) validate(old, vm *VirtualMachine) error {
	previous := map[string]int32{}
	if old != nil {
		for _, d := range old.Spec.Disks {
			previous[d.Name] = d.SizeGiB
		}
	}
	var errs field.ErrorList
	for i, d := range vm.Spec.Disks {
		size := DiskSizeGiBQuantity(d.SizeGiB)
		if size.Cmp(v.MaxDiskSize) <= 0 {
			continue
		}
		if had, ok := previous[d.Name]; ok && had >= d.SizeGiB {
			continue
		}
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "disks").Index(i).Child("sizeGiB"), d.SizeGiB, diskSizeLimitDetail(v.MaxDiskSize)))
	}
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("VirtualMachine").GroupKind(), vm.Name, errs)
}

// ValidateCreate refuses a VMClass whose spec.diskDefaults.size is above the
// ceiling.
func (v *VMClassDiskSizeValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	class, ok := obj.(*VMClass)
	if !ok {
		return nil, fmt.Errorf("expected a VMClass object but got %T", obj)
	}
	return nil, v.validate(nil, class)
}

// ValidateUpdate refuses a VMClass update that sets spec.diskDefaults.size
// above the ceiling and above what it was. A class already above it that keeps
// (or lowers) its size is admitted.
func (v *VMClassDiskSizeValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	class, ok := newObj.(*VMClass)
	if !ok {
		return nil, fmt.Errorf("expected a VMClass object but got %T", newObj)
	}
	old, ok := oldObj.(*VMClass)
	if !ok {
		return nil, fmt.Errorf("expected a VMClass object but got %T", oldObj)
	}
	return nil, v.validate(old, class)
}

// ValidateDelete always admits.
func (v *VMClassDiskSizeValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks class's spec.diskDefaults.size against the ceiling; old
// (nil on create) supplies the size it already had.
func (v *VMClassDiskSizeValidator) validate(old, class *VMClass) error {
	req, ok := ClassDiskSizeRequest(class)
	if !ok || req.Size.Cmp(v.MaxDiskSize) <= 0 {
		return nil
	}
	if prev, had := ClassDiskSizeRequest(old); had && prev.Size.Cmp(req.Size) >= 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("VMClass").GroupKind(), class.Name, field.ErrorList{field.Invalid(
		field.NewPath("spec", "diskDefaults", "size"), req.Size.String(), diskSizeLimitDetail(v.MaxDiskSize))})
}
