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
	"strings"

	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/status"
)

// What ListVMs lists (the VMAdoption discovery feed).
//
// ListVMs is the manager's only source of VMs to adopt: every VMInfo it returns
// can become a tenant-owned VirtualMachine, whose delete destroys the vSphere
// object and whose reconfigure changes it. It therefore lists ordinary VMs
// only, and leaves out (fail closed):
//
//   - vSphere templates (config.template): a template is shared image state
//     that VMs are cloned from, never a VM. This covers every prepared image
//     (ADR-0009 Slice 3 marks each one as a template) and every template an
//     operator made by hand or that an earlier release prepared without a
//     stamp.
//   - any object carrying a VirtRigaud prepared-image stamp key
//     (virtrigaud.image.*), valid or not, template or not: an image import in
//     flight is a plain VM until it is marked as a template, and an import that
//     failed before that step leaves one behind. Create and Clone clear these
//     keys on every VM they make, so an ordinary VM never carries them.
//   - any VM whose owner stamp (virtrigaud.owner.*) cannot be read (a repeated
//     key or a non-string value; VirtRigaud writes neither): who owns it cannot
//     be told, so it is not offered to anyone.
//   - any VM whose configuration could not be read: none of the above can be
//     ruled out.
//
// A VM with a readable owner stamp IS listed, with the stamped UID reported in
// ProviderRaw[contracts.VMInfoOwnerUIDKey]. The adoption controller then skips
// it while the VirtualMachine that UID names still exists (in any namespace,
// under any Provider), and may adopt it once that VirtualMachine is gone.
//
// listExclusionFor is the provider-side half of keeping image state out of
// adoption, and the primary control. The manager-side half is
// contracts.VMInfoArtifactKey: adoption skips any listed VM carrying it, as a
// backstop for third-party and SDK providers that list templates. This
// provider deliberately keeps EXCLUDING templates and image artifacts rather
// than returning them tagged with that key: providers can be rolled before the
// manager, and a manager older than the key would adopt a tagged template.

// listExclusion names why ListVMs leaves a vSphere object out of the listing,
// or is "" when the object is an ordinary VM that is listed. It is logged.
type listExclusion string

const (
	// listedVM: an ordinary VM, listed.
	listedVM listExclusion = ""
	// excludedNoConfig: the object's configuration could not be read.
	excludedNoConfig listExclusion = "configuration unavailable"
	// excludedTemplate: the object is a vSphere template.
	excludedTemplate listExclusion = "vSphere template"
	// excludedImageStamp: the object carries a prepared-image stamp key.
	excludedImageStamp listExclusion = "carries a VirtRigaud prepared-image stamp"
	// excludedUnreadableOwnerStamp: the VM's owner stamp cannot be read.
	excludedUnreadableOwnerStamp listExclusion = "owner stamp cannot be read"
)

// listExclusionFor decides whether ListVMs lists the object whose config is
// cfg (see the comment at the top of this file).
func listExclusionFor(cfg *types.VirtualMachineConfigInfo) listExclusion {
	if cfg == nil {
		return excludedNoConfig
	}
	if cfg.Template {
		return excludedTemplate
	}
	if carriesImageStampKey(cfg.ExtraConfig) {
		return excludedImageStamp
	}
	if _, err := ownerFromExtraConfig(cfg.ExtraConfig); err != nil {
		return excludedUnreadableOwnerStamp
	}
	return listedVM
}

// listCancelledError is ListVMs' answer once ctx is cancelled or past its
// deadline (codes.Canceled or codes.DeadlineExceeded): the listing stops at
// once, without a warning per remaining VM, and a partial listing is never
// answered as the whole datacenter.
func listCancelledError(ctx context.Context) error {
	return status.FromContextError(ctx.Err()).Err()
}

// carriesImageStampKey reports whether extraConfig holds any key in the
// prepared-image stamp namespace (virtrigaud.image.*, compared
// case-insensitively as VMX keys are) whose value is not the empty string.
// Unlike imageStampFromExtraConfig it does not validate the stamp: any trace
// of one counts, including a key VirtRigaud does not write and a non-string
// value. An empty value is a cleared key (vSphere removes a key set to "").
func carriesImageStampKey(extraConfig []types.BaseOptionValue) bool {
	for _, bov := range extraConfig {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil || !strings.HasPrefix(strings.ToLower(ov.Key), imageStampKeyPrefix) {
			continue
		}
		if s, ok := ov.Value.(string); ok && s == "" {
			continue
		}
		return true
	}
	return false
}
