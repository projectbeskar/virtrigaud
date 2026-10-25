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
	stderrors "errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Routed ImportDisk of a CLUSTERED provider (ADR-0007 Addendum B, B2-B4; R11
// slice 1).
//
// A migration into a clustered Provider creates its target VirtualMachine
// first; the ordinary scheduler places it, and the import lands its disk on
// that VM's pending host, named in ImportDiskRequest.target_host_id. The
// provider never picks a host. Only the host-side backends are served: s3 (the
// pod is the S3 client and streams into the host's private directory) and nfs
// (the host's bounded copy reads the object). pvc is refused before any host
// is leased: its source is read in the provider pod.
//
// The landing name is the single-host one, "<default pool>/<ns>.<vm>-migrated.qcow2"
// (importedDiskVolumeName), in the landing host's default pool. Before
// anything is written, in this order (B2):
//
//  1. the landing host's name check (landingDomainCheck, as bindExistingDomain
//     decides a create): a domain named "<ns>.<vm>" on the host refuses the
//     import with AlreadyExists — VM_PREVIOUS_INCARNATION for one stamped for
//     the VM's namespace and name under another UID, VM_PREVIOUS_INCARNATION
//     with the "own" kind for one stamped with the request's own UID (the VM
//     was already created, so the import is never repeated), and a plain
//     AlreadyExists for a foreign or unstamped one (the manager then releases
//     its target's pending host);
//  2. the landing file through A6.1's guard (clusterDiskGuard.checkDiskFree):
//     when the file — or any other disk name of the VM — exists on the landing
//     host, EVERY host of the Provider is scanned, because on a shared pool a
//     domain on another host may use it. A use refuses the import, and a host
//     that cannot be scanned fails it closed (HOST_UNAVAILABLE or
//     VM_DISK_CHECK_FAILED); an unused leftover is replaced by the publish;
//  3. serialisation: the in-process landing lock, keyed on the landing name
//     alone and so provider-wide across every host (routedLandingKey), is
//     held around the checks and the publish (the routed DeleteLandedDisk
//     and the clustered Create that attaches the disk in place take it too),
//     and every write-phase host command runs under flock(1) on the host lock
//     "<staging>/virtrigaud-locks/import-<domain>.lock" (a command left over
//     from a cancelled call makes the re-send answer "still running").
//
// The write half is the single-host one (landS3Import, landNFSImport) over
// the guarded connection routedImportConn: every host command of it — the
// private directory, the stdin writer or the bounded nfs copy, the header
// checks, qemu-img info / convert / check, the publish (mv -T) and
// pool-refresh — runs under the guard script, flock(1) and timeout(1) ending
// before the routed budget (withRoutedBudget), so no host step of a call can
// still run once the manager's 35-minute settle time has passed. All writes go
// to a private directory; the one publish is an atomic rename onto the
// landing name, so a re-send replaces the earlier result and never adds a
// second disk.
//
// Outcomes (B4, R21-style; classifyRoutedImportFailure): a refusal keeps its
// type; a failure before the publish whose commands all ended with an exit
// status of their own is "not performed" (Unavailable + VM_OPERATION_FAILED);
// a command stopped by timeout(1), killed, or whose session dropped once the
// write phase began, a budget that ran out, or a lost rename is an untyped
// unknown outcome. HOST_UNAVAILABLE is answered only when the host never
// answered a command of this call (the lease, the dial, or the first read
// failed): a transport failure after that is "not performed" while only reads
// ran, and unknown once a write began — never HOST_UNAVAILABLE, which the
// manager reads as definite (hostOpRPCError, now answered by hostProgress too, R25).

const (
	// importLockKind names the host lock of a routed import:
	// "import-<domain>.lock" in the provider's lock directory.
	importLockKind = "import"

	// guardOpImport names the routed import in the cluster-wide guard's
	// answers.
	guardOpImport = "import"
)

