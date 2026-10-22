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
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Cloud-init seed confidentiality (R27).
//
// A VM's cloud-init seed ISO carries its user-data, which may hold passwords,
// keys and tokens. Earlier releases built it in a per-create directory under
// the world-listable /tmp (so the random directory name was discoverable),
// with the ISO left at the SSH user's umask (typically 0644), and they relied
// on the directory name staying secret. They also lost every seed when the
// host emptied /tmp at boot, after which the VM no longer started.
//
// Seeds are now built in a dedicated seed directory (EnvSeedDir, default
// DefaultSeedDir), owned by the provider's SSH user and not listable or
// writable by anyone else (ensureSeedRoot refuses one that is: fail closed).
// Inside it, each seed still gets its own `mktemp -d` directory, and:
//
//   - the ISO is created under umask 0077 and then set to seedFileMode (0600)
//     explicitly, never left to the umask: only the SSH user can read it until
//     libvirt starts the domain. At start, libvirt's DAC driver (qemu.conf
//     dynamic_ownership = 1, the default) gives the read-only CD-ROM file to
//     the qemu user — keeping the mode, so 0600 for qemu alone — and, for a
//     read-only disk, does not hand it back at shutdown;
//   - user-data, meta-data, network-config and vendor-data, which only
//     genisoimage needs, are removed as soon as the ISO is built;
//   - a second copy of the ISO, cloudInitISOPrivateCopyName (0600), stays with
//     the SSH user: once libvirt has given cloud-init.iso to the qemu user, the
//     SSH user can no longer read it, and the provider never reads a seed as
//     root. Every SSH-user read of an existing seed (a clone reading the files
//     in it, clone_seed.go) goes through seedCopySource, which picks that copy;
//   - the per-seed directory is opened to traverse-only (cloudInitSeedDirMode,
//     0711) last, so the qemu process can reach the ISO by its exact path.
//
// AppArmor (Ubuntu): virt-aa-helper adds every disk source of a domain,
// CD-ROMs included, to that domain's own profile, and may itself read any
// "*.iso" file, so a seed directory anywhere outside the paths virt-aa-helper
// refuses (/etc, /usr, /boot, /dev, ...) needs no AppArmor change. The default
// is below /var/lib/libvirt so seeds live, are backed up and are cleaned up
// with the rest of the host's VM state.
//
// Seeds made by earlier releases stay where their domains reference them: the
// delete and reclaim paths recognize seed directories in every seed root
// (seedRoots: the seed directory and the legacy staging directory).

const (
	// EnvSeedDir names the provider-pod environment variable that sets the
	// host directory cloud-init seeds are built in: one absolute directory,
	// owned by the provider's SSH user and neither readable nor writable by
	// its group or others (mode 0700 or 0711; 0711 lets a non-root qemu user
	// reach the ISOs). Unset or blank selects DefaultSeedDir. Set it through
	// the Provider's spec.runtime.env. It applies to every host of a clustered
	// Provider.
	EnvSeedDir = "VIRTRIGAUD_LIBVIRT_SEED_DIR"

	// DefaultSeedDir is the seed directory when EnvSeedDir is unset or blank.
	DefaultSeedDir = "/var/lib/libvirt/virtrigaud-seeds"

	// seedRootMode is the mode the provider creates a missing seed directory
	// with (when the SSH user may create it, e.g. root): the qemu user can
	// traverse it, nobody else can list it.
	seedRootMode = "0711"

	// seedFileMode is the mode of every file in a seed directory: private to
	// its owner (the SSH user until libvirt gives the ISO to the qemu user).
	seedFileMode = "0600"

	// seedFileUmask is the umask seed files are created under (0666 &^ 0077 =
	// seedFileMode), so a file is private from the moment it exists.
	seedFileUmask = "0077"

	// cloudInitISOPrivateCopyName is the SSH user's own copy of a seed's ISO,
	// next to cloudInitISOName, that a clone reads the seed from (the domain's
	// cloud-init.iso belongs to the qemu user once the domain has started). No
	// domain ever references it.
	cloudInitISOPrivateCopyName = "cloud-init.private.iso"

	// seedRootMissingMarker and seedRootNotDirMarker are what seedRootScript
	// prints for a seed directory that does not exist (and could not be
	// created) or is not a directory.
	seedRootMissingMarker = "missing"
	seedRootNotDirMarker  = "notdir"

	// seedRootForbiddenBits are the permission bits a seed directory must not
	// have: read or write for its group or others (listing or replacing the
	// seed directories in it).
	seedRootForbiddenBits = 0o066

	// seedAncestorWriteBits are the permission bits no directory above the seed
	// directory may have unless it is sticky: write for its group or others
	// (renaming the seed directory, or a directory on its path, away and
	// planting another).
	seedAncestorWriteBits = 0o022

	// seedDirStickyBit is the sticky bit: in a sticky directory only an entry's
	// owner (or the directory's, or root) may rename or remove it.
	seedDirStickyBit = 0o1000

	// seedRootOtherExec is the traverse bit for others the seed directory needs
	// when qemu runs as a user other than the SSH user (checkSeedRootAtStartup).
	seedRootOtherExec = 0o001

	// Line tags of seedRootScript's report.
	seedRootSelfTag     = "self"
	seedRootLeafTag     = "leaf"
	seedRootAncestorTag = "anc"

	// seedRootDoc is where the seed directory is documented.
	seedRootDoc = "docs/libvirt-domain-ownership.md#cloud-init-seed-directory"
)

