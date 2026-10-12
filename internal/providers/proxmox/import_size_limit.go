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

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
)

// checkNodeImportSize enforces ImportDiskRequest.max_virtual_size_bytes
// (maxBytes; 0 = no limit) on the node-local stage an import produced — the
// file the Create's `qm importdisk` lands — read on the node as format
// (nothing is probed). The stage is written by the provider (the S3 download,
// or the convert from the NFS export), so it is the disk that will land,
// whatever the staging location holds by then. A disk above the limit, or one
// whose size cannot be read while a limit applies, is refused
// (importlimit.Refusal) and its stage removed: nothing is left to land.
func (p *Provider) checkNodeImportSize(ctx context.Context, stagePath, format string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	stdout, _, err := p.ssh.runSSH(ctx, fmt.Sprintf("qemu-img info -f %s --output=json -- %s",
		proxmoxShellQuote(format), proxmoxShellQuote(stagePath)))
	var size int64
	if err == nil {
		size, err = importlimit.QemuImgVirtualSize(stdout)
	}
	if refusal := importlimit.Check(size, err, maxBytes); refusal != nil {
		p.cleanupNodeFile(stagePath)
		return refusal
	}
	return nil
}
