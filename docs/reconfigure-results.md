# Reconfigure results: applied, restart required, or failed

A `Reconfigure` changes a VirtualMachine's CPU, memory and disk size. Since this
release it has exactly three outcomes, and a provider never reports success for
a change it did not apply:

| Outcome | What the provider did | What the manager records |
|---|---|---|
| **Applied** | Applied every requested change to the running VM *and* to its persistent definition (or, for a VM that is off, to the definition it boots from). | `status.currentResources` = the new size; `Reconfiguring=False`, reason `ReconcileSuccess`. |
| **Restart required** | Applied at least one change to the persistent definition only, because the running VM cannot take it; every other change was applied live too. The change takes effect at the VM's next **power cycle** (power off, then on — a reboot from inside the guest keeps the running QEMU and is not enough). | `Reconfiguring=True`, reason `RestartRequired`; `status.currentResources` holds, per resource, the larger of the running and the next-boot size (see [the invariant](#statuscurrentresources-invariant)). |
| **Failed** | Could apply a change neither way, or could not tell (the host was unreachable). Part of the change may already be in the persistent definition, or even live. | `Reconfiguring=False`, reason `ProviderError`, with a message saying what could not be done. On a clustered Provider `status.currentResources` is recorded as for restart required (the larger size); on a single-host Provider it is untouched. The call is retried on a per-VM backoff. |