// seedDirDefault is the seed directory of a provider that did not load one
// (a struct-literal provider): DefaultSeedDir. Tests point it at a scratch
// directory (TestMain), so no test reaches the real host directory.
var seedDirDefault = DefaultSeedDir

// seedRootScript is the fixed `sh -c` script behind ensureSeedRoot. The path is
// ALWAYS the positional parameter "$1", never interpolated into the text. It
// first tries to create a missing directory (seedRootMode; mkdir fails, quietly,
// when the SSH user may not create it), then prints seedRootMissingMarker or
// seedRootNotDirMarker, or a report: "self <SSH user's uid>", "leaf <octal mode>
// <owner uid> <path>" for the directory (following a symbolic link), and one
// "anc <octal mode> <owner uid> <path>" for every directory above it — up to
// "/", both along the configured path and along its canonical path (realpath),
// so a symbolic link on the path and the directories holding it are covered.
const seedRootScript = `if [ ! -e "$1" ]; then mkdir -m ` + seedRootMode + ` -- "$1" 2>/dev/null; fi; ` +
	`if [ ! -e "$1" ]; then echo ` + seedRootMissingMarker + `; exit 0; fi; ` +
	`if [ ! -d "$1" ]; then echo ` + seedRootNotDirMarker + `; exit 0; fi; ` +
	`r=$(realpath -e -- "$1") || exit 1; ` +
	`echo "` + seedRootSelfTag + ` $(id -u)"; ` +
	`printf '` + seedRootLeafTag + ` '; stat -L -c '%a %u %n' -- "$1" || exit 1; ` +
	`for s in "$1" "$r"; do p=$s; while [ "$p" != / ]; do p=$(dirname -- "$p"); ` +
	`printf '` + seedRootAncestorTag + ` '; stat -L -c '%a %u %n' -- "$p" || exit 1; done; done`

// errSeedRootUnusable is the cause of every refusal to build a seed in an
// unusable seed directory. Its text is constant (it reaches the requesting
// VirtualMachine's status); the details are logged provider-side.
var errSeedRootUnusable = errors.New("the cloud-init seed directory on the libvirt host is missing, not a directory, " +
	"not owned by the provider's SSH user, readable or writable by other users, or below a directory other users " +
	"can write; no cloud-init data was written (details are in the provider log; see " + seedRootDoc + ")")

// seedRootRefusal is the message of the retryable error that carries
// errSeedRootUnusable: the create resumes once the host is fixed.
const seedRootRefusal = "refusing to stage cloud-init data"

// seedRootCheckFailed is the message of a seed-directory check that could not
// run on the host.
const seedRootCheckFailed = "could not check the cloud-init seed directory on the host (details are in the provider log)"

// seedDirPathRE is the shape a configured seed directory must have: absolute,
// segments of letters, digits, '.', '_' and '-' only (the path is written into
// domain definitions and host command lines).
var seedDirPathRE = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

// seedDirMaxLength bounds a configured seed directory's length.
const seedDirMaxLength = 256

// forbiddenSeedDirRoots are host directories a seed directory may not be in or
// be: directories emptied at boot (a VM whose CD-ROM file is gone no longer
// starts) and pseudo-filesystems.
var forbiddenSeedDirRoots = []string{"/tmp", "/var/tmp", "/run", "/var/run", "/dev", "/proc", "/sys"}

