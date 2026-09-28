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
	"slices"
	"time"

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
//   - Create and Clone: when a file already exists where the VM's disk (or a
//     clone's UEFI varstore, or an imported disk attached in place) is about to
//     be written, EVERY host of the provider's registry is scanned — the landing
//     host too — and the file is overwritten only when no domain on any host
//     uses it. The common case, with nothing there, costs the one `test` it
//     always did and scans nothing.
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
// canonical references. Residual (documented): a host that mounts the shared
// export under a DIFFERENT path, or reaches the file through a second mount
// or a bind mount that realpath does not resolve, is not matched. (st_dev,
// st_ino) cannot close that gap: st_dev is assigned by each NFS client, so it
// differs between hosts for the same file, and st_ino alone is not unique
// across filesystems. Mount a shared pool at the same path on every host of a
// Provider (libvirt's shared-storage migration requires that too).
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
}

// newClusterDiskGuard returns the guard of a clustered op of domain for owner,
// landing on host.
func (p *Provider) newClusterDiskGuard(host hostconn.HostID, owner contracts.ObjectIdentity, domain, op string) *clusterDiskGuard {
	return &clusterDiskGuard{p: p, host: host, owner: owner, domain: domain, op: op}
}

// ensureTargetFree refuses to let the create or clone write a file (subject,
// for the answer) over target on host h. On a single-host provider (nil g) it
// is exactly ensureDiskTargetFree. On a clustered one: a symbolic link at
// target is refused as there; nothing at target is fine (no scan); and an
// existing file is overwritten only when no domain on any host of the
// Provider uses it (refuseIfUsed).
func (g *clusterDiskGuard) ensureTargetFree(ctx context.Context, h hostCommandRunner, subject, target string) error {
	if g == nil {
		return ensureDiskTargetFree(ctx, h, subject, target)
	}
	exists, err := checkWriteTarget(ctx, h, subject, target)
	if err != nil || !exists {
		return err
	}
	return g.refuseIfUsed(ctx, h, subject, target)
}

// refuseIfUsed refuses the write of an EXISTING file at target (on host h, the
// landing host) when any domain on any host of the Provider uses it, or when a
// previous incarnation of the owner exists on any host (see the file comment),
// and fails closed when a host cannot be checked. When nothing uses it, it is
// a leftover of an earlier, failed attempt for this very name, and the write
// may replace it.
func (g *clusterDiskGuard) refuseIfUsed(ctx context.Context, h hostCommandRunner, subject, target string) error {
	canon, err := canonicalizeOnHost(ctx, h, []string{target})
	if err != nil {
		return err
	}
	files := []string{target}
	if canon[0] != target {
		files = append(files, canon[0])
	}
	res, err := g.p.scanClusterDiskUse(ctx, clusterScan{files: files, owner: g.owner, target: g.host, conn: h})
	switch {
	case res.incarnations > 0:
		log.Printf("WARN Refusing %s of libvirt domain %s on host %s: %d domain(s) on the Provider's hosts are stamped for %s/%s "+
			"(a previous incarnation; ADR-0007 A6)", g.op, g.domain, g.host, res.incarnations, g.owner.Namespace, g.owner.Name)
		return &previousIncarnationError{op: g.op, domain: g.domain}
	case res.users > 0:
		log.Printf("WARN Refusing to write %s at %s on host %s: %d domain(s) on the Provider's hosts use it", subject, target, g.host, res.users)
		return contracts.NewConflictError(fmt.Sprintf(
			"%s already exists and is in use by another domain on a host of this Provider; refusing to overwrite it", subject), nil)
	case err != nil:
		return g.incomplete(err)
	}
	log.Printf("INFO %s exists at %s on host %s but no domain on any host of the Provider uses it "+
		"(left by an earlier failed attempt); overwriting it", subject, target, g.host)
	return nil
}

// incomplete turns a scan that could not check every host into the refusal
// of the guard's operation; the caller's own cancellation passes unchanged.
func (g *clusterDiskGuard) incomplete(err error) error {
	var ie *clusterGuardIncompleteError
	if stderrors.As(err, &ie) {
		return &clusterGuardIncompleteError{op: g.op, domain: g.domain, unreachable: ie.unreachable}
	}
	return err
}

// nvramSubject names a clone's UEFI varstore file for the guard's answers.
func nvramSubject(domainName string) string {
	return fmt.Sprintf("the UEFI varstore path of libvirt domain %q", domainName)
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
	// domains whose owner stamp names them (stampsNameOwner).
	owner contracts.ObjectIdentity
	// target is the host the operation runs on and conn the operation's own
	// connection to it: that host is scanned over conn, never re-leased —
	// unless skipTarget, when it is not scanned at all (a Delete, whose
	// host-local plan already checked it).
	target     hostconn.HostID
	conn       hostCommandRunner
	skipTarget bool
}

// clusterScanResult sums what the scanned hosts reported.
type clusterScanResult struct {
	// users counts the domains that reference a candidate file.
	users int
	// incarnations counts the domains stamped with the owner's namespace and
	// name.
	incarnations int
	// seedUsed reports whether a domain references a file under seedDir.
	seedUsed bool
}

