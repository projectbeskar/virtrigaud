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

// ImagePrepareRequest contains the information needed to prepare/import a VM
// image into a provider so it can back subsequent VM creation. It is the
// manager-side, transport-agnostic mirror of the provider.v1 ImagePrepareRequest
// message (issue #154).
//
// Since ADR-0009 a request either carries the VMImage's identity (Image with a
// UID, SourceDigest, Provider) and an EMPTY TargetName — the provider then
// derives, stamps and verifies the artifact from that identity — or, from an
// older manager only, no identity and a bare TargetName (deprecated legacy
// mode, served for one release).
type ImagePrepareRequest struct {
	// ImageJSON is the JSON-encoded VMImage spec describing the image source
	// (e.g. source.vsphere.ovaURL, source.libvirt.path/url, source.proxmox.*).
	ImageJSON string
	// TargetName is the bare name of the prepared template/image of a legacy
	// (identity-less) request. It must be empty when Image is set: the provider
	// derives the artifact name from the identity (ADR-0009 D1, D7).
	TargetName string
	// StorageHint names the target storage location (vSphere datastore, libvirt
	// pool, Proxmox storage), or empty to let the provider choose.
	StorageHint string
	// Image identifies the VMImage being prepared (ADR-0009 D1, D4). Its UID is
	// authoritative: it is hashed into the artifact name and recorded in the
	// artifact's stamp, and an existing artifact is reused only when its stamp
	// carries the same UID. Namespace and Name are informational (the readable
	// name prefix, audit). An identity without a UID is not sent on the wire.
	Image ObjectIdentity
	// SourceDigest is the digest of the VMImage's canonical spec.source,
	// "sha256:" followed by 64 lowercase hex digits (ADR-0009 D2; computed by
	// imageartifact.SourceDigest). Required when Image is set.
	SourceDigest string
	// Provider identifies the Provider object the prepare runs through.
	// Informational only: recorded in the artifact's stamp as preparedBy, never
	// part of the reuse rule (ADR-0009 D3, D5).
	Provider ObjectIdentity
}

// ImagePrepareResponse contains the result of an image-prepare operation.
type ImagePrepareResponse struct {
	// TaskRef references an async operation when the prepare is not synchronous;
	// an empty TaskRef means the operation completed synchronously.
	TaskRef string
	// PreparedImageID is the provider-specific identifier of the prepared image
	// (e.g. a vSphere/Proxmox template name or VMID). It is known at trigger time
	// — even for async prepares — so the manager can stamp it onto VMImage.status
	// and use it as the template ref when creating VMs (issue #154, PR-6 / #214).
	// Empty for providers that address the prepared image by path only.
	PreparedImageID string
	// PreparedImagePath is the provider-specific path of the prepared image (e.g.
	// a libvirt pool path, <poolPath>/<target>.qcow2). Empty for providers that
	// address the prepared image by id/name only (vSphere, Proxmox).
	PreparedImagePath string
	// Artifact echoes the provenance stamp of the artifact the provider verified
	// or wrote for an identity request (ADR-0009 D3, D7). Nil from a provider
	// that predates ADR-0009 and in deprecated legacy mode. See
	// ConfirmsIdentity.
	Artifact *PreparedArtifact
}

// PreparedArtifact is the manager-side mirror of the provider.v1
// PreparedArtifact message: the stamp of a prepared-image artifact (ADR-0009
// D3). Only Image.UID and SourceDigest are authoritative.
//
// Everything in it comes from the hypervisor (the stamp the provider read or
// wrote), and anyone with write access to the image location can shape it:
// Image.Namespace and Image.Name in particular are untrusted. Never copy them
// into conditions, events or log messages as if they were facts; name the
// request's own VMImage instead (ADR-0009 D11).
type PreparedArtifact struct {
	// Name is the hypervisor-side artifact name the provider derived (D1).
	Name string
	// Image is the VMImage identity recorded in the stamp.
	Image ObjectIdentity
	// SourceDigest is the source digest recorded in the stamp.
	SourceDigest string
	// Reused is true when an existing artifact with a matching stamp was
	// reused, false when this call (or the task it returned) prepared it.
	Reused bool
}

// ConfirmsIdentity reports whether r proves that the provider prepared (or
// verified) the artifact for req's identity: r carries an Artifact with a
// non-empty Name whose Image.UID and SourceDigest are non-empty and equal
// req's (ADR-0009 D7). A response that does not confirm the identity — no
// Artifact (an older provider, or legacy mode), no artifact name, or another
// UID or digest — must not be recorded as prepared. The echoed namespace and
// name and Reused are never consulted (see PreparedArtifact).
//
// Artifact.Name is not compared with PreparedImageID: they differ by design on
// vSphere (the id is the template's absolute inventory path) and Proxmox (the
// id is the VMID); only libvirt uses the artifact name as the id.
func (r ImagePrepareResponse) ConfirmsIdentity(req ImagePrepareRequest) bool {
	if r.Artifact == nil || r.Artifact.Name == "" || req.Image.IsZero() || req.SourceDigest == "" {
		return false
	}
	return r.Artifact.Image.UID == req.Image.UID && r.Artifact.SourceDigest == req.SourceDigest
}

// ImagePreparer is an optional capability of a Provider: it prepares/imports a
// VM image into the provider (download and/or convert into a template or storage
// pool) so it can back subsequent VM creation. The manager gRPC client implements
// it; callers type-assert a Provider to ImagePreparer to invoke a prepare without
// widening the core Provider interface, so in-process provider implementations and
// test fakes that do not support it are unaffected (issue #154). This mirrors the
// Cloner pattern.
type ImagePreparer interface {
	// PrepareImage prepares the image described by req: under the artifact name
	// the provider derives from req.Image and req.SourceDigest (ADR-0009), or —
	// for a legacy request — under req.TargetName. Returns a TaskRef the caller
	// can poll via IsTaskComplete when the operation is asynchronous; an empty
	// TaskRef means it completed synchronously. The operation is idempotent:
	// preparing an image whose artifact already exists (with a matching stamp)
	// is a no-op success.
	PrepareImage(ctx context.Context, req ImagePrepareRequest) (ImagePrepareResponse, error)
}
