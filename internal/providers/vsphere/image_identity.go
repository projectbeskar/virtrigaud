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
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/progress"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Identity-mode image preparation (ADR-0009 Slice 3).
//
// An identity request names the VMImage (UID, namespace, name) and the digest
// of its spec.source. The artifact is a vSphere template:
//
//   - named imageartifact.ArtifactName(NameRuleVSphere, image, digest)
//     ("<ns>.<name>"(cut)"_<h16>", at most 80 bytes; D1);
//   - looked up, created and reused ONLY in the Provider's import folder
//     (resolveArtifactFolder; D5): vCenter keeps VM names unique within a
//     folder, which is the uniqueness scope and the atomic create-if-absent
//     (DuplicateName; D6) the design needs;
//   - stamped with virtrigaud.image.* in the import spec, after the OVF's
//     reserved keys are stripped and before ImportVApp (D3);
//   - reused only when it is a template whose stamp carries the request's
//     image UID and source digest; an unfinished copy of the same image is in
//     progress while live and removed once abandoned; anything else at the
//     name is a Conflict, never touched (D4, decideArtifact);
//   - addressed by its absolute inventory path in prepared_image_id, which
//     Create resolves exactly (D5).
//
// Error classification for the synchronous import:
//
//   - a source that can never import (InvalidSpec, codes.InvalidArgument — the
//     manager holds the image): an unusable source kind or URL, a 4xx from
//     the source other than 408/429, a checksum mismatch, an unreadable
//     archive or OVF, an OVF vCenter's parser rejects, or an OVF that is not
//     exactly one VM;
//   - an object at the name that is not this image's: codes.AlreadyExists
//     (Conflict);
//   - this image's artifact still being prepared, or the name still changing
//     under concurrent prepares: imageartifact.InProgressError
//     (codes.Unavailable with the IMAGE_ARTIFACT_IN_PROGRESS reason, kept out
//     of the manager's circuit breaker);
//   - a configured import folder that is missing, ambiguous or outside the
//     datacenter's VM folder: codes.FailedPrecondition (retried by the
//     manager, not counted toward its circuit breaker);
//   - a failure of this image's source or content — the source server failing,
//     refusing or breaking off the download, a download that cannot be staged,
//     vCenter refusing to create, upload or convert what the OVF describes —
//     is retryable but tagged IMAGE_SOURCE_UNAVAILABLE
//     (imageartifact.SourceUnavailableError): the manager keeps it out of its
//     circuit breaker, so one tenant's bad image cannot stop the Provider;
//   - vCenter itself unreachable, an expired session, missing rights
//     (isVCenterUnreachable) is a generic retryable codes.Unavailable that
//     counts toward the breaker.

const (
	// maxArtifactProbeRounds bounds how often one identity prepare probes the
	// artifact name again (after a DuplicateName, a lost convergence or the
	// removal of an abandoned object) before it gives up with a retryable
	// error.
	maxArtifactProbeRounds = 4

	// artifactCleanupTimeout bounds a destroy of this call's own partial
	// import, which runs on a context detached from the request's so it still
	// happens when the request was cancelled.
	artifactCleanupTimeout = 2 * time.Minute

	// propRuntimePowerState, propRecentTask, propDisabledMethod and
	// propInfoState are the property paths the artifact probe reads.
	propRuntimePowerState = "runtime.powerState"
	propRecentTask        = "recentTask"
	propDisabledMethod    = "disabledMethod"
	propInfoState         = "info.state"

	// ovaURLSchemeHTTP and ovaURLSchemeHTTPS are the only OVA URL schemes an
	// identity prepare downloads.
	ovaURLSchemeHTTP  = "http"
	ovaURLSchemeHTTPS = "https"
)

// artifactLocation is where an identity prepare's artifact lives: its name in
// the Provider's import folder.
type artifactLocation struct {
	// folder is the resolved import folder (resolveArtifactFolder).
	folder *object.Folder
	// name is the artifact name (imageartifact.ArtifactName).
	name string
}