// Constant wire texts of the routed import's refusals.
const (
	wireRoutedImportBackend = "clustered libvirt ImportDisk serves the s3 and nfs backends only: a pvc (or legacy, " +
		"empty) backend is read in the provider pod and is not supported into a clustered provider (ADR-0007 Addendum B, B3)"
	wireRoutedImportNoHost = "clustered libvirt ImportDisk requires target_host_id: the target VirtualMachine's " +
		"pending host (ADR-0007 Addendum B); a clustered provider never picks a host"
	wireRoutedImportNoIdentity = "clustered libvirt ImportDisk requires target_vm with the target VirtualMachine's " +
		"namespace, name and uid (a canonical UUID) (ADR-0007 Addendum B)"
	wireRoutedImportPool = "clustered libvirt ImportDisk lands in the host's default storage pool only: " +
		"storage_hint must be empty or \"default\""
	wireRoutedImportNFSURL = "disk import not performed: the nfs import source must be an nfs:// URL"
	// wireRoutedImportNameTaken is the plain AlreadyExists of a foreign or
	// unstamped domain with the target VM's name on the landing host. It names
	// neither the other owner nor the host.
	wireRoutedImportNameTaken = "disk import not performed: a libvirt domain with the target VirtualMachine's name " +
		"exists on the landing host and is not this VirtualMachine's; nothing was landed"
	// wireRoutedImportLandingBusy answers an import that waited its budget
	// for other work on the same landing name in this provider.
	wireRoutedImportLandingBusy = "disk import not performed: other work on the landing disk is still running in " +
		"the provider; it is retried"
	// wireRoutedImportBudgetShort answers an import whose checks left too
	// little of its time budget for the write phase.
	wireRoutedImportBudgetShort = "disk import not performed: too little of the call's time budget was left to " +
		"start writing the disk; it is retried"
	// wireRoutedImportFormat refuses a staged format the import cannot read.
	wireRoutedImportFormat = "disk import not performed: the staged disk format must be qcow2, raw or vmdk " +
		"(qcow2 only from nfs)"
	// wireRoutedImportLandingSymlinkFmt answers a symbolic link at the
	// landing name: a host-guard refusal (never written through).
	wireRoutedImportLandingSymlinkFmt = "disk import on host %q was refused: the landing name is a symbolic link on " +
		"the host; nothing was written"
	// wireRoutedImportPoolMisplacedFmt answers a landing host whose default
	// pool is not where Create attaches an imported disk.
	wireRoutedImportPoolMisplacedFmt = "disk import on host %q not performed: " + defaultPoolMisplacedMessage
)

const (
	// routedImportLandingWait bounds the wait for the landing lock (another
	// import, delete or in-place create of the same landing name).
	routedImportLandingWait = 2 * time.Minute

	// routedImportMinWriteBudget is the least time budget the write phase is
	// started with.
	routedImportMinWriteBudget = time.Minute

	// routedImportReapAge is the age after which a private write directory
	// in the pool can belong to no call still running: the longest routed
	// budget plus the manager's re-send settle time (35 minutes).
	routedImportReapAge = defaultRoutedBudget + 35*time.Minute
)

// reapWriteDirsScript is the fixed `sh -c` script of reapStaleWriteDirs. "$1"
// is the pool directory, "$2" the private directories' name prefix, "$3" the
// minimum age in minutes. Only directories (never a symbolic link: find does
// not follow them), directly in the pool, owned by the SSH user and unchanged
// for longer than the age are removed.
const reapWriteDirsScript = `find "$1" -mindepth 1 -maxdepth 1 -type d -name "$2*" -uid "$(id -u)" ` +
	`-mmin +"$3" -exec rm -rf -- {} +`

// reapStaleWriteDirs removes, best-effort, the private write directories
// (vmDiskWriteDirPrefix) in poolDir on the landing host behind h that are
// older than routedImportReapAge: left by a call whose cleanup was stopped,
// failed or never ran. A failure is logged and changes nothing else.
func reapStaleWriteDirs(ctx context.Context, h hostCommandRunner, host hostconn.HostID, poolDir string) {
	minutes := strconv.Itoa(int(routedImportReapAge / time.Minute))
	if _, err := runHost(ctx, h, "sh", "-c", reapWriteDirsScript, "sh", poolDir, vmDiskWriteDirPrefix, minutes); err != nil {
		log.Printf("WARN Could not reap stale private write directories in the default pool on host %s (they are "+
			"reaped by the next import): exit %d", host, guardExitCode(err))
	}
}

