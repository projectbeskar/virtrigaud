# ADR-0013: Disk resize as its own, honest flow

## Status

**Proposed (2026-10-23).** Nothing in this record is implemented. William Rizzo
reviews it before any slice starts. The questions he needs to answer are in
[Open questions](#open-questions).

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Tracking**: item R19 from the review of PR #401 (R5, "resize the right disk").

**Related**:
- PR #401 (R5). It stopped vSphere and Proxmox `Reconfigure` from resizing the
  root disk with a `spec.disks` size. It did this by limiting which disks
  `Reconfigure` touches, not by giving disks a stable identity.
- [`docs/reconfigure-results.md`](../reconfigure-results.md): the honest
  `Reconfigure` result contract (`supports_honest_reconfigure`), and the
  `RestartRequired`, `ResizePendingPowerOff` and `InsufficientHostCapacity`
  reasons.
- [ADR-0007](./0007-clustered-orchestrator-provider.md): the clustered provider.
  This ADR touches its committed-capacity resize gate and the B7.4 disk-space
  filter.
- [ADR-0011](./0011-migration-staging-cleanup.md): the model for a new RPC
  whose capability is negotiated and whose per-item outcome is an enum.

All line numbers refer to `origin/main` at `11e99bfe`.

## Context

### C1. A disk size change on its own never reaches the provider

- `needsReconfigure` compares only CPU and memory with
  `status.currentResources`
  (`internal/controller/virtualmachine_controller.go:2827-2845`). A
  `Reconfigure` is sent from two places, and both are gated by that check (or
  by the recheck of a pending or failed one):
  - the main path (`virtualmachine_controller.go:856-893`);
  - for a clustered VM observed powered off with a CPU/memory shrink pending,
    `applyPendingShrinkWhileOff` (called at `virtualmachine_controller.go:832`;
    its `needsReconfigure` gate and `reconfigureVM` call are at
    `virtualmachine_resize_gate.go:242-243` and `:268`).
- Disk sizes are packed into the `Reconfigure` request, but only as passengers:
  - the VMClass `diskDefaults.size`, rounded down to whole GiB, goes into
    `Class.DiskDefaults.SizeGiB`
    (`virtualmachine_controller.go:2367-2375`, `vmclass_quantity.go:74`);
  - each `spec.disks[].sizeGiB` goes into `Disks`
    (`virtualmachine_controller.go:2634-2642`).
- So a change to `spec.disks[].sizeGiB` alone, or to the VMClass
  `diskDefaults.size` alone, is silently ignored. The docs admit this as a known
  limitation
  ([`reconfigure-results.md` "When a `Reconfigure` is sent"](../reconfigure-results.md#when-a-reconfigure-is-sent)).
- The VirtualMachine controller watches VMClasses only for changes to their
  consumer selector (`internal/controller/consumergrant.go:397`). Any other
  class edit is picked up at the VM's next periodic requeue.
- `status.currentResources` holds CPU, memory and GPU only
  (`api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go:235-251`). Nothing
  in status records a disk.

### C2. What each provider does with a disk size today

| | Create | Reconfigure (only reached with a CPU/memory change) |
|---|---|---|
| **vSphere** | The root disk is grown to `diskDefaults.size` by `resizeVMDisk` (`internal/providers/vsphere/post_create.go:144-147`, `server.go:3477`). It picks the **first `VirtualDisk` in device order** (`server.go:3487-3494`), not the boot disk and not the lowest device key. It has no delta-disk check, and it silently does nothing when the target is not larger (`server.go:3510-3514`). `spec.disks` entries are attached as `<vm>-additional-<N>.vmdk` and recorded in ExtraConfig `virtrigaud.disk.additional.<N>` (the file) and `.<N>.name` (the entry name) (`server.go:3078-3283`, `post_create.go:67-110`). | The root disk is never touched: `reconfigureDesired` does not even parse `Class.DiskDefaults` (`reconfigure.go:117-130`). A `spec.disks` entry grows only the disk recorded under its name (`planDiskGrowth`, `reconfigure.go:522-575`). An entry with no record, an ambiguous name, a delta disk or a physical RDM is skipped and logged, and the call still succeeds (`reconfigure.go:541-560`). A shrink is refused with `InvalidSpec` (`reconfigure.go:561-564`). A shared-bus or multi-writer disk is grown only while the VM is off (`reconfigure.go:492-505`). The whole request, CPU and memory included, is refused with `RECONFIGURE_NEEDS_POWER_OFF` when any part of it cannot be applied online (`reconfigure.go:269-280`). |
| **libvirt** | The primary volume is created at `diskDefaults.size`, or at 20 GiB when the class sets none (`internal/providers/libvirt/provider_virsh.go:2184-2194`, `:321`). Its target is `vda` (`provider_virsh.go:2233`). **`spec.disks` is never attached**: `server.go:418-419` parses it and no code reads it. | The primary disk is grown first, and only from `Class.DiskDefaults.SizeGiB` (`reconfigure.go:281-289`). The size is a floor: grow-only, with a 1 MiB threshold (`disk_expand.go:37`, `:200-202`). A running domain is grown with `blockresize` and then a best-effort in-guest filesystem grow (`disk_expand.go:260-363`). A shut-off domain is grown with `vol-resize` (`reconfigure.go:454-481`, `storage.go:572-601`). The primary disk is found by position, as the first non-seed row of `domblklist` (`disk_expand.go:55-86`). On a clustered Provider it is the first `device=disk` row of `domblklist --details`, and a network disk fails (`disk_expand.go:117-154`). `spec.disks` sizes are ignored. No pool free space is checked. |
| **Proxmox** | `diskDefaults.size` is **not applied**: the class is parsed into CPU and memory only (`internal/providers/proxmox/server.go:1257-1270`). Create has three paths, and none sizes or attaches a disk from the request: <br>• an imported (migration) disk goes to `createFromImportedDisk` (`server.go:265-266`, `s3import.go:280`), always as `scsi0` (`s3import.go:255`); <br>• a VM without a template is created with `CreateVM` (`server.go:273-295`), whose config carries no disk keys (`pveapi/client.go:712-775`); <br>• a template clone keeps the template's disks unchanged (`server.go:297-415`, which covers the clone and `configureClonedVM`). <br>`spec.disks` is not attached. | No disk is resized. Each `spec.disks` size is logged and dropped (`reconfigure.go:74-81`). `pveapi.ResizeDisk` exists (`pveapi/client.go:1091-1121`) but has no caller. |
| **mock** | Ignores the class and the disks (`internal/providers/mock/provider.go:214-276`). | Never parses `desired_json` (`provider.go:423-461`). |

### C3. Capabilities overstate disk support

- `supports_disk_expansion_online` (`proto/provider/v1/provider.proto:889`) is
  reported `true` by:
  - vSphere (`internal/providers/vsphere/server.go:522`);
  - libvirt, single-host and clustered (`internal/providers/libvirt/server.go:1202`, `:1300`);
  - Proxmox (`internal/providers/proxmox/capabilities.go:32`);
  - mock (`internal/providers/mock/provider.go:124`).
- Proxmox and mock implement no disk resize at all, so their `true` is false.
- Nothing in the manager acts on the flag. It is only mirrored into
  `Provider.status.reportedCapabilities`
  (`internal/controller/provider_controller.go:430`).
- `supports_honest_reconfigure` is `true` only on libvirt
  (`libvirt/server.go:1237`, `:1314`). vSphere reports `false` for two
  reasons, both stated at `vsphere/server.go:531-535`: a `spec.disks` entry it
  finds no attached disk for is left alone while the call succeeds, and its
  responses carry no `honest_result` marker.

### C4. Disks have no stable identity

- **vSphere.** The additional-disk records key on the disk **file**, with a
  base-name fallback after a storage migration (`findRecordedDisk`,
  `reconfigure.go:431-448`; the base-name fallback is `:442-447`). The device key appears only in reports
  (`server.go:3683`, `:4442`). The VMDK backing UUID is used only by the
  disk-in-use guard (`disk_guard.go:442-458`). No code reads
  `config.bootOptions.bootOrder` (the only `BootOptions` use is secure boot,
  `server.go:2773-2775`).
- **libvirt.** The "primary" disk is chosen by position (C2).
- **Proxmox.** The root disk depends on the template. `DetectPrimaryDisk`
  scans virtio, then scsi, sata and ide (`pveapi/client.go:1384-1410`), and its
  result is used only for the boot order.

### C5. The manager cannot see what is applied

- `DescribeResponse` carries no disks (`provider.proto:256-281`), and neither
  does the contract type (`internal/providers/contracts/provider.go:110-130`).
- `GetDiskInfo` lists every disk with its virtual size (`provider.proto:715-742`).
  The manager uses it only for the disk-size limit check on clones and
  migrations (`internal/controller/disk_size_limit_copies.go:103`, `:166`).
  Its disk ids differ by provider: the device key on vSphere
  (`vsphere/server.go:3683`), the host path on libvirt
  (`libvirt/disk_capacity.go:53-67`), and the bus key on Proxmox
  (`proxmox/server.go:1715-1753`). None of them says which disk is the root or
  which `spec.disks` entry a disk belongs to.
- `ListVMs` disk sizes are unreliable:
  - libvirt always reports 0 (`libvirt/domainxml.go:108`);
  - Proxmox reads them from the wrong part of the volume id
    (`proxmox/server.go:2341-2373`);
  - mock hardcodes 20 GiB (`mock/provider.go:507-514`).

### C6. Clustered capacity has no disk dimension

- The resize gate admits a CPU/memory grow against the host's free capacity
  (`internal/controller/virtualmachine_resize_gate.go:89-177`).
  `ResourceRequest` is CPU and memory only (`internal/scheduler/scheduler.go:111-116`).
- Storage is a **filter**, not committed capacity. The B7.4 check rejects a
  host whose `status.allocatableStorageBytes` is missing or below
  `RequiredStorageBytes` (`internal/scheduler/filter.go:143-150`;
  `api/infra.virtrigaud.io/v1beta1/host_types.go:144-149`).
- `RequiredStorageBytes` is non-zero only for a migration target
  (`internal/controller/virtualmachine_import_wait.go:243-249`). Ordinary VMs'
  disks are not counted, and nothing reduces the free figure for disks other
  VMs have pending (`scheduler.go:219-225`).

### C7. Related gaps found while reading

- **`DiskSpec.expandPolicy` is unused.** It is documented as "how the disk can
  be expanded", with enum `Online|Offline` and default `Offline`
  (`virtualmachine_types.go:620-624`). No Go code outside the API package
  reads it.
- **libvirt's single-host online grow resizes a volume of a running domain
  outside QEMU** with `vol-resize` before `blockresize`, and only logs a
  failure (`disk_expand.go:309-311`). Two things are wrong with it:
  - It targets the volume named by convention (`vmDiskVolumeName(id)` in the
    `default` pool), not necessarily the domain's actual disk.
  - QEMU's image locking makes it fail harmlessly only for a qcow2 image,
    which libvirt resizes with `qemu-img`. A raw volume is resized by libvirt
    directly, which no image lock guards.

  The new flow must never resize the image of a running domain outside QEMU
  itself.
- **vSphere memory (memory side, noted because it interacts with disks).**
  - `hotPlugMemoryIncrementSize` is never read. A memory grow that is not a
    multiple of it is refused by vCenter's task, which shows up as
    `ProviderError`, not `ResizePendingPowerOff` (`reconfigure.go:287-301`).
  - Linux guests with less than 3 GB that cross the 3 GB boundary on hot-add
    are not modelled.
  - The interaction with disks: one vSphere `ReconfigVM_Task` carries CPU,
    memory and disks, so a memory change the VM cannot take online blocks a
    disk grow that could go ahead (`reconfigure.go:272`). Separating the flows
    (D1) removes this coupling. The memory gaps are follow-ups, not part of
    this ADR.
- **The PVE fake** accepts a shrink and any size string
  (`internal/providers/proxmox/pvefake/server.go:1529-1592`). It is not a
  faithful oracle for resize tests.

### What #401 did, and why it is not enough

`docs/upgrading.md:38` (vSphere and Proxmox `Reconfigure` no longer resize the
root disk) and `docs/upgrading.md:954-958` (rollback brings the wrong-disk
resize back) record R5's fix: never touch a disk without a name record.

That is safe, but it leaves several things silent:
- disks without a record;
- the root disk on vSphere and Proxmox;
- every disk-only change.

The honest-result contract (`docs/upgrading.md:79-80`) covers CPU and memory,
and libvirt's root-disk grow inside `Reconfigure`. That grow runs first, and a
failure fails the whole call (`libvirt/reconfigure.go:281-289`;
`docs/upgrading.md:79`). The contract cannot cover a disk-only change, which is
never sent, and the manager has no observed disk size to check any result
against.

## Options considered

### Option A: extend `needsReconfigure` to compare disk sizes, and keep disks inside `Reconfigure`

This is the smallest change. The manager would also send a `Reconfigure` when
a disk size differs from the spec.

- **Against: there is nothing to compare with.** Status has no observed disk
  size (C5). The manager would have to record "sent" sizes, which claims more
  than the provider confirmed. That breaks the "Status reflects reality"
  rule.
- **Against: coupling.** One untyped `desired_json` carries everything. One
  refusal blocks everything (C7, vSphere). One `honest_result` marker vouches
  for disks that a provider may skip (C3, vSphere).
- **Against: no safe fallback.** An old provider that ignores disks (Proxmox
  today) looks the same as one that applied them.

### Option B: a typed `ResizeDisks` RPC, and per-disk observation in `Describe` (**recommended**)

- `Describe` reports every disk of the VM, each with:
  - a stable identity;
  - a role: root, an additional disk with its `spec.disks` name, or unmanaged;
  - its capacity;
  - any reason it cannot be grown.
- The manager writes these into a new `status.disks`. It decides a resize by
  comparing the desired sizes with that **observed** state.
- It then sends a separate, typed `ResizeDisks` call. Each disk gets an
  outcome from an enum.
- A capability flag gates the call. A provider that predates it answers
  `Unimplemented`, which maps to a condition, never to silence.

### Option C: keep `Reconfigure`, but add typed disk fields to `ReconfigureRequest` and `TaskResponse`

This middle ground still couples disk refusals with CPU/memory refusals
inside one hypervisor operation. It also overloads `TaskResponse`, which every
RPC uses. It saves one RPC and costs clarity. Not chosen.

### Option D: do nothing beyond documentation

The current state is documented
([`reconfigure-results.md`](../reconfigure-results.md#which-disk-a-size-goes-to)).
But a tenant who edits `spec.disks[].sizeGiB` still gets no signal on the
object. That is the defect R19 is about. Not chosen.

## Decision

### D1. Goals and non-goals

**Goals**

1. A desired disk size (VMClass `diskDefaults.size` for the root disk,
   `spec.disks[].sizeGiB` for an additional disk) that differs from the
   **observed** size either reaches the provider on its own, or the VM says
   why it cannot. It is never silently dropped.
2. Grow only. A desired size below the observed size is refused by the
   manager with a condition and never sent. Providers refuse a shrink too, as
   defence in depth.
3. Status shows the per-disk sizes the provider reported, and nothing it did
   not report.
4. Every provider identifies a disk by the most stable identity its
   hypervisor offers (D4): the vSphere backing UUID, the libvirt target dev,
   the Proxmox bus key. Where none exists yet, the fallbacks are documented
   and never silent:
   - the root disk is found by boot order, else by position (the lowest
     vSphere device key, the first libvirt `device='disk'` entry);
   - a vSphere additional disk is found by its recorded file until its UUID
     record is written (D4).
   A disk that no rule identifies is reported `Unmanaged` and never resized.
5. On a clustered Provider, a grow is admitted against the host's storage
   (D6).

**Non-goals**

- Shrinking a disk.
- Adding or removing disks on an existing VM: hot-attach, or attaching a
  `spec.disks` entry added after creation.
- Changing a disk's type, storage class or datastore.
- Making libvirt and Proxmox `Create` attach `spec.disks` (C2). That is a
  separate item. Until it ships, this flow reports each such entry as
  `DiskNotFound` (D3) rather than ignoring it.
- **Guest filesystem growth.** "Applied" means the virtual block device has
  the new capacity, as the hypervisor reports it. Growing the partition and
  filesystem is the guest's job (cloud-init `growpart` does it at boot on most
  cloud images). libvirt's existing best-effort in-guest grow
  (`disk_expand.go:345-363`) is kept for the root disk. Its outcome is
  reported per disk as informational and never decides "applied". See Q5.
- Committed storage accounting on clustered hosts (D6, Q4).
- The vSphere memory gaps in C7.

### D2. API (CRD): additive only, v1beta1 stays compatible

New status field: `VirtualMachineStatus.Disks []VirtualMachineDiskStatus`.
Marked `+listType=map`, `+listMapKey=id`, `+kubebuilder:validation:MaxItems=32`.

```go
// VirtualMachineDiskStatus is one disk of the VM as its provider last
// reported it in Describe. The operator writes it only from a provider
// report; it never records a size the provider did not report.
type VirtualMachineDiskStatus struct {
    // ID is the provider's stable identity of the disk within this VM
    // (vSphere: the virtual disk's backing UUID; libvirt: the target dev;
    // Proxmox: the bus key such as scsi0). Opaque, and never a host or
    // datastore path.
    // +kubebuilder:validation:MaxLength=128
    ID string `json:"id"`
    // Role is Root, Additional (created for a spec.disks entry) or Unmanaged
    // (any other disk, e.g. a template's second disk; never resized).
    // +kubebuilder:validation:Enum=Root;Additional;Unmanaged
    Role DiskRole `json:"role"`
    // Name is the spec.disks entry name of an Additional disk.
    // +optional
    // +kubebuilder:validation:MaxLength=63
    Name string `json:"name,omitempty"`
    // CapacityBytes is the virtual size the provider reported. Unset when
    // the provider could not read it.
    // +optional
    // +kubebuilder:validation:Minimum=0
    CapacityBytes *int64 `json:"capacityBytes,omitempty"`
    // NotResizableReason says why the disk cannot be grown at all right now
    // (Snapshots, RawDeviceMapping, NetworkDisk, UnsupportedBacking). Empty
    // when the provider knows of no reason.
    // +optional
    NotResizableReason string `json:"notResizableReason,omitempty"`
}
```

**New condition type `DiskSizeApplied`.** It is set only when the VM asks for
at least one disk size.

| Status / reason | Meaning |
|---|---|
| `True` / `DiskSizesApplied` | Every desired size is at or below the observed capacity (1 MiB tolerance, matching libvirt's grow threshold, `disk_expand.go:37`). |
| `False` / `DiskResizing` | A `ResizeDisks` call or its task is in flight. |
| `False` / `DiskResizePendingPowerOff` | At least one disk can be grown only while the VM is off, and that disk was not changed. Other disks in the same call may have been grown (D7); their sizes are in `status.disks`. Retried on the backoff, and at once after a spec change, like `ResizePendingPowerOff`. |
| `False` / `DiskShrinkRefused` | A desired size is below the observed one. Nothing is sent. |
| `False` / `DiskNotResizable` | The disk reports a `NotResizableReason` (for example, it has snapshots). |
| `False` / `DiskNotFound` | No observed disk has the entry's role and name: added after creation, adopted, a pre-#401 vSphere record without a name, or a provider whose `Create` attaches no `spec.disks`. |
| `False` / `ProviderLacksDiskResize` | The disk is observed smaller than desired, but the Provider does not report `supportsDiskResize`. The capability field exists from Slice 1, so this reason is accurate before any provider implements `ResizeDisks` (D11). |
| `False` / `InsufficientHostStorage` | Clustered only: the grow does not fit the host's reported free storage (D6). |
| `False` / `ProviderError` | The call failed. Retried on a per-VM backoff (5 s doubling to 5 min). |
| `Unknown` / `DisksNotReported` | The provider does not report disks (no `disks_reported` marker), so the manager cannot tell. |

**What stays the same, and how the new pieces fit in:**

- The `DiskSizeWithinLimit` condition (`internal/k8s/conditions.go:31-39`;
  its reasons at `:214-234`) keeps reporting a growth that the
  administrator's limit withholds.
- `status.phase` is `Reconfiguring` while `DiskSizeApplied=False/DiskResizing`
  (Q6).
- The messages are constant and never carry a path.
- **The new capability is plumbed like every other one.**
  - A `SupportsDiskResize` field in `Provider.status.reportedCapabilities`
    (`ReportedCapabilities`, `api/infra.virtrigaud.io/v1beta1/provider_types.go:476`;
    compare `SupportsHonestReconfigure` at `:539-547`), mirrored by the
    Provider controller (`internal/controller/provider_controller.go:450`).
  - A field in `contracts.Capabilities`
    (`internal/providers/contracts/capabilities.go:76-83`).
  - The wire mapping in the transport client
    (`internal/transport/grpc/client.go:525-544`).
  - A capability constant and `Builder` method in the SDK, as for
    `CapabilityHonestReconfigure`
    (`sdk/provider/capabilities/capabilities.go:69`, `:288`, `:468`).

**Rejected alternatives:**

- A `rootDiskGiB` field in `VirtualMachineResources`. That type is both the
  spec override and `status.currentResources`, so the field would mix a
  desired value with a recorded one, and it would not cover additional disks.
- A CEL rule that refuses a shrink at admission. `spec.disks` is an atomic
  list (`virtualmachine_types.go:83-86`), and a transition rule needs
  `listType=map` to correlate entries. Switching the list type would refuse
  updates of stored objects that have duplicate names, which is a breaking
  tightening. The controller refuses instead (`DiskShrinkRefused`).

A per-VM root-disk size override is deliberately left open (Q3).

### D3. Proto: additive fields, one new RPC, one capability

```proto
// One disk of a VM, as Describe reports it.
message VMDisk {
  string id = 1;              // stable identity within the VM; opaque; never a path
  DiskRole role = 2;
  string spec_name = 3;       // the spec.disks name of an ADDITIONAL disk
  int64 capacity_bytes = 4;   // virtual size; 0 = not read
  DiskResizeBlocker blocker = 5;
}
enum DiskRole { DISK_ROLE_UNSPECIFIED = 0; DISK_ROLE_ROOT = 1; DISK_ROLE_ADDITIONAL = 2; DISK_ROLE_UNMANAGED = 3; }
enum DiskResizeBlocker {
  DISK_RESIZE_BLOCKER_UNSPECIFIED = 0;   // none known
  DISK_RESIZE_BLOCKER_SNAPSHOTS = 1;     // delta disk: snapshots or a linked clone
  DISK_RESIZE_BLOCKER_RAW_DEVICE_MAPPING = 2;
  DISK_RESIZE_BLOCKER_NETWORK_DISK = 3;  // no host path the provider may resize
  DISK_RESIZE_BLOCKER_UNSUPPORTED_BACKING = 4;
}

// DescribeResponse, additive (numbers assigned at implementation time):
repeated VMDisk disks = <next free>;
bool disks_reported = <next free>;   // per-response proof; false from a provider that predates it

// GetCapabilitiesResponse, additive:
bool supports_disk_resize = <next free>;

rpc ResizeDisks(ResizeDisksRequest) returns (ResizeDisksResponse);

message ResizeDisksRequest {
  string id = 1;
  string target_host_id = 2;      // as every routed per-VM call
  ObjectIdentity owner = 3;       // owner-checked as Reconfigure
  repeated DiskGrow disks = 4;
}
message DiskGrow {
  string disk_id = 1;             // a VMDisk.id from Describe
  int64 capacity_bytes = 2;       // absolute target; grow-only
  bool allow_online = 3;          // false: grow only while the VM is powered off
}
message ResizeDisksResponse {
  repeated DiskResizeResult results = 1;  // one per requested disk
  TaskRef task = 2;                       // set only when the hypervisor task outlives the call
}
message DiskResizeResult {
  string disk_id = 1;
  DiskResizeOutcome outcome = 2;
  int64 capacity_bytes = 3;       // observed after the call; 0 = not read
  GuestFilesystemOutcome guest_filesystem = 4;  // informational only
}
enum DiskResizeOutcome {
  DISK_RESIZE_OUTCOME_UNSPECIFIED = 0;
  DISK_RESIZE_OUTCOME_APPLIED = 1;
  DISK_RESIZE_OUTCOME_ALREADY_AT_SIZE = 2;
  DISK_RESIZE_OUTCOME_NEEDS_POWER_OFF = 3;
  DISK_RESIZE_OUTCOME_NOT_RESIZABLE = 4;
  DISK_RESIZE_OUTCOME_SHRINK_REFUSED = 5;
  DISK_RESIZE_OUTCOME_INSUFFICIENT_SPACE = 6;
  DISK_RESIZE_OUTCOME_NOT_FOUND = 7;
  DISK_RESIZE_OUTCOME_FAILED = 8;
}
enum GuestFilesystemOutcome { GUEST_FILESYSTEM_OUTCOME_NOT_ATTEMPTED = 0; GUEST_FILESYSTEM_OUTCOME_GROWN = 1; GUEST_FILESYSTEM_OUTCOME_FAILED = 2; }
```

**Field numbers** are placeholders, assigned as the next free numbers when
each slice is implemented. Today `DescribeResponse` uses 1-7 and
`GetCapabilitiesResponse` 1-30. ADR-0012 (Accepted) already plans
`DescribeResponse.owner_check` and a `supports_owner_stamp_required`
capability, also as next free numbers
([ADR-0012 D1](./0012-proxmox-legacy-unstamped-name-rule.md#d1-wire-contract-additive));
whichever lands first takes the lower numbers.

**How the RPC behaves:**

- **Errors.** Per-disk outcomes travel in the response. Only call-level facts
  are gRPC errors:
  - routing (`HOST_UNAVAILABLE`);
  - ownership (`VM_NOT_OWNED`, `OWNER_STAMP_UNREADABLE`);
  - a provider that cannot reach the hypervisor.

  A `FAILED` disk is not a provider failure and never counts toward the
  circuit breaker.
- **Synchronous by default.** The manager gives the call a 5-minute deadline,
  like `ReconfigureCallTimeout`
  (`internal/providers/contracts/deadlines.go:26`). The provider waits for the
  hypervisor task within its own budget, kept a margin below that deadline so
  its answer arrives before the manager gives up. It returns `task` only when
  the hypervisor task outlives that budget, for example an eager-zeroed thick
  VMDK. The manager then treats completion as "observe again", never as
  "applied". Because the decision is made from observation, an async result
  needs no extra trust.
- **Providers that predate the RPC.** The manager sends `ResizeDisks` only to
  a Provider that reports `supportsDiskResize`. An `Unimplemented` answer
  (from a provider image rolled back behind a stale capability snapshot) is
  recorded as `ProviderLacksDiskResize`. It is not an error.
- **Why `disks_reported`.** An empty `disks` list from an old provider looks
  the same as a VM without disks. This is the same reason
  `honest_result` exists (`provider.proto:203-218`).
- **`Reconfigure` stops carrying disks.** When the Provider reports
  `supportsDiskResize`, the manager sends `Reconfigure` with
  `Class.DiskDefaults.SizeGiB` and every `Disks[].SizeGiB` set to 0. Every
  provider already reads 0 as "leave this disk as it is"
  (`internal/controller/disk_size_limit.go:217-221`). To a Provider without
  the capability, `Reconfigure` keeps carrying them exactly as today. The disk
  code paths stay in the providers' `Reconfigure` for older managers until a
  later release removes them (Slice 7).
- **Fixing the existing capability.** `supports_disk_expansion_online` is
  corrected: Proxmox and mock stop reporting it until they implement a resize
  (Slice 1).
- **The capability arrives before the RPC.** `supports_disk_resize` is added
  in Slice 1, and no provider reports it until its `ResizeDisks` ships (Slice
  3 onwards). So in Slices 1-2 every disk observed smaller than desired is
  `ProviderLacksDiskResize`, which is true at that point.

### D4. Stable identity, per provider

**vSphere**
- **Id.** The disk's backing UUID (`backing.uuid`, present on FlatVer2,
  SeSparse, SparseVer2 and RDM backings). It survives storage vMotion and file
  renames. A disk without one is reported as `key:<deviceKey>` and is
  `Unmanaged`.
- **Root.** The first `VirtualMachineBootOptionsBootableDiskDevice` in
  `config.bootOptions.bootOrder`. When there is none, the disk with the lowest
  device key.
  - `Create` records it in ExtraConfig as `virtrigaud.disk.root.uuid`, so it
    is never re-guessed.
  - `Create`'s `resizeVMDisk` is changed to use the same rule (today: the
    first disk in device order, `server.go:3487-3494`).
- **Additional disks.**
  - The existing `virtrigaud.disk.additional.<N>` / `.name` records are
    resolved at read time with the existing `findRecordedDisk` logic. A
    resolved disk is reported with its UUID and `spec_name`.
  - The first `ResizeDisks` that touches a disk also writes
    `virtrigaud.disk.additional.<N>.uuid`, in the same `ReconfigVM_Task` as
    the extend. Describe never writes.
  - A record without a name stays `Unmanaged`, as #401 decided. The VM shows
    `DiskNotFound` for its entry.

**libvirt (single-host and clustered)**
- **Id.** The target dev (`vda`). It is stable in the domain definition. The
  source path is resolved by the provider at call time and never leaves it.
- **Root.** The disk with `<boot order='1'>`. When there is none, the first
  `device='disk'` entry, which is the current rule (`disk_expand.go:55-86`,
  `:117-142`). Domains created by VirtRigaud always have `vda`
  (`provider_virsh.go:2233`).
- **Additional disks.** None until libvirt `Create` attaches `spec.disks`
  (non-goal). Any other disk is `Unmanaged`.

**Proxmox**
- **Id.** The bus key (`scsi0`, `virtio1`), validated with the existing
  `pveBusDiskKey` (`proxmox/server.go:1718`).
- **Root.** The first disk in the `boot: order=` list. When there is none,
  `DetectPrimaryDisk` (`pveapi/client.go:1384-1410`). The imported-disk path
  always uses `scsi0` (`s3import.go:255`).
- **Additional disks.** None until Proxmox `Create` attaches `spec.disks`.

**Migrating from today's records needs no data migration.** Identity is
resolved when the disk is read, from what the hypervisor already holds. Only
the vSphere UUID record is written lazily, as part of a resize.

### D5. Controller flow

The disk step is a separate step of `reconcileVM`, after the CPU/memory step
(`virtualmachine_controller.go:853-893`):

1. **Observe.**
   - On every `Describe` with `disks_reported`, replace `status.disks` with
     the report.
   - Without the marker, leave `status.disks` as it is and set
     `DiskSizeApplied=Unknown/DisksNotReported`.
   - The status write happens even when nothing else changed, so an
     out-of-band resize (for example, in vCenter) shows up.
2. **Compute the desired sizes.**
   - Root: `diskDefaults.size`, rounded down to whole GiB as `Create` sends it
     (`vmclass_quantity.go:74`), in bytes.
   - Each `spec.disks` entry: `sizeGiB × 2^30`.
   - Q1 decides whether the root size is applied to existing VMs at all.
3. **Classify each desired disk against the observed one.** The possible
   results are applied, `DiskNotFound`, `DiskShrinkRefused`,
   `DiskNotResizable`, or "to grow".
   - The disk-size limit (`withholdDiskGrowthOverLimit`,
     `disk_size_limit.go:224`) removes over-limit grows, as today.
4. **Order with CPU/memory.**
   - A `Reconfigure` that is due is sent first, and that pass returns, as
     today. That covers both send sites: the main path
     (`virtualmachine_controller.go:893`) and, for a clustered VM observed
     powered off, `applyPendingShrinkWhileOff`
     (`virtualmachine_controller.go:832`, `virtualmachine_resize_gate.go:268`),
     which returns before the disk step is reached.
   - The disk step runs in a pass that sends no `Reconfigure`. That includes a
     pass where the CPU/memory change is held: `ResizePendingPowerOff`,
     `ShrinkPendingPowerOff`, `InsufficientHostCapacity`, or a backoff wait.
     So a held CPU/memory change never blocks a disk grow, and the reverse is
     also true.
   - The disk step never runs while `status.reconfigureTaskRef` is set, so a
     VM never has two hypervisor operations in flight.
5. **Gates.**
   - Skip when the Provider does not report `supportsDiskResize`
     (`ProviderLacksDiskResize`).
   - Skip when the VM is `Suspended` or `Unknown` (already
     `PowerStateUnmanaged`).
   - Clustered: admit the grow (D6).
6. **Call.** One `ResizeDisks` carries every disk to grow. The root disk has
   no policy and always gets `allow_online=true`. For a `spec.disks` entry,
   `allow_online` depends on William's answer to Q2:
   - **If `expandPolicy` is honoured:** `false` when it says `Offline`. Its
     CRD default is `Offline`, so every additional-disk grow on a running VM
     then waits for a power-off (`DiskResizePendingPowerOff`). That is a
     regression from vSphere's online grow today, and needs an upgrade note.
   - **If it is not honoured** (kept a no-op, documented as such): always
     `true`, and the provider decides what it can do online, as today.
7. **Record.**
   - Write each result's `capacity_bytes` into `status.disks`: it is a
     provider report.
   - Set the condition from the worst outcome.
   - Retry on a disk-specific per-VM backoff (a separate
     `unschedulableBackoff`, like `reconfigureRetry`,
     `virtualmachine_controller.go:284-287`), and at once after a spec change.

**Idempotency.** The request carries absolute target sizes. A provider
answers `ALREADY_AT_SIZE` for a disk at or above the target. A repeated or
lost call is therefore harmless.

### D6. Clustered libvirt: storage admission for a grow

- **The check.** Before `ResizeDisks`, a routed VM's grow is checked as a
  filter:
  - Δ = the sum of the grows (desired − observed);
  - Δ must fit the bound host's `status.allocatableStorageBytes`;
  - a missing value fails closed, as B7.4 does (`filter.go:143-150`).
  - The figure measures the host's `default` pool directory
    (`host_types.go:144-149`). A VM whose disk lives elsewhere (an LV or
    other block device) is checked against a figure that does not describe
    its storage. The check is still applied, as a coarse guard, and the
    provider's own free-space check (D7) is the one that sees the real
    volume.
- **On refusal.** Nothing is sent, and the VM shows
  `DiskSizeApplied=False/InsufficientHostStorage`.
- **What it does not do.** It does not reserve storage. qcow2 grows are thin
  (metadata only), so the check is conservative for those and the only guard
  for raw images. Committed storage accounting (a storage dimension in
  `ResourceRequest`, `scheduler.go:111-116`) is a separate ADR-0007 amendment
  (Q4).
- **Provider side.** The routed `ResizeDisks` follows the `Reconfigure`
  routing: `withHostConn`, then the `ownedDomainTarget` owner check
  (`provider_virsh.go:1704-1714`, `routing.go:80-103`). A network disk is
  `NOT_RESIZABLE/NETWORK_DISK` (today it is a generic failure,
  `disk_expand.go:134`).

### D7. How each provider applies `ResizeDisks`

**vSphere**
- One `ReconfigVM_Task` edits each disk, found by UUID, with
  `CapacityInBytes`/`CapacityInKB` (today's `grownDisk`,
  `reconfigure.go:510`).
- A delta disk is `NOT_RESIZABLE/SNAPSHOTS`, and a physical RDM is
  `RAW_DEVICE_MAPPING` (today's `diskNotExtendable`, `reconfigure.go:452-476`).
- A shared-bus or multi-writer disk on a running VM, a suspended VM, or
  `allow_online=false` on a running VM gives `NEEDS_POWER_OFF`
  (`reconfigure.go:492-505`). A disk that needs power-off does not hold back
  the other disks in the same call.
- A task fault is `FAILED` with a constant message.
- Reports `supports_disk_resize`. `supports_honest_reconfigure` stays a
  separate decision. It can follow once both reasons it is `false` today
  (`server.go:531-535`) are gone: disks no longer silently skipped (the
  `Reconfigure` disk path is retired, Slice 7), and every `Reconfigure`
  response marked with `honest_result` (C3).

**libvirt**
- **Running domain.** Grow with `blockresize` (QEMU grows the image and the
  guest sees the new size).
  - The provider **never** runs `vol-resize`/`qemu-img` on the image of a
    running domain. This removes the call at `disk_expand.go:309-311`.
  - A block-device disk on a clustered host keeps the explicit
    `ResizeVolumeByPath` before `blockresize` (an LV must grow first). It is
    `FAILED` if that step fails, instead of being only logged.
- **Shut-off domain.** Grow with `vol-resize`, as today
  (`reconfigure.go:454-481`).
- **Free space.** For a non-thin (raw) volume, the pool's free space is
  checked first; too little gives `INSUFFICIENT_SPACE`.
- **Observed size.** Read for all disks in one call. On the single-host virsh
  path that is `virsh domblkinfo --all` (one SSH exec), which works for
  inactive domains on libvirt 4.5 or newer (to verify in the lab). On the
  go-libvirt path it is `DomainGetBlockInfo` per target.
- **Guest filesystem.** The in-guest filesystem grow stays best-effort, root
  disk only, and is reported in `guest_filesystem` (Q5).

**Proxmox**
- `PUT /nodes/{node}/qemu/{vmid}/resize` with an **absolute** size, which
  makes it idempotent (`pveapi.ResizeDisk`, `pveapi/client.go:1091-1121`).
  The provider waits for the returned UPID within the deadline.
- PVE grows a running VM's disk live through QEMU.
- Owner-checked as `Reconfigure` (`proxmox/server.go:666`;
  `vm_owner_check.go:186-216`, with a new op constant).
- **`Create` applies `diskDefaults.size`** to the root disk after the clone,
  grow-only, and waits for the task. This is a visible behaviour change (Q7).
- The PVE fake is made grow-only, and its fixed `scsi0` override
  (`pvefake/server.go:1406`, `:1425`) is fixed, so tests mean something.

**mock** implements the RPC in memory, with observable per-disk sizes, for
controller and conformance tests.

### D8. Upgrade, rollback and compatibility

| Manager | Provider | Behaviour |
|---|---|---|
| new | old (no `disks_reported`, no capability) | `DiskSizeApplied=Unknown/DisksNotReported`. Disk sizes still ride along with a CPU/memory `Reconfigure` exactly as today. Never silent: the condition is on the object. |
| new | new | The new flow. `Reconfigure` sends disk sizes as 0. |
| old | new | Unknown proto fields are ignored. `ResizeDisks` is never called. The provider's `Reconfigure` keeps its current disk behaviour for this manager. |
| new | rolled back behind a stale capability | `ResizeDisks` gets `Unimplemented`, so `ProviderLacksDiskResize` (no breaker count). `Describe` without the marker leaves `status.disks` as it is, with `Unknown`. |

- `Provider.status.reportedCapabilities.supportsDiskResize` (D2) shows
  operators which Providers can resize. It is a snapshot from the last
  successful `GetCapabilities`, which is why an `Unimplemented` answer is
  still handled (last row).
- The CRD change is new optional status fields (`status.disks` on the
  VirtualMachine, `supportsDiskResize` on the Provider) and a new condition
  type.
  Rolling the CRD back drops `status.disks` (pruned). No stored object
  becomes invalid.
- **Rollout order:** CRDs, then manager, then providers. This is the existing
  order (`docs/release-notes/v0.4.0.md`).

### D9. Security

- **No tenant path control.**
  - `disk_id` is opaque. Each provider resolves it only among the devices of
    the owner-checked VM, and validates its shape: a UUID or the vSphere
    fallback `key:<deviceKey>` (an integer device key, D4); a target dev
    pattern; or `pveBusDiskKey`.
  - A path is never accepted from the wire.
  - `status.disks` carries no host or datastore path. `GetDiskInfo` paths are
    never copied into status.
- **Status is trusted.** `status.disks` is written only by the operator,
  through the status subresource. ADR-0011 D11 applies.
- **Messages are constant.** Results and conditions never carry hypervisor
  output, paths or endpoints, matching `reconfigure-results.md`.
- **Storage exhaustion.**
  - Bounded by the spec maximum (65536 GiB, `virtualmachine_types.go:609-612`)
    and the administrator's disk-size limit, which still applies to every
    grow.
  - On clustered Providers, also by the D6 check.
  - Per-namespace storage quota is out of scope.
- **Guest commands.** libvirt's in-guest filesystem grow runs commands inside
  the tenant's guest through the agent. Keeping it on by default is Q5.

### D10. Testing and lab plan

**Unit tests**
- Controller: classification, ordering with CPU/memory holds, backoff, the
  stale capability, and status written only from reports (envtest for the
  status and CRD schema).
- vSphere against vcsim: UUID identity, boot-order root, delta and RDM disks,
  shared bus.
- libvirt fakes: live vs offline, network disk, `domblkinfo --all` parsing,
  and that no `vol-resize` runs on a running domain.
- Proxmox against the corrected fake: absolute size, UPID wait, owner check,
  `Create` applying the class size.

**Conformance (`test/conformance/`)**
- `ResizeDisks` is idempotent (`ALREADY_AT_SIZE`), refuses a shrink, and a
  `NOT_FOUND` id changes nothing.

**Lab** (only with William's go-ahead; the lab is shared)
- **vSphere:** online grow of an additional disk, the root disk via a class
  change, the same with a snapshot present (`DiskNotResizable`), and a
  shared bus (`DiskResizePendingPowerOff`).
- **libvirt single-host and a clustered Provider on a shared pool:**
  - a running grow via `blockresize`;
  - a shut-off grow via `vol-resize`;
  - `domblkinfo --all` on a shut-off domain;
  - `InsufficientHostStorage` with a lowered figure.
- **Proxmox (a lab PVE cluster, API token):** root grow on a running VM, and a
  `Create` with a class size larger than the template's disk.

### D11. Slices (smallest first; each one a PR)

1. **Observe and tell (contract, manager, mock).**
   - Proto: `Describe.disks` and `disks_reported`, and the
     `supports_disk_resize` capability field (reported by no provider yet).
   - CRD: `status.disks`, and `supportsDiskResize` in
     `status.reportedCapabilities`, with the contracts, transport and SDK
     plumbing (D2).
   - Manager: writes `status.disks`, and the `DiskSizeApplied` condition with
     `DisksNotReported`, `DiskShrinkRefused`, `DiskNotFound` and
     `ProviderLacksDiskResize`.
   - Mock reports disks. Proxmox and mock stop advertising
     `supports_disk_expansion_online`.

   After this slice, every VM with a disk size says whether it is applied, or
   that the manager cannot tell. Nothing is resized yet.
2. **Providers report disks.** One PR per provider:
   - libvirt (single-host and clustered);
   - vSphere (UUID, boot-order root);
   - Proxmox (bus key, boot order).
3. **`ResizeDisks`, single-host.**
   - The RPC, the manager step (D5), and zero disk sizes in `Reconfigure` for
     providers that report `supports_disk_resize`.
   - Needs William's answer to Q2 first (D5 step 6).
   - Implemented in mock and single-host libvirt, reusing today's grow code
     and removing the running-image `vol-resize`.
4. **vSphere `ResizeDisks`.** Includes the root disk, the `Create` root-rule
   alignment and lazy UUID records. Then reconsider
   `supports_honest_reconfigure`.
5. **Proxmox `ResizeDisks`.** Includes `Create` applying `diskDefaults.size`
   and a faithful PVE fake.
6. **Clustered libvirt.** Routed `ResizeDisks` and the D6 storage admission.
7. **Cleanup** (a later release):
   - remove the disk paths from the providers' `Reconfigure`;
   - consolidate `reconfigure-results.md` into a disk-resize page.

## Consequences

**Positive**
- A disk size is either applied or the object says why, for every provider
  and every version mix.
- CPU/memory and disk changes stop blocking each other.
- Status reports observed per-disk sizes, which tenants, the clustered check
  and future storage accounting can rely on.
- Proxmox finally applies the class disk size.

**Negative and costs**
- One new RPC, two new enums and a new capability, implemented in four
  providers.
- `Describe` gets heavier. On virsh-over-SSH libvirt that is one extra exec
  per poll (Q8).
- A new condition appears on every VM that asks for a disk size. It is
  `Unknown` until its provider is upgraded, which may surprise users.
- Proxmox VMs created after Slice 5 get root disks at least as large as the
  class size, when their class sets one. `diskDefaults` is optional
  (`vmclass_types.go:52-54`), and its `40Gi` default applies only to a class
  that sets `diskDefaults` without a size. A class without `diskDefaults`
  changes nothing. For the others, new Proxmox VMs whose templates are smaller
  get bigger root disks.

**Neutral**
- `GetDiskInfo` and `ListVMs` are unchanged. Their disk-size bugs (C5) are
  separate fixes.

## Open questions

1. **Does a VMClass `diskDefaults.size` change grow the root disks of existing
   VMs?** A class is shared, so one edit grows every VM that uses it, and the
   grow cannot be undone. CPU and memory already follow class edits, and
   libvirt treats the class size as a floor today. *Recommendation:* yes, as a
   floor, because it is consistent with CPU/memory and with the "silently
   ignored" framing of R19. The administrator's disk-size limit is the brake.
   The alternative is create-time only, plus a per-VM override (Q3).
2. **Should `spec.disks[].expandPolicy` be honoured?** This is William's
   decision, and it is needed before Slice 3 (D5 step 6 describes both
   outcomes). Its CRD default is `Offline`, so honouring it means every
   additional-disk grow on a running VM waits for a power-off. That is a
   regression from vSphere's online grow today, and an explicit choice cannot
   be told apart from the default. The alternative is to keep it a documented
   no-op and let the provider decide what it can do online. *Leaning:* the
   field promises `Offline`, which argues for honouring it with an upgrade
   note. The regression argues against.
3. **A per-VM root size override (`spec.rootDisk.sizeGiB`)?**
   *Recommendation:* not now. Revisit after Slice 3 if a "grow one VM's root
   disk" request comes in.
4. **Clustered storage: a filter (D6) or committed storage accounting?**
   *Recommendation:* the filter now, and an ADR-0007 amendment for
   accounting.
5. **Keep libvirt's in-guest filesystem grow on by default?** In a regulated
   environment, running commands in the guest without consent stands out.
   *Recommendation:* keep it for the root disk in this ADR, report the
   outcome, and decide on an opt-in switch separately.
6. **Should `status.phase` be `Reconfiguring` during a disk resize?**
   *Recommendation:* yes, while the call or task is in flight.
7. **Proxmox `Create` applying `diskDefaults.size`.** Is the visible change in
   Q1's direction acceptable in a minor release? It affects only classes that
   set `diskDefaults` (`vmclass_types.go:52-54`): an explicit size, or the
   `40Gi` default when `diskDefaults` is present without one. New VMs from
   templates smaller than that get bigger root disks.
8. **How often to observe disks.** On every `Describe` (2-5 min for a stable
   VM), or less often for virsh-over-SSH single-host libvirt?
   *Recommendation:* every `Describe`. It is one exec, and drift must be
   visible.

## Follow-ups

- Implement Slices 1-7 (D11). Each slice that touches the CRD or proto goes
  through the `crd-update` / `proto-update` workflow.
- Separate items:
  - libvirt and Proxmox `Create` attach `spec.disks`;
  - `ListVMs` disk sizes on libvirt and Proxmox (C5);
  - the vSphere `GetDiskInfo` metadata `unit_number` prints a pointer
    (`vsphere/server.go:3658`);
  - vSphere `hotPlugMemoryIncrementSize` and the < 3 GB Linux hot-add rules
    (C7).
- Docs (by the tech writer, per slice): `docs/reconfigure-results.md`,
  `docs/upgrading.md`, and a disk-resize page.
