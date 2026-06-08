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

package vsphere

import (
	"archive/tar"
	"context"
	"crypto/md5"  // #nosec G501 -- md5 is an explicitly supported OVA checksum algorithm, not used for security
	"crypto/sha1" // #nosec G505 -- sha1 is an explicitly supported OVA checksum algorithm, not used for security
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25/progress"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// vsphereDefaultChecksumType is the checksum algorithm assumed when an OVA
	// source requests verification (non-empty checksum) but omits the algorithm.
	// It matches the v1beta1 VSphereImageSource kubebuilder default.
	vsphereDefaultChecksumType = "sha256"

	// ovaDiskProvisioningThin requests thin-provisioned disks for the imported
	// template, minimizing datastore consumption for a base image that is cloned
	// (and grown) per VM at Create time.
	ovaDiskProvisioningThin = string(types.OvfCreateImportSpecParamsDiskProvisioningTypeThin)
)

// vsphereImageSource is the normalized, provider-internal view of a VMImage's
// vSphere source, decoupled from the on-the-wire JSON shape ImagePrepare
// receives (see parseVSphereImageSource). Exactly one of TemplateName,
// ContentLibrary, or OVAURL is expected to be set; the caller enforces that and
// applies the documented precedence.
type vsphereImageSource struct {
	// TemplateName references a template expected to already exist in vCenter.
	// When set, ImagePrepare verifies its existence and does NOT import.
	TemplateName string
	// ContentLibrary references a content-library item. When set, ImagePrepare
	// verifies the item exists; deploying it to a template is out of scope here.
	ContentLibrary *vsphereContentLibraryRef
	// OVAURL is an HTTP(S) URL to an .ova or .ovf to import as a template. This
	// is the only source that performs a real import.
	OVAURL string
	// Checksum is the expected checksum of the downloaded OVA; empty disables
	// verification.
	Checksum string
	// ChecksumType is the checksum algorithm (md5/sha1/sha256/sha512); defaults
	// to sha256 when a Checksum is set but the algorithm is omitted.
	ChecksumType string
}

// vsphereContentLibraryRef mirrors v1beta1.ContentLibraryRef for the parsed
// source.
type vsphereContentLibraryRef struct {
	// Library is the content-library name.
	Library string
	// Item is the library-item name within Library.
	Item string
	// Version is the optional item version.
	Version string
}

// rawVSphereVMImageSpec mirrors the subset of v1beta1.VMImageSpec the vSphere
// provider consumes for ImagePrepare. It is parsed directly from the rich
// source.vsphere.* shape because that is the only serialized form able to carry
// contentLibrary and ovaURL. Unmarshalling into the v1beta1 types directly is
// avoided to keep the parser tolerant of partial/loosely-typed JSON.
type rawVSphereVMImageSpec struct {
	Source struct {
		VSphere *struct {
			TemplateName   string `json:"templateName"`
			OVAURL         string `json:"ovaURL"`
			Checksum       string `json:"checksum"`
			ChecksumType   string `json:"checksumType"`
			ContentLibrary *struct {
				Library string `json:"library"`
				Item    string `json:"item"`
				Version string `json:"version"`
			} `json:"contentLibrary"`
		} `json:"vsphere"`
	} `json:"source"`
}

