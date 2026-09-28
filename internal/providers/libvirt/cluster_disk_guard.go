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
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Cluster-wide disk guard of a CLUSTERED provider (ADR-0007 A6, R3 and R2;
// slice A6.1).
//
// The hosts of one clustered Provider may share a storage pool directory (an
// NFS export mounted on every host, ADR-0007 D6). A VM's disk file is then
// visible — and writable — from every host, but each host's libvirtd knows
// only its own domains, so the host-local in-use checks (ensureDiskTargetFree,
// planDomainDeletion) cannot see a domain on ANOTHER host that uses the file.
// Two operations could then destroy a disk in use:
//
//   - a Create or Clone of <namespace>/<name> on host B writing
//     <pool>/<namespace>.<name>-disk.qcow2 while a domain of the same name still
//     exists on host A — after orphan-on-delete, a force-delete that left the
//     domain behind, or a backup restore that gave the VirtualMachine a new UID;
//   - a Delete on host B removing a disk file that a domain on host A uses.
//
// The guard closes both:
//
//   - Create and Clone: every name the VM's disk may have had in the storage
//     pool — "<domain>-disk" (blank), "<domain>-disk.qcow2" (from an image, or
//     a clone), "<domain>-migrated.qcow2" (imported), and the same for the
//     legacy bare name (diskFileNames) — is probed on the landing host. When
//     any of them exists, EVERY host of the provider's registry is scanned
//     once — the landing host too — and the file about to be written (a blank
//     volume, an image copy, a clone, or an imported disk attached in place) is
//     written only when no domain on any host uses it. The common case, with
//     nothing there, costs two host commands on the landing host and scans
//     nothing. A clone's UEFI varstore that already exists keeps the
//     host-local check when it resolves into the host-local NVRAM directory,
//     and is scanned on every host otherwise (ensureVarstoreFree).
//   - Delete: after the host-local plan (planDomainDeletion) and before
//     anything is destroyed, undefined or removed, every OTHER host is scanned
//     for a domain using one of the files the delete would remove.
//
// "Uses" is the host-local check's definition, per host (domainRefsOnHost):
// any domain defined there, running or not, VirtRigaud's or anyone else's,
// that references the file as a disk, anywhere in a disk's backing chain, or as
// another file or shared directory.
//
// Paths are compared canonically ON EACH HOST: the candidate — as the landing
// host names it, and as it resolves there — is resolved again with
// `realpath -m` on the scanned host and compared with that host's raw and
// canonical references; a Delete's seed directory is resolved on each host
// too. A candidate in a host-local directory (hostLocalDirs: the NVRAM
// directory) is compared on the operation's own host only. Each host is
// scanned through its Host endpoint's libvirt instance (qemu+ssh://…/system
// or …/session): domains of the other instance on that host are not seen.
// Residuals (documented): a host that mounts the shared export under a
// DIFFERENT path, reaches the file through a second mount, a bind mount or a
// hard link (realpath resolves none of them), or uses it as a protocol disk
// (nbd, rbd, iSCSI: no host path), is not matched. (st_dev, st_ino) cannot
// close that gap: st_dev is assigned by each NFS client, so it differs between
// hosts for the same file, and st_ino alone is not unique across filesystems.
// Mount a shared pool at the same path on every host of a Provider (libvirt's
// shared-storage migration requires that too). The converse residual: on
// host-LOCAL pools the same path on two hosts is two files, so a domain on
// another host with the same disk path is reported as a use (a false "in
// use", fail-safe) until a pool ownership marker says which hosts share a pool
// (follow-up).
//
// Fail closed: a host that cannot be checked — not leased (unknown, draining,
// unreachable), past its deadline, or whose domains or disk chains cannot be
// read — fails the operation and nothing is written or removed. A host that
// could not be reached is answered as a host-scoped Unavailable
// (HOST_UNAVAILABLE); a host that answered but could not be scanned as
// VM_DISK_CHECK_FAILED + VM_OPERATION_FAILED. Both are retryable and kept out of
// the manager's per-Provider circuit breaker, and neither names another host.
//
// Previous incarnation (R2): while it scans, the guard also counts the domains
// whose owner stamp names the requesting VirtualMachine's namespace and name
// (A6 decision 2: at most one domain VirtRigaud created per <namespace>.<name>
// per clustered Provider). Any such domain, on any host, answers a Create or
// Clone with AlreadyExists + VM_PREVIOUS_INCARNATION, and the manager holds the
// VM on its pending host. A domain that uses the file but is stamped for
// anything else (or not at all) is a plain AlreadyExists: the slice 2
// exclusion. On the landing host the same answer comes from the name check
// (bindExistingDomain, cloneOnHost), which runs before any file is looked at.
//
// Bounded: the fan-out is slice 4's (fanOutHosts, onHostWithin): at most
// effectiveListHostConcurrency hosts at a time, each on its own lease with the
// per-host deadline effectiveListHostTimeout, a host with more than
// clusteredListMaxDomainsPerHost domains fails closed instead of being read,
// and the whole scan runs inside the caller's deadline less
// clusterDiskGuardMargin, so the operation's own work — and its answer — still
// fits. The landing host of a Create or Clone is scanned over the call's own
// connection, never re-leased.

// Operations the cluster-wide guard protects, as named in its answers (Delete
// uses guardOpDelete).
const (
	guardOpCreate = "create"
	guardOpClone  = "clone"
)

// clusterDiskGuardMargin is kept back from the caller's deadline by the
// cluster-wide disk scan, so the operation that asked for it (a Delete's
// teardown, a Create's disk write) and its answer still fit.
const clusterDiskGuardMargin = 30 * time.Second

// clusterDiskGuard is the cluster-wide disk guard of one clustered Create or
// Clone: the VirtualMachine the disk is written for (owner), its domain name,
// and the host it lands on (whose connection — the call's own — each check is
// handed). A nil guard is the single-host behaviour: the host-local
// ensureDiskTargetFree.
type clusterDiskGuard struct {
	p *Provider
	// host is the landing host.
	host hostconn.HostID
	// owner is the VirtualMachine the disk is written for (a Create's owner, a
	// Clone's target VM).
	owner contracts.ObjectIdentity
	// domain is the domain being created, and op the operation (guardOp*),
	// for the answers.
	domain string
	op     string
	// legacy is the VM's bare, pre-namespacing domain name when it has one
	// (legacyNameOf), "" otherwise: a disk of an earlier incarnation may carry
	// it.
	legacy string
}

// newClusterDiskGuard returns the guard of a clustered op of domain for owner,
// landing on host; legacy is the VM's bare domain name, or "".
func (p *Provider) newClusterDiskGuard(host hostconn.HostID, owner contracts.ObjectIdentity, domain, legacy, op string) *clusterDiskGuard {
	return &clusterDiskGuard{p: p, host: host, owner: owner, domain: domain, legacy: legacy, op: op}
}

// diskFileNames are the names, in the storage pool directory, that a disk of
// the VM this guard protects may have had — in THIS incarnation or an earlier
// one, whichever way it was made — for the domain name and the legacy bare
// name alike:
//
//   - "<domain>-disk": a blank volume (vol-create names the file after the
//     volume, without an extension);
//   - "<domain>-disk.qcow2": a disk copied from an image or cloned;
//   - "<domain>-migrated.qcow2": an imported migration disk attached in place.
//
// A VM re-created with another disk kind than its previous incarnation (a
// blank VM re-created from an image, or the reverse) would otherwise never
// meet the earlier disk, and its previous incarnation would stay unseen.
func (g *clusterDiskGuard) diskFileNames() []string {
	var out []string
	for _, d := range []string{g.domain, g.legacy} {
		if d == "" {
			continue
		}
		vol := vmDiskVolumeName(d)
		out = append(out, vol, vol+qcow2Ext, importedVolumeFileName(d))
	}
	return out
}

// ensureDiskFree refuses to let the create or clone write the VM's disk file
// target (subject, for the answer) in the storage pool directory poolDir, on
// host h (the landing host). On a single-host provider (nil g) it is exactly
// ensureDiskTargetFree. On a clustered one:
//
//   - a symbolic link at target is refused, as there;
//   - every name the VM's disk may have had (diskFileNames) is probed on h, in
//     one command;
//   - when none of them — target included — exists, nothing is scanned (the
//     common case);
//   - otherwise every host of the Provider is scanned once, for all of the
//     existing names (refuseIfUsed): a previous incarnation of the VM anywhere
//     holds it, a domain that uses target refuses the write, and a host that
//     cannot be checked fails it closed. An existing target no domain uses is a
//     leftover of an earlier, failed attempt for this very name, and may be
//     replaced.
func (g *clusterDiskGuard) ensureDiskFree(ctx context.Context, h hostCommandRunner, subject, poolDir, target string) error {
	if g == nil {
		return ensureDiskTargetFree(ctx, h, subject, target)
	}
	targetExists, err := checkWriteTarget(ctx, h, subject, target)
	if err != nil {
		return err
	}
	var candidates []string
	for _, name := range g.diskFileNames() {
		if p := filepath.Join(poolDir, name); p != target && filepath.Dir(p) == filepath.Clean(poolDir) {
			candidates = append(candidates, p)
		}
	}
	others, err := existingHostPaths(ctx, h, candidates)
	if err != nil {
		return err
	}
	if !targetExists && len(others) == 0 {
		return nil
	}
	var targets []string
	if targetExists {
		targets = []string{target}
	}
	return g.refuseIfUsed(ctx, h, subject, targets, others)
}

// refuseIfUsed scans every host of the Provider once for the existing files
// targets (the file about to be written) and others (other names a disk of
// the VM may have had), as host h (the landing host) names and resolves them,
// and decides (see the file comment): a domain stamped for the owner's
// namespace and name on any host is a previous incarnation
// (previousIncarnationError); a domain that uses a target refuses the write
// (Conflict); a host that cannot be checked fails it closed
// (clusterGuardIncompleteError). A use of an OTHER name by a domain that is not
// a previous incarnation refuses nothing: that file is not written.
func (g *clusterDiskGuard) refuseIfUsed(ctx context.Context, h hostCommandRunner, subject string, targets, others []string) error {
	raw := append(append([]string(nil), targets...), others...)
	canon, err := canonicalizeOnHost(ctx, h, raw)
	if err != nil {
		return err
	}
	var files []string
	isTarget := map[string]bool{}
	add := func(p string, target bool) {
		if !slices.Contains(files, p) {
			files = append(files, p)
		}
		if target {
			isTarget[p] = true
		}
	}
	for i, p := range raw {
		add(p, i < len(targets))
		add(canon[i], i < len(targets))
	}
	res, err := g.p.scanClusterDiskUse(ctx, clusterScan{files: files, owner: g.owner, target: g.host, conn: h,
		// A previous incarnation holds the VM whatever the other hosts say.
		stopWhen: func(r hostDiskScan) bool { return r.incarnations > 0 || r.ownElsewhere > 0 },
	})
	targetUsed := false
	for i, used := range res.used {
		targetUsed = targetUsed || (used && isTarget[files[i]])
	}
	switch {
	case res.ownElsewhere > 0:
		log.Printf("WARN Refusing %s of libvirt domain %s on host %s: %d domain(s) on the Provider's hosts are stamped with "+
			"%s/%s's own UID (its own domain elsewhere; ADR-0007 A6)", g.op, g.domain, g.host, res.ownElsewhere, g.owner.Namespace, g.owner.Name)
		return &previousIncarnationError{op: g.op, domain: g.domain, own: true}
	case res.incarnations > 0:
		log.Printf("WARN Refusing %s of libvirt domain %s on host %s: %d domain(s) on the Provider's hosts are stamped for %s/%s "+
			"(a previous incarnation; ADR-0007 A6)", g.op, g.domain, g.host, res.incarnations, g.owner.Namespace, g.owner.Name)
		return &previousIncarnationError{op: g.op, domain: g.domain}
	case targetUsed:
		log.Printf("WARN Refusing to write %s at %v on host %s: a domain on the Provider's hosts uses it", subject, targets, g.host)
		return contracts.NewConflictError(fmt.Sprintf(
			"%s already exists and is in use by another domain on a host of this Provider; refusing to overwrite it", subject), nil)
	case err != nil:
		return g.incomplete(err)
	}
	if len(targets) > 0 {
		log.Printf("INFO %s exists at %s on host %s but no domain on any host of the Provider uses it "+
			"(left by an earlier failed attempt); overwriting it", subject, targets[0], g.host)
	}
	return nil
}

// varstoreSubject names a clone's UEFI varstore in the guard's answers (the
// wording of ensureNVRAMTargetFree's).
func varstoreSubject(domainName string) string {
	return fmt.Sprintf("the UEFI varstore path of libvirt domain %q", domainName)
}

// ensureVarstoreFree refuses to let the clustered clone this guard protects
// write its UEFI varstore at target on host h (the landing host). A symbolic
// link there is refused and a free path needs nothing more, as on a single
// host. For an existing file, where it RESOLVES decides the check: in a
// host-local directory (hostLocalDirs: the NVRAM directory, never shared) the
// host-local check is the whole check (ensureNVRAMTargetFree's); anywhere
// else the file may be on shared storage — rewriteNVRAMPath keeps the
// SOURCE's varstore directory, which need not be the NVRAM directory — so
// every host of the Provider is scanned for a domain that uses it, as for a
// disk (refuseIfUsed; A6.1 fix verification, N3).
func (g *clusterDiskGuard) ensureVarstoreFree(ctx context.Context, h hostCommandRunner, target string) error {
	subject := varstoreSubject(g.domain)
	exists, err := checkWriteTarget(ctx, h, subject, target)
	if err != nil || !exists {
		return err
	}
	canon, err := canonicalizeOnHost(ctx, h, []string{target})
	if err != nil {
		return err
	}
	if inHostLocalDir(canon[0]) {
		return refuseNVRAMInUseOnHost(ctx, h, g.domain, target)
	}
	return g.refuseIfUsed(ctx, h, subject, []string{target}, nil)
}

// imageUsedElsewhere reports whether a domain on another host of the Provider
// uses the base image a create is about to copy — raw as the tenant named it,
// canonical as the landing host h resolved it (imagePathRequest.UsedElsewhere;
// ADR-0007 A6.1 security review). The host-local confinement already checked
// h; copying a live disk of another host would hand its content to this VM's
// tenant. The first use decides; a host that cannot be checked fails closed
// (clusterGuardIncompleteError).
func (g *clusterDiskGuard) imageUsedElsewhere(ctx context.Context, _ hostCommandRunner, raw, canonical string) (bool, error) {
	files := []string{raw}
	if canonical != raw {
		files = append(files, canonical)
	}
	// The first use, or the first host that cannot be checked, decides.
	res, err := g.p.scanClusterDiskUse(ctx, clusterScan{files: files, target: g.host, skipTarget: true,
		stopWhen:    func(r hostDiskScan) bool { return r.users > 0 },
		stopOnError: true,
	})
	if res.users > 0 {
		return true, nil
	}
	if err != nil {
		return false, g.incomplete(err)
	}
	return false, nil
}

// existingPathsScript is the fixed `sh -c` script behind existingHostPaths.
// The paths are ALWAYS the positional parameters, never interpolated into the
// text. It prints, one per line, each of them that exists (a symbolic link,
// dangling or not, included). It holds no single quote, so it can be matched
// verbatim by a quoting shell (the test fixtures do).
const existingPathsScript = `for p; do if [ -e "$p" ] || [ -L "$p" ]; then printf "%s\n" "$p"; fi; done`

// existingHostPaths returns those of paths that exist on the host behind h, in
// one command. An answer that is not one of the paths fails the check (a
// generic retryable error), as does a failure to run it.
func existingHostPaths(ctx context.Context, h hostCommandRunner, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	res, err := runHost(ctx, h, append([]string{"sh", "-c", existingPathsScript, "sh"}, paths...)...)
	if err != nil {
		return nil, hostCheckFailed("probe the disk names of the VM", err)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line == "" {
			continue
		}
		if !slices.Contains(paths, line) {
			return nil, hostCheckFailed("probe the disk names of the VM", fmt.Errorf("unexpected output line %q", line))
		}
		out = append(out, line)
	}
	return out, nil
}

// incomplete turns a scan that could not check every host into the refusal
// of the guard's operation; the caller's own cancellation passes unchanged.
func (g *clusterDiskGuard) incomplete(err error) error {
	var ie *clusterGuardIncompleteError
	if stderrors.As(err, &ie) {
		return ie.with(g.op, g.domain)
	}
	return err
}

// stampsNameOwner reports whether any of the recorded owner stamps names
// owner's namespace and name — a domain VirtRigaud created for that namespace
// and name (ADR-0007 A6 decision 2). The UID is deliberately not compared: on
// a clustered Provider a second domain for the same namespace and name is
// never created, whether the existing one is stamped with another UID (a
// previous incarnation) or with the requester's own (its domain on another
// host). An owner without a namespace or name matches nothing.
func stampsNameOwner(recorded []contracts.ObjectIdentity, owner contracts.ObjectIdentity) bool {
	if owner.Namespace == "" || owner.Name == "" {
		return false
	}
	for _, r := range recorded {
		if r.Namespace == owner.Namespace && r.Name == owner.Name {
			return true
		}
	}
	return false
}

// incarnationKinds classifies the recorded owner stamps of one domain against
// the requester owner (ADR-0007 A6, decision 2): own is true when a stamp names
// owner's namespace and name with owner's own UID — the requester's own domain
// (its placement record was lost) — and previous when one names them under
// another UID — a previous incarnation. A domain with two stamps can be both.
// An owner without a namespace or name matches nothing.
func incarnationKinds(recorded []contracts.ObjectIdentity, owner contracts.ObjectIdentity) (previous, own bool) {
	if owner.Namespace == "" || owner.Name == "" {
		return false, false
	}
	for _, r := range recorded {
		if r.Namespace != owner.Namespace || r.Name != owner.Name {
			continue
		}
		if owner.UID != "" && r.UID == owner.UID {
			own = true
		} else {
			previous = true
		}
	}
	return previous, own
}

// clusterScan is one cluster-wide disk scan.
type clusterScan struct {
	// files are the candidate files as the operation's host names them (raw
	// and canonical); each scanned host resolves them itself.
	files []string
	// seedDir, when set, is a directory the operation would remove (a
	// Delete's cloud-init seed directory): the scan reports whether a domain
	// references a file under it.
	seedDir string
	// owner, when its namespace and name are set, makes the scan count the
	// domains whose owner stamp names them (incarnationKinds: under another
	// UID, or with the owner's own).
	owner contracts.ObjectIdentity
	// target is the host the operation runs on and conn the operation's own
	// connection to it: that host is scanned over conn, never re-leased —
	// unless skipTarget, when it is not scanned at all (a Delete, whose
	// host-local plan already checked it).
	target     hostconn.HostID
	conn       hostCommandRunner
	skipTarget bool
	// stopWhen, when set, is asked after each host that answered: true means
	// that host's scan already decides the operation, so the hosts still
	// running are cancelled and the rest are not started (a Delete or an
	// image check: the first use; a Create or Clone: the first previous
	// incarnation).
	stopWhen func(r hostDiskScan) bool
	// stopOnError means that any host that cannot be checked decides the
	// operation (it fails closed whatever the other hosts say: a Delete, an
	// image check). The scan then stops at the first such host, and when a
	// host is ALREADY known to fail — tombstoned, or found unreachable less
	// than clusterGuardUnreachableMemo ago — it fails at once, before taking
	// a scan slot or dialing anything.
	stopOnError bool
}

// clusterScanResult sums what the scanned hosts reported.
type clusterScanResult struct {
	// users counts the domains that reference a candidate file.
	users int
	// used reports, per candidate file (clusterScan.files), whether a domain
	// references it.
	used []bool
	// incarnations counts the domains stamped with the owner's namespace and
	// name under another UID (previous incarnations).
	incarnations int
	// ownElsewhere counts the domains stamped with the owner's namespace, name
	// and own UID: its own domain, found where the operation does not expect
	// it (another host, or another name).
	ownElsewhere int
	// seedUsed reports whether a domain references a file under seedDir.
	seedUsed bool
}

// hostDiskScan is one host's part of a clusterScanResult.
type hostDiskScan = clusterScanResult

// add folds one host's scan r into the sum.
func (s *clusterScanResult) add(r clusterScanResult) {
	s.users += r.users
	s.incarnations += r.incarnations
	s.ownElsewhere += r.ownElsewhere
	s.seedUsed = s.seedUsed || r.seedUsed
	if len(s.used) < len(r.used) {
		s.used = append(s.used, make([]bool, len(r.used)-len(s.used))...)
	}
	for i, u := range r.used {
		s.used[i] = s.used[i] || u
	}
}

// scanClusterDiskUse runs s on every host of the registry (see the file
// comment) and sums the answers. It returns what the hosts that answered
// found, together with a *clusterGuardIncompleteError when any host could not
// be checked (the caller decides what a partial answer is worth: a use or an
// incarnation already found is definitive, an empty one is not), or the
// caller's context error when the caller gave up.
func (p *Provider) scanClusterDiskUse(ctx context.Context, s clusterScan) (clusterScanResult, error) {
	if p.clusterReg == nil {
		return clusterScanResult{}, contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil)
	}
	// The hosts the inventory names but that cannot be routed to (the
	// operator's tombstones, rejected entries) are scanned too — and fail
	// closed at once: they exist, and may use the file. Both sets come from
	// one registry snapshot, so a host moving between them mid-listing is
	// never missed.
	snap := p.clusterReg.Snapshot(clusterGuardUnreachableMemo)
	unroutable := map[hostconn.HostID]bool{}
	for _, id := range snap.Unroutable {
		unroutable[id] = true
	}
	hosts := guardHosts(append(slices.Clone(snap.Routable), snap.Unroutable...), s)
	if len(hosts) == 0 {
		return clusterScanResult{}, nil
	}
	// Hosts already known to fail: tombstoned, or found unreachable moments
	// ago (the landing host, scanned over the call's own connection, never
	// is). A scan that any failure decides (stopOnError) fails at once on
	// one — no scan slot taken, nothing dialed, no other host read for an
	// answer already known. Any other scan checks them first, so their
	// failures are known before the slow hosts are read.
	knownBad := map[hostconn.HostID]bool{}
	for _, id := range hosts {
		if id == s.target && s.conn != nil {
			continue
		}
		if unroutable[id] || slices.Contains(snap.RecentlyUnreachable, id) {
			knownBad[id] = true
		}
	}
	if s.stopOnError && len(knownBad) > 0 {
		log.Printf("WARN cluster disk guard: %d host(s) of the Provider are known to be unreachable (tombstoned, or "+
			"unreachable less than %s ago); failing closed without scanning", len(knownBad), clusterGuardUnreachableMemo)
		return clusterScanResult{}, &clusterGuardIncompleteError{unreachable: true}
	}
	slices.SortStableFunc(hosts, func(a, b hostconn.HostID) int {
		switch {
		case knownBad[a] == knownBad[b]:
			return 0
		case knownBad[a]:
			return -1
		default:
			return 1
		}
	})
	budget, cancel := budgetWithMargin(ctx, clusterDiskGuardMargin)
	defer cancel()

	// At most clusterGuardConcurrency scans run at once in this provider
	// process, so a burst of creates, clones or deletes (or a tenant retrying
	// them) cannot multiply the per-host reads. A scan that gets no slot within
	// the call's budget fails closed as busy (retried; never counted by the
	// breaker).
	if err := p.guardSemaphore().Acquire(budget, 1); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return clusterScanResult{}, cerr
		}
		log.Printf("WARN cluster disk guard: no scan slot within the call's budget (%d scans at a time); failing closed",
			clusterGuardConcurrency)
		return clusterScanResult{}, &clusterGuardIncompleteError{busy: true}
	}
	defer p.guardSemaphore().Release(1)

	var (
		mu      sync.Mutex
		decided bool // s.stopWhen settled the outcome; the other hosts are cancelled
		scans   = make([]hostDiskScan, len(hosts))
		errs    = make([]error, len(hosts))
		counted = make([]bool, len(hosts))
	)
	// finish records host i's outcome unless the scan was already decided
	// (then the host was cancelled, or finished too late to matter), and
	// decides — cancelling the hosts still running — when s.stopWhen says so.
	finish := func(i int, r hostDiskScan, err error) error {
		mu.Lock()
		defer mu.Unlock()
		if decided {
			return err
		}
		scans[i], errs[i], counted[i] = r, err, true
		if (err != nil && s.stopOnError) || (err == nil && s.stopWhen != nil && s.stopWhen(r)) {
			decided = true
			cancel()
		}
		return err
	}
	p.fanOutHosts(budget, len(hosts), 0, func(i int) error {
		id := hosts[i]
		if id == s.target && s.conn != nil {
			hctx, hcancel := context.WithTimeout(budget, p.effectiveListHostTimeout())
			defer hcancel()
			r, err := scanHostDiskUse(hctx, s.conn, s, false)
			if err == nil {
				err = hctx.Err()
			}
			return finish(i, r, err)
		}
		if unroutable[id] {
			return finish(i, hostDiskScan{}, contracts.NewHostUnavailableError(
				fmt.Sprintf("host %q is in the Provider's inventory but cannot be connected to", id), nil))
		}
		// A host found unreachable moments ago is failed at once, without
		// dialing it again (a dead host must not cost every scan its dial
		// timeout).
		if p.clusterReg.RecentlyUnreachable(id, clusterGuardUnreachableMemo) {
			return finish(i, hostDiskScan{}, contracts.NewHostUnavailableError(
				fmt.Sprintf("host %q was unreachable moments ago; not dialed again", id), nil))
		}
		var r hostDiskScan
		err := p.onHostWithin(budget, id, func(hctx context.Context, c libvirtConn) error {
			vp, verr := virshOf(c)
			if verr != nil {
				return verr
			}
			var serr error
			r, serr = scanHostDiskUse(hctx, vp, s, id != s.target)
			return serr
		})
		if isHostTransportFailure(err) || (stderrors.Is(err, context.DeadlineExceeded) && budget.Err() == nil) {
			p.clusterReg.MarkUnreachable(id)
		}
		return finish(i, r, err)
	})
	if err := ctx.Err(); err != nil {
		return clusterScanResult{}, err
	}

	var out clusterScanResult
	incomplete := &clusterGuardIncompleteError{}
	failed := false
	for i := range hosts {
		switch {
		case !counted[i] && decided:
			continue // cancelled once the outcome was decided
		case !counted[i]:
			// Never run: the budget was spent before its turn.
			log.Printf("WARN cluster disk guard: host %s was not checked within the call's budget; failing closed", hosts[i])
			failed, incomplete.unreachable = true, true
		case errs[i] != nil:
			log.Printf("WARN cluster disk guard: host %s could not be checked (%s); failing closed: %v",
				hosts[i], listFailureClass(errs[i]), errs[i])
			failed = true
			incomplete.unreachable = incomplete.unreachable || guardHostUnreachable(errs[i])
		default:
			out.add(scans[i])
		}
	}
	if failed {
		return out, incomplete
	}
	return out, nil
}

