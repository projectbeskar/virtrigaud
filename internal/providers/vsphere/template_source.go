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
	"fmt"
	"path"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Clone-source (template) resolution.
//
// A VMImage's templateName (or the name ImagePrepare prepared) used to be
// resolved with find.Finder.VirtualMachine: a datacenter-wide search that
// matched ANY VM by name — running or powered off, another tenant's included —
// and resolved a MOID-shaped argument ("vm-1234") as a managed object reference
// in any datacenter. A tenant could therefore point a VMImage at another
// tenant's (or a production) VM and full-clone it, reading its disks.
// ImagePrepare's idempotency gate likewise treated ANY VM with the target name
// as the prepared image.
//
// Now a template reference:
//
//   - is validated before any vCenter call (templateRefError);
//   - is resolved WITHOUT the finder: a bare name is compared exactly against
//     the names of the VMs in the default datacenter's VM folder tree (no MOID
//     resolution, no globbing); an inventory path (the documented alternative,
//     see VSphereImageSource.TemplateName) is resolved exactly with
//     SearchIndex.FindByInventoryPath;
//   - only ever yields an object marked as a vSphere template
//     (config.template == true). A regular VM is never a clone source and never
//     counts as a prepared image (selectTemplate).

const (
	// inventoryPathSeparator separates the elements of a vSphere inventory path.
	inventoryPathSeparator = "/"
	// templatePathReservedChars are characters a template inventory path must not
	// contain: '\' is escaped by vSphere in names (a literal one cannot match) and
	// ':' is the separator of the "Type:value" managed object reference form. A
	// '%' is allowed in a path, where vSphere uses it to escape '/', '\' and '%'
	// inside an element.
	templatePathReservedChars = `\:`
)

// templateRefError returns a codes.InvalidArgument status error when ref cannot
// safely be used to look up a vSphere template, or nil when it can. It performs
// no vCenter call, so it runs before any lookup.
//
// ref is either a bare template name, held to the same rules as a VM name
// (unsafeNameReason: not MOID-shaped, none of / \ % :), or — because
// VSphereImageSource.TemplateName documents "a simple name or a full inventory
// path" — an inventory path: absolute ("/DC/vm/folder/tmpl") or relative to the
// datacenter's VM folder ("folder/tmpl"). A path must not contain '\' or ':' or
// an empty, "." or ".." element, and its last element must not be MOID-shaped.
func templateRefError(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return status.Error(codes.InvalidArgument, "a vSphere template name is required")
	}
	if !strings.Contains(ref, inventoryPathSeparator) {
		if reason := unsafeNameReason(ref); reason != "" {
			return status.Errorf(codes.InvalidArgument,
				"template name %q cannot be used: %s; reference the template by its exact name or its full inventory path",
				ref, reason)
		}
		return nil
	}

	if strings.ContainsAny(ref, templatePathReservedChars) {
		return status.Errorf(codes.InvalidArgument,
			"template inventory path %q cannot be used: it contains one of the characters %q", ref, templatePathReservedChars)
	}
	elems := strings.Split(strings.TrimPrefix(ref, inventoryPathSeparator), inventoryPathSeparator)
	for _, e := range elems {
		if e == "" || e == "." || e == ".." {
			return status.Errorf(codes.InvalidArgument,
				"template inventory path %q cannot be used: it has an empty, \".\" or \"..\" element", ref)
		}
	}
	if vmMoIDPattern.MatchString(elems[len(elems)-1]) {
		return status.Errorf(codes.InvalidArgument,
			"template inventory path %q cannot be used: its last element has the shape of a vCenter "+
				"VirtualMachine managed object ID (\"vm-\" followed only by digits)", ref)
	}
	return nil
}

// templateCandidate is a VM (template or not) that a template reference names.
type templateCandidate struct {
	ref        types.ManagedObjectReference
	isTemplate bool
}

// selectTemplate is the whole clone-source decision for the candidates a
// template reference resolved to. It fails closed:
//
//   - exactly one candidate is a template: use it (same-named regular VMs are
//     ignored — they are never a clone source, and must not be able to make a
//     shared template name unusable);
//   - more than one template: ambiguous — codes.InvalidArgument (the reference
//     can never succeed as-is; an inventory path disambiguates);
//   - no template but at least one regular VM: codes.InvalidArgument — a regular
//     VM is never used as a clone source or treated as a prepared image;
//   - no candidate at all: found == false (the caller's not-found handling).
//
// Messages name only the requested reference, never another VM or its location.
func selectTemplate(ref string, candidates []templateCandidate) (tmpl types.ManagedObjectReference, found bool, err error) {
	var templates []types.ManagedObjectReference
	for _, c := range candidates {
		if c.isTemplate {
			templates = append(templates, c.ref)
		}
	}
	switch {
	case len(templates) == 1:
		return templates[0], true, nil
	case len(templates) > 1:
		return types.ManagedObjectReference{}, false, status.Errorf(codes.InvalidArgument,
			"vSphere template name %q is ambiguous: more than one template has that name; "+
				"reference the template by its full inventory path", ref)
	case len(candidates) > 0:
		return types.ManagedObjectReference{}, false, status.Errorf(codes.InvalidArgument,
			"%q does not name a vSphere template: only VMs marked as templates are used as clone sources "+
				"or treated as prepared images; reference an existing template, or choose a different name", ref)
	default:
		return types.ManagedObjectReference{}, false, nil
	}
}