// parseVSphereImageSource normalizes the JSON-encoded image spec carried by
// ImagePrepareRequest.ImageJson into a vsphereImageSource.
//
// Two shapes are accepted, in priority order, mirroring the libvirt PR-1
// approach (parseLibvirtImageSource):
//
//  1. The rich v1beta1.VMImageSpec shape with a nested
//     source.vsphere.{templateName,ovaURL,contentLibrary,checksum,checksumType}.
//     This is the ONLY shape that can express contentLibrary or ovaURL, so it is
//     tried first.
//  2. The flat, provider-agnostic contracts.VMImage shape that Create already
//     round-trips (server.go Create unmarshals ImageJson into a struct with
//     TemplateName/Path/Format). It can only express a TemplateName for vSphere
//     (Path is a libvirt/imported-disk concern), so it is the fallback.
//
// Whichever shape yields a usable source (any of templateName/ovaURL/
// contentLibrary) wins. An empty or unparseable ImageJson returns an empty
// source — the caller turns that into an InvalidSpec error so a no-op is never
// reported as success.
func parseVSphereImageSource(imageJSON string) vsphereImageSource {
	var src vsphereImageSource
	if strings.TrimSpace(imageJSON) == "" {
		return src
	}

	// Shape 1: rich v1beta1 spec with nested source.vsphere.
	var spec rawVSphereVMImageSpec
	if err := json.Unmarshal([]byte(imageJSON), &spec); err == nil && spec.Source.VSphere != nil {
		vs := spec.Source.VSphere
		if vs.TemplateName != "" || vs.OVAURL != "" || vs.ContentLibrary != nil {
			src = vsphereImageSource{
				TemplateName: vs.TemplateName,
				OVAURL:       vs.OVAURL,
				Checksum:     vs.Checksum,
				ChecksumType: vs.ChecksumType,
			}
			if vs.ContentLibrary != nil {
				src.ContentLibrary = &vsphereContentLibraryRef{
					Library: vs.ContentLibrary.Library,
					Item:    vs.ContentLibrary.Item,
					Version: vs.ContentLibrary.Version,
				}
			}
			return src
		}
	}

	// Shape 2: flat contracts.VMImage (Go field names, no JSON tags). For vSphere
	// only TemplateName is meaningful here.
	var img struct {
		TemplateName string
	}
	if err := json.Unmarshal([]byte(imageJSON), &img); err == nil && img.TemplateName != "" {
		src.TemplateName = img.TemplateName
	}

	return src
}

// vsphereChecksumHasher returns a hash.Hash for the requested algorithm. An
// empty or "sha256" type yields sha256 (the v1beta1 default). The boolean
// reports whether the algorithm was recognized, so callers can reject an
// explicitly-bad algorithm rather than silently hashing with the wrong one.
func vsphereChecksumHasher(checksumType string) (h hash.Hash, ok bool) {
	switch strings.ToLower(strings.TrimSpace(checksumType)) {
	case "", vsphereDefaultChecksumType:
		return sha256.New(), true
	case "md5":
		return md5.New(), true // #nosec G401 -- supported OVA verification algorithm, not used for security
	case "sha1":
		return sha1.New(), true // #nosec G401 -- supported OVA verification algorithm, not used for security
	case "sha512":
		return sha512.New(), true
	default:
		return sha256.New(), false
	}
}

// ImagePrepare implements the ProviderServer interface. It prepares a vSphere
// template named req.TargetName from the image source encoded in req.ImageJson,
// honoring three source kinds in precedence order:
//
//  1. Idempotency gate: if a template/VM named TargetName already exists, it
//     returns success without importing — a re-run is a cheap no-op.
//  2. source.vsphere.templateName: verify-only. The named template must already
//     exist; if found, success; if missing, a NotFound error. No download.
//  3. source.vsphere.contentLibrary: verify-only. The library item must exist;
//     deploying it to a template is out of scope for this PR.
//  4. source.vsphere.ovaURL: the real work. Download the OVA/OVF, optionally
//     verify its checksum, import it into vCenter via an NFC lease, and mark the
//     resulting VM as a template.
//
// The import is driven synchronously, so the returned TaskResponse carries an
// empty (nil) Task — consistent with the libvirt provider — which the controller
// treats as "completed synchronously". Very large OVAs may eventually warrant an
// async TaskRef the controller polls (a future enhancement; the gRPC client
// timeout is tracked separately in PR-4 of #154).
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil || p.finder == nil {
		return nil, errors.NewUnavailable("vSphere", fmt.Errorf("provider client not initialized"))
	}
	targetName := strings.TrimSpace(req.GetTargetName())
	if targetName == "" {
		return nil, errors.NewInvalidSpec("ImagePrepare target name is required")
	}

	src := parseVSphereImageSource(req.GetImageJson())

	p.logger.Info("ImagePrepare: starting",
		"target_name", targetName,
		"has_template_name", src.TemplateName != "",
		"has_content_library", src.ContentLibrary != nil,
		"has_ova_url", src.OVAURL != "",
		"storage_hint", req.GetStorageHint(),
	)

	// Set datacenter context so finder lookups (VM, datastore, folder) resolve.
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("ImagePrepare: find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// 1) Idempotency gate (load-bearing): a re-run must never re-import. If a
	//    template/VM named targetName already exists, we are done.
	if vm := p.findExistingByName(ctx, targetName); vm != nil {
		p.logger.Info("ImagePrepare: target already exists; nothing to do",
			"target_name", targetName, "vm", vm.Reference().Value)
		return &providerv1.TaskResponse{}, nil
	}

	switch {
	case src.TemplateName != "":
		// 2) templateName: verify-only. Don't fabricate an import.
		return p.imagePrepareVerifyTemplate(ctx, src.TemplateName)
	case src.ContentLibrary != nil:
		// 3) contentLibrary: verify-only.
		return p.imagePrepareVerifyContentLibrary(ctx, src.ContentLibrary)
	case src.OVAURL != "":
		// 4) ovaURL: the real import.
		return p.imagePrepareImportOVA(ctx, src, targetName, req.GetStorageHint())
	default:
		return nil, errors.NewInvalidSpec(
			"ImagePrepare requires a vSphere image source with templateName, contentLibrary, or ovaURL " +
				"(source.vsphere.*)")
	}
}

