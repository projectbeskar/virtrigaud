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
	"slices"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Owner transfer on a CLUSTERED provider (ADR-0007 Addendum A, slice 4).
//
// Every routed per-VM call on a clustered provider checks the domain's owner
// stamp and treats a domain the requester does not own as absent
// (withOwnedDomain). A domain adopted as it is — unstamped, or stamped by a
// VirtualMachine that has since been deleted — would therefore be unreachable
// for the VirtualMachine that adopts it; so would the domain of a
// VirtualMachine restored from a backup with a new UID (A6). TransferOwner
// closes that gap: it re-stamps the domain with the identity of the
// VirtualMachine that takes it over, on the host it is on, and nothing else.
// It never creates, moves, starts or otherwise redefines the domain.
//
// It is a compare-and-swap and fails closed:
//
//   - the domain must still be the one the manager saw: the same name and the
//     same UUID (expected_uuid), read in one document with its stamp;
//   - the domain is stamped only when every stamp it carries is the new
//     owner's (an idempotent retry, which succeeds) or one the manager
//     verified belongs to no existing VirtualMachine (replaceable_owner_uids);
//     an unstamped domain may be taken over (the single-host adoption policy
//     adopts unstamped domains too). A domain stamped for anyone else, with
//     more than one stamp, or whose stamp cannot be read, is refused with a
//     uniform Conflict and left untouched;
//   - the stamp — uid, namespace and name, all the new owner's — is written
//     with `virsh metadata` (an argv, shell-quoted on the SSH transport; every
//     value XML-escaped) to the domain addressed by its UUID: the persistent
//     definition always, the running domain too when it is active. It is then
//     read back: the domain must carry exactly the new owner's stamp (in both
//     definitions when it is active), or the call fails and the manager does
//     not bind;
//   - transfers are serialized in the provider process, so two transfers of one
//     domain through this provider cannot both pass the check before either
//     writes.
//
// Single-host adoption does not stamp and is unchanged: a single-host provider
// does not check owners on per-VM calls, and its TransferOwner answers
// Unimplemented.

// domainStateShutOff is the `virsh list --all` state of an inactive domain.
const domainStateShutOff = "shut off"

// transferRefusedMessage is the uniform refusal of a domain whose owner may not
// be transferred: it never discloses which other VirtualMachine (if any) owns
// it.
const transferRefusedMessage = "libvirt domain %q on host %s is owned by another VirtualMachine, " +
	"or its owner cannot be verified; its owner was not changed"

// TransferOwner re-stamps the domain req.VM.ID on host req.VM.HostID with
// req.VM.Owner, compare-and-swap (see the file comment). It is the backend of
// the TransferOwner RPC and is implemented in CLUSTERED topology only.
func (p *Provider) TransferOwner(ctx context.Context, req contracts.TransferOwnerRequest) error {
	if !p.clustered() {
		return contracts.NewNotSupportedError("TransferOwner is implemented by a clustered libvirt provider only")
	}
	if err := validateTransferOwnerRequest(req); err != nil {
		return err
	}
	p.transferMu.Lock()
	defer p.transferMu.Unlock()
	return p.withHostConn(ctx, req.VM.HostID, func(c libvirtConn) error {
		vp, err := virshOf(c)
		if err != nil {
			return err
		}
		return transferOwnerOn(ctx, vp, c.HostID(), req)
	})
}

// validateTransferOwnerRequest refuses, before any host is touched, a transfer
// that names no domain, no owner UID or no canonical expected UUID. A missing
// host is refused by withHostConn.
func validateTransferOwnerRequest(req contracts.TransferOwnerRequest) error {
	switch {
	case strings.TrimSpace(req.VM.ID) == "":
		return contracts.NewInvalidSpecError("TransferOwner requires the VM's id", nil)
	case req.VM.Owner.IsZero():
		return contracts.NewInvalidSpecError("TransferOwner requires the new owner's uid", nil)
	case !canonicalUUIDRE.MatchString(strings.TrimSpace(req.ExpectedUUID)):
		return contracts.NewInvalidSpecError("TransferOwner requires the VM's UUID (expected_uuid)", nil)
	}
	return nil
}

