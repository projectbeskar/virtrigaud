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
	"path/filepath"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Per-create staging on the hypervisor host.
//
// Create and Clone stage files on the host before `virsh define`: the domain
// XML, and (Create) the cloud-init NoCloud seed (user-data, which may carry
// secrets, meta-data and the seed ISO). These used to live at paths derived
// only from the domain name (/tmp/<name>-domain.xml,
// /tmp/virtrigaud-cloudinit/<name>/), so two concurrent creates of the same
// name overwrote each other's files — including another tenant's user-data —
// and anything else on the host that could predict the path could pre-create
// it. Namespaced domain names (domain_naming.go) remove the cross-tenant
// collision; as defense in depth every staged file or directory is now also
// unique to ONE create:
//
//   - it is made by `mktemp` on the host: created exclusively (O_EXCL), with an
//     unpredictable suffix, mode 0600 (file) / 0700 (directory), directly under
//     the sticky staging directory, so no other user can pre-create, replace or
//     rename it;
//   - its name still starts with the domain name, so an operator can tell whose
//     it is;
//   - it is removed when the create no longer needs it (the domain XML after
//     `virsh define`, the seed directory when the create fails; a created
//     domain keeps its seed ISO, which the domain's CD-ROM references and Delete
//     removes).

const (
	// defaultHostStagingDir is the host directory per-create staging files are
	// made in. It must be a sticky, world-writable directory (as /tmp is), so a
	// staged file cannot be renamed or removed by another user.
	defaultHostStagingDir = "/tmp"

	// mktempTemplateSuffix is the run of 'X's mktemp replaces with random
	// characters. Ten gives ~60 bits of randomness.
	mktempTemplateSuffix = "XXXXXXXXXX"

	// domainXMLStagingInfix follows the domain name in a staged domain-XML file
	// name: <staging>/<domain>-domain.xml.<random>.
	domainXMLStagingInfix = "-domain.xml."

	// cloudInitSeedDirPrefix starts a per-create cloud-init seed directory name:
	// <staging>/virtrigaud-cloudinit-<domain>.<random>. The seed ISO inside it
	// is what a created domain's CD-ROM references; getCloudInitISOPath
	// recognizes it by this prefix, and Delete removes the directory.
	cloudInitSeedDirPrefix = "virtrigaud-cloudinit-"

	// cloudInitSeedDirMode is applied to a seed directory once its ISO is
	// built: the qemu process (another user) can reach the ISO by its exact
	// path, but nobody else can list the directory to discover it.
	cloudInitSeedDirMode = "0711"

	// stagingCleanupTimeout bounds a best-effort cleanup command, which runs
	// even after the request context is cancelled.
	stagingCleanupTimeout = 30 * time.Second
)

// stagingDir returns the host staging directory: p.hostStagingDir when set
// (tests point it at a scratch directory), else defaultHostStagingDir.
func (p *Provider) stagingDir() string {
	if p.hostStagingDir != "" {
		return p.hostStagingDir
	}
	return defaultHostStagingDir
}

// makeHostTemp runs `mktemp [-d] <template>` on the host behind h and returns
// the path it created. template must be absolute and end in
// mktempTemplateSuffix; the returned path is checked to be the template with
// only that suffix replaced, so nothing else the host prints is ever used as
// a path.
func makeHostTemp(ctx context.Context, h hostCommandRunner, template string, dir bool) (string, error) {
	return makeHostTempSuffix(ctx, h, template, "", dir)
}

// makeHostTempSuffix is makeHostTemp for a name that ends in suffix after the
// random part (`mktemp --suffix=<suffix> <template>`, GNU coreutils), e.g.
// ".partial": the returned path is the template with only its
// mktempTemplateSuffix replaced, followed by suffix. suffix must be empty or a
// plain file-name tail (no '/', no control characters).
func makeHostTempSuffix(ctx context.Context, h hostCommandRunner, template, suffix string, dir bool) (string, error) {
	if !strings.HasPrefix(template, "/") || !strings.HasSuffix(template, mktempTemplateSuffix) ||
		strings.ContainsRune(suffix, '/') || strings.ContainsFunc(suffix, isControlRune) {
		return "", fmt.Errorf("invalid staging template %q (suffix %q)", template, suffix)
	}
	argv := []string{"mktemp"}
	if dir {
		argv = append(argv, "-d")
	}
	if suffix != "" {
		argv = append(argv, "--suffix="+suffix)
	}
	res, err := runHost(ctx, h, append(argv, template)...)
	if err != nil {
		return "", fmt.Errorf("create staging path on host: %w", err)
	}
	path := strings.TrimSpace(res.Stdout)
	prefix := strings.TrimSuffix(template, mktempTemplateSuffix)
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) || len(path) != len(template)+len(suffix) ||
		strings.ContainsAny(path[len(prefix):len(path)-len(suffix)], "/\n\x00") {
		return "", fmt.Errorf("create staging path on host: unexpected mktemp output %q", path)
	}
	return path, nil
}

