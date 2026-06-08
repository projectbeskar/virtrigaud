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
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// defaultStoragePool is the libvirt storage pool used for image preparation when
// the caller supplies neither a storage hint nor a source-level storage pool.
// It mirrors clonePoolName / ImportDisk's "default" fallback.
const defaultStoragePool = "default"

// defaultChecksumType is the checksum algorithm assumed when the image source
// requests verification (non-empty checksum) but omits the algorithm. It matches
// the v1beta1 LibvirtImageSource kubebuilder default.
const defaultChecksumType = "sha256"

// libvirtImageSource is the normalized, provider-internal view of a VMImage's
// libvirt source, decoupled from the two on-the-wire JSON shapes ImagePrepare may
// receive (see parseLibvirtImageSource). All fields are optional; the caller is
// responsible for enforcing that at least one of Path/URL is set.
type libvirtImageSource struct {
	// Path is an image file already present on the libvirt host. When set, no
	// download is performed — the file is converted into the pool directly.
	Path string
	// URL is an HTTP(S)/FTP location to download on the libvirt host before
	// converting into the pool. Used only when Path is empty.
	URL string
	// Format is the source image format (informational; the target is always
	// qcow2). Defaults to "qcow2" when unset.
	Format string
	// Checksum is the expected checksum of the source image; empty disables
	// verification.
	Checksum string
	// ChecksumType is the checksum algorithm (md5/sha1/sha256/sha512); defaults
	// to sha256 when a Checksum is set but the algorithm is omitted.
	ChecksumType string
	// StoragePool is the libvirt pool to import into; overridden by the request's
	// StorageHint when that is set, and falls back to defaultStoragePool.
	StoragePool string
}

// rawVMImageSpec mirrors the subset of v1beta1.VMImageSpec the libvirt provider
// consumes. It is parsed first because it is the only serialized shape that
// carries source.libvirt.storagePool. Unmarshalling into v1beta1 types directly
// is avoided to keep this parser tolerant of partial/loosely-typed JSON.
type rawVMImageSpec struct {
	Source struct {
		Libvirt *struct {
			Path         string `json:"path"`
			URL          string `json:"url"`
			Format       string `json:"format"`
			Checksum     string `json:"checksum"`
			ChecksumType string `json:"checksumType"`
			StoragePool  string `json:"storagePool"`
		} `json:"libvirt"`
	} `json:"source"`
}

// parseLibvirtImageSource normalizes the JSON-encoded image spec carried by
// ImagePrepareRequest.ImageJson into a libvirtImageSource.
//
// The controller wiring that calls ImagePrepare lands in a later PR (#154 is a
// vertical slice; this is PR-1), so the exact serialized shape is not yet pinned.
// Two shapes are therefore accepted, in priority order:
//
//  1. The rich v1beta1.VMImageSpec shape with a nested
//     source.libvirt.{path,url,format,checksum,checksumType,storagePool}. This is
//     the ONLY shape that can express a storage pool, so it is tried first.
//  2. The flat, provider-agnostic contracts.VMImage shape
//     ({Path,URL,Format,Checksum,ChecksumType}) that Create already round-trips
//     (server.go Create unmarshals ImageJson into contracts.VMImage). It has no
//     storage pool; the pool then comes from the request StorageHint or the
//     default.
//
// Whichever shape yields a usable source (a Path or URL) wins. An empty or
// unparseable ImageJson, or one with neither path nor url, returns an empty
// source — the caller turns that into an InvalidSpec error so a no-op is never
// reported as success.
func parseLibvirtImageSource(imageJSON string) libvirtImageSource {
	var src libvirtImageSource
	if strings.TrimSpace(imageJSON) == "" {
		return src
	}

	// Shape 1: rich v1beta1 spec with nested source.libvirt.
	var spec rawVMImageSpec
	if err := json.Unmarshal([]byte(imageJSON), &spec); err == nil && spec.Source.Libvirt != nil {
		lv := spec.Source.Libvirt
		if lv.Path != "" || lv.URL != "" {
			return libvirtImageSource{
				Path:         lv.Path,
				URL:          lv.URL,
				Format:       lv.Format,
				Checksum:     lv.Checksum,
				ChecksumType: lv.ChecksumType,
				StoragePool:  lv.StoragePool,
			}
		}
	}

	// Shape 2: flat contracts.VMImage (Go field names, no JSON tags).
	var img contracts.VMImage
	if err := json.Unmarshal([]byte(imageJSON), &img); err == nil {
		if img.Path != "" || img.URL != "" {
			return libvirtImageSource{
				Path:         img.Path,
				URL:          img.URL,
				Format:       img.Format,
				Checksum:     img.Checksum,
				ChecksumType: img.ChecksumType,
				// contracts.VMImage carries no storage pool.
			}
		}
	}

	return src
}

