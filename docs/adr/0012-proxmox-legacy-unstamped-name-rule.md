# ADR-0012: Retiring the Proxmox legacy unstamped-name rule

## Status

**Proposed (2026-10-17), awaiting maintainer sign-off.** Design only; nothing in
this record is implemented yet.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Tracking**: v0.4.0-rc2 item R14 (security review finding M2 of PR #393).

**Related**:
- PR #380: the Proxmox owner stamp (one `virtrigaud.owner v1 …` line of the VM
  description), first shipped in v0.4.0-rc1.
- PR #393: owner check on every single-host Proxmox per-VM call
  (`internal/providers/proxmox/vm_owner_check.go`).
- PR #392: single-host adoption stamps the adopted VM (`TransferOwner`,
  compare-and-swap on the PVE config `digest`).
- PR #395 (rc2 R4): single-host `VMClone` targets are stamped.
- [`docs/vm-ownership.md`](../vm-ownership.md#limits): the Limits entry this ADR
  retires.

## Context

### The rule

Since #380 every VM the Proxmox provider creates or clones for a
`VirtualMachine` carries an owner stamp, and since #393 every per-VM call
(`Describe`, `Power`, `Reconfigure`, `SnapshotCreate/Delete/Revert`,
`GetDiskInfo`, `ExportDisk`, a `Clone` source) and `Delete` checks it against
the request's `owner` before it reads or changes anything (`checkOwnerInConfig`,
`checkDelete`). One case is accepted without a stamp:

> an **unstamped** VM whose PVE name is the owner's name, or the name adoption
> gives it (`contracts.AdoptedVMName`: sanitized PVE name, or `vm-<vmid>`), is
> treated as the owner's (`unstampedNamedLikeOwner`).

This is the **legacy name rule**. It exists for upgrade compatibility. These
VMs are legitimately unstamped today:

| Source of an unstamped, still-managed VM | Fixed by |
|---|---|
| Created by v0.3.11 or earlier (no stamping) | — |
| Adopted by a release before #392, or through a provider without `supportsOwnerTransfer` | — |
| `VMClone` target made by a manager older than rc2 R4 (no `target_vm.uid`) | — |
| `Create` sent by a manager that sends no `owner` | — |

Without the rule, every such `VirtualMachine` would be held
`Ready=False/VMNotOwned` on upgrade, and its `Delete` would leave its VM on the
cluster.

### The threat (M2)

The provider is stateless. When it reads an unstamped VM named like the owner it
cannot tell a legacy VM from a look-alike planted at the same VMID, because it
does not know whether the owner's VM was ever stamped.

- **Who.** A PVE principal (user or API token) other than the Provider's own
  token, holding `VM.Allocate` on a path where VMID *N* can be created (a
  shared pool, `/vms`, `/vms/N`), plus whatever PVE checks to set `name` on a
  create. This is **not** a Kubernetes tenant. Through VirtRigaud, a tenant can
  only create VMs that the provider stamps with the tenant's own UID, and a VM
  stamped with another UID is refused. The only path a tenant has to an
  unstamped VM is a mixed-version one: a `VMClone` made by a manager older than
  rc2.
- **Preconditions.**
  1. `VirtualMachine` *V* is bound (`status.id = N`) through a single-host
     Proxmox Provider. The manager and the provider are at #393 or later.
  2. VMID *N* is freed while *V* still exists. Someone destroys *V*'s VM
     outside VirtRigaud: an administrator, an accident, or the attacker
     (`VM.Allocate` includes destroy).
  3. The attacker creates an unstamped VM at *N* named *V*'s name (PVE accepts
     an explicit `vmid` on create, so no `/cluster/nextid` race is needed). It
     must exist before the manager's next `Describe` sees *N* free and creates
     a new VM. The attacker controls both steps, so this window is easy to win.
  4. The attacker knows *V*'s name. It is the PVE name of *V*'s original VM:
     visible with `VM.Audit` and often guessable.