One failure has its own reason: a provider that **cannot apply a change to a VM
that is not powered off and cannot stage it for the next boot either** (vSphere
without hot-add, among others — see
[vSphere: changes a running VM cannot take](#vsphere-changes-a-running-vm-cannot-take))
refuses the whole request before changing anything. The manager records
`Reconfiguring=False`, reason **`ResizePendingPowerOff`**, with a constant
message; `status.currentResources` is untouched, and the call is asked again on
the same per-VM backoff as a failure, and at once after a spec change — so
setting `spec.powerState: Off` applies it. The VM stays `Ready`. On the wire it
is `FailedPrecondition` with the `google.rpc.ErrorInfo` reason
`RECONFIGURE_NEEDS_POWER_OFF`, which never counts toward the Provider's circuit
breaker.

The result travels on the wire as `TaskResponse.restart_required` (an additive
field of the `Reconfigure` response, `proto/provider/v1/provider.proto`) and
reaches the manager as `contracts.ReconfigureResult.RestartRequired`. vSphere,
Proxmox and the mock provider report `false`; see
[Other providers](#other-providers).

A provider that implements this contract (the **honest result contract**)
marks **every** Reconfigure response with `TaskResponse.honest_result`
(`contracts.ReconfigureResult.Honest`; the SDK's
`capabilities.HonestReconfigureResponse` builds one) and advertises
`GetCapabilitiesResponse.supports_honest_reconfigure`. The libvirt provider
does both.

## libvirt: what "applied" means

The libvirt provider grows the disk first — the step a request can make fail
(a VMClass disk size the host cannot give), so it fails before any CPU or
memory changes. It then reads the domain's persistent definition
(`virsh dumpxml --inactive`) and changes it **first** (`--config`), then the
running domain (`--live`). A live change is therefore never left unpersisted —
before this release a live change was never written to the definition and was
silently undone at the next power cycle.

- **vCPUs.** A count above the definition's vCPU maximum raises the maximum
  first (`setvcpus --config --maximum`). A running domain then gets
  `setvcpus --live`; if it refuses — a grow beyond its running maximum (a VM
  created without CPU hot-add), or an unplug of vCPUs that are not hotpluggable
  — the change is *restart required*.
- **Memory.** A domain's `<memory>` is the balloon maximum: what the guest can
  use without any host action. `<currentMemory>` is the balloon target, which a
  running guest may ignore.
  - A grow within the running `<memory>` (a VM created with memory hot-add)
    moves the balloon target up (`setmem --live`): **applied**.
  - A grow beyond it raises `<memory>` in the definition: **restart required**.
  - A **shrink** lowers `<memory>` and `<currentMemory>` in the definition, so
    the smaller size is enforced at the next boot rather than merely requested
    of the guest. On a running VM it is always **restart required**; the live
    balloon is neither trusted nor touched. (Lowering `<memory>` also removes
    any memory hot-add headroom the VM had.)
  - A `setmaxmem` failure fails the call.
- **Disk.** Grow-only: the VMClass disk size is a floor, and a disk already that
  large is not touched. A running VM's disk is grown live (`blockresize`, then a
  best-effort in-guest filesystem grow); a stopped VM's backing volume is
  resized. A grow that fails — including one a network disk (no host path)
  would need on a clustered Provider — fails the call.
- **State.** Only a *running* (or `idle`) domain is changed live and only a
  *shut off* domain is changed in its definition alone. A domain that is
  active but not running — paused, suspended to RAM (`pmsuspended`), shutting
  down, crashed but kept — or in an unknown state is refused with a retryable
  error before anything is changed: a guest suspended to RAM wakes at its old
  size.

Errors say what could not be done ("could not set 4 vCPUs in the VM's
persistent definition") and never carry the virsh command line, its output, a
host path or an SSH endpoint; the provider log has the detail. On a clustered
(routed) Provider they are `VM_OPERATION_FAILED`, which the manager keeps out
of the Provider's circuit breaker; an unreachable host stays
`HOST_UNAVAILABLE`. On a single-host Provider a failure of the operation on the
VM also carries `VM_OPERATION_FAILED` (historical code and message kept); a host
that could not be reached keeps the plain error, which the breaker counts.

## Power states: "Off" means powered off

The libvirt provider reports `running`/`idle` domains as `On`; `shut off`,
`in shutdown` and `crashed` as `Off`; `paused` and `pmsuspended` as
**`Suspended`**; anything else as **`Unknown`** (both new values of
`status.powerState`). While a VM is `Suspended` or `Unknown` the manager neither
powers it on nor reconfigures it (`Ready=False`, reason `PowerStateUnmanaged`).
A `Suspended` VM whose `spec.powerState` is `Off` is powered off (destroyed). One
whose `spec.powerState` is `OffGraceful` is **held** with a message saying a hard
`Off` is required: a suspended guest cannot shut down gracefully, and a graceful
request is never turned into a hard power-off. (Resuming or waking the domain
first was not chosen: it would run the guest again at the size it holds — on a
clustered Provider with a shrink waiting for power-off, exactly what the
`Suspended` state guards against — a `pmsuspended` wake needs the guest's
cooperation, and a timed destroy fallback is more machinery than one field
change by the owner.) Resume it on the hypervisor and it is shut down
gracefully. An `Unknown` VM is left alone entirely — including one a
third-party provider reports in a state outside the enum. An adopted VM that is not powered off is adopted with
`spec.powerState: On`. New domains on a clustered Provider are created with
guest suspend to RAM and to disk disabled (`<pm>`). Clustered domains created
before this release keep their definition and can still suspend to RAM; they are
then reported `Suspended` and left alone. Adding `<pm>` to them on a later
offline reconfigure is a follow-up (it means rewriting the domain definition).

## `status.currentResources` invariant

`status.currentResources` never records less than the VM can hold, now or after
its next boot: per resource, the larger of the size it runs with and the size it
boots with next.

- A **grow** pending a restart is recorded at once: the next boot takes it, so a
  clustered Provider's committed-capacity accounting counts it from now on.
- A **shrink** pending a restart is **not** recorded: the running VM still
  holds its old size. On a clustered Provider a running VM's shrink is not even
  sent until the VM is powered off (`ShrinkPendingPowerOff`), and a powered-off
  domain takes it in its definition, which is *applied*.
- While a change is pending a restart, the manager asks the provider again
  every 2 minutes, and at once after a spec change. Once the VM has been power
  cycled the provider answers *applied*, the desired size is recorded and the
  condition clears.
- A **failed** `Reconfigure` may have applied part of the change. On a clustered
  Provider it is recorded like a restart-required one — the larger size — so a
  VM that may run with its grown size is never counted below it, even if its
  owner reverts the spec. On a single-host Provider (no capacity accounting)
  `status.currentResources` is untouched.
  - This includes a clustered **grow** that failed because its host was
    unreachable (`HostUnavailable`): nobody can tell whether the host applied
    it, so the VM is counted at the grown size. If the owner then reverts the
    spec, that is a shrink — and a running clustered VM's shrink waits for a
    power-off (`ShrinkPendingPowerOff`) — so the count comes back down only
    after the VM is powered off (`spec.powerState: Off`) and the shrink is
    applied.
- A clustered answer **without the honest-result marker** (an older provider
  image behind a capability snapshot that is out of date) is not trusted: it is
  recorded like a failure — the larger size — with
  `Reconfiguring=False/ProviderLacksHonestReconfigure`, and re-checked every
  2 minutes until a marked answer records what was applied. Single-host
  Providers ignore the marker.
- A failed `Reconfigure` is sent again — on a per-VM backoff, 5 s doubling to
  5 min, and at once after a spec change — even if the spec is reverted to the
  recorded size, until one succeeds, so a partly-applied definition converges.
- On a clustered Provider, whenever `Describe` reports more vCPUs online
  (`DescribeResponse.vcpus`) than recorded, the recorded CPU is raised to them;
  it is never lowered by a report. A reported value is bounded first: vCPUs to
  128 and a memory maximum to 100 TiB (the VMClass maxima, which are also the
  CRD maxima of the recorded fields), and the committed-capacity sums saturate
  rather than overflow, so no report can make a host look free.

## Clustered memory ceiling

A clustered VM's memory counts at the larger of `status.currentResources` and
its balloon ceiling, `status.placement.memoryCeilingMiB`. `Describe` now reports
the domain's actual memory maximum (`DescribeResponse.max_memory_mib`):

- a VM scheduled before the ceiling was recorded gets it recorded **once** from
  that report — its VMClass's memory hot-add flag, which the VM's owner can
  change, no longer sizes it;
- a recorded ceiling is **raised** at once when the provider reports more than
  both the ceiling and the recorded memory (the domain can reach it — the
  conservative side);
- a recorded ceiling is **lowered** when the provider reports less — after a
  memory shrink, which lowers the domain's maximum — except while a change is
  pending a restart, a reconfigure task is in flight, or the last Reconfigure
  failed.

## Providers without the honest result

A provider reports the contract with
`GetCapabilitiesResponse.supports_honest_reconfigure`, surfaced as
`Provider.status.reportedCapabilities.supportsHonestReconfigure`, and proves it
on every answer with `TaskResponse.honest_result`. The libvirt provider does
both. On a **clustered** Provider:

- without the capability (an older provider image), the manager sends no resize
  at all — grow or shrink — because the committed-capacity accounting would
  trust a reply that may not be true: the VM keeps its size with
  `Reconfiguring=False`, reason `ProviderLacksHonestReconfigure`, re-checked
  every 30 s;
- with the capability but an answer without the marker (the capability is a
  snapshot the manager keeps if a later `GetCapabilities` fails, so a
  rolled-back provider can still look capable), the answer is not trusted —
  see the invariant above.

A single-host Provider without either is resized as before, and the manager
logs a warning.

## Which disk a size goes to

A `Reconfigure` request carries the VMClass `spec.diskDefaults.size` and each
`spec.disks[].sizeGiB`. A size goes only to the disk `Create` made for it. On
vSphere and Proxmox **`Reconfigure` never touches the root (primary) disk**, and
no provider's `Reconfigure` adds a disk or shrinks one. A size of 0
means "leave this disk as it is" (the manager sends a growth above the
[disk-size limit](../charts/virtrigaud/README.md#disk-size-limit) that way).

| Size | vSphere | libvirt | Proxmox |
|---|---|---|---|
| VMClass `spec.diskDefaults.size` (the root disk) | Not applied by `Reconfigure` (`Create` applies it). | Grows the primary disk, a floor (unchanged by this release). | Not applied (and the Proxmox `Create` does not apply it yet either). |
| `spec.disks[].sizeGiB` (additional disks) | Grows the additional disk `Create` attached for that entry, found by the name recorded with it (below). A **smaller** size is refused (`InvalidSpec`, nothing changed — the CPU/memory change in the same request included): a disk is never shrunk. | Not applied: the libvirt `Create` attaches no `spec.disks`. | Not applied: the Proxmox `Create` attaches no `spec.disks`, so no disk is the entry's — not `scsi0`, not a template's `scsi1`. |

How vSphere finds an entry's disk: `Create` records each additional disk it
attaches in the VM's `ExtraConfig` — `virtrigaud.disk.additional.<N>` (the disk
file) and, since this release, `virtrigaud.disk.additional.<N>.name` (the
`spec.disks` name). An entry goes to the one disk recorded with its name, so
reordering or removing `spec.disks` entries never moves a size to another disk.
The disk is found by its recorded file, or by the file's base name after a
storage migration. vSphere leaves a disk alone, and logs why, when:

- **no disk is recorded under the entry's name**: the entry was added to
  `spec.disks` after the VM was created, the VM was adopted, or an earlier
  release (rc1 and before) attached the disk and recorded it without a name.
  Such disks are never matched by position, because the list may have been
  edited since; their size is not changed by `Reconfigure` (no earlier release
  grew an additional disk either);
- **the name is ambiguous**: used by more than one `spec.disks` entry, or
  recorded on more than one disk;
- **vCenter cannot extend the disk**: a delta disk (the VM has snapshots, or it
  is a linked clone) or a physical-mode RDM. Removing the snapshots (or
  consolidating) makes the disk growable, but nothing re-sends the size by
  itself: the next `Reconfigure` — sent only on a CPU or memory change, see
  [below](#when-a-reconfigure-is-sent) — grows it.

The disks left alone in the last two cases (and a recorded disk missing from
the VM) are logged at `WARN` by the provider while the call succeeds; an entry
no disk is recorded for is logged at `INFO`.

A disk on a SCSI controller with a shared bus, or with multi-writer sharing, is
grown only while the VM is powered off (see the next section).

Before this release vSphere grew the **first** disk (the root disk) to
`spec.disks[0].sizeGiB`, and Proxmox sent every `spec.disks` size to `scsi0` and,
after a disk grow, dropped the CPU/memory change while reporting success.

### When a `Reconfigure` is sent

The manager sends a `Reconfigure` when the effective CPU or memory changes (or a
pending/failed one is re-checked); the disk sizes travel with it. **A disk size
change alone does not trigger one** (a known limitation): to apply a
`spec.disks` size change — or a growth that was skipped earlier, e.g. while the
disk had snapshots — the VM's CPU or memory must change too.

## vSphere: changes a running VM cannot take

vCenter changes a powered-on VM's CPU count only with **CPU hot-add** (more
vCPUs) or **CPU hot-remove** (fewer) enabled on the VM, and its memory only
upwards, with **memory hot-add** enabled and within the VM's **hot-plug memory
limit**. It grows a disk on a **shared SCSI bus** or with **multi-writer**
sharing only while the VM is off, and changes **no hardware of a suspended VM**.
vSphere has no pending state to stage anything for the next boot. So before a
change of a VM that is not powered off, the provider reads
`config.cpuHotAddEnabled`, `config.cpuHotRemoveEnabled`,
`config.memoryHotAddEnabled`, `config.hotPlugMemoryLimit` and the disk's
controller and sharing; when the change is not possible now it refuses the whole
request — everything in it — before changing anything, with
`RECONFIGURE_NEEDS_POWER_OFF` (`Reconfiguring=False/ResizePendingPowerOff`).
Before this release vCenter refused the task and the manager retried it on its
backoff indefinitely, with a vCenter task failure each time.

To apply such a change: set `spec.powerState: Off`; the manager powers the VM off
and the next `Reconfigure` applies the change; then set `spec.powerState: On`. The
manager never powers a VM off by itself. To resize running VMs in future, enable
CPU/memory hot-add on them (VMClass `performanceProfile.cpuHotAddEnabled` /
`memoryHotAddEnabled` for new VMs, or in vCenter while the VM is off).

A change that is possible online, and a disk grow with unchanged CPU/memory, goes
ahead on the running VM as before.

Two more refusals happen before anything is changed: a CPU count that is not a
multiple of the VM's cores per socket (`InvalidSpec`; vCenter would fail the
task), and a `spec.disks` shrink (above). A `ReconfigVM_Task` vCenter refuses to
start (a fault such as `TaskInProgress` during a backup snapshot,
`InvalidState`, `InvalidPowerState`) or fails anyway is answered with
`VM_OPERATION_FAILED` and a constant message (the fault is in the provider log;
a failure to reach vCenter, or an expired session, is not, and still counts
toward the breaker): it shows as `Reconfiguring=False/ProviderError`, is
retried on the per-VM backoff, and never counts toward the Provider's circuit
breaker, so one VM's failing reconfigure cannot fast-fail every other VM of the
Provider.

## Other providers

- **vSphere** has no pending state: a change is applied by one
  `ReconfigVM_Task`, refused before anything changes when the VM cannot take it
  now (above), or the call fails. It reports `restart_required: false`, and
  does not yet report `supports_honest_reconfigure`: a `spec.disks` entry it
  leaves alone (above) does not fail the call, and its responses carry no
  `honest_result` marker. `supports_reconfigure_online` stays `true` with the same
  per-VM meaning as libvirt's (online only for VMs with hot-add enabled).
- **Proxmox** reports `restart_required: false`, but a CPU/memory change PVE
  stores as *pending* (hot-plug not enabled for it) is currently reported as
  applied; it does not report `supports_honest_reconfigure`. Detecting the
  pending change (PVE's `pending` config) and reporting it as restart required
  is a follow-up change. A CPU/memory config update PVE fails is answered with
  `VM_OPERATION_FAILED` (kept out of the circuit breaker). Validated against the
  in-repo PVE fake only, not a real PVE.