// Formats of the routed import's host-scoped wire texts (they name the host
// id, never its endpoint or a path).
const (
	wireRoutedImportUnknownFmt = "disk import on host %q: the outcome is unknown: a host command was stopped by the " +
		"time budget, killed or lost its connection, and may have landed the disk (details are in the provider log)"
	wireRoutedImportInProgressFmt = "disk import on host %q not performed: the same import is still running from an " +
		"earlier attempt; retry later"
	wireRoutedImportGuardRefusedFmt = "disk import on host %q was refused: the provider's lock directory on the host " +
		"is not a directory owned by its SSH user, or a lock or target file is a symbolic link"
	wireRoutedImportToolsMissingFmt = "disk import on host %q not performed: a required host command is missing or " +
		"not runnable (flock from util-linux, timeout from coreutils, qemu-img)"
	wireRoutedImportHostDownFmt = "disk import: host %q is unreachable"
)

// routedLandingKey is the landing-lock key of a clustered provider: the
// landing volume name ALONE, provider-wide, whatever the host (ADR-0007
// Addendum B, B2 step 3; #415 security review). On a shared pool the same
// file is the same landing name on every host, so the routed ImportDisk, the
// routed DeleteLandedDisk and the clustered in-place Create of one name are
// serialised across hosts: a delete through host A can never remove a disk an
// import or create through host B is landing or attaching, and an import
// through host A never renames over a disk a create through host B attaches.
// A clustered Provider runs one replica, so the in-process lock covers every
// caller of that Provider; two Providers fronting one pool are not
// serialised (docs/clustered-provider-inventory.md).
func routedLandingKey(volumeName string) string {
	return volumeName
}

// routedImport is a routed ImportDisk request, checked before any host is
// leased.
type routedImport struct {
	// host is the landing host (target_host_id).
	host hostconn.HostID
	// owner is the target VirtualMachine (namespace, name and uid).
	owner contracts.ObjectIdentity
	// domain is the target VM's domain name, volumeName its landing volume.
	domain, volumeName string
	// backend is migration.BackendS3 or migration.BackendNFS.
	backend string
	// nfsURL is the staged object of an nfs import.
	nfsURL string
}

// routedImportS3Formats are the staged formats a routed s3 import reads ("" is
// the legacy vmdk default of the shared s3 core).
var routedImportS3Formats = map[string]bool{"": true, "qcow2": true, "raw": true, "vmdk": true}

// parseRoutedImport checks a clustered ImportDisk request before any host is
// leased. Every refusal is InvalidArgument with a constant text.
func parseRoutedImport(req *providerv1.ImportDiskRequest) (routedImport, error) {
	r := routedImport{backend: req.GetBackendType()}
	format := strings.ToLower(strings.TrimSpace(req.GetFormat()))
	switch r.backend {
	case migration.BackendS3:
		if err := migration.EnsureRelayMode(req.GetTransferMode()); err != nil {
			return routedImport{}, err
		}
		if !routedImportS3Formats[format] {
			return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportFormat)
		}
	case migration.BackendNFS:
		r.nfsURL = strings.TrimSpace(req.GetSourceUrl())
		if !strings.HasPrefix(r.nfsURL, "nfs://") {
			return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportNFSURL)
		}
		if format != "" && format != migration.StagedFormatQCOW2 {
			return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportFormat)
		}
	default:
		return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportBackend)
	}
	r.host = hostconn.HostID(strings.TrimSpace(req.GetTargetHostId()))
	if r.host == "" {
		return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportNoHost)
	}
	r.owner = ownerFromProto(req.GetTargetVm())
	if !hasNamingIdentity(r.owner) || !canonicalUUIDRE.MatchString(r.owner.UID) {
		return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportNoIdentity)
	}
	if hint := strings.TrimSpace(req.GetStorageHint()); hint != "" && hint != defaultStoragePool {
		return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportPool)
	}
	volumeName, err := importVolumeName(req)
	if err != nil {
		return routedImport{}, err
	}
	domain, err := domainNameFor(r.owner, "")
	if err != nil {
		return routedImport{}, status.Error(codes.InvalidArgument, wireRoutedImportNoIdentity)
	}
	r.domain, r.volumeName = domain, volumeName
	return r, nil
}