// findExistingByName returns a VirtualMachine matching name (a plain name or an
// inventory path), or nil if none is found. It first tries the configured
// DefaultFolder, then falls back to a global search, so an existing template is
// detected regardless of where it lives. Any lookup error is treated as "not
// found" so preparation proceeds rather than skipping work on a false negative.
//
// Both powered-off VMs and templates are returned by the finder; the caller only
// cares that the name is taken, so no template/VM distinction is made here.
func (p *Provider) findExistingByName(ctx context.Context, name string) *object.VirtualMachine {
	// Scoped search within the default folder first (cheaper, avoids unrelated
	// same-named VMs in other folders shadowing a real template).
	if folder := strings.TrimSpace(p.config.DefaultFolder); folder != "" {
		if vms, err := p.finder.VirtualMachineList(ctx, path.Join(folder, name)); err == nil && len(vms) > 0 {
			return vms[0]
		}
	}
	// Global search by name.
	if vm, err := p.finder.VirtualMachine(ctx, name); err == nil && vm != nil {
		return vm
	}
	return nil
}

// imagePrepareVerifyTemplate implements the templateName source: the named
// template must already exist in vCenter. On success the prepared image is the
// template itself, so no import is performed. A missing template yields an
// honest NotFound rather than a fabricated success.
func (p *Provider) imagePrepareVerifyTemplate(ctx context.Context, templateName string) (*providerv1.TaskResponse, error) {
	vm, err := p.finder.VirtualMachine(ctx, templateName)
	if err != nil {
		if _, ok := err.(*find.NotFoundError); ok {
			return nil, errors.NewNotFound("vSphere template", templateName)
		}
		return nil, fmt.Errorf("ImagePrepare: look up template %q: %w", templateName, err)
	}
	p.logger.Info("ImagePrepare: template exists; nothing to import",
		"template_name", templateName, "vm", vm.Reference().Value)
	return &providerv1.TaskResponse{}, nil
}

