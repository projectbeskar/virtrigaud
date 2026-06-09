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

package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// defaultProxmoxStorage is the Proxmox storage used for image preparation when
	// the caller supplies neither a storage hint nor a source-level storage. It is
	// the last-resort fallback; local-lvm is the default file/block storage present
	// on a stock single-node PVE install.
	defaultProxmoxStorage = "local-lvm"

	// defaultProxmoxImageFormat is the disk format assumed for an imported image
	// when the source omits one. It matches the v1beta1 ProxmoxImageSource
	// kubebuilder default.
	defaultProxmoxImageFormat = "qcow2"

	// proxmoxTemplateFlag is the value PVE sets on the `template` field of a VM
	// that has been converted to a template.
	proxmoxTemplateFlag = 1
)

// proxmoxImageSource is the normalized, provider-internal view of a VMImage's
// Proxmox source, decoupled from the on-the-wire JSON shapes ImagePrepare may
// receive (see parseProxmoxImageSource).
//
// Two mutually-exclusive intents are expressed:
//
//   - A reference to an EXISTING template (TemplateID or TemplateName set): the
//     template is verified to exist; nothing is imported.
//   - An IMPORT request (URL set): a disk image is downloaded into the target
//     storage and a template named after the request's TargetName is prepared.
//
// All fields are optional; the caller enforces that at least one usable source is
// present and applies the documented precedence.
type proxmoxImageSource struct {
	// TemplateID references an existing Proxmox template by VMID. When non-nil an
	// existing template is referenced and no import is performed.
	TemplateID *int
	// TemplateName references an existing Proxmox template by name. When set an
	// existing template is referenced and no import is performed.
	TemplateName string
	// URL is an HTTP(S) location of a disk image to import (source.http.url). It is
	// the only source that performs a real import.
	URL string
	// Storage is the source-preferred Proxmox storage to import into; overridden by
	// the request's StorageHint when that is set, and falls back to
	// defaultProxmoxStorage.
	Storage string
	// Node is the source-preferred Proxmox node the template lives on / is imported
	// to; falls back to the client's FindNode default.
	Node string
	// Format is the disk format of the imported image (raw/qcow2/vmdk); defaults to
	// defaultProxmoxImageFormat when unset.
	Format string
}

// referencesExistingTemplate reports whether the source points at a template that
// is expected to already exist (verify-only path) rather than requesting an
// import.
func (s proxmoxImageSource) referencesExistingTemplate() bool {
	return s.TemplateID != nil || strings.TrimSpace(s.TemplateName) != ""
}

// isEmpty reports whether no usable source was parsed (neither an existing
// template reference nor an import URL). The caller turns this into an
// InvalidSpec so a no-op is never reported as success.
func (s proxmoxImageSource) isEmpty() bool {
	return !s.referencesExistingTemplate() && strings.TrimSpace(s.URL) == ""
}

// rawProxmoxVMImageSpec mirrors the subset of v1beta1.VMImageSpec the Proxmox
// provider consumes for ImagePrepare. It is parsed directly from the rich
// source.{proxmox,http}.* shape because that is the only serialized form able to
// carry the Proxmox-native fields (templateID/storage/node/format) and the
// generic HTTP import URL. Unmarshalling into the v1beta1 types directly is
// avoided to keep the parser tolerant of partial/loosely-typed JSON.
type rawProxmoxVMImageSpec struct {
	Source struct {
		Proxmox *struct {
			TemplateID   *int   `json:"templateID"`
			TemplateName string `json:"templateName"`
			Storage      string `json:"storage"`
			Node         string `json:"node"`
			Format       string `json:"format"`
		} `json:"proxmox"`
		HTTP *struct {
			URL string `json:"url"`
		} `json:"http"`
	} `json:"source"`
}

// parseProxmoxImageSource normalizes the JSON-encoded image spec carried by
// ImagePrepareRequest.ImageJson into a proxmoxImageSource.
//
// Two shapes are accepted, in priority order, mirroring the libvirt (PR-1) and
// vSphere (PR-2) parsers:
//
//  1. The rich v1beta1.VMImageSpec shape with nested
//     source.proxmox.{templateID,templateName,storage,node,format} and/or
//     source.http.url. This is the ONLY shape that can express the Proxmox-native
//     fields, so it is tried first. A source.proxmox reference to an existing
//     template takes precedence over a source.http import URL when both appear.
//  2. The flat, provider-agnostic contracts.VMImage shape that Create already
//     round-trips (server.go Create reads TemplateName from it). It can express a
//     TemplateName (existing template) or a URL (import); it carries no
//     storage/node/format.
//
// An empty or unparseable ImageJson, or one carrying neither an existing-template
// reference nor an import URL, returns an empty source — the caller turns that
// into an InvalidSpec error so a no-op is never reported as success.
func parseProxmoxImageSource(imageJSON string) proxmoxImageSource {
	var src proxmoxImageSource
	if strings.TrimSpace(imageJSON) == "" {
		return src
	}

	// Shape 1: rich v1beta1 spec with nested source.proxmox / source.http.
	var spec rawProxmoxVMImageSpec
	if err := json.Unmarshal([]byte(imageJSON), &spec); err == nil {
		if px := spec.Source.Proxmox; px != nil {
			src.TemplateID = px.TemplateID
			src.TemplateName = px.TemplateName
			src.Storage = px.Storage
			src.Node = px.Node
			src.Format = px.Format
		}
		if http := spec.Source.HTTP; http != nil {
			src.URL = http.URL
		}
		if !src.isEmpty() {
			return src
		}
	}

	// Shape 2: flat contracts.VMImage (Go field names, no JSON tags). It can only
	// express a TemplateName (existing template) or a URL (import) for Proxmox.
	var img contracts.VMImage
	if err := json.Unmarshal([]byte(imageJSON), &img); err == nil {
		if img.TemplateName != "" || img.URL != "" {
			src.TemplateName = img.TemplateName
			src.URL = img.URL
			src.Format = img.Format
		}
	}

	return src
}