// importDiskRouted is the CLUSTERED ImportDisk (see the file comment). It
// returns wire errors.
func (p *Provider) importDiskRouted(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	r, err := parseRoutedImport(req)
	if err != nil {
		return nil, err
	}
	// The pod is the S3 client: its configuration is refused before any host
	// is leased. Credentials are never logged.
	var s3client storage.Storage
	if r.backend == migration.BackendS3 {
		cfg, err := migration.S3StorageConfigFromRequest(req.GetStorageOptionsJson(), req.GetCredentials())
		if err != nil {
			return nil, refusal(codes.InvalidArgument, wireImportRefused, fmt.Errorf("invalid s3 import configuration: %w", err))
		}
		if s3client, err = storage.NewStorage(cfg); err != nil {
			return nil, notPerformed(wireImportNotPerformed, fmt.Errorf("create the s3 client: %w", err))
		}
		defer func() { _ = s3client.Close() }()
	}

	bctx, cancel := withRoutedBudget(ctx)
	defer cancel()
	// The landing lock is held from the checks to the publish. Its wait is
	// bounded (routedImportLandingWait), so an import queued behind another
	// one of the same name answers "busy" with most of its budget unspent
	// instead of starting late and running out of it.
	lctx, lcancel := context.WithTimeout(bctx, routedImportLandingWait)
	unlock, err := landingLocks.lockForImport(lctx, routedLandingKey(r.volumeName))
	lcancel()
	if err != nil {
		log.Printf("WARN routed disk import of %s on host %s: gave up waiting for other work on the landing disk: %v",
			r.volumeName, r.host, err)
		return nil, vmOperationStatus(codes.Unavailable, wireRoutedImportLandingBusy)
	}
	defer unlock()

	prog := &importProgress{}
	var resp *providerv1.ImportDiskResponse
	err = p.withHostConn(bctx, string(r.host), func(bctx context.Context, c libvirtConn) error {
		out, err := p.importOnHost(bctx, c, r, prog, s3client, req)
		resp = out
		return err
	})
	if err != nil {
		return nil, classifyRoutedImportFailure(string(r.host), ctx, bctx, prog, err,
			append(nfsURLLocations(r.nfsURL), req.GetSourceUrl())...)
	}
	log.Printf("INFO Routed disk import landed %s on host %s for %s/%s", resp.GetPath(), r.host, r.owner.Namespace, r.owner.Name)
	return resp, nil
}

