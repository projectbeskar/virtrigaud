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

package vsphere

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
)

// Prepared-image artifact stamps and the reuse decision (ADR-0009 D3, D4, D6).
//
// A prepared image is a vSphere template in the Provider's import folder,
// named imageartifact.ArtifactName(NameRuleVSphere, …). Its provenance stamp
// lives in ExtraConfig under virtrigaud.image.*, inside the reserved
// "virtrigaud." prefix: stripReservedExtraConfig removes any such key an OVF
// carries, and the real stamp is added to the import spec afterwards, before
// ImportVApp, so the entity carries it from the moment it exists.

const (
	// imageStampKeyPrefix is the ExtraConfig namespace of the image stamp.
	imageStampKeyPrefix = reservedExtraConfigPrefix + "image."
	// imageStampKeyVersion holds the stamp version (imageartifact.StampVersion).
	imageStampKeyVersion = imageStampKeyPrefix + "stampversion"
	// imageStampKeyUID holds the VMImage UID. Authoritative (ADR-0009 D4).
	imageStampKeyUID = imageStampKeyPrefix + "uid"
	// imageStampKeyNamespace holds the VMImage namespace. Informational.
	imageStampKeyNamespace = imageStampKeyPrefix + "namespace"
	// imageStampKeyName holds the VMImage name. Informational.
	imageStampKeyName = imageStampKeyPrefix + "name"
	// imageStampKeySourceDigest holds the source digest. Authoritative.
	imageStampKeySourceDigest = imageStampKeyPrefix + "sourcedigest"
	// imageStampKeyPreparedBy holds the Provider as "namespace/name/uid".
	// Informational.
	imageStampKeyPreparedBy = imageStampKeyPrefix + "preparedby"
	// imageStampKeyPreparedAt holds when the import started (RFC 3339). An
	// unfinished artifact is aged by it (ADR-0009 D4).
	imageStampKeyPreparedAt = imageStampKeyPrefix + "preparedat"

	// preparedBySeparator joins the parts of the preparedby value. None of a
	// Kubernetes namespace, name or UID can contain it.
	preparedBySeparator = "/"

	// minArtifactStalenessBound is the floor of the staleness bound of an
	// unfinished artifact (ADR-0009 D4, Q4).
	minArtifactStalenessBound = 2 * time.Hour

	// destroyTaskMethod is the vSphere method name of Destroy_Task, as listed
	// in a managed entity's disabledMethod whenever vCenter blocks it. (An
	// active import lease does NOT disable it on vCenter 8.0.2 — verified; see
	// artifactObject.observation.)
	destroyTaskMethod = "Destroy_Task"

	// maxPrepareTimeout caps spec.prepare.timeout for the staleness bound, so
	// an absurd value neither overflows nor shrinks the bound.
	maxPrepareTimeout = 24 * 365 * time.Hour
)

// imageStampKeys are every ExtraConfig key of the image stamp, in the order
// they are written.
var imageStampKeys = []string{
	imageStampKeyVersion,
	imageStampKeyUID,
	imageStampKeyNamespace,
	imageStampKeyName,
	imageStampKeySourceDigest,
	imageStampKeyPreparedBy,
	imageStampKeyPreparedAt,
}

// imageStampExtraConfig returns the ExtraConfig entries that stamp s on a
// template being imported. An empty preparedby (the request named no
// Provider) is left out: vSphere drops an ExtraConfig key whose value is "".
func imageStampExtraConfig(s imageartifact.Stamp) []types.BaseOptionValue {
	values := map[string]string{
		imageStampKeyVersion:      strconv.Itoa(s.StampVersion),
		imageStampKeyUID:          s.Image.UID,
		imageStampKeyNamespace:    s.Image.Namespace,
		imageStampKeyName:         s.Image.Name,
		imageStampKeySourceDigest: s.SourceDigest,
		imageStampKeyPreparedAt:   s.PreparedAt,
	}
	if s.PreparedBy.UID != "" {
		values[imageStampKeyPreparedBy] = strings.Join(
			[]string{s.PreparedBy.Namespace, s.PreparedBy.Name, s.PreparedBy.UID}, preparedBySeparator)
	}
	out := make([]types.BaseOptionValue, 0, len(values))
	for _, key := range imageStampKeys {
		if v, ok := values[key]; ok {
			out = append(out, &types.OptionValue{Key: key, Value: v})
		}
	}
	return out
}

// clearedImageStampExtraConfig returns every image-stamp key with an empty
// value. vSphere removes an ExtraConfig key set to "", so Create and Clone add
// these to the new VM's config: a clone of a prepared template (which copies
// the template's ExtraConfig) never carries the template's image stamp
// (ADR-0009 D3). The same mechanism clears the owner stamp (ownerExtraConfig).
func clearedImageStampExtraConfig() []types.BaseOptionValue {
	out := make([]types.BaseOptionValue, 0, len(imageStampKeys))
	for _, key := range imageStampKeys {
		out = append(out, &types.OptionValue{Key: key, Value: ""})
	}
	return out
}