// resolveImageStorage selects the Proxmox storage to import into, in priority
// order: the request StorageHint, then source.proxmox.storage, then the provider
// default. This mirrors the libvirt/vSphere hint-or-default behavior while
// additionally honoring the image's own storage preference.
func resolveImageStorage(storageHint, sourceStorage string) string {
	switch {
	case strings.TrimSpace(storageHint) != "":
		return strings.TrimSpace(storageHint)
	case strings.TrimSpace(sourceStorage) != "":
		return strings.TrimSpace(sourceStorage)
	default:
		return defaultProxmoxStorage
	}
}

// resolveImageNode selects the Proxmox node, preferring the source-supplied node
// and falling back to the client's FindNode default. A FindNode failure is only
// surfaced when no source node was given, so an explicit node short-circuits node
// discovery entirely.
func (p *Provider) resolveImageNode(ctx context.Context, sourceNode string) (string, error) {
	if n := strings.TrimSpace(sourceNode); n != "" {
		return n, nil
	}
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return "", errors.NewInternal("ImagePrepare: find Proxmox node", err)
	}
	return node, nil
}

// ImagePrepare implements the ProviderServer interface. It prepares a Proxmox
// template named req.TargetName from the image source encoded in req.ImageJson,
// honoring the source kinds in precedence order:
//
//  1. source.proxmox.{templateID,templateName}: verify-only. The referenced
//     template must already exist on the node; if found, success; if missing, an
//     honest NotFound error. No import.
//  2. source.http.url: the real import. Requires a non-empty TargetName.
//     Idempotency-gated: if a template named TargetName already exists, it is a
//     no-op success; otherwise the image is downloaded into the resolved storage
//     and prepared as the named template.
//
// The import is driven by a PVE task, so the returned ImagePrepareResponse
// carries the task ref when the API reports one (the controller polls it); a
// verify-only or idempotent no-op returns an empty Task, which the controller
// treats as "completed synchronously" — consistent with the libvirt and vSphere
// providers. In every case prepared_image_id is the template's name/VMID, which
// is deterministic and known at trigger time even on the async import path — so
// the manager can stamp it immediately and create VMs from the prepared template
// without re-resolving the source (issue #154, PR-6 / #214). prepared_image_path
// is empty: Proxmox addresses templates by name/VMID, not an on-disk path.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	targetName := strings.TrimSpace(req.GetTargetName())
	src := parseProxmoxImageSource(req.GetImageJson())

	p.logger.Info("ImagePrepare: starting",
		"target_name", targetName,
		"has_template_id", src.TemplateID != nil,
		"has_template_name", src.TemplateName != "",
		"has_url", src.URL != "",
		"storage_hint", req.GetStorageHint(),
	)

	if src.isEmpty() {
		return nil, errors.NewInvalidSpec(
			"ImagePrepare requires a Proxmox image source with an existing template " +
				"(source.proxmox.templateID / source.proxmox.templateName) or an import URL " +
				"(source.http.url)")
	}

	node, err := p.resolveImageNode(ctx, src.Node)
	if err != nil {
		return nil, err
	}

	// Verify-only path: an existing template is referenced; never import.
	if src.referencesExistingTemplate() {
		return p.imagePrepareVerifyTemplate(ctx, node, src)
	}

	// Import path: source.http.url. A target name is mandatory — never fabricate
	// one from the URL basename.
	if targetName == "" {
		return nil, errors.NewInvalidSpec(
			"ImagePrepare target name is required when importing from a URL (source.http.url)")
	}
	return p.imagePrepareImport(ctx, node, targetName, src, req.GetStorageHint())
}