// clusterGuardConcurrency bounds how many cluster-wide disk scans one provider
// process runs at once (each scan itself reads at most
// effectiveListHostConcurrency hosts at a time).
const clusterGuardConcurrency = 2

// clusterGuardUnreachableMemo is how long a host found unreachable (a failed
// dial, a dropped connection, no answer within its deadline) is failed at once
// by the scans that follow, without being dialed again.
const clusterGuardUnreachableMemo = 30 * time.Second

// guardSemaphore returns the provider's scan semaphore
// (clusterGuardConcurrency slots), made on first use.
func (p *Provider) guardSemaphore() *semaphore.Weighted {
	p.guardSemOnce.Do(func() { p.guardSem = semaphore.NewWeighted(clusterGuardConcurrency) })
	return p.guardSem
}

// guardHosts returns the hosts s scans: every host the registry knows
// (routable, and — failing closed — unroutable), plus s.target when it is not
// among them (a landing host that started draining mid-call is still scanned,
// over the call's own connection), minus s.target when s.skipTarget.
func guardHosts(registry []hostconn.HostID, s clusterScan) []hostconn.HostID {
	out := make([]hostconn.HostID, 0, len(registry)+1)
	for _, h := range registry {
		if h == s.target && s.skipTarget {
			continue
		}
		out = append(out, h)
	}
	if s.target != "" && !s.skipTarget && !slices.Contains(out, s.target) {
		out = append(out, s.target)
	}
	return out
}

