# Reconfigure results: applied, restart required, or failed

A `Reconfigure` changes a VirtualMachine's CPU, memory and disk size. Since this
release it has exactly three outcomes, and a provider never reports success for
a change it did not apply:

| Outcome | What the provider did | What the manager records |
|---|---|---|
| **Applied** | Applied every requested change to the running VM *and* to its persistent definition (or, for a VM that is off, to the definition it boots from). | `status.currentResources` = the new size; `Reconfiguring=False`, reason `ReconcileSuccess`. |
| **Restart required** | Applied at least one change to the persistent definition only, because the running VM cannot take it; every other change was applied live too. The change takes effect at the VM's next **power cycle** (power off, then on — a reboot from inside the guest keeps the running QEMU and is not enough). | `Reconfiguring=True`, reason `RestartRequired`; `status.currentResources` holds, per resource, the larger of the running and the next-boot size (see [the invariant](#statuscurrentresources-invariant)). |
| **Failed** | Could apply a change neither way, or could not tell (the host was unreachable). Part of the change may already be in the persistent definition, or even live. | `Reconfiguring=False`, reason `ProviderError`, with a message saying what could not be done. On a clustered Provider `status.currentResources` is recorded as for restart required (the larger size); on a single-host Provider it is untouched. The call is retried on a per-VM backoff. |

The result travels on the wire as `TaskResponse.restart_required` (an additive
field of the `Reconfigure` response, `proto/provider/v1/provider.proto`) and
reaches the manager as `contracts.ReconfigureResult.RestartRequired`. vSphere,
Proxmox and the mock provider report `false`; see
[Other providers](#other-providers).

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
A `Suspended` VM whose `spec.powerState` is `Off` (or `OffGraceful`) is powered
off (destroyed); an `Unknown` VM is left alone entirely — including one a
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
- A failed `Reconfigure` is sent again — on a per-VM backoff, 5 s doubling to
  5 min, and at once after a spec change — even if the spec is reverted to the
  recorded size, until one succeeds, so a partly-applied definition converges.
- On a clustered Provider, whenever `Describe` reports more vCPUs online
  (`DescribeResponse.vcpus`) than recorded, the recorded CPU is raised to them;
  it is never lowered by a report.

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
`Provider.status.reportedCapabilities.supportsHonestReconfigure`. The libvirt
provider reports it. On a **clustered** Provider that does not (an older
provider image), the manager sends no resize at all — grow or shrink — because
the committed-capacity accounting would trust a reply that may not be true: the
VM keeps its size with `Reconfiguring=False`, reason
`ProviderLacksHonestReconfigure`, re-checked every 30 s. A single-host Provider
without it is resized as before, and the manager logs a warning.

## Other providers

- **vSphere** has no pending state: a CPU/memory change is applied by one
  `ReconfigVM_Task` or the call fails. It reports `restart_required: false`, and
  does not yet report `supports_honest_reconfigure` (a smaller disk size is
  ignored silently).
- **Proxmox** reports `restart_required: false`, but a CPU/memory change PVE
  stores as *pending* (hot-plug not enabled for it) is currently reported as
  applied; it does not report `supports_honest_reconfigure`. Both are fixed by a
  follow-up change.