// imagePrepareVerifyTemplate implements the existing-template source: the
// referenced template (by VMID or name) must already exist on the node and be a
// template (template=1). On success the prepared image is the template itself, so
// no import is performed. A missing template yields an honest NotFound rather than
// a fabricated success.
func (p *Provider) imagePrepareVerifyTemplate(ctx context.Context, node string, src proxmoxImageSource) (*providerv1.ImagePrepareResponse, error) {
	// By VMID: a direct GetVM is cheaper and unambiguous.
	if src.TemplateID != nil {
		vmid := *src.TemplateID
		vm, err := p.client.GetVM(ctx, node, vmid)
		if err != nil {
			if err == pveapi.ErrVMNotFound {
				return nil, errors.NewNotFound("Proxmox template", fmt.Sprintf("%d", vmid))
			}
			return nil, errors.NewInternal(fmt.Sprintf("ImagePrepare: look up template %d on node %q", vmid, node), err)
		}
		if vm.Template != proxmoxTemplateFlag {
			return nil, errors.NewInvalidSpec(
				"ImagePrepare: VM %d on node %q exists but is not a template", vmid, node)
		}
		p.logger.Info("ImagePrepare: template exists; nothing to import",
			"node", node, "template_id", vmid, "name", vm.Name)
		// The prepared image is the existing template, addressed by its VMID.
		return imagePrepareDone(fmt.Sprintf("%d", vmid)), nil
	}

	// By name: list templates on the node and match.
	name := strings.TrimSpace(src.TemplateName)
	vm, err := p.findTemplateByName(ctx, node, name)
	if err != nil {
		return nil, err
	}
	if vm == nil {
		return nil, errors.NewNotFound("Proxmox template", name)
	}
	p.logger.Info("ImagePrepare: template exists; nothing to import",
		"node", node, "template_name", name, "template_id", vm.VMID)
	// The prepared image is the existing template, addressed by its name.
	return imagePrepareDone(name), nil
}

// findTemplateByName returns the template VM matching name on the node, or nil if
// none is found. Only VMs flagged as templates (template=1) are considered, so a
// running VM that merely shares the name does not satisfy a template reference.
func (p *Provider) findTemplateByName(ctx context.Context, node, name string) (*pveapi.VM, error) {
	vms, err := p.client.ListVMs(ctx, node)
	if err != nil {
		return nil, errors.NewInternal(fmt.Sprintf("ImagePrepare: list VMs on node %q", node), err)
	}
	for _, vm := range vms {
		if vm == nil {
			continue
		}
		if vm.Name == name && vm.Template == proxmoxTemplateFlag {
			return vm, nil
		}
	}
	return nil, nil
}

// imagePrepareImport implements the source.http.url import path. It is
// idempotency-gated first: if a template named targetName already exists on the
// node, it returns success without importing. Otherwise it resolves the target
// storage and asks PVE to download/convert the image into a template named
// targetName.
func (p *Provider) imagePrepareImport(ctx context.Context, node, targetName string, src proxmoxImageSource, storageHint string) (*providerv1.ImagePrepareResponse, error) {
	// Idempotency gate (load-bearing): a re-run must never re-import. If a template
	// named targetName already exists on the node, we are done — the prepared image
	// is that template, addressed by name.
	existing, err := p.findTemplateByName(ctx, node, targetName)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		p.logger.Info("ImagePrepare: target template already exists; nothing to do",
			"node", node, "target_name", targetName, "template_id", existing.VMID)
		return imagePrepareDone(targetName), nil
	}

	storage := resolveImageStorage(storageHint, src.Storage)
	format := strings.TrimSpace(src.Format)
	if format == "" {
		format = defaultProxmoxImageFormat
	}

	p.logger.Info("ImagePrepare: importing image into template",
		"node", node, "target_name", targetName, "storage", storage,
		"format", format, "url", src.URL)

	taskID, err := p.client.PrepareImage(ctx, node, storage, src.URL, targetName, format)
	if err != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("ImagePrepare: import image %q as template %q", src.URL, targetName), err)
	}

	// The prepared template's name is deterministic and known here — populate
	// prepared_image_id in the SAME response as the task ref so the manager can
	// stamp the location now, even though the PVE task is still running (issue
	// #154, PR-6 / #214). The manager keeps Available=false until the task
	// completes, but it never has to re-discover the template name.
	result := imagePrepareDone(targetName)
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}
	return result, nil
}

// imagePrepareDone builds an ImagePrepareResponse whose prepared_image_id is the
// Proxmox template's name/VMID. prepared_image_path is left empty because Proxmox
// clones templates by name/VMID, not an on-disk path the manager consumes. The
// Task is left nil; callers populate it on the async import path (issue #154,
// PR-6 / #214).
func imagePrepareDone(templateRef string) *providerv1.ImagePrepareResponse {
	return &providerv1.ImagePrepareResponse{PreparedImageId: templateRef}
}

// Compile-time assertion that the v1beta1 image source shape this provider parses
// stays in sync with the API: if ProxmoxImageSource loses one of these fields,
// the build breaks here, prompting parseProxmoxImageSource to be revisited.
var _ = func() v1beta1.ProxmoxImageSource {
	return v1beta1.ProxmoxImageSource{
		TemplateID:   nil,
		TemplateName: "",
		Storage:      "",
		Node:         "",
		Format:       "",
		FullClone:    nil,
	}
}