// importOnHost runs the routed import on the landing host's leased connection
// c: the checks of B2 (name, landing file, locks), then the shared write half
// over the guarded connection.
func (p *Provider) importOnHost(ctx context.Context, c libvirtConn, r routedImport, prog *importProgress,
	s3client storage.Storage, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.routedDiskConnFn != nil {
		c = p.routedDiskConnFn(c)
	}
	if err := p.checkHostDiskTransport(c); err != nil {
		return nil, refusal(codes.FailedPrecondition, wireImportNeedsSSH, err)
	}
	reader := &routedImportConn{libvirtConn: c, progress: prog}
	h := hostConnRunner{conn: reader}

	// B2 step 1: the landing host's name check, before any file is looked at.
	if err := landingDomainCheck(ctx, h, c.HostID(), r.domain, r.owner); err != nil {
		return nil, err
	}
	poolPath, err := defaultPoolPath(ctx, reader)
	if err != nil {
		return nil, fmt.Errorf("resolve the default pool's directory: %w", err)
	}
	// The pool must be where Create looks for the disk it attaches in place
	// (the routed import never creates or moves a pool).
	if err := requireDefaultPoolDir(ctx, h, c.HostID(), poolPath); err != nil {
		return nil, err
	}
	targetPath := filepath.Join(poolPath, r.volumeName+qcow2Ext)

	// B2 step 2: the landing file through A6.1's cluster-wide guard.
	g := p.newClusterDiskGuard(c.HostID(), r.owner, r.domain, "", guardOpImport)
	if _, err := g.checkDiskFree(ctx, h, importedDiskSubject(r.volumeName), poolPath, targetPath); err != nil {
		return nil, err
	}

	// Removals of the import's own private files run under timeout(1) only:
	// never under the import's flock, which a still-running command of an
	// earlier, cancelled call may hold (its directory would be leaked), and
	// never counted as a command that may still be landing a disk.
	cleanup := &routedImportConn{libvirtConn: c, progress: prog, cleanup: true}
	// Private write directories that outlived every call that could still
	// write into them (a cleanup that was stopped or failed) are reaped
	// first, under the landing lock (#415).
	reapStaleWriteDirs(ctx, hostConnRunner{conn: cleanup}, c.HostID(), poolPath)

	// The write phase needs room in the budget; with too little left it is
	// not started at all (not performed), rather than cut short (unknown).
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < routedImportMinWriteBudget {
		log.Printf("WARN Routed disk import of %s on host %s not started: %v of its budget left (less than %v)",
			r.volumeName, c.HostID(), time.Until(deadline).Round(time.Second), routedImportMinWriteBudget)
		return nil, vmOperationStatus(codes.Unavailable, wireRoutedImportBudgetShort)
	}

	// B2 step 3 + B3: from here every host command runs under flock(1) on the
	// import's host lock and timeout(1) within the budget.
	lock, err := p.hostLockFor(importLockKind, r.domain)
	if err != nil {
		return nil, err
	}
	prog.beginWrites()
	l := importLanding{
		conn:          &routedImportConn{libvirtConn: c, progress: prog, lock: &lock},
		cleanupRunner: hostConnRunner{conn: cleanup},
		poolName:      defaultStoragePool,
		poolPath:      poolPath,
		volumeName:    r.volumeName,
		targetPath:    targetPath,
		routed:        true,
	}
	log.Printf("INFO Routed disk import: backend=%s host=%s volume=%s target=%s", r.backend, c.HostID(), r.volumeName, targetPath)
	if r.backend == migration.BackendNFS {
		return landNFSImport(ctx, l, r.nfsURL, req)
	}
	return landS3Import(ctx, l, s3client, req)
}

// requireDefaultPoolDir refuses a landing host whose "default" pool is not at
// defaultPoolRequiredDir (lexically, or once both are resolved on the host
// behind h), with Create's constant InvalidSpec (errDefaultPoolMisplaced): a
// disk landed elsewhere could never be attached in place by the Create that
// follows. Nothing is changed on the host.
func requireDefaultPoolDir(ctx context.Context, h hostCommandRunner, host hostconn.HostID, dir string) error {
	// #415: the refusal is scoped to the host (FailedPrecondition +
	// HOST_STORAGE_MISCONFIGURED + VM_OPERATION_FAILED), so the manager can
	// place the target elsewhere instead of failing the migration for good.
	if filepath.Clean(dir) == filepath.Clean(defaultPoolRequiredDir) {
		return nil
	}
	canon, err := canonicalizeOnHost(ctx, h, []string{dir, defaultPoolRequiredDir})
	if err != nil {
		return err
	}
	if canon[0] == canon[1] {
		return nil
	}
	log.Printf("ERROR The libvirt storage pool %q on host %s is at %q, not %s; the routed import refuses to land there",
		defaultStoragePool, host, dir, defaultPoolRequiredDir)
	return statusWithReasons(codes.FailedPrecondition, fmt.Sprintf(wireRoutedImportPoolMisplacedFmt, host),
		contracts.HostStorageMisconfiguredReason, contracts.VMOperationFailedReason).Err()
}

// redactedCause is an error whose text has the staged object's locations
// replaced (redactLocations); its chain is the original error's.
type redactedCause struct {
	text string
	err  error
}

// Error returns the redacted text.
func (e *redactedCause) Error() string { return e.text }