// imagePrepareVerifyContentLibrary implements the contentLibrary source: verify
// the referenced library and item exist via the vCenter REST (vAPI) endpoint.
// Deploying a content-library item into a standalone template is intentionally
// out of scope for this PR; an existing-but-unhandled item returns a clear
// InvalidSpec pointing the user at templateName/ovaURL rather than a fake
// success.
func (p *Provider) imagePrepareVerifyContentLibrary(ctx context.Context, ref *vsphereContentLibraryRef) (*providerv1.TaskResponse, error) {
	if ref.Library == "" || ref.Item == "" {
		return nil, errors.NewInvalidSpec("ImagePrepare contentLibrary requires both library and item")
	}

	// The REST/vAPI client is a separate session from the vim25 SOAP client; log
	// in with the same credentials and log out when done.
	rc := rest.NewClient(p.client.Client)
	userInfo := url.UserPassword(p.config.Username, p.config.Password)
	if err := rc.Login(ctx, userInfo); err != nil {
		return nil, errors.NewUnavailable("vSphere content library (vAPI)", err)
	}
	defer func() {
		if err := rc.Logout(ctx); err != nil {
			p.logger.Warn("ImagePrepare: content-library REST logout failed", "error", err)
		}
	}()

	mgr := library.NewManager(rc)
	libs, err := mgr.FindLibrary(ctx, library.Find{Name: ref.Library})
	if err != nil {
		return nil, fmt.Errorf("ImagePrepare: find content library %q: %w", ref.Library, err)
	}
	if len(libs) == 0 {
		return nil, errors.NewNotFound("vSphere content library", ref.Library)
	}

	items, err := mgr.FindLibraryItems(ctx, library.FindItem{LibraryID: libs[0], Name: ref.Item})
	if err != nil {
		return nil, fmt.Errorf("ImagePrepare: find library item %q in %q: %w", ref.Item, ref.Library, err)
	}
	if len(items) == 0 {
		return nil, errors.NewNotFound("vSphere content library item", fmt.Sprintf("%s/%s", ref.Library, ref.Item))
	}

	p.logger.Info("ImagePrepare: content-library item exists",
		"library", ref.Library, "item", ref.Item, "library_id", libs[0], "item_id", items[0])

	// Verified, but deploying CL item -> template is not implemented in this PR.
	return nil, errors.NewInvalidSpec(
		"ImagePrepare: content library item %q/%q exists but content-library prepare is not yet implemented; "+
			"use source.vsphere.templateName or source.vsphere.ovaURL", ref.Library, ref.Item)
}

// imagePrepareImportOVA implements the ovaURL source: the real import. It
// resolves placement (resource pool, datastore, folder), downloads the OVA/OVF
// to a temp file (optionally verifying its checksum), imports it into vCenter via
// an NFC lease using govmomi's ovf/importer, marks the resulting VM as a
// template, and cleans up the partially-created VM on any mid-import failure so a
// retry starts clean.
func (p *Provider) imagePrepareImportOVA(ctx context.Context, src vsphereImageSource, targetName, storageHint string) (*providerv1.TaskResponse, error) {
	placement, err := p.resolveImagePlacement(ctx, storageHint)
	if err != nil {
		return nil, err
	}

	// Download the OVA/OVF to a temp file. The provider pod streams the bytes and
	// then pushes the VMDKs to vCenter over the NFC lease; staging to disk first
	// keeps the import path simple and lets a FileArchive/TapeArchive re-open the
	// descriptor and each disk by name.
	localPath, cleanupDownload, err := p.downloadOVA(ctx, src.OVAURL)
	if err != nil {
		return nil, err
	}
	defer cleanupDownload()

	if src.Checksum != "" {
		if err := verifyFileChecksum(localPath, src.Checksum, src.ChecksumType); err != nil {
			return nil, err
		}
	}

	// Choose the archive based on the source kind: an .ova is a tar (TapeArchive);
	// a bare .ovf is a plain file with sibling .vmdk(s) (FileArchive). For an OVA the
	// descriptor's exact entry name is resolved up front (skipping macOS sidecars).
	archive, descriptorPath, err := p.newOVAArchive(localPath, src.OVAURL)
	if err != nil {
		return nil, err
	}

	imp := &importer.Importer{
		Log:          p.ovaImportLog,
		Sinker:       progress.NewProgressLogger(p.ovaImportLog, "ImagePrepare upload"),
		Client:       p.client.Client,
		Finder:       p.finder,
		Datacenter:   placement.datacenter,
		Datastore:    placement.datastore,
		ResourcePool: placement.resourcePool,
		Folder:       placement.folder,
		Archive:      archive,
	}

	opts := importer.Options{
		// Accept-all EULA / no interactive properties: rely on OVF defaults.
		DiskProvisioning: ovaDiskProvisioningThin,
		// Name the resulting VM after the target so the idempotency gate and the
		// downstream MarkAsTemplate operate on a deterministically-named object.
		Name: &targetName,
		// Do not power on or mark-as-template here; we mark-as-template explicitly
		// after the import succeeds so a partial import never yields a template.
	}

	p.logger.Info("ImagePrepare: importing OVA into vCenter",
		"target_name", targetName,
		"ova_url", src.OVAURL,
		"datastore", placement.datastore.Name(),
		"resource_pool", placement.resourcePool.Reference().Value,
		"folder", placement.folder.Reference().Value,
	)

	moref, err := imp.Import(ctx, descriptorPath, opts)
	if err != nil {
		return nil, errors.NewInternal(fmt.Sprintf("ImagePrepare: import OVA %q as %q", src.OVAURL, targetName), err)
	}

	vm := object.NewVirtualMachine(p.client.Client, *moref)

	// Promote the imported VM to a template. On failure, clean up the VM so a
	// retry starts clean (the idempotency gate would otherwise treat the
	// powered-off VM as "already prepared").
	if err := vm.MarkAsTemplate(ctx); err != nil {
		p.cleanupPartialImport(ctx, vm, targetName)
		return nil, errors.NewInternal(fmt.Sprintf("ImagePrepare: mark imported VM %q as template", targetName), err)
	}

	p.logger.Info("ImagePrepare: imported OVA and marked as template",
		"target_name", targetName, "vm", vm.Reference().Value)
	return &providerv1.TaskResponse{}, nil
}

