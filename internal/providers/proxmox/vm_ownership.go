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

package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// VM ownership (owner stamp).
//
// Proxmox picks a new VMID on every Create and does not keep VM names unique,
// so without a record of who made a VM a Create re-sent after an unknown
// outcome made a second VM, and a VM could not be found again by the
// VirtualMachine it was made for. The provider now stamps every VM it creates
// or clones for a known VirtualMachine with that VirtualMachine's identity, as
// the vSphere (ExtraConfig) and libvirt (domain <metadata>) providers do.
//
// The stamp is ONE line of the VM's `description` (the "Notes" field):
//
//	virtrigaud.owner v1 uid=<uid> namespace=<ns> name=<name> phase=<creating|ready> sum=<16 hex>
//
// Why the description and not a tag (see docs/vm-ownership.md#proxmox):
//   - it is accepted by both POST /nodes/{node}/qemu (create) and POST
//     .../qemu/{vmid}/clone, so the stamp is written by the same call that
//     makes the VM; the clone endpoint takes no `tags`;
//   - it carries the UID, namespace and name verbatim, without the PVE tag
//     charset;
//   - setting it is not subject to the datacenter `user-tag-access` /
//     `registered-tags` policy, which can forbid an API token to create a tag;
//   - neither field is visible to the guest.
//
// The line is strict (fixed fields, single spaces, a checksum over the rest of
// the line), so an edit that changes any field makes the stamp UNREADABLE,
// never a different owner. Any line that mentions the marker
// (case-insensitively) and is not a canonical stamp is unreadable too. The
// checksum detects accidental edits only: anyone with VM.Config.Options on the
// VM can rewrite the whole line, exactly as they could edit vSphere ExtraConfig
// or a libvirt domain's metadata. PVE ACLs are the trust boundary.
//
// phase=creating is written by the call that makes the VM; phase=ready once
// every step of the Create or Clone has succeeded. A re-sent Create that finds
// its own VM still "creating" finishes it instead of making another.

const (
	// ownerStampMarker starts the owner stamp line; it is reserved in VM
	// descriptions: every line containing it is replaced by the provider.
	ownerStampMarker = "virtrigaud.owner"
	// ownerStampVersion is the stamp format version.
	ownerStampVersion = "v1"
	// ownerStampPhaseCreating marks a VM whose Create or Clone has not
	// completed every step yet.
	ownerStampPhaseCreating = "creating"
	// ownerStampPhaseReady marks a VM whose Create or Clone completed.
	ownerStampPhaseReady = "ready"
	// ownerStampSumHexLen is the number of hex digits of the stamp checksum.
	ownerStampSumHexLen = 16
	// ownerStampFieldCount is the number of space-separated fields of a stamp
	// line: marker, version, uid, namespace, name, phase, sum.
	ownerStampFieldCount = 7
	// ownerIdentityFieldMaxLen bounds each stamped identity field (a
	// Kubernetes name is at most 253 characters).
	ownerIdentityFieldMaxLen = 253

	// pveDescriptionMaxLen is PVE's limit on a VM description.
	pveDescriptionMaxLen = 8192

	// pveConfigKeyDescription, pveConfigKeyLock, pveConfigKeyTemplate,
	// pveConfigKeyName and pveConfigKeyDelete are VM config keys (and the
	// PUT /config "delete" parameter) the ownership code reads or writes.
	pveConfigKeyDescription = "description"
	pveConfigKeyLock        = "lock"
	pveConfigKeyTemplate    = "template"
	pveConfigKeyName        = "name"
	pveConfigKeyDelete      = "delete"

	// pveStatusStopped is the status of a VM that is not running.
	pveStatusStopped = "stopped"
	// pveStatusUnknown is the /cluster/resources status of a guest on a node
	// that is down.
	pveStatusUnknown = "unknown"
)

