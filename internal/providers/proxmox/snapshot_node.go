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

package proxmox

import (
	"context"
	"fmt"
)

// snapshotVMNode resolves the node VM vmid is actually on, from the cluster's
// resource list, for the snapshot path. exists is false only when the cluster
// has no such VM at all. The provider's configured or first node
// (parseVMReference, FindNode) is not used: after an HA move PVE answers that
// node with "Configuration file ... does not exist", which says nothing about
// the snapshot.
//
// PVE filters /cluster/resources by the caller's VM.Audit privilege, so a VM
// missing from it may only be invisible to this token (a privilege-separated
// or pool-scoped token, an ACL change). Absence is therefore confirmed with
// the permission-independent VMID probe (VMIDFree): only a free VMID is
// absent. A VMID in use but not listed is an error (the manager retries) and
// is logged as a likely ACL problem — never a snapshot "gone".
func (p *Provider) snapshotVMNode(ctx context.Context, vmid int) (node string, exists bool, err error) {
	node, exists, err = p.client.ResolveVMNode(ctx, vmid)
	if err != nil {
		return "", false, fmt.Errorf("resolve the node of VM %d: %w", vmid, err)
	}
	if exists {
		return node, true, nil
	}
	free, err := p.client.VMIDFree(ctx, vmid)
	if err != nil {
		return "", false, fmt.Errorf("confirm that VM %d is absent: %w", vmid, err)
	}
	if !free {
		p.logger.Warn("VM exists in the cluster but is not visible in /cluster/resources to this token; "+
			"check the token's VM.Audit privilege (ACL) — its snapshot is not treated as gone", "vmid", vmid)
		return "", false, fmt.Errorf("VM %d exists but is not visible to this token (VM.Audit)", vmid)
	}
	return "", false, nil
}