// hostDiskScan is one host's part of a clusterScanResult.
type hostDiskScan = clusterScanResult

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
	hosts := guardHosts(p.clusterReg.Hosts(), s)
	if len(hosts) == 0 {
		return clusterScanResult{}, nil
	}
	budget, cancel := budgetWithMargin(ctx, clusterDiskGuardMargin)
	defer cancel()

	scans := make([]hostDiskScan, len(hosts))
	errs := p.fanOutHosts(budget, len(hosts), 0, func(i int) error {
		if hosts[i] == s.target && s.conn != nil {
			hctx, hcancel := context.WithTimeout(budget, p.effectiveListHostTimeout())
			defer hcancel()
			r, err := scanHostDiskUse(hctx, s.conn, s)
			if err == nil {
				err = hctx.Err()
			}
			scans[i] = r
			return err
		}
		return p.onHostWithin(budget, hosts[i], func(hctx context.Context, c libvirtConn) error {
			vp, err := virshOf(c)
			if err != nil {
				return err
			}
			r, err := scanHostDiskUse(hctx, vp, s)
			scans[i] = r
			return err
		})
	})
	if err := ctx.Err(); err != nil {
		return clusterScanResult{}, err
	}

	var out clusterScanResult
	incomplete := &clusterGuardIncompleteError{}
	failed := false
	for i, err := range errs {
		if err != nil {
			log.Printf("WARN cluster disk guard: host %s could not be checked (%s); failing closed: %v",
				hosts[i], listFailureClass(err), err)
			failed = true
			incomplete.unreachable = incomplete.unreachable || guardHostUnreachable(err)
			continue
		}
		out.users += scans[i].users
		out.incarnations += scans[i].incarnations
		out.seedUsed = out.seedUsed || scans[i].seedUsed
	}
	if failed {
		return out, incomplete
	}
	return out, nil
}

// guardHosts returns the hosts s scans: every routable host of the registry,
// plus s.target when it is not among them (a landing host that started
// draining mid-call is still scanned, over the call's own connection), minus
// s.target when s.skipTarget.
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

// scanHostDiskUse is one host's scan: every domain defined on the host behind
// h (domainRefsOnHostBounded; none skipped), the candidate files resolved on
// this host, and the counts of s.
func scanHostDiskUse(ctx context.Context, h hostCommandRunner, s clusterScan) (hostDiskScan, error) {
	doms, err := domainRefsOnHostBounded(ctx, h, "", clusteredListMaxDomainsPerHost)
	if err != nil {
		return hostDiskScan{}, err
	}
	var canon []string
	if len(s.files) > 0 {
		if canon, err = canonicalizeOnHost(ctx, h, s.files); err != nil {
			return hostDiskScan{}, err
		}
	}
	var out hostDiskScan
	for _, d := range doms {
		for i := range s.files {
			if d.refs.files[s.files[i]] || d.refs.contains(canon[i]) {
				log.Printf("WARN cluster disk guard: file %s is used by domain %s", s.files[i], d.uuid)
				out.users++
				break
			}
		}
		if s.seedDir != "" && (otherDomains{d}).useUnder(s.seedDir) {
			out.seedUsed = true
		}
		if stampsNameOwner(d.owners, s.owner) {
			log.Printf("WARN cluster disk guard: domain %s is stamped for %s/%s", d.uuid, s.owner.Namespace, s.owner.Name)
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
	res, err := p.scanClusterDiskUse(ctx, clusterScan{files: files, seedDir: plan.seedDir, target: host, skipTarget: true})
	switch {
	case res.users > 0:
		log.Printf("WARN Refusing %s of libvirt domain %s on host %s: %d domain(s) on other hosts of the Provider use its disk(s) %v",
			guardOpDelete, plan.name, host, res.users, plan.disks)
		return domainDeletionPlan{}, &diskDependentsError{domain: plan.name, op: guardOpDelete, dependents: res.users, onOtherHosts: true}
	case err != nil && len(files) > 0:
		var ie *clusterGuardIncompleteError
		if stderrors.As(err, &ie) {
			return domainDeletionPlan{}, &clusterGuardIncompleteError{op: guardOpDelete, domain: plan.name, unreachable: ie.unreachable}
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
}

// Error is the refusal, safe for the requesting VM's status.
func (e *previousIncarnationError) Error() string {
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
func (e *previousIncarnationError) GRPCStatus() *status.Status {
	return statusWithReasons(codes.AlreadyExists, e.Error(), contracts.VMPreviousIncarnationReason)
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
}

// Error is the refusal, safe for the requesting VM's status.
func (e *clusterGuardIncompleteError) Error() string {
	if e.unreachable {
		return fmt.Sprintf("%s of libvirt domain %q not performed: a host of this Provider could not be reached to verify "+
			"that no domain on it uses the VM's disk file (details are in the provider log); it is retried", e.op, e.domain)
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
	return nil
}