// inventoryPath is the artifact's absolute inventory path, the
// prepared_image_id of an identity prepare.
func (l artifactLocation) inventoryPath() string {
	return path.Join(l.folder.InventoryPath, l.name)
}

// stagedOVA is the OVA an identity prepare downloaded and verified. It is
// downloaded at most once per call, however often the name is re-probed.
type stagedOVA struct {
	cleanup    func()
	archive    importer.Archive
	descriptor string
}

// close removes the staged download; it is safe on a nil receiver.
func (s *stagedOVA) close() {
	if s != nil && s.cleanup != nil {
		s.cleanup()
	}
}

// identitySourceError returns an InvalidSpec error unless src is what an
// identity prepare imports: exactly one input, an http(s) source.vsphere.ovaURL
// (ADR-0009 D1). templateName and contentLibrary reference existing objects;
// the manager never sends them to a prepare, and they are never stamped.
func identitySourceError(src vsphereImageSource) error {
	if src.OVAURL == "" {
		return errors.NewInvalidSpec("ImagePrepare: an identity prepare imports only a source.vsphere.ovaURL; " +
			"templateName and contentLibrary reference existing objects and are never prepared")
	}
	if src.TemplateName != "" || src.ContentLibrary != nil {
		return errors.NewInvalidSpec("ImagePrepare: the vSphere image source sets ovaURL together with templateName " +
			"or contentLibrary; a prepared image is imported from exactly one source: remove the reference or the ovaURL")
	}
	u, err := url.Parse(src.OVAURL)
	if err != nil || (u.Scheme != ovaURLSchemeHTTP && u.Scheme != ovaURLSchemeHTTPS) || u.Host == "" {
		return errors.NewInvalidSpec("ImagePrepare: ovaURL %q is not an http(s) URL", redactURL(src.OVAURL))
	}
	if urlPathExt(src.OVAURL) == ovfDescriptorExt {
		// A bare .ovf has no container for the disks it references, and its
		// references were resolved on the provider's filesystem.
		return errors.NewInvalidSpec("ImagePrepare: ovaURL names a bare .ovf, which cannot carry its disks; " +
			"publish the image as an .ova")
	}
	return nil
}

