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
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
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

// Image preparation (ImagePrepare, issue #154; ADR-0009 Slice 4).
//
// An ImagePrepare request is served in one of two modes (imageartifact.Mode):
//
//   - IDENTITY mode (the request carries the VMImage identity and the source
//     digest; see image_publish.go): the artifact is
//     <pool>/<name>.qcow2 with name = imageartifact.ArtifactName (libvirt rule:
//     at most 200 bytes, '_' before the 16-hex hash), stamped by the sidecar
//     .<name>.virtrigaud-image.json, reused only when that stamp carries the
//     request's image UID and source digest and records the artifact's inode
//     and size (imageartifact.Decide, fail-closed), and published atomically
//     with link(2) so it is never overwritten. The only accepted input is
//     source.libvirt.url: a source that also (or only) sets source.libvirt.path
//     is InvalidSpec, because a path is a reference to an existing image and
//     converting it would mint a stamped copy of any file in the allowed image
//     directories.
//   - deprecated LEGACY mode (a manager older than ADR-0009: no identity, a
//     bare target_name; this release only): the pre-ADR bare-name artifact
//     <pool>/<target_name>.qcow2, reused by name, with the ADR-0009 D4/D6
//     provider-internal fixes below. The server signals every such request
//     (imageartifact.SignalLegacyRequest).
//
// Both modes share the provider-internal rules of ADR-0009 D4/D6:
//
//   - the pool must resolve (on the host) to an allowed image directory
//     (#334, EnvImageDirs) — a VM could not be created from an image anywhere
//     else — and the artifact name is never a #334 reserved name;
//   - a failed probe of the final name is a retryable error, never "absent";
//   - staging is private to one prepare: the download, the qemu-img convert
//     output and the stamp are created by mktemp (O_EXCL, mode 0600, an
//     unpredictable name) as dotfiles with reserved suffixes in the pool
//     directory, and removed afterwards; stale ones (older than the staleness
//     bound) are swept. The source URL is handed to curl on stdin (`curl -q
//     -K -`), so it never appears on a command line, in a log, in the host's
//     process list or in a file at rest;
//   - the download is exactly one transfer (curl URL globbing off), limited to
//     http/https/ftp (redirects included), bounded in connect time, total time
//     (spec.prepare.timeout) and size (EnvImageMaxDownloadGiB);
//   - nothing is ever downloaded, converted, written or removed at the final
//     name: the converted image is finalized READ-ONLY (chmod 0444, restorecon
//     when SELinux is enabled, sync; no chown, so the provider still owns it and
//     fs.protected_hardlinks lets it link the file) and published with `ln -T`,
//     which never replaces an existing file. finalizeClonedDisk (chown + chmod 777) is never applied to
//     a prepared image — only to the per-VM copies Create makes from it.
//
// Failure classification (the manager holds on InvalidSpec, retries the rest):
//
//   - PERMANENT, InvalidSpec (gRPC InvalidArgument): a malformed request or
//     source (path+url or no url in identity mode, a reserved target name, a
//     URL that is not http/https/ftp), a pool that does not exist, has no
//     directory, is outside the allowed image directories or cannot hold hard
//     links; a source URL answering HTTP 4xx (except 408/425/429), resolving to
//     more than one transfer, larger than the download limit, or failing with a
//     curl error that retrying cannot fix (unsupported protocol, malformed URL,
//     access/login denied, remote file not found, TLS certificate not
//     verifiable); a checksum mismatch or unsupported checksum
//     type; an image qemu-img cannot read or whose header references other
//     files; a host path rejected by the #334 confinement.
//   - CONFLICT (AlreadyExists): an artifact at the derived name that was not
//     prepared for this VMImage (identity mode, ADR-0009 D4).
//   - IN PROGRESS (Unavailable, retryable): the artifact is being published
//     by another prepare of this same image.
//   - TRANSIENT (retryable): the SSH transport or the host failing (a probe,
//     mktemp, chmod/sync, convert, link or checksum command that could not
//     run), HTTP 5xx/408/425/429, DNS/connect/timeout/TLS-handshake/transfer
//     errors of the download, a storage pool lookup that failed for any reason
//     other than "Storage pool not found". Of these, the download failures the
//     image source caused (sourceCurlExits: HTTP 5xx/408/425/429, DNS, connect,
//     timeout, TLS-handshake, send/receive, truncated body) go on the wire as
//     imageartifact.SourceUnavailableError (IMAGE_SOURCE_UNAVAILABLE), which
//     the manager retries without counting toward the Provider's circuit
//     breaker; the host or the transport failing still counts. Error text
//     never carries the source URL (it may embed credentials), a curl exit
//     code, an HTTP status or a checksum the host computed (no reachability
//     oracle for a tenant-chosen URL); details are in the provider log, URL
//     redacted.