// resolveTargetPool selects the libvirt storage pool to import into, in priority
// order: the request StorageHint, then source.libvirt.storagePool, then the
// provider default. This mirrors ImportDisk's hint-or-default behavior while
// additionally honoring the image's own pool preference.
func resolveTargetPool(storageHint, sourcePool string) string {
	switch {
	case storageHint != "":
		return storageHint
	case sourcePool != "":
		return sourcePool
	default:
		return defaultStoragePool
	}
}

// targetImagePath builds the absolute path of the prepared template inside the
// pool directory: <poolPath>/<targetName>.qcow2. The target is always qcow2
// regardless of the source format (qemu-img convert -O qcow2).
func targetImagePath(poolPath, targetName string) string {
	return filepath.Join(poolPath, fmt.Sprintf("%s.qcow2", targetName))
}

// checksumTool maps a checksum algorithm to its coreutils binary
// (md5sum/sha1sum/sha256sum/sha512sum). An empty or unknown algorithm falls back
// to the sha256 default. The boolean reports whether the algorithm was
// recognized, so callers can reject an explicitly-bad algorithm rather than
// silently verifying with the wrong tool.
func checksumTool(checksumType string) (tool string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(checksumType)) {
	case "", defaultChecksumType:
		return "sha256sum", true
	case "md5":
		return "md5sum", true
	case "sha1":
		return "sha1sum", true
	case "sha512":
		return "sha512sum", true
	default:
		return "sha256sum", false
	}
}

// imagePrepare performs the libvirt-side work behind the ImagePrepare RPC:
// resolve the target pool/path, no-op if the target already exists, otherwise
// register (convert) the source image into the pool as <targetName>.qcow2,
// optionally verifying the source checksum.
//
// It is synchronous (virsh/qemu-img are blocking), so it returns no task
// reference; the gRPC layer maps a nil error to a TaskResponse with an empty
// Task, which the controller treats as "completed synchronously".
func (p *Provider) imagePrepare(ctx context.Context, imageJSON, targetName, storageHint string) error {
	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	if strings.TrimSpace(targetName) == "" {
		return contracts.NewInvalidSpecError("ImagePrepare target name is required", nil)
	}

	src := parseLibvirtImageSource(imageJSON)
	if src.Path == "" && src.URL == "" {
		return contracts.NewInvalidSpecError(
			"ImagePrepare requires a libvirt image source with a path or url "+
				"(source.libvirt.path / source.libvirt.url)", nil)
	}

	poolName := resolveTargetPool(storageHint, src.StoragePool)

	storageProvider := NewStorageProvider(p.virshProvider)
	poolInfo, err := storageProvider.GetPoolInfo(ctx, poolName)
	if err != nil {
		return fmt.Errorf("get storage pool %q info: %w", poolName, err)
	}
	if poolInfo.Path == "" {
		return contracts.NewInvalidSpecError(
			fmt.Sprintf("storage pool %q has no filesystem path; cannot place image", poolName), nil)
	}

	targetPath := targetImagePath(poolInfo.Path, targetName)

	// Idempotency (load-bearing): a re-run must be a cheap no-op. Probe the
	// target file on the libvirt host directly; if it exists, the image is
	// already prepared and we return without re-downloading/converting.
	if p.targetImageExists(ctx, targetPath) {
		log.Printf("INFO ImagePrepare: target image %q already exists in pool %q; nothing to do",
			targetPath, poolName)
		return nil
	}

	log.Printf("INFO ImagePrepare: preparing image %q into pool %q (path=%q url=%q)",
		targetName, poolName, src.Path, src.URL)

	if src.Path != "" {
		// Source already on the libvirt host: convert it into the pool as the
		// named template. No download needed.
		if err := p.verifyChecksum(ctx, src.Path, src.Checksum, src.ChecksumType); err != nil {
			return err
		}
		if err := p.convertIntoPool(ctx, src.Path, targetPath); err != nil {
			return err
		}
		p.finalizeClonedDisk(ctx, targetPath)
		log.Printf("INFO ImagePrepare: prepared image %q at %q from host path", targetName, targetPath)
		return nil
	}

	// URL source: download ON THE LIBVIRT HOST (not through the provider pod) to
	// avoid streaming GBs through the pod, then convert and clean up the temp
	// file. Mirrors ImportDisk/clone host-exec ("!") convention.
	//
	// Stage the download in the POOL DIRECTORY (disk-backed, sized for images),
	// not /tmp: /tmp is frequently tmpfs (RAM) on the libvirt host, so a multi-GB
	// image could exhaust host memory. The leading dot keeps the partial file
	// from being treated as a pool volume on refresh; the deferred cleanup
	// removes it regardless of outcome.
	tmpPath := filepath.Join(poolInfo.Path, fmt.Sprintf(".virtrigaud-imageprepare-%s.download", targetName))
	if err := p.downloadOnHost(ctx, src.URL, tmpPath); err != nil {
		return err
	}
	defer p.removeHostFile(ctx, tmpPath)

	if err := p.verifyChecksum(ctx, tmpPath, src.Checksum, src.ChecksumType); err != nil {
		return err
	}
	if err := p.convertIntoPool(ctx, tmpPath, targetPath); err != nil {
		return err
	}
	p.finalizeClonedDisk(ctx, targetPath)
	log.Printf("INFO ImagePrepare: prepared image %q at %q from url", targetName, targetPath)
	return nil
}