// ownerIdentityFieldRE is the charset of a stamped identity field: what a
// Kubernetes UID (a UUID), namespace (DNS label) or name (DNS subdomain) can
// hold, and nothing that could break the stamp line (no space, '=', newline).
var ownerIdentityFieldRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ownerStampFieldKeys are the keys of the stamp line's key=value fields, in
// order (fields 2 to 6).
var ownerStampFieldKeys = []string{"uid", "namespace", "name", "phase", "sum"}

// ownerStamp is one owner stamp: the VirtualMachine that owns the VM, and how
// far the Create or Clone that made the VM got.
type ownerStamp struct {
	owner contracts.ObjectIdentity
	phase string
}

// withPhase returns a copy of s with phase.
func (s ownerStamp) withPhase(phase string) ownerStamp {
	s.phase = phase
	return s
}

// body is the stamp line without its checksum field.
func (s ownerStamp) body() string {
	return fmt.Sprintf("%s %s uid=%s namespace=%s name=%s phase=%s",
		ownerStampMarker, ownerStampVersion, s.owner.UID, s.owner.Namespace, s.owner.Name, s.phase)
}

// line is the canonical stamp line.
func (s ownerStamp) line() string {
	b := s.body()
	return b + " sum=" + ownerStampSum(b)
}

// ownerStampSum is the checksum of a stamp line body.
func ownerStampSum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])[:ownerStampSumHexLen]
}

// validateOwnerIdentity checks that every field of o can be stamped: present,
// within the length bound and in ownerIdentityFieldRE's charset.
func validateOwnerIdentity(o contracts.ObjectIdentity) error {
	for _, f := range []struct{ key, value string }{
		{"uid", o.UID}, {"namespace", o.Namespace}, {"name", o.Name},
	} {
		if len(f.value) > ownerIdentityFieldMaxLen || !ownerIdentityFieldRE.MatchString(f.value) {
			return fmt.Errorf("owner %s %q is empty, longer than %d characters, or has characters other than "+
				"letters, digits, '.', '_' and '-'", f.key, f.value, ownerIdentityFieldMaxLen)
		}
	}
	return nil
}

// isOwnerStampLine reports whether line mentions the stamp marker, in any
// case: such a line is either a canonical stamp or an unreadable one.
func isOwnerStampLine(line string) bool {
	return strings.Contains(strings.ToLower(line), ownerStampMarker)
}

// parseOwnerStampLine parses one stamp line strictly: exactly the canonical
// fields, in order, separated by single spaces, with valid identity values, a
// known phase and a matching checksum.
func parseOwnerStampLine(line string) (ownerStamp, error) {
	fields := strings.Split(strings.TrimSpace(line), " ")
	if len(fields) != ownerStampFieldCount {
		return ownerStamp{}, fmt.Errorf("owner stamp has %d fields, want %d", len(fields), ownerStampFieldCount)
	}
	if fields[0] != ownerStampMarker || fields[1] != ownerStampVersion {
		return ownerStamp{}, fmt.Errorf("owner stamp does not start with %q", ownerStampMarker+" "+ownerStampVersion)
	}
	values := make([]string, len(ownerStampFieldKeys))
	for i, key := range ownerStampFieldKeys {
		v, ok := strings.CutPrefix(fields[i+2], key+"=")
		if !ok {
			return ownerStamp{}, fmt.Errorf("owner stamp field %d is not %s=", i+3, key)
		}
		values[i] = v
	}
	s := ownerStamp{
		owner: contracts.ObjectIdentity{UID: values[0], Namespace: values[1], Name: values[2]},
		phase: values[3],
	}
	if err := validateOwnerIdentity(s.owner); err != nil {
		return ownerStamp{}, err
	}
	if s.phase != ownerStampPhaseCreating && s.phase != ownerStampPhaseReady {
		return ownerStamp{}, fmt.Errorf("owner stamp phase %q is unknown", s.phase)
	}
	if values[4] != ownerStampSum(strings.Join(fields[:ownerStampFieldCount-1], " ")) {
		return ownerStamp{}, fmt.Errorf("owner stamp checksum does not match")
	}
	return s, nil
}

