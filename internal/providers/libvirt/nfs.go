/*
Copyright 2025.

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
	"log"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// exportDiskToNFS implements the ADR-0006 Slice 4 libvirt SOURCE path for the NFS
// backend: the libvirt HOST's qemu-img writes the flattened qcow2 directly to the
// NFS export over libnfs (qemu's block-nfs driver), with no provider-pod hop and
// no S3 client. This is the host-side `direct`-style transport that is natural
// for NFS (the host already has the disk and NFS reachability). The destination
// is the controller-built, C7'-hardened nfs:// URL.
func (s *Server) exportDiskToNFS(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	vp := libvirtProvider.virshProvider
	if !strings.Contains(vp.uri, "ssh://") {
		return nil, fmt.Errorf("nfs export requires an ssh:// libvirt transport (host-side qemu-img → nfs://); got %q", vp.uri)
	}

	nfsURL := strings.TrimSpace(req.DestinationUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, fmt.Errorf("nfs export destination must be an nfs:// URL, got %q", nfsURL)
	}

	// Resolve the source disk path on the host.
	diskInfo, err := libvirtProvider.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve source disk info: %w", err)
	}
	srcPath := diskInfo.Path
	if srcPath == "" {
		return nil, fmt.Errorf("source disk %q has no resolvable host path", req.DiskId)
	}

	log.Printf("INFO Exporting disk from libvirt host to NFS: backend=nfs vm=%s src=%s dest=%s",
		req.VmId, srcPath, nfsURL)

	// Flatten + write straight to the NFS export. -U reads a possibly-running
	// source (crash-consistent; a consistent copy still needs power-off/snapshot
	// first). qemu-img's libnfs driver performs the NFS write — no pod buffering.
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "convert", "-U", "-f", "qcow2", "-O", "qcow2",
		shellQuote(srcPath), shellQuote(nfsURL)); err != nil {
		return nil, fmt.Errorf("host-side qemu-img convert to nfs failed: %w%s", err, qemuImgStderr(res))
	}

	log.Printf("INFO Source disk written to NFS export: dest=%s", nfsURL)

	return &providerv1.ExportDiskResponse{
		ExportId: fmt.Sprintf("export-libvirt-nfs-%s-%d", req.VmId, time.Now().Unix()),
		// qemu-img does not emit a byte SHA256 over the libnfs write; NFS integrity
		// is established by the target's `qemu-img check` on the converted qcow2
		// (ADR-0006 Slice 4 — no in-stream checksum for the qemu-img transport).
	}, nil
}

// importDiskFromNFS implements the ADR-0006 Slice 4 libvirt TARGET path: the
// host's qemu-img reads the staged qcow2 directly from the NFS export over libnfs
// and writes it into the target storage pool. No download, no pod staging.
func (s *Server) importDiskFromNFS(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	vp := libvirtProvider.virshProvider
	if !strings.Contains(vp.uri, "ssh://") {
		return nil, fmt.Errorf("nfs import requires an ssh:// libvirt transport (host-side qemu-img from nfs://); got %q", vp.uri)
	}

	nfsURL := strings.TrimSpace(req.SourceUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, fmt.Errorf("nfs import source must be an nfs:// URL, got %q", nfsURL)
	}

	// Resolve the target pool and its host path.
	poolName := "default"
	if req.StorageHint != "" {
		poolName = req.StorageHint
	}
	storageProvider := NewStorageProvider(vp)
	if err := storageProvider.EnsureDefaultStoragePool(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure storage pool: %w", err)
	}
	poolInfo, err := storageProvider.GetPoolInfo(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve target pool %q: %w", poolName, err)
	}
	if poolInfo.Path == "" {
		return nil, fmt.Errorf("target pool %q has no resolvable host path", poolName)
	}

	volumeName := req.TargetName
	if volumeName == "" {
		volumeName = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	volumeName = sanitizeVolumeName(volumeName)
	poolPath := strings.TrimRight(poolInfo.Path, "/")
	targetPath := fmt.Sprintf("%s/%s.qcow2", poolPath, volumeName)

	log.Printf("INFO Importing disk from NFS to libvirt host: backend=nfs pool=%s volume=%s src=%s target=%s",
		poolName, volumeName, nfsURL, targetPath)

	// Read the staged qcow2 straight from NFS and write the pool volume.
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "convert", "-f", "qcow2", "-O", "qcow2",
		shellQuote(nfsURL), shellQuote(targetPath)); err != nil {
		return nil, fmt.Errorf("host-side qemu-img convert from nfs failed: %w%s", err, qemuImgStderr(res))
	}

	// Validate the converted qcow2 (ADR-0006 D5 structural integrity for NFS).
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "check", shellQuote(targetPath)); err != nil {
		return nil, fmt.Errorf("qemu-img check failed on imported qcow2 %s: %w%s", targetPath, err, qemuImgStderr(res))
	}

	if _, err := vp.runVirshCommand(ctx, "pool-refresh", poolName); err != nil {
		log.Printf("WARN pool-refresh failed after import (volume may still be usable by path): %v", err)
	}

	log.Printf("INFO NFS object imported to pool volume: target=%s", targetPath)

	return &providerv1.ImportDiskResponse{
		DiskId: volumeName,
		Path:   targetPath,
		Task:   nil,
	}, nil
}