// imageStampFromExtraConfig reads the image stamp from a VM's ExtraConfig.
//
// It returns (nil, nil) when the VM carries no stamp: no image-stamp key, or
// only keys with empty values (a cleared stamp). It returns (nil, err) for a
// stamp that cannot be trusted, which ADR-0009 D3 treats exactly like no
// stamp; err says why, for the provider log only. It fails closed, like
// ownerFromExtraConfig, on:
//
//   - a key that appears more than once (keys compare case-insensitively, as
//     VMX keys do) or has a non-string value — VirtRigaud never writes either;
//   - an unknown stamp version, an implausible image UID or a malformed source
//     digest (imageartifact.Stamp.Validate);
//   - a preparedat that is not RFC 3339: an unfinished artifact is aged by it,
//     so a stamp without a usable age is not VirtRigaud's.
//
// The informational preparedby is parsed leniently: a malformed value is
// ignored.
func imageStampFromExtraConfig(extraConfig []types.BaseOptionValue) (*imageartifact.Stamp, error) {
	values := make(map[string]string, len(imageStampKeys))
	present := false
	for _, bov := range extraConfig {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil {
			continue
		}
		key := strings.ToLower(ov.Key)
		if !slices.Contains(imageStampKeys, key) {
			continue
		}
		if _, seen := values[key]; seen {
			return nil, fmt.Errorf("image stamp repeats ExtraConfig key %q", key)
		}
		value, ok := ov.Value.(string)
		if !ok {
			return nil, fmt.Errorf("image stamp ExtraConfig key %q has a non-string value (%T)", key, ov.Value)
		}
		values[key] = value
		if value != "" {
			present = true
		}
	}
	if !present {
		return nil, nil
	}

	version, err := strconv.Atoi(values[imageStampKeyVersion])
	if err != nil {
		return nil, fmt.Errorf("image stamp version %q is not a number", values[imageStampKeyVersion])
	}
	s := imageartifact.Stamp{
		StampVersion: version,
		Image: imageartifact.StampIdentity{
			UID:       values[imageStampKeyUID],
			Namespace: values[imageStampKeyNamespace],
			Name:      values[imageStampKeyName],
		},
		SourceDigest: values[imageStampKeySourceDigest],
		PreparedAt:   values[imageStampKeyPreparedAt],
	}
	if parts := strings.Split(values[imageStampKeyPreparedBy], preparedBySeparator); len(parts) == 3 {
		s.PreparedBy = imageartifact.StampIdentity{Namespace: parts[0], Name: parts[1], UID: parts[2]}
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, s.PreparedAt); err != nil {
		return nil, fmt.Errorf("image stamp preparedat %q is not RFC 3339", s.PreparedAt)
	}
	return &s, nil
}

// artifactStalenessBound returns how long an unfinished artifact whose stamp
// matches the request may exist before it counts as abandoned (ADR-0009 D4,
// Q4): max(2 × spec.prepare.timeout, 2h). The timeout is read from the image
// JSON the request carries (the VMImage spec); an absent or unparseable
// timeout leaves the 2h floor.
func artifactStalenessBound(imageJSON string) time.Duration {
	var spec struct {
		Prepare *struct {
			Timeout string `json:"timeout"`
		} `json:"prepare"`
	}
	bound := minArtifactStalenessBound
	if err := json.Unmarshal([]byte(imageJSON), &spec); err != nil || spec.Prepare == nil {
		return bound
	}
	timeout, err := time.ParseDuration(spec.Prepare.Timeout)
	if err != nil || timeout <= 0 {
		return bound
	}
	// A huge timeout is capped, never ignored: ignoring it would shrink the
	// bound to the floor and let a slow import be removed as abandoned.
	timeout = min(timeout, maxPrepareTimeout)
	if twice := 2 * timeout; twice > bound {
		bound = twice
	}
	return bound
}

// moidLess orders managed object IDs the way the lowest-MOID convergence
// (ADR-0009 D6) needs every concurrent prepare to agree on: by length, then
// lexicographically. For vCenter's "vm-<n>" IDs (no leading zeros) that is
// numeric order, so "vm-99" sorts before "vm-100"; vCenter assigns them in
// creation order (verified on vCenter 8.0.2), so the lowest MOID is the first
// object created.
func moidLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// artifactObject is what the probe found about one object named like the
// artifact among the import folder's VirtualMachine children.
type artifactObject struct {
	// ref is the object's managed object reference.
	ref types.ManagedObjectReference
	// template is config.template.
	template bool
	// poweredOff is runtime.powerState == poweredOff. An import in progress
	// is powered off; a prepare never powers anything on.
	poweredOff bool
	// stamp is the object's trusted image stamp; nil when it has none or an
	// untrusted one (stampErr says why).
	stamp *imageartifact.Stamp
	// stampErr is why the stamp is untrusted (provider log only).
	stampErr error
	// activeTask is true when a task in the object's recentTask is queued or
	// running, or cannot be read (fail closed). Only read for an incomplete
	// object with a matching stamp. This is the signal that an import is in
	// flight: on vCenter 8.0.2 (verified) the entity of an active import lease
	// has ResourcePool.ImportVAppLRO running in its recentTask.
	activeTask bool
	// destroyDisabled is true when vCenter lists Destroy_Task in the object's
	// disabledMethod. A defensive extra signal only: an active import lease
	// does NOT disable Destroy_Task on vCenter 8.0.2 (verified; it disables
	// PowerOffVM_Task, MarkAsVirtualMachine, ResetVM_Task and others).
	destroyDisabled bool
}