// ownerStampRead is what a VM description says about the VM's owner.
type ownerStampRead struct {
	// stamp is the VM's one readable stamp; nil when the VM carries none, or
	// when state is set.
	stamp *ownerStamp
	// uids are the distinct UIDs of the readable stamps, in order.
	uids []string
	// state is "" (no stamp, or exactly one readable stamp),
	// contracts.OwnerStampUnreadable or contracts.OwnerStampMultiple.
	state string
	// problem says why state is set, for the provider log.
	problem string
}

// readOwnerStamp reads the owner stamp from a VM description. A stamp line
// that does not parse, or the same owner stamped twice (VirtRigaud writes one
// line), is unreadable; two readable stamps of different owners are multiple.
// Unreadable wins over multiple.
func readOwnerStamp(description string) ownerStampRead {
	var (
		r         ownerStampRead
		stamps    []ownerStamp
		malformed error
	)
	for _, line := range strings.Split(description, "\n") {
		if !isOwnerStampLine(line) {
			continue
		}
		s, err := parseOwnerStampLine(line)
		if err != nil {
			if malformed == nil {
				malformed = err
			}
			continue
		}
		stamps = append(stamps, s)
		if !slices.Contains(r.uids, s.owner.UID) {
			r.uids = append(r.uids, s.owner.UID)
		}
	}
	switch {
	case malformed != nil:
		r.state, r.problem = contracts.OwnerStampUnreadable, malformed.Error()
	case len(r.uids) > 1:
		r.state, r.problem = contracts.OwnerStampMultiple, "more than one owner is stamped"
	case len(stamps) > 1:
		r.state, r.problem = contracts.OwnerStampUnreadable, "the owner stamp is repeated"
	case len(stamps) == 1:
		r.stamp = &stamps[0]
	}
	return r
}

// matches reports whether r is exactly want: one readable stamp equal to
// *want, or no stamp at all when want is nil.
func (r ownerStampRead) matches(want *ownerStamp) bool {
	if r.state != "" {
		return false
	}
	if want == nil {
		return r.stamp == nil
	}
	return r.stamp != nil && *r.stamp == *want
}

// stripOwnerStamp removes every line that mentions the stamp marker from a
// description, and trailing blank space.
func stripOwnerStamp(description string) string {
	var kept []string
	for _, line := range strings.Split(description, "\n") {
		if !isOwnerStampLine(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), " \t\r\n")
}

// descriptionTruncatedNote ends notes cut short to leave room for the stamp.
const descriptionTruncatedNote = "\n[notes truncated by VirtRigaud to fit the owner stamp]"

