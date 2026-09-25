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
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Identity-mode image preparation (ADR-0009 D1, D3, D4, D6; Slice 4).
//
// The artifact of VMImage <ns>/<name> (UID u) prepared from a source with digest
// d is <pool>/<A>.qcow2, A = imageartifact.ArtifactName(NameRuleLibvirt, ...),
// stamped by the sidecar <pool>/.<A>.virtrigaud-image.json (image_sidecar.go).
//
// Probe and reuse (D4). One host command reads both names at once (never
// following a symlink) and the sidecar's content. The observation is:
//
//   - complete: the artifact is a regular file, the sidecar is a trusted stamp,
//     and the stamp's recorded inode and size equal the artifact's;
//   - a trusted stamp with NO artifact: a publish between its two links, live
//     while the sidecar's mtime is younger than the staleness bound, abandoned
//     after it;
//   - anything else present — an artifact without a sidecar, an untrusted or
//     unreadable sidecar, an artifact that is not a regular file or whose
//     inode/size the stamp does not record — carries NO trusted stamp.
//
// imageartifact.Decide then reuses only a complete artifact whose stamp
// carries the request's UID and digest; a matching publish in flight is a
// retryable Unavailable; a matching abandoned stamp is removed (only if it is
// still the very file that was probed) before importing again; everything
// else is a Conflict that is never overwritten, deleted, re-stamped or
// adopted. A probe that fails is a retryable error, never "absent".
//
// Publish (D6). The download and conversion run in private staging files
// (image.go). Then:
//
//  1. the converted image is finalized read-only (0444, restorecon, sync; no
//     chown) and stat'ed: its inode and size go into the stamp;
//  2. the stamp is written to a private staging file, made read-only, synced;
//  3. `ln <stamp staging> <sidecar>`: link(2) never replaces a file, so
//     exactly one concurrent prepare CREATES the sidecar. A prepare whose link
//     finds the name taken re-runs the probe (reuse, in progress, abandoned,
//     or Conflict) and never links the artifact;
//  4. only the sidecar's creator runs `ln <converted> <artifact>`. If the
//     artifact name is taken (EEXIST), something this prepare did not publish
//     sits there: the prepare first unlinks ITS OWN sidecar (only if the
//     sidecar is still a link to its staging file), so the stamp never names
//     another file, then returns a Conflict. Any other failure also withdraws
//     the sidecar and is retryable;
//  5. the staging names are removed; the artifact and the sidecar keep their
//     own links.
//
// A link that fails with EEXIST but whose destination is the very file being
// linked (an NFS retransmission of a LINK that succeeded) counts as created.
// A filesystem that cannot hold hard links fails the prepare with an explicit
// InvalidSpec.

// imageArtifact names an identity-mode artifact in a pool directory.
type imageArtifact struct {
	// name is the artifact base name (prepared_image_id).
	name string
	// path is the artifact file, <dir>/<name>.qcow2 (prepared_image_path).
	path string
	// sidecar is the stamp file, <dir>/.<name>.virtrigaud-image.json.
	sidecar string
}

// newImageArtifact returns the artifact name lays out in dir. The name comes
// from imageartifact.ArtifactName, so it is never a #334 reserved name and
// never a path; that is re-checked here, fail-closed.
func newImageArtifact(dir, name string) (imageArtifact, error) {
	if name == "" || name != filepath.Base(name) || reservedImageName(name+qcow2Ext) {
		return imageArtifact{}, contracts.NewInvalidSpecError(
			fmt.Sprintf("prepared-image artifact name %q is not a usable image file name", name), nil)
	}
	return imageArtifact{
		name:    name,
		path:    filepath.Join(dir, name+qcow2Ext),
		sidecar: filepath.Join(dir, imageSidecarName(name)),
	}, nil
}

