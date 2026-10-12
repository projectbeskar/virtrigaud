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

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
)

// checkLandingVirtualSize enforces ImportDiskRequest.max_virtual_size_bytes
// (maxBytes; 0 = no limit) on the vmdk an import is about to upload to the
// datastore: vmdkPath is the pod-local file the upload or the NFC lease reads
// — the converted output, or the private download — so the staging location
// (a tenant's NFS export or PVC) cannot be changed between this read and the
// landing. qemu-img reads it as vmdk (nothing is probed). A disk above the
// limit, or one whose size cannot be read while a limit applies, is refused
// (importlimit.Refusal); nothing is landed.
func checkLandingVirtualSize(ctx context.Context, vmdkPath string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	size, err := importlimit.LocalVirtualSize(ctx, vmdkPath, "vmdk")
	return importlimit.Check(size, err, maxBytes)
}

// refuseOversizedBeforeConvert is an early, cheaper refusal: the private
// download at path, read as format, is refused when it is already known to be
// above maxBytes, before it is converted (a sparse multi-TiB image would
// otherwise be converted first). It refuses only a size it read: a size it
// cannot read is left to checkLandingVirtualSize, which fails closed on the
// converted vmdk right before the landing.
func refuseOversizedBeforeConvert(ctx context.Context, path, format string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	size, err := importlimit.LocalVirtualSize(ctx, path, format)
	if err != nil {
		return nil
	}
	return importlimit.Check(size, nil, maxBytes)
}