// transferOwnerOn is the TransferOwner core on host's connection (vp).
func transferOwnerOn(ctx context.Context, vp *VirshProvider, host hostconn.HostID, req contracts.TransferOwnerRequest) error {
	id, owner := req.VM.ID, req.VM.Owner
	expected := strings.TrimSpace(req.ExpectedUUID)

	domains, err := vp.listDomains(ctx)
	if err != nil {
		return contracts.NewRetryableError("failed to list domains", err)
	}
	state, found := "", false
	for _, d := range domains {
		if d.Name == id {
			state, found = d.State, true
			break
		}
	}
	if !found {
		return contracts.NewNotFoundError(fmt.Sprintf("libvirt domain %q not found on host %s; its owner was not changed", id, host), nil)
	}

	res, err := vp.runVirshCommand(ctx, "dumpxml", id)
	if err != nil {
		return contracts.NewRetryableError(fmt.Sprintf("read the definition of domain %q", id), err)
	}
	d, perr := parseDomainLibvirtxml(res.Stdout)
	if perr != nil || !canonicalUUIDRE.MatchString(strings.TrimSpace(d.UUID)) {
		log.Printf("WARN Refusing to transfer the owner of domain %s on host %s: its identity cannot be read (%v)", id, host, perr)
		return contracts.NewConflictError(fmt.Sprintf(transferRefusedMessage, id, host), nil)
	}
	uuid := strings.TrimSpace(d.UUID)
	if !strings.EqualFold(uuid, expected) {
		log.Printf("WARN Refusing to transfer the owner of domain %s on host %s: its UUID %s is not the expected %s (replaced)",
			id, host, uuid, expected)
		return contracts.NewNotFoundError(fmt.Sprintf(
			"libvirt domain %q on host %s is not the expected VM (it was replaced); its owner was not changed", id, host), nil)
	}

	recorded, oerr := domainOwners(res.Stdout)
	already, derr := ownerTransferDecision(owner, recorded, oerr, req.ReplaceableOwnerUIDs)
	if derr != nil {
		log.Printf("WARN Refusing to transfer the owner of domain %s on host %s to %s/%s (uid %s): %v (recorded %v)",
			id, host, owner.Namespace, owner.Name, owner.UID, derr, recorded)
		return contracts.NewConflictError(fmt.Sprintf(transferRefusedMessage, id, host), nil)
	}
	active := state != domainStateShutOff
	if already {
		// An idempotent retry: the stamp is there. It is still verified (in both
		// definitions of an active domain), so a stamp an interrupted earlier
		// transfer wrote to only one of them is completed below.
		if verifyOwnerStamp(ctx, vp, uuid, id, owner, active) == nil {
			return nil
		}
	}

	// Stamp the domain addressed by its UUID: the persistent definition, and
	// the running domain when it is active. virsh metadata replaces the element
	// of this namespace URI (libvirt keeps one per URI), so a replaceable stale
	// stamp is overwritten, never duplicated.
	args := []string{"metadata", uuid,
		"--uri", ownerMetadataNamespaceURI,
		"--key", ownerMetadataPrefix,
		"--set", renderOwnerSetXML(owner),
		"--config"}
	if active {
		args = append(args, "--live")
	}
	if _, err := vp.runVirshCommand(ctx, args...); err != nil {
		return fmt.Errorf("stamp domain %q with its new owner: %w", id, err)
	}
	if err := verifyOwnerStamp(ctx, vp, uuid, id, owner, active); err != nil {
		return err
	}
	log.Printf("INFO Transferred the owner of domain %s (uuid %s) on host %s to VirtualMachine %s/%s (uid %s); replaced stale owner(s) %v",
		id, uuid, host, owner.Namespace, owner.Name, owner.UID, recorded)
	return nil
}

// ownerTransferDecision decides whether a domain whose recorded stamps are
// recorded (read with error readErr) may be stamped with owner. already is
// true when it carries exactly owner's stamp. It refuses (a non-nil error,
// never a stamp) a stamp that cannot be read, more than one stamp, a stamp
// without a UID, and a stamp that is neither owner's nor in replaceable.
func ownerTransferDecision(owner contracts.ObjectIdentity, recorded []contracts.ObjectIdentity, readErr error, replaceable []string) (already bool, err error) {
	switch {
	case readErr != nil:
		return false, fmt.Errorf("its owner metadata cannot be read: %w", readErr)
	case len(recorded) == 0:
		return false, nil
	case len(recorded) > 1:
		return false, fmt.Errorf("it carries %d owner stamps", len(recorded))
	}
	uid := recorded[0].UID
	switch {
	case uid == "":
		return false, fmt.Errorf("its owner stamp has no uid")
	case uid == owner.UID:
		return true, nil
	case slices.Contains(replaceable, uid):
		return false, nil
	}
	return false, fmt.Errorf("it is stamped for a VirtualMachine the manager did not report as gone")
}

// verifyOwnerStamp reads domain uuid back and requires that it is still named
// id and carries exactly owner's stamp — in the persistent definition too when
// the domain is active.
func verifyOwnerStamp(ctx context.Context, vp *VirshProvider, uuid, id string, owner contracts.ObjectIdentity, active bool) error {
	reads := [][]string{{"dumpxml", uuid}}
	if active {
		reads = append(reads, []string{"dumpxml", "--inactive", uuid})
	}
	for _, args := range reads {
		res, err := vp.runVirshCommand(ctx, args...)
		if err != nil {
			return contracts.NewRetryableError(fmt.Sprintf("read domain %q back after stamping it", id), err)
		}
		d, perr := parseDomainLibvirtxml(res.Stdout)
		if perr != nil || strings.TrimSpace(d.Name) != id || !strings.EqualFold(strings.TrimSpace(d.UUID), uuid) {
			return contracts.NewRetryableError(fmt.Sprintf("domain %q changed while its owner was being transferred", id), perr)
		}
		owners, oerr := domainOwners(res.Stdout)
		if oerr != nil || len(owners) != 1 || owners[0] != owner {
			return contracts.NewRetryableError(fmt.Sprintf(
				"domain %q does not carry exactly its new owner's stamp after stamping", id), oerr)
		}
	}
	return nil
}

// renderOwnerSetXML is the owner element `virsh metadata --set` writes under
// ownerMetadataNamespaceURI (libvirt adds the namespace and the --key prefix).
// Every value is CR-derived and passed through xmlEscape (issue #260).
func renderOwnerSetXML(owner contracts.ObjectIdentity) string {
	return fmt.Sprintf("<%s %s='%s' %s='%s' %s='%s'/>", ownerMetadataElement,
		ownerAttrUID, xmlEscape(owner.UID),
		ownerAttrNamespace, xmlEscape(owner.Namespace),
		ownerAttrName, xmlEscape(owner.Name))
}