// imagePrepareIdentity serves an identity request (ADR-0009 D1-D6; see the
// comment at the top of this file). It probes the artifact name in the import
// folder and acts on the decision, re-probing after anything that changes what
// is at the name, at most maxArtifactProbeRounds times.
func (p *Provider) imagePrepareIdentity(ctx context.Context, req *providerv1.ImagePrepareRequest, id imageartifact.Request) (*providerv1.ImagePrepareResponse, error) {
	src := parseVSphereImageSource(req.GetImageJson())
	if err := identitySourceError(src); err != nil {
		return nil, err
	}
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleVSphere, id.Image, id.SourceDigest)
	if err != nil {
		return nil, errors.NewInvalidSpec("ImagePrepare: cannot name the prepared-image artifact: %v", err)
	}
	if reason := unsafeNameReason(name); reason != "" {
		// Unreachable for a D1 name (tested); refuse rather than look it up.
		return nil, errors.NewInvalidSpec("ImagePrepare: artifact name %q cannot be used: %s", name, reason)
	}
	bound := artifactStalenessBound(req.GetImageJson())

	// A finder of this call's own: the shared p.finder is re-scoped by every
	// RPC (SetDatacenter), which concurrent prepares must not race on.
	finder := find.NewFinder(p.client.Client, true)
	datacenter, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, p.artifactRetryError(ctx, name, "resolve the default datacenter", err)
	}
	finder.SetDatacenter(datacenter)
	folder, err := p.resolveArtifactFolder(ctx, finder, name)
	if err != nil {
		return nil, err
	}
	loc := artifactLocation{folder: folder, name: name}

	p.logger.InfoContext(ctx, "ImagePrepare: identity prepare",
		"artifact", name, "folder", folder.InventoryPath,
		"image", id.Image.Namespace+"/"+id.Image.Name, "image_uid", id.Image.UID,
		"source_digest", id.SourceDigest, "staleness_bound", bound.String())

	var staged *stagedOVA
	defer func() { staged.close() }()

	for round := 0; round < maxArtifactProbeRounds; round++ {
		objs, err := p.probeArtifact(ctx, loc, id)
		if err != nil {
			return nil, p.artifactRetryError(ctx, name, "look up the artifact in the import folder", err)
		}
		d := decideArtifact(objs, id, time.Now(), bound)
		if len(d.cleanup) > 0 {
			for _, o := range d.cleanup {
				if err := p.removeAbandonedArtifact(ctx, loc, o.ref, id, bound); err != nil {
					return nil, err
				}
			}
			continue
		}

		switch d.outcome {
		case imageartifact.OutcomeReuse:
			p.logger.InfoContext(ctx, "ImagePrepare: reusing the prepared-image artifact of this VMImage",
				"artifact", name, "vm", d.target.ref.Value)
			return p.artifactResponse(ctx, loc, d.target.ref, *d.target.stamp, true)
		case imageartifact.OutcomeInProgress:
			p.logger.InfoContext(ctx, "ImagePrepare: the prepared-image artifact of this VMImage is still being prepared",
				"artifact", name, "vm", d.target.ref.Value)
			return nil, imageartifact.InProgressError(name)
		case imageartifact.OutcomeAbandoned:
			if err := p.removeAbandonedArtifact(ctx, loc, d.target.ref, id, bound); err != nil {
				return nil, err
			}
		case imageartifact.OutcomeImport:
			resp, err := p.importArtifact(ctx, finder, loc, id, src, req.GetStorageHint(), &staged)
			if stderrors.Is(err, errArtifactNameTaken) {
				continue
			}
			return resp, err
		default:
			p.logArtifactConflict(ctx, loc, id, d.target)
			return nil, imageartifact.ConflictError(name)
		}
	}
	p.logger.WarnContext(ctx, "ImagePrepare: the artifact name did not settle; giving up for now",
		"artifact", name, "rounds", maxArtifactProbeRounds)
	// Other prepares are changing what is at the name: the same situation as
	// an in-progress artifact, and like it no sign of an unhealthy provider.
	return nil, imageartifact.InProgressError(name)
}

// resolveArtifactFolder resolves the import folder of an identity prepare
// (ADR-0009 D5). It falls back to the datacenter's VM folder ONLY when the
// Provider's DefaultFolder is empty. A configured folder that does not
// resolve is an error the manager retries, so every retry resolves the same
// location — an artifact that lands in an unintended folder would be shared
// with whoever can read that folder (stricter than resolveVMFolder and the
// legacy resolveImageFolder):
//
//   - a vCenter failure is codes.Unavailable (artifactRetryError);
//   - a folder that is missing, ambiguous or outside the default datacenter's
//     VM folder is codes.FailedPrecondition (importFolderError): a
//     configuration problem, retried by the manager like any provider error
//     but — unlike Unavailable — not counted toward the Provider's circuit
//     breaker, so a wrong defaults.folder cannot stop the Provider's other
//     RPCs.
//
// finder must be scoped to the default datacenter.
func (p *Provider) resolveArtifactFolder(ctx context.Context, finder *find.Finder, artifact string) (*object.Folder, error) {
	vmFolder, err := finder.DefaultFolder(ctx)
	if err != nil {
		return nil, p.artifactRetryError(ctx, artifact, "resolve the datacenter VM folder", err)
	}
	folderName := strings.TrimSpace(p.config.DefaultFolder)
	if folderName == "" {
		return vmFolder, nil
	}

	folder, err := finder.Folder(ctx, folderName)
	if err != nil {
		if reason := importFolderLookupReason(err); reason != "" {
			return nil, p.importFolderError(ctx, folderName, reason, err)
		}
		return nil, p.artifactRetryError(ctx, artifact, "resolve the Provider's default folder", err)
	}
	under := strings.TrimSuffix(vmFolder.InventoryPath, inventoryPathSeparator) + inventoryPathSeparator
	if folder.InventoryPath != vmFolder.InventoryPath && !strings.HasPrefix(folder.InventoryPath, under) {
		return nil, p.importFolderError(ctx, folderName, "it is not under the default datacenter's VM folder",
			fmt.Errorf("resolved to %s, the VM folder is %s", folder.InventoryPath, vmFolder.InventoryPath))
	}
	return folder, nil
}