// targetImageExists reports whether the prepared image already exists on the
// libvirt host. It stats the path over the host-exec ("!") channel; any non-nil
// error (missing file, transient probe failure) is treated as "does not exist"
// so preparation proceeds rather than skipping work on a false negative.
func (p *Provider) targetImageExists(ctx context.Context, targetPath string) bool {
	res, err := p.virshProvider.runVirshCommand(ctx, "!", "stat", "-c", "%s", targetPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(res.Stdout) != ""
}

// downloadOnHost fetches url to dstPath on the libvirt host using curl. The
// download runs on the host (not the provider pod) so large images never transit
// the pod. curl flags: -f (fail on HTTP errors), -s (silent), -S (still show
// errors), -L (follow redirects).
func (p *Provider) downloadOnHost(ctx context.Context, url, dstPath string) error {
	log.Printf("INFO ImagePrepare: downloading %q to %q on libvirt host", url, dstPath)
	res, err := p.virshProvider.runVirshCommand(ctx, "!", "curl", "-fsSL", url, "-o", dstPath)
	if err != nil {
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		return contracts.NewRetryableError(
			fmt.Sprintf("download image from %q on libvirt host: %s", url, strings.TrimSpace(stderr)), err)
	}
	return nil
}

// convertIntoPool registers srcPath into the pool by converting it to a
// standalone qcow2 at targetPath via qemu-img convert. The source format is
// auto-probed (no -f), matching createFullCopy, so VMDK/raw/qcow2 sources all
// work. On failure the partial target is removed so a retry starts clean.
func (p *Provider) convertIntoPool(ctx context.Context, srcPath, targetPath string) error {
	log.Printf("INFO ImagePrepare: converting %q -> %q (qcow2)", srcPath, targetPath)
	res, err := p.virshProvider.runVirshCommand(ctx, "!",
		"qemu-img", "convert", "-O", "qcow2", srcPath, targetPath)
	if err != nil {
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		p.removeHostFile(ctx, targetPath)
		return fmt.Errorf("convert image into pool (%q -> %q): %w, output: %s",
			srcPath, targetPath, err, strings.TrimSpace(stderr))
	}
	return nil
}

// verifyChecksum verifies that path hashes to expected using the requested
// algorithm. An empty expected checksum disables verification (no-op). On a
// mismatch it returns an InvalidSpec error; the caller is responsible for
// cleaning up any target it created. The checksum is computed on the libvirt host
// (where the file lives), reusing ImportDisk's sha256sum pattern generalized to
// the requested algorithm.
func (p *Provider) verifyChecksum(ctx context.Context, path, expected, checksumType string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	tool, known := checksumTool(checksumType)
	if !known {
		return contracts.NewInvalidSpecError(
			fmt.Sprintf("unsupported checksum type %q (want md5/sha1/sha256/sha512)", checksumType), nil)
	}

	log.Printf("INFO ImagePrepare: verifying %s of %q", tool, path)
	res, err := p.virshProvider.runVirshCommand(ctx, "!", tool, path)
	if err != nil {
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		return contracts.NewRetryableError(
			fmt.Sprintf("compute %s of %q: %s", tool, path, strings.TrimSpace(stderr)), err)
	}

	// coreutils *sum output: "<hex>  <path>".
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return contracts.NewRetryableError(
			fmt.Sprintf("compute %s of %q: empty output", tool, path), nil)
	}
	got := fields[0]
	if !strings.EqualFold(got, strings.TrimSpace(expected)) {
		return contracts.NewInvalidSpecError(
			fmt.Sprintf("checksum mismatch for %q: expected %s, got %s", path, expected, got), nil)
	}
	log.Printf("INFO ImagePrepare: checksum OK for %q", path)
	return nil
}

// removeHostFile best-effort removes a file on the libvirt host (a downloaded
// temp file, or a partial target after a failed convert). Failures are logged,
// not returned, since this is cleanup.
func (p *Provider) removeHostFile(ctx context.Context, path string) {
	if _, err := p.virshProvider.runVirshCommand(ctx, "!", "rm", "-f", path); err != nil {
		log.Printf("WARN ImagePrepare: failed to remove host file %q: %v", path, err)
	}
}

// Compile-time assertion that the v1beta1 image source shape this provider parses
// stays in sync with the API: if LibvirtImageSource loses one of these fields,
// the build breaks here, prompting parseLibvirtImageSource to be revisited.
var _ = func() v1beta1.LibvirtImageSource {
	return v1beta1.LibvirtImageSource{
		Path:         "",
		URL:          "",
		Format:       "",
		Checksum:     "",
		ChecksumType: "",
		StoragePool:  "",
	}
}
