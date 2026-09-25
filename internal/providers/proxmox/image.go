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
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// proxmoxProviderType is the provider_type label of this provider's metrics
	// (the ADR-0009 D7 legacy-request counter).
	proxmoxProviderType = "proxmox"

	// proxmoxTemplateFlag is the value PVE sets on the `template` field of a VM
	// that has been converted to a template.
	proxmoxTemplateFlag = 1

	// urlImportUnsupportedMessage is the InvalidSpec message of every Proxmox URL
	// image import (ADR-0009 D10), with or without an image identity. It reaches
	// the VMImage status (reason InvalidSource), so it names no URL, storage,
	// node or other object: only what the image's owner can do instead.
	urlImportUnsupportedMessage = "Proxmox URL image import (source.http) is not supported in this release: " +
		"it does not yet produce a usable template (ADR-0009 Slice 6). Create the template on " +
		"Proxmox VE and reference it with source.proxmox.templateID"

	// emptySourceMessage is the InvalidSpec message of a request whose image
	// JSON carries no Proxmox source this provider can serve.
	emptySourceMessage = "ImagePrepare requires a Proxmox image source that references an existing template " +
		"(source.proxmox.templateID or source.proxmox.templateName)"
)

// proxmoxImageSource is the normalized, provider-internal view of a VMImage's
// Proxmox source, decoupled from the on-the-wire JSON shapes ImagePrepare may
// receive (see parseProxmoxImageSource).
//
// Two mutually-exclusive intents are expressed:
//
//   - A reference to an EXISTING template (TemplateID or TemplateName set): the
//     template is verified to exist; nothing is imported.
//   - An IMPORT request (URL set, no template reference): refused with
//     InvalidSpec in this release (ADR-0009 D10), because the import never
//     produced a usable template. ADR-0009 Slice 6 implements it.
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
	// URL is an HTTP(S) location of a disk image to import (source.http.url).
	// Without a template reference it makes the request an import, which is
	// refused in this release (ADR-0009 D10). It is never logged: a URL may
	// carry a token in its query string.
	URL string
	// Storage is the source-preferred Proxmox storage to import into. Parsed for
	// the ADR-0009 Slice 6 import; not used until then.
	Storage string
	// Node is the source-preferred Proxmox node the template lives on / is imported
	// to; falls back to the client's FindNode default.
	Node string
	// Format is the disk format of the imported image (raw/qcow2/vmdk). Parsed
	// for the ADR-0009 Slice 6 import; not used until then.
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

// ImagePrepare implements the ProviderServer interface (ADR-0009 D7, D10).
//
// The request is validated first with imageartifact.ParseRequest, so a
// malformed request is refused (InvalidArgument) before anything else. A legacy
// request (no image identity, a bare target_name, from a manager older than
// ADR-0009) then emits the deprecation signal (a WARN log and one increment of
// virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"})
// and is served exactly like an identity request: this provider has no working
// bare-name path to keep, so in neither mode does it find, reuse or create
// anything by the target name.
//
// The image source in req.ImageJson is served by kind, in precedence order:
//
//  1. source.proxmox.{templateID,templateName}: verify-only, unchanged by
//     ADR-0009 (an explicit reference to an existing template). The template
//     must already exist on the node: if found, success with prepared_image_id
//     set to its VMID or name; if missing, an honest NotFound. No import, no
//     task and no artifact echo (nothing was prepared or stamped). The manager
//     never sends a reference-style source to a prepare
//     (imageSourceNeedsPrepare); this path answers direct callers and older
//     managers exactly as before.
//  2. source.http.url (or the flat contracts.VMImage URL): refused with
//     InvalidSpec (urlImportUnsupportedMessage) before any PVE call, with or
//     without an identity (D10). The pre-ADR import only downloaded a file
//     named after the bare target name, never turned it into a template VM,
//     and reported another party's same-named template as "prepared".
//     ADR-0009 Slice 6 implements the import with stamped template VMs that
//     are found only by tag and stamp in the provider's own PVE pool.
//
// Anything else is InvalidSpec. The provider still advertises
// supports_image_artifact_identity (capabilities.go): the capability means the
// provider never reuses a prepared-image artifact by bare name, which holds,
// and it lets the manager deliver this honest InvalidSpec to the VMImage
// instead of holding it for a provider upgrade that would change nothing.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	parsed, err := imageartifact.ParseRequest(req)
	if err != nil {
		return nil, err
	}
	if parsed.Mode == imageartifact.ModeLegacy {
		imageartifact.SignalLegacyRequest(ctx, p.logger, proxmoxProviderType, parsed.LegacyTargetName)
	}

	src := parseProxmoxImageSource(req.GetImageJson())

	logAttrs := []any{
		"identity", parsed.Mode == imageartifact.ModeIdentity,
		"has_template_id", src.TemplateID != nil,
		"has_template_name", src.TemplateName != "",
		"has_url", src.URL != "",
	}
	if parsed.Mode == imageartifact.ModeIdentity {
		logAttrs = append(logAttrs,
			"image", parsed.Image.Namespace+"/"+parsed.Image.Name, "image_uid", parsed.Image.UID)
	} else {
		logAttrs = append(logAttrs, "target_name", parsed.LegacyTargetName)
	}
	p.logger.InfoContext(ctx, "ImagePrepare: starting", logAttrs...)

	if src.isEmpty() {
		return nil, errors.NewInvalidSpec(emptySourceMessage)
	}

	// URL import guard (ADR-0009 D10): refuse before any PVE call, in either
	// request mode. Never fall back to a lookup or a download by bare name.
	if !src.referencesExistingTemplate() {
		p.logger.WarnContext(ctx, "ImagePrepare: refusing a Proxmox URL image import, which is not supported in this release (ADR-0009 D10)",
			logAttrs...)
		return nil, errors.NewInvalidSpec(urlImportUnsupportedMessage)
	}

	// Verify-only path: an existing template is referenced; never import.
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}
	node, err := p.resolveImageNode(ctx, src.Node)
	if err != nil {
		return nil, err
	}
	return p.imagePrepareVerifyTemplate(ctx, node, src)
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
//
// It serves ONLY the reference-style source.proxmox.templateName, an explicit
// reference the image's owner wrote. It must never be used to find, reuse or
// adopt a prepared-image artifact: PVE names are not unique and a Proxmox
// artifact name is not disjoint from other names, so artifacts are found only
// by tag and stamp inside the provider's own PVE pool (ADR-0009 D5, Slice 6;
// imageartifact.NameRuleProxmox).
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

// imagePrepareDone builds an ImagePrepareResponse whose prepared_image_id is the
// referenced Proxmox template's name/VMID. prepared_image_path is left empty
// because Proxmox clones templates by name/VMID, not an on-disk path the manager
// consumes. There is no Task (verification is synchronous) and no artifact echo
// (a referenced template is not a prepared, stamped artifact).
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
