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
	"fmt"
	"log"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Disk dependency guard.
//
// A linked clone is a qcow2 overlay whose backing file is its source VM's disk
// (clone.go). While the clone exists, that file must be neither removed nor
// rewritten, or the clone's data is lost:
//
//   - Delete of the source would remove the backing file;
//   - SnapshotRevert of the source to an internal snapshot rewrites the image
//     the clone reads;
//   - SnapshotDelete of an external snapshot of the source commits the
//     snapshot's overlay into its base — the file the clone reads;
//   - SnapshotCreate on the source: an internal or memory (full-system)
//     snapshot writes its snapshot table and saved RAM into the backing file
//     itself; a disk-only (external) snapshot moves the source's disk under a
//     new overlay, after which the dependency is no longer visible at the
//     source's own disk and a later snapshot delete would commit into the file
//     the clone reads. Both are refused.
//
// diskDependents finds such dependents on the host: every OTHER domain defined
// there — running or not, VirtRigaud's or anyone else's — whose disks, backing
// chains (read with `qemu-img info`, since dumpxml omits them for a shut-off
// domain) or other files reference one of the given files. The same check also
// covers two domains sharing one disk file. It runs before anything is changed,
// on the host the operation runs on: the single host, or the leased host of a
// clustered provider.
//
// A refusal is a diskDependentsError: gRPC FailedPrecondition with a
// VM_DISK_IN_USE ErrorInfo, which the manager maps to a Conflict and keeps out
// of the per-Provider circuit breaker. Its message names only the requesting
// VM's own domain and the NUMBER of dependents — never another domain (it may
// be another tenant's) or a host path; the paths are logged provider-side.
//
// Call-site checklist. Every core below takes the host it runs on (a
// hostCommandRunner / *VirshProvider: the single host, or a clustered
// provider's leased host) and must keep its protection when it is moved into a
// shared or routed core:
//
//   - Delete: deleteExistingDomain → planDomainDeletion (dependents, pool
//     confinement, own-chain and seed-directory checks) BEFORE destroy/undefine;
//     reached by deleteOn (single-host) and deleteClustered (routed).
//   - SnapshotCreate / SnapshotDelete / SnapshotRevert:
//     refuseIfDiskHasDependents(ctx, host, domain, guardOp*) right before the
//     snapshot command — Server.SnapshotCreate/Delete/Revert and the
//     contracts.Provider methods in provider_virsh.go; a routed snapshot core
//     (snapshotCreateOn/snapshotDeleteOn/snapshotRevertOn) must call it on the
//     leased host with the owner-checked handle.
//   - Clone: ensureNVRAMTargetFree before any file is written,
//     copyClonedNVRAM (O_NOFOLLOW) and finalizeClonedDisk (clonedDiskMode) —
//     all free functions over the host's *VirshProvider — and the Linked=true
//     refusal (linkedClonesDisabledMessage) before any host command; a routed
//     clone core must keep all four.

// Operations the dependency guard protects, as named in its refusal.
const (
	guardOpDelete         = "delete"
	guardOpSnapshotCreate = "snapshot create"
	guardOpSnapshotDelete = "snapshot delete"
	guardOpSnapshotRevert = "snapshot revert"
)

// diskDependentsError refuses an operation on a domain because other domains
// on its host use one of its disks as a disk or as a backing file.
type diskDependentsError struct {
	// domain is the requesting VM's own domain name.
	domain string
	// op is the refused operation (guardOp*).
	op string
	// dependents is how many other domains use the disk.
	dependents int
}

// Error is the refusal, safe for the requesting VM's status.
func (e *diskDependentsError) Error() string {
	return fmt.Sprintf("%s of libvirt domain %q refused: its disk is the backing file (or a disk) of %d other domain(s) "+
		"on this host, such as a linked clone of this VM; delete the linked clones first", e.op, e.domain, e.dependents)
}

// GRPCStatus renders the refusal as codes.FailedPrecondition carrying a
// google.rpc.ErrorInfo{Reason: VM_DISK_IN_USE}. status.FromError finds it
// through any fmt.Errorf wrapping, so the single-host handlers' historical
// "failed to ..." wrapping keeps it.
func (e *diskDependentsError) GRPCStatus() *status.Status {
	return diskInUseStatus(e.Error(), false)
}

// diskInUseStatus is the wire form of a disk-dependents refusal with message
// msg. routed adds the VM_OPERATION_FAILED ErrorInfo a clustered provider
// attaches to a per-VM failure on its host (ADR-0007 Addendum A, slice 2).
func diskInUseStatus(msg string, routed bool) *status.Status {
	st := status.New(codes.FailedPrecondition, msg)
	infos := []*errdetails.ErrorInfo{{Reason: contracts.VMDiskInUseReason, Domain: contracts.ErrorInfoDomain}}
	if routed {
		infos = append(infos, &errdetails.ErrorInfo{Reason: contracts.VMOperationFailedReason, Domain: contracts.ErrorInfoDomain})
	}
	withInfo := st
	for _, info := range infos {
		next, err := withInfo.WithDetails(info)
		if err != nil {
			// Unreachable in practice (ErrorInfo always marshals); a plain
			// FailedPrecondition is still a correct, uncounted answer.
			return st
		}
		withInfo = next
	}
	return withInfo
}

