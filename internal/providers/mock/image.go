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

package mock

import (
	"context"
	"path"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Image preparation (ADR-0009).
//
// The mock keeps one in-memory image location, shared by every request it
// serves (like a vSphere import folder or a libvirt pool shared by the
// Providers that resolve to it). It implements the ADR-0009 contract in full:
//
//   - An identity request (image + source_digest, empty target_name) gets the
//     artifact name imageartifact.ArtifactName derives (libvirt rule). An
//     artifact is stamped when its import starts. An existing artifact at that
//     name is reused only when it is complete and its stamp carries the
//     request's image UID and source digest; one still being prepared for the
//     same identity is a retryable Unavailable; one whose import failed is
//     removed and imported again; anything else (no stamp, another UID or
//     digest) is AlreadyExists and is never touched (imageartifact.Decide).
//     Every answer echoes the stamp in ImagePrepareResponse.artifact.
//   - A legacy request (no image, a bare target_name, from a manager older than
//     ADR-0009) is served the pre-ADR way — named and reused by the bare name,
//     with no artifact echo — and emits the deprecation signal. The mock names
//     artifacts with the '_' libvirt rule and bare names never contain '_', so
//     a legacy request can never reach a new-scheme artifact. (That argument
//     does not hold for a '-' rule: see imageartifact.NameRuleProxmox.)
//
// An import takes imagePrepareDelay: with a positive delay the response carries
// a task, and the artifact is complete once the task is done; with 0 it is
// complete within the call.

const (
	// mockProviderType is the provider_type label of this provider's metrics.
	mockProviderType = "mock"
	// mockImageDir is the synthetic directory of prepared_image_path.
	mockImageDir = "/var/lib/virtrigaud/mock"
	// mockImageExt is the synthetic file extension of prepared_image_path.
	mockImageExt = ".qcow2"
	// mockImageNameRule is the artifact naming rule the mock applies: the
	// libvirt rule, since the mock reports a pool-style path.
	mockImageNameRule = imageartifact.NameRuleLibvirt
	// defaultImagePrepareDelay is how long an import takes by default.
	defaultImagePrepareDelay = 15 * time.Second
)

// preparedImage is an artifact in the mock's image location.
type preparedImage struct {
	// stamp is the artifact's provenance stamp; nil for an unstamped artifact
	// (a legacy bare-name artifact, or one placed out of band).
	stamp *imageartifact.Stamp
	// taskID is the task importing the artifact; empty when it was complete
	// on arrival.
	taskID string
}

// ImagePrepare prepares an image (ADR-0009 D7; see the comment above). It
// returns the prepared location — prepared_image_id is the artifact name and
// prepared_image_path a synthetic pool path — which is known at trigger time
// even when the import is asynchronous (issue #154, PR-6 / #214).
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	p.simulateDelay()

	if p.shouldFail("image_prepare") {
		return nil, errors.NewInternal("mock provider configured to fail image operations", nil)
	}

	parsed, err := imageartifact.ParseRequest(req)
	if err != nil {
		return nil, err
	}
	if parsed.Mode == imageartifact.ModeLegacy {
		return p.imagePrepareLegacy(ctx, parsed.LegacyTargetName), nil
	}
	return p.imagePrepareIdentity(ctx, parsed)
}