// importFolderLookupReason returns why the finder error err, from looking up
// the configured import folder, is a configuration problem: the folder does
// not exist, or the name matches more than one folder (both definitive). It
// returns "" for any other error, a vCenter failure.
func importFolderLookupReason(err error) string {
	var notFound *find.NotFoundError
	var multiple *find.MultipleFoundError
	switch {
	case stderrors.As(err, &notFound):
		return "it does not exist"
	case stderrors.As(err, &multiple):
		return "it names more than one folder"
	}
	return ""
}

// importFolderError logs why the configured import folder cannot be used and
// returns the codes.FailedPrecondition error for it (ADR-0009 D5; see
// resolveArtifactFolder).
func (p *Provider) importFolderError(ctx context.Context, folder, reason string, detail error) error {
	p.logger.ErrorContext(ctx, "ImagePrepare: the Provider's default folder cannot be used as the import folder; not falling back",
		"folder", folder, "reason", reason, "error", detail)
	return status.Errorf(codes.FailedPrecondition,
		"ImagePrepare: the Provider's default folder %q cannot be used as the image import folder: %s; "+
			"prepared images are never imported into a fallback folder; fix the Provider's defaults.folder "+
			"or the vCenter inventory (will retry)", folder, reason)
}

// probeArtifact observes every VirtualMachine child of the import folder named
// exactly like the artifact (vmsNamedInFolder: no finder, no MOID or path
// resolution). An object that vanished while it was read is left out. Any
// other vCenter error is returned: a failed probe is never "absent".
func (p *Provider) probeArtifact(ctx context.Context, loc artifactLocation, id imageartifact.Request) ([]artifactObject, error) {
	refs, err := p.vmsNamedInFolder(ctx, loc.folder, loc.name)
	if err != nil {
		return nil, err
	}
	objs := make([]artifactObject, 0, len(refs))
	for _, ref := range refs {
		o, found, err := p.observeArtifactObject(ctx, ref, id)
		if err != nil {
			return nil, err
		}
		if found {
			objs = append(objs, o)
		}
	}
	return objs, nil
}

// observeArtifactObject reads what ADR-0009 D4 decides on for the object ref:
// whether it is a template, its stamp, its power state, and — for an
// unfinished object whose stamp matches id, where liveness matters — whether a
// task on it is queued or running and whether vCenter blocks its Destroy_Task
// (as it does while an HttpNfcLease holds it). found is false when the object
// no longer exists.
func (p *Provider) observeArtifactObject(ctx context.Context, ref types.ManagedObjectReference, id imageartifact.Request) (o artifactObject, found bool, err error) {
	var vm mo.VirtualMachine
	err = property.DefaultCollector(p.client.Client).RetrieveOne(ctx, ref, []string{
		propConfigTemplate, propConfigExtraConfig, propRuntimePowerState, propRecentTask, propDisabledMethod,
	}, &vm)
	if err != nil {
		if fault.Is(err, &types.ManagedObjectNotFound{}) {
			return artifactObject{}, false, nil
		}
		return artifactObject{}, false, fmt.Errorf("read %s: %w", ref.Value, err)
	}

	o = artifactObject{
		ref:             ref,
		poweredOff:      vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOff,
		destroyDisabled: slices.Contains(vm.DisabledMethod, destroyTaskMethod),
	}
	if vm.Config != nil {
		o.template = vm.Config.Template
		o.stamp, o.stampErr = imageStampFromExtraConfig(vm.Config.ExtraConfig)
	} else {
		// An inaccessible VM has no config and therefore no provable stamp.
		o.stampErr = stderrors.New("the object has no readable config")
	}
	if o.needsLiveness(id) {
		if o.activeTask, err = p.anyTaskActive(ctx, vm.RecentTask); err != nil {
			return artifactObject{}, false, err
		}
	}
	return o, true, nil
}