// prepareIdentity serves an identity request (ADR-0009 D1-D6; see the file
// comment).
func (ip *imagePreparer) prepareIdentity(ctx context.Context, req imageartifact.Request, job prepareJob) (imagePrepareResult, error) {
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt, req.Image, req.SourceDigest)
	if err != nil {
		return imagePrepareResult{}, contracts.NewInvalidSpecError(
			fmt.Sprintf("cannot name the prepared-image artifact: %v", err), nil)
	}
	art, err := newImageArtifact(job.loc.dir, name)
	if err != nil {
		return imagePrepareResult{}, err
	}

	outcome, pr, err := ip.decide(ctx, art, req, job.staleness)
	if err != nil {
		return imagePrepareResult{}, err
	}
	switch outcome {
	case imageartifact.OutcomeReuse:
		log.Printf("INFO ImagePrepare: reusing prepared-image artifact %q (stamp matches VMImage uid=%s)", art.path, req.Image.UID)
		return reusedArtifact(art, pr), nil
	case imageartifact.OutcomeInProgress:
		return imagePrepareResult{}, imageartifact.InProgressError(art.name)
	case imageartifact.OutcomeConflict:
		return imagePrepareResult{}, ip.conflict(art, req, pr)
	case imageartifact.OutcomeAbandoned:
		if err := ip.removeAbandonedSidecar(ctx, art, pr); err != nil {
			return imagePrepareResult{}, err
		}
	case imageartifact.OutcomeImport:
	default:
		return imagePrepareResult{}, fmt.Errorf("unexpected prepared-image outcome %s", outcome)
	}

	log.Printf("INFO ImagePrepare: preparing artifact %q for VMImage %s/%s (uid=%s) into pool %q from %s",
		art.name, req.Image.Namespace, req.Image.Name, req.Image.UID, job.loc.pool, redactURL(job.src.URL))
	ip.sweepStaging(ctx, job)
	staged, err := ip.stageFromURL(ctx, job)
	if err != nil {
		return imagePrepareResult{}, err
	}
	defer removeHostPath(ctx, ip.host, staged.path, false)
	return ip.publishIdentity(ctx, art, staged, imageartifact.NewStamp(req, ip.now()), req, job)
}

// reusedArtifact is the result of reusing the complete, matching artifact pr
// observed. Its echo reports the stamp found on the host.
func reusedArtifact(art imageArtifact, pr artifactProbe) imagePrepareResult {
	stamp := pr.doc.Stamp
	return imagePrepareResult{ID: art.name, Path: art.path, Stamp: &stamp, Reused: true}
}

// conflict logs, provider-side only, what occupies art's name, and returns the
// uniform Conflict (ADR-0009 D4/D11): the error never names another image.
func (ip *imagePreparer) conflict(art imageArtifact, req imageartifact.Request, pr artifactProbe) error {
	var owner string
	switch {
	case pr.doc != nil:
		owner = fmt.Sprintf("stamp image=%s/%s uid=%s digest=%s preparedBy=%s/%s inode=%d size=%d",
			pr.doc.Image.Namespace, pr.doc.Image.Name, pr.doc.Image.UID, pr.doc.SourceDigest,
			pr.doc.PreparedBy.Namespace, pr.doc.PreparedBy.Name, pr.doc.Artifact.Inode, pr.doc.Artifact.Size)
	case pr.docErr != nil:
		owner = "untrusted stamp: " + pr.docErr.Error()
	case pr.sidecar.exists:
		owner = "the stamp is not a regular file"
	default:
		owner = "no stamp"
	}
	log.Printf("WARN ImagePrepare: refusing prepared-image artifact %q: not prepared for VMImage uid=%s digest=%s "+
		"(artifact exists=%t regular=%t inode=%d size=%d; %s)", art.path, req.Image.UID, req.SourceDigest,
		pr.artifact.exists, pr.artifact.regular, pr.artifact.inode, pr.artifact.size, owner)
	return imageartifact.ConflictError(art.name)
}

// decide probes art and applies the ADR-0009 D4 rule for req.
func (ip *imagePreparer) decide(ctx context.Context, art imageArtifact, req imageartifact.Request, staleness time.Duration) (imageartifact.Outcome, artifactProbe, error) {
	pr, err := ip.probeArtifact(ctx, art)
	if err != nil {
		return 0, artifactProbe{}, err
	}
	return imageartifact.Decide(pr.observation(staleness), req), pr, nil
}

// hostFileStat is what the probe found at one name.
type hostFileStat struct {
	exists  bool
	regular bool
	inode   uint64
	size    int64
	mtime   int64
}

