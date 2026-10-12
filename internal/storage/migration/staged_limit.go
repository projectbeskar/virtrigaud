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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// EnvStagedImportMaxGiB names the provider environment variable that bounds
// how many GiB an import copies from a staging backend (the staged object's
// file size), default DefaultStagedImportMaxGiB. The tenant's NFS server
// controls that size: without a bound, one object could fill the node's root
// filesystem, the libvirt pool or the vSphere pod's /tmp (eviction).
const EnvStagedImportMaxGiB = "VIRTRIGAUD_MIGRATION_IMPORT_MAX_GIB"

// EnvTmpSizeLimit names the provider pod environment variable that carries
// the effective sizeLimit of the pod's /tmp emptyDir (a Kubernetes quantity,
// e.g. "512Gi"), set by the manager-rendered Deployment and the chart. A
// provider that stages into /tmp bounds its copies by it: statfs reports the
// node's free space, not the emptyDir's limit, and a pod that writes past
// the limit is evicted (an outage for every tenant). Unset: no limit.
const EnvTmpSizeLimit = "VIRTRIGAUD_TMP_SIZE_LIMIT"

// DefaultStagedImportMaxGiB is the copy bound when EnvStagedImportMaxGiB is
// unset.
const DefaultStagedImportMaxGiB = 1024

// StagedCopySpaceFactor is how many times the staged object's size must be
// free where it is copied: the copy itself and the converted disk made from
// it. The converted disk holds the image's allocated data, which for a
// COMPRESSED staged image (compressed qcow2 clusters, a streamOptimized VMDK)
// can exceed the file size, up to the declared virtual size. The check is
// deliberately not made against the virtual size, which would refuse every
// sparse disk larger than the free space; the bound on that growth is the
// target Provider's disk-size limit (#382, max_virtual_size_bytes), which
// operators of shared hosts should set (ADR-0011 D10).
const StagedCopySpaceFactor = 2

// gib is one GiB in bytes.
const gib = int64(1) << 30

// Constant refusals of a staged copy; they name no location.
var (
	// ErrStagedObjectTooLarge: the staged object is larger than the bound.
	// Permanent for that object.
	ErrStagedObjectTooLarge = errors.New("the staged object is larger than the import size limit")
	// ErrStagingSpaceExhausted: not enough free space where the object would
	// be copied. May clear up; retryable.
	ErrStagingSpaceExhausted = errors.New("not enough free space to copy the staged object")
)

// stagedImageMetadataSlack is the fixed part of the room StagedCopyLimit
// leaves above a request's disk-size limit for an image's own metadata.
const stagedImageMetadataSlack = int64(16) << 20

// StagedCopyLimit is the most bytes an import copies from a staging backend,
// and the largest virtual size a staged image header may declare: the
// provider bound (EnvStagedImportMaxGiB, default DefaultStagedImportMaxGiB),
// lowered by requestBytes when that is set (> 0).
//
// requestBytes is ImportDiskRequest.max_virtual_size_bytes (#382), a limit on
// the disk's VIRTUAL size, while the copy is bounded by the staged FILE size,
// which for a fully allocated qcow2 or vmdk exceeds the virtual size by its
// metadata (L1/L2 tables, refcounts, grain tables: well under 1/64). The
// request bound is therefore requestBytes + requestBytes/64 +
// stagedImageMetadataSlack, so a disk exactly at the limit is not refused
// here; the exact disk-size check runs on the converted disk (importlimit).
func StagedCopyLimit(requestBytes int64) (int64, error) {
	limit := int64(DefaultStagedImportMaxGiB) * gib
	if raw := strings.TrimSpace(os.Getenv(EnvStagedImportMaxGiB)); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || n > 1<<20 {
			return 0, fmt.Errorf("%s=%q is not a positive number of GiB", EnvStagedImportMaxGiB, raw)
		}
		limit = n * gib
	}
	if requestBytes > 0 && requestBytes < limit {
		// requestBytes < limit <= 2^50: the sum cannot overflow.
		if bound := requestBytes + requestBytes/64 + stagedImageMetadataSlack; bound < limit {
			limit = bound
		}
	}
	return limit, nil
}

// CheckStagedCopy decides, before a staged object of size bytes is copied
// where freeBytes are available, whether it may be: ErrStagedObjectTooLarge
// above limit, ErrStagingSpaceExhausted when less than StagedCopySpaceFactor
// times its size is free, nil otherwise.
func CheckStagedCopy(size, limit, freeBytes int64) error {
	if size < 0 || size > limit {
		return ErrStagedObjectTooLarge
	}
	if freeBytes/StagedCopySpaceFactor < size {
		return ErrStagingSpaceExhausted
	}
	return nil
}

// BoundedWriter writes to W and fails with ErrStagedObjectTooLarge — writing
// nothing more — once more than Max bytes would have been written. It bounds a
// staged download (an S3 object) by the size measured before it (min of the
// object's reported size and the import limit): a backend that reports one
// size and serves more is refused instead of filling the disk.
type BoundedWriter struct {
	// W receives the bytes.
	W io.Writer
	// Max is the most bytes W may receive.
	Max int64
	// n counts the bytes written so far.
	n int64
}

// Write implements io.Writer.
func (b *BoundedWriter) Write(p []byte) (int, error) {
	if b.n+int64(len(p)) > b.Max {
		return 0, ErrStagedObjectTooLarge
	}
	n, err := b.W.Write(p)
	b.n += int64(n)
	return n, err
}

// StagedCopyDDCount is the `qemu-img dd` block count (bs=1M) that copies at
// most measured+1 bytes of a staged object, rounded up to a whole MiB: a copy
// longer than measured proves the object grew after it was measured (a
// tenant NFS server can report one size and serve more), and is refused. The
// copy is therefore bounded by the MEASURED size (plus under 1 MiB), not by
// the import limit.
func StagedCopyDDCount(measured int64) int64 {
	const mib = int64(1) << 20
	return (measured + 1 + mib - 1) / mib
}