// anyTaskActive reports whether any of tasks is queued or running. A task
// that no longer exists is not active.
func (p *Provider) anyTaskActive(ctx context.Context, tasks []types.ManagedObjectReference) (bool, error) {
	pc := property.DefaultCollector(p.client.Client)
	for _, ref := range tasks {
		var t mo.Task
		if err := pc.RetrieveOne(ctx, ref, []string{propInfoState}, &t); err != nil {
			if fault.Is(err, &types.ManagedObjectNotFound{}) {
				continue
			}
			return false, fmt.Errorf("read state of task %s: %w", ref.Value, err)
		}
		if t.Info.State == types.TaskInfoStateQueued || t.Info.State == types.TaskInfoStateRunning {
			return true, nil
		}
	}
	return false, nil
}

// stageOVA downloads the OVA of src, verifies its checksum when the source
// pins one, and opens its archive. Its errors are already classified
// (downloadOVA, verifyFileChecksum, newOVAArchive).
func (p *Provider) stageOVA(ctx context.Context, src vsphereImageSource) (*stagedOVA, error) {
	localPath, cleanup, err := p.downloadOVA(ctx, src.OVAURL)
	if err != nil {
		return nil, err
	}
	if src.Checksum != "" {
		if err := verifyFileChecksum(localPath, src.Checksum, src.ChecksumType); err != nil {
			cleanup()
			return nil, p.stagedFileError(ctx, err)
		}
	}
	archive, descriptor, err := p.newOVAArchive(localPath, src.OVAURL)
	if err != nil {
		cleanup()
		return nil, p.stagedFileError(ctx, err)
	}
	return &stagedOVA{cleanup: cleanup, archive: archive, descriptor: descriptor}, nil
}