const (
	// libvirtProviderType is this provider's provider_type metric label.
	libvirtProviderType = "libvirt"

	// defaultImagePrepareTimeout is the CRD default of VMImage
	// spec.prepare.timeout, used when the request's spec carries none.
	defaultImagePrepareTimeout = 30 * time.Minute
	// minImagePrepareStaleness is the floor of the staleness bound.
	minImagePrepareStaleness = 2 * time.Hour
	// maxImagePrepareTimeout clamps a requested spec.prepare.timeout, so the
	// staleness bound can neither overflow nor keep a crashed prepare's stamp
	// "in progress" for an unbounded time.
	maxImagePrepareTimeout = 7 * 24 * time.Hour

	// imagePrepareStagingPrefix starts every staging file a prepare makes in
	// the pool directory: .virtrigaud-imageprepare-<random><suffix>. The
	// staleness sweep matches it (it also matches the pre-ADR shared
	// .virtrigaud-imageprepare-<target>.download name).
	imagePrepareStagingPrefix = ".virtrigaud-imageprepare-"
	// downloadStagingSuffix ends the staged download (#334 reserved suffix).
	downloadStagingSuffix = ".download"
	// convertStagingSuffix ends the staged qemu-img convert output, the file
	// that becomes the artifact (#334 reserved suffix).
	convertStagingSuffix = ".partial"
	// stampStagingSuffix ends the staged sidecar stamp.
	stampStagingSuffix = ".stamp.partial"

	// preparedImageMode is the mode of a published prepared image and of its
	// sidecar: read-only for everyone, the provider's SSH user still owning it.
	preparedImageMode = "0444"

	// curlAllowedProtocols restricts the download, and every redirect it
	// follows, to the protocols the CRD admits for source.libvirt.url.
	curlAllowedProtocols = "=http,https,ftp"
	// curlConnectTimeoutSeconds bounds connection setup of the download.
	curlConnectTimeoutSeconds = "30"

	// EnvImageMaxDownloadGiB names the provider-pod environment variable that
	// bounds the size of an image download, in GiB (curl --max-filesize). A
	// larger source is refused as InvalidSpec. Unset, blank or invalid selects
	// DefaultImageMaxDownloadGiB.
	EnvImageMaxDownloadGiB = "VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB"
	// DefaultImageMaxDownloadGiB is the default download size limit.
	DefaultImageMaxDownloadGiB = 256
	// maxImageMaxDownloadGiB bounds EnvImageMaxDownloadGiB (16 TiB).
	maxImageMaxDownloadGiB = 16 * 1024
)

// imagePrepareResult is where an ImagePrepare left the prepared image.
type imagePrepareResult struct {
	// ID is prepared_image_id: the artifact base name (identity mode) or the
	// bare target name (legacy mode).
	ID string
	// Path is prepared_image_path: the absolute path of the artifact file.
	Path string
	// Stamp is the stamp the provider verified (reuse) or wrote (a new
	// artifact); nil in legacy mode, which echoes no artifact.
	Stamp *imageartifact.Stamp
	// Reused is true when an existing, matching artifact was reused.
	Reused bool
}

// imageHost is the host a prepare runs on: host commands, plus host commands
// fed content on stdin (runHostStdin), for what must never be on a command
// line. *VirshProvider implements it.
type imageHost interface {
	hostCommandRunner
	runHostStdin(ctx context.Context, stdin []byte, argv ...string) (*VirshResult, error)
}

// imagePreparer runs one ImagePrepare against one host.
type imagePreparer struct {
	// host is the libvirt host the image is prepared on.
	host imageHost
	// policy is the #334 image-path confinement policy.
	policy imagePathPolicy
	// now is the provider clock (the stamp's informational preparedAt).
	now func() time.Time
}

// prepareLocation is the storage pool an image is prepared into.
type prepareLocation struct {
	// pool is the libvirt storage pool name.
	pool string
	// dir is the pool's directory, canonical on the host and an allowed image
	// directory.
	dir string
}

// prepareJob is a validated prepare: its source, its location, and the
// prepare timeout and staleness bound of the requesting VMImage.
type prepareJob struct {
	src       libvirtImageSource
	loc       prepareLocation
	timeout   time.Duration
	staleness time.Duration
}

// stagedImage is a converted image finalized for publishing: a read-only,
// synced staging file in the pool directory.
type stagedImage struct {
	path  string
	inode uint64
	size  int64
}