// removeHostPath removes path (recursively when recursive) on the host behind
// h, best-effort: a failure is logged, never returned, and the command runs
// with a context that survives the caller's cancellation so a cancelled create
// still cleans up after itself.
func removeHostPath(ctx context.Context, h hostCommandRunner, path string, recursive bool) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stagingCleanupTimeout)
	defer cancel()
	flag := "-f"
	if recursive {
		flag = "-rf"
	}
	if _, err := runHost(cctx, h, "rm", flag, "--", path); err != nil {
		log.Printf("WARN Failed to remove staging path %s on the host: %v", path, err)
	}
}

// defineDomainFromXML defines domainName from domainXML on the host behind vp
// (the single host, or the leased target host in clustered mode): the XML is
// written to a per-call staging file (mode 0600, created exclusively by
// mktemp; see the file comment), `virsh define` reads it, and the file is
// removed whatever the outcome.
func (p *Provider) defineDomainFromXML(ctx context.Context, vp *VirshProvider, domainName, domainXML string) error {
	path, err := makeHostTemp(ctx, vp,
		filepath.Join(p.stagingDir(), domainName+domainXMLStagingInfix+mktempTemplateSuffix), false)
	if err != nil {
		return fmt.Errorf("failed to create domain definition file: %w", err)
	}
	defer removeHostPath(ctx, vp, path, false)

	// Write the domain XML over stdin (no heredoc, no shell interpolation of the
	// path or of any user-derived value inside the XML; see writeRemoteFile).
	if err := vp.writeRemoteFile(ctx, path, []byte(domainXML)); err != nil {
		return fmt.Errorf("failed to create domain definition file: %w", err)
	}
	log.Printf("INFO Created domain definition file: %s", path)

	result, err := vp.runRemoteVirshCommand(ctx, "define", path)
	if err != nil {
		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		defineErr := fmt.Errorf("failed to define domain: %w, output: %s", err, stderr)
		// `virsh define` may have succeeded on the host with only its reply
		// lost (e.g. the SSH session dropped): check before reporting failure,
		// so the caller does not remove files the new domain already uses.
		switch verifyDefined(ctx, vp, domainName, domainXML) {
		case defineVerified:
			log.Printf("WARN virsh define of %s reported an error (%v), but the domain exists with the UUID this "+
				"create generated; treating the define as successful", domainName, err)
			return nil
		case defineAbsent:
			return defineErr
		default:
			return fmt.Errorf("%w: %w", errDefineOutcomeUnknown, defineErr)
		}
	}
	log.Printf("INFO Successfully defined domain: %s", domainName)
	return nil
}

// errDefineOutcomeUnknown marks a failed `virsh define` whose outcome could
// not be established afterwards (verifyDefined): the domain may or may not
// exist, so the caller must keep anything it may reference (the seed ISO).
var errDefineOutcomeUnknown = errors.New("could not establish whether the domain was defined")

// defineOutcome is what verifyDefined established about a failed define.
type defineOutcome int

const (
	// defineUnknown: the check itself failed; the domain may exist.
	defineUnknown defineOutcome = iota
	// defineVerified: a domain of that name exists with the generated UUID —
	// the define succeeded and only its reply was lost.
	defineVerified
	// defineAbsent: no domain of that name exists, or one exists with another
	// UUID (so it is not the one this create generated).
	defineAbsent
)

// virshNoDomainMarkers are the virsh error texts for a domain that does not
// exist (`error: failed to get domain 'x'`, libvirt's VIR_ERR_NO_DOMAIN
// message).
var virshNoDomainMarkers = []string{"failed to get domain", "Domain not found"}