// artifactProbe is one probe of an artifact's two names.
type artifactProbe struct {
	// hostNow is the host's clock (seconds since the epoch) at the probe, so
	// ages never depend on the pod's clock.
	hostNow  int64
	artifact hostFileStat
	sidecar  hostFileStat
	// doc is the sidecar's trusted document, nil when there is no readable,
	// regular sidecar or it is untrusted (docErr says why).
	doc    *imageSidecar
	docErr error
}

// observation maps the probe onto imageartifact.Observation (see the file
// comment). Only a trusted stamp that proves the file at the artifact name, or
// a trusted stamp with no artifact at all, is ever passed on.
func (pr artifactProbe) observation(staleness time.Duration) imageartifact.Observation {
	obs := imageartifact.Observation{Exists: pr.artifact.exists || pr.sidecar.exists}
	if pr.doc == nil {
		return obs
	}
	switch {
	case !pr.artifact.exists:
		stamp := pr.doc.Stamp
		obs.Stamp = &stamp
		obs.Live = pr.sidecarAge() < staleness
	case pr.artifact.regular && pr.doc.Artifact.Inode == pr.artifact.inode && pr.doc.Artifact.Size == pr.artifact.size:
		stamp := pr.doc.Stamp
		obs.Stamp = &stamp
		obs.Complete = true
	}
	return obs
}

// sidecarAge is how long ago the sidecar was last written, by the host clock.
func (pr artifactProbe) sidecarAge() time.Duration {
	return time.Duration(pr.hostNow-pr.sidecar.mtime) * time.Second
}

// imageArtifactProbeScript prints, for artifact "$1" and sidecar "$2": the
// host clock; for each name either "absent" or "<type>|<inode>|<size>|<mtime>"
// (stat without following a symlink); and, when the sidecar is a regular file,
// "readable" plus at most maxImageSidecarBytes+1 bytes of it, or "unreadable".
// Any failure exits 3, which the caller reports as a retryable error.
const imageArtifactProbeScript = `LC_ALL=C; export LC_ALL
date +%s || exit 3
for f in "$1" "$2"; do
  if [ -e "$f" ] || [ -L "$f" ]; then stat -c '%F|%i|%s|%Y' -- "$f" || exit 3; else echo absent; fi
done
if [ -f "$2" ] && [ ! -L "$2" ]; then
  if [ -r "$2" ]; then echo readable; head -c 4097 -- "$2" || exit 3; else echo unreadable; fi
fi`

// probe output markers.
const (
	probeAbsent     = "absent"
	probeReadable   = "readable"
	probeUnreadable = "unreadable"
)

// statRegularEmptyFile is GNU stat's %F rendering of an empty regular file.
const statRegularEmptyFile = "regular empty file"

// probeArtifact reads art's artifact and sidecar in one host command. A failed
// or unparseable probe is a retryable error, never "absent" (ADR-0009 D4).
func (ip *imagePreparer) probeArtifact(ctx context.Context, art imageArtifact) (artifactProbe, error) {
	res, err := runHost(ctx, ip.host, "sh", "-c", imageArtifactProbeScript, "sh", art.path, art.sidecar)
	if err != nil {
		return artifactProbe{}, hostCheckFailed("probe prepared-image artifact "+art.path, err)
	}
	pr, err := parseArtifactProbe(res.Stdout)
	if err != nil {
		return artifactProbe{}, hostCheckFailed("parse probe of prepared-image artifact "+art.path, err)
	}
	return pr, nil
}

// parseArtifactProbe parses imageArtifactProbeScript's output.
func parseArtifactProbe(out string) (artifactProbe, error) {
	parts := strings.SplitN(out, "\n", 5)
	if len(parts) < 3 {
		return artifactProbe{}, fmt.Errorf("truncated probe output (%d lines)", len(parts))
	}
	var pr artifactProbe
	var err error
	if pr.hostNow, err = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); err != nil {
		return artifactProbe{}, fmt.Errorf("host clock: %w", err)
	}
	if pr.artifact, err = parseHostFileStat(parts[1]); err != nil {
		return artifactProbe{}, fmt.Errorf("artifact: %w", err)
	}
	if pr.sidecar, err = parseHostFileStat(parts[2]); err != nil {
		return artifactProbe{}, fmt.Errorf("sidecar: %w", err)
	}
	if !pr.sidecar.regular {
		return pr, nil
	}
	if len(parts) < 4 {
		return artifactProbe{}, errors.New("truncated probe output: no sidecar content")
	}
	switch parts[3] {
	case probeReadable:
		content := ""
		if len(parts) == 5 {
			content = parts[4]
		}
		doc, perr := parseImageSidecar([]byte(content))
		if perr != nil {
			pr.docErr = perr
			return pr, nil
		}
		pr.doc = &doc
	case probeUnreadable:
		pr.docErr = errors.New("the sidecar is not readable")
	default:
		return artifactProbe{}, fmt.Errorf("unexpected sidecar marker %q", parts[3])
	}
	return pr, nil
}

