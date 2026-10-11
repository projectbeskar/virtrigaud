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
// has no such VM at all (confirmed absent cluster-wide). The provider's
// configured or first node (parseVMReference, FindNode) is not used: after an
// HA move PVE answers that node with "Configuration file ... does not exist",
// which says nothing about the snapshot.
func (p *Provider) snapshotVMNode(ctx context.Context, vmid int) (node string, exists bool, err error) {
	node, exists, err = p.client.ResolveVMNode(ctx, vmid)
	if err != nil {
		return "", false, fmt.Errorf("resolve the node of VM %d: %w", vmid, err)
	}
	return node, exists, nil
}
