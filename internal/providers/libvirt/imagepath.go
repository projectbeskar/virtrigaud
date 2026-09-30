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
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Image-path confinement (security).
//
// A libvirt image path reaches this provider from tenant-writable objects:
// VMImage.spec.source.libvirt.path, VirtualMachine.spec.importedDisk.path, and
// (via the controller) VMImage.status.providerStatus[].path. Unconfined, such a
// path lets whoever can create those objects boot a VM from ANY file the SSH
// user can read on the hypervisor host — /etc/shadow, a host block device, or
// another tenant's live VM disk — and read it from inside the guest; a value
// beginning with "-" could also be read as an option by the tool it reaches.
//
// Every such path is therefore confined ON THE TARGET HOST before any use:
//
//  1. Lexical pre-check (validateImagePathSyntax): absolute, no ".." segment, no
//     segment starting with "-", no control characters — the same shape the CRD
//     admits (v1beta1.LibvirtImagePathPattern).
//  2. Canonicalization on the host (`realpath -e -- <path>`), so a symlink or a
//     ".." hidden behind one cannot escape; everything after uses the canonical
//     path, never the caller's string.
//  3. The canonical path must be DIRECTLY inside an allowed image directory
//     (VIRTRIGAUD_LIBVIRT_IMAGE_DIRS, default /var/lib/libvirt/images, itself
//     canonicalized on the host) — no subdirectories, so e.g. the cloud-init seed
//     directory below the default pool is out of reach.
//  4. The file name must not be a VirtRigaud-managed artifact: a VM disk
//     (<vm>-disk.qcow2), an imported migration disk (<vm>-migrated.qcow2),
//     staging/temporary files, or cloud-init seeds.
//  5. It must be a regular, non-empty file (not a device, FIFO, directory).
//  6. It must not be a disk (or backing file, or shared directory) of ANY domain
//     defined on the host — VirtRigaud's or anyone else's (shared hosts). Each
//     disk's backing chain is read with `qemu-img info -U` (through `sudo -n`
//     when allowed), because dumpxml omits <backingStore> for shut-off
//     domains; a running domain's chain is taken from its live <backingStore>.
//  7. Its header must not reference other files: no qcow2 backing file or
//     external data file, no VMDK extent outside itself. `qemu-img convert`
//     would otherwise read (and flatten) those files, re-opening the escape.
//
// A base image passing these checks is always COPIED into the VM's own disk.
// The one exception is a migration's imported disk: VirtualMachine.spec.
// importedDisk with the file named exactly <vm name>-migrated.qcow2 directly in
// the target pool's directory, not used by any domain — only that may be
// attached in place, because it IS this VM's disk.
//
// Rejections are sdk InvalidSpec errors (gRPC InvalidArgument, non-retryable),
// so the manager records a ValidationError condition and backs off instead of
// hot-retrying. Host-side failures of the checks themselves are a generic
// retryable error; their detail (command lines naming other domains' disks and
// the allowed directories) is logged provider-side only (hostCheckFailed). Messages carry the caller's own path only — never the
// canonical target of a symlink, the allowed directories, or another VM's name
// — and host-side "does not exist" is reported only for paths inside an
// allowed directory, so the check is not an existence oracle for arbitrary host
// files.

// EnvImageDirs names the provider-pod environment variable listing the
// directories a libvirt image path may resolve into. Entries are absolute
// directories separated by ',' or ':'; unset or blank selects DefaultImageDir.
// Set it through the Provider's spec.runtime.env.
const EnvImageDirs = "VIRTRIGAUD_LIBVIRT_IMAGE_DIRS"

// DefaultImageDir is the only allowed image directory when EnvImageDirs is unset
// or blank: libvirt's stock image directory, which is also the directory of the
// "default" storage pool this provider creates VM disks and prepared images in.
const DefaultImageDir = "/var/lib/libvirt/images"

// Names VirtRigaud gives the files it manages inside a pool directory. They are
// reserved: a base image path may never name one (see reservedImageName).
const (
	// qcow2Ext is the extension of every qcow2 volume VirtRigaud writes.
	qcow2Ext = ".qcow2"
	// vmDiskVolumeSuffix forms a VM's primary disk volume name, <vm>-disk
	// (Create, Clone), stored as <vm>-disk.qcow2.
	vmDiskVolumeSuffix = "-disk"
	// hiddenFilePrefix marks staging files (.virtrigaud-imageprepare-*,
	// .virtrigaud-export-*) and any other dotfile.
	hiddenFilePrefix = "."
)

// reservedImageSuffixes are file-name endings of VirtRigaud-managed artifacts
// that must never be used as a base image: VM disks, imported migration disks,
// cloud-init seed ISOs, and host-side download/staging temporaries.
var reservedImageSuffixes = []string{
	vmDiskVolumeSuffix + qcow2Ext,               // <vm>-disk.qcow2
	vmDiskVolumeSuffix,                          // legacy <vm>-disk
	contracts.ImportedDiskNameSuffix + qcow2Ext, // <vm>-migrated.qcow2
	"cloud-init.iso", "-cidata.iso",             // cloud-init seeds
	".download", "-temp.img", "-download.img", ".partial", // staging
}

// forbiddenImageDirRoots are host directories that can never be (or contain) an
// allowed image directory, whether configured via EnvImageDirs or reached by a
// configured directory that is a symlink: system configuration and secrets,
// pseudo-filesystems and devices, binaries, logs, libvirt's own per-domain state
// (NVRAM, saves, channels), VirtRigaud's cloud-init seed directory, temporary
// directories (VirtRigaud stages domain XML and cloud-init data under /tmp), and
// container/kubelet state on hosts that are also Kubernetes nodes.
var forbiddenImageDirRoots = []string{
	"/etc", "/proc", "/sys", "/dev", "/boot", "/root", "/run", "/var/run",
	"/tmp", "/var/tmp", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/var/log", "/var/lib/libvirt/qemu", DefaultImageDir + "/cloud-init",
	"/var/lib/kubelet", "/var/lib/k0s", "/var/lib/rancher", "/var/lib/containerd", "/var/lib/docker",
}

// allowedSourceImageFormats are the image formats (as probed by qemu-img on the
// host) accepted as a base image. Formats that can reference other files are
// either excluded (qed, qcow v1, parallels, ...) or inspected (qcow2 backing
// and data files, VMDK extents) by checkImageHeader.
var allowedSourceImageFormats = map[string]bool{
	"qcow2": true, "raw": true, "vmdk": true, "vpc": true, "vhdx": true, "vdi": true,
}

// libvirtImagePathRE is the provider-side copy of the CRD admission pattern.
var libvirtImagePathRE = regexp.MustCompile(v1beta1.LibvirtImagePathPattern)

// domainUUIDRE matches the libvirt domain UUIDs `virsh list --uuid` prints.
var domainUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// realpathMissingExitCode is GNU realpath's exit status for a path that does
// not exist (or cannot be resolved); other non-zero codes (126/127: tool not
// runnable/missing) are host problems, not a bad path.
const realpathMissingExitCode = 1

