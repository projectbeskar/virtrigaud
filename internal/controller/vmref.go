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
	"strings"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// errVMUnbound is the sentinel every UnboundVMError matches (errors.Is). A
// caller that only needs to branch uses isVMUnbound.
var errVMUnbound = errors.New("virtual machine has no confirmed host binding")

// errPlacementTopologyMismatch is the sentinel every
// PlacementTopologyMismatchError matches (errors.Is).
var errPlacementTopologyMismatch = errors.New("virtual machine placement does not match its provider's topology")

// reasonVMUnbound is the condition reason the VMSnapshot, VMClone and
// VMMigration controllers record on their OWN object while the clustered VM
// they act on has no confirmed host binding. It is the Placed condition's
// Unbound reason; the VirtualMachine controller alone sets Placed itself.
const reasonVMUnbound = k8s.ReasonUnbound

// UnboundVMError reports that a VirtualMachine on a clustered provider has no
// confirmed host binding (status.placement.host), so no per-VM provider call
// may be sent for it (ADR-0007 Addendum A, A1). The caller sets Placed=False /
// Unbound (on a VirtualMachine) or its own waiting condition, and requeues; it
// never lets the provider pick a host.
type UnboundVMError struct {
	// Namespace and Name identify the VirtualMachine.
	Namespace, Name string
	// Provider is the clustered Provider the VM references.
	Provider string
}

// Error implements error.
func (e *UnboundVMError) Error() string {
	return fmt.Sprintf("VirtualMachine %s/%s on clustered provider %q has no confirmed host binding (status.placement.host is empty); "+
		"per-VM calls are not sent until it is bound", e.Namespace, e.Name, e.Provider)
}

// Is makes errors.Is(err, errVMUnbound) true for every UnboundVMError.
func (e *UnboundVMError) Is(target error) bool { return target == errVMUnbound }

// PlacementTopologyMismatchError reports that a VirtualMachine records a
// clustered placement (status.placement.host or .pendingHost) but its Provider
// is not topology: cluster. That only happens if the Provider's topology was
// changed after the VM was placed (it is immutable since ADR-0007 Addendum A,
// but objects edited before that may exist). The VM is failed CLOSED: no
// per-VM call is sent, because the single-host path neither routes to the
// recorded host nor checks ownership — it could act on, or destroy, another
// tenant's same-named VM.
type PlacementTopologyMismatchError struct {
	// Namespace and Name identify the VirtualMachine.
	Namespace, Name string
	// Provider is the (non-clustered) Provider the VM references.
	Provider string
	// Host is the recorded host (bound or pending).
	Host string
}

// Error implements error.
func (e *PlacementTopologyMismatchError) Error() string {
	return fmt.Sprintf("VirtualMachine %s/%s records a placement on host %q but provider %q is not topology: cluster; "+
		"no provider call is made (the provider's topology must not change after VMs are placed)",
		e.Namespace, e.Name, e.Host, e.Provider)
}

// Is makes errors.Is(err, errPlacementTopologyMismatch) true for every
// PlacementTopologyMismatchError.
func (e *PlacementTopologyMismatchError) Is(target error) bool {
	return target == errPlacementTopologyMismatch
}

// isVMUnbound reports whether err is, or wraps, an UnboundVMError.
func isVMUnbound(err error) bool { return errors.Is(err, errVMUnbound) }

// isPlacementTopologyMismatch reports whether err is, or wraps, a
// PlacementTopologyMismatchError.
func isPlacementTopologyMismatch(err error) bool {
	return errors.Is(err, errPlacementTopologyMismatch)
}

// vmRefErrorReason is the condition reason for a vmRefFor failure recorded on
// a VMSnapshot / VMClone / VMMigration.
func vmRefErrorReason(err error) string {
	if isPlacementTopologyMismatch(err) {
		return k8s.ReasonPlacementTopologyMismatch
	}
	return reasonVMUnbound
}

