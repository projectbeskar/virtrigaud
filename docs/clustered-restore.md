# Backup and restore of clustered VirtualMachines

This page covers VirtualMachines on a **clustered** Provider
(`Provider.spec.topology: cluster`, [ADR-0007](adr/0007-clustered-orchestrator-provider.md),
section A6). It explains what happens when such a VirtualMachine comes back
from a backup, a GitOps re-apply or a re-create, and how an administrator
re-attaches it to its existing domain.

Clustered Providers are **experimental** in v0.4.0. Single-host libvirt,
vSphere and Proxmox Providers are not changed by anything on this page; their
recovery is in [Other Providers](#other-providers).

## Why a restore needs care

A restore, whether by Velero or by `kubectl apply` of an exported manifest,
re-creates the VirtualMachine with a **new UID**. By default it also comes back
**without its status**, so it has lost `status.id` and `status.placement`.

On a clustered Provider the UID is the only identity a domain's owner stamp
accepts. Every call the operator routes to a host is owner-checked against it.
Before A6, a restored VM looked new: it was scheduled onto another host and a
**second** domain `<namespace>.<name>` was created there. The original kept
running outside VirtRigaud's accounting, and on a shared pool its disk was
overwritten.

The rule VirtRigaud now keeps is this: **a clustered Provider holds at most
one domain per `<namespace>.<name>`** (A6, decision 2). The domains counted
are the ones VirtRigaud created, which carry the namespace and name in their
owner stamp. A domain that only has the same name but is unstamped or stamped
for another VirtualMachine is not counted: the create on that host is refused,
the host is excluded, and the VM is placed elsewhere.

The rule has one side effect. After `virtrigaud.io/orphan-on-delete`, a VM with
the same name can be re-created on a clustered Provider only after an
administrator re-attaches or removes the old domain.

## The four guards

| Guard | Where | When | What it does |
|---|---|---|---|
| **R1**, the restore marker | manager | before a VM is first placed | A VM whose `infra.virtrigaud.io/placement-uid` annotation names another UID is held (`RestorePending`). |
| **R2**, the previous-incarnation pin | provider and manager | on a `Create` or `Clone` | A domain stamped with the VM's namespace and name under another UID refuses the create (`VM_PREVIOUS_INCARNATION`). The VM stays on its pending host (`RestorePending`) and the host is not excluded. |
| **R3**, the cluster-wide disk guard | provider | when a file already exists where the disk goes, and on every `Delete` | A disk that a domain on **any** host uses is never overwritten or removed. See [the disk guard](clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61). |
| **R4**, the pre-schedule uniqueness check | manager, through the provider | before a VM is first placed | The Provider's hosts are asked for the domains stamped with the VM's namespace and name. A previous incarnation holds the VM (`RestorePending`). The VM's own domain has its host recorded as `pendingHost`, and the create retry binds it. |

R2 and R3 shipped in A6.1. R1 and R4 are A6.2.

### The restore marker (R1)

The marker is the annotation `infra.virtrigaud.io/placement-uid`. Its value is
the UID under which the VirtualMachine entered placement. The manager writes
it:

- on a create, before the first `status.placement.pendingHost` write;
- on a clone's target VirtualMachine, before the target's `pendingHost` write;
- on adoption, right before the binding write;
- on a bound VM, after an owner-checked call on its host succeeds. This covers
  VMs placed before the marker existed, and restored VMs once they are
  re-attached.

Backups and exported manifests carry the marker. A VM re-created under a new
UID therefore finds that the marker names another UID. If such a VM has no
`status.id` and no `status.placement.pendingHost`, it is **held**:

- it gets `Placed=False` and `Provisioning=False` with reason `RestorePending`,
  and one `Warning` event;
- it is re-checked with a backoff from 15 seconds, doubling up to 5 minutes;
- no image is prepared, nothing is scheduled and no `Create` is sent;
- deleting it makes no provider call, so its previous domain is never touched.

**Trust.** Tenants can write annotations, so the marker is untrusted input. It
can only *hold* the VirtualMachine that carries it. It never names a host,
never selects one, never authorizes a bind and never skips the scheduler or
R4. So a forged marker only makes the forger's own VM wait. Releasing a marker
(removing it, or setting it to the VM's own UID) is safe **once every host of
the Provider is reachable**, because R2, R3 and R4 still run after it. While a
host cannot be checked, a released VM is scheduled on the evidence of the
reachable hosts only. On a host-local pool, a second domain can then be created
next to a previous incarnation on the unreachable host; see
[The host with the domain is unreachable](#the-host-with-the-domain-is-unreachable).
Release a marker only when every `Host` of the Provider is `Ready`.

The key is in the operator's reserved `virtrigaud.io` domain. A `VMClone` or
`VMMigration` target is built without reserved keys, so it never inherits the
marker.

**Single-host and thin-client Providers ignore the marker completely.** They
neither read nor write it.

> A change to the marker does not trigger a reconcile on its own. A released VM
> is picked up at its next re-check, within 5 minutes.

### The pre-schedule uniqueness check (R4)

R4 runs for a clustered VM that has never been placed: no `status.id` and no
`pendingHost`. It runs once before the VM's first scheduling, and again on each
retry while the VM is held or unschedulable. It **never** runs for a bound VM
or for a VM whose create is already pending.

The manager sends one `ListVMs` with an owner filter (the VM's namespace and
name). On each host the provider looks up only this VM's candidates:

- the domain it would name for the VM, `<namespace>.<name>`, whatever its stamp;
- the legacy bare name `<name>`, but only when its stamp records this
  namespace and name.

**Candidate names only.** A domain named otherwise is **not** looked at, even
when its owner stamp records the VM's namespace and name. That includes every
**adopted** domain: adoption stamps a domain for the adopting VirtualMachine
under the name the domain already had. So if an adopted VM is detached with
`orphan-on-delete` and a VM with its namespace and name is created again (or
restored without its marker), R4 does not see the old domain, and neither
does R2. Only R3 protects a shared pool. Before re-creating an adopted VM,
find its old domain **by stamp** (runbook step 1) and re-attach or remove it.
A follow-up (ADR-0007, A6.2 follow-ups) makes R4 match by stamp.

| The hosts report | Result |
|---|---|
| no domain stamped with the VM's namespace and name | the VM is scheduled as usual |
| exactly one, stamped with the VM's **own** UID | its host is recorded as `pendingHost`, without scheduling and without capacity admission, because the domain already runs there. It is recorded, and counted, at the **domain's own size** (its current vCPUs and memory, its balloon maximum as the ceiling). The create retry there binds it as an idempotent success and records that size in `status.currentResources`. If it differs from the spec, a resize through the usual gate converges it (a grow must fit the host, and a shrink waits for power-off) |
| one stamped under **another** UID, more than one, or a candidate whose stamp cannot be read | held: `Placed=False/RestorePending`. Nothing is scheduled or created |
| only an unstamped domain, or one stamped for another VirtualMachine, that has the name | not counted. The slice 2 rule applies: that host is excluded when the create reaches it |

**Hosts that could not be checked** (unreachable, draining, past their
deadline) do not hold the VM (A6, decision 4). The VM proceeds on what the
reachable hosts report. A VM whose marker names another UID is held by R1
anyway. The residual is a previous incarnation on an unreachable host with a
**host-local** pool, where a second domain can still be created. On a shared
pool, R3 fails the create closed instead.

**The provider must support the filter.** R4 runs only through a clustered
provider that reports `supportsListOwnerFilter` and marks its answer as
filtered (`owner_filter_applied`). An older provider image does not, and the VM
is held with `Placed=False/ProviderLacksListOwnerFilter` until the provider is
upgraded. If the capability query or the listing fails, the VM is held with
`Placed=False/UniquenessCheckFailed`. Both holds back off from 15 seconds to 5
minutes. Neither message names a host or an endpoint; the details go to the
manager log.

**Cost.** One `GetCapabilities` and one filtered `ListVMs` per check. On each
host that is one `virsh list --all` and at most two definition reads, plus the
persistent definition of a running candidate, whatever the number of domains
on the host. The fan-out is the cross-host listing's: at most 8 hosts at a time
and 60 seconds per host, inside a 45-second deadline from the manager. A host
that is not finished by then is reported unreachable. Host-scoped failures are
never counted toward the Provider's circuit breaker.

**The VM's own domain on another host.** A pending create can still meet the
VM's own domain elsewhere. The disk guard then answers
`VM_PREVIOUS_INCARNATION` with kind `own`. That happens when the domain's host
was unreachable at the first placement, when the status was restored with a
different pending host, or when an administrator re-stamped a previous
incarnation that the guard had found on another host. The manager then runs
R4's lookup. If it finds exactly that one domain, on a `Host` of the Provider,
and no other domain for the namespace and name, it moves `pendingHost` there
in a checked status write. The admitted size is kept, and the next create
retry binds the domain. Otherwise the VM stays held with
`Placed=False/OwnDomainOnAnotherHost`, and deleting it is held too.

## Conditions

| Condition / reason | Set on | Meaning | What to do |
|---|---|---|---|
| `Placed=False/RestorePending` (and `Provisioning`) | a never-placed VM | R1: the marker names another UID. Or R4: a previous incarnation, more than one domain, or an unreadable stamp exists | [Re-attach](#re-attach-runbook) the previous domain, [discard it](#discarding-the-previous-domain), or release the marker |
| `Placed=False/RestorePending` (and `Provisioning`) | a VM with `pendingHost` | R2: the create on the pending host met a previous incarnation | [Re-attach](#re-attach-runbook) or [discard](#discarding-the-previous-domain). The marker needs no change |
| `Ready=False/RestorePending` | a bound VM whose marker names another UID | its status was restored, but the domain on its host is still stamped with the old UID | [Re-stamp](#re-attach-runbook) the domain. The marker is rewritten automatically |
| `Placed=False/OwnDomainOnAnotherHost` | a VM with `pendingHost`, or a never-placed VM | the VM's own domain is on another host, and the lookup could not move `pendingHost` there (or, for a never-placed VM, the domain is on a host that is not a `Host` of the Provider). Deleting the VM is held too (`DeleteBlocked=True/OwnDomainOnAnotherHost`) | See [Own domain found on another host](#own-domain-found-on-another-host) |
| `Placed=False/ProviderLacksListOwnerFilter` | a never-placed VM | the provider image predates A6.2 | Upgrade the clustered provider |
| `Placed=False/UniquenessCheckFailed` | a never-placed VM | R4's query failed | Check the provider (manager log); the check is retried |

Each hold emits one `Warning` event when it starts, and the manager counts it
under the error reason `restore-pending` (or `preschedule-check`). No message
names a host, a domain, another UID or another tenant's object.

## Backing up clustered VirtualMachines

- **Restore the VirtualMachine status** (A6, decision 6). With Velero, list the
  kind under `restoreStatus`:

  ```yaml
  apiVersion: velero.io/v1
  kind: Restore
  spec:
    restoreStatus:
      includedResources:
        - virtualmachines.infra.virtrigaud.io
  ```

  A restored status keeps the binding. The `spec.providerRef` lock and the
  committed-capacity count are in place from the first moment, and the owner
  checks keep the VM fail-closed until its domain is re-stamped.
- Back up the Provider's namespace with it (the `Provider`, `Host`, `HostPool`
  and their credential Secrets). The re-attach needs the `Host` objects.
- **Never run `kubectl replace --force` on a VirtualMachine.** Its delete half
  runs the finalizer, which destroys the domain.
- Restoring into a **second** Kubernetes cluster whose manager also manages the
  hosts is not supported. Fence the first cluster (scale its manager to zero)
  before you recover in the second one.

## Scenarios

### Velero restore, status not restored (the default)

The VM comes back with the marker naming its old UID and no status. **R1 holds
it** (`Placed=False/RestorePending`). No `Create` is sent and the original
domain keeps running. Follow the [re-attach runbook](#re-attach-runbook). If
the original was deleted normally after the backup, release the marker
instead: R4 finds nothing, and the VM is created as new.

### Velero restore with `restoreStatus`

- **`status.id` restored.** The VM is bound, so R1 does not hold it. Its
  owner-checked calls on the bound host find nothing, because the domain is
  stamped with the old UID. It shows `Ready=False/RestorePending` instead of
  `VMMissingOnHost`, and it is never re-created. **Re-stamp the domain** (steps
  1 and 2 of the runbook). The next reconcile's `Describe` succeeds and the
  marker is rewritten automatically.
- **Only `pendingHost` restored** (the backup was taken during a create). The
  create retry meets the domain stamped with the old UID. **R2 pins the VM** to
  its pending host (`RestorePending`). Re-stamp the domain; the create retry
  then binds it. If the domain is on a different host than `pendingHost`, the
  retry is answered "own domain elsewhere" and the manager moves `pendingHost`
  there itself.

### `kubectl apply` of an exported manifest

This is the same as Velero without status: `apply` ignores status, and the
manifest carries the marker. R1 holds the VM.

### GitOps re-apply

A manifest from git has no marker and no status. **R4** asks the hosts. If a
domain stamped with the VM's namespace and name under another UID exists on a
reachable host, the VM is held (`RestorePending`). Re-attach it with the
runbook (without the marker step), or remove the old domain. If R4 finds
nothing, the VM is created as new.

### `orphan-on-delete`, then a re-create of the same name

`orphan-on-delete` leaves the domain running, stamped with the deleted VM's
UID. The re-created VM, with a new UID and no marker, is **held by R4**. This
is the accepted side effect of the one-domain rule. Decide which you want:

- **keep the running domain** and manage it again: re-attach it (runbook steps
  1 and 2). R4 then records its host and the create binds it;
- **start fresh**: [discard the old domain](#discarding-the-previous-domain).

On a shared pool, R3 also refuses to write over the old domain's disk.

### Removing an old domain on purpose

See [Discarding the previous domain](#discarding-the-previous-domain). After
the domain is gone, the held VM is created as new at its next re-check.

### Own domain found on another host

A domain stamped with the VM's **own** UID is the VM's own domain; only its
placement record was lost. No re-stamp is needed:

- **Before the first placement**, R4 records that host as `pendingHost`, and
  the create retry binds the domain. No administrator action is needed.
- **With a pending create elsewhere**, the manager moves `pendingHost` to the
  domain's host itself.
- **The lookup cannot tell** (`Placed=False/OwnDomainOnAnotherHost` stays).
  This happens when the provider lacks the owner filter, the host is not a
  `Host` of the Provider, or another domain for the namespace and name exists.
  Resolve the cause: upgrade the provider, restore the `Host` object, or
  remove the extra domain. As a last resort, set the pending host by hand:

  ```sh
  kubectl patch virtualmachines.infra.virtrigaud.io <name> -n <namespace> --subresource=status \
    --type=merge -p '{"status":{"placement":{"pendingHost":"<host>"}}}'
  ```

  Deleting such a VM is held (`DeleteBlocked=True/OwnDomainOnAnotherHost`),
  whether or not it was ever placed: releasing it would leave its own domain
  running. The hold lasts until its pending host points at the domain (the
  delete then removes it), or until you set `virtrigaud.io/force-delete` or
  `virtrigaud.io/orphan-on-delete` (the domain is then left for manual
  removal).

### Restore into a new namespace while the original runs

R1 holds the copy. A domain name is derived from the namespace, so the copy can
never re-attach to the original's domain. Release the marker to create it as a
new VM, or delete it.

### The backup is older than a normal delete

The domain is gone. R1 holds the restored VM. Release the marker: R4 finds
nothing, and the VM is created as new.

### The host with the domain is unreachable

- A VM whose marker names another UID stays held (R1), whatever the hosts
  report.
- A VM without a marker (a GitOps re-apply) is **not** held by an unreachable
  host (decision 4). It is scheduled on the evidence of the reachable hosts. On
  a shared pool, R3 fails its create closed. On a **host-local** pool, a second
  domain can be created. This is the one accepted residual. Bring the host back
  before re-applying VMs whose previous incarnation it may hold.

## Re-attach runbook

Re-attaching is not automated in v0.4.0; the `VMRestoreBinding` kind (A6.4)
will automate it later. The steps below need root (or the provider's SSH user)
on the hosts, and read access to VirtualMachines in all namespaces.

**0. Read the held VM's UID.**

```sh
NEW_UID=$(kubectl get virtualmachines.infra.virtrigaud.io <name> -n <namespace> -o jsonpath='{.metadata.uid}')
kubectl get virtualmachines.infra.virtrigaud.io <name> -n <namespace> \
  -o jsonpath='{.metadata.annotations.infra\.virtrigaud\.io/placement-uid}{"\n"}'
```

**1. Find the previous domain by its stamp, not by its name.** A domain may
be called anything: `<namespace>.<name>`, the legacy bare `<name>`, or the
name it had before it was adopted. So on **each** host of the Provider (every
`Host` in the Provider's namespace, whatever its state), list every domain
whose owner stamp records the held VM's namespace and name:

```sh
NS=<namespace>; NAME=<name>
URI=https://virtrigaud.io/xmlns/libvirt/owner/v1
for d in $(virsh list --all --uuid); do
  s=$(virsh metadata --domain "$d" --uri "$URI" 2>/dev/null) || continue
  if printf '%s\n' "$s" | grep -qF -e "namespace='$NS' name='$NAME'" -e "namespace=\"$NS\" name=\"$NAME\""; then
    echo "$d $(virsh domname "$d") $s"
  fi
done
```

Each line is a domain's UUID, its name and its owner stamp, for example
`<owner uid='…' namespace='<namespace>' name='<name>'/>` (libvirt may print
the attributes with double quotes). `virsh metadata` reads the running
definition of an active domain. Step 2 rewrites both definitions. Then:

- **Stop if more than one domain, on any host, is stamped for this namespace
  and name,** or if a host could not be checked. At most one may be
  re-attached. Discard the others first.
- Check that the namespace and name in the stamp are the held VM's own.
- Check that **no VirtualMachine with the stamp's UID exists**, in any
  namespace, terminating ones included:

  ```sh
  kubectl get virtualmachines.infra.virtrigaud.io -A \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | grep -x '<stamp uid>'
  ```

  If it prints the UID, that VirtualMachine still owns the domain. Stop.

**2. Re-stamp the domain with the held VM's UID**, keeping the namespace and
name. Address the domain by its UUID (`virsh domuuid <domain>`):

```sh
virsh metadata --domain <domain uuid> --uri https://virtrigaud.io/xmlns/libvirt/owner/v1 --key virtrigaud \
  --set "<owner uid='${NEW_UID}' namespace='<namespace>' name='<name>'/>" --config --live
# drop --live if the domain is shut off
```

This is the same write the provider's `TransferOwner` makes. Read the stamp
back with the command from step 1, and for a running domain also with
`virsh dumpxml --inactive <domain>`. libvirt keeps one element per namespace
URI, so the old stamp is replaced, not duplicated.

**3. Only for a VM held by R1 (never placed): release the marker.** Set it to
the held VM's UID, or remove it:

```sh
kubectl annotate virtualmachines.infra.virtrigaud.io <name> -n <namespace> \
  infra.virtrigaud.io/placement-uid="${NEW_UID}" --overwrite
# or: kubectl annotate virtualmachines.infra.virtrigaud.io <name> -n <namespace> infra.virtrigaud.io/placement-uid-
```

At its next re-check (within 5 minutes), R4 finds the domain stamped with the
VM's own UID and records its host as `pendingHost`. The create retry there
binds the VM as an idempotent success, and records `status.id` and the
resources as for any create. **There is no manual status edit.**

A VM that is pinned by R2, or whose status was restored, needs no step 3. Its
create retry (R2), or its next `Describe` (a restored `status.id`), binds it,
and the marker is rewritten automatically.

**4. Verify.** `Placed=True/Bound`, `status.id` and `status.placement.host`
are set, the marker equals the VM's UID, and
`virtrigaud_host_committed_cpu` / `_memory_mib` count the VM once on its host.

### Discarding the previous domain

To give the held VM a fresh start instead, remove the old domain and its files
on its host:

```sh
virsh destroy <namespace>.<name>          # if it is running
virsh undefine <namespace>.<name> --nvram # --nvram for UEFI domains
```

Then remove its disk from the pool: `<pool>/<namespace>.<name>-disk.qcow2`, or
`-disk` for a blank volume, or `-migrated.qcow2` for an imported disk. Also
remove its cloud-init seed. **On a shared pool, first check that no domain on
any host uses the disk.** Finally, release the marker (runbook step 3) if the
VM carries one. At its next re-check, R4 finds nothing and the VM is created
as new.

## Other Providers

- **Single-host libvirt** and **vSphere**: a restored VM keeps retrying its
  create, which is refused because the owner stamp names the old UID
  (`ProviderConflict`, re-checked every 2 minutes). Re-stamp the domain as in
  step 2, keeping the namespace and name; the create retry then binds it. On
  vSphere, the stamp is the VM's ExtraConfig key `virtrigaud.owner.uid`. No
  marker is involved.
- **Proxmox** has no owner stamp, so a restore without status creates a second
  VM. This is tracked with the parked Proxmox work. Restore Proxmox
  VirtualMachines with their status.

## See also

- [ADR-0007 A6](adr/0007-clustered-orchestrator-provider.md): the decision, the
  threat model and the A6.1 and A6.2 amendments.
- [Clustered-provider inventory](clustered-provider-inventory.md): listing and
  adoption, the cluster-wide disk guard, and previous incarnations.
- [Libvirt domain ownership](libvirt-domain-ownership.md): the owner stamp.