// qemuImgFailureExitCode is qemu-img's exit status when it cannot open or
// parse the image; other non-zero codes (126/127) mean the tool is unusable.
const qemuImgFailureExitCode = 1

// statRegularFile is GNU stat's %F rendering of a non-empty regular file.
const statRegularFile = "regular file"

// hostCommandRunner runs a virsh control command, or a host command when the
// first argument is "!", on ONE hypervisor host. *VirshProvider implements it;
// the confinement always runs against the host that will consume the image (the
// leased target host in clustered mode, ADR-0007), never a default host.
type hostCommandRunner interface {
	runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error)
}

// imagePathPolicy is the provider's image-path confinement policy.
type imagePathPolicy struct {
	// dirs are the allowed image directories as configured (absolute, lexically
	// clean). They are canonicalized on the target host at check time.
	dirs []string
}

// imagePathRequest is one image path to confine.
type imagePathRequest struct {
	// Path is the caller-supplied path (untrusted).
	Path string
	// ImportedDisk marks Path as a disk imported for this VM
	// (contracts.VMImage.ImportedDisk), which may be attached in place when it
	// is this VM's own imported volume.
	ImportedDisk bool
	// VMName is the VM being created; its imported volume is
	// <VMName>contracts.ImportedDiskNameSuffix.qcow2.
	VMName string
	// PoolDir is the directory of the storage pool the VM's disks are created
	// in, the only place an imported disk is attached in place from.
	PoolDir string
	// Clustered, set by a clustered provider's create, applies two rules the
	// single host does not need (ADR-0007 A6.1 security review):
	//   - Uniform: every refusal that depends on whether a file exists or
	//     what it is — missing, a reserved (VirtRigaud-managed) name, not a
	//     regular file, in use by a domain, a header it may not have — is the
	//     same "not allowed" answer, so a refused VMImage path tells a tenant
	//     much less about the hosts' storage. It is not a guarantee: a path
	//     that is ACCEPTED still shows that a usable image is there (and is
	//     copied into the tenant's VM), so an allowed image directory must not
	//     be one the tenant should not read (see EnvImageDirs, and keep it
	//     apart from a shared storage pool);
	//   - UsedElsewhere, when non-nil, is asked about a base image (never an
	//     imported disk attached in place, which the create's disk guard
	//     checks) after the host-local in-use check: whether a domain on ANY
	//     other host of the Provider uses it — copying another host's live
	//     disk would hand its content to this VM's tenant. It fails closed
	//     (an error) when a host cannot be checked.
	Clustered     bool
	UsedElsewhere func(ctx context.Context, h hostCommandRunner, raw, canonical string) (bool, error)
}

// confinedImage is an image path that passed confinement.
type confinedImage struct {
	// Path is the canonical path on the host. Callers must use it (not the
	// caller-supplied string) for every subsequent operation.
	Path string
	// Format is the image format qemu-img probed on the host; pass it as
	// `qemu-img convert -f` so the tool does not re-probe.
	Format string
	// AdoptInPlace is true only for this VM's own imported disk: attach it as
	// the VM's disk instead of copying it.
	AdoptInPlace bool
}

// newImageRejection builds the non-retryable, InvalidArgument-coded rejection
// of an image; subject names it for the message (e.g. `libvirt image path
// "/x"`). It crosses the gRPC boundary as codes.InvalidArgument (the sdk error
// implements GRPCStatus, which status.FromError finds through any fmt.Errorf
// wrapping), which the manager maps to contracts.ErrorTypeInvalidSpec.
func newImageRejection(subject, reason string) error {
	return sdkerrors.NewInvalidSpec("%s", subject+" rejected: "+reason)
}

// imagePathSubject names a caller-supplied image path in a rejection message.
func imagePathSubject(path string) string {
	return fmt.Sprintf("libvirt image path %q", path)
}

// downloadedImageSubject names a host-side download in a rejection message.
// The URL is deliberately not echoed (it may embed credentials).
const downloadedImageSubject = "downloaded image"

// importedImageSubject names a migration import's staged disk in a rejection
// message (the source URL is not echoed).
const importedImageSubject = "imported disk"

// newImagePathError is newImageRejection for a caller-supplied image path.
func newImagePathError(path, reason string) error {
	return newImageRejection(imagePathSubject(path), reason)
}

// isInvalidArgument reports whether err carries (possibly wrapped) a gRPC
// InvalidArgument status — the provider's non-retryable rejection class.
func isInvalidArgument(err error) bool {
	return err != nil && status.Code(err) == codes.InvalidArgument
}

// isControlRune reports C0 controls (including NUL), DEL and C1 controls.
func isControlRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// validateImagePathSyntax is the pre-resolve (lexical) check applied to every
// image path before it is sent to the host. It returns a human-readable reason,
// or nil. It mirrors v1beta1.LibvirtImagePathPattern, with specific messages for
// the common mistakes, and the pattern itself as the final catch-all.
func validateImagePathSyntax(p string) error {
	switch {
	case p == "":
		return errors.New("path is empty")
	case len(p) > v1beta1.LibvirtImagePathMaxLength:
		return fmt.Errorf("path is longer than %d bytes", v1beta1.LibvirtImagePathMaxLength)
	case !utf8.ValidString(p):
		return errors.New("path is not valid UTF-8")
	case strings.HasPrefix(p, "-"):
		return errors.New("path must not begin with '-'")
	case !strings.HasPrefix(p, "/"):
		return errors.New("path must be absolute")
	case strings.ContainsFunc(p, isControlRune):
		return errors.New("path must not contain control characters")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("path must not contain a '..' segment")
		}
		if strings.HasPrefix(seg, "-") {
			return errors.New("no path segment may begin with '-'")
		}
	}
	if !libvirtImagePathRE.MatchString(p) {
		return errors.New("path does not match the allowed image path pattern")
	}
	return nil
}

// isForbiddenImageDir reports whether dir is "/" or lies at or below one of the
// forbiddenImageDirRoots. dir must be absolute and clean.
func isForbiddenImageDir(dir string) bool {
	if dir == "/" {
		return true
	}
	for _, root := range forbiddenImageDirRoots {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return true
		}
	}
	return false
}

// parseImageDirs parses an EnvImageDirs value: absolute directories separated by
// ',' or ':' (surrounding whitespace ignored, duplicates dropped). A blank value
// yields [DefaultImageDir]. Any malformed or forbidden entry is an error — the
// provider refuses to start rather than run with a policy it did not intend.
func parseImageDirs(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ':' })
	dirs := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		d := strings.TrimSpace(f)
		if d == "" {
			continue
		}
		if trimmed := strings.TrimRight(d, "/"); trimmed != "" {
			d = trimmed // "/srv/images/" is a directory spelling, not a new path
		}
		if err := validateImagePathSyntax(d); err != nil {
			return nil, fmt.Errorf("image directory %q: %w", d, err)
		}
		d = filepath.Clean(d)
		if isForbiddenImageDir(d) {
			return nil, fmt.Errorf("image directory %q is a system directory and cannot hold images", d)
		}
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return []string{DefaultImageDir}, nil
	}
	return dirs, nil
}

