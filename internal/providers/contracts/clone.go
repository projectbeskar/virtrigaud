/*
Copyright 2025.

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

import "context"

// CloneRequest contains all information needed to clone an existing VM into a
// new one. It is the manager-side, transport-agnostic mirror of the
// provider.v1 CloneRequest message (issue #179).
type CloneRequest struct {
	// SourceVmID is the provider-specific identifier of the VM to clone from.
	SourceVmID string
	// TargetName is the desired name of the cloned VM.
	TargetName string
	// Linked requests a copy-on-write linked clone when true. Best-effort:
	// providers that cannot honor it fall back to a full clone unless the
	// caller has already gated on SupportsLinkedClones.
	Linked bool
	// ClassJSON is a JSON-encoded VMClass override for the target VM, or
	// empty to inherit the source VM's resources.
	ClassJSON string
	// PlacementJSON is a JSON-encoded placement hint for the target VM, or
	// empty to let the provider choose.
	PlacementJSON string
	// CustomizeJSON is a JSON-encoded customization spec (hostname, network,
	// cloud-init, sysprep, ...), or empty for no customization.
	CustomizeJSON string
}

// CloneResponse contains the result of a clone operation.
type CloneResponse struct {
	// TargetVmID is the provider-specific identifier of the newly cloned VM.
	TargetVmID string
	// TaskRef references an async operation if the clone is not synchronous.
	TaskRef string
}

// Cloner is an optional capability of a Provider: it clones an existing VM
// into a new one. The manager gRPC client implements it; callers type-assert a
// Provider to Cloner to invoke a clone without widening the core Provider
// interface, so in-process provider implementations and test fakes that do not
// support cloning are unaffected (issue #179). This mirrors the
// CapabilityReporter pattern.
type Cloner interface {
	// Clone clones SourceVmID into a new VM named TargetName. Returns the new
	// VM's provider identifier and, when the operation is asynchronous, a
	// TaskRef the caller can poll via IsTaskComplete.
	Clone(ctx context.Context, req CloneRequest) (CloneResponse, error)
}