// composeDescription is base without any stamp line, followed by the line of
// stamp (when not nil). When the result would exceed PVE's description limit,
// the notes are cut (at a character boundary) and marked with
// descriptionTruncatedNote: the stamp always fits (descriptionTruncated says
// when, so the caller can warn).
func composeDescription(base string, stamp *ownerStamp) string {
	out := stripOwnerStamp(base)
	if stamp == nil {
		return out
	}
	line := stamp.line()
	if out == "" {
		return line
	}
	if len(out)+1+len(line) <= pveDescriptionMaxLen {
		return out + "\n" + line
	}
	budget := pveDescriptionMaxLen - len(line) - 1 - len(descriptionTruncatedNote)
	if budget <= 0 {
		return line
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + descriptionTruncatedNote + "\n" + line
}

// descriptionTruncated reports whether composeDescription cuts base's notes.
func descriptionTruncated(base string, stamp *ownerStamp) bool {
	out := stripOwnerStamp(base)
	return stamp != nil && out != "" && len(out)+1+len(stamp.line()) > pveDescriptionMaxLen
}

// warnIfTruncated logs that the notes of VM vmid are cut to fit its stamp.
func (p *Provider) warnIfTruncated(vmid int, base string, stamp *ownerStamp) {
	if descriptionTruncated(base, stamp) {
		p.logger.Warn("The VM's notes exceed PVE's description limit with the owner stamp; truncating them",
			"vmid", vmid, "limit_bytes", pveDescriptionMaxLen)
	}
}

// configString returns the string value of a PVE VM config key ("" if absent).
func configString(cfg map[string]interface{}, key string) string {
	v, _ := cfg[key].(string)
	return v
}

// configIsTemplate reports whether a PVE VM config is a template's.
func configIsTemplate(cfg map[string]interface{}) bool {
	switch v := cfg[pveConfigKeyTemplate].(type) {
	case float64:
		return v == 1
	case string:
		return v == "1"
	case bool:
		return v
	}
	return false
}

// ownerFromCreateRequest reads CreateRequest.owner. An absent owner, or one
// without a UID (a manager older than this provider), is the zero identity:
// the VM is created unstamped and nothing is re-bound. An owner with a UID
// must be stampable, or the Create is refused (InvalidSpec) before anything
// is done.
func ownerFromCreateRequest(req *providerv1.CreateRequest) (contracts.ObjectIdentity, error) {
	return stampableOwner(req.GetOwner(), "owner")
}

// cloneTargetOwner reads CloneRequest.target_vm when it carries a UID: the
// VirtualMachine the clone will be bound to, which the clone is stamped with.
// Without a UID (the VirtualMachine does not exist yet) the clone is left
// unstamped.
func cloneTargetOwner(req *providerv1.CloneRequest) (contracts.ObjectIdentity, error) {
	return stampableOwner(req.GetTargetVm(), "target_vm")
}

// stampableOwner converts a wire identity with a UID into a validated owner,
// and anything without a UID into the zero identity.
func stampableOwner(o *providerv1.ObjectIdentity, field string) (contracts.ObjectIdentity, error) {
	if o.GetUid() == "" {
		return contracts.ObjectIdentity{}, nil
	}
	owner := contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
	if err := validateOwnerIdentity(owner); err != nil {
		return contracts.ObjectIdentity{}, errors.NewInvalidSpec("%s cannot be stamped on a Proxmox VM: %v", field, err)
	}
	return owner, nil
}

// ownedVM is a VM of the cluster whose readable owner stamp records the
// requester's UID.
type ownedVM struct {
	node        string
	vmid        int
	status      string
	lock        string
	description string
	stamp       ownerStamp
}

// ownershipConflictError is the codes.AlreadyExists answer of a Create or
// Clone that cannot tell which VM this VirtualMachine owns. The message names
// only the requested VM name: it lands in the VirtualMachine's status, so it
// never says which other VM or owner is involved (the provider log does).
func ownershipConflictError(name string) error {
	return &errors.ProviderError{
		Code: codes.AlreadyExists,
		Message: fmt.Sprintf("cannot tell which Proxmox VM named %q this VirtualMachine owns: a VM of that name "+
			"carries an owner stamp that cannot be read or records more than one owner, a template carries this "+
			"VirtualMachine's stamp, more than one VM does, or the stamp does not match this VirtualMachine's "+
			"namespace and name. Refusing to create or bind; details are in the provider log", name),
	}
}

// findOwnedVM looks across the whole cluster for the VM named name that is
// stamped with owner's UID: a VM an earlier Create (or Clone) for this
// VirtualMachine made. It returns nil when there is none. VMIDs are
// cluster-wide and a VM may have been migrated, so every node is searched.
//
// Only VMs with the requested name are read (one config read each): a VM
// renamed since its create is not found. It fails closed:
//   - the cluster list, or the config of any same-named VM, cannot be read:
//     a retryable error, nothing created;
//   - a same-named VM (not a template) has an unreadable or multiple stamp,
//     a template carries owner's UID, more than one VM does, or the one that
//     does records another namespace or name: ownershipConflictError.
func (p *Provider) findOwnedVM(ctx context.Context, name string, owner contracts.ObjectIdentity) (*ownedVM, error) {
	vms, err := p.client.ListClusterVMs(ctx)
	if err != nil {
		p.logger.Error("Listing the cluster's VMs to look for a VM this VirtualMachine owns failed", "name", name, "error", err)
		return nil, p.createNotPerformed("cannot list the Proxmox cluster's VMs to look for a VM this VirtualMachine "+
			"already owns; nothing was created (will retry)", err)
	}
	var (
		matches   []*ownedVM
		ambiguous bool
		// beingMade: an unnamed guest PVE is still cloning or creating that
		// may be this provider's (unattributedInflight).
		beingMade bool
	)
	for _, vm := range vms {
		if vm.Type != pveapi.ClusterResourceTypeQEMU {
			continue
		}
		if nameless(vm) {
			// A guest whose config has no name: one PVE is still cloning or
			// creating has none yet. Its config decides (unattributedInflight).
			// A guest on a node that is down (status "unknown") cannot be in
			// progress: no task runs there, and its config cannot be read, so
			// it would otherwise hold every Create.
			if vm.Status != pveStatusUnknown && p.unattributedInflight(ctx, vm.Node, vm.VMID) {
				p.logger.Warn("A VM is still being cloned or created by this provider's PVE user and carries no name or "+
					"owner stamp yet; it may be this VirtualMachine's", "name", name, "vmid", vm.VMID, "node", vm.Node)
				beingMade = true
			}
			continue
		}
		if vm.Name != name {
			continue
		}
		cfg, err := p.client.GetVMConfig(ctx, vm.Node, vm.VMID)
		if stderrors.Is(err, pveapi.ErrVMNotFound) {
			if gone, goneErr := p.vmGone(ctx, vm.VMID); goneErr == nil && gone {
				continue // removed since the list
			}
			err = fmt.Errorf("VM %d is not on node %s but may exist elsewhere in the cluster", vm.VMID, vm.Node)
		}
		if err != nil {
			p.logger.Error("Reading the owner stamp of a same-named VM failed", "name", name, "vmid", vm.VMID, "node", vm.Node, "error", err)
			return nil, p.vmCreateNotPerformed(fmt.Sprintf("cannot read the owner stamp of a Proxmox VM named %q; "+
				"nothing was created (will retry)", name), err)
		}
		read := readOwnerStamp(configString(cfg, pveConfigKeyDescription))
		template := configIsTemplate(cfg) || vm.Template == 1
		logArgs := []any{"name", name, "vmid", vm.VMID, "node", vm.Node, "requester_uid", owner.UID}
		switch {
		case read.state != "":
			if template {
				continue // a template is never bound
			}
			p.logger.Warn("A same-named VM carries an owner stamp that cannot be relied on",
				append(logArgs, "owner_stamp_state", read.state, "problem", read.problem, "owner_uids", read.uids)...)
			ambiguous = true
		case read.stamp == nil || read.stamp.owner.UID != owner.UID:
			continue
		case template:
			p.logger.Warn("A template carries this VirtualMachine's owner stamp; refusing to bind it", logArgs...)
			ambiguous = true
		default:
			matches = append(matches, &ownedVM{
				node: vm.Node, vmid: vm.VMID, status: vm.Status,
				lock: configString(cfg, pveConfigKeyLock), description: configString(cfg, pveConfigKeyDescription),
				stamp: *read.stamp,
			})
		}
	}
	switch {
	case ambiguous:
		return nil, ownershipConflictError(name)
	case len(matches) == 0 && beingMade:
		return nil, p.outcomeUnknown(fmt.Sprintf("Proxmox is still cloning or creating a VM that may be this "+
			"VirtualMachine's (%q); not starting another, will retry", name), nil)
	case len(matches) == 0:
		return nil, nil
	case len(matches) > 1:
		ids := make([]int, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.vmid)
		}
		p.logger.Warn("More than one VM carries this VirtualMachine's owner stamp; not choosing one",
			"name", name, "requester_uid", owner.UID, "vmids", ids)
		return nil, ownershipConflictError(name)
	}
	m := matches[0]
	if m.stamp.owner != owner {
		// Same UID, other namespace/name: the stamp was edited.
		p.logger.Warn("A VM's owner stamp records this VirtualMachine's UID with another namespace or name; refusing to bind it",
			"name", name, "vmid", m.vmid, "stamp_namespace", m.stamp.owner.Namespace, "stamp_name", m.stamp.owner.Name,
			"requester_namespace", owner.Namespace, "requester_name", owner.Name)
		return nil, ownershipConflictError(name)
	}
	return m, nil
}

