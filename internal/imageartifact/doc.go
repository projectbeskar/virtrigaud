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

// Package imageartifact implements the shared, hypervisor-agnostic rules of
// prepared-image artifact identity (ADR-0009,
// docs/adr/0009-prepared-image-artifact-identity.md).
//
// A prepared image (a vSphere template, a libvirt pool file, a Proxmox
// template) is identified by the VMImage's UID and the digest of its
// spec.source. The package holds the pieces of that design that must be the
// same everywhere:
//
//   - SourceDigest (D2): the manager's digest of a VMImage's spec.source. Only
//     the manager computes it; a provider checks its syntax
//     (ValidateSourceDigest) and uses it verbatim.
//   - ArtifactName (D1): the artifact name a PROVIDER derives from the image
//     identity and the source digest, per hypervisor (NameRule). The manager
//     never derives an artifact name.
//   - Stamp (D3): the provenance record a provider writes on every artifact it
//     prepares, and Decide (D4): the fail-closed rule that reuses an existing
//     artifact only when its stamp carries the request's UID and digest.
//   - ParseRequest (D7): the provider-side decoding of an ImagePrepareRequest
//     into an identity request or a deprecated legacy request, and
//     SignalLegacyRequest, the deprecation signal every provider emits for a
//     legacy request.
//
// Where a stamp lives and how completeness and liveness are observed are
// hypervisor-specific and stay in each provider.
package imageartifact
