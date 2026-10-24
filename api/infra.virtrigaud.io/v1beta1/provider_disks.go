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

// ProviderIgnoresAdditionalDisks reports whether a Provider of type t is known
// NOT to attach a VirtualMachine's spec.disks: its Create attaches none of them
// and its Reconfigure applies none of them (docs/reconfigure-results.md, "Which
// disk a size goes to"). It is true for libvirt and Proxmox. vSphere attaches
// them at create, and every other type is not claimed either way (false).
//
// The manager reports such a VM on its DisksApplied condition and a Warning
// event, and the VirtualMachine validating webhook returns an admission
// warning; the spec is never refused. This is a manager-side table, kept in
// step with the providers by hand: the long-term fix is a provider capability
// bit in GetCapabilitiesResponse, so a provider that learns to attach
// additional disks says so itself.
func ProviderIgnoresAdditionalDisks(t ProviderType) bool {
	switch t {
	case ProviderTypeLibvirt, ProviderTypeProxmox:
		return true
	}
	return false
}
