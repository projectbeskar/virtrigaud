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

package migration

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrStagedImageRejected marks a staged migration image whose header is not a
// self-contained image of the expected format (CheckStagedImageHeader). It is
// permanent: retrying reads the same header.
var ErrStagedImageRejected = errors.New("staged image rejected")

// stagedImageInfo is the part of `qemu-img info --output=json` the staged
// image guard reads.
type stagedImageInfo struct {
	Format              string `json:"format"`
	BackingFilename     string `json:"backing-filename"`
	FullBackingFilename string `json:"full-backing-filename"`
	FormatSpecific      *struct {
		Data struct {
			DataFile string `json:"data-file"`
			Extents  []struct {
				Filename string `json:"filename"`
			} `json:"extents"`
		} `json:"data"`
	} `json:"format-specific"`
}

// CheckStagedImageHeader checks the `qemu-img info --output=json -f
// <wantFormat>` output of a staged migration image before it is converted or
// imported. The staged object comes from a backend the tenant controls (its
// NFS server or bucket), so its header is untrusted: a qcow2 backing file, an
// external data file or a VMDK extent can name ANY path or protocol the
// converting process can open — a node or pod file (/etc/pve/priv/*,
// /root/.ssh/*, a service-account token, a block device) or a json:/nbd:/http:
// target that bypasses the SSRF host policy — and qemu-img convert would
// flatten its contents into the tenant's imported disk. The image must
// therefore be exactly wantFormat and reference nothing outside itself (self is
// the path or URL qemu-img was given: a VMDK extent may only name it). The
// error wraps ErrStagedImageRejected and names no path.
func CheckStagedImageHeader(infoJSON, wantFormat, self string) error {
	var info stagedImageInfo
	if err := json.Unmarshal([]byte(infoJSON), &info); err != nil {
		return fmt.Errorf("%w: qemu-img info output is not JSON", ErrStagedImageRejected)
	}
	if info.Format != wantFormat {
		return fmt.Errorf("%w: the image is %q, not %q", ErrStagedImageRejected, info.Format, wantFormat)
	}
	if info.BackingFilename != "" || info.FullBackingFilename != "" {
		return fmt.Errorf("%w: the image has a backing file; a staged image must be self-contained", ErrStagedImageRejected)
	}
	if fs := info.FormatSpecific; fs != nil {
		if fs.Data.DataFile != "" {
			return fmt.Errorf("%w: the image uses an external data file; a staged image must be self-contained", ErrStagedImageRejected)
		}
		for _, e := range fs.Data.Extents {
			if e.Filename != "" && e.Filename != self {
				return fmt.Errorf("%w: the image references extent files outside itself; a staged image must be self-contained",
					ErrStagedImageRejected)
			}
		}
	}
	return nil
}