// guardCheckFailed logs why the dependency guard could not run and returns the
// generic retryable error the requester sees: the underlying error names
// other domains' disks and host commands, so it stays in the provider log.
func guardCheckFailed(op, domain string, err error) error {
	log.Printf("ERROR libvirt disk dependency check for %s of domain %s failed: %v", op, domain, err)
	return contracts.NewRetryableError(fmt.Sprintf(
		"%s of libvirt domain %q not performed: could not verify that no other domain uses its disks "+
			"(transient host error; details are in the provider log)", op, domain), nil)
}

// otherDomains are the references of every domain on a host except one.
type otherDomains []hostDomainRefs

// otherDomainsOnHost reads the references of every domain defined on the host
// behind h except selfUUID (domainRefsOnHost), failing closed.
func otherDomainsOnHost(ctx context.Context, h hostCommandRunner, selfUUID string) (otherDomains, error) {
	if selfUUID == "" {
		// Without its UUID the domain cannot be told apart from the others,
		// and would count as its own dependent.
		return nil, fmt.Errorf("the domain definition has no UUID")
	}
	return domainRefsOnHost(ctx, h, selfUUID)
}

// using counts the domains that reference any of files — raw, or canonical as
// canon[i] — as a disk, a backing file anywhere in a disk's chain, or another
// file or shared directory. self only labels the log line.
func (o otherDomains) using(self string, files, canon []string) int {
	n := 0
	for _, d := range o {
		for i := range files {
			if d.refs.files[files[i]] || d.refs.contains(canon[i]) {
				log.Printf("WARN disk %s of domain %s is used by domain %s", files[i], self, d.uuid)
				n++
				break
			}
		}
	}
	return n
}

// useUnder reports whether any of the domains references a file inside dir
// (e.g. a clone whose CD-ROM still points at its source VM's cloud-init seed).
func (o otherDomains) useUnder(dir string) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, d := range o {
		for f := range d.refs.files {
			if strings.HasPrefix(f, prefix) {
				return true
			}
		}
	}
	return false
}

// diskDependents counts the domains defined on the host behind h, other than
// selfUUID, that reference any of files (canonicalized on the host) as a disk,
// a backing file anywhere in a disk's chain, or another file or shared
// directory. It fails closed like diskSourcesInUse: any host failure is an
// error, never a count.
func diskDependents(ctx context.Context, h hostCommandRunner, selfUUID string, files []string) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	others, err := otherDomainsOnHost(ctx, h, selfUUID)
	if err != nil {
		return 0, err
	}
	canon, err := canonicalizeOnHost(ctx, h, files)
	if err != nil {
		return 0, err
	}
	return others.using(selfUUID, files, canon), nil
}

// checkDiskDependents refuses op on the domain doc describes when another
// domain on the host behind h uses one of files (the domain's own disks) — a
// diskDependentsError — and returns a generic retryable error when that cannot
// be established. No files: nothing to protect.
func checkDiskDependents(ctx context.Context, h hostCommandRunner, doc *domainDisksDoc, files []string, op string) error {
	if len(files) == 0 {
		return nil
	}
	n, err := diskDependents(ctx, h, doc.UUID, files)
	if err != nil {
		return guardCheckFailed(op, doc.Name, err)
	}
	if n > 0 {
		log.Printf("WARN Refusing %s of libvirt domain %s: %d other domain(s) use its disk(s) %v", op, doc.Name, n, files)
		return &diskDependentsError{domain: doc.Name, op: op, dependents: n}
	}
	return nil
}

// refuseIfDiskHasDependents is the dependency guard of the snapshot
// operations: it reads the definition of domain (a name or UUID) on the host
// behind h and refuses op while another domain uses one of its own disks
// (domainDisksDoc.diskFiles). The single-host handlers and — on a clustered
// provider — the cores routed to the leased host both call it before the
// snapshot command.
func refuseIfDiskHasDependents(ctx context.Context, h hostCommandRunner, domain, op string) error {
	res, err := h.runVirshCommand(ctx, "dumpxml", domain)
	if err != nil {
		return guardCheckFailed(op, domain, err)
	}
	doc, err := parseDomainDisks(res.Stdout)
	if err != nil {
		return guardCheckFailed(op, domain, err)
	}
	if doc.Name == "" {
		doc.Name = domain
	}
	return checkDiskDependents(ctx, h, doc, doc.diskFiles(), op)
}