// imagePrepare performs the libvirt-side work behind the ImagePrepare RPC for
// the parsed request req (see the comment above). It is synchronous
// (virsh/qemu-img are blocking), so the gRPC layer returns an
// ImagePrepareResponse with no task alongside the returned location.
func (p *Provider) imagePrepare(ctx context.Context, req imageartifact.Request, imageJSON, storageHint string) (imagePrepareResult, error) {
	if p.virshProvider == nil {
		return imagePrepareResult{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	ip := &imagePreparer{host: p.virshProvider, now: time.Now}
	return ip.prepare(ctx, req, imageJSON, storageHint, p.imagePolicy)
}

// prepare validates the request (before any host command), resolves the
// location and dispatches on the request mode. policy supplies the #334
// confinement policy.
func (ip *imagePreparer) prepare(ctx context.Context, req imageartifact.Request, imageJSON, storageHint string,
	policy func() (imagePathPolicy, error)) (imagePrepareResult, error) {
	src := parseLibvirtImageSource(imageJSON)
	switch req.Mode {
	case imageartifact.ModeIdentity:
		if err := validateIdentitySource(src); err != nil {
			return imagePrepareResult{}, err
		}
	case imageartifact.ModeLegacy:
		if err := validateLegacyTarget(req.LegacyTargetName); err != nil {
			return imagePrepareResult{}, err
		}
		if src.Path == "" && src.URL == "" {
			return imagePrepareResult{}, contracts.NewInvalidSpecError(
				"ImagePrepare requires a libvirt image source with a path or url "+
					"(source.libvirt.path / source.libvirt.url)", nil)
		}
	default:
		return imagePrepareResult{}, contracts.NewInvalidSpecError("ImagePrepare request has no valid mode", nil)
	}
	// Legacy mode keeps the pre-ADR precedence: a path, when set, is what is
	// converted; the URL is used only without one.
	if src.Path == "" {
		if err := validateImageSourceURL(src.URL); err != nil {
			return imagePrepareResult{}, err
		}
	}

	pol, err := policy()
	if err != nil {
		return imagePrepareResult{}, err
	}
	ip.policy = pol
	loc, err := ip.resolveLocation(ctx, resolveTargetPool(storageHint, src.StoragePool))
	if err != nil {
		return imagePrepareResult{}, err
	}
	timeout := imagePrepareTimeout(imageJSON)
	job := prepareJob{src: src, loc: loc, timeout: timeout, staleness: stalenessFor(timeout)}
	if req.Mode == imageartifact.ModeIdentity {
		return ip.prepareIdentity(ctx, req, job)
	}
	return ip.prepareLegacy(ctx, req.LegacyTargetName, job)
}

// validateIdentitySource enforces ADR-0009 D1's single-input rule for an
// identity-mode import: exactly source.libvirt.url.
func validateIdentitySource(src libvirtImageSource) error {
	switch {
	case src.Path != "" && src.URL != "":
		return contracts.NewInvalidSpecError("the libvirt image source sets both path and url; an image import takes "+
			"exactly one input, source.libvirt.url (a source.libvirt.path is a reference to an existing image and "+
			"is used as written, never prepared)", nil)
	case src.URL == "":
		return contracts.NewInvalidSpecError("ImagePrepare requires source.libvirt.url (a source.libvirt.path is a "+
			"reference to an existing image and is used as written, never prepared)", nil)
	}
	return nil
}

// validateLegacyTarget refuses a legacy bare target name that is not a plain
// file name or collides with VirtRigaud-managed volume names (#334): the
// prepared file is <pool>/<name>.qcow2 and would alias a VM disk (<vm>-disk)
// or an imported disk (<vm>-migrated).
func validateLegacyTarget(name string) error {
	if strings.TrimSpace(name) == "" {
		return contracts.NewInvalidSpecError("ImagePrepare target name is required", nil)
	}
	if name != filepath.Base(name) || reservedImageName(name+qcow2Ext) {
		return newImageRejection(fmt.Sprintf("image target name %q", name),
			"it collides with VirtRigaud-managed volume names (<vm>-disk, <vm>-migrated, dotfiles); rename the VMImage")
	}
	return nil
}

// validateImageSourceURL refuses a download URL the host must not fetch: not
// valid UTF-8, with control characters, unparseable, or not http/https/ftp
// with a host (the CRD pattern, re-checked here: curl would also read
// file:// and other schemes). The URL is never echoed (it may carry
// credentials).
func validateImageSourceURL(raw string) error {
	if raw == "" {
		return contracts.NewInvalidSpecError("ImagePrepare requires source.libvirt.url", nil)
	}
	if !utf8.ValidString(raw) || strings.ContainsFunc(raw, isControlRune) || strings.ContainsRune(raw, ' ') {
		return contracts.NewInvalidSpecError("the image source URL contains spaces or control characters", nil)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return contracts.NewInvalidSpecError("the image source URL is not a valid URL", nil)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp":
	default:
		return contracts.NewInvalidSpecError("the image source URL must use http, https or ftp", nil)
	}
	if u.Host == "" {
		return contracts.NewInvalidSpecError("the image source URL has no host", nil)
	}
	return nil
}

// redactURL renders raw for the provider log: scheme, host and path only. The
// user-info, query and fragment are dropped (they may carry credentials or
// presigned tokens); "?…" marks a dropped query.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable URL>"
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if u.RawQuery != "" || u.ForceQuery {
		out += "?…"
	}
	return out
}

// rawVMImagePrepareSpec mirrors spec.prepare.timeout of the serialized
// VMImageSpec.
type rawVMImagePrepareSpec struct {
	Prepare *struct {
		Timeout *metav1.Duration `json:"timeout"`
	} `json:"prepare"`
}

// imagePrepareTimeout is the requester's spec.prepare.timeout from its
// serialized spec, clamped to maxImagePrepareTimeout (the CRD default when it
// carries none or an unusable one). It bounds the download (curl --max-time).
func imagePrepareTimeout(imageJSON string) time.Duration {
	var spec rawVMImagePrepareSpec
	if err := json.Unmarshal([]byte(imageJSON), &spec); err == nil &&
		spec.Prepare != nil && spec.Prepare.Timeout != nil && spec.Prepare.Timeout.Duration > 0 {
		return min(spec.Prepare.Timeout.Duration, maxImagePrepareTimeout)
	}
	return defaultImagePrepareTimeout
}

// stalenessFor is ADR-0009 D4's single staleness bound for a prepare timeout,
// max(2 × timeout, 2h). A staging file or a stamp without its artifact whose
// mtime is older than the bound is no longer being written: it is abandoned.
func stalenessFor(timeout time.Duration) time.Duration {
	return max(2*timeout, minImagePrepareStaleness)
}

// imagePrepareStaleness is stalenessFor(imagePrepareTimeout(imageJSON)).
func imagePrepareStaleness(imageJSON string) time.Duration {
	return stalenessFor(imagePrepareTimeout(imageJSON))
}

// imageMaxDownloadBytes is the download size limit (EnvImageMaxDownloadGiB).
func imageMaxDownloadBytes() int64 {
	gib := int64(DefaultImageMaxDownloadGiB)
	if raw := strings.TrimSpace(os.Getenv(EnvImageMaxDownloadGiB)); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 || v > maxImageMaxDownloadGiB {
			log.Printf("WARN ImagePrepare: ignoring invalid %s=%q (want 1..%d); using %d GiB",
				EnvImageMaxDownloadGiB, raw, maxImageMaxDownloadGiB, DefaultImageMaxDownloadGiB)
		} else {
			gib = v
		}
	}
	return gib << 30
}

