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
	"strings"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
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

// errArtifactNameTaken is returned (wrapped) by importOVA when vCenter refuses
// the import because an object with the entity name already exists in the
// target folder (DuplicateName). An identity prepare then re-runs the ADR-0009
// D4 probe instead of failing: vCenter's per-folder name uniqueness is the
// atomic create-if-absent of ADR-0009 D6.
var errArtifactNameTaken = stderrors.New("an object with the artifact name already exists in the import folder")

// isDuplicateNameFault reports whether err carries a vSphere DuplicateName
// fault, whether it came synchronously from ImportVApp (a SOAP fault) or
// through the HttpNfcLease's error (a task error).
func isDuplicateNameFault(err error) bool {
	return err != nil && fault.Is(err, &types.DuplicateName{})
}

// ovfDescriptorProperty is the InvalidArgument.invalidProperty vCenter names
// when CreateImportSpec cannot parse the OVF descriptor.
const ovfDescriptorProperty = "ovfDescriptor"

// isOVFContentFault reports whether err, returned by CreateImportSpec, means
// vCenter could not use the OVF descriptor itself — an OVF fault, or an
// InvalidArgument naming the descriptor — which is a permanent property of the
// source, not a transient vCenter failure. Any other fault (a session, a
// transport or an inventory problem) is not.
func isOVFContentFault(err error) bool {
	if err == nil {
		return false
	}
	content := false
	fault.In(err, func(f types.BaseMethodFault, _ string, _ []types.LocalizableMessage) bool {
		switch f := f.(type) {
		case types.BaseOvfFault:
			content = true
		case *types.InvalidArgument:
			content = strings.EqualFold(f.InvalidProperty, ovfDescriptorProperty)
		}
		return content
	})
	return content
}

// importOVA imports the OVF at fpath the way govmomi's importer.Importer.Import
// (v0.52.0) does — CreateImportSpec, ImportVApp, NFC upload, lease completion —
// with these additions:
//
//   - stripReservedExtraConfig runs on the import spec BEFORE ImportVApp, so the
//     created entity never carries a reserved key it did not get from
//     VirtRigaud, not even for the duration of the upload. The removal is
//     logged provider-side.
//   - Identity mode (stamp != nil, ADR-0009): the image stamp is appended to
//     the import spec AFTER the stripping, so the entity carries the real
//     stamp from the moment it exists and an OVF can never supply one. An OVF
//     that is not exactly one virtual machine (a VirtualSystemCollection, or
//     an import spec that is not a VirtualMachineImportSpec) is refused as
//     InvalidSpec: an artifact is a single template. A source that can never
//     import (an unreadable or invalid OVF, one vCenter's OVF parser rejects)
//     is InvalidSpec, a DuplicateName is errArtifactNameTaken, and anything
//     else is returned as a (retryable) vCenter error.
//
// It returns the created entity whenever it is known — also with an error
// after the lease became ready (the upload or the lease completion failed) —
// so an identity prepare can destroy its own partial object. Legacy mode
// (stamp == nil) behaves exactly as before ADR-0009; its caller ignores the
// entity on error.
//
// It honours the Importer/Options fields ImagePrepare uses (Name,
// DiskProvisioning, and the OVF create-import-spec parameters); it does not
// implement Importer.Hidden, VerifyManifest or Options.Annotation, which
// ImagePrepare never sets.
func (p *Provider) importOVA(ctx context.Context, imp *importer.Importer, fpath string, opts importer.Options, stamp *imageartifact.Stamp) (*types.ManagedObjectReference, error) {
	identity := stamp != nil

	descriptor, err := importer.ReadOvf(fpath, imp.Archive)
	if err != nil {
		if identity {
			return nil, errors.NewInvalidSpec("ImagePrepare: the OVF descriptor cannot be read from the downloaded source: %v", err)
		}
		return nil, fmt.Errorf("read OVF descriptor: %w", err)
	}
	envelope, err := importer.ReadEnvelope(descriptor)
	if err != nil {
		if identity {
			return nil, errors.NewInvalidSpec("ImagePrepare: the source is not a valid OVF descriptor: %v", err)
		}
		return nil, fmt.Errorf("parse OVF descriptor: %w", err)
	}
	if identity && (envelope.VirtualSystemCollection != nil || envelope.VirtualSystem == nil) {
		return nil, multiVMOVFError()
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
		if identity && isOVFContentFault(err) {
			return nil, errors.NewInvalidSpec("ImagePrepare: vCenter cannot import the OVF descriptor: %v", err)
		}
		return nil, fmt.Errorf("create OVF import spec: %w", err)
	}
	if len(spec.Error) > 0 {
		if identity {
			return nil, errors.NewInvalidSpec("ImagePrepare: vCenter rejected the OVF: %s", spec.Error[0].LocalizedMessage)
		}
		return nil, &task.Error{LocalizedMethodFault: &spec.Error[0]}
	}
	for _, w := range spec.Warning {
		_, _ = imp.Log(fmt.Sprintf("Warning: %s\n", w.LocalizedMessage))
	}

	if removed := stripReservedExtraConfig(spec.ImportSpec); len(removed) > 0 {
		p.logger.Warn("ImagePrepare: removed VirtRigaud-reserved ExtraConfig keys carried by the OVF; they are never imported",
			"entity", name, "keys", removed)
	}
	if identity {
		vmSpec, ok := spec.ImportSpec.(*types.VirtualMachineImportSpec)
		if !ok {
			return nil, multiVMOVFError()
		}
		// After the stripping: the only virtrigaud.image.* keys the entity can
		// ever carry are the ones written here.
		vmSpec.ConfigSpec.ExtraConfig = append(vmSpec.ConfigSpec.ExtraConfig, imageStampExtraConfig(*stamp)...)
	}

	lease, err := imp.ResourcePool.ImportVApp(ctx, spec.ImportSpec, imp.Folder, imp.Host)
	if err != nil {
		if identity && isDuplicateNameFault(err) {
			return nil, fmt.Errorf("ImportVApp %q: %w", name, errArtifactNameTaken)
		}
		return nil, fmt.Errorf("ImportVApp: %w", err)
	}
	info, err := lease.Wait(ctx, spec.FileItem)
	if err != nil {
		_ = lease.Abort(ctx, nil)
		if identity && isDuplicateNameFault(err) {
			return nil, fmt.Errorf("NFC lease for %q: %w", name, errArtifactNameTaken)
		}
		return nil, fmt.Errorf("wait for NFC lease: %w", err)
	}

	updater := lease.StartUpdater(ctx, info)
	defer updater.Done()

	entity := info.Entity
	for _, item := range info.Items {
		if err := imp.Upload(ctx, lease, item); err != nil {
			_ = lease.Abort(ctx, &types.LocalizedMethodFault{Fault: &types.FileFault{File: item.Path}})
			return &entity, fmt.Errorf("upload %s: %w", item.Path, err)
		}
	}
	if err := lease.Complete(ctx); err != nil {
		return &entity, fmt.Errorf("complete NFC lease: %w", err)
	}
	return &entity, nil
}

// multiVMOVFError is the InvalidSpec for an OVF that is not exactly one
// virtual machine (ADR-0009 D5): a prepared-image artifact is one template.
func multiVMOVFError() error {
	return errors.NewInvalidSpec("ImagePrepare: the OVF describes more than one virtual machine (a vApp / " +
		"VirtualSystemCollection); a prepared image must be exactly one virtual machine")
}