// imagePlacement bundles the resolved vSphere placement objects for an OVA
// import.
type imagePlacement struct {
	datacenter   *object.Datacenter
	resourcePool *object.ResourcePool
	datastore    *object.Datastore
	folder       *object.Folder
}

// resolveImagePlacement resolves where an imported template lands, mirroring the
// priority chain createVirtualMachine uses but driven solely by provider
// defaults and the request's storage hint (ImagePrepare has no per-VM placement
// overrides):
//
//   - ResourcePool: DefaultCluster's root resource pool (or the finder default
//     pool when DefaultCluster is empty).
//   - Datastore:    storageHint → DefaultDatastore → a datastore from
//     DefaultStoragePod (datastore cluster).
//   - Folder:       DefaultFolder → datacenter default VM folder.
func (p *Provider) resolveImagePlacement(ctx context.Context, storageHint string) (*imagePlacement, error) {
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("ImagePrepare: find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Resource pool from the default cluster (or the finder default pool).
	var resourcePool *object.ResourcePool
	if cluster := strings.TrimSpace(p.config.DefaultCluster); cluster != "" {
		cc, err := p.finder.ClusterComputeResource(ctx, cluster)
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: find cluster %q: %w", cluster, err)
		}
		resourcePool, err = cc.ResourcePool(ctx)
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: resource pool of cluster %q: %w", cluster, err)
		}
	} else {
		resourcePool, err = p.finder.ResourcePoolOrDefault(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: resolve default resource pool: %w", err)
		}
	}

	// Datastore: storageHint, then DefaultDatastore, then DefaultStoragePod.
	datastore, err := p.resolveImageDatastore(ctx, storageHint)
	if err != nil {
		return nil, err
	}

	// Folder: DefaultFolder, falling back to the datacenter VM folder.
	folder, err := p.resolveImageFolder(ctx, datacenter)
	if err != nil {
		return nil, err
	}

	return &imagePlacement{
		datacenter:   datacenter,
		resourcePool: resourcePool,
		datastore:    datastore,
		folder:       folder,
	}, nil
}