// parseHostFileStat parses one probe line: "absent" or
// "<type>|<inode>|<size>|<mtime>".
func parseHostFileStat(line string) (hostFileStat, error) {
	if line == probeAbsent {
		return hostFileStat{}, nil
	}
	fields := strings.Split(line, "|")
	if len(fields) != 4 {
		return hostFileStat{}, fmt.Errorf("unexpected stat line %q", line)
	}
	inode, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return hostFileStat{}, fmt.Errorf("unexpected stat line %q: %w", line, err)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return hostFileStat{}, fmt.Errorf("unexpected stat line %q: %w", line, err)
	}
	mtime, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return hostFileStat{}, fmt.Errorf("unexpected stat line %q: %w", line, err)
	}
	return hostFileStat{
		exists:  true,
		regular: fields[0] == statRegularFile || fields[0] == statRegularEmptyFile,
		inode:   inode,
		size:    size,
		mtime:   mtime,
	}, nil
}

// removeStaleSidecarScript removes the sidecar "$1" only if it is still the
// regular file (inode|mtime "$2") the probe judged abandoned, so a sidecar
// another prepare published since is never removed. It prints "removed" when it
// removed it.
const removeStaleSidecarScript = `LC_ALL=C; export LC_ALL
if [ -f "$1" ] && [ ! -L "$1" ]; then
  cur=$(stat -c '%i|%Y' -- "$1") || exit 3
  if [ "$cur" = "$2" ]; then rm -f -- "$1" || exit 3; echo removed; fi
fi`

// removeAbandonedSidecar removes the matching stamp of a crashed prepare of
// this same image (a sidecar with no artifact, older than the staleness bound;
// ADR-0009 D4) — only that file, and only if it is unchanged since the probe.
func (ip *imagePreparer) removeAbandonedSidecar(ctx context.Context, art imageArtifact, pr artifactProbe) error {
	log.Printf("INFO ImagePrepare: removing the abandoned stamp %q of this VMImage (no artifact, last written %v ago)",
		art.sidecar, pr.sidecarAge())
	res, err := runHost(ctx, ip.host, "sh", "-c", removeStaleSidecarScript, "sh", art.sidecar,
		fmt.Sprintf("%d|%d", pr.sidecar.inode, pr.sidecar.mtime))
	if err != nil {
		return hostCheckFailed("remove abandoned prepared-image stamp "+art.sidecar, err)
	}
	if strings.TrimSpace(res.Stdout) != "removed" {
		log.Printf("INFO ImagePrepare: stamp %q changed since it was probed; left in place", art.sidecar)
	}
	return nil
}

// sweepStaging removes, best-effort, this provider's staging files in the pool
// directory that have not been written for the staleness bound: a prepare
// that crashed or was cancelled left them. A live staging file is written
// continuously (curl, qemu-img) or is seconds old, so it is never swept.
func (ip *imagePreparer) sweepStaging(ctx context.Context, job prepareJob) {
	minutes := int64(math.Ceil(job.staleness.Minutes()))
	if _, err := runHost(ctx, ip.host, "find", job.loc.dir, "-maxdepth", "1", "-type", "f",
		"-name", imagePrepareStagingPrefix+"*", "-mmin", "+"+strconv.FormatInt(minutes, 10), "-delete"); err != nil {
		log.Printf("WARN ImagePrepare: failed to sweep stale staging files in %q: %v", job.loc.dir, err)
	}
}