// poolNotFoundMarker is libvirt's error text for a storage pool that does not
// exist (VIR_ERR_NO_STORAGE_POOL). It is the ONLY pool lookup failure treated
// as permanent: virsh prints "failed to get pool '<name>'" for every lookup
// failure, a libvirtd restart or a lost connection included, and those must
// stay retryable.
const poolNotFoundMarker = "Storage pool not found"

// maxPoolNameBytes bounds a storage pool name taken from the request.
const maxPoolNameBytes = 255

// poolTargetXML is the part of a storage pool definition resolveLocation reads.
type poolTargetXML struct {
	Target struct {
		Path string `xml:"path"`
	} `xml:"target"`
}

// resolveLocation resolves the storage pool an image is prepared into to its
// directory, canonical on the host, and requires it to be an allowed image
// directory (#334): Create confines every image path it copies from, so an
// image prepared anywhere else could never be used, and the provider never
// writes, probes or sweeps outside the allowed directories on a tenant's
// say-so (the pool name comes from the VMImage).
func (ip *imagePreparer) resolveLocation(ctx context.Context, pool string) (prepareLocation, error) {
	if pool == "" || len(pool) > maxPoolNameBytes || strings.HasPrefix(pool, "-") ||
		strings.ContainsRune(pool, '/') || strings.ContainsFunc(pool, isControlRune) || !utf8.ValidString(pool) {
		return prepareLocation{}, contracts.NewInvalidSpecError(fmt.Sprintf("storage pool name %q is not valid", pool), nil)
	}
	res, err := ip.host.runVirshCommand(ctx, "pool-dumpxml", "--pool", pool)
	if err != nil {
		if res != nil && res.ExitCode > 0 && strings.Contains(res.Stderr, poolNotFoundMarker) {
			return prepareLocation{}, contracts.NewInvalidSpecError(
				fmt.Sprintf("storage pool %q does not exist on the libvirt host", pool), nil)
		}
		return prepareLocation{}, hostCheckFailed(fmt.Sprintf("read storage pool %q", pool), err)
	}
	var def poolTargetXML
	if err := xml.Unmarshal([]byte(res.Stdout), &def); err != nil {
		return prepareLocation{}, hostCheckFailed(fmt.Sprintf("parse storage pool %q", pool), err)
	}
	dir := strings.TrimSpace(def.Target.Path)
	if dir == "" {
		return prepareLocation{}, contracts.NewInvalidSpecError(
			fmt.Sprintf("storage pool %q has no filesystem path; cannot place an image in it", pool), nil)
	}
	notAllowed := contracts.NewInvalidSpecError(fmt.Sprintf("storage pool %q is not in an allowed image directory, "+
		"so no VM could be created from an image prepared in it; choose another pool or configure %s on the provider",
		pool, EnvImageDirs), nil)
	if validateImagePathSyntax(dir) != nil || len(ip.policy.dirs) == 0 {
		return prepareLocation{}, notAllowed
	}
	canon, err := canonicalizeOnHost(ctx, ip.host, append([]string{dir}, ip.policy.dirs...))
	if err != nil {
		return prepareLocation{}, err
	}
	poolDir := canon[0]
	if validateImagePathSyntax(poolDir) != nil || isForbiddenImageDir(poolDir) || !containsString(canon[1:], poolDir) {
		log.Printf("WARN ImagePrepare: storage pool %q (directory %q) is outside the allowed image directories %v",
			pool, poolDir, ip.policy.dirs)
		return prepareLocation{}, notAllowed
	}
	return prepareLocation{pool: pool, dir: poolDir}, nil
}