// resolveImageDatastore picks the datastore for an OVA import: the request's
// storage hint wins, then the provider DefaultDatastore, then a datastore chosen
// from the DefaultStoragePod (datastore cluster) by free space. An empty result
// with no candidates is an InvalidSpec.
func (p *Provider) resolveImageDatastore(ctx context.Context, storageHint string) (*object.Datastore, error) {
	if hint := strings.TrimSpace(storageHint); hint != "" {
		ds, err := p.finder.Datastore(ctx, hint)
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: find datastore from storage hint %q: %w", hint, err)
		}
		return ds, nil
	}
	if ds := strings.TrimSpace(p.config.DefaultDatastore); ds != "" {
		found, err := p.finder.Datastore(ctx, ds)
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: find default datastore %q: %w", ds, err)
		}
		return found, nil
	}
	if pod := strings.TrimSpace(p.config.DefaultStoragePod); pod != "" {
		ds, err := p.resolveDatastoreFromStoragePod(ctx, pod)
		if err != nil {
			return nil, fmt.Errorf("ImagePrepare: resolve datastore from StoragePod %q: %w", pod, err)
		}
		return ds, nil
	}
	return nil, errors.NewInvalidSpec(
		"ImagePrepare: no datastore available (set storage_hint, or the provider's " +
			"defaultDatastore / defaultStoragePod)")
}

// resolveImageFolder resolves the folder an imported template lands in:
// DefaultFolder, falling back to the datacenter's default VM folder.
func (p *Provider) resolveImageFolder(ctx context.Context, datacenter *object.Datacenter) (*object.Folder, error) {
	folderName := strings.TrimSpace(p.config.DefaultFolder)
	if folderName != "" {
		if folder, err := p.finder.Folder(ctx, folderName); err == nil {
			return folder, nil
		}
		p.logger.Warn("ImagePrepare: default folder not found; using datacenter VM folder",
			"folder", folderName)
	}
	folder, err := p.finder.Folder(ctx, datacenter.Name()+"/vm")
	if err != nil {
		return nil, fmt.Errorf("ImagePrepare: find datacenter VM folder: %w", err)
	}
	return folder, nil
}