// stageSidecar writes the sidecar document data to a new private staging file
// in dir (content on stdin, never a command line), makes it read-only and
// syncs it.
func (ip *imagePreparer) stageSidecar(ctx context.Context, dir string, data []byte) (string, error) {
	tmp, err := ip.stagingTemp(ctx, dir, stampStagingSuffix)
	if err != nil {
		return "", err
	}
	fail := func(step string, err error) (string, error) {
		removeHostPath(ctx, ip.host, tmp, false)
		log.Printf("ERROR ImagePrepare: %s stamp %q on the libvirt host: %v", step, tmp, err)
		return "", contracts.NewRetryableError("staging the prepared-image stamp failed on the libvirt host "+
			"(details are in the provider log)", nil)
	}
	if err := ip.host.writeRemoteFile(ctx, tmp, data); err != nil {
		return fail("write", err)
	}
	if _, err := runHost(ctx, ip.host, "sh", "-c", `chmod `+preparedImageMode+` -- "$1" && sync -- "$1"`, "sh", tmp); err != nil {
		return fail("finalize", err)
	}
	return tmp, nil
}

// maxSidecarPublishAttempts bounds the publish loop: a name that keeps
// changing between the link and the probe is retried by the manager instead.
const maxSidecarPublishAttempts = 2

// publishIdentity publishes staged under art with stamp (steps 2-5 of the file
// comment).
func (ip *imagePreparer) publishIdentity(ctx context.Context, art imageArtifact, staged stagedImage,
	stamp imageartifact.Stamp, req imageartifact.Request, job prepareJob) (imagePrepareResult, error) {
	data, err := encodeImageSidecar(imageSidecar{
		Stamp:    stamp,
		Artifact: imageSidecarArtifact{Inode: staged.inode, Size: staged.size},
	})
	if err != nil {
		return imagePrepareResult{}, err
	}
	stampTmp, err := ip.stageSidecar(ctx, job.loc.dir, data)
	if err != nil {
		return imagePrepareResult{}, err
	}
	defer removeHostPath(ctx, ip.host, stampTmp, false)

	for attempt := 1; attempt <= maxSidecarPublishAttempts; attempt++ {
		outcome, err := ip.link(ctx, stampTmp, art.sidecar)
		if err != nil {
			return imagePrepareResult{}, err
		}
		switch outcome {
		case linkCreated:
			return ip.publishArtifact(ctx, art, staged, stampTmp, stamp, job)
		case linkUnsupported:
			return imagePrepareResult{}, noHardLinksError(job.loc.pool)
		}

		// The sidecar name is taken: this prepare did not create it, so it
		// must not link the artifact. Re-run the D4 probe.
		decided, pr, err := ip.decide(ctx, art, req, job.staleness)
		if err != nil {
			return imagePrepareResult{}, err
		}
		switch decided {
		case imageartifact.OutcomeReuse:
			log.Printf("INFO ImagePrepare: artifact %q was published concurrently for this VMImage; reusing it", art.path)
			return reusedArtifact(art, pr), nil
		case imageartifact.OutcomeInProgress:
			return imagePrepareResult{}, imageartifact.InProgressError(art.name)
		case imageartifact.OutcomeConflict:
			return imagePrepareResult{}, ip.conflict(art, req, pr)
		case imageartifact.OutcomeAbandoned:
			if err := ip.removeAbandonedSidecar(ctx, art, pr); err != nil {
				return imagePrepareResult{}, err
			}
		}
		// Abandoned (removed) or Import (the stamp vanished meanwhile): try
		// to create the sidecar again.
	}
	return imagePrepareResult{}, contracts.NewRetryableError(fmt.Sprintf(
		"the prepared-image artifact %q could not be published because its stamp kept changing; it will be retried", art.name), nil)
}