// prepareLegacy serves a deprecated legacy (identity-less) request the pre-ADR
// way — the bare-name artifact <pool>/<target>.qcow2, reused by name, no
// artifact echo — with the ADR-0009 D4/D6 provider-internal fixes: the
// existence probe fails closed (retryable, never "absent"), staging is private,
// and the artifact is finalized read-only and published with `ln`, never
// written, converted or removed at its final name. It can never reach an
// identity-mode artifact: a bare name is a DNS-1123 name and never contains
// '_' (ADR-0009 D1.3).
func (ip *imagePreparer) prepareLegacy(ctx context.Context, target string, job prepareJob) (imagePrepareResult, error) {
	artifact := filepath.Join(job.loc.dir, target+qcow2Ext)
	result := imagePrepareResult{ID: target, Path: artifact}

	exists, err := hostPathPresent(ctx, ip.host, artifact)
	if err != nil {
		return imagePrepareResult{}, err
	}
	if exists {
		return ip.legacyReuse(ctx, target, artifact, job.loc.pool, result)
	}

	log.Printf("INFO ImagePrepare: preparing legacy image %q into pool %q", target, job.loc.pool)
	ip.sweepStaging(ctx, job)
	staged, err := ip.stageSource(ctx, job)
	if err != nil {
		return imagePrepareResult{}, err
	}
	defer removeHostPath(ctx, ip.host, staged.path, false)

	outcome, err := ip.link(ctx, staged.path, artifact)
	if err != nil {
		return imagePrepareResult{}, err
	}
	switch outcome {
	case linkCreated:
		log.Printf("INFO ImagePrepare: prepared legacy image %q at %q", target, artifact)
		ip.refreshPool(ctx, job.loc.pool)
		return result, nil
	case linkExists:
		// A concurrent legacy prepare of the same name published first: reuse
		// it by name, as the pre-ADR provider would have on its next call.
		log.Printf("INFO ImagePrepare: legacy image %q was published concurrently; reusing it", artifact)
		return result, nil
	default:
		return imagePrepareResult{}, noHardLinksError(job.loc.pool)
	}
}

// legacyReuse returns an existing bare-name image unless it is in use as a VM
// disk: an earlier release attached a prepared image IN PLACE as the disk of
// the first VM created from it, and handing that disk to every new VM (which
// Create now refuses) must be reported with the one fix that works.
func (ip *imagePreparer) legacyReuse(ctx context.Context, target, artifact, pool string, result imagePrepareResult) (imagePrepareResult, error) {
	inUse, err := pathInUseOnHost(ctx, ip.host, artifact)
	if err != nil {
		return imagePrepareResult{}, err
	}
	if inUse {
		log.Printf("WARN ImagePrepare: prepared image %q is in use as a VM disk (legacy in-place attach)", artifact)
		return imagePrepareResult{}, newImageRejection(fmt.Sprintf("prepared image %q", target),
			"the prepared image file is in use as a VM disk (legacy in-place attach by an earlier release); "+
				"create a VMImage with a new name to re-prepare the image")
	}
	log.Printf("INFO ImagePrepare: target image %q already exists in pool %q; nothing to do", artifact, pool)
	return result, nil
}

// stageSource stages the job's source as a finalized image in the pool
// directory: a host path (legacy mode only; confined first, #334) is
// checksummed and converted, a URL is downloaded, checksummed, inspected and
// converted.
func (ip *imagePreparer) stageSource(ctx context.Context, job prepareJob) (stagedImage, error) {
	if job.src.Path == "" {
		return ip.stageFromURL(ctx, job)
	}
	img, err := ip.policy.confine(ctx, ip.host, imagePathRequest{Path: job.src.Path})
	if err != nil {
		return stagedImage{}, err
	}
	if err := ip.verifyChecksum(ctx, img.Path, job.src.Checksum, job.src.ChecksumType); err != nil {
		return stagedImage{}, err
	}
	return ip.convertToStaged(ctx, job.loc.dir, img.Path, img.Format)
}

// stageFromURL downloads the job's URL ON THE LIBVIRT HOST into a private
// staging file (multi-GB images never transit the pod, and never /tmp, which
// is often RAM-backed), verifies its checksum, refuses a header that
// references other host files (#334: convert would flatten them into the
// image), and converts it into a finalized staging image. The download is
// removed whatever the outcome.
func (ip *imagePreparer) stageFromURL(ctx context.Context, job prepareJob) (stagedImage, error) {
	dl, err := ip.download(ctx, job.loc.dir, job.src.URL, job.timeout)
	if err != nil {
		return stagedImage{}, err
	}
	defer removeHostPath(ctx, ip.host, dl, false)

	if err := ip.verifyChecksum(ctx, dl, job.src.Checksum, job.src.ChecksumType); err != nil {
		return stagedImage{}, err
	}
	format, err := inspectHostImage(ctx, ip.host, downloadedImageSubject, dl)
	if err != nil {
		return stagedImage{}, err
	}
	return ip.convertToStaged(ctx, job.loc.dir, dl, format)
}

// stagingTemp creates a private staging file in dir (see the comment above):
// dir/.virtrigaud-imageprepare-<random><suffix>, mode 0600.
func (ip *imagePreparer) stagingTemp(ctx context.Context, dir, suffix string) (string, error) {
	path, err := makeHostTempSuffix(ctx, ip.host, filepath.Join(dir, imagePrepareStagingPrefix+mktempTemplateSuffix), suffix, false)
	if err != nil {
		log.Printf("ERROR ImagePrepare: %v", err)
		return "", contracts.NewRetryableError("could not create a staging file in the storage pool on the libvirt host "+
			"(details are in the provider log)", nil)
	}
	return path, nil
}