// imagePrepareIdentity serves an identity request (ADR-0009 D1-D4).
func (p *Provider) imagePrepareIdentity(ctx context.Context, req imageartifact.Request) (*providerv1.ImagePrepareResponse, error) {
	name, err := imageartifact.ArtifactName(mockImageNameRule, req.Image, req.SourceDigest)
	if err != nil {
		return nil, errors.NewInvalidSpec("cannot name the prepared-image artifact: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if img, exists := p.images[name]; exists {
		switch outcome := imageartifact.Decide(p.observeImageLocked(img), req); outcome {
		case imageartifact.OutcomeReuse:
			return preparedImageResponse(name, img.stamp.PreparedArtifact(name, true), nil), nil
		case imageartifact.OutcomeInProgress:
			return nil, imageartifact.InProgressError(name)
		case imageartifact.OutcomeAbandoned:
			// A failed import of this same image: remove only that artifact,
			// then import again below.
			p.logger.InfoContext(ctx, "Removing an abandoned prepared-image artifact of this VMImage before importing it again",
				"artifact", name)
			delete(p.images, name)
		default:
			// The recorded owner goes to the provider log only (ADR-0009 D11).
			attrs := []any{"artifact", name, "requestImageUID", req.Image.UID}
			if img.stamp != nil {
				attrs = append(attrs, "stampImageUID", img.stamp.Image.UID,
					"stampImage", img.stamp.Image.Namespace+"/"+img.stamp.Image.Name,
					"stampSourceDigest", img.stamp.SourceDigest)
			}
			p.logger.WarnContext(ctx, "Refusing a prepared-image artifact that was not prepared for the requesting VMImage", attrs...)
			return nil, imageartifact.ConflictError(name)
		}
	}

	stamp := imageartifact.NewStamp(req, time.Now())
	task := p.startImageImportLocked(name, &stamp)
	return preparedImageResponse(name, stamp.PreparedArtifact(name, false), task), nil
}

// imagePrepareLegacy serves a legacy (identity-less) request the pre-ADR way,
// for this release only (ADR-0009 D7, Q3): the artifact is the bare target
// name, an existing artifact of that name is reused whatever it holds, and no
// artifact is echoed. It emits the deprecation signal first.
func (p *Provider) imagePrepareLegacy(ctx context.Context, name string) *providerv1.ImagePrepareResponse {
	imageartifact.SignalLegacyRequest(ctx, p.logger, mockProviderType, name)

	p.mu.Lock()
	defer p.mu.Unlock()

	if img, exists := p.images[name]; exists {
		obs := p.observeImageLocked(img)
		switch {
		case obs.Complete:
			return preparedImageResponse(name, nil, nil)
		case obs.Live:
			// Reuse by name while it is still importing: hand back its task.
			return preparedImageResponse(name, nil, &providerv1.TaskRef{Id: img.taskID})
		}
		delete(p.images, name) // a failed legacy import: import again
	}
	return preparedImageResponse(name, nil, p.startImageImportLocked(name, nil))
}

// startImageImportLocked records a new artifact named name carrying stamp and
// starts its import. It returns the import's task, or nil when the import
// completes within the call (imagePrepareDelay == 0). p.mu must be held.
func (p *Provider) startImageImportLocked(name string, stamp *imageartifact.Stamp) *providerv1.TaskRef {
	img := &preparedImage{stamp: stamp}
	var ref *providerv1.TaskRef
	if p.imagePrepareDelay > 0 {
		taskID := p.generateID("task")
		p.tasks[taskID] = &Task{ID: taskID, Created: time.Now()}
		img.taskID = taskID
		ref = &providerv1.TaskRef{Id: taskID}
		go p.completeTaskAfterDelay(taskID, p.imagePrepareDelay)
	}
	p.images[name] = img
	return ref
}

// observeImageLocked reports what an identity prepare finds in img (ADR-0009
// D4): complete when it has no import task or the task succeeded, live while
// the task runs, neither when the task failed or is unknown. p.mu must be held.
func (p *Provider) observeImageLocked(img *preparedImage) imageartifact.Observation {
	obs := imageartifact.Observation{Exists: true, Stamp: img.stamp}
	if img.taskID == "" {
		obs.Complete = true
		return obs
	}
	task, ok := p.tasks[img.taskID]
	switch {
	case !ok:
	case !task.Done:
		obs.Live = true
	case task.Error == "":
		obs.Complete = true
	}
	return obs
}

// preparedImageResponse builds the ImagePrepare response for the artifact
// named name: its synthetic location, the stamp echo (nil in legacy mode) and
// the import task (nil when complete).
func preparedImageResponse(name string, artifact *providerv1.PreparedArtifact, task *providerv1.TaskRef) *providerv1.ImagePrepareResponse {
	return &providerv1.ImagePrepareResponse{
		Task:              task,
		PreparedImageId:   name,
		PreparedImagePath: path.Join(mockImageDir, name+mockImageExt),
		Artifact:          artifact,
	}
}

// PlantImageArtifact places a complete artifact named name in the mock's image
// location as if it had been put there out of band — by a legacy (pre-ADR-0009)
// prepare, by hand, or by another party — carrying a copy of stamp (nil: no
// stamp). It replaces whatever was at name. It exists for tests and demos of
// the ADR-0009 rules: an identity prepare reuses a planted artifact at its
// derived name only when the stamp matches, and refuses it otherwise.
func (p *Provider) PlantImageArtifact(name string, stamp *imageartifact.Stamp) {
	img := &preparedImage{}
	if stamp != nil {
		s := *stamp
		img.stamp = &s
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.images[name] = img
}