// publishArtifact links the artifact after this prepare created the sidecar
// (step 4 of the file comment).
func (ip *imagePreparer) publishArtifact(ctx context.Context, art imageArtifact, staged stagedImage, stampTmp string,
	stamp imageartifact.Stamp, job prepareJob) (imagePrepareResult, error) {
	outcome, err := ip.link(ctx, staged.path, art.path)
	if err == nil && outcome == linkCreated {
		log.Printf("INFO ImagePrepare: published prepared-image artifact %q (inode %d, %d bytes, mode %s)",
			art.path, staged.inode, staged.size, preparedImageMode)
		ip.refreshPool(ctx, job.loc.pool)
		return imagePrepareResult{ID: art.name, Path: art.path, Stamp: &stamp}, nil
	}

	// Our sidecar is published but the artifact is not: withdraw the sidecar
	// FIRST, so it never stamps a file this prepare did not publish.
	ip.withdrawSidecar(ctx, stampTmp, art.sidecar)
	switch {
	case err != nil:
		return imagePrepareResult{}, err
	case outcome == linkExists:
		log.Printf("WARN ImagePrepare: the artifact name %q is occupied by a file this prepare did not publish; "+
			"withdrew this prepare's stamp", art.path)
		return imagePrepareResult{}, imageartifact.ConflictError(art.name)
	default:
		return imagePrepareResult{}, noHardLinksError(job.loc.pool)
	}
}

// withdrawSidecarScript removes the sidecar "$2" only while it is still a link
// to this prepare's stamp staging file "$1".
const withdrawSidecarScript = `if [ "$1" -ef "$2" ]; then rm -f -- "$2" || exit 3; fi`

// withdrawSidecar unlinks this prepare's published sidecar, best-effort and
// even after the request was cancelled. If it fails, the stamp stays without
// an artifact and is aged out as abandoned (ADR-0009 D4).
func (ip *imagePreparer) withdrawSidecar(ctx context.Context, stampTmp, sidecar string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stagingCleanupTimeout)
	defer cancel()
	if _, err := runHost(cctx, ip.host, "sh", "-c", withdrawSidecarScript, "sh", stampTmp, sidecar); err != nil {
		log.Printf("ERROR ImagePrepare: failed to withdraw this prepare's stamp %q; it will be aged out as abandoned: %v",
			sidecar, err)
	}
}

// linkOutcome is what linkNoClobber did.
type linkOutcome int

const (
	// linkCreated: the destination is now a link to the source.
	linkCreated linkOutcome = iota + 1
	// linkExists: the destination was taken by another file (EEXIST); it was
	// left untouched.
	linkExists
	// linkUnsupported: the filesystem cannot hold hard links.
	linkUnsupported
)

// linkNoClobberScript hard-links "$1" to "$2" without ever replacing "$2"
// (link(2) fails with EEXIST instead) and prints linked, exists or
// unsupported. A failed link whose destination is the source file itself
// (-ef: an NFS retransmission of a LINK that succeeded) counts as linked. Any
// other failure prints ln's message and exits 3.
const linkNoClobberScript = `LC_ALL=C; export LC_ALL
if err=$(ln -- "$1" "$2" 2>&1); then echo linked; exit 0; fi
if [ "$1" -ef "$2" ]; then echo linked; exit 0; fi
if [ -e "$2" ] || [ -L "$2" ]; then echo exists; exit 0; fi
case "$err" in
  *"Operation not permitted"*|*"Operation not supported"*) echo unsupported; printf '%s\n' "$err" >&2; exit 0 ;;
esac
printf '%s\n' "$err" >&2
exit 3`

// link publishes src at dst with linkNoClobberScript. A command that could not
// run, or failed for another reason, is a retryable error.
func (ip *imagePreparer) link(ctx context.Context, src, dst string) (linkOutcome, error) {
	res, err := runHost(ctx, ip.host, "sh", "-c", linkNoClobberScript, "sh", src, dst)
	if err != nil {
		log.Printf("ERROR ImagePrepare: link %q -> %q on the libvirt host: %v", src, dst, err)
		return 0, contracts.NewRetryableError("publishing the prepared image failed on the libvirt host "+
			"(details are in the provider log)", nil)
	}
	switch strings.TrimSpace(res.Stdout) {
	case "linked":
		return linkCreated, nil
	case "exists":
		return linkExists, nil
	case "unsupported":
		log.Printf("ERROR ImagePrepare: hard link %q -> %q is not supported by the filesystem: %s",
			src, dst, strings.TrimSpace(res.Stderr))
		return linkUnsupported, nil
	}
	log.Printf("ERROR ImagePrepare: link %q -> %q: unexpected output %q", src, dst, res.Stdout)
	return 0, contracts.NewRetryableError("publishing the prepared image failed on the libvirt host "+
		"(details are in the provider log)", nil)
}
