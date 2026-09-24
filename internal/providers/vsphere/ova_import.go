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
	"strings"

	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/types"
)

// reservedExtraConfigPrefix is the ExtraConfig key namespace VirtRigaud owns
// (the owner stamp, ownerExtraConfigKey*). Only the provider may write keys in
// it; content supplied by a tenant — an OVF/OVA's <vmw:ExtraConfig> entries — is
// stripped of them before it reaches vCenter.
const reservedExtraConfigPrefix = "virtrigaud."

// defaultOVFEntityName is the name govmomi's importer gives an imported entity
// when neither the options nor the OVF name it. ImagePrepare always sets one.
const defaultOVFEntityName = "Govc Virtual Appliance"

// isReservedExtraConfigKey reports whether key lies in VirtRigaud's reserved
// ExtraConfig namespace. VMX keys are case-insensitive, so the match is too.
func isReservedExtraConfigKey(key string) bool {
	return strings.HasPrefix(strings.ToLower(key), reservedExtraConfigPrefix)
}

// stripReservedExtraConfig removes every reserved ExtraConfig entry from an OVF
// import spec tree — a VirtualMachineImportSpec, or every VM under a
// VirtualAppImportSpec, recursively — and returns the removed keys.
//
// Why: an OVF can carry arbitrary <vmw:ExtraConfig> entries, which vCenter maps
// into the import spec. A tenant's OVA could set virtrigaud.owner.uid to another
// VirtualMachine's UID; while it is being imported (and if MarkAsTemplate or
// the cleanup fails) the object is a regular VM named after the VMImage, which a
// same-named create in that folder would then accept as its own.
func stripReservedExtraConfig(spec types.BaseImportSpec) []string {
	var removed []string
	switch s := spec.(type) {
	case *types.VirtualMachineImportSpec:
		kept := s.ConfigSpec.ExtraConfig[:0]
		for _, bov := range s.ConfigSpec.ExtraConfig {
			if bov == nil {
				continue
			}
			if o := bov.GetOptionValue(); o != nil && isReservedExtraConfigKey(o.Key) {
				removed = append(removed, o.Key)
				continue
			}
			kept = append(kept, bov)
		}
		s.ConfigSpec.ExtraConfig = kept
	case *types.VirtualAppImportSpec:
		for _, child := range s.Child {
			removed = append(removed, stripReservedExtraConfig(child)...)
		}
	}
	return removed
}

// importOVA imports the OVF at fpath the way govmomi's importer.Importer.Import
// (v0.52.0) does — CreateImportSpec, ImportVApp, NFC upload, lease completion —
// with one addition: stripReservedExtraConfig runs on the import spec BEFORE
// ImportVApp, so the created entity never carries a reserved key, not even for
// the duration of the upload. The removal is logged provider-side.
//
// It honours the Importer/Options fields ImagePrepare uses (Name,
// DiskProvisioning, and the OVF create-import-spec parameters); it does not
// implement Importer.Hidden, VerifyManifest or Options.Annotation, which
// ImagePrepare never sets.
func (p *Provider) importOVA(ctx context.Context, imp *importer.Importer, fpath string, opts importer.Options) (*types.ManagedObjectReference, error) {
	descriptor, err := importer.ReadOvf(fpath, imp.Archive)
	if err != nil {
		return nil, fmt.Errorf("read OVF descriptor: %w", err)
	}
	envelope, err := importer.ReadEnvelope(descriptor)
	if err != nil {
		return nil, fmt.Errorf("parse OVF descriptor: %w", err)
	}

	name := defaultOVFEntityName
	switch {
	case opts.Name != nil:
		name = *opts.Name
	case envelope.VirtualSystem != nil && envelope.VirtualSystem.Name != nil:
		name = *envelope.VirtualSystem.Name
	case envelope.VirtualSystem != nil:
		name = envelope.VirtualSystem.ID
	}

	networkMapping, err := imp.NetworkMap(ctx, envelope, opts.NetworkMapping)
	if err != nil {
		return nil, fmt.Errorf("map OVF networks: %w", err)
	}

	params := types.OvfCreateImportSpecParams{
		DiskProvisioning:   opts.DiskProvisioning,
		EntityName:         name,
		IpAllocationPolicy: opts.IPAllocationPolicy,
		IpProtocol:         opts.IPProtocol,
		OvfManagerCommonParams: types.OvfManagerCommonParams{
			DeploymentOption: opts.Deployment,
			Locale:           "US",
		},
		PropertyMapping: importer.OVFMap(opts.PropertyMapping),
		NetworkMapping:  networkMapping,
	}
	spec, err := ovf.NewManager(imp.Client).CreateImportSpec(ctx, string(descriptor), imp.ResourcePool, imp.Datastore, &params)
	if err != nil {
		return nil, fmt.Errorf("create OVF import spec: %w", err)
	}
	if len(spec.Error) > 0 {
		return nil, &task.Error{LocalizedMethodFault: &spec.Error[0]}
	}
	for _, w := range spec.Warning {
		_, _ = imp.Log(fmt.Sprintf("Warning: %s\n", w.LocalizedMessage))
	}

	if removed := stripReservedExtraConfig(spec.ImportSpec); len(removed) > 0 {
		p.logger.Warn("ImagePrepare: removed VirtRigaud-reserved ExtraConfig keys carried by the OVF; they are never imported",
			"entity", name, "keys", removed)
	}

	lease, err := imp.ResourcePool.ImportVApp(ctx, spec.ImportSpec, imp.Folder, imp.Host)
	if err != nil {
		return nil, fmt.Errorf("ImportVApp: %w", err)
	}
	info, err := lease.Wait(ctx, spec.FileItem)
	if err != nil {
		_ = lease.Abort(ctx, nil)
		return nil, fmt.Errorf("wait for NFC lease: %w", err)
	}

	updater := lease.StartUpdater(ctx, info)
	defer updater.Done()

	for _, item := range info.Items {
		if err := imp.Upload(ctx, lease, item); err != nil {
			_ = lease.Abort(ctx, &types.LocalizedMethodFault{Fault: &types.FileFault{File: item.Path}})
			return nil, fmt.Errorf("upload %s: %w", item.Path, err)
		}
	}
	if err := lease.Complete(ctx); err != nil {
		return nil, fmt.Errorf("complete NFC lease: %w", err)
	}
	return &info.Entity, nil
}
