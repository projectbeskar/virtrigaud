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
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// This file holds the controller half of the VirtualMachine provider binding.
//
// A VM's status.id is a provider-specific identifier (a Proxmox VMID, a vSphere
// MOID, a libvirt domain name) that is meaningful only on the hypervisor of the
// Provider that assigned it; the same id on another Provider may name an
// unrelated VM, possibly another tenant's. The CRD makes spec.providerRef
// immutable once the VM is bound; as defense in depth the operator records the
// Provider it bound the VM through (status.boundProvider) in the same status
// write that binds it, and refuses every per-VM provider call — and the
// finalizer's Delete — while spec.providerRef resolves to any other Provider.
//
// Bind paths (each records the Provider): VirtualMachine create
// (createVM, and recordPendingHost for a clustered create in flight), VMClone
// target bind (bindTargetVM) and adoption (adoptVM). A VM bound before the field
// existed is backfilled by the VirtualMachine controller on its first reconcile
// from its current spec.providerRef: that is trust on first reconcile, the only
// window in which a reference re-pointed BEFORE the upgrade is accepted.

// errProviderRefMismatch is the sentinel every ProviderRefMismatchError
// matches (errors.Is).
var errProviderRefMismatch = errors.New("virtual machine's spec.providerRef does not match the provider it is bound through")

// errReasonProviderRefMismatch is the metrics reason recorded when a bound VM's
// spec.providerRef no longer resolves to its bound Provider.
const errReasonProviderRefMismatch = "provider-ref-mismatch"

// providerRefMismatchRetryInterval re-checks a VM refused for a provider
// reference mismatch. Only an administrator can resolve it (spec.providerRef
// is immutable once bound; status.boundProvider is written through the status
// subresource, whose updates do not trigger a reconcile), so it re-checks
// slowly instead of hammering anything.
const providerRefMismatchRetryInterval = 2 * time.Minute

// ProviderRefMismatchError reports that a bound VirtualMachine's
// spec.providerRef resolves to a Provider other than the one recorded in
// status.boundProvider. No provider call may be made for the VM through the
// mismatched Provider: its status.id is meaningful only on the bound one.
type ProviderRefMismatchError struct {
	// Namespace and Name identify the VirtualMachine.
	Namespace, Name string
	// Bound is the Provider the VM is bound through.
	Bound infravirtrigaudiov1beta1.BoundProviderRef
	// Current is the Provider spec.providerRef resolves to now.
	Current types.NamespacedName
	// CurrentUID is the UID of the Provider object spec.providerRef resolves
	// to, when it was compared ("" when only the reference was checked).
	CurrentUID types.UID
}

// Error implements error.
func (e *ProviderRefMismatchError) Error() string {
	if e.Bound.Namespace == e.Current.Namespace && e.Bound.Name == e.Current.Name {
		return fmt.Sprintf("VirtualMachine %s/%s is bound through Provider %s/%s (uid %s), but that Provider now has uid %s "+
			"(it was deleted and re-created); no provider call is made. If the re-created Provider manages the same "+
			"hypervisor, an administrator may clear status.boundProvider to re-accept it",
			e.Namespace, e.Name, e.Bound.Namespace, e.Bound.Name, e.Bound.UID, e.CurrentUID)
	}
	return fmt.Sprintf("VirtualMachine %s/%s is bound through Provider %s/%s, but spec.providerRef now resolves to %s/%s; "+
		"its provider id is meaningful only on the bound Provider, so no provider call is made "+
		"(to detach the VM without deleting the hypervisor VM, set the %s=true annotation and delete it)",
		e.Namespace, e.Name, e.Bound.Namespace, e.Bound.Name, e.Current.Namespace, e.Current.Name,
		infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
}

// Is makes errors.Is(err, errProviderRefMismatch) true for every
// ProviderRefMismatchError.
func (e *ProviderRefMismatchError) Is(target error) bool { return target == errProviderRefMismatch }

// isProviderRefMismatch reports whether err is, or wraps, a
// ProviderRefMismatchError.
func isProviderRefMismatch(err error) bool { return errors.Is(err, errProviderRefMismatch) }

// vmProviderKey returns the Provider vm's spec.providerRef resolves to: its
// namespace defaults to the VM's own.
func vmProviderKey(vm *infravirtrigaudiov1beta1.VirtualMachine) types.NamespacedName {
	key := types.NamespacedName{Namespace: vm.Spec.ProviderRef.Namespace, Name: vm.Spec.ProviderRef.Name}
	if key.Namespace == "" {
		key.Namespace = vm.Namespace
	}
	return key
}

// vmIsBound reports whether vm is bound to a hypervisor VM: it has a provider
// id, or a clustered create is in flight (status.placement.pendingHost). This
// is the state in which the CRD locks spec.providerRef.
func vmIsBound(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.Status.ID != "" || pendingHost(vm) != ""
}

// boundProviderRefFor builds the status.boundProvider record for provider.
func boundProviderRefFor(provider *infravirtrigaudiov1beta1.Provider) *infravirtrigaudiov1beta1.BoundProviderRef {
	return &infravirtrigaudiov1beta1.BoundProviderRef{
		Namespace: provider.Namespace,
		Name:      provider.Name,
		UID:       string(provider.UID),
	}
}

// recordBoundProvider records provider as the Provider vm is bound through. A
// bind site calls it on the in-memory object it is about to persist, so the
// record lands in the same status write as the binding (status.id or
// status.placement.pendingHost).
func recordBoundProvider(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider) {
	vm.Status.BoundProvider = boundProviderRefFor(provider)
}

// checkBoundProvider returns a *ProviderRefMismatchError when vm records a
// bound Provider (status.boundProvider) that is not key or — when both UIDs are
// known — not the object with uid. A VM with no record returns nil: it is
// either unbound (nothing to protect) or bound before the record existed, and
// the VirtualMachine controller backfills it on its next reconcile. Pass an
// empty uid to compare the reference only (before the Provider is fetched).
func checkBoundProvider(vm *infravirtrigaudiov1beta1.VirtualMachine, key types.NamespacedName, uid types.UID) error {
	bound := vm.Status.BoundProvider
	if bound == nil {
		return nil
	}
	boundNS := bound.Namespace
	if boundNS == "" {
		boundNS = vm.Namespace
	}
	if boundNS == key.Namespace && bound.Name == key.Name &&
		(bound.UID == "" || uid == "" || types.UID(bound.UID) == uid) {
		return nil
	}
	b := *bound
	b.Namespace = boundNS
	return &ProviderRefMismatchError{Namespace: vm.Namespace, Name: vm.Name, Bound: b, Current: key, CurrentUID: uid}
}

// checkVMProvider is checkBoundProvider for a fetched Provider object: the
// Provider vm's spec.providerRef resolved to. A nil provider is not checked.
func checkVMProvider(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider) error {
	if provider == nil {
		return nil
	}
	return checkBoundProvider(vm, types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}, provider.UID)
}

// hasOrphanOnDeleteAnnotation reports whether vm carries the orphan-on-delete
// annotation set to "true": deleting it detaches the hypervisor VM instead of
// destroying it.
func hasOrphanOnDeleteAnnotation(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.Annotations[infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation] == "true"
}