// imageDirsFromEnv reads the allowed image directories from EnvImageDirs.
func imageDirsFromEnv() ([]string, error) {
	dirs, err := parseImageDirs(os.Getenv(EnvImageDirs))
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", EnvImageDirs, err)
	}
	return dirs, nil
}

// reservedImageName reports whether base (a file name) is a VirtRigaud-managed
// artifact name that must never be used as a base image.
func reservedImageName(base string) bool {
	if strings.HasPrefix(base, hiddenFilePrefix) {
		return true
	}
	for _, suffix := range reservedImageSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// importedVolumeFileName is the file name of the disk a VMMigration imports for
// vmName: <vm>-migrated.qcow2.
func importedVolumeFileName(vmName string) string {
	return vmName + contracts.ImportedDiskNameSuffix + qcow2Ext
}

// vmDiskVolumeName is the volume name of vmName's primary disk: <vm>-disk.
func vmDiskVolumeName(vmName string) string {
	return vmName + vmDiskVolumeSuffix
}

// runHost runs a host command ("!"-prefixed) through h.
func runHost(ctx context.Context, h hostCommandRunner, argv ...string) (*VirshResult, error) {
	return h.runVirshCommand(ctx, append([]string{"!"}, argv...)...)
}

// splitNULList splits NUL-terminated command output (realpath -z) into fields.
func splitNULList(s string) []string {
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// canonicalizeOnHost resolves each path on the host with `realpath -m -z`
// (symlinks and ".." resolved; missing components allowed, so the call never
// fails on a path that does not exist). Output order matches input order.
func canonicalizeOnHost(ctx context.Context, h hostCommandRunner, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	res, err := runHost(ctx, h, append([]string{"realpath", "-m", "-z", "--"}, paths...)...)
	if err != nil {
		return nil, hostCheckFailed("canonicalize paths", err)
	}
	out := splitNULList(res.Stdout)
	if len(out) != len(paths) {
		return nil, hostCheckFailed("canonicalize paths",
			fmt.Errorf("got %d results for %d paths", len(out), len(paths)))
	}
	return out, nil
}

// inUseRejectionReason explains why a path that some domain uses is refused,
// with the one remedy that applies to an image an earlier release attached in
// place: prepare it again under a new VMImage name.
const inUseRejectionReason = "it is a disk (or backing file) of an existing VM on the host and cannot be used as " +
	"an image; if it is a prepared image that an earlier release attached in place as a VM disk, create a " +
	"VMImage with a new name to prepare a fresh copy"

// hostCheckFailedMessage is the ONLY text a failed host-side check exposes to
// the caller. The underlying error carries the full host command line — every
// domain's disk paths, the allowed directories, other domains' UUIDs — and
// would otherwise travel through gRPC into a tenant-visible VM condition.
const hostCheckFailedMessage = "could not verify the image on the libvirt host " +
	"(transient host error; details are in the provider log)"

// hostCheckFailed logs a host-side check failure in full, provider-side only,
// and returns a generic retryable error whose message carries none of it. The
// failure stays reachable through errors.As/Is (hiddenCauseError), so a host
// that could not be reached is still recognized as such
// (isHostTransportFailure).
func hostCheckFailed(what string, err error) error {
	log.Printf("ERROR libvirt image confinement: %s: %v", what, err)
	return &hiddenCauseError{visible: contracts.NewRetryableError(hostCheckFailedMessage, nil), cause: err}
}

// hiddenCauseError is visible — its message and its contracts error — with
// cause attached for classification only: Error never includes it, and
// errors.As/Is reach it after visible.
type hiddenCauseError struct {
	visible error
	cause   error
}

// Error returns the visible error's message only.
func (e *hiddenCauseError) Error() string { return e.visible.Error() }

// Unwrap exposes the visible error, then the hidden cause.
func (e *hiddenCauseError) Unwrap() []error { return []error{e.visible, e.cause} }

// confine applies the full image-path confinement (see the file comment) to req
// on the host behind h and returns the canonical, checked image. Every
// rejection is a newImagePathError; host/transport failures are retryable.
func (pol imagePathPolicy) confine(ctx context.Context, h hostCommandRunner, req imagePathRequest) (confinedImage, error) {
	if err := validateImagePathSyntax(req.Path); err != nil {
		return confinedImage{}, newImagePathError(req.Path, err.Error())
	}
	if len(pol.dirs) == 0 {
		return confinedImage{}, newImagePathError(req.Path, "no image directories are allowed on this provider")
	}

	// Canonicalize the allowed directories (and the pool directory for an
	// imported disk) on THIS host: /var/lib/libvirt/images may itself be a
	// symlink, and a configured directory must not smuggle in a system one.
	lookup := append([]string(nil), pol.dirs...)
	poolIdx := -1
	if req.ImportedDisk && req.PoolDir != "" {
		poolIdx = len(lookup)
		lookup = append(lookup, req.PoolDir)
	}
	canon, err := canonicalizeOnHost(ctx, h, lookup)
	if err != nil {
		return confinedImage{}, err
	}
	allowed := make(map[string]bool, len(pol.dirs))
	for i := range pol.dirs {
		if isForbiddenImageDir(canon[i]) {
			log.Printf("WARN image directory %q resolves to a system directory on this host; ignoring it", pol.dirs[i])
			continue
		}
		allowed[canon[i]] = true
	}
	poolDir := ""
	if poolIdx >= 0 && !isForbiddenImageDir(canon[poolIdx]) {
		poolDir = canon[poolIdx]
	}

	notAllowed := newImagePathError(req.Path,
		"it does not resolve to a file directly inside an allowed image directory "+
			"(configure "+EnvImageDirs+" on the provider)")

	// Resolve the image itself. Existence is only disclosed for paths the
	// caller aimed at an allowed directory; anything else gets the same
	// "not allowed" answer whether or not it exists on the host.
	res, err := runHost(ctx, h, "realpath", "-e", "-z", "--", req.Path)
	if err != nil {
		if res == nil || res.ExitCode != realpathMissingExitCode {
			return confinedImage{}, hostCheckFailed("resolve image path", err)
		}
		lexParent := filepath.Dir(filepath.Clean(req.Path))
		if !req.Clustered && (allowed[lexParent] || containsString(pol.dirs, lexParent) || (poolDir != "" && lexParent == poolDir)) {
			return confinedImage{}, newImagePathError(req.Path, "it does not exist on the libvirt host")
		}
		if req.Clustered {
			log.Printf("WARN rejected libvirt image path %q: it does not exist on the host", req.Path)
		}
		return confinedImage{}, notAllowed
	}
	resolved := splitNULList(res.Stdout)
	if len(resolved) != 1 {
		return confinedImage{}, hostCheckFailed("resolve image path", errors.New("unexpected realpath output"))
	}
	canonical := resolved[0]
	if validateImagePathSyntax(canonical) != nil {
		// The host-side name itself is unusable (control characters, a "-"
		// segment, ...); never pass it on to other tools.
		return confinedImage{}, notAllowed
	}
	parent, base := filepath.Dir(canonical), filepath.Base(canonical)

	adopt := req.ImportedDisk && req.VMName != "" && poolDir != "" &&
		parent == poolDir && base == importedVolumeFileName(req.VMName)
	if !adopt {
		if !allowed[parent] {
			log.Printf("WARN rejected libvirt image path %q: canonical path is outside the allowed image directories %v",
				req.Path, pol.dirs)
			return confinedImage{}, notAllowed
		}
		if reservedImageName(base) {
			if req.Clustered {
				log.Printf("WARN rejected libvirt image path %q: it names a VirtRigaud-managed file", req.Path)
				return confinedImage{}, notAllowed
			}
			return confinedImage{}, newImagePathError(req.Path,
				"it names a VirtRigaud-managed file (a VM disk, an imported migration disk, a cloud-init seed, "+
					"or a staging file), which cannot be used as a base image")
		}
	}

	if err := checkRegularFile(ctx, h, req.Path, canonical); err != nil {
		if req.Clustered && isInvalidArgument(err) {
			log.Printf("WARN rejected libvirt image path %q: %v", req.Path, err)
			return confinedImage{}, notAllowed
		}
		return confinedImage{}, err
	}
	inUse, err := diskSourcesInUse(ctx, h)
	if err != nil {
		return confinedImage{}, err
	}
	if inUse.contains(canonical) {
		log.Printf("WARN rejected libvirt image path %q: it is in use by a domain on the host", req.Path)
		if req.Clustered {
			return confinedImage{}, notAllowed
		}
		return confinedImage{}, newImagePathError(req.Path, inUseRejectionReason)
	}
	if !adopt && req.UsedElsewhere != nil {
		used, err := req.UsedElsewhere(ctx, h, req.Path, canonical)
		if err != nil {
			return confinedImage{}, err
		}
		if used {
			log.Printf("WARN rejected libvirt image path %q: a domain on another host of the Provider uses it", req.Path)
			return confinedImage{}, notAllowed
		}
	}
	format, err := inspectHostImage(ctx, h, imagePathSubject(req.Path), canonical)
	if err != nil {
		// A header the image may not have (a backing file, an external data
		// file, an unsupported format) says what the file is: on a clustered
		// Provider it is the same "not allowed" answer too.
		if req.Clustered && isInvalidArgument(err) {
			log.Printf("WARN rejected libvirt image path %q: %v", req.Path, err)
			return confinedImage{}, notAllowed
		}
		return confinedImage{}, err
	}
	return confinedImage{Path: canonical, Format: format, AdoptInPlace: adopt}, nil
}

// containsString reports whether list contains s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// checkRegularFile requires canonical to be a non-empty regular file on the host
// (not a block/character device, FIFO, socket or directory). displayPath is the
// caller's path, used in the rejection message.
func checkRegularFile(ctx context.Context, h hostCommandRunner, displayPath, canonical string) error {
	res, err := runHost(ctx, h, "stat", "-c", "%F", "--", canonical)
	if err != nil {
		return hostCheckFailed("stat image", err)
	}
	if kind := strings.TrimSpace(res.Stdout); kind != statRegularFile {
		return newImagePathError(displayPath, fmt.Sprintf("it is not a non-empty regular file (found: %s)", kind))
	}
	return nil
}

// qemuImgInfo is the subset of `qemu-img info --output=json` checkImageHeader
// and the backing-chain walk need to find references to other files.
type qemuImgInfo struct {
	Filename            string `json:"filename"`
	Format              string `json:"format"`
	BackingFilename     string `json:"backing-filename"`
	FullBackingFilename string `json:"full-backing-filename"`
	// BackingFilenameFormat is the backing file's format as the header
	// names it (empty: probed).
	BackingFilenameFormat string `json:"backing-filename-format"`
	FormatSpecific        *struct {
		Data struct {
			DataFile string `json:"data-file"`
			Extents  []struct {
				Filename string `json:"filename"`
			} `json:"extents"`
		} `json:"data"`
	} `json:"format-specific"`
}

// referencedFiles returns every host file this image consists of or points at:
// itself, its backing file, a qcow2 external data file, and VMDK extents.
func (i qemuImgInfo) referencedFiles() []string {
	refs := make([]string, 0, 4)
	for _, f := range []string{i.Filename, i.FullBackingFilename, i.BackingFilename} {
		if strings.HasPrefix(f, "/") {
			refs = append(refs, f)
		}
	}
	if i.FormatSpecific != nil {
		if strings.HasPrefix(i.FormatSpecific.Data.DataFile, "/") {
			refs = append(refs, i.FormatSpecific.Data.DataFile)
		}
		for _, e := range i.FormatSpecific.Data.Extents {
			if strings.HasPrefix(e.Filename, "/") {
				refs = append(refs, e.Filename)
			}
		}
	}
	return refs
}

// inspectHostImage runs `qemu-img info --output=json` on path (canonical, or a
// VirtRigaud-chosen staging file) on the host and applies checkImageHeader. It
// returns the probed format. subject names the image in rejection messages
// (imagePathSubject, downloadedImageSubject or importedImageSubject).
func inspectHostImage(ctx context.Context, h hostCommandRunner, subject, path string) (string, error) {
	return inspectHostImageAs(ctx, h, subject, path, "")
}

// inspectHostImageAs is inspectHostImage with the format pinned (`qemu-img info
// -f format`) — for callers whose convert forces a source format, so the
// header is read exactly as the convert will read it. An empty format probes.
func inspectHostImageAs(ctx context.Context, h hostCommandRunner, subject, path, format string) (string, error) {
	argv := []string{"qemu-img", "info", "--output=json"}
	if format != "" {
		argv = append(argv, "-f", format)
	}
	res, err := runHost(ctx, h, append(argv, "--", path)...)
	if err != nil {
		if res != nil && res.ExitCode == qemuImgFailureExitCode {
			return "", newImageRejection(subject, "qemu-img cannot read it as a disk image")
		}
		return "", hostCheckFailed("inspect image", err)
	}
	var info qemuImgInfo
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		return "", hostCheckFailed("parse qemu-img info output", err)
	}
	if reason := checkImageHeader(info, path); reason != "" {
		return "", newImageRejection(subject, reason)
	}
	return info.Format, nil
}

// checkImageHeader returns a rejection reason when an image references any file
// other than itself — a qcow2/vmdk backing file, a qcow2 external data file, or
// a VMDK extent elsewhere — or has a format outside allowedSourceImageFormats;
// "" when it is self-contained. qemu-img convert would otherwise read (and
// flatten into the VM disk) whatever those references point at.
func checkImageHeader(info qemuImgInfo, canonical string) string {
	if !allowedSourceImageFormats[info.Format] {
		return fmt.Sprintf("image format %q is not supported for base images", info.Format)
	}
	if info.BackingFilename != "" || info.FullBackingFilename != "" {
		return "the image has a backing file; base images must be self-contained (flatten it with qemu-img convert)"
	}
	if info.FormatSpecific == nil {
		return ""
	}
	if info.FormatSpecific.Data.DataFile != "" {
		return "the image uses an external data file; base images must be self-contained"
	}
	for _, e := range info.FormatSpecific.Data.Extents {
		if e.Filename != "" && e.Filename != canonical {
			return "the image references extent files outside itself; base images must be self-contained"
		}
	}
	return ""
}

// inUseSet is the set of host paths currently referenced by any domain: disk
// and backing-chain sources, other file-backed devices, firmware/kernel files,
// and (as prefixes) shared directories. All entries are canonical.
type inUseSet struct {
	files map[string]bool
	dirs  []string
}

// contains reports whether canonical is referenced, or lies under a directory
// shared into a domain.
func (s inUseSet) contains(canonical string) bool {
	if s.files[canonical] {
		return true
	}
	for _, d := range s.dirs {
		if canonical == d || strings.HasPrefix(canonical, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// domainPathRefs are the host paths one domain definition references.
type domainPathRefs struct {
	files   []string
	dirs    []string
	disks   []string    // file/dev sources inside <disk> (subject to the backing-chain walk)
	volumes [][2]string // {pool, volume} of type='volume' disks
}

// pathTextElements are domain XML elements whose text content is a host file.
var pathTextElements = map[string]bool{"nvram": true, "kernel": true, "initrd": true, "loader": true, "dtb": true}

// parseDomainPathRefs extracts every host path a domain XML references:
// <source file|dev|path=...> anywhere (disks, backing stores, char devices),
// <source dir=...> (filesystem passthrough), <source pool= volume=> (volume
// disks, resolved later), and the text of firmware/kernel elements. Sources
// inside a <disk> are additionally returned as disks, whose backing chains the
// caller walks (never char devices or sockets, which qemu-img must not open).
// Over-inclusion is harmless: it can only make the in-use check stricter.
func parseDomainPathRefs(domainXML string) (domainPathRefs, error) {
	var refs domainPathRefs
	dec := xml.NewDecoder(strings.NewReader(domainXML))
	capture := false
	diskDepth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return refs, nil
		}
		if err != nil {
			return domainPathRefs{}, fmt.Errorf("parse domain XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			capture = pathTextElements[t.Name.Local]
			if t.Name.Local == "disk" {
				diskDepth++
			}
			if t.Name.Local != "source" {
				continue
			}
			var pool, volume string
			for _, a := range t.Attr {
				switch a.Name.Local {
				case "file", "dev", "path":
					if a.Value != "" {
						refs.files = append(refs.files, a.Value)
						if diskDepth > 0 && a.Name.Local != "path" {
							refs.disks = append(refs.disks, a.Value)
						}
					}
				case "dir":
					if a.Value != "" {
						refs.dirs = append(refs.dirs, a.Value)
					}
				case "pool":
					pool = a.Value
				case "volume":
					volume = a.Value
				}
			}
			if pool != "" && volume != "" {
				refs.volumes = append(refs.volumes, [2]string{pool, volume})
			}
		case xml.CharData:
			if capture {
				if v := strings.TrimSpace(string(t)); v != "" {
					refs.files = append(refs.files, v)
				}
			}
		case xml.EndElement:
			capture = false
			if t.Name.Local == "disk" && diskDepth > 0 {
				diskDepth--
			}
		}
	}
}

// listDomainUUIDs returns the UUIDs of every domain defined on the host
// (running or not). Any unexpected output fails the check.
func listDomainUUIDs(ctx context.Context, h hostCommandRunner) ([]string, error) {
	res, err := h.runVirshCommand(ctx, "list", "--all", "--uuid")
	if err != nil {
		return nil, hostCheckFailed("list domains", err)
	}
	var uuids []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		uuid := strings.TrimSpace(line)
		if uuid == "" {
			continue
		}
		if !domainUUIDRE.MatchString(uuid) {
			return nil, hostCheckFailed("list domains", fmt.Errorf("unexpected output line %q", uuid))
		}
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

// maxBackingChainDepth bounds the backing-chain walk: a longer chain (or a
// loop) fails the check closed.
const maxBackingChainDepth = 32

// qemuImgMissingFile is the qemu-img error text for a file that does not exist.
const qemuImgMissingFile = "No such file or directory"

// backingFormatRE matches a qemu block-driver name as a header's
// backing-filename-format, passed to qemu-img as -f.
var backingFormatRE = regexp.MustCompile(`^[a-z0-9]+$`)

// What backingKindScript reports about a backing file path.
const (
	backingKindFile  = "file"
	backingKindOther = "other"
)

// backingKindScript is the fixed `sh -c` script behind checkChainFileKind.
// The path is "$1", never interpolated into the text. It prints
// backingKindFile for a regular file (a symbolic link is followed),
// backingKindOther for anything else that exists (a device, FIFO, socket,
// directory), and nothing when the SSH user cannot see it.
const backingKindScript = `if [ -f "$1" ]; then echo ` + backingKindFile + `; elif [ -e "$1" ]; then echo ` +
	backingKindOther + `; fi`

// backingChainFiles returns every host file in disk's image chain: the disk,
// each backing file, qcow2 data files and VMDK extents. It is needed because
// `virsh dumpxml` omits <backingStore> for a shut-off domain, so a stopped VM's
// base images would otherwise look unused. It reads with `qemu-img info -U`
// (force-share: a read-only inspection that must work on running VMs' images;
// conversions never use -U), through passwordless sudo when the host allows it
// (qemuImgInfoOnHost).
//
// The chain is walked one image at a time — never `--backing-chain`, which
// would have qemu-img open, as root, whatever backing name each header holds.
// Every image — the disk itself too — must be a regular file before qemu-img
// opens it (checkChainFileKind: a device or FIFO is refused at once). A
// backing file is followed only when it is an absolute local path
// (full-backing-filename), opened with the format its parent's header names;
// a protocol (nbd:, http:, ...), json: or relative backing name, a device,
// FIFO or other non-regular file, an unknown backing format and a chain deeper
// than maxBackingChainDepth fail the check (closed), as does any unreadable
// image. A qcow2 external data file is recorded, never opened by the walk; one
// named by anything but an absolute local path fails the check too. A file
// that no longer exists ends the chain: a missing disk contributes nothing.
//
// The disk itself is opened in format, the one its domain definition names
// (walkBackingChainFrom): never probed, so a guest cannot make its raw disk
// read as a qcow2 image naming another file as its backing file — and a raw
// disk's chain is not walked at all. An empty format (a source the
// definition gives none for, such as a nested backing store) is probed — by
// the SSH user only (walkBackingChainFrom: root opens only a qcow2 or raw
// image in a format a trusted header or the definition names).
func backingChainFiles(ctx context.Context, h hostCommandRunner, disk, format string) ([]string, error) {
	levels, err := walkBackingChainFrom(ctx, h, disk, format)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, l := range levels {
		refs = append(refs, l.path)
		refs = append(refs, l.refs...)
	}
	return refs, nil
}

// backingLevel is one image of a backing chain (walkBackingChain): its path
// as named (disk, then each full-backing-filename), every host file it
// consists of or points at (qemuImgInfo.referencedFiles), and the format it
// was opened as — the one its parent's header names (the caller's for the
// disk itself), or "" when qemu-img had to probe it — and the external data
// file its header names, if any.
type backingLevel struct {
	path     string
	refs     []string
	format   string
	dataFile string
}

// walkBackingChain reads disk's image chain one image at a time, top first,
// under the rules backingChainFiles documents. The disk's own format is
// probed.
func walkBackingChain(ctx context.Context, h hostCommandRunner, disk string) ([]backingLevel, error) {
	return walkBackingChainFrom(ctx, h, disk, "")
}

// rawDiskFormat is qemu's raw format: no header, so no backing chain.
const rawDiskFormat = "raw"

// walkBackingChainFrom is walkBackingChain with the disk itself opened as
// format ("" probes it), as a copy that pins the disk's format reads it. A
// raw disk has no backing chain: it is only checked to be a regular file,
// and never opened — its bytes are the guest's, and a header a guest wrote
// there is never read.
//
// An image is read through sudo (qemuImgInfoOnHost, as root when the host
// allows it) only when it is opened as qcow2 or raw, in a format named by the
// definition or by the header of a parent itself read in a named format (not
// probed): every root read is
// `qemu-img info -U -f <qcow2|raw> --output=json -- <image>`, the one shape
// the documented sudoers rule allows. An image whose format nothing names is
// probed by the SSH user alone, and so is every image below it — a probed
// header may be a guest's forgery, so nothing it names is opened as root —
// and an image of another format too.
func walkBackingChainFrom(ctx context.Context, h hostCommandRunner, disk, format string) ([]backingLevel, error) {
	return walkChainChecked(ctx, h, disk, format, nil)
}

// walkChainChecked is walkBackingChainFrom that calls before (when not nil)
// with each image of the chain and the format it is about to be opened in —
// after its file-kind check, BEFORE qemu-img opens it. An error from before
// ends the walk and is returned as is: a copy refuses an image it must not
// read before anyone reads it (checkCopySource).
func walkChainChecked(ctx context.Context, h hostCommandRunner, disk, format string,
	before func(path, format string) error) ([]backingLevel, error) {
	if format == rawDiskFormat {
		if err := checkChainFileKind(ctx, h, disk); err != nil {
			return nil, err
		}
		if before != nil {
			if err := before(disk, format); err != nil {
				return nil, err
			}
		}
		return []backingLevel{{path: disk, refs: []string{disk}, format: format}}, nil
	}
	var levels []backingLevel
	cur := disk
	// probed is set once an image of the chain was opened without a named
	// format: from there on, nothing is read as root.
	probed := false
	for depth := 0; ; depth++ {
		if depth > maxBackingChainDepth {
			return nil, hostCheckFailed("read backing chain", fmt.Errorf("%s: backing chain longer than %d images", disk, maxBackingChainDepth))
		}
		if err := checkChainFileKind(ctx, h, cur); err != nil {
			return nil, err
		}
		if before != nil {
			if err := before(cur, format); err != nil {
				return nil, err
			}
		}
		args := []string{"-U"}
		if format != "" {
			args = append(args, "-f", format)
		}
		args = append(args, "--output=json", "--", cur)
		if format == "" {
			probed = true
		}
		var res *VirshResult
		var err error
		if !probed && privilegedSourceFormats[format] {
			res, err = qemuImgInfoOnHost(ctx, h, args...)
		} else {
			res, err = runHost(ctx, h, append([]string{"qemu-img", "info"}, args...)...)
		}
		if err != nil {
			if res != nil && res.ExitCode == qemuImgFailureExitCode && strings.Contains(res.Stderr, qemuImgMissingFile) {
				return levels, nil // the chain ends at a file that no longer exists
			}
			return nil, hostCheckFailed("read backing chain", err)
		}
		var info qemuImgInfo
		if jerr := json.Unmarshal([]byte(res.Stdout), &info); jerr != nil {
			return nil, hostCheckFailed("parse backing chain", jerr)
		}
		dataFile := ""
		if info.FormatSpecific != nil {
			dataFile = info.FormatSpecific.Data.DataFile
			if dataFile != "" && !strings.HasPrefix(dataFile, "/") {
				return nil, hostCheckFailed("read backing chain",
					fmt.Errorf("%s: external data file %q is not a local file path", cur, dataFile))
			}
		}
		levels = append(levels, backingLevel{path: cur, refs: info.referencedFiles(), format: format, dataFile: dataFile})
		if info.BackingFilename == "" && info.FullBackingFilename == "" {
			return levels, nil
		}
		next := info.FullBackingFilename
		if !strings.HasPrefix(next, "/") {
			return nil, hostCheckFailed("read backing chain",
				fmt.Errorf("%s: backing file %q is not a local file path; not followed", cur, next))
		}
		format = info.BackingFilenameFormat
		if format != "" && !backingFormatRE.MatchString(format) {
			return nil, hostCheckFailed("read backing chain",
				fmt.Errorf("%s: backing file format %q is not a qemu format name; not followed", cur, format))
		}
		cur = next
	}
}

// checkChainFileKind refuses to have qemu-img open an image of a chain — the
// disk itself or a backing file — that exists and is not a regular file
// (backingKindScript): qemu-img would open a device, or block forever on a
// FIFO. A path the SSH user cannot see is left to qemu-img (through sudo) to
// open or report missing.
func checkChainFileKind(ctx context.Context, h hostCommandRunner, path string) error {
	res, err := runHost(ctx, h, "sh", "-c", backingKindScript, "sh", path)
	if err != nil {
		return hostCheckFailed("check chain file", err)
	}
	if strings.TrimSpace(res.Stdout) == backingKindOther {
		return hostCheckFailed("read backing chain", fmt.Errorf("%s is not a regular file; not opened", path))
	}
	return nil
}

// liveDomainDoc is what liveChainListed reads of a domain definition.
type liveDomainDoc struct {
	XMLName xml.Name `xml:"domain"`
	// ID is the running domain's id; an inactive definition has none.
	ID      string `xml:"id,attr"`
	Devices struct {
		Disks []struct {
			Source *struct {
				File   string `xml:"file,attr"`
				Dev    string `xml:"dev,attr"`
				Volume string `xml:"volume,attr"`
			} `xml:"source"`
			BackingStore *struct{} `xml:"backingStore"`
		} `xml:"disk"`
	} `xml:"devices"`
}

// liveChainListed reports whether domainXML is a RUNNING domain's definition
// that lists the image chain of every disk with a source: libvirt then shows
// each backing file as a nested <backingStore> (ending in an empty one), so
// the chain is taken from the definition instead of opening the images. An
// inactive definition, or a disk without a <backingStore> element, returns
// false: its chain is read with qemu-img.
func liveChainListed(domainXML string) bool {
	var d liveDomainDoc
	if err := xml.Unmarshal([]byte(domainXML), &d); err != nil {
		return false
	}
	if id := strings.TrimSpace(d.ID); id == "" || id == "-1" {
		return false
	}
	for _, disk := range d.Devices.Disks {
		if disk.Source == nil || (disk.Source.File == "" && disk.Source.Dev == "" && disk.Source.Volume == "") {
			continue // empty removable media: nothing to walk
		}
		if disk.BackingStore == nil {
			return false
		}
	}
	return true
}

// qemuImgInfoOnHost runs `qemu-img info <args>` on the host behind h through
// passwordless sudo (`sudo -n`), so a domain's disk the provider's host
// account cannot read (a 0600 libvirt-qemu image, a root_squash NFS pool) does
// not fail the disk in-use check: the check reads EVERY domain's disk chain,
// and one unreadable file used to fail every Delete, snapshot and create on
// the host. When sudo itself refuses (no passwordless sudo for qemu-img, or
// no sudo at all), it runs as the host account, as before. It only ever reads
// the headers of disks named by domain definitions and of the local, regular
// backing files their headers name (backingChainFiles), an adopted imported
// disk's and GetDiskInfo's own disk — never a caller-supplied image path,
// which inspectHostImage reads unprivileged. Every call is
// `qemu-img info -U -f <qcow2|raw> --output=json -- <image>`, so sudo can be
// limited to exactly that shape with a regular-expression rule (see
// docs/libvirt-clones.md).
func qemuImgInfoOnHost(ctx context.Context, h hostCommandRunner, args ...string) (*VirshResult, error) {
	res, err := runHost(ctx, h, append([]string{"sudo", "-n", "qemu-img", "info"}, args...)...)
	if err != nil && sudoRefused(res) {
		return runHost(ctx, h, append([]string{"qemu-img", "info"}, args...)...)
	}
	return res, err
}

// sudoRefusalLineRE matches, as a WHOLE line, what sudo itself prints when it
// refuses to run a command without a password (-n): a password or a terminal
// is required, the user may not run the command (or sudo at all), or is not
// in the sudoers file.
var sudoRefusalLineRE = regexp.MustCompile(`^(?:` +
	`sudo: a password is required` +
	`|sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper` +
	`|sudo: sorry, you must have a tty to run sudo` +
	`|sudo: no tty present and no askpass program specified` +
	`|Sorry, user \S+ is not allowed to execute '[^'\n]*' as \S+ on \S+\.` +
	`|Sorry, user \S+ may not run sudo on \S+\.` +
	`|\S+ is not in the sudoers file\.(?:  This incident (?:will be|has been) reported(?: to the administrator)?\.)?` +
	`)$`)

// sudoNotFoundLineRE matches, as a whole line, a shell's report that sudo is
// not installed (sh, dash, bash, zsh; direct or through exec).
var sudoNotFoundLineRE = regexp.MustCompile(`^(?:(?:\S+: )?(?:line \d+: |\d+: )?(?:exec: )?sudo: (?:command )?not found|zsh:\d+: command not found: sudo)$`)

// Exit statuses of a refused `sudo -n ...`: sudo's own refusal, and a shell's
// for a command that is not installed (sudo missing).
const (
	sudoExitRefused  = 1
	sudoExitNotFound = 127
)

// sudoRefused reports whether a failed `sudo -n ...` failed in sudo itself —
// not permitted, a password required, or sudo not installed — rather than in
// the command it ran. It takes sudo's exit status AND its whole stderr: every
// non-empty line must be one of sudo's (or the shell's) exact messages, so a
// qemu-img error — whose lines start "qemu-img:", and which may quote a file
// name holding a newline and a sudo-like line — is never taken for a refusal.
// Only a genuine refusal lets a caller fall back to the SSH user.
func sudoRefused(res *VirshResult) bool {
	if res == nil {
		return false
	}
	var lineRE *regexp.Regexp
	switch res.ExitCode {
	case sudoExitRefused:
		lineRE = sudoRefusalLineRE
	case sudoExitNotFound:
		lineRE = sudoNotFoundLineRE
	default:
		return false
	}
	lines := 0
	for _, line := range strings.Split(res.Stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !lineRE.MatchString(line) {
			return false
		}
		lines++
	}
	return lines > 0
}

// domainGone reports whether uuid is no longer defined on the host — i.e. it
// was undefined between the list and the dumpxml, which is not a failure.
func domainGone(ctx context.Context, h hostCommandRunner, uuid string) (bool, error) {
	uuids, err := listDomainUUIDs(ctx, h)
	if err != nil {
		return false, err
	}
	return !containsString(uuids, uuid), nil
}

// diskSourcesInUse collects, on the host behind h, every path referenced by any
// defined domain (running or not) — including each disk's full backing chain —
// canonicalized. It fails closed: if a definition that still exists, a volume
// path, or a backing chain cannot be read, the caller gets a (generic)
// retryable error rather than an incomplete set. A domain undefined between the
// list and the dumpxml is skipped.
func diskSourcesInUse(ctx context.Context, h hostCommandRunner) (inUseSet, error) {
	doms, err := domainRefsOnHost(ctx, h, "")
	if err != nil {
		return inUseSet{}, err
	}
	set := inUseSet{files: map[string]bool{}}
	for _, d := range doms {
		for f := range d.refs.files {
			set.files[f] = true
		}
		set.dirs = append(set.dirs, d.refs.dirs...)
	}
	return set, nil
}

// hostDomainRefs is what one domain defined on a host references: every path of
// its definition (disks and their full backing chains, other file-backed
// devices, firmware/kernel files, shared directories), raw and canonical.
type hostDomainRefs struct {
	// uuid is the domain's UUID as `virsh list --uuid` printed it.
	uuid string
	// refs are the domain's references (see inUseSet).
	refs inUseSet
	// owners are the VirtRigaud owner stamps of the definition that was read
	// (domainOwners), nil when it carries none or they cannot be parsed. They
	// are informational — only the cluster-wide disk guard reads them, to tell
	// a previous incarnation of a VM from a foreign domain (ADR-0007 A6) — and
	// never authorize anything.
	owners []contracts.ObjectIdentity
}

// domainRefsOnHost reads, on the host behind h, the references of every defined
// domain (running or not) except skipUUID (never read when non-empty), one
// entry per domain. It is diskSourcesInUse's scan, kept per domain so a caller
// can tell which OTHER domains use a file (diskDependents), with the same
// fail-closed rules: an unreadable definition that still exists, volume path
// or backing chain is a (generic) retryable error, and a domain undefined
// between the list and its dumpxml is skipped. Each disk's chain is read once,
// however many domains reference it, and every path is canonicalized in one
// call.
func domainRefsOnHost(ctx context.Context, h hostCommandRunner, skipUUID string) ([]hostDomainRefs, error) {
	return domainRefsOnHostBounded(ctx, h, skipUUID, 0)
}

// domainRefsOnHostBounded is domainRefsOnHost reading at most maxDomains
// domains (0: no bound). A host with more fails the check closed (a generic
// retryable error) before any definition is read, rather than being read
// partially or past its caller's deadline — the cluster-wide disk guard
// bounds each host as a clustered ListVMs does
// (clusteredListMaxDomainsPerHost). The bound runs no extra command.
func domainRefsOnHostBounded(ctx context.Context, h hostCommandRunner, skipUUID string, maxDomains int) ([]hostDomainRefs, error) {
	uuids, err := listDomainUUIDs(ctx, h)
	if err != nil {
		return nil, err
	}
	if maxDomains > 0 && len(uuids) > maxDomains {
		return nil, hostCheckFailed("list domains",
			fmt.Errorf("%d domains on the host exceed the scan bound of %d", len(uuids), maxDomains))
	}
	type rawRefs struct {
		uuid               string
		files, dirs, disks []string
		// formats are the disks' level-0 formats, from the definition.
		formats map[string]string
		owners  []contracts.ObjectIdentity
	}
	var doms []rawRefs
	for _, uuid := range uuids {
		if skipUUID != "" && strings.EqualFold(uuid, skipUUID) {
			continue
		}
		xmlRes, err := h.runVirshCommand(ctx, "dumpxml", uuid)
		if err != nil {
			gone, gerr := domainGone(ctx, h, uuid)
			if gerr != nil {
				return nil, gerr
			}
			if gone {
				log.Printf("INFO domain %s was undefined during the disk in-use check; skipping it", uuid)
				continue
			}
			return nil, hostCheckFailed(fmt.Sprintf("read definition of domain %s", uuid), err)
		}
		refs, err := parseDomainPathRefs(xmlRes.Stdout)
		if err != nil {
			return nil, hostCheckFailed(fmt.Sprintf("parse definition of domain %s", uuid), err)
		}
		d := rawRefs{uuid: uuid, files: refs.files, dirs: refs.dirs, disks: refs.disks, formats: map[string]string{}}
		if owners, oerr := domainOwners(xmlRes.Stdout); oerr == nil {
			d.owners = owners
		}
		var byVolume map[[2]string]string
		if doc, perr := parseDomainDisks(xmlRes.Stdout); perr == nil {
			d.formats, byVolume = doc.diskSourceFormats()
		}
		for _, pv := range refs.volumes {
			volRes, verr := h.runVirshCommand(ctx, "vol-path", "--pool", pv[0], "--vol", pv[1])
			if verr != nil {
				return nil, hostCheckFailed(
					fmt.Sprintf("resolve volume %q in pool %q of domain %s", pv[1], pv[0], uuid), verr)
			}
			p := strings.TrimSpace(volRes.Stdout)
			if p == "" {
				return nil, hostCheckFailed(
					fmt.Sprintf("resolve volume %q in pool %q of domain %s", pv[1], pv[0], uuid), errors.New("empty path"))
			}
			d.files = append(d.files, p)
			d.disks = append(d.disks, p)
			if f, ok := byVolume[pv]; ok {
				d.formats[p] = f
			}
		}
		if liveChainListed(xmlRes.Stdout) {
			// A running domain's definition lists every disk's chain in
			// <backingStore> (already among d.files): no image is opened.
			d.disks = nil
		}
		doms = append(doms, d)
	}

	// Each disk's chain is walked from the format its definition names (a raw
	// disk not at all): the scan never probes a guest-written image.
	type chainKey struct{ disk, format string }
	chains := map[chainKey][]string{}
	for _, d := range doms {
		for _, disk := range d.disks {
			key := chainKey{disk, d.formats[disk]}
			if _, walked := chains[key]; walked {
				continue
			}
			chain, err := backingChainFiles(ctx, h, disk, key.format)
			if err != nil {
				return nil, err
			}
			chains[key] = chain
		}
	}

	// One canonicalization for every domain: its files and chains, then its
	// shared directories, in domain order.
	var all []string
	type span struct{ files, dirs [2]int }
	spans := make([]span, len(doms))
	for i, d := range doms {
		start := len(all)
		all = append(all, d.files...)
		for _, disk := range d.disks {
			all = append(all, chains[chainKey{disk, d.formats[disk]}]...)
		}
		spans[i].files = [2]int{start, len(all)}
	}
	for i, d := range doms {
		start := len(all)
		all = append(all, d.dirs...)
		spans[i].dirs = [2]int{start, len(all)}
	}
	canon, err := canonicalizeOnHost(ctx, h, all)
	if err != nil {
		return nil, err
	}

	out := make([]hostDomainRefs, len(doms))
	for i, d := range doms {
		set := inUseSet{files: map[string]bool{}}
		for j := spans[i].files[0]; j < spans[i].files[1]; j++ {
			set.files[all[j]] = true
			set.files[canon[j]] = true
		}
		set.dirs = append(set.dirs, canon[spans[i].dirs[0]:spans[i].dirs[1]]...)
		out[i] = hostDomainRefs{uuid: d.uuid, refs: set, owners: d.owners}
	}
	return out, nil
}

// pathInUseOnHost reports whether path (canonicalized on the host) is a disk,
// backing file or shared directory of any domain on the host behind h.
func pathInUseOnHost(ctx context.Context, h hostCommandRunner, path string) (bool, error) {
	canon, err := canonicalizeOnHost(ctx, h, []string{path})
	if err != nil {
		return false, err
	}
	inUse, err := diskSourcesInUse(ctx, h)
	if err != nil {
		return false, err
	}
	return inUse.contains(canon[0]), nil
}

// hostConnRunner adapts a hostconn.Conn (the import RPCs' connection seam) to
// hostCommandRunner, so those paths can reuse the header check.
type hostConnRunner struct {
	conn hostconn.Conn
}

// runVirshCommand implements hostCommandRunner over the Conn's RunHost ("!"
// host commands) and Virsh (control commands).
func (r hostConnRunner) runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	var res *hostconn.Result
	var err error
	if len(args) > 0 && args[0] == "!" {
		res, err = r.conn.RunHost(ctx, args[1:]...)
	} else {
		res, err = r.conn.Virsh(ctx, args...)
	}
	if res == nil {
		return nil, err
	}
	return &VirshResult{Command: res.Command, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Duration: res.Duration}, err
}
