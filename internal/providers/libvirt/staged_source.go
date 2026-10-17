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
	stderrors "errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/stagedfile"
)

// stagedPVCMountRoot is where the provider pod mounts the migration PVCs
// (storage.PVCMountRoot); a variable so tests can stage beneath a temp dir.
var stagedPVCMountRoot = storage.PVCMountRoot

// Source URL schemes of a pod-side (pvc) ImportDisk.
const (
	pvcURLScheme  = "pvc://"
	fileURLScheme = "file://"
)

// Constant messages of a staged-disk open failure. They name no path: the
// staging PVC is writable by the tenant, so a refusal may be caused by a link
// the tenant planted, and the message reaches the VMMigration's status. The
// provider log carries the detail.
const (
	stagedDiskRefusedMessage    = "refused the staged disk: symbolic links, hard links and special files are not imported"
	stagedDiskNotFoundMessage   = "the staged disk was not found"
	stagedDiskOpenFailedMessage = "the staged disk could not be opened"
	stagedDiskBadURLMessage     = "unsupported source URL (expected pvc://<pvc-name>/<file-path> or file:///<absolute-path>)"
)

// openStagedImportSource opens the staged disk a pod-side ImportDisk names,
// once, for every later read (the size check, the copy to the host): the file
// cannot be swapped between them.
//
//   - pvc://<pvc-name>/<path>: the file is resolved beneath the PVC's mount
//     (<storage.PVCMountRoot>/<pvc-name>) with no symbolic link in any
//     component (stagedfile). The tenant can write that PVC; a planted link
//     would otherwise import the provider pod's credentials, TLS key or
//     ServiceAccount token into a disk the tenant owns.
//   - file:///<absolute-path>: (never sent by the manager; tests and manual
//     use) the path is resolved from "/" with the same rules.
//
// Only a regular file with a single link is opened. It returns the open file
// and its path (for logs and the default volume name); errors are gRPC
// statuses with constant messages.
func openStagedImportSource(sourceURL string) (*os.File, string, error) {
	var root, rel string
	switch {
	case strings.HasPrefix(sourceURL, pvcURLScheme):
		pvcName, r, err := storage.SplitPVCURL(sourceURL)
		if err != nil {
			log.Printf("WARN Refused an import source URL: %v", err)
			return nil, "", status.Error(codes.InvalidArgument, stagedDiskBadURLMessage)
		}
		root, rel = filepath.Join(stagedPVCMountRoot, pvcName), r
	case strings.HasPrefix(sourceURL, fileURLScheme):
		p := strings.TrimPrefix(sourceURL, fileURLScheme)
		if !filepath.IsAbs(p) {
			return nil, "", status.Error(codes.InvalidArgument, stagedDiskBadURLMessage)
		}
		root, rel = "/", strings.TrimPrefix(filepath.Clean(p), "/")
	default:
		return nil, "", status.Error(codes.InvalidArgument, stagedDiskBadURLMessage)
	}

	f, err := stagedfile.OpenRead(root, rel)
	if err != nil {
		log.Printf("WARN Could not open the staged disk for import: %s", stagedfile.LogDetail(err))
		switch {
		case stagedfile.IsUnsafe(err):
			return nil, "", status.Error(codes.FailedPrecondition, stagedDiskRefusedMessage)
		case stderrors.Is(err, fs.ErrNotExist):
			return nil, "", status.Error(codes.NotFound, stagedDiskNotFoundMessage)
		default:
			return nil, "", status.Error(codes.Internal, stagedDiskOpenFailedMessage)
		}
	}
	return f, filepath.Join(root, rel), nil
}