// downloadOVA streams the OVA/OVF at ovaURL to a temp file on the provider pod's
// filesystem and returns the local path plus a cleanup func that removes it. The
// cleanup is always safe to call (it tolerates an already-removed file). A
// non-2xx response or transport error is reported as Unavailable (retryable).
func (p *Provider) downloadOVA(ctx context.Context, ovaURL string) (localPath string, cleanup func(), err error) {
	noop := func() {}

	ext := strings.ToLower(filepath.Ext(ovaURL))
	if ext != ".ova" && ext != ".ovf" {
		// Default to .ova so the archive selection has a deterministic shape; the
		// importer ultimately parses the descriptor regardless of extension.
		ext = ".ova"
	}

	tmp, err := os.CreateTemp("", "virtrigaud-ova-*"+ext)
	if err != nil {
		return "", noop, fmt.Errorf("ImagePrepare: create temp file for OVA: %w", err)
	}
	localPath = tmp.Name()
	cleanup = func() {
		_ = tmp.Close()
		if rmErr := os.Remove(localPath); rmErr != nil && !os.IsNotExist(rmErr) {
			p.logger.Warn("ImagePrepare: failed to remove temp OVA", "path", localPath, "error", rmErr)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ovaURL, nil)
	if err != nil {
		cleanup()
		return "", noop, errors.NewInvalidSpec("ImagePrepare: invalid OVA URL %q: %v", ovaURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cleanup()
		return "", noop, errors.NewUnavailable("OVA download", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cleanup()
		return "", noop, errors.NewUnavailable("OVA download",
			fmt.Errorf("unexpected HTTP status %d downloading %q", resp.StatusCode, ovaURL))
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("ImagePrepare: write OVA to temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("ImagePrepare: flush OVA temp file: %w", err)
	}

	p.logger.Info("ImagePrepare: downloaded OVA", "url", ovaURL, "path", localPath)
	return localPath, cleanup, nil
}

// newOVAArchive returns the importer.Archive and the descriptor path to pass to
// importer.Import for the staged file. A bare .ovf is a plain file with sibling
// disks (FileArchive, descriptor is the file itself). An .ova is a tar of the
// descriptor + disks (TapeArchive); the descriptor's EXACT entry name is resolved
// up front (skipping macOS AppleDouble sidecars) rather than relying on a "*.ovf"
// glob, which would otherwise match a "._foo.ovf" metadata file packed before the
// real descriptor by macOS tar.
func (p *Provider) newOVAArchive(localPath, ovaURL string) (importer.Archive, string, error) {
	opener := importer.Opener{Client: p.client.Client}
	if strings.EqualFold(filepath.Ext(ovaURL), ".ovf") {
		return &importer.FileArchive{Path: localPath, Opener: opener}, localPath, nil
	}
	descriptor, err := findOVADescriptorName(localPath)
	if err != nil {
		return nil, "", err
	}
	return &importer.TapeArchive{Path: localPath, Opener: opener}, descriptor, nil
}

// findOVADescriptorName returns the exact base name of the OVF descriptor inside
// an OVA tar. It skips macOS AppleDouble sidecar files (a leading "._" on the base
// name) and __MACOSX/ directory entries, which OVAs repackaged on macOS include
// and which precede the real descriptor in tar order. govmomi's TapeArchive
// matches the descriptor by glob, so without this filter a "._foo.ovf" sidecar (a
// small binary AppleDouble blob) is parsed as the descriptor and fails with an
// XML null-byte error. Returning the exact name also makes the subsequent disk
// uploads resolve their hrefs exactly, never matching a "._disk.vmdk" sidecar.
func findOVADescriptorName(ovaPath string) (string, error) {
	f, err := os.Open(filepath.Clean(ovaPath))
	if err != nil {
		return "", fmt.Errorf("open OVA to locate descriptor: %w", err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read OVA tar to locate descriptor: %w", err)
		}
		if strings.Contains(h.Name, "__MACOSX/") {
			continue
		}
		base := path.Base(h.Name)
		if strings.HasPrefix(base, "._") {
			continue // macOS AppleDouble sidecar
		}
		if strings.EqualFold(filepath.Ext(base), ".ovf") {
			return base, nil
		}
	}
	return "", errors.NewInvalidSpec("OVA contains no .ovf descriptor (after skipping macOS sidecar files)")
}

// ovaImportLog adapts govmomi's progress.LogFunc to the provider's structured
// logger, surfacing importer warnings/upload progress lines at debug level.
func (p *Provider) ovaImportLog(msg string) (int, error) {
	p.logger.Debug("ImagePrepare: ova import", "msg", strings.TrimRight(msg, "\n"))
	return len(msg), nil
}

// cleanupPartialImport best-effort destroys a VM left behind by a failed import
// (e.g. MarkAsTemplate failed after ImportVApp succeeded) so a retry starts
// clean and the idempotency gate does not mistake the orphan for a prepared
// template. Failures are logged, not returned.
func (p *Provider) cleanupPartialImport(ctx context.Context, vm *object.VirtualMachine, targetName string) {
	task, err := vm.Destroy(ctx)
	if err != nil {
		p.logger.Warn("ImagePrepare: failed to start cleanup of partial import",
			"target_name", targetName, "vm", vm.Reference().Value, "error", err)
		return
	}
	if err := task.Wait(ctx); err != nil {
		p.logger.Warn("ImagePrepare: cleanup of partial import did not complete",
			"target_name", targetName, "vm", vm.Reference().Value, "error", err)
		return
	}
	p.logger.Info("ImagePrepare: cleaned up partial import",
		"target_name", targetName, "vm", vm.Reference().Value)
}

// verifyFileChecksum verifies that the file at path hashes to expected using the
// requested algorithm. An empty expected disables verification (no-op). An
// unknown algorithm or a mismatch returns an InvalidSpec error so the caller
// rejects the OVA before importing it.
func verifyFileChecksum(path, expected, checksumType string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	h, known := vsphereChecksumHasher(checksumType)
	if !known {
		return errors.NewInvalidSpec(
			"ImagePrepare: unsupported checksum type %q (want md5/sha1/sha256/sha512)", checksumType)
	}

	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("ImagePrepare: open OVA for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("ImagePrepare: hash OVA: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(expected)) {
		return errors.NewInvalidSpec(
			"ImagePrepare: checksum mismatch for OVA: expected %s, got %s", expected, got)
	}
	return nil
}