// needsLiveness reports whether liveness matters for o when prepared for
// req: an incomplete object whose stamp matches (anything else is decided
// without it).
func (o artifactObject) needsLiveness(req imageartifact.Request) bool {
	return !o.template && o.stamp != nil && o.stamp.Matches(req)
}

// observation is o as an imageartifact.Observation (ADR-0009 D4). An
// incomplete object is live while any of these holds — only when none does
// may it be removed as abandoned; age alone never licenses a destroy:
//
//   - a task in its recentTask is queued or running, or cannot be read — an
//     import in flight shows here (ResourcePool.ImportVAppLRO, running, on
//     vCenter 8.0.2);
//   - vCenter disables Destroy_Task on it (a defensive extra: an import lease
//     does not do that on vCenter 8.0.2);
//   - its stamp's preparedAt is younger than bound (or in the future).
func (o artifactObject) observation(now time.Time, bound time.Duration) imageartifact.Observation {
	obs := imageartifact.Observation{Exists: true, Complete: o.template, Stamp: o.stamp}
	if o.template {
		return obs
	}
	obs.Live = o.activeTask || o.destroyDisabled
	if o.stamp != nil {
		preparedAt, err := time.Parse(time.RFC3339, o.stamp.PreparedAt)
		if err != nil || now.Sub(preparedAt) < bound {
			obs.Live = true
		}
	}
	return obs
}

// decideObject is imageartifact.Decide for one vSphere object, plus the
// vSphere refinement of "abandoned": only a powered-off non-template may be
// removed. A matching unfinished object that is powered on was not left by a
// crashed prepare (which never powers on), so it is a Conflict.
func decideObject(o artifactObject, req imageartifact.Request, now time.Time, bound time.Duration) imageartifact.Outcome {
	outcome := imageartifact.Decide(o.observation(now, bound), req)
	if outcome == imageartifact.OutcomeAbandoned && !o.poweredOff {
		return imageartifact.OutcomeConflict
	}
	return outcome
}

// artifactDecision is what an identity prepare does about the objects at the
// artifact name.
type artifactDecision struct {
	// outcome is the ADR-0009 D4 outcome.
	outcome imageartifact.Outcome
	// target is the object outcome is about (nil for OutcomeImport).
	target *artifactObject
	// cleanup are abandoned duplicates (matching stamp, not live, not the
	// lowest MOID) to remove before probing again. When non-empty, outcome is
	// meaningless: remove them and re-probe.
	cleanup []artifactObject
}

// decideArtifact applies ADR-0009 D4 to every object named like the artifact
// in the import folder. vCenter keeps names unique within a folder
// (DuplicateName), so more than one object is either a concurrent prepare
// that has not converged yet (ADR-0009 D6: the object with the lowest MOID
// survives, every other prepare destroys its own) or an inventory VirtRigaud
// did not make:
//
//   - none: import;
//   - one: its D4 outcome;
//   - several: the lowest-MOID object is the survivor. Any other object that
//     is not a matching, unfinished copy is a Conflict (a foreign object, or
//     a second complete template, means the name cannot address one verified
//     artifact); a live copy means the prepares are still converging (in
//     progress); an abandoned copy is cleaned up; then the survivor decides.
func decideArtifact(objs []artifactObject, req imageartifact.Request, now time.Time, bound time.Duration) artifactDecision {
	if len(objs) == 0 {
		return artifactDecision{outcome: imageartifact.OutcomeImport}
	}
	sorted := slices.Clone(objs)
	slices.SortFunc(sorted, func(a, b artifactObject) int {
		switch {
		case moidLess(a.ref.Value, b.ref.Value):
			return -1
		case moidLess(b.ref.Value, a.ref.Value):
			return 1
		}
		return 0
	})
	survivor := sorted[0]
	survivorOutcome := decideObject(survivor, req, now, bound)
	if len(sorted) == 1 || survivorOutcome == imageartifact.OutcomeConflict {
		return artifactDecision{outcome: survivorOutcome, target: &survivor}
	}

	var (
		cleanup    []artifactObject
		inProgress *artifactObject
	)
	for i := range sorted[1:] {
		extra := sorted[i+1]
		switch decideObject(extra, req, now, bound) {
		case imageartifact.OutcomeInProgress:
			if inProgress == nil {
				inProgress = &extra
			}
		case imageartifact.OutcomeAbandoned:
			cleanup = append(cleanup, extra)
		default:
			// A foreign object, or a second complete template with the same
			// stamp: the name no longer addresses one verified artifact.
			return artifactDecision{outcome: imageartifact.OutcomeConflict, target: &extra}
		}
	}
	if len(cleanup) > 0 {
		return artifactDecision{cleanup: cleanup}
	}
	return artifactDecision{outcome: imageartifact.OutcomeInProgress, target: inProgress}
}