// importArtifact imports the stamped artifact into the free name (ADR-0009
// D6). It returns an error wrapping errArtifactNameTaken when the caller must
// probe the name again: vCenter refused the import with DuplicateName, or
// another prepare's object with a lower MOID won the convergence and this
// call destroyed its own. On any other failure after the entity was created,
// it destroys that entity (only the one this call created) so a retry starts
// clean. finder must be scoped to the default datacenter.
func (p *Provider) importArtifact(ctx context.Context, finder *find.Finder, loc artifactLocation, id imageartifact.Request, src vsphereImageSource, storageHint string, staged **stagedOVA) (*providerv1.ImagePrepareResponse, error) {
	pool, datastore, err := p.resolveImageComputeAndStorage(ctx, finder, storageHint)
	if err != nil {
		if isProviderError(err, codes.InvalidArgument) {
			return nil, err // no datastore configured at all
		}
		return nil, p.artifactRetryError(ctx, loc.name, "resolve the import resource pool and datastore", err)
	}
	if *staged == nil {
		s, err := p.stageOVA(ctx, src)
		if err != nil {
			return nil, err
		}
		*staged = s
	}

	stamp := imageartifact.NewStamp(id, time.Now())
	imp := &importer.Importer{
		Log:          p.ovaImportLog,
		Sinker:       progress.NewProgressLogger(p.ovaImportLog, "ImagePrepare upload"),
		Client:       p.client.Client,
		Finder:       finder,
		Datastore:    datastore,
		ResourcePool: pool,
		Folder:       loc.folder,
		Archive:      (*staged).archive,
	}
	name := loc.name
	opts := importer.Options{DiskProvisioning: ovaDiskProvisioningThin, Name: &name}

	p.logger.InfoContext(ctx, "ImagePrepare: importing the prepared-image artifact",
		"artifact", loc.name, "folder", loc.folder.InventoryPath, "ova_url", redactURL(src.OVAURL),
		"datastore", datastore.Name(), "resource_pool", pool.Reference().Value)

	ref, err := p.importOVA(ctx, imp, (*staged).descriptor, opts, &stamp)
	if err != nil {
		if ref != nil {
			p.destroyOwnImport(ctx, *ref, loc.name)
		}
		switch {
		case stderrors.Is(err, errArtifactNameTaken):
			p.logger.InfoContext(ctx, "ImagePrepare: vCenter reports the artifact name as taken (DuplicateName); probing again",
				"artifact", loc.name)
			return nil, err
		case isProviderError(err, codes.InvalidArgument):
			return nil, err
		}
		return nil, p.importFailure(ctx, loc.name, "import the OVA", err)
	}
	own := *ref
	vm := object.NewVirtualMachine(p.client.Client, own)

	// Convergence (ADR-0009 D6): if another prepare created an object with the
	// same name concurrently (a vCenter that did not enforce DuplicateName),
	// the object with the lowest MOID survives and every other prepare destroys
	// its own, then reuses the survivor after the D4 probe.
	lost, err := p.lostConvergence(ctx, loc, own)
	if err != nil {
		p.destroyOwnImport(ctx, own, loc.name)
		return nil, p.artifactRetryError(ctx, loc.name, "re-list the import folder after the import", err)
	}
	if lost {
		p.logger.InfoContext(ctx, "ImagePrepare: another object with the artifact name has a lower MOID; destroying this call's own import",
			"artifact", loc.name, "vm", own.Value)
		if err := p.destroyVM(ctx, own); err != nil {
			return nil, p.artifactRetryError(ctx, loc.name, "destroy this call's duplicate import", err)
		}
		return nil, fmt.Errorf("converged on another object: %w", errArtifactNameTaken)
	}

	// Promote to a template only now: an unfinished artifact is never complete.
	if err := vm.MarkAsTemplate(ctx); err != nil {
		p.destroyOwnImport(ctx, own, loc.name)
		return nil, p.importFailure(ctx, loc.name, "mark the imported artifact as a template", err)
	}

	// The template is complete. It is handed out only while the name addresses
	// it alone: prepared_image_id is its inventory path.
	refs, err := p.vmsNamedInFolder(ctx, loc.folder, loc.name)
	if err != nil {
		return nil, p.artifactRetryError(ctx, loc.name, "re-list the import folder after marking the template", err)
	}
	if len(refs) != 1 || refs[0] != own {
		p.logger.InfoContext(ctx, "ImagePrepare: imported the artifact, but other objects with its name remain in the import folder; the next call decides",
			"artifact", loc.name, "vm", own.Value, "objects", len(refs))
		return nil, imageartifact.InProgressError(loc.name)
	}
	p.logger.InfoContext(ctx, "ImagePrepare: imported and stamped the prepared-image artifact",
		"artifact", loc.name, "vm", own.Value)
	return p.artifactResponse(ctx, loc, own, stamp, false)
}

// lostConvergence re-lists the objects named like the artifact in the import
// folder and reports whether one of them has a lower MOID than own (moidLess),
// in which case this call must destroy own (ADR-0009 D6). own missing from the
// listing is an error: it was created by this call and must be there.
func (p *Provider) lostConvergence(ctx context.Context, loc artifactLocation, own types.ManagedObjectReference) (bool, error) {
	refs, err := p.vmsNamedInFolder(ctx, loc.folder, loc.name)
	if err != nil {
		return false, err
	}
	if !slices.Contains(refs, own) {
		return false, fmt.Errorf("the imported object %s is not listed in the import folder", own.Value)
	}
	for _, r := range refs {
		if moidLess(r.Value, own.Value) {
			return true, nil
		}
	}
	return false, nil
}

// removeAbandonedArtifact removes ref, an unfinished object at the artifact
// name that ADR-0009 D4 found abandoned by a crashed prepare of this same
// image. It re-reads the object first and destroys it only if that is still
// true (a powered-off non-template with a matching stamp, no queued or
// running task, Destroy_Task not blocked, older than bound); otherwise it does
// nothing and the caller probes again.
func (p *Provider) removeAbandonedArtifact(ctx context.Context, loc artifactLocation, ref types.ManagedObjectReference, id imageartifact.Request, bound time.Duration) error {
	o, found, err := p.observeArtifactObject(ctx, ref, id)
	if err != nil {
		return p.artifactRetryError(ctx, loc.name, "re-read an abandoned artifact object", err)
	}
	if !found || decideObject(o, id, time.Now(), bound) != imageartifact.OutcomeAbandoned {
		return nil
	}
	p.logger.InfoContext(ctx, "ImagePrepare: removing an abandoned, unfinished artifact object of this VMImage "+
		"(stamp matches; not a template; powered off; no running task; not held; older than the staleness bound)",
		"artifact", loc.name, "vm", ref.Value, "prepared_at", o.stamp.PreparedAt, "staleness_bound", bound.String())
	if err := p.destroyVM(ctx, ref); err != nil {
		return p.artifactRetryError(ctx, loc.name, "remove an abandoned artifact object", err)
	}
	return nil
}