// nameless reports whether /cluster/resources lists vm as a guest whose config
// has no name: an empty name, or the "VM <vmid>" PVE's status daemon reports
// for one. A Kubernetes name (lower-case, no space) is never of that form, so
// such a guest is never a requested name.
func nameless(vm pveapi.ClusterVM) bool {
	return vm.Name == "" || vm.Name == fmt.Sprintf("VM %d", vm.VMID)
}

// vmGone reports whether no guest of the cluster has vmid: it is neither in
// /cluster/resources nor taken according to /cluster/nextid?vmid= (which,
// unlike the resource list, does not depend on the token's permissions). A
// "does not exist" answer from one node never proves that alone: the VM may
// have moved to another node.
func (p *Provider) vmGone(ctx context.Context, vmid int) (bool, error) {
	vms, err := p.client.ListClusterVMs(ctx)
	if err != nil {
		return false, err
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return false, nil
		}
	}
	return p.client.VMIDFree(ctx, vmid)
}

// vmIDOn is the VM id Create or Clone returns for vmid on node: the bare VMID
// when node is the provider's default node (where a bare id is looked up), and
// "<node>:<vmid>" otherwise.
func (p *Provider) vmIDOn(ctx context.Context, node string, vmid int) string {
	if def, err := p.client.FindNode(ctx); err == nil && def == node {
		return strconv.Itoa(vmid)
	}
	return node + ":" + strconv.Itoa(vmid)
}