// placementTopologyError returns a *PlacementTopologyMismatchError when vm
// records a clustered placement but provider is not clustered, and nil
// otherwise.
func placementTopologyError(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider) error {
	if provider == nil || isClusterTopology(provider) {
		return nil
	}
	host := boundHost(vm)
	if host == "" {
		host = pendingHost(vm)
	}
	if host == "" {
		return nil
	}
	return &PlacementTopologyMismatchError{Namespace: vm.Namespace, Name: vm.Name, Provider: provider.Name, Host: host}
}

// vmRefFor is THE one helper that builds the contracts.VMRef for a per-VM
// provider call on vm (ADR-0007 Addendum A, A1). provider is the Provider vm
// references (resolved from spec.providerRef); the VM, VMSnapshot, VMClone
// (source), VMMigration (source) controllers all go through it, so the routing
// rule lives in exactly one place:
//
//   - single-host / thin-client Provider (topology single, the default): the
//     ref carries status.id only — no host, no owner — byte-for-byte today's
//     call (D9). A VM that nevertheless records a clustered placement fails
//     CLOSED with a *PlacementTopologyMismatchError (no ref).
//   - clustered Provider (topology cluster): HostID is the confirmed binding,
//     status.placement.host, and Owner is the VM's identity, which the provider
//     checks against the VM's owner stamp. An empty binding returns an
//     *UnboundVMError and no ref: a clustered VM is never sent a per-VM call
//     without a host, because only the operator knows where a VM runs (D1).
func vmRefFor(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider) (contracts.VMRef, error) {
	if provider == nil || !isClusterTopology(provider) {
		if err := placementTopologyError(vm, provider); err != nil {
			return contracts.VMRef{}, err
		}
		return contracts.VMRef{ID: vm.Status.ID}, nil
	}
	host := boundHost(vm)
	if host == "" {
		return contracts.VMRef{}, &UnboundVMError{Namespace: vm.Namespace, Name: vm.Name, Provider: provider.Name}
	}
	return contracts.VMRef{ID: vm.Status.ID, HostID: host, Owner: vmOwnerIdentity(vm)}, nil
}

// pendingCreateRef builds the VMRef that addresses a clustered VM whose Create
// is in flight: status.placement.pendingHost is set but the provider has not
// confirmed the VM, so status.id is still empty (ADR-0007 Addendum A, A2). The
// id is the name Create was sent with (the VirtualMachine name), and the owner
// is the VM's identity. The operator does not know the provider's host-side
// name for the VM (the libvirt provider names a new domain
// "<namespace>.<name>"); a provider that derives one resolves the bare name
// through the owner, under its owner check. It is used for exactly two things, per A2: routing the Create
// retry, and the finalizer's owner-checked cleanup Delete. ok is false when no
// create is pending.
func pendingCreateRef(vm *infravirtrigaudiov1beta1.VirtualMachine) (contracts.VMRef, bool) {
	host := pendingHost(vm)
	if host == "" {
		return contracts.VMRef{}, false
	}
	return contracts.VMRef{ID: vm.Name, HostID: host, Owner: vmOwnerIdentity(vm)}, true
}

// vmOwnerIdentity is the requesting VirtualMachine's identity as it is stamped
// onto (and checked against) the hypervisor VM: Create stamps it (#333) and the
// routed Describe / owner-checked Delete require it (A2).
func vmOwnerIdentity(vm *infravirtrigaudiov1beta1.VirtualMachine) contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: string(vm.UID), Namespace: vm.Namespace, Name: vm.Name}
}

// boundHost returns the VM's confirmed host binding, or "".
func boundHost(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if vm.Status.Placement == nil {
		return ""
	}
	return strings.TrimSpace(vm.Status.Placement.Host)
}

// pendingHost returns the host a clustered VM's in-flight Create is aimed at,
// or "".
func pendingHost(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if vm.Status.Placement == nil {
		return ""
	}
	return strings.TrimSpace(vm.Status.Placement.PendingHost)
}
