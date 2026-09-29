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

import (
	"context"
	"strings"
)

// OwnerFilter selects the VMs of ONE VirtualMachine in a ListVMs (ADR-0007
// A6.2, R4): the ones a provider would name for the VirtualMachine with this
// namespace and name, or whose owner stamp records them (under any UID). It is
// the manager-side mirror of ListVMsRequest.owner_namespace / owner_name. It
// selects; it never authorizes anything — only an owner UID identifies an
// owner.
type OwnerFilter struct {
	// Namespace is the VirtualMachine's namespace.
	Namespace string
	// Name is the VirtualMachine's name.
	Name string
}

// IsZero reports whether f selects nothing (an unfiltered listing).
func (f OwnerFilter) IsZero() bool {
	return f.Namespace == "" && f.Name == ""
}

// Complete reports whether f names both a namespace and a name; a filter with
// only one of them is invalid.
func (f OwnerFilter) Complete() bool {
	return f.Namespace != "" && f.Name != ""
}

// Matches reports whether info's owner stamp records f's namespace and name
// (whatever its UID). A VM with no stamp, with more than one, or with a stamp
// for another VirtualMachine does not match.
func (f OwnerFilter) Matches(info VMInfo) bool {
	return f.Complete() && info.OwnerNamespace == f.Namespace && info.OwnerName == f.Name
}

// OwnerUIDs returns the owner UIDs a provider reported on info
// (ProviderRaw[VMInfoOwnerUIDKey], comma-separated), without empties.
func OwnerUIDs(info VMInfo) []string {
	raw := strings.TrimSpace(info.ProviderRaw[VMInfoOwnerUIDKey])
	if raw == "" {
		return nil
	}
	var out []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// OwnerFilteredLister is an optional capability of a Provider: a ListVMs
// restricted to one VirtualMachine's candidates (OwnerFilter). The manager
// gRPC client implements it; the VirtualMachine controller type-asserts a
// Provider to it for the pre-schedule uniqueness check of a clustered VM
// (ADR-0007 A6.2, R4), and uses it only when the provider reports
// Capabilities.SupportsListOwnerFilter AND the answer carries
// VMList.OwnerFilterApplied. The core Provider interface and the fakes that do
// not filter are unaffected (the Cloner / OwnerTransferrer pattern).
type OwnerFilteredLister interface {
	// ListVMsForOwner lists the VMs of the VirtualMachine owner names, across
	// every host of a clustered provider. VMList.UnreachableHostIDs names the
	// hosts that could not be checked (unknown, not empty), and
	// VMList.OwnerFilterApplied is true only when the provider applied the
	// filter; an answer without it is an unfiltered listing from a provider
	// that ignored the filter.
	ListVMsForOwner(ctx context.Context, owner OwnerFilter) (VMList, error)
}