// verifyDefined runs `virsh domuuid <domainName>` after a define reported an
// error and compares the answer with the UUID in domainXML (the fresh random
// UUID the create generated). domainName has passed the naming rule, so virsh
// cannot resolve it as an ID or UUID.
func verifyDefined(ctx context.Context, vp *VirshProvider, domainName, domainXML string) defineOutcome {
	d, err := parseDomainLibvirtxml(domainXML)
	want := ""
	if err == nil {
		want = strings.TrimSpace(d.UUID)
	}
	if want == "" {
		return defineUnknown
	}
	res, err := vp.runVirshCommand(ctx, "domuuid", domainName)
	if err != nil {
		if res != nil {
			for _, m := range virshNoDomainMarkers {
				if strings.Contains(res.Stderr, m) {
					return defineAbsent
				}
			}
		}
		log.Printf("WARN Could not check whether domain %s was defined: %v", domainName, err)
		return defineUnknown
	}
	if strings.EqualFold(strings.TrimSpace(res.Stdout), want) {
		return defineVerified
	}
	return defineAbsent
}

// ensureDiskTargetFree refuses to let VirtRigaud write a disk file over target
// — a VM's own disk path (<pool>/<domain>-disk.qcow2, Create and Clone) or a
// migration's landing disk (<pool>/<domain>-migrated.qcow2, ImportDisk) —
// when that file is a disk (or backing file) of ANY domain defined on the
// host behind h. subject names the file for the error message, which reaches
// the requester's status and so never names the other domain.
//
// With namespaced domain names the file of another VM can only sit at such a
// path in corner cases (a legacy VM literally named "<namespace>.<name>", a
// hand-made domain, a linked clone backed by it, or a VM already running on a
// disk a second migration would re-import), but `qemu-img convert` would
// silently replace a live disk, so it is checked. A file at target that no
// domain uses is overwritten: it can only be left over from an earlier,
// failed attempt for this very domain name (same namespace and name — the
// same VM retried, or a deleted one it replaces), which is exactly the file
// about to be written. The common case — no file there — costs one `test -e`;
// the full in-use scan runs only when a file exists.
func ensureDiskTargetFree(ctx context.Context, h hostCommandRunner, subject, target string) error {
	exists, err := hostPathExists(ctx, h, target)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	inUse, err := pathInUseOnHost(ctx, h, target)
	if err != nil {
		return err
	}
	if inUse {
		log.Printf("WARN Refusing to write %s: %s is in use by another domain on the host", subject, target)
		return contracts.NewConflictError(fmt.Sprintf(
			"%s already exists on the host and is in use by another domain; refusing to overwrite it", subject), nil)
	}
	log.Printf("INFO %s exists at %s but no domain uses it (left by an earlier failed attempt); overwriting it", subject, target)
	return nil
}

// pathExistsScript is the fixed `sh -c` script behind hostPathExists. The path
// is ALWAYS the positional parameter "$1", never interpolated into the text. It
// exits 0 either way (so an absent file — the common case — is not logged as a
// failed command) and prints pathExistsMarker when something exists at "$1".
const pathExistsScript = `if [ -e "$1" ]; then echo ` + pathExistsMarker + `; fi`

// pathExistsMarker is what pathExistsScript prints for an existing path.
const pathExistsMarker = "present"

// hostPathExists reports whether anything exists at path on the host behind h.
// A failure to run the check is a retryable error: the caller must not assume
// either answer.
func hostPathExists(ctx context.Context, h hostCommandRunner, path string) (bool, error) {
	res, err := runHost(ctx, h, "sh", "-c", pathExistsScript, "sh", path)
	if err != nil {
		return false, contracts.NewRetryableError(fmt.Sprintf("check whether %s exists on the host", path), err)
	}
	return strings.TrimSpace(res.Stdout) == pathExistsMarker, nil
}

// domainDiskSubject names a domain's primary disk file for ensureDiskTargetFree.
func domainDiskSubject(domainName string) string {
	return fmt.Sprintf("the disk file of libvirt domain %q", domainName)
}

// importedDiskSubject names a migration's landing disk for ensureDiskTargetFree.
func importedDiskSubject(volumeName string) string {
	return fmt.Sprintf("imported disk %q", volumeName)
}