// inheritedDescription reads the description a clone of vmid would inherit,
// without any owner stamp line.
//
// It also reports whether the source has a description at all: PVE copies it
// to the clone whenever the clone request's own description is empty, so
// cloneRequestDescription then sends an explicit one. cfg is the source's config.
func (p *Provider) inheritedDescription(ctx context.Context, node string, vmid int) (base string, sourceHas bool, cfg map[string]interface{}, err error) {
	cfg, err = p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return "", false, nil, err
	}
	desc := configString(cfg, pveConfigKeyDescription)
	if isOwnerStampLine(desc) {
		p.logger.Info("The clone source carries an owner stamp; the clone does not inherit it", "source_vmid", vmid)
	}
	return stripOwnerStamp(desc), strings.TrimSpace(desc) != "", cfg, nil
}

// unstampedCloneNote is the description sent for a clone that has neither
// notes to keep nor an owner stamp, when its source has a description: PVE
// ignores an empty description in a clone request and copies the source's,
// owner stamp included.
const unstampedCloneNote = "Cloned by VirtRigaud."

// cloneRequestDescription is the description a clone request carries: base
// with stamp, or unstampedCloneNote when that is empty but the source has a
// description (which PVE would otherwise copy). "" sends none.
func cloneRequestDescription(base string, sourceHas bool, stamp *ownerStamp) string {
	desc := composeDescription(base, stamp)
	if desc == "" && sourceHas {
		return unstampedCloneNote
	}
	return desc
}

// writeOwnerStamp sets the VM's description to base with stamp (no stamp when
// nil) and reads it back: it succeeds only when the VM then carries exactly
// that stamp.
func (p *Provider) writeOwnerStamp(ctx context.Context, node string, vmid int, base string, stamp *ownerStamp) error {
	p.warnIfTruncated(vmid, base, stamp)
	desc := composeDescription(base, stamp)
	vals := url.Values{}
	if desc == "" {
		vals.Set(pveConfigKeyDelete, pveConfigKeyDescription)
	} else {
		vals.Set(pveConfigKeyDescription, desc)
	}
	task, err := p.client.ReconfigureVMRaw(ctx, node, vmid, vals)
	if err != nil {
		return fmt.Errorf("write owner stamp of VM %d: %w", vmid, err)
	}
	if err := p.client.WaitForTask(ctx, node, task); err != nil {
		return fmt.Errorf("write owner stamp of VM %d: %w", vmid, err)
	}
	return p.verifyOwnerStamp(ctx, node, vmid, stamp)
}

