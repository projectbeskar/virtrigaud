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

import "strings"

// VMRef addresses one hypervisor VM for a per-VM provider call (ADR-0007
// Addendum A, A1). Every per-VM method on Provider (and on the optional
// Cloner capability) takes a VMRef instead of a bare VM id, so the compiler
// finds every call site that must say which host a clustered provider should
// run the call on. The host is never passed as a context value.
type VMRef struct {
	// ID is the provider-specific VM identifier (VirtualMachine status.id).
	ID string
	// HostID names the host a clustered ("brain-in-operator") provider must run
	// the call on: the VM's confirmed binding, VirtualMachine
	// status.placement.host (ADR-0007 D3). The operator fills it only for a
	// Provider with topology: cluster. It is empty for single-host and
	// thin-client providers, which ignore it; the transport threads it to the
	// wire as target_host_id (source_host_id for a clone source), and a
	// clustered provider rejects an empty value instead of defaulting to a host.
	HostID string
	// Owner is the identity of the VirtualMachine the call is made for — the
	// identity Create stamps onto the hypervisor VM (#333). The operator fills it
	// with HostID on a clustered provider, and the transport sends it only with a
	// host (DescribeRequest.owner, DeleteRequest.owner; later slices reuse it for
	// the other per-VM requests). A clustered provider acts on, or reports, a VM
	// only when its owner stamp records Owner.UID, so a VirtualMachine can never
	// read or destroy another tenant's VM that took over the name on its host.
	Owner ObjectIdentity
}

// Routed reports whether the reference carries a host binding, i.e. whether it
// addresses a VM on a clustered provider. Whitespace-only is not a binding.
func (r VMRef) Routed() bool {
	return strings.TrimSpace(r.HostID) != ""
}