// templateCandidates returns every VM (template or not) that ref names in the
// default datacenter, without using find.Finder:
//
//   - a bare name: every VirtualMachine in the datacenter's VM folder tree
//     (including nested folders and vApps) whose name is exactly ref;
//   - an inventory path: the VirtualMachine FindByInventoryPath returns for it,
//     absolute or relative to the datacenter's VM folder.
//
// An absolute path must lie under the default datacenter's VM folder (the API
// scopes templateName to the Provider's datacenter; FindByInventoryPath alone
// would reach any datacenter the service account can see): anything else is a
// codes.InvalidArgument status error.
//
// Each candidate's config.template is read individually. ref must already have
// passed templateRefError and the finder must be scoped to the datacenter. Any
// other returned error is a vCenter failure (retryable), never "not found"; it
// may carry other objects' identifiers, so lookupTemplate logs it and returns a
// generic error instead (templateLookupFailedError).
func (p *Provider) templateCandidates(ctx context.Context, ref string) ([]templateCandidate, error) {
	vmFolder, err := p.finder.DefaultFolder(ctx)
	if err != nil {
		return nil, fmt.Errorf("find datacenter VM folder: %w", err)
	}

	var refs []types.ManagedObjectReference
	if strings.Contains(ref, inventoryPathSeparator) {
		inventoryPath := ref
		if strings.HasPrefix(ref, inventoryPathSeparator) {
			if err := absoluteTemplatePathError(ref, vmFolder.InventoryPath); err != nil {
				return nil, err
			}
		} else {
			inventoryPath = path.Join(vmFolder.InventoryPath, ref)
		}
		obj, err := object.NewSearchIndex(p.client.Client).FindByInventoryPath(ctx, inventoryPath)
		if err != nil {
			return nil, fmt.Errorf("look up template inventory path %q: %w", ref, err)
		}
		if obj != nil && obj.Reference().Type == virtualMachineMoType {
			refs = append(refs, obj.Reference())
		}
	} else {
		v, err := view.NewManager(p.client.Client).CreateContainerView(ctx, vmFolder.Reference(), []string{virtualMachineMoType}, true)
		if err != nil {
			return nil, fmt.Errorf("create VM container view: %w", err)
		}
		defer func() { _ = v.Destroy(ctx) }()

		var vms []mo.VirtualMachine
		if err := v.Retrieve(ctx, []string{virtualMachineMoType}, []string{propName}, &vms); err != nil {
			return nil, fmt.Errorf("list VM names: %w", err)
		}
		for _, vm := range vms {
			if vm.Name == ref {
				refs = append(refs, vm.Self)
			}
		}
	}

	candidates := make([]templateCandidate, 0, len(refs))
	pc := property.DefaultCollector(p.client.Client)
	for _, r := range refs {
		var vm mo.VirtualMachine
		if err := pc.RetrieveOne(ctx, r, []string{propConfigTemplate}, &vm); err != nil {
			return nil, fmt.Errorf("read config.template of %s: %w", r.Value, err)
		}
		// An inaccessible VM has no config: it is not a usable template.
		candidates = append(candidates, templateCandidate{ref: r, isTemplate: vm.Config != nil && vm.Config.Template})
	}
	return candidates, nil
}

// absoluteTemplatePathError returns a codes.InvalidArgument status error unless
// the absolute inventory path ref lies strictly under vmFolderPath (the default
// datacenter's VM folder, e.g. "/DC0/vm"). The comparison is exact
// (case-sensitive), so any variant fails closed. An unknown or relative
// vmFolderPath cannot vouch for anything and is refused as well.
func absoluteTemplatePathError(ref, vmFolderPath string) error {
	if !strings.HasPrefix(vmFolderPath, inventoryPathSeparator) ||
		!strings.HasPrefix(ref, strings.TrimSuffix(vmFolderPath, inventoryPathSeparator)+inventoryPathSeparator) {
		return status.Errorf(codes.InvalidArgument,
			"template inventory path %q is not under the Provider's default datacenter VM folder; "+
				"use an absolute path under that folder, or a path relative to it", ref)
	}
	return nil
}

// templateLookupFailedError is the error returned for a vCenter failure while
// resolving a template reference. It is deliberately generic: the underlying
// error can name other objects (another tenant's VM MOID, vCenter fault text)
// and would otherwise land in the requesting VirtualMachine's or VMImage's
// status. The details go to the provider log. It is not a gRPC status error, so
// the manager treats it as transient and retries.
func templateLookupFailedError(ref string) error {
	return fmt.Errorf("look up vSphere template %q: a vCenter error occurred (details in the provider log); will retry", ref)
}

// lookupTemplate validates ref and resolves it to a vSphere template (see
// templateCandidates and selectTemplate). It returns found == false, with a nil
// error, only when no VM at all matches ref. A codes.InvalidArgument status
// error means the reference is unsafe, outside the default datacenter,
// ambiguous, or names a regular VM; any other error is a generic, retryable
// vCenter failure (templateLookupFailedError).
func (p *Provider) lookupTemplate(ctx context.Context, ref string) (*object.VirtualMachine, bool, error) {
	if err := templateRefError(ref); err != nil {
		return nil, false, err
	}
	candidates, err := p.templateCandidates(ctx, ref)
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.InvalidArgument {
			p.logger.Warn("Refusing template reference", "template", ref, "error", err)
			return nil, false, err
		}
		p.logger.Error("vCenter lookup of template reference failed", "template", ref, "error", err)
		return nil, false, templateLookupFailedError(ref)
	}
	tmpl, found, err := selectTemplate(ref, candidates)
	if err != nil || !found {
		if err != nil {
			p.logger.Warn("Refusing template reference", "template", ref, "candidates", len(candidates), "error", err)
		}
		return nil, false, err
	}
	vm := object.NewVirtualMachine(p.client.Client, tmpl)
	return vm, true, nil
}
