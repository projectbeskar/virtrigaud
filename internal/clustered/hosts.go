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

package clustered

import "slices"

// HostID identifies one hypervisor host a clustered provider fronts. Under
// ADR-0007 it equals the Host object name (a DNS-1123 subdomain).
type HostID string

// HostSet is the routable host set of a clustered provider: the hosts its host
// registry fronts and that are not draining.
type HostSet interface {
	// Hosts returns the ids of every routable host, sorted.
	Hosts() []HostID
}

// Fronts reports whether host is a routable host of set (known and not
// draining).
func Fronts(set HostSet, host HostID) bool {
	return slices.Contains(set.Hosts(), host)
}