// Unwrap returns the original error.
func (e *redactedCause) Unwrap() error { return e.err }

// stagedLocationMarker replaces a staged object's location in a logged text.
const stagedLocationMarker = "<staged object>"

// redactLocations returns err with every URL in its text redacted
// (redactCommandText: an S3 client error can carry the endpoint URL) and every
// non-empty location replaced by stagedLocationMarker, for the provider log: a
// staged object's nfs:// URL or s3:// key names the tenant's staging export or
// bucket.
func redactLocations(err error, locations ...string) error {
	if err == nil {
		return nil
	}
	return &redactedCause{text: redactText(redactCommandText(err.Error()), locations...), err: err}
}

// redactText is redactLocations on a text.
func redactText(text string, locations ...string) string {
	for _, l := range locations {
		if l = strings.TrimSpace(l); l != "" {
			text = strings.ReplaceAll(text, l, stagedLocationMarker)
		}
	}
	return text
}

// landingDomainCheck is the routed import's name check on the landing host
// behind h (ADR-0007 Addendum B, B2 step 1), with bindExistingDomain's
// ownership semantics: no domain named domain is the common case; one stamped
// with owner's own UID is a previousIncarnationError of the own kind (the VM
// was already created there); one stamped for owner's namespace and name under
// another UID is a previous incarnation; any other (foreign, unstamped,
// unreadable stamp) is a plain Conflict. Only read-only virsh queries run; a
// read that fails is a retryable error, never a decision.
func landingDomainCheck(ctx context.Context, h hostCommandRunner, host hostconn.HostID, domain string, owner contracts.ObjectIdentity) error {
	list, err := h.runVirshCommand(ctx, "list", "--all")
	if err != nil {
		return contracts.NewRetryableError("failed to list domains", err)
	}
	found := false
	for _, d := range parseDomainListTable(list.Stdout) {
		if d.Name == domain {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	res, err := h.runVirshCommand(ctx, "dumpxml", domain)
	if err != nil {
		return contracts.NewRetryableError(fmt.Sprintf("read owner metadata of domain %q", domain), err)
	}
	recorded, perr := domainOwners(res.Stdout)
	switch {
	case perr == nil && requesterOwnsDomain(owner, recorded):
		log.Printf("WARN Refusing the import for %s/%s on host %s: domain %s is already stamped with its own UID (%s); "+
			"the VirtualMachine was already created, so the import is not repeated", owner.Namespace, owner.Name, host, domain, owner.UID)
		return &previousIncarnationError{op: guardOpImport, domain: domain, own: true, onLandingHost: true}
	case perr == nil && stampsNameOwner(recorded, owner):
		log.Printf("WARN Refusing the import for %s/%s on host %s: domain %s is a previous incarnation (stamped for its "+
			"namespace and name under another UID)", owner.Namespace, owner.Name, host, domain)
		return &previousIncarnationError{op: guardOpImport, domain: domain}
	case perr != nil:
		log.Printf("WARN Refusing the import for %s/%s on host %s: the owner metadata of domain %s could not be read: %v",
			owner.Namespace, owner.Name, host, domain, perr)
	default:
		log.Printf("WARN Refusing the import for %s/%s on host %s: domain %s exists and is owned by %v",
			owner.Namespace, owner.Name, host, domain, recorded)
	}
	return contracts.NewConflictError(wireRoutedImportNameTaken, nil)
}

// importProgress records how far a routed import got on its landing host, for
// classifyRoutedImportFailure. It is safe for concurrent use.
type importProgress struct {
	mu sync.Mutex
	// answered: a host command of this call came back with an answer from
	// the host (an exit status of its own), so the host was reached.
	answered bool
	// writes: the write phase began (the private directory is made next).
	writes bool
	// lost: a write-phase command did not end on its own — stopped by
	// timeout(1), killed, its session dropped or its call cancelled — and may
	// still run. Commands of the cleanup connection (routedImportConn.cleanup:
	// removals of the import's own private files, the reaper) are never
	// counted: they cannot land a disk.
	lost bool
}

// beginWrites marks the start of the write phase.
func (p *importProgress) beginWrites() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = true
}

// record notes the result err of a host command, run in the write phase (by
// the guarded connection) when write is set.
func (p *importProgress) record(ctx context.Context, write bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil || hostAnswered(err) {
		p.answered = true
	}
	if write && err != nil && !endedOnItsOwn(ctx, err) {
		p.lost = true
	}
}

// snapshot returns the recorded progress.
func (p *importProgress) snapshot() (answered, writes, lost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.answered, p.writes, p.lost
}

// hostAnswered reports whether err is a host command that the host itself
// answered: an exit status of its own, not a failure to reach the host.
func hostAnswered(err error) bool {
	var ve *VirshError
	return stderrors.As(err, &ve) && ve.ExitCode >= 0 && !isHostTransportFailure(err)
}

// endedOnItsOwn reports whether a failed host command is known to have
// stopped: it ended with an exit status of its own (hostCommandEnded) other
// than timeout(1)'s "timed out" (ADR-0007 Addendum B, B4 counts a command
// stopped by timeout(1) as an unknown outcome).
func endedOnItsOwn(ctx context.Context, err error) bool {
	return hostCommandEnded(ctx, err) && guardExitCode(err) != timeoutExpiredExit
}

// routedImportConn is the landing host's connection as the routed import
// uses it. Every command's result is recorded in progress. With lock set (the
// write phase) every command runs through the host guard: hostGuardScript,
// flock(1) -n on lock and timeout(1) ending hostCommandSlack before the
// command's context deadline (guardedHostCommand). A virsh command then runs
// as a host command (`virsh -c <uri> ...`) so it is guarded too. With cleanup
// set (the import's own removals and the reaper) every command runs under
// timeout(1) only — never under the import's flock, which a still-running
// command of an earlier call may hold — and is never counted as lost.
type routedImportConn struct {
	libvirtConn
	progress *importProgress
	lock     *hostLock
	cleanup  bool
}

// wrap returns the argv the host runs for argv: guarded (write phase), under
// timeout(1) only (cleanup), or unchanged (read phase).
func (c *routedImportConn) wrap(ctx context.Context, argv []string) ([]string, error) {
	switch {
	case c.lock != nil:
		return guardedHostCommand(ctx, *c.lock, "", argv...)
	case c.cleanup:
		timeout, err := hostCommandTimeout(ctx)
		if err != nil {
			return nil, err
		}
		return append(timeout, argv...), nil
	}
	return argv, nil
}

// RunHost runs argv on the host, guarded in the write phase.
func (c *routedImportConn) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	run, err := c.wrap(ctx, argv)
	if err != nil {
		return nil, err
	}
	res, err := c.libvirtConn.RunHost(ctx, run...)
	c.progress.record(ctx, c.lock != nil, err)
	return res, err
}

