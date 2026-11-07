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

package operation

import (
	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Protection is the protection the manager requires for the mutating calls it
// sends to a Provider.
type Protection int

const (
	// ProtectionNone: mutating calls are sent as before, unsigned.
	ProtectionNone Protection = iota
	// ProtectionIdentity: every mutating call is sent as a signed operation
	// (OperationEnvelope with an operation grant) and recorded as an attempt
	// marker on the VirtualMachine before it is first sent. A call the
	// manager has no signed path for is refused locally, never sent unsigned.
	ProtectionIdentity
)

// String names p for logs and messages.
func (p Protection) String() string {
	if p == ProtectionIdentity {
		return "operation-identity"
	}
	return "none"
}

// RequiredProtection is the protection the manager requires for a Provider of
// type t. It comes only from the Provider's spec.type, which is immutable:
// the development-only cloudhypervisor type requires operation identity, and
// every other type requires none. What a Provider advertises
// (GetCapabilities fields 31-34) is unauthenticated: it is a hint that must
// match this answer, and never lowers it.
func RequiredProtection(t infravirtrigaudiov1beta1.ProviderType) Protection {
	if t == infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor {
		return ProtectionIdentity
	}
	return ProtectionNone
}
