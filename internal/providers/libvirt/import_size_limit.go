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

package libvirt

import (
	"context"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
)

// checkHostImportSize enforces ImportDiskRequest.max_virtual_size_bytes
// (maxBytes; 0 = no limit) on the disk an import is about to land, at path on
// the libvirt host, read as format (nothing is probed) — the private staged
// copy or the converted output, never the tenant-reachable staging location
// itself, so it cannot change between this read and the landing. A disk above
// the limit, or one whose size cannot be read while a limit applies, is
// refused (importlimit.Refusal: FailedPrecondition with its ErrorInfo
// reason); the caller lands nothing.
func checkHostImportSize(ctx context.Context, h hostCommandRunner, path, format string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	res, err := runHost(ctx, h, "qemu-img", "info", "-f", format, "--output=json", "--", path)
	if err != nil {
		return importlimit.Check(0, err, maxBytes)
	}
	size, err := importlimit.QemuImgVirtualSize(res.Stdout)
	return importlimit.Check(size, err, maxBytes)
}