// pathPresentScript prints pathExistsMarker when anything — a dangling symlink
// included — occupies "$1" (hostPathExists' [ -e ] follows symlinks).
const pathPresentScript = `if [ -e "$1" ] || [ -L "$1" ]; then echo ` + pathExistsMarker + `; fi`

// hostPathPresent reports whether anything, a symlink included, occupies path
// on the host behind h. A failure to run the check is a retryable error: the
// caller must not assume either answer.
func hostPathPresent(ctx context.Context, h hostCommandRunner, path string) (bool, error) {
	res, err := runHost(ctx, h, "sh", "-c", pathPresentScript, "sh", path)
	if err != nil {
		return false, contracts.NewRetryableError(fmt.Sprintf("check whether %s exists on the host", path), err)
	}
	return strings.TrimSpace(res.Stdout) == pathExistsMarker, nil
}

// curlURLConfig renders the curl config (read by `curl -K -` from stdin) that
// carries rawURL (already validated: no control characters), quoted per
// curl's config syntax, with URL globbing off: `[1-9]` or `{a,b}` in a URL
// would otherwise make one prepare fan out into many requests from the
// hypervisor host.
func curlURLConfig(rawURL string) []byte {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(rawURL)
	return []byte("globoff\nurl = \"" + escaped + "\"\n")
}

// download fetches rawURL into a new private staging file in dir on the host
// and returns its path; on failure nothing is left behind. The URL reaches
// curl on stdin (-K -), never on the command line or in a file, so it is
// neither logged nor visible in the host's process list nor left at rest;
// -q keeps the SSH user's ~/.curlrc out. The transfer is a single one (URL
// globbing off), limited to http, https and ftp (redirects included), and
// bounded in connect time, total time (spec.prepare.timeout) and size.
func (ip *imagePreparer) download(ctx context.Context, dir, rawURL string, timeout time.Duration) (string, error) {
	dl, err := ip.stagingTemp(ctx, dir, downloadStagingSuffix)
	if err != nil {
		return "", err
	}

	log.Printf("INFO ImagePrepare: downloading %s to %s on the libvirt host", redactURL(rawURL), dl)
	res, err := ip.host.runHostStdin(ctx, curlURLConfig(rawURL), "curl", "-q", "-K", "-", "--globoff", "-fsSL",
		"--proto", curlAllowedProtocols, "--proto-redir", curlAllowedProtocols,
		"--connect-timeout", curlConnectTimeoutSeconds,
		"--max-time", strconv.FormatInt(int64(timeout/time.Second), 10),
		"--max-filesize", strconv.FormatInt(imageMaxDownloadBytes(), 10),
		"-w", "%{http_code}", "-o", dl)
	if err != nil {
		removeHostPath(ctx, ip.host, dl, false)
		return "", classifyDownloadFailure(rawURL, res)
	}
	if _, single := parseCurlHTTPCode(res.Stdout); !single {
		removeHostPath(ctx, ip.host, dl, false)
		log.Printf("WARN ImagePrepare: download of %s did not report exactly one transfer (write-out %q); refusing it",
			redactURL(rawURL), res.Stdout)
		return "", contracts.NewInvalidSpecError(errNotOneTransfer, nil)
	}
	return dl, nil
}

// errNotOneTransfer is the requester-facing error of a URL that resolved to
// other than exactly one transfer.
const errNotOneTransfer = "the image source URL must name exactly one file"

// curlHTTPCodeLen is the length of one %{http_code} write-out.
const curlHTTPCodeLen = 3