// Virsh runs a virsh command on the host; in the write phase (and for the
// cleanup connection) it runs as a host command against the same libvirtd
// (remoteVirshConnectURI), so it is wrapped too.
func (c *routedImportConn) Virsh(ctx context.Context, args ...string) (*hostconn.Result, error) {
	if c.lock == nil && !c.cleanup {
		res, err := c.libvirtConn.Virsh(ctx, args...)
		c.progress.record(ctx, false, err)
		return res, err
	}
	argv := []string{"virsh"}
	if uri := remoteVirshConnectURI(c.uri()); uri != "" {
		argv = append(argv, "-c", uri)
	}
	return c.RunHost(ctx, append(argv, args...)...)
}

// StreamIn runs argv on the host with r as its stdin, guarded in the write
// phase. Its failure is reported as a *VirshError carrying the command's exit
// status (or -1 when the session failed), as RunHost's are.
func (c *routedImportConn) StreamIn(ctx context.Context, r io.Reader, argv ...string) error {
	run, err := c.wrap(ctx, argv)
	if err != nil {
		return err
	}
	err = streamInError(c.libvirtConn.StreamIn(ctx, r, run...))
	c.progress.record(ctx, c.lock != nil, err)
	return err
}

// streamInError normalises a StreamIn failure to the *VirshError shape
// RunHost failures have: the remote command's exit status (ssh.ExitError, or
// exec.ExitError on a local connection), or -1 when the session failed. A
// cancelled call and an error already in that shape are returned unchanged.
func streamInError(err error) error {
	if err == nil || stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ve *VirshError
	if stderrors.As(err, &ve) {
		return err
	}
	code := -1
	var sshExit *ssh.ExitError
	var localExit *exec.ExitError
	switch {
	case stderrors.As(err, &sshExit):
		code = sshExit.ExitStatus()
	case stderrors.As(err, &localExit):
		code = localExit.ExitCode()
	}
	return &VirshError{Command: "stream-in", ExitCode: code, Stderr: err.Error(), Cause: err}
}

