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
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// DeleteStagedDisk removes a VMMigration's staging object (ADR-0011). The
// object is derived from the migration identity (migration.ResolveStagedDisk;
// the request carries no location). Both backends run in the provider pod,
// exactly where this provider's export and import run: s3 as the S3 client,
// nfs through the pod's qemu-img over libnfs. libnfs through qemu-img cannot
// unlink, so an nfs object is emptied (truncated to zero bytes) instead of
// removed (migration.EmptyNFSStagedDisk). Credentials are never logged; the
// location is logged at debug only.
func (p *Provider) DeleteStagedDisk(ctx context.Context, req *providerv1.DeleteStagedDiskRequest) (*providerv1.DeleteStagedDiskResponse, error) {
	disk, err := migration.ResolveStagedDisk(req)
	if err != nil {
		return nil, err
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()
	p.logger.Debug("Deleting a migration's staged disk", "migration", migrationRef, "backend", disk.Backend, "key", disk.Key)

	var outcome providerv1.StagedDiskDeleteOutcome
	switch disk.Backend {
	case migration.BackendS3:
		outcome, err = migration.DeleteS3StagedDisk(ctx, disk, req.GetCredentials())
	default: // migration.BackendNFS: ResolveStagedDisk accepts nothing else.
		outcome, err = migration.EmptyNFSStagedDisk(ctx, p.runQemuImg, disk.NFSURL)
	}
	if err != nil {
		// The status message names no location; the cause (stderr, host,
		// path) is logged at debug only.
		p.logger.Warn("Could not delete a migration's staged disk", "migration", migrationRef, "backend", disk.Backend,
			"reason", status.Convert(err).Message())
		p.logger.Debug("Staged disk delete failure cause", "migration", migrationRef, "error", err.Error())
		return nil, err
	}
	p.logger.Info("Migration staged disk removed", "migration", migrationRef, "backend", disk.Backend, "outcome", outcome.String())
	return &providerv1.DeleteStagedDiskResponse{Outcome: outcome}, nil
}

// runQemuImg runs the pod's qemu-img with args (argv, no shell) and returns
// its stdout and stderr: the migration.QemuImgRunner of the nfs delete.
func (p *Provider) runQemuImg(ctx context.Context, args ...string) (string, string, error) {
	bin := p.qemuImgPath
	if bin == "" {
		bin = "qemu-img"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- the provider's qemu-img; argv (no shell) built by migration.EmptyNFSStagedDisk
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// checkStagedLocalImage checks the header of a staged migration image the pod
// downloaded to path (a pod-local file the tenant can no longer change) before
// it is converted: its leading bytes are parsed without qemu
// (migration.CheckStagedImageBytes); then, read as format exactly as the
// convert reads it, it must reference no other file
// (migration.CheckStagedImageHeader). The staged
// object comes from a backend the tenant controls; a qcow2 backing file or
// data file could otherwise make the pod's qemu-img read its service-account
// token, its vCenter credentials or a json:/nbd:/http: target into the
// tenant's disk. A refused or unreadable header is InvalidSpec.
func (p *Provider) checkStagedLocalImage(ctx context.Context, path, format string, limit int64) error {
	// 1. Parse the leading bytes WITHOUT qemu: qemu opens a qcow2 external
	// data file and VMDK extents while it reads a header, so `qemu-img info`
	// on a crafted image could open a pod file or an nbd:/http: target before
	// anything refuses it.
	head, err := readFileHead(path, migration.StagedImageHeadBytes)
	if err != nil {
		return errors.NewInternal("reading the staged image header failed", err)
	}
	if _, err := migration.CheckStagedImageBytes(head, format, limit); err != nil {
		p.logger.Warn("Refused a staged migration image", "reason", err.Error())
		return errors.NewInvalidSpec("refused the staged migration image: %v", err)
	}
	// 2. Only then qemu-img, read as the convert reads it.
	out, stderr, err := p.runQemuImg(ctx, "info", "-U", "--output=json", "-f", format, "--", path)
	if err != nil {
		var exitErr *exec.ExitError
		if stderrors.As(err, &exitErr) {
			p.logger.Debug("qemu-img cannot read the staged image", "path", path, "stderr", stderr)
			return errors.NewInvalidSpec("the staged migration image is not a readable %s image", format)
		}
		return errors.NewInternal("qemu-img info on the staged image could not run", err)
	}
	if err := migration.CheckStagedImageHeader(out, format, path); err != nil {
		p.logger.Warn("Refused a staged migration image", "reason", err.Error())
		return errors.NewInvalidSpec("refused the staged migration image: %v", err)
	}
	return nil
}

// readFileHead returns the first n bytes of the local file path (fewer when
// it is shorter).
func readFileHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- path is the provider's own temp file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && !stderrors.Is(err, io.ErrUnexpectedEOF) && !stderrors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:read], nil
}

// stagedCopyTmpDir is where the vSphere pod copies staged objects.
const stagedCopyTmpDir = "/tmp"

// tmpFreeBytes is how much more the pod may write to dir; tests replace it.
var tmpFreeBytes = podTmpFreeBytes

// podTmpFreeBytes is the space left in the pod's /tmp (dir): the filesystem's
// free space (Bavail × Frsize) — but /tmp is an emptyDir with a sizeLimit
// (migration.EnvTmpSizeLimit), and the kubelet evicts the pod past it, which
// statfs does not show. With a limit set, the result is at most the limit
// minus what dir already holds (allocated blocks).
func podTmpFreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	frsize := st.Frsize
	if frsize <= 0 {
		frsize = st.Bsize
	}
	if frsize <= 0 {
		return 0, fmt.Errorf("statfs %s reports no block size", dir)
	}
	free := int64(math.MaxInt64)
	if st.Bavail <= math.MaxInt64 {
		if blocks := int64(st.Bavail); blocks <= math.MaxInt64/frsize { // #nosec G115 -- st.Bavail <= MaxInt64 checked above
			free = blocks * frsize
		}
	}
	raw := strings.TrimSpace(os.Getenv(migration.EnvTmpSizeLimit))
	if raw == "" {
		return free, nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a quantity: %w", migration.EnvTmpSizeLimit, raw, err)
	}
	used, err := dirAllocatedBytes(dir)
	if err != nil {
		return 0, err
	}
	return max(min(free, q.Value()-used), 0), nil
}

// dirAllocatedBytes is the space the files under dir occupy (allocated
// blocks, as the kubelet counts emptyDir usage).
func dirAllocatedBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if stderrors.Is(err, fs.ErrNotExist) || stderrors.Is(err, fs.ErrPermission) {
				return nil // a file removed meanwhile, or not ours to read
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // removed meanwhile
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			total += sys.Blocks * 512
		} else {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// boundedNFSCopy copies the staged NFS object byte for byte to dst in the pod,
// bounded (re-review Medium 1): the tenant's NFS server controls its size, and
// a full /tmp evicts the provider pod — an outage for every tenant. Its size
// is read first (`qemu-img info -f raw`, which opens nothing but that file)
// and refused above limit (InvalidSpec) or when /tmp has less than
// migration.StagedCopySpaceFactor times it free (ResourceExhausted, retryable).
// The copy is `qemu-img dd` of at most the MEASURED size + 1 MiB (a tenant NFS
// server can report a small size and serve more), and a copy longer than the
// measured size is refused. Refusals
// are constant; the caller removes dst.
func (p *Provider) boundedNFSCopy(ctx context.Context, nfsURL, dst string, limit int64) error {
	out, _, err := p.runQemuImg(ctx, "info", "-U", "--output=json", "-f", "raw", nfsURL)
	if err != nil {
		return errors.NewInternal("failed to read the size of the staged nfs object", err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return errors.NewInternal("failed to parse the size of the staged nfs object", err)
	}
	free, err := tmpFreeBytes(stagedCopyTmpDir)
	if err != nil {
		return errors.NewInternal("failed to read the pod's free /tmp space", err)
	}
	if err := stagedCopyRefusal(migration.CheckStagedCopy(info.VirtualSize, limit, free)); err != nil {
		return err
	}
	if _, stderr, err := p.runQemuImg(ctx, "dd", "-f", "raw", "-O", "raw", "bs=1M",
		"count="+strconv.FormatInt(migration.StagedCopyDDCount(info.VirtualSize), 10), "if="+nfsURL, "of="+dst); err != nil {
		p.logger.Debug("Staged nfs copy failed", "stderr", stderr)
		return errors.NewInternal("failed to copy the staged nfs object into the pod", err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		return errors.NewInternal("failed to read the size of the staged copy", err)
	}
	if st.Size() > info.VirtualSize || st.Size() > limit {
		return stagedCopyRefusal(migration.ErrStagedObjectTooLarge)
	}
	return nil
}

// stagedCopyRefusal maps a migration.CheckStagedCopy refusal: too large is
// InvalidSpec (permanent), no space is ResourceExhausted (retryable); both
// constant. nil stays nil.
func stagedCopyRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case stderrors.Is(err, migration.ErrStagedObjectTooLarge):
		return errors.NewInvalidSpec("staged import: %s", err.Error())
	default:
		return status.Error(codes.ResourceExhausted, "staged import: "+err.Error())
	}
}

// stagedDownloadBound is the most bytes an S3 staged object may be downloaded
// into the pod's /tmp: its reported size, refused (constant) above the import
// limit or when /tmp cannot hold StagedCopySpaceFactor times it.
func (p *Provider) stagedDownloadBound(ctx context.Context, s3client storage.Storage, sourceURL string, limit int64) (int64, error) {
	meta, err := s3client.GetMetadata(ctx, sourceURL)
	if err != nil {
		return 0, errors.NewInternal("failed to read the size of the staged s3 object", err)
	}
	free, err := tmpFreeBytes(stagedCopyTmpDir)
	if err != nil {
		return 0, errors.NewInternal("failed to read the pod's free /tmp space", err)
	}
	if err := stagedCopyRefusal(migration.CheckStagedCopy(meta.Size, limit, free)); err != nil {
		return 0, err
	}
	return meta.Size, nil
}