// parseCurlHTTPCode parses curl's %{http_code} write-out of ONE transfer: ""
// (no transfer reached a server) or exactly three digits. Anything else — the
// codes of several transfers run together, or garbage — is not a single
// transfer (single=false), which the caller treats as permanent (fail closed:
// never retry a fan-out).
func parseCurlHTTPCode(stdout string) (code int, single bool) {
	out := strings.TrimSpace(stdout)
	if out == "" {
		return 0, true
	}
	if len(out) != curlHTTPCodeLen {
		return 0, false
	}
	n, err := strconv.Atoi(out)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// curl exit codes classifyDownloadFailure distinguishes.
const (
	curlExitHTTPError = 22
)

// permanentCurlExits are curl exit codes that retrying the same request cannot
// fix, with the reason reported to the requester.
var permanentCurlExits = map[int]string{
	1:  "the URL's protocol, or a redirect's, is not allowed (http, https or ftp only)",
	3:  "the URL is malformed",
	9:  "access to the remote file was denied",
	60: "the server's TLS certificate could not be verified by the libvirt host",
	63: "the image is larger than this provider's download limit",
	67: "the login was denied",
	78: "the remote file does not exist",
}

// sourceCurlExits are the curl exit codes of a retryable download failure that
// the image source caused: its name did not resolve, it could not be reached,
// it timed out, broke off or truncated the transfer, or (exit 22, see
// isSourceFailure) answered 5xx/408/425/429. Any other retryable failure —
// curl missing, a write error in the pool directory, out of memory, the SSH
// transport — is the libvirt host's.
var sourceCurlExits = map[int]bool{
	6:  true, // the host name did not resolve
	7:  true, // the connection failed
	8:  true, // a weird server reply (FTP)
	16: true, // an HTTP/2 framing error
	18: true, // a partial file: the body ended early
	28: true, // timed out (--connect-timeout or --max-time)
	35: true, // the TLS handshake failed
	47: true, // too many redirects
	52: true, // an empty reply
	55: true, // sending failed
	56: true, // receiving failed
	92: true, // an HTTP/2 stream error
	95: true, // an HTTP/3 error
}

// isSourceFailure reports whether a retryable curl failure (exit code, HTTP
// status of its one transfer) was the image source's (sourceCurlExits, or an
// HTTP 5xx/408/425/429 answer).
func isSourceFailure(exitCode, httpStatus int) bool {
	if exitCode == curlExitHTTPError {
		return httpStatus >= http.StatusInternalServerError || httpStatus == http.StatusRequestTimeout ||
			httpStatus == http.StatusTooEarly || httpStatus == http.StatusTooManyRequests
	}
	return sourceCurlExits[exitCode]
}

// imageSourceFailure marks a retryable ImagePrepare failure the image source
// caused, not the libvirt host or the SSH transport. It wraps (and reads as)
// the retryable ProviderError, so nothing else changes in the provider;
// imagePrepareRPCError sends it as imageartifact.SourceUnavailableError, which
// the manager retries without counting toward the Provider's circuit breaker:
// one tenant's failing image source must not stop the Provider for every
// tenant.
type imageSourceFailure struct{ pe *contracts.ProviderError }

// Error returns the wrapped ProviderError's text.
func (e *imageSourceFailure) Error() string { return e.pe.Error() }

// Unwrap returns the wrapped ProviderError.
func (e *imageSourceFailure) Unwrap() error { return e.pe }

// downloadRetryMessage is the ONLY text a transient download failure exposes
// to the requester: no curl exit code and no HTTP status, so a tenant-chosen
// URL cannot be used to probe what the hypervisor host can reach.
const downloadRetryMessage = "the image download failed on the libvirt host and will be retried " +
	"(details are in the provider log)"

// classifyDownloadFailure turns a failed download (res may be nil) into the
// requester-facing error: InvalidSpec when retrying cannot help (an HTTP 4xx
// other than 408/425/429, a permanentCurlExits code, or a write-out that is
// not exactly one transfer), retryable otherwise (the transport or the host
// failed, HTTP 5xx, DNS/connect/timeout/transfer errors) — an
// imageSourceFailure when the source caused it (isSourceFailure). The URL, the
// exit code and the HTTP status never appear in the retryable error; the log
// carries them, the URL redacted.
func classifyDownloadFailure(rawURL string, res *VirshResult) error {
	if res == nil || res.ExitCode < 0 {
		log.Printf("WARN ImagePrepare: download of %s: the libvirt host could not be reached", redactURL(rawURL))
		return contracts.NewRetryableError(downloadRetryMessage, nil)
	}
	httpStatus, single := parseCurlHTTPCode(res.Stdout)
	log.Printf("WARN ImagePrepare: download of %s failed on the libvirt host (curl exit %d, write-out %q): %s",
		redactURL(rawURL), res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	if !single {
		return contracts.NewInvalidSpecError(errNotOneTransfer, nil)
	}
	if res.ExitCode == curlExitHTTPError && httpStatus >= 400 && httpStatus < 500 &&
		httpStatus != http.StatusRequestTimeout && httpStatus != http.StatusTooEarly && httpStatus != http.StatusTooManyRequests {
		return contracts.NewInvalidSpecError("the image source URL answered with an HTTP client error "+
			"(the file does not exist or may not be downloaded)", nil)
	}
	if reason, ok := permanentCurlExits[res.ExitCode]; ok {
		return contracts.NewInvalidSpecError("the image source URL cannot be downloaded: "+reason, nil)
	}
	if isSourceFailure(res.ExitCode, httpStatus) {
		return &imageSourceFailure{pe: contracts.NewRetryableError(downloadRetryMessage, nil)}
	}
	return contracts.NewRetryableError(downloadRetryMessage, nil)
}

// convertToStaged converts src (probed format srcFormat, passed as -f so
// qemu-img does not re-probe) into a new private staging file in dir as a
// standalone qcow2 and finalizes it (finalizeStaged). On failure the staging
// file is removed; on success the caller removes its name once the artifact is
// published (the artifact keeps its own link).
func (ip *imagePreparer) convertToStaged(ctx context.Context, dir, src, srcFormat string) (stagedImage, error) {
	partial, err := ip.stagingTemp(ctx, dir, convertStagingSuffix)
	if err != nil {
		return stagedImage{}, err
	}
	ok := false
	defer func() {
		if !ok {
			removeHostPath(ctx, ip.host, partial, false)
		}
	}()

	log.Printf("INFO ImagePrepare: converting %q (%s) -> %q (qcow2)", src, srcFormat, partial)
	res, err := runHost(ctx, ip.host, "qemu-img", "convert", "-f", srcFormat, "-O", "qcow2", src, partial)
	if err != nil {
		stderr := ""
		if res != nil {
			stderr = strings.TrimSpace(res.Stderr)
		}
		log.Printf("ERROR ImagePrepare: qemu-img convert %q -> %q failed: %v: %s", src, partial, err, stderr)
		return stagedImage{}, contracts.NewRetryableError("converting the image failed on the libvirt host "+
			"(details are in the provider log)", nil)
	}
	staged, err := ip.finalizeStaged(ctx, partial)
	if err != nil {
		return stagedImage{}, err
	}
	ok = true
	return staged, nil
}

// restoreconScript restores the SELinux label of "$1" when SELinux is enabled
// on the host (selinuxenabled), through non-interactive sudo; on a host
// without SELinux it does nothing.
const restoreconScript = `if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then sudo -n restorecon -- "$1"; fi`

// syncStatScript flushes "$1" to stable storage and prints its inode and size.
const syncStatScript = `sync -- "$1" && stat -c '%i|%s' -- "$1"`

// finalizeStaged makes a converted staging file publishable (ADR-0009 D6):
// read-only (chmod 0444), SELinux-labelled (restorecon when SELinux is
// enabled, best-effort), flushed (sync, so a crash never publishes unwritten
// data), and stat'ed for the inode and size the stamp records. It is NEVER
// chowned: with fs.protected_hardlinks=1 the provider could no longer link a
// file it does not own, and the image only ever needs to be read (Create
// copies it). finalizeClonedDisk (chown + chmod 777) is for per-VM disks only.
func (ip *imagePreparer) finalizeStaged(ctx context.Context, path string) (stagedImage, error) {
	failed := func(step string, err error) (stagedImage, error) {
		log.Printf("ERROR ImagePrepare: %s %q on the libvirt host: %v", step, path, err)
		return stagedImage{}, contracts.NewRetryableError("finalizing the prepared image failed on the libvirt host "+
			"(details are in the provider log)", nil)
	}
	if _, err := runHost(ctx, ip.host, "chmod", preparedImageMode, "--", path); err != nil {
		return failed("chmod", err)
	}
	if _, err := runHost(ctx, ip.host, "sh", "-c", restoreconScript, "sh", path); err != nil {
		log.Printf("WARN ImagePrepare: failed to restore the SELinux context of %q: %v", path, err)
	}
	res, err := runHost(ctx, ip.host, "sh", "-c", syncStatScript, "sh", path)
	if err != nil {
		return failed("sync", err)
	}
	inode, size, err := parseInodeSize(res.Stdout)
	if err != nil {
		return failed("stat", err)
	}
	return stagedImage{path: path, inode: inode, size: size}, nil
}

// parseInodeSize parses "<inode>|<size>" (stat -c '%i|%s').
func parseInodeSize(out string) (uint64, int64, error) {
	inodeStr, sizeStr, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok {
		return 0, 0, fmt.Errorf("unexpected stat output %q", out)
	}
	inode, err := strconv.ParseUint(inodeStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected stat output %q: %w", out, err)
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected stat output %q: %w", out, err)
	}
	return inode, size, nil
}

// verifyChecksum verifies that path hashes to expected with the requested
// algorithm, on the libvirt host (where the file lives). An empty expected
// checksum disables verification. A mismatch or an unsupported algorithm is
// InvalidSpec; a hashing command that could not run is retryable.
func (ip *imagePreparer) verifyChecksum(ctx context.Context, path, expected, checksumType string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	tool, known := checksumTool(checksumType)
	if !known {
		return contracts.NewInvalidSpecError(
			fmt.Sprintf("unsupported checksum type %q (want md5/sha1/sha256/sha512)", checksumType), nil)
	}

	log.Printf("INFO ImagePrepare: verifying %s of %q", tool, path)
	res, err := runHost(ctx, ip.host, tool, "--", path)
	if err != nil {
		stderr := ""
		if res != nil {
			stderr = res.Stderr
		}
		return contracts.NewRetryableError(
			fmt.Sprintf("compute %s of %q: %s", tool, path, strings.TrimSpace(stderr)), nil)
	}

	// coreutils *sum output: "<hex>  <path>".
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return contracts.NewRetryableError(fmt.Sprintf("compute %s of %q: empty output", tool, path), nil)
	}
	got := fields[0]
	if !strings.EqualFold(got, strings.TrimSpace(expected)) {
		// The computed hash goes to the provider log only.
		log.Printf("WARN ImagePrepare: checksum mismatch for %q: expected %s, got %s", path, expected, got)
		return contracts.NewInvalidSpecError(
			"the image source does not match its checksum (source.libvirt.checksum)", nil)
	}
	log.Printf("INFO ImagePrepare: checksum OK for %q", path)
	return nil
}

// refreshPool asks libvirt to rescan pool so a newly published image is listed
// as a volume (best-effort; dotfiles are never listed).
func (ip *imagePreparer) refreshPool(ctx context.Context, pool string) {
	if _, err := ip.host.runVirshCommand(ctx, "pool-refresh", "--pool", pool); err != nil {
		log.Printf("WARN ImagePrepare: failed to refresh storage pool %q: %v", pool, err)
	}
}

// noHardLinksError is the permanent failure of a pool whose filesystem cannot
// hold hard links, which publishing requires (ADR-0009 D6).
func noHardLinksError(pool string) error {
	return contracts.NewInvalidSpecError(fmt.Sprintf("storage pool %q is on a filesystem without hard links; "+
		"image preparation publishes an image atomically with a hard link and cannot use this pool", pool), nil)
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