// classifyRoutedImportFailure turns the failure of a routed import on host
// into the answer the manager reads (ADR-0007 Addendum B, B4; see the file
// comment). parent is the call's context, budget the routed budget, prog what
// the import recorded. locations are the staged object's locations (the
// source URL): they are never logged (#415), the cause is logged with them
// replaced.
func classifyRoutedImportFailure(host string, parent, budget context.Context, prog *importProgress, err error,
	locations ...string) error {
	if err == nil {
		return nil
	}
	cause := redactLocations(err, locations...)
	log.Printf("WARN routed disk import on host %s failed: %v", host, cause)
	// A symbolic link at the landing name: a host-guard refusal, never
	// written through (#415: not the plain AlreadyExists of a name conflict).
	var se *symlinkTargetError
	if stderrors.As(err, &se) {
		obsmetrics.RecordHostGuardRefusal(libvirtProviderType, obsmetrics.HostGuardReasonTargetSymlink)
		log.Printf("ERROR ALERT host guard refused disk import on host %q (%s): the landing name is a symbolic link; "+
			"check the default pool on that host", host, obsmetrics.HostGuardReasonTargetSymlink)
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf(wireRoutedImportLandingSymlinkFmt, host))
	}
	// The cluster-wide guard's answers: a previous incarnation, a host that
	// could not be checked, the domain busy.
	if st := clusterGuardStatus(err); st != nil {
		return st.Err()
	}
	// A refusal keeps its type (InvalidArgument, AlreadyExists,
	// FailedPrecondition, the disk-size reasons, ResourceExhausted as "not
	// performed"), with its own constant text.
	if typed := definiteStatusError(err); typed != nil {
		return typed
	}
	answered, writes, lost := prog.snapshot()
	if !writes {
		// Only reads ran on the landing host.
		if contracts.IsHostUnavailable(err) || (!answered && isHostTransportFailure(err)) {
			return hostUnavailableStatus(contracts.NewHostUnavailableError(fmt.Sprintf(wireRoutedImportHostDownFmt, host), nil))
		}
		return notPerformed(wireImportNotPerformed, cause)
	}
	switch guardExitCode(err) {
	case flockBusyExit:
		return vmOperationStatus(codes.Unavailable, fmt.Sprintf(wireRoutedImportInProgressFmt, host))
	case hostGuardRefusedExit:
		reportHostGuardRefusal("disk import", host, err)
		return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf(wireRoutedImportGuardRefusedFmt, host))
	case commandNotFound, commandNotRunnable:
		if !lost {
			return vmOperationStatus(codes.FailedPrecondition, fmt.Sprintf(wireRoutedImportToolsMissingFmt, host))
		}
	}
	if lost || budgetRanOut(parent, budget) || parent.Err() != nil {
		log.Printf("WARN routed disk import on host %s: outcome unknown (a host command may still run, or the budget ran out)", host)
		return vmOperationStatus(codes.Unknown, fmt.Sprintf(wireRoutedImportUnknownFmt, host))
	}
	// Every command ended with an exit status of its own: nothing is still
	// running, and the only publishing step either did not run or ended
	// without renaming.
	if stage, _, _ := stageOf(err); stage == stagePublish {
		return notPerformed(wireImportFailed, cause)
	}
	return notPerformed(wireImportNotPerformed, cause)
}