- **Impact.** *V* treats the attacker's VM as its own on every call:
  - **`Describe`.** The attacker's IPs and power state land in `status.ips`.
    Consumers that route by them (Services/EndpointSlices built from status,
    CAPI, DNS automation) send *V*'s traffic to the attacker. This is the most
    serious effect.
  - **`ExportDisk`, `Clone` source, `VMMigration` source.** The tenant's
    migration or clone copies the attacker's disk into a new VM bound to the
    tenant. That persists attacker-controlled content into the tenant's
    workload, possibly on another hypervisor.
  - **`Power`, `Reconfigure`, snapshots.** These act on the attacker's VM: low
    impact.
  - **`Delete`.** It destroys the attacker's own VM: no harm to others.

  Nothing of *V*'s original VM is disclosed, because it is gone. The finding is
  an integrity and impersonation issue.
- **Already documented** in `docs/vm-ownership.md` Limits, with the
  ACL mitigation ("grant `VM.Allocate` … only as you would
  `VM.Config.Options`").

### What a stamp does and does not prove

The stamp's checksum is unkeyed (`sum` = truncated SHA-256 of the line). Anyone
with `VM.Config.Options` on a VM can write a valid stamp for any UID they know,
which the provider documents as "PVE ACLs are the trust boundary". The UID is
not secret. It is readable by anyone who can `get` *V* in Kubernetes, or who has
`VM.Audit` on *V*'s original VM (it is in the Notes). Removing the name rule
therefore raises the attacker's bar from **knowing a name** to **knowing a
UUID and writing a well-formed stamp**. That is meaningful against a pool
co-tenant who never saw *V*'s VM config. It is not a defence against a principal
who could read *V*'s Notes before destroying it. Only binding to an attribute the
attacker cannot reproduce (Option E) closes that.

## Options considered

Each option is assessed on security effect, upgrade and rollback, API changes,
PVE API cost, failure modes, and effort.

### Option A: manager-side "stamp confirmed" latch plus a `require_owner_stamp` flag

The manager records on the `VirtualMachine` that the provider has **confirmed**
that the VM at `status.id` carries *V*'s stamp. From then on, every per-VM
request for *V* carries `require_owner_stamp = true`, and the provider refuses an
unstamped VM for that owner (`VM_NOT_OWNED`) instead of applying the name rule.

- **Where the confirmation comes from.** Only from a provider read-back, never
  from a claim. `DescribeResponse` gains `owner_check`, which says how the
  provider accepted the VM: `STAMPED` (the stamp records the request's UID) or
  `LEGACY_UNSTAMPED` (unstamped, accepted by the name rule). It costs nothing extra
  because the provider has already read the config for the check. A `Create`,
  `Clone` or `TransferOwner` success is not latched by itself. The next
  `Describe`, one reconcile later, confirms it. That keeps a single path and
  keeps "Status reflects reality" literal.
- **Latch, bound to the id.** `status.ownerStamp.confirmedID` is set once, to
  the `status.id` that was confirmed. The flag is sent only while
  `confirmedID == status.id`, so a later change of `status.id` can't carry a
  stale latch. The status is written only on change, so no hot loop.
- **Security effect.** This closes M2 for every VM that has been confirmed
  stamped: everything created or cloned by rc1+/rc2+ and everything adopted
  through #392. It does nothing for legacy VMs until they are stamped, which is
  Option B's job.
- **Upgrade and rollback.** Everything is additive, and a missing value means
  today's behaviour:
  - **Old manager with a new provider.** The flag is absent, so the name rule
    applies as today, and `owner_check` is ignored.
  - **New manager with an old provider.** `owner_check` is `UNSPECIFIED`, so
    nothing is latched and the flag (sent only after a latch) is never sent.
    The Provider status says the legacy rule is still in effect.
  - **Manager downgrade.** The older CRD prunes `status.ownerStamp` on its next
    status write, and behaviour returns to today's. The stamps stay valid. On
    re-upgrade the latch is rebuilt from the next `Describe`.
  - **Lost status** (restore, admin `replace`). Behaviour fails open to today's,
    and the latch is rebuilt from the next `Describe`. A restore that gives *V*
    a new UID is the separate A6 `TransferOwner` case.
- **Failure modes.**
  - **A confirmed VM later reads as unstamped**, because an admin stripped the
    Notes or the VMID was reused by a look-alike. It is held as
    `VMNotOwned`. The manager must **never** auto-re-stamp a confirmed VM,
    because this is exactly the attack signature. The resolution is the existing
    runbook: restore the stamp line by hand, or detach and re-adopt.
  - **Templates.** They are checked like any VM: a template stamped for *V* is
    accepted, and an unstamped one is refused once the latch is set.
- **PVE API cost.** None.
- **Effort.** Medium:
  - 3 proto fields (B adds none) with bindings regenerated.
  - Proxmox provider: the check already distinguishes the cases.
  - Mock provider, transport, and `contracts.VMRef`.
  - One CRD status struct.
  - The controller latch, with envtest.

### Option B: one-time stamping of legacy VMs on first owned contact

When `Describe` answers `owner_check = LEGACY_UNSTAMPED` through a provider that
reports `supports_owner_transfer`, the manager calls the existing
`TransferOwner(id = status.id, owner = V, replaceable_owner_uids = [])`. It is
compare-and-swap and already enforces what this needs:
- the PVE name must be the owner's (`unstampedNamedLikeOwner`), so it matches
  the rule that accepted the VM;
- the VM must have no stamp;
- the VM must not be a template or locked;
- the write is conditional on the config `digest`, and the stamp is read back.

The next `Describe` reports `STAMPED`, and Option A latches.

- **Security effect.** This turns the legacy population into stamped VMs, so
  that Option A covers them and a later release can drop the name rule. It is
  trust-on-first-use. A look-alike already at the VMID when the upgraded
  manager first reconciles (seconds to minutes after the upgrade) is stamped as
  *V*'s. That is no worse than today, because the same VM is already accepted
  as *V*'s on every reconcile. It freezes the current trust and does not add
  new trust.
- **Upgrade and rollback.** The stamps are ordinary #380 stamps. Providers from
  rc1 onward accept them by UID. Providers older than rc1 ignore a Notes line.
  A downgrade loses nothing.
- **Failure modes.**
  - **The stamp write fails.** A locked VM (backup), a `digest` race or PVE
    unreachable is retryable: the manager backs off (shared blocked-VM backoff,
    at most 5 min) and the VM stays accepted by the name rule meanwhile.
    `PermissionDenied`, because the token lacks `VM.Config.Options` on the
    legacy VM's path, is recorded once (Warning event, metric) and retried
    slowly.
  - **The VMID is reused between `Describe` and `TransferOwner`.** The name
    check and the `digest` CAS run again at write time. A same-named VM planted
    in that gap is the same residual the name rule has today.
  - **Adopted VMs.** An unstamped VM adopted before #392 matches
    `AdoptedVMName`, so it is stamped. One that still carries a previous
    owner's stamp is already `VMNotOwned`, and B never touches it
    (`replaceable_owner_uids` is empty). The existing runbook still applies.
  - **Templates.** `TransferOwner` refuses them, so the VM stays legacy and is
    reported.
  - **A provider without `supports_owner_transfer`.** B does not run, and the
    Provider status reports "N VMs unconfirmed: upgrade the provider".
- **PVE API cost.** This is once per legacy VM: 1 config `GET`, 1 config
  `POST` with its task wait, and 1 read-back `GET`. The manager paces it per
  Provider (proposal: at most one in flight and 30 per minute), so an upgrade
  with thousands of legacy VMs does not burst the token's budget.
- **Effort.** Small to medium. It is one controller step that reuses the
  adoption `TransferOwner` client and its error classification.

### Option C: keep the name rule only for `VirtualMachine`s created before a cutoff

The manager sends `require_owner_stamp` when
`metadata.creationTimestamp > cutoff`, with the cutoff taken from a Helm value
or recorded at first start.

- **Security effect.** This is a heuristic, not evidence. It protects new
  `VirtualMachine`s only. Those are already the ones Option A protects with
  certainty. It never protects legacy ones.
- **Failure modes.** A `VirtualMachine` created after the cutoff can be
  legitimately unstamped: through a provider older than #380, an older
  `VMClone` manager, or adoption through a provider without transfer. It is
  then wrongly held `VMNotOwned` and its `Delete` leaves the VM behind. The
  cutoff is also new configuration that can be wrong.
- **API changes.** The same flag as A, plus configuration. **Effort.** Small.
- **Rejected.** It is strictly worse than A at the same cost.

### Option D: do nothing, document only (status quo)

- **Security effect.** M2 stays open. It relies on PVE ACL hygiene: grant
  `VM.Allocate` where VirtRigaud's VMs live only to principals trusted like the
  Provider token. That is already documented.
- **Cost.** None. The name rule stays forever, and the legacy population never
  shrinks.
- **Rejected as the end state.** The attack needs only a common PVE
  delegation (a shared pool), and its strongest effect (traffic redirection
  through `status.ips`) is invisible to the tenant.

### Option E (considered, deferred): bind to an immutable PVE creation fingerprint

The manager records an attribute of the VM that PVE sets at creation and a
non-root principal cannot set or change, and the provider compares it on every
call. The candidate is the `meta: ctime=…` config property, which PVE describes
as read-only meta-information. `vmgenid` and `smbios1.uuid` can be set by the
creator, so they are not candidates. This is the Proxmox counterpart of
libvirt's `expected_uuid` and vSphere's never-reused MOID. It is the only option
that defeats a **copied** stamp.

It is deferred because three questions are unverified:
- whether PVE really refuses `meta` to non-root principals on create and
  update;
- whether a backup restore in place keeps or changes `ctime`, because changing
  it would refuse *V*'s own restored VM;
- which PVE versions set it, because VMs created on older PVE have none.

It also has a first-contact gap for legacy VMs, the same as B. It is additive to
this design: `status.ownerStamp` would gain `fingerprint`, and the request would
carry `expected_fingerprint`.

## Decision (proposed)

**Adopt A and B together in one release, then drop the name rule in the next
minor release. Spike E separately.**

### D1: Wire contract (additive)

Field numbers are the next free ones at implementation time. Today they are 4,
8 and 27.

```proto
message ObjectIdentity {
  string uid = 1;
  string namespace = 2;
  string name = 3;
  // require_owner_stamp, on the owner of a per-VM request (DescribeRequest,
  // PowerRequest, ReconfigureRequest, Snapshot*Request, GetDiskInfoRequest,
  // ExportDiskRequest, DeleteRequest .owner; CloneRequest.source_owner), tells
  // a provider that applies a legacy rule to unstamped VMs (Proxmox: the name
  // rule) that this owner's VM is known to be stamped: an unstamped VM is
  // NOT_FOUND + VM_NOT_OWNED. Ignored everywhere else, and by providers that
  // have no such rule. False (the default, and every older manager) keeps the
  // provider's legacy behaviour.
  bool require_owner_stamp = 4;
}

// OwnerCheckResult says how a provider that owner-checks per-VM calls accepted
// the VM for the request's owner.
enum OwnerCheckResult {
  // Not checked: no owner on the request, or a provider that does not check
  // owners on this call or predates this field.
  OWNER_CHECK_RESULT_UNSPECIFIED = 0;
  // The VM's owner stamp records the request owner's uid.
  OWNER_CHECK_RESULT_STAMPED = 1;
  // The VM is unstamped and was accepted by a legacy rule (Proxmox: named
  // like the owner).
  OWNER_CHECK_RESULT_LEGACY_UNSTAMPED = 2;
}

message DescribeResponse {
  // ... fields 1-7 unchanged ...
  OwnerCheckResult owner_check = 8;
}

message GetCapabilitiesResponse {
  // ... fields 1-26 unchanged ...
  // supports_owner_stamp_required reports that the provider honours
  // ObjectIdentity.require_owner_stamp and reports
  // DescribeResponse.owner_check. The manager uses it only to report, in the
  // Provider status, whether the legacy rule is still in effect; the flag is
  // safe to send to any provider.
  bool supports_owner_stamp_required = 27;
}
```

The ObjectIdentity placement is chosen over adding a field to each of the ten
per-VM request messages. The provider reads the owner through one helper
(`requestOwner`) and the manager writes it through one (`vmOwnerIdentity` /
`contracts.VMRef`), so one field reaches every call. The cost is that the field
is meaningless on the other `ObjectIdentity` uses (migration, image, target
identities). The field comment says so.

Provider parity:
- **Proxmox** honours the flag and reports `owner_check`.
- **Mock** implements both, for controller and conformance tests.
- **vSphere and single-host libvirt** have no legacy rule (see D6). They leave
  `owner_check` unset and ignore the flag. They do **not** advertise the
  capability, so the manager reports nothing for them.
- **Clustered libvirt** already requires a stamp on every routed call. It may
  report `STAMPED` (optional) but needs no change.

### D2: CRD status (additive)

```go
// VirtualMachineStatus
// OwnerStamp records that the provider confirmed (DescribeResponse.owner_check
// = STAMPED) that the VM at status.id carries this VirtualMachine's owner stamp.
// From then on every per-VM request for that id requires the stamp, so a VM
// planted at a reused id under this VirtualMachine's name is refused. Written
// once by the manager; never by tenants (status subresource).
// +optional
OwnerStamp *VMOwnerStampStatus `json:"ownerStamp,omitempty"`

// VMOwnerStampStatus is the manager's record of a confirmed owner stamp.
type VMOwnerStampStatus struct {
	// ConfirmedID is the status.id whose stamp was confirmed. The requirement
	// applies only while status.id equals it.
	// +kubebuilder:validation:MaxLength=255
	ConfirmedID string `json:"confirmedID"`
	// ConfirmedTime is when the confirmation was recorded.
	ConfirmedTime metav1.Time `json:"confirmedTime"`
}
```

A typed field is chosen over a condition. A condition is an observation that
controllers rewrite. This is a latch that is only ever set, and its binding to
an id must be explicit.

### D3: Manager behaviour

1. **Build the owner.** For a VM with `status.ownerStamp.confirmedID ==
   status.id`, the owner is built with `require_owner_stamp = true`. This
   covers `Describe`, `Power`, `Reconfigure`, snapshots, `GetDiskInfo`,
   `ExportDisk`, `Delete`, and a `Clone` or `VMMigration` source (built from
   the **source** VM's latch).
2. **On `Describe`:**
   - `STAMPED`: if not latched for this id, write `status.ownerStamp` once
     (status write only on change, `ObservedGeneration` untouched).
   - `LEGACY_UNSTAMPED` through a provider with `supports_owner_transfer`, and
     not latched: run the paced one-time `TransferOwner` (Option B). Its
     outcome is recorded:
     - success: an `OwnerStampMigrated` Normal event;
     - retryable failure: back off;
     - `AlreadyExists` or `NotFound`: one Warning event, metric reason
       `owner-stamp-migration-refused`, re-tried only at the slow cadence.
   - `LEGACY_UNSTAMPED` through a provider without the capability: nothing
     beyond the Provider status count.
   - `VM_NOT_OWNED` while latched: the existing `VMNotOwned` hold, with **no**
     `TransferOwner`.
3. **Report in the Provider status and metrics:**
   - the Provider status counts this Provider's VMs not yet confirmed and
     whether the provider reports `supports_owner_stamp_required`;
   - a gauge `virtrigaud_vm_owner_stamp_unconfirmed{provider_namespace,provider}`
     reports the same count;
   - this count is the pre-flight gate for D4.
4. **Opt-out.** A per-Provider opt-out of Option B
   (`virtrigaud.io/stamp-legacy-vms: "false"`) is proposed for operators who
   must not have VirtRigaud write the Notes of existing VMs. Open question 2
   decides whether it ships.

### D4: Phases

| Phase | Release | What |
|---|---|---|
| 1 | v0.4.0 (rc2 if accepted into scope, otherwise rc3 or v0.4.1) | D1 to D3: the flag, `owner_check`, the latch, one-time legacy stamping, reporting. The name rule still applies to unconfirmed VMs. `docs/vm-ownership.md` Limits is rewritten to the residual below. |
| 2 | v0.5.0 | The Proxmox provider drops the name rule by default: with an owner UID present, an unstamped VM is `VM_NOT_OWNED` on every per-VM call and on `Delete` (fail-safe: `VMLeftOnHypervisor`). `TransferOwner` keeps its name check, because it is the VMID-reuse guard of adoption, not an authorization. An escape hatch `PROXMOX_ALLOW_LEGACY_UNSTAMPED_NAME_RULE=true` restores phase-1 behaviour, logs a warning at start and is reported in capabilities and diagnostics. Upgrade notes require the D3 gauge to be 0 first. |
| 3 | v0.6.0 | The escape hatch is removed. `require_owner_stamp` stays on the wire, and a provider without the rule ignores it. |

Phase 2 compatibility:
- A phase-2 provider behind an older manager refuses every unconfirmed
  unstamped VM. That is intended, and documented as "upgrade the manager to
  phase 1 and let the gauge reach 0 before upgrading the provider".
- A phase-2 manager behind a phase-1 provider still benefits from the flag.

### D5: Residual after phase 1

- **Legacy VMs not yet stamped.** This is the window between upgrade and
  first contact, VMs whose stamping is refused or failing, and providers
  without transfer. The name rule still applies to them, so they keep M2 until
  stamped. The gauge shows them.
- **A copied or forged stamp.** A principal who knows *V*'s UID and can write
  the new VM's description can forge the stamp. This is unchanged and is the
  documented ACL trust boundary. Option E is the follow-up that closes it.
- **A VMID freed and reused between the owner check and the call.** This is
  unchanged and is the existing Limits entry, which the `digest` fix for
  `Reconfigure` would close.

### D6: vSphere and libvirt

There is no name rule in either provider, and Option A and B need no
provider change there.

- **vSphere.** `Create` binds only on a stamped UID (`requesterOwnsVM`), and
  single-host per-VM calls are not owner-checked (they ignore `owner`). The
  M2 precondition, an id reused by another VM, does not arise: MOIDs are never
  reused within a vCenter. A vCenter rebuilt under the same endpoint can
  re-issue MOIDs, but that is a different, pre-existing issue and out of scope.
  If per-VM owner checks are ever added to vSphere, they should require the
  stamp from day one and reuse this latch, with no name rule.
- **Single-host libvirt.**
  - **No rule and no check.** There is no name rule, but there is also no
    per-VM owner check: `status.id` is the (namespaced) domain name, and an
    undefined then redefined domain of that name is acted on unchecked.
  - **Not R14.** That is the same class of issue, but its precondition is
    write access to libvirtd on the host (effectively hypervisor root, with no
    fine-grained ACL comparable to `VM.Allocate`), so it is not tracked here.
    Bare-named legacy domains (pre-namespacing) are unstamped.
  - **If checks are added.** If per-VM owner checks are ever added to
    single-host libvirt (parity with clustered), the same design applies
    unchanged. A latch from `owner_check = STAMPED` plus a one-time
    `TransferOwner` (single-host libvirt supports it, with `expected_uuid`) can
    stamp legacy domains without ever introducing a name rule.
- **Clustered libvirt.** It already requires the stamp on every routed call.

## Consequences

- **Security.**
  - After phase 1, M2 is closed for every confirmed VM: all VMs created, cloned
    or adopted by rc1+ and rc2+ once one `Describe` has run, and legacy VMs once
    stamped.
  - After phase 2, the name rule is gone. The remaining trust boundary is the
    documented one: PVE ACLs on `VM.Config.Options` and knowledge of the UID.
- **Contracts.**
  - Three additive proto fields and one additive status struct.
  - No breaking change and no migration tool.
  - The latch is derived from provider read-back, so it is self-healing after
    a manager downgrade or a lost status.
- **Behaviour change visible to operators.**
  - Phase 1 writes one Notes line on each legacy Proxmox VM, the same line
    adoption writes today. Opt-out is pending decision.
  - A confirmed VM whose stamp is removed by hand becomes `VMNotOwned` instead
    of silently accepted.
  - Phase 2 holds any VM still unstamped.
- **Cost.** There is no steady-state PVE API cost. The one-time cost is about 3
  calls per legacy VM, paced.

## Test plan

- **Proxmox provider** (`pvefake`):
  - **Cases.** Run every case of the owner table (stamped own, stamped other,
    unstamped named-like, unstamped adopted-name, unstamped other name,
    unreadable, multiple) for each flag value (`require_owner_stamp` false and
    true).
  - **Calls covered.** `Describe`, `Power`, `Reconfigure`, `SnapshotCreate`,
    `SnapshotDelete`, `SnapshotRevert`, `GetDiskInfo`, `ExportDisk` (both
    paths), `Clone` source and `Delete`. Assert that nothing is read or changed
    on a refusal.
  - **`owner_check` values.** Assert `owner_check` for each `Describe` case.
    It is `UNSPECIFIED` without an owner.
  - **Capability.** The capability is advertised.
  - **Phase 2.** The rule is off by default and the escape hatch restores it.
- **Proto and transport.**
  - Round-trip the three fields.
  - The flag is set on every per-VM request, including `Delete` and
    `CloneRequest.source_owner`, and only for a matching latch.
  - `owner_check` is mapped into `contracts.DescribeResponse`.
- **Controller (envtest).** For the latch and the one-time stamp:
  - the latch is written once on `STAMPED`, with no write on repeat;
  - nothing is written on `LEGACY_UNSTAMPED`;
  - a latch for another `status.id` is ignored;
  - one `TransferOwner` is sent for `LEGACY_UNSTAMPED` with
    `supports_owner_transfer`, none without the capability, and none once
    latched;
  - retryable errors back off, and `AlreadyExists` or `NotFound` give one
    event at the slow cadence;
  - a latched VM answered `VM_NOT_OWNED` is held `VMNotOwned` with no
    `TransferOwner`;
  - the gauge and the Provider status count are correct.

  For pacing and compatibility:
  - pacing holds under 100 legacy VMs;
  - an old-manager request (no flag) to a new provider behaves as today;
  - a new manager behind an old provider (no capability, `UNSPECIFIED`) sends
    no `TransferOwner` and writes no latch;
  - a manager downgrade (field pruned) re-latches on re-upgrade.
- **Attack regression (`pvefake` plus controller).**
  - **Confirmed *V*.** Destroy VMID *N*, then create an unstamped VM at *N*
    named *V*. `Describe`, `Power`, `ExportDisk`, `Clone`-source and `Delete`
    are refused. `status.ips` is not updated. There is no `TransferOwner`.
  - **Legacy *V*.** It is stamped once, and the same replacement afterwards is
    refused.
- **Mock.** It implements the flag and `owner_check`, so the conformance suite
  can assert the contract for any provider that advertises the capability.
- **Lab** (after merge, separately, on the Proxmox lab):
  - a VM created unstamped by v0.3.11 is stamped and latched after upgrade;
  - the token's `description` write on legacy VMs works;
  - for the Option E spike: whether `meta` can be set by a non-root token on
    create and update, and what a restore in place does to `ctime`.

## Open questions (for the maintainer)

1. **Release target for phase 1.** rc2 (it is about two proto fields, one
   capability, one status struct and two controller steps) or rc3/v0.4.1?
2. **Option B default.** Should one-time legacy stamping be on by default with
   a per-Provider opt-out (proposed), or opt-in? It writes Notes on existing
   VMs.
3. **Phase 2.** Is v0.5.0 the right release to drop the name rule, and should
   an escape hatch exist for one minor (proposed)?
4. **Proto placement.** Is a single `ObjectIdentity.require_owner_stamp`
   acceptable (proposed), or should there be per-request fields?
5. **Option E spike.** Should it be scheduled (virtualization specialist plus
   a lab check of `meta` read-only-ness, restore behaviour and PVE version
   coverage)? It is the only fix for a copied stamp.
6. **Residual risk.** Does the security review accept the forged-stamp
   residual (known UID plus `VM.Config.Options`) for v0.4.0, given the
   documented ACL boundary?
7. **PVE permission (to verify).** Does `POST /nodes/{node}/qemu` with `name`
   or `description` require `VM.Config.Options` in addition to `VM.Allocate`?
   This refines the "who" above but does not change the design.