// guardHostUnreachable reports whether a host's scan failed because the host
// could not be reached (not leased, a dropped connection or unreachable
// libvirtd, or no answer within its deadline), rather than because it
// answered and could not be scanned.
func guardHostUnreachable(err error) bool {
	return contracts.IsHostUnavailable(err) || isHostTransportFailure(err) || stderrors.Is(err, context.DeadlineExceeded)
}

// hostLocalDirs are directories that are never shared storage: the same path
// on two hosts is two files. A candidate file inside one is compared on the
// operation's own host only, never across hosts (ADR-0007 A6.1 review). The
// libvirt NVRAM directory holds every UEFI domain's varstore by name.
var hostLocalDirs = []string{"/var/lib/libvirt/qemu/nvram"}

// inHostLocalDir reports whether p (raw or canonical) lies in a hostLocalDirs
// directory.
func inHostLocalDir(p string) bool {
	for _, d := range hostLocalDirs {
		if strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// scanHostDiskUse is one host's scan: every domain defined on the host behind
// h (domainRefsOnHostBounded; none skipped), the candidate files and the seed
// directory resolved on THIS host, and the counts of s. remote is true for a
// host other than the operation's own: a candidate in a host-local directory
// (hostLocalDirs) names another file there and is not compared.
func scanHostDiskUse(ctx context.Context, h hostCommandRunner, s clusterScan, remote bool) (hostDiskScan, error) {
	doms, err := domainRefsOnHostBounded(ctx, h, "", clusteredListMaxDomainsPerHost)
	if err != nil {
		return hostDiskScan{}, err
	}
	lookup := append([]string(nil), s.files...)
	if s.seedDir != "" {
		lookup = append(lookup, s.seedDir)
	}
	var canon []string
	if len(lookup) > 0 {
		if canon, err = canonicalizeOnHost(ctx, h, lookup); err != nil {
			return hostDiskScan{}, err
		}
	}
	compared := make([]bool, len(s.files))
	for i, f := range s.files {
		compared[i] = !remote || (!inHostLocalDir(f) && !inHostLocalDir(canon[i]))
	}
	out := hostDiskScan{used: make([]bool, len(s.files))}
	for _, d := range doms {
		uses := false
		for i := range s.files {
			if !compared[i] {
				continue
			}
			if d.refs.files[s.files[i]] || d.refs.contains(canon[i]) {
				log.Printf("WARN cluster disk guard: file %s is used by domain %s", s.files[i], d.uuid)
				out.used[i] = true
				uses = true
			}
		}
		if uses {
			out.users++
		}
		if s.seedDir != "" && ((otherDomains{d}).useUnder(s.seedDir) || (otherDomains{d}).useUnder(canon[len(s.files)])) {
			out.seedUsed = true
		}
		previous, own := incarnationKinds(d.owners, s.owner)
		if own {
			log.Printf("WARN cluster disk guard: domain %s is stamped with the requester's own UID (%s/%s)",
				d.uuid, s.owner.Namespace, s.owner.Name)
			out.ownElsewhere++
		}
		if previous {
			log.Printf("WARN cluster disk guard: domain %s is stamped for %s/%s under another UID",
				d.uuid, s.owner.Namespace, s.owner.Name)
			out.incarnations++
		}
	}
	return out, nil
}

// checkDeletionAcrossHosts is the cluster-wide half of a clustered Delete's
// plan (deleteClustered): after planDomainDeletion cleared plan on the VM's own
// host, every OTHER host of the Provider is scanned for a domain that uses one
// of the files the delete would remove. A use refuses the delete
// (diskDependentsError, onOtherHosts) and a host that cannot be checked fails
// it closed (clusterGuardIncompleteError) — either way before anything is
// changed. The seed directory is not a disk: when only it would be removed, a
// failed scan keeps it (and the delete proceeds), and a domain elsewhere that
// references a file under it keeps it too, as on the VM's own host.
func (p *Provider) checkDeletionAcrossHosts(ctx context.Context, host hostconn.HostID, plan domainDeletionPlan) (domainDeletionPlan, error) {
	files := plan.guardedFiles()
	if len(files) == 0 && plan.seedDir == "" {
		return plan, nil
	}
	s := clusterScan{files: files, seedDir: plan.seedDir, target: host, skipTarget: true}
	if len(files) > 0 {
		// The first use, or the first host that cannot be checked, refuses
		// the delete whatever the other hosts say.
		s.stopWhen = func(r hostDiskScan) bool { return r.users > 0 }
		s.stopOnError = true
	}
	res, err := p.scanClusterDiskUse(ctx, s)
	switch {
	case res.users > 0:
		log.Printf("WARN Refusing %s of libvirt domain %s on host %s: %d domain(s) on other hosts of the Provider use its disk(s) %v",
			guardOpDelete, plan.name, host, res.users, plan.disks)
		return domainDeletionPlan{}, &diskDependentsError{domain: plan.name, op: guardOpDelete, dependents: res.users, onOtherHosts: true}
	case err != nil && len(files) > 0:
		var ie *clusterGuardIncompleteError
		if stderrors.As(err, &ie) {
			return domainDeletionPlan{}, ie.with(guardOpDelete, plan.name)
		}
		return domainDeletionPlan{}, err
	case err != nil:
		log.Printf("WARN Keeping the cloud-init seed directory %s of domain %s: could not verify on every host that no domain uses it: %v",
			plan.seedDir, plan.name, err)
		plan.seedDir = ""
	case res.seedUsed:
		log.Printf("WARN Keeping the cloud-init seed directory %s of domain %s: a domain on another host references it", plan.seedDir, plan.name)
		plan.seedDir = ""
	}
	return plan, nil
}

// previousIncarnationError refuses a clustered Create or Clone because a
// domain VirtRigaud created for the requesting VirtualMachine's namespace and
// name — a previous incarnation of it — exists on a host of the Provider
// (ADR-0007 A6, R2). Nothing was created or written. Its message names only
// the requested domain (derived from the requester's own namespace and name):
// never a host, a path or another domain.
type previousIncarnationError struct {
	// op is the refused operation (guardOpCreate, guardOpClone).
	op string
	// domain is the domain that was to be created.
	domain string
	// own marks the domain found as stamped with the requester's OWN UID: its
	// own domain, elsewhere (VMPreviousIncarnationKindOwn), not a previous
	// incarnation under another UID.
	own bool
}

// Error is the refusal, safe for the requesting VM's status.
func (e *previousIncarnationError) Error() string {
	if e.own {
		return fmt.Sprintf("%s of libvirt domain %q refused: a domain of this VirtualMachine — stamped with its own UID — "+
			"already exists on another host of this Provider (its placement record was lost); nothing was created. An "+
			"administrator must point the VirtualMachine's status.placement.pendingHost at that host; no re-stamp is "+
			"needed (ADR-0007 A6 runbook)", e.op, e.domain)
	}
	return fmt.Sprintf("%s of libvirt domain %q refused: a domain VirtRigaud created for this VirtualMachine's namespace and name "+
		"(a previous incarnation of it, e.g. left by orphan-on-delete, a force-delete or a backup restore) exists on a host "+
		"of this Provider; nothing was created. An administrator must re-attach it to this VirtualMachine or remove it "+
		"(ADR-0007 A6 runbook)", e.op, e.domain)
}

// Unwrap exposes the equivalent Conflict, so contracts.IsConflict (and the
// create pipeline's non-retryable handling) see it.
func (e *previousIncarnationError) Unwrap() error {
	return contracts.NewConflictError(e.Error(), nil)
}

// GRPCStatus renders the refusal as codes.AlreadyExists carrying a
// google.rpc.ErrorInfo{Reason: VM_PREVIOUS_INCARNATION}: the manager holds the
// VM on its pending host instead of excluding the host.
// The own kind is marked with ErrorInfo metadata
// (VMPreviousIncarnationKindKey: VMPreviousIncarnationKindOwn), so the manager
// can keep a deleted VM's finalizer while its own domain runs elsewhere.
func (e *previousIncarnationError) GRPCStatus() *status.Status {
	info := &errdetails.ErrorInfo{Reason: contracts.VMPreviousIncarnationReason, Domain: contracts.ErrorInfoDomain}
	if e.own {
		info.Metadata = map[string]string{contracts.VMPreviousIncarnationKindKey: contracts.VMPreviousIncarnationKindOwn}
	}
	st := status.New(codes.AlreadyExists, e.Error())
	if withInfo, err := st.WithDetails(info); err == nil {
		return withInfo
	}
	return st
}

// clusterGuardIncompleteError reports that op of domain was not performed
// because the cluster-wide disk guard could not check every host of the
// Provider. Nothing was changed. It is retryable and never names the host.
type clusterGuardIncompleteError struct {
	// op is the operation not performed (guardOp*).
	op string
	// domain names the requesting VM's domain.
	domain string
	// unreachable reports that a host could not be reached (as opposed to a
	// host that answered but could not be scanned).
	unreachable bool
	// busy reports that the scan never started: the provider's scan slots
	// (clusterGuardConcurrency) stayed taken for the call's whole budget.
	busy bool
}

// with returns e for op of domain (a scan's answer, given to the operation
// that asked for it).
func (e *clusterGuardIncompleteError) with(op, domain string) *clusterGuardIncompleteError {
	return &clusterGuardIncompleteError{op: op, domain: domain, unreachable: e.unreachable, busy: e.busy}
}

// Error is the refusal, safe for the requesting VM's status.
func (e *clusterGuardIncompleteError) Error() string {
	switch {
	case e.unreachable:
		return fmt.Sprintf("%s of libvirt domain %q not performed: a host of this Provider could not be reached to verify "+
			"that no domain on it uses the VM's disk file (details are in the provider log); it is retried", e.op, e.domain)
	case e.busy:
		return fmt.Sprintf("%s of libvirt domain %q not performed: the provider is busy with other cross-host disk checks "+
			"and could not verify in time that no other domain uses the VM's disk file; it is retried", e.op, e.domain)
	}
	return fmt.Sprintf("%s of libvirt domain %q not performed: could not verify on every host of this Provider that no other "+
		"domain uses the VM's disk file (transient host error; details are in the provider log)", e.op, e.domain)
}

// Unwrap exposes the equivalent retryable contracts error: host-scoped when a
// host could not be reached.
func (e *clusterGuardIncompleteError) Unwrap() error {
	if e.unreachable {
		return contracts.NewHostUnavailableError(e.Error(), nil)
	}
	return contracts.NewRetryableError(e.Error(), nil)
}

// GRPCStatus renders the refusal: a host that could not be reached is
// codes.Unavailable + HOST_UNAVAILABLE, a host that could not be scanned
// codes.Unavailable + VM_DISK_CHECK_FAILED + VM_OPERATION_FAILED. The manager
// keeps both out of its per-Provider circuit breaker.
func (e *clusterGuardIncompleteError) GRPCStatus() *status.Status {
	if e.unreachable {
		return statusWithReasons(codes.Unavailable, e.Error(), contracts.HostUnavailableReason)
	}
	return diskCheckFailedStatus(e.Error(), true)
}

// lockDomain takes this provider process's lock on domain for op, held from
// the cluster-wide check until the write, define or teardown it guards
// completes (ADR-0007 A6.1 security review: the check and the act are not
// atomic). Every clustered Create, Clone and Delete of one domain name —
// "<namespace>.<name>" — is serialized, so a retry that arrives while an
// earlier attempt still runs (the manager gave up waiting, the provider did
// not) waits for it instead of checking and writing next to it. The wait is
// bounded by the call's budget (its deadline less clusterDiskGuardMargin); an
// operation that gets no lock in time is not performed (domainBusyError,
// retried). The lock is in-process only: actors outside VirtRigaud (an
// administrator's virsh, another tool, a second provider process fronting the
// same hosts) are not serialized by it.
func (p *Provider) lockDomain(ctx context.Context, domain, op string) (func(), error) {
	budget, cancel := budgetWithMargin(ctx, clusterDiskGuardMargin)
	defer cancel()
	unlock, err := p.domainLocks.lock(budget, domain)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		log.Printf("WARN %s of libvirt domain %s waited its whole budget for another operation on the same domain", op, domain)
		return nil, &domainBusyError{op: op, domain: domain}
	}
	return unlock, nil
}

// domainBusyError reports that op of domain was not performed because another
// operation on the same domain was still running in this provider process for
// the call's whole budget. Nothing was changed; it is retryable.
type domainBusyError struct {
	op, domain string
}

// Error is the refusal, safe for the requesting VM's status.
func (e *domainBusyError) Error() string {
	return fmt.Sprintf("%s of libvirt domain %q not performed: another operation on the same domain is still running "+
		"in the provider; it is retried", e.op, e.domain)
}

// Unwrap exposes the equivalent retryable contracts error.
func (e *domainBusyError) Unwrap() error { return contracts.NewRetryableError(e.Error(), nil) }

// GRPCStatus renders the refusal as codes.Unavailable + VM_OPERATION_FAILED:
// retried, and never counted by the manager's circuit breaker.
func (e *domainBusyError) GRPCStatus() *status.Status {
	return statusWithReasons(codes.Unavailable, e.Error(), contracts.VMOperationFailedReason)
}

// clusterGuardStatus returns the wire form of a cluster-wide guard's answer in
// err's chain (previousIncarnationError, clusterGuardIncompleteError), or nil
// when there is none.
func clusterGuardStatus(err error) *status.Status {
	var pi *previousIncarnationError
	if stderrors.As(err, &pi) {
		return pi.GRPCStatus()
	}
	var ie *clusterGuardIncompleteError
	if stderrors.As(err, &ie) {
		log.Printf("WARN %v", err)
		return ie.GRPCStatus()
	}
	var be *domainBusyError
	if stderrors.As(err, &be) {
		return be.GRPCStatus()
	}
	return nil
}