// verifyOwnerStamp reads the VM's description and checks that it carries
// exactly stamp (no stamp when nil).
func (p *Provider) verifyOwnerStamp(ctx context.Context, node string, vmid int, stamp *ownerStamp) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read back owner stamp of VM %d: %w", vmid, err)
	}
	if read := readOwnerStamp(configString(cfg, pveConfigKeyDescription)); !read.matches(stamp) {
		return fmt.Errorf("VM %d does not carry the expected owner stamp after it was written (state %q)", vmid, read.state)
	}
	return nil
}

// ensureOwnerStamp makes sure a VM just cloned carries exactly stamp (no stamp
// when nil): the stamp was sent with the clone request, but PVE copies the
// source's description when the request's is empty, so it is checked and,
// if needed, written.
func (p *Provider) ensureOwnerStamp(ctx context.Context, node string, vmid int, base string, stamp *ownerStamp) error {
	if p.verifyOwnerStamp(ctx, node, vmid, stamp) == nil {
		return nil
	}
	return p.writeOwnerStamp(ctx, node, vmid, base, stamp)
}

// The constant messages of Delete's refusals (checkDelete). They name no VM,
// stamp or owner: the details go to the provider log only.
const (
	// deleteNotOwnedMessage answers a Delete whose VMID holds a VM that is not
	// the requester's (VM_NOT_OWNED).
	deleteNotOwnedMessage = "the VM at this VMID is not this VirtualMachine's (its VMID was reused, or it was " +
		"re-created under another UID); it was not touched"
	// deleteTemplateMessage answers a Delete whose VMID holds a template
	// (VM_IS_IMAGE_ARTIFACT).
	deleteTemplateMessage = "refused: the Proxmox VMID names a template, not a VM; deleting it would destroy the " +
		"image for every VM cloned from it, so nothing was changed"
	// deleteStampUnreadableMessage answers a Delete whose VM carries an owner
	// stamp that cannot be read or records more than one owner
	// (OWNER_STAMP_UNREADABLE).
	deleteStampUnreadableMessage = "refused: the VM carries an owner stamp (" + ownerStampMarker + " line of its " +
		"description) that cannot be read or records more than one owner; nothing was changed. Fix the stamp, or " +
		"remove the VM by hand"
)