// destroyVM destroys the VM ref and waits for the task.
func (p *Provider) destroyVM(ctx context.Context, ref types.ManagedObjectReference) error {
	t, err := object.NewVirtualMachine(p.client.Client, ref).Destroy(ctx)
	if err != nil {
		return fmt.Errorf("start destroy of %s: %w", ref.Value, err)
	}
	if err := t.Wait(ctx); err != nil {
		return fmt.Errorf("destroy %s: %w", ref.Value, err)
	}
	return nil
}

// destroyOwnImport best-effort destroys ref, an object THIS call created and
// could not finish, on a context detached from the request's (a cancelled
// request still cleans up), bounded by artifactCleanupTimeout. A failure is
// logged: the object then carries this image's stamp and ages into an
// abandoned object a later prepare removes (ADR-0009 D4).
func (p *Provider) destroyOwnImport(ctx context.Context, ref types.ManagedObjectReference, artifact string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artifactCleanupTimeout)
	defer cancel()
	if err := p.destroyVM(cctx, ref); err != nil {
		p.logger.WarnContext(ctx, "ImagePrepare: could not destroy this call's unfinished import; a later prepare removes it once abandoned",
			"artifact", artifact, "vm", ref.Value, "error", err)
		return
	}
	p.logger.InfoContext(ctx, "ImagePrepare: destroyed this call's unfinished import", "artifact", artifact, "vm", ref.Value)
}

// artifactResponse builds the response for the artifact ref carrying stamp:
// prepared_image_id is its absolute inventory path, which must resolve
// (SearchIndex.FindByInventoryPath, as Create's lookupTemplate does) to ref
// itself; the artifact echo is the stamp (ADR-0009 D5, D7).
func (p *Provider) artifactResponse(ctx context.Context, loc artifactLocation, ref types.ManagedObjectReference, stamp imageartifact.Stamp, reused bool) (*providerv1.ImagePrepareResponse, error) {
	invPath := loc.inventoryPath()
	obj, err := object.NewSearchIndex(p.client.Client).FindByInventoryPath(ctx, invPath)
	if err != nil {
		return nil, p.artifactRetryError(ctx, loc.name, "resolve the artifact's inventory path", err)
	}
	switch {
	case obj == nil:
		p.logger.ErrorContext(ctx, "ImagePrepare: the artifact's inventory path does not resolve",
			"artifact", loc.name, "inventory_path", invPath, "vm", ref.Value)
		return nil, status.Errorf(codes.Unavailable,
			"ImagePrepare: the prepared-image artifact %q does not resolve by its inventory path; will retry", loc.name)
	case obj.Reference() != ref:
		p.logger.WarnContext(ctx, "ImagePrepare: the artifact's inventory path resolves to another object",
			"artifact", loc.name, "inventory_path", invPath, "vm", ref.Value, "resolved", obj.Reference().Value)
		return nil, imageartifact.InProgressError(loc.name)
	}
	return &providerv1.ImagePrepareResponse{
		PreparedImageId: invPath,
		Artifact:        stamp.PreparedArtifact(loc.name, reused),
	}, nil
}

