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

package imageartifact

import (
	"fmt"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// StampVersion is the only stamp version this release writes and trusts
// (ADR-0009 D3). A stamp of any other version is untrusted.
const StampVersion = 1

// StampIdentity is an object identity as recorded in a stamp.
type StampIdentity struct {
	// UID is the object's Kubernetes UID.
	UID string `json:"uid"`
	// Namespace is the object's namespace (informational).
	Namespace string `json:"namespace"`
	// Name is the object's name (informational).
	Name string `json:"name"`
}

// stampIdentity converts a contracts identity to its stamp form.
func stampIdentity(o contracts.ObjectIdentity) StampIdentity {
	return StampIdentity{UID: o.UID, Namespace: o.Namespace, Name: o.Name}
}

// Identity returns s as a contracts.ObjectIdentity.
func (s StampIdentity) Identity() contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: s.UID, Namespace: s.Namespace, Name: s.Name}
}

// Stamp is the provenance record a provider writes on every artifact it
// prepares (ADR-0009 D3): which VMImage (by UID) and which source (by digest)
// it holds, and — for audit only — who prepared it and when. Only Image.UID and
// SourceDigest are authoritative (Matches); the rest is never consulted for
// reuse. A stamp holds no secrets and no URLs. The JSON field names are the
// stamp's serialized form (the libvirt sidecar); where a stamp is stored is
// provider-specific.
type Stamp struct {
	// StampVersion is StampVersion.
	StampVersion int `json:"stampVersion"`
	// Image is the VMImage the artifact was prepared for.
	Image StampIdentity `json:"image"`
	// SourceDigest is the source digest the artifact was prepared from.
	SourceDigest string `json:"sourceDigest"`
	// PreparedBy is the Provider object the prepare ran through (audit only;
	// empty when the request did not name one).
	PreparedBy StampIdentity `json:"preparedBy"`
	// PreparedAt is when the prepare started, RFC 3339 in UTC.
	PreparedAt string `json:"preparedAt"`
}

// NewStamp returns the stamp an identity request req writes on the artifact it
// prepares, started at preparedAt.
func NewStamp(req Request, preparedAt time.Time) Stamp {
	return Stamp{
		StampVersion: StampVersion,
		Image:        stampIdentity(req.Image),
		SourceDigest: req.SourceDigest,
		PreparedBy:   stampIdentity(req.PreparedBy),
		PreparedAt:   preparedAt.UTC().Format(time.RFC3339),
	}
}

// Validate returns an error unless s can be trusted at all: a known version,
// a plausible image UID and a well-formed source digest. An untrusted stamp is
// treated exactly like no stamp (ADR-0009 D3): the artifact is never reused.
func (s Stamp) Validate() error {
	if s.StampVersion != StampVersion {
		return fmt.Errorf("stamp version %d is not %d", s.StampVersion, StampVersion)
	}
	if err := validateUID(s.Image.UID); err != nil {
		return fmt.Errorf("stamp image: %w", err)
	}
	if err := ValidateSourceDigest(s.SourceDigest); err != nil {
		return fmt.Errorf("stamp: %w", err)
	}
	return nil
}

// Matches reports whether s is a trusted stamp (Validate) for req's identity:
// the same image UID and the same source digest (ADR-0009 D4 conditions 3 and
// 4). The image namespace and name and PreparedBy are never consulted.
func (s Stamp) Matches(req Request) bool {
	return s.Validate() == nil && req.Image.UID != "" &&
		s.Image.UID == req.Image.UID && s.SourceDigest == req.SourceDigest
}

// PreparedArtifact returns the ImagePrepareResponse.artifact echo of the
// artifact named name that carries s (ADR-0009 D7). The echo reports the
// stamp — the identity the provider verified or wrote — not the request.
func (s Stamp) PreparedArtifact(name string, reused bool) *providerv1.PreparedArtifact {
	return &providerv1.PreparedArtifact{
		Name: name,
		Image: &providerv1.ObjectIdentity{
			Uid:       s.Image.UID,
			Namespace: s.Image.Namespace,
			Name:      s.Image.Name,
		},
		SourceDigest: s.SourceDigest,
		Reused:       reused,
	}
}

// Observation is what a provider found at an artifact's derived name
// (ADR-0009 D4). A failed probe is not an observation: it is a retryable
// error, never "absent".
type Observation struct {
	// Exists is true when anything occupies the name: the artifact, OR any
	// part of it such as a stamp without its artifact (a libvirt sidecar whose
	// file is missing). A provider must never report Exists=false together
	// with a stamp, Complete or Live; Decide treats that inconsistent
	// observation as a Conflict.
	Exists bool
	// Complete is true when the object is a finished artifact (vSphere: a
	// template; libvirt: the file and its sidecar with matching inode and
	// size; Proxmox: a template whose stamp names its own VMID).
	Complete bool
	// Live is true when an incomplete object is still being prepared (a
	// running task, a lease, a recently written file). Ignored when Complete.
	Live bool
	// Stamp is the object's stamp, or nil when it has none or its stamp could
	// not be parsed (both untrusted).
	Stamp *Stamp
}

// Outcome is what a provider does about an Observation (ADR-0009 D4).
type Outcome int

const (
	// OutcomeImport means nothing occupies the name: import the artifact.
	OutcomeImport Outcome = iota + 1
	// OutcomeReuse means a complete artifact with a matching stamp: reuse it.
	OutcomeReuse
	// OutcomeInProgress means an incomplete artifact with a matching stamp is
	// still being prepared: return a retryable error (InProgressError).
	OutcomeInProgress
	// OutcomeAbandoned means an incomplete artifact with a matching stamp that
	// is no longer being prepared, left by a crashed prepare of this same
	// image: the provider may remove only that object, then import again.
	OutcomeAbandoned
	// OutcomeConflict means anything else: no stamp, an untrusted stamp,
	// another UID or another digest. Never overwrite, delete, re-stamp or
	// adopt it; return ConflictError.
	OutcomeConflict
)

// String returns the outcome's lowercase name.
func (o Outcome) String() string {
	switch o {
	case OutcomeImport:
		return "import"
	case OutcomeReuse:
		return "reuse"
	case OutcomeInProgress:
		return "in_progress"
	case OutcomeAbandoned:
		return "abandoned"
	case OutcomeConflict:
		return "conflict"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Decide is the ADR-0009 D4 reuse rule for an identity request req: an object
// at the derived name is reused only when it is complete and its stamp
// Matches; an incomplete matching object is in progress while live and
// abandoned otherwise; every other object is a Conflict. The name is free
// (Import) only when nothing at all was observed: an observation that claims
// the name is free but carries a stamp, or claims Complete or Live, is
// inconsistent and fails closed as a Conflict, so an orphaned stamp (e.g. a
// foreign libvirt sidecar) is never treated as free space.
func Decide(obs Observation, req Request) Outcome {
	switch {
	case !obs.Exists && (obs.Stamp != nil || obs.Complete || obs.Live):
		return OutcomeConflict
	case !obs.Exists:
		return OutcomeImport
	case obs.Stamp == nil || !obs.Stamp.Matches(req):
		return OutcomeConflict
	case obs.Complete:
		return OutcomeReuse
	case obs.Live:
		return OutcomeInProgress
	default:
		return OutcomeAbandoned
	}
}