// checkDelete decides whether Delete may destroy the VM at vmid on node. It
// returns nil to go on with the delete, and otherwise the answer; nothing is
// stopped or destroyed then:
//   - the config cannot be read: Unavailable (retried);
//   - the VM is a template: FailedPrecondition + VM_IS_IMAGE_ARTIFACT, with or
//     without an owner on the request — a template (for example an adopted
//     VM converted to one) is never destroyed by a VirtualMachine's delete;
//   - with an owner (DeleteRequest.owner) only: a VM stamped with another UID,
//     or unstamped and not named like the owner (unstampedNamedLikeOwner), is
//     NotFound + VM_NOT_OWNED — the requester's VM is gone, and the VM at the
//     VMID is not its own; a stamp that cannot be read or records more than
//     one owner is FailedPrecondition + OWNER_STAMP_UNREADABLE.
//
// A VM the config read cannot find on this node is left to the delete itself,
// which confirms cluster-wide whether it is gone.
func (p *Provider) checkDelete(ctx context.Context, node string, vmid int, owner contracts.ObjectIdentity) error {
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return nil // not on this node: the delete confirms cluster-wide that it is gone
	}
	if err != nil {
		p.logger.Error("Reading the VM configuration before a delete failed; not deleting", "vmid", vmid, "node", node, "error", err)
		return errors.NewUnavailable(fmt.Sprintf("cannot read the configuration of Proxmox VM %d; not deleting it (will retry)", vmid), nil)
	}
	logArgs := []any{"vmid", vmid, "node", node, "requester_uid", owner.UID, "requester_namespace", owner.Namespace, "requester_name", owner.Name}
	if configIsTemplate(cfg) {
		p.logger.Warn("Refusing to delete: the VMID names a template, never a VirtualMachine's VM", logArgs...)
		return statusWithReasons(codes.FailedPrecondition, deleteTemplateMessage, contracts.VMIsImageArtifactReason)
	}
	if owner.IsZero() {
		return nil
	}
	read := readOwnerStamp(configString(cfg, pveConfigKeyDescription))
	switch {
	case read.state != "":
		p.logger.Warn("Refusing to delete: the VM's owner stamp cannot be relied on",
			append(logArgs, "owner_stamp_state", read.state, "problem", read.problem)...)
		return statusWithReasons(codes.FailedPrecondition, deleteStampUnreadableMessage, contracts.OwnerStampUnreadableReason)
	case read.stamp == nil && !unstampedNamedLikeOwner(configString(cfg, pveConfigKeyName), vmid, owner):
		// An unstamped VM is this VirtualMachine's only if it has the name
		// Create (or adoption) gave it: another name means the VMID was
		// reused by an unrelated VM.
		p.logger.Warn("Refusing to delete: the VM is unstamped and not named like this VirtualMachine (its VMID was reused?)",
			append(logArgs, "vm_name", configString(cfg, pveConfigKeyName))...)
		return statusWithReasons(codes.NotFound, deleteNotOwnedMessage, contracts.VMNotOwnedReason)
	case read.stamp == nil:
		p.logger.Info("The VM carries no owner stamp (created before owner stamps, adopted, or cloned without a target UID) "+
			"and is named like this VirtualMachine; deleting it as before", logArgs...)
		return nil
	case read.stamp.owner.UID != owner.UID:
		p.logger.Warn("Refusing to delete: the VM is stamped with another owner (its VMID was reused, or the requester was re-created)",
			append(logArgs, "owner_uid", read.stamp.owner.UID, "owner_namespace", read.stamp.owner.Namespace, "owner_name", read.stamp.owner.Name)...)
		return statusWithReasons(codes.NotFound, deleteNotOwnedMessage, contracts.VMNotOwnedReason)
	}
	return nil
}

// unstampedNamedLikeOwner reports whether an unstamped VM whose PVE name is
// pveName (VMID vmid) is named like owner's VirtualMachine: pveName is the
// owner's name — the name Create gives the VM — or the name the adoption
// controller gives the VirtualMachine adopting it (contracts.AdoptedVMName).
// Anything else is another VM at a reused VMID.
func unstampedNamedLikeOwner(pveName string, vmid int, owner contracts.ObjectIdentity) bool {
	if owner.Name == "" {
		return false
	}
	return pveName == owner.Name || contracts.AdoptedVMName(pveName, strconv.Itoa(vmid)) == owner.Name
}

// deleteOwner reads DeleteRequest.owner; the zero identity when absent.
func deleteOwner(req *providerv1.DeleteRequest) contracts.ObjectIdentity {
	o := req.GetOwner()
	return contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
}

// ownerRaw adds the owner stamp of a listed VM to its ProviderRaw:
// contracts.VMInfoOwnerUIDKey (the readable stamped UIDs, comma-separated) and
// contracts.VMInfoOwnerStampStateKey when the stamp cannot be relied on.
func ownerRaw(raw map[string]string, read ownerStampRead) {
	if len(read.uids) > 0 {
		raw[contracts.VMInfoOwnerUIDKey] = strings.Join(read.uids, ",")
	}
	if read.state != "" {
		raw[contracts.VMInfoOwnerStampStateKey] = read.state
	}
}