// logArtifactConflict logs, provider-side only, why the object at the
// artifact name is refused (ADR-0009 D4, D11): the requester gets the uniform
// imageartifact.ConflictError, which names no other image or owner.
func (p *Provider) logArtifactConflict(ctx context.Context, loc artifactLocation, id imageartifact.Request, target *artifactObject) {
	attrs := []any{
		"artifact", loc.name, "folder", loc.folder.InventoryPath,
		"request_image_uid", id.Image.UID, "request_source_digest", id.SourceDigest,
	}
	if target != nil {
		attrs = append(attrs, "vm", target.ref.Value, "template", target.template, "powered_off", target.poweredOff)
		switch {
		case target.stamp != nil:
			attrs = append(attrs,
				"stamp_image_uid", target.stamp.Image.UID,
				"stamp_image", target.stamp.Image.Namespace+"/"+target.stamp.Image.Name,
				"stamp_source_digest", target.stamp.SourceDigest,
				"stamp_prepared_by", target.stamp.PreparedBy.Namespace+"/"+target.stamp.PreparedBy.Name,
				"stamp_prepared_at", target.stamp.PreparedAt)
		case target.stampErr != nil:
			attrs = append(attrs, "stamp_error", target.stampErr.Error())
		default:
			attrs = append(attrs, "stamp", "none")
		}
	}
	p.logger.WarnContext(ctx, "ImagePrepare: refusing an object at the prepared-image artifact name that was not prepared "+
		"for the requesting VMImage (or that makes the name ambiguous); it is never used, replaced or deleted", attrs...)
}

// artifactRetryError logs a vCenter failure of an identity prepare and returns
// a generic, retryable codes.Unavailable error. The underlying error can name
// other objects (MOIDs, fault text) and would otherwise land in the VMImage's
// status; its details go to the provider log only.
func (p *Provider) artifactRetryError(ctx context.Context, artifact, step string, err error) error {
	p.logger.ErrorContext(ctx, "ImagePrepare: vCenter call failed", "artifact", artifact, "step", step, "error", err)
	return status.Errorf(codes.Unavailable,
		"ImagePrepare: %s for the prepared-image artifact %q failed: a vCenter error occurred "+
			"(details in the provider log); will retry", step, artifact)
}

// stagedFileError passes a classified error (a gRPC status) through, and turns
// a raw failure to read back the staged download (a local I/O error) into the
// image-source error: it concerns this image's download only.
func (p *Provider) stagedFileError(ctx context.Context, err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return p.imageSourceError(ctx, "the downloaded image could not be read back on the provider", err)
}

// isVCenterUnreachable reports whether err, from a vCenter or ESXi call of an
// import, means the endpoint itself failed — its session, its connectivity or
// the provider's rights — rather than this image's content: a
// NotAuthenticated, InvalidLogin, NoPermission, HostCommunication or
// HostNotConnected fault, an untrusted certificate, or a transport-level error
// (a *url.Error or net.Error, a context deadline or cancellation).
func isVCenterUnreachable(err error) bool {
	if err == nil {
		return false
	}
	for _, f := range []types.BaseMethodFault{
		&types.NotAuthenticated{}, &types.InvalidLogin{}, &types.NoPermission{},
		&types.HostCommunication{}, &types.HostNotConnected{},
	} {
		if fault.Is(err, f) {
			return true
		}
	}
	if soap.IsCertificateUntrusted(err) {
		return true
	}
	var urlErr *url.Error
	var netErr net.Error
	return stderrors.As(err, &urlErr) || stderrors.As(err, &netErr) ||
		stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled)
}

// importFailure classifies a failed import step of this call's own object: a
// vCenter/ESXi endpoint failure (isVCenterUnreachable) is a generic retryable
// vCenter error that counts toward the manager's circuit breaker
// (artifactRetryError); anything else — vCenter refusing to create, upload or
// convert what this image's OVF describes — is an image-source error the
// manager retries without counting it (imageSourceError), so one tenant's
// crafted OVA cannot open the breaker for every tenant of the Provider.
func (p *Provider) importFailure(ctx context.Context, artifact, step string, err error) error {
	if isVCenterUnreachable(err) {
		return p.artifactRetryError(ctx, artifact, step, err)
	}
	return p.imageSourceError(ctx, "vCenter could not import the image", err, "artifact", artifact, "step", step)
}

// isProviderError reports whether err carries a gRPC status with code.
func isProviderError(err error, code codes.Code) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == code
}