// validateSeedDir is the lexical check of a configured seed directory.
func validateSeedDir(dir string) error {
	switch {
	case len(dir) > seedDirMaxLength:
		return fmt.Errorf("seed directory is longer than %d bytes", seedDirMaxLength)
	case !seedDirPathRE.MatchString(dir):
		return errors.New("seed directory must be an absolute path of letters, digits, '.', '_' and '-'")
	case filepath.Clean(dir) != dir:
		return errors.New("seed directory must be a clean path (no '.' or '..' segment, no trailing '/')")
	}
	for _, seg := range strings.Split(dir[1:], "/") {
		if strings.HasPrefix(seg, "-") {
			return errors.New("no seed directory segment may begin with '-'")
		}
	}
	for _, root := range forbiddenSeedDirRoots {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return fmt.Errorf("seed directory must not be in %s (emptied at boot, or not a real directory)", root)
		}
	}
	return nil
}

// seedDirFromEnv returns the seed directory EnvSeedDir configures, or
// DefaultSeedDir when it is unset or blank.
func seedDirFromEnv() (string, error) {
	v := strings.TrimSpace(os.Getenv(EnvSeedDir))
	if v == "" {
		return DefaultSeedDir, nil
	}
	if err := validateSeedDir(v); err != nil {
		return "", fmt.Errorf("invalid %s: %w", EnvSeedDir, err)
	}
	return v, nil
}

// loadSeedDir validates and records the seed directory from the environment.
// Constructors call it so a malformed EnvSeedDir fails provider start-up (fail
// closed) instead of surfacing per request.
func (p *Provider) loadSeedDir() error {
	dir, err := seedDirFromEnv()
	if err != nil {
		return err
	}
	p.hostSeedDir = dir
	log.Printf("INFO libvirt cloud-init seed directory configured: %s", dir)
	return nil
}

// seedDir returns the host directory new cloud-init seeds are built in.
func (p *Provider) seedDir() string {
	if p.hostSeedDir != "" {
		return p.hostSeedDir
	}
	return seedDirDefault
}

// seedRoots returns every host directory a VirtRigaud seed directory may be
// directly inside: the seed directory, then the staging directory earlier
// releases built seeds in (domains created by them keep referencing those).
func (p *Provider) seedRoots() []string {
	seed, staging := filepath.Clean(p.seedDir()), filepath.Clean(p.stagingDir())
	if seed == staging {
		return []string{seed}
	}
	return []string{seed, staging}
}

// ensureSeedRoot checks, on the host behind h, that root is a directory owned
// by the SSH user that its group and others can neither read nor write
// (seedRootForbiddenBits), below directories that only root or the SSH user own
// and that no one else can write unless they are sticky (seedAncestorWriteBits:
// otherwise another account could rename root, or a directory on its path,
// away and put its own in its place between this check and the write). It
// creates root first (seedRootMode) when it is missing and the SSH user may.
// An unusable directory is a retryable error carrying errSeedRootUnusable (the
// operator fixes the host; the create then resumes); a check that cannot run
// is a generic retryable error. It never writes anything into root.
func ensureSeedRoot(ctx context.Context, h hostCommandRunner, root string) error {
	_, err := inspectSeedRoot(ctx, h, root)
	return err
}

// inspectSeedRoot is ensureSeedRoot, also returning the seed directory's mode.
func inspectSeedRoot(ctx context.Context, h hostCommandRunner, root string) (uint64, error) {
	res, err := runHost(ctx, h, "sh", "-c", seedRootScript, "sh", root)
	if err != nil {
		log.Printf("ERROR Could not check the cloud-init seed directory %s on the host: %v", root, err)
		return 0, contracts.NewRetryableError(seedRootCheckFailed, nil)
	}
	mode, reason := seedRootProblem(res.Stdout)
	if reason != "" {
		log.Printf("ERROR Refusing to build cloud-init seeds in %s on the host: %s. It must be a directory owned by "+
			"the provider's SSH user with mode 0711 (or 0700 when qemu runs as that user or root), e.g. "+
			"`install -d -m 0711 -o <ssh user> -g <ssh user> %s`, below directories owned by root or the SSH user "+
			"that no one else can write (or that are sticky); see %s", root, reason, root, seedRootDoc)
		return 0, contracts.NewRetryableError(seedRootRefusal, errSeedRootUnusable)
	}
	return mode, nil
}

