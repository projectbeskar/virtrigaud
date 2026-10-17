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
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/providers/importlimit"
)

// privateLocalImportCopy copies the staged disk in (in the tenant's staging
// PVC, already opened by openStagedImportSource with no symbolic link
// followed in any component, a regular single-link file) into a fresh private
// directory (0700) as a 0600 file, so the disk-size check and the import read
// the same bytes: the tenant can no longer replace them in between. The
// caller keeps ownership of in. cleanup removes the copy and its directory.
func privateLocalImportCopy(in *os.File) (path string, cleanup func(), err error) {
	info, err := in.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stat staged disk: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("the staged disk is not a regular file")
	}
	dir, err := os.MkdirTemp("", "virtrigaud-import-")
	if err != nil {
		return "", nil, fmt.Errorf("create private import directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "staged-disk")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create private import copy: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		cleanup()
		return "", nil, fmt.Errorf("copy staged disk: %w", err)
	}
	if err := out.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("flush private import copy: %w", err)
	}
	return path, cleanup, nil
}

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