// seedRootProblem reads seedRootScript's output and returns the seed
// directory's mode and what makes it unusable, or "" when it is usable.
func seedRootProblem(out string) (uint64, string) {
	if f := strings.Fields(out); len(f) == 1 {
		switch f[0] {
		case seedRootMissingMarker:
			return 0, "it does not exist and the SSH user cannot create it"
		case seedRootNotDirMarker:
			return 0, "it is not a directory"
		}
	}
	type entry struct {
		mode        uint64
		owner, path string
	}
	parse := func(rest string) (entry, bool) {
		f := strings.SplitN(rest, " ", 3)
		if len(f) != 3 || f[1] == "" || f[2] == "" {
			return entry{}, false
		}
		mode, err := strconv.ParseUint(f[0], 8, 32)
		return entry{mode: mode, owner: f[1], path: f[2]}, err == nil
	}
	var self string
	var leaf *entry
	var ancestors []entry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		tag, rest, _ := strings.Cut(line, " ")
		switch tag {
		case seedRootSelfTag:
			self = strings.TrimSpace(rest)
		case seedRootLeafTag, seedRootAncestorTag:
			e, ok := parse(rest)
			if !ok {
				return 0, fmt.Sprintf("unexpected check output %q", line)
			}
			if tag == seedRootLeafTag {
				leaf = &e
			} else {
				ancestors = append(ancestors, e)
			}
		default:
			return 0, fmt.Sprintf("unexpected check output %q", line)
		}
	}
	if self == "" || leaf == nil || len(ancestors) == 0 {
		return 0, fmt.Sprintf("unexpected check output %q", out)
	}
	if leaf.owner != self {
		return 0, fmt.Sprintf("it is owned by uid %s, not the SSH user (uid %s)", leaf.owner, self)
	}
	if leaf.mode&seedRootForbiddenBits != 0 {
		return 0, fmt.Sprintf("its mode %04o lets its group or others list or write it", leaf.mode)
	}
	for _, a := range ancestors {
		if a.owner != "0" && a.owner != self {
			return 0, fmt.Sprintf("the directory %s above it is owned by uid %s, neither root nor the SSH user (uid %s)",
				a.path, a.owner, self)
		}
		if a.mode&seedDirStickyBit == 0 && a.mode&seedAncestorWriteBits != 0 {
			return 0, fmt.Sprintf("the directory %s above it (mode %04o) is writable by its group or others and not sticky",
				a.path, a.mode)
		}
	}
	return leaf.mode, ""
}

// newSeedDir makes a fresh seed directory for domainName in root on the host
// behind h, after ensureSeedRoot: `mktemp -d`, so it is created exclusively,
// mode 0700, with an unpredictable name <root>/virtrigaud-cloudinit-<domain>.<random>
// (isOwnSeedDirName).
func newSeedDir(ctx context.Context, h hostCommandRunner, root, domainName string) (string, error) {
	if err := ensureSeedRoot(ctx, h, root); err != nil {
		return "", err
	}
	return makeHostTemp(ctx, h,
		filepath.Join(root, cloudInitSeedDirPrefix+domainName+seedDirNameSeparator+mktempTemplateSuffix), true)
}

// sealSeedDir finishes the seed directory dir on the host behind h once its
// ISO is at <dir>/cloud-init.iso: the ISO is set to seedFileMode explicitly,
// the SSH user's private copy of it is made (cloudInitISOPrivateCopyName, also
// seedFileMode), genisoimage's inputs are removed (every NoCloud file:
// user-data, meta-data, network-config, vendor-data — seedFileNames), and only then is the
// directory opened to traverse-only (cloudInitSeedDirMode) for the qemu user.
// Every file is private from creation (the directory is 0700 until the last
// step), so no step exposes the seed.
func sealSeedDir(ctx context.Context, h hostCommandRunner, dir string) error {
	iso := filepath.Join(dir, cloudInitISOName)
	priv := filepath.Join(dir, cloudInitISOPrivateCopyName)
	if _, err := runHost(ctx, h, "chmod", seedFileMode, "--", iso); err != nil {
		return fmt.Errorf("set the cloud-init seed ISO mode: %w", err)
	}
	if _, err := runHost(ctx, h, withUmask(seedFileUmask, "cp", "--", iso, priv)...); err != nil {
		return fmt.Errorf("keep a private copy of the cloud-init seed ISO: %w", err)
	}
	if _, err := runHost(ctx, h, "chmod", seedFileMode, "--", priv); err != nil {
		return fmt.Errorf("set the private cloud-init seed ISO mode: %w", err)
	}
	inputs := make([]string, 0, len(seedFileNames))
	for _, name := range seedFileNames {
		inputs = append(inputs, filepath.Join(dir, name))
	}
	if _, err := runHost(ctx, h, append([]string{"rm", "-f", "--"}, inputs...)...); err != nil {
		return fmt.Errorf("remove the cloud-init seed inputs: %w", err)
	}
	if _, err := runHost(ctx, h, "chmod", cloudInitSeedDirMode, "--", dir); err != nil {
		return fmt.Errorf("set the cloud-init seed directory mode: %w", err)
	}
	return nil
}

// seedCopySource returns the file the provider's SSH user reads to get at the
// content of the seed ISO srcISO that a domain references: the SSH user's
// private copy next to it (cloudInitISOPrivateCopyName) when srcISO is the ISO
// of a VirtRigaud seed directory in one of the seed roots — a per-create one,
// or an earlier release's per-name <root>/virtrigaud-cloudinit/<name> (an
// operator adds the copy there when making a legacy seed private, see
// docs/upgrading.md) — and that copy exists, else srcISO itself (a seed of an
// earlier release: its ISO kept the SSH user's umask, typically 0644). A
// failed check falls back to srcISO.
//
// Every read of an existing seed by the SSH user — a clone reading the files
// inside it with isoinfo (planCloneSeed, readSeedISO) — must go through it: once the domain has started, libvirt has given srcISO to the qemu user
// with mode 0600, and the provider never reads a seed as root (no sudo rule
// covers seeds). Other local users can read neither file (both 0600, in
// directories they cannot list).
func (p *Provider) seedCopySource(ctx context.Context, h hostCommandRunner, srcISO string) string {
	dir := filepath.Dir(srcISO)
	roots := p.seedRoots()
	inSeedRoot := isSeedDirIn(dir, roots...) || isLegacySeedDirIn(dir, roots...)
	if filepath.Base(srcISO) != cloudInitISOName || !inSeedRoot {
		return srcISO
	}
	priv := filepath.Join(filepath.Dir(srcISO), cloudInitISOPrivateCopyName)
	exists, err := hostPathExists(ctx, h, priv)
	if err != nil {
		log.Printf("WARN Could not check for the private copy of the cloud-init seed %s: %v; reading the seed itself", srcISO, err)
		return srcISO
	}
	if !exists {
		return srcISO
	}
	return priv
}

// isLegacySeedDirIn reports whether dir is an earlier release's per-name seed
// directory, <root>/virtrigaud-cloudinit/<name>, for one of roots.
func isLegacySeedDirIn(dir string, roots ...string) bool {
	dir = filepath.Clean(dir)
	for _, root := range roots {
		legacyRoot := filepath.Join(filepath.Clean(root), legacyCloudInitSeedRoot)
		if filepath.Dir(dir) == legacyRoot && dir != legacyRoot {
			return true
		}
	}
	return false
}

// seedRootCheckTimeout bounds checkSeedRootAtStartup.
const seedRootCheckTimeout = 30 * time.Second

// checkSeedRootAtStartup checks, once at a single-host Provider's startup, the
// seed directory on the host (ensureSeedRoot, which creates it when it is
// missing and the SSH user may), so an unusable one is named in the log at
// once rather than at the first create. It also warns when others cannot
// traverse the directory (no o+x, e.g. 0700): a VM whose qemu runs as another
// user then cannot open its seed ISO — a warning only, as qemu may run as the
// SSH user or root. Best-effort: startup goes on. A clustered Provider checks
// each host at its first seed instead (it dials hosts lazily).
func (p *Provider) checkSeedRootAtStartup(ctx context.Context) {
	vp := p.virshProvider
	if vp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, seedRootCheckTimeout)
	defer cancel()
	mode, err := inspectSeedRoot(ctx, vp, p.seedDir())
	if err != nil {
		log.Printf("WARN The cloud-init seed directory %s is not usable on the host: creates fail until it is fixed (see %s)",
			p.seedDir(), seedRootDoc)
		return
	}
	if mode&seedRootOtherExec == 0 {
		log.Printf("WARN The cloud-init seed directory %s has mode %04o: a qemu process running as another user than "+
			"the provider's SSH user cannot reach the seed ISOs in it, and such VMs fail to start; use 0711 (see %s)",
			p.seedDir(), mode, seedRootDoc)
	}
}
