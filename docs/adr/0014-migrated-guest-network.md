# ADR-0014: A migrated guest keeps its identity and gets working network

## Status

**Proposed (2026-10-23).** Nothing in this record is implemented. William Rizzo
reviews it before any slice starts. The questions he needs to answer are in
[Open questions](#open-questions).

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Tracking**: item R36 (found in the v0.4.0-rc4 lab runs). Maintainer decision
"option 1": a migrated guest keeps its identity on every target. A later
"option 2" (a fresh identity on every target) may follow; this design keeps
that switch open (see [D2](#d2-api-crd-additive-v1beta1-stays-compatible)).

**Related**:
- [ADR-0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md)
  §"vSphere target" item 5: the only place the guest's network was mentioned so
  far ("a guest-OS concern, out of scope for the raw-disk transfer").
- [ADR-0007](./0007-clustered-orchestrator-provider.md) Addendum B: a
  clustered target VM is created at `Validating`, before the import.
- R26 (`internal/providers/cloneidentity`): a clone gets a NEW cloud-init
  identity. This ADR is the opposite case: a migration keeps the identity. Both
  use the same tools.
- R27 (`internal/providers/libvirt/seed_dir.go`, `seed_reclaim.go`): the libvirt
  per-create seed directories.
- [ADR-0012](./0012-proxmox-legacy-unstamped-name-rule.md) and
  [ADR-0013](./0013-disk-resize-flow.md): both also add proto fields. All field
  numbers here are placeholders (`<next free>`), assigned when the slice is
  implemented, after whichever of those lands first.

All VirtRigaud line numbers refer to `origin/main` at `d1843694`. cloud-init
line numbers refer to the upstream tag `24.4`. The cited logic is unchanged in
`25.1` (the network gate in `stages.py` is byte-identical). The lab guests are
Ubuntu 24.04 with cloud-init 24.x/25.x. Proxmox line numbers refer to
`pve-qemu-server` at `71ea0ac7` (2026-10-20).

## Context

### C1. What the lab showed on v0.4.0-rc4

The guest was an Ubuntu 24.04 cloud image. Its network was configured by
cloud-init: `/etc/netplan/50-cloud-init.yaml` has a `match: macaddress:` for the
source NIC plus `set-name:`.

| Target | Guest boots | Network | Identity |
|--------|-------------|---------|----------|
| vSphere | yes | **none** | kept (by accident: cloud-init does not run) |
| Proxmox | yes | **none** | kept (by accident: cloud-init does not run) |
| libvirt (single-host and clustered) | yes | works | **changed**: new instance-id, new hostname, new SSH host keys, per-instance modules run again |

### C2. What each target does today with a migrated VM

The migration's target VirtualMachine carries only the provider, power state,
imported disk, class, networks and placement
(`internal/controller/vmmigration_clustered_target.go:308-351`). It has no
`userData` and no `metaData`. So the VirtualMachine controller sends a
`CreateRequest` with no `user_data` and no `meta_data`
(`internal/controller/virtualmachine_controller.go:2653-2679`).

- **vSphere.** The imported-disk branch writes the cloud-init guestinfo keys
  only when user-data is present (`internal/providers/vsphere/server.go:2922`).
  The migrated VM therefore gets no `guestinfo.metadata` and no
  `guestinfo.userdata`. Its NIC is a vmxnet3 whose MAC vCenter generates
  (`server.go:2847-2884`: no `AddressType`, no `MacAddress`). In the guest,
  `ds-identify` finds no datasource. The VMware check needs a guestinfo
  metadata, userdata or vendordata key (`tools/ds-identify:1660-1692`), and
  Ubuntu's default policy is `notfound=disabled`
  (`tools/ds-identify:103`). So cloud-init is disabled. The old netplan matches
  the source MAC, which no NIC has, and the guest has no network.
- **Proxmox.** A cloud-init drive is attached only when the request has
  user-data (`internal/providers/proxmox/s3import.go:393-409`). The NIC gets a
  PVE-generated MAC. The result is the same as on vSphere.
- **libvirt.** Every create builds a NoCloud seed
  (`internal/providers/libvirt/provider_virsh.go:398-447`). The seed's
  `instance-id` is the TARGET domain name (`:416`, `:433`), and the domain name
  is `<namespace>.<name>` (`domain_naming.go:172-194`). The seed has no
  `network-config` file. The `network:` key that `generateMetaData` writes into
  meta-data (`cloudinit.go:181-229`) is **ignored by cloud-init**: NoCloud
  reads network configuration only from the `network-config` file or the
  deprecated `network-interfaces` meta-data key
  (`cloudinit/sources/DataSourceNoCloud.py:224-225`). The guest therefore sees
  a new instance and renders cloud-init's fallback network configuration (DHCP
  on the first NIC, matched by its current MAC). That is why the network
  works, and also why the identity changes. In one case the identity happens
  to stay the same: a libvirt-to-libvirt move whose target has the same
  namespace and name, because the domain name, and so the instance-id, is
  unchanged.
- **libvirt ignores `meta_data`.** The gRPC server converts only `user_data`
  (`internal/providers/libvirt/server.go:390-395`). `spec.metaData` therefore
  has no effect on a libvirt VM.

### C3. A NIC's MAC address never reaches any provider (a latent bug)

`VirtualMachine.spec.networks[].macAddress` exists and is validated by a
pattern (`api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go:596-599`).
It is dropped on the way to every provider:

- The controller builds each `contracts.NetworkAttachment` without
  `MacAddress` (`virtualmachine_controller.go:2587-2593`). The field exists in
  the contract (`internal/providers/contracts/types.go:90-91`).
- **libvirt** would render it (`provider_virsh.go:2292-2296`), but it never
  receives it.
- **vSphere** parses only `NetworkName` and the static-IP fields, and only from
  the first attachment (`server.go:2409-2425`).
- **Proxmox** parses `networks_json` with lower-case keys (`"name"`, `"vlan"`,
  `"mac"`: `internal/providers/proxmox/server.go:1360-1430`, MAC at `:1427`).
  The manager sends `json.Marshal([]contracts.NetworkAttachment)`
  (`internal/transport/grpc/client.go:1450-1451`), and that struct has no JSON
  tags (`types.go:77-105`), so its keys are `"Name"`, `"Bridge"`, `"VLAN"` and
  `"MacAddress"`. **No field of a Proxmox network attachment is applied**, not
  the bridge, the VLAN tag or the MAC: every NIC falls back to `virtio` on
  `vmbr0` (`server.go:1438-1448`). The only unit test uses hand-written
  lower-case JSON (`provider_test.go:604-606`). Two more Proxmox problems sit
  next to this one:
  - `buildNetworkString` writes `,mac=` (`server.go:1551-1552`), which is not a
    PVE key. PVE's keys are `macaddr` or the `<model>=<MAC>` alias
    (`pve-qemu-server src/PVE/QemuServer/Network.pm:49`, `:66`). The client
    builder uses `macaddr=` correctly (`pveapi/client.go:800-801`).
  - The VM listing reads `mac=` (`server.go:2384-2385`). PVE stores
    `virtio=<MAC>`, so the listing never reports a MAC.

The Proxmox bridge and VLAN loss is a correctness bug for every Proxmox VM,
whether or not it was migrated. It should be fixed on its own (Slice 0).

### C4. The source's instance-id: what the hypervisor knows

| Source | instance-id the guest last saw | Can the provider read it? |
|--------|--------------------------------|---------------------------|
| libvirt, created by VirtRigaud | the domain name (`provider_virsh.go:416`, `:433`); for a clone, `virtrigaud-<target UID>` (`cloneidentity.go:79-90`) | Yes: the seed ISO's `meta-data`, read with `isoinfo` as R26 does (`clone_seed.go`, `readSeedISO`), from the SSH user's private copy once the domain has started (R27) |
| libvirt, adopted (a `cidata` CD-ROM or a `ds=nocloud;i=` SMBIOS serial that VirtRigaud did not write) | whatever that seed says | Yes, from the domain XML and the ISO, bounded like `readSeedISO` |
| vSphere, created by VirtRigaud **with** user-data | `instance-id` of `spec.metaData`, else the VM name (`server.go:1569-1574`, `:2448`) | Yes: `guestinfo.metadata` in ExtraConfig, as `vsphere/clone_identity.go:39-51` reads it |
| vSphere, guestinfo userdata/vendordata but no `instance-id` in metadata | the BIOS UUID, lower-cased (`DataSourceVMware.py:265-273`) | Derived from `config.uuid`. The SMBIOS byte order differs on old virtual hardware, so this needs a lab check (deferred) |
| vSphere, no guestinfo at all | unknown. cloud-init is disabled on the source, and whatever ID is on disk is from an earlier life | No |
| Proxmox (`citype` nocloud or configdrive2) | `sha1_hex(user_data . network_data)` (`Cloudinit.pm:603-607`, `:248-253`) after `cicustom` snippets replace the generated files (`:610-620`, `:642-677`); a `cicustom` `meta=` snippet is used verbatim. PVE's network data includes the NIC MACs (`:543-549`) | Yes, but **not** reliably through the API. `GET .../cloudinit/dump?type=meta` recomputes from the *generated* user/network data and ignores `cicustom` (`:746-770`). The authoritative copy is the cloud-init drive the guest booted from: `isoinfo` on `pvesm path <vm-N-cloudinit>` over the provider's existing SSH session. `isoinfo` ships with `genisoimage`, a `qemu-server` dependency (`debian/control:37`) |
| any source, guest running with qemu-guest-agent | `/var/lib/cloud/data/instance-id` itself | Possible with `guest-file-open`/`guest-file-read`, where the guest allows it (RHEL's default agent config blocks them). Deferred |

Two consequences:

- PVE derives the instance-id from the MAC addresses. Even a PVE-to-PVE move
  with a new MAC therefore gives the guest a new instance.
- No hash or name we compute could reproduce an arbitrary instance-id on PVE.
  Only a seed that VirtRigaud writes verbatim can carry one.

## Research: what cloud-init actually does

All citations are cloud-init `24.4` unless marked otherwise.

**R1. New instance is decided by comparing one string.**
`Init.is_new_instance()` returns true when there is no previous instance-id or
when `/var/lib/cloud/data/instance-id` differs from
`datasource.get_instance_id()` (`cloudinit/stages.py:535-546`). The datasource
*type* plays no part. So a guest that last booted from VMware guestinfo and
now boots from a NoCloud seed with the **same** instance-id is the same
instance. Per-instance semaphores live under
`/var/lib/cloud/instances/<instance-id>/sem/`, so none of the per-instance
modules run again: `ssh` (host keys, `ssh_deletekeys` defaults to true,
`cc_ssh.py:103`), `set_hostname`, `users_groups`, `apt_configure`, `locale`,
`seed_random`, `set_passwords`, `grub_dpkg`, `write_files` and `runcmd` are all
`PER_INSTANCE`. `update_hostname`, `update_etc_hosts`, `bootcmd`, `growpart`
and `resizefs` are `PER_ALWAYS`.

**R2. A changed datasource is found again; the cached one is not trusted.**
At the local stage, cloud-init loads the cached datasource (`obj.pkl`) and,
with `existing="check"`, keeps it only if `/run/cloud-init/.instance-id`
matches it (never true on a fresh boot, because `/run` is a tmpfs) or if the
datasource's `check_instance_id()` returns true (`stages.py:345-369`).

- NoCloud's `check_instance_id` reads only the kernel cmdline and the seed
  *directories*, not a CD-ROM (`DataSourceNoCloud.py:256-270`, `:286-306`).
- VMware has no `check_instance_id`.

So the cache is dropped and `ds-identify`'s list is crawled again. The
exception: `manual_cache_clean: true` or the `manual_clean_marker` file makes
it `existing="trust"` (`cloudinit/cmd/main.py:488-497`). The guest then keeps
using the *source's* cached datasource object and never looks at a new seed.
Identity is preserved; nothing is re-rendered.

**R3. On a same-instance boot, network configuration is not rendered again,
whatever the datasource carries.** `apply_network_config` returns early ("No
network config applied. Neither a new instance nor datasource network update
allowed") unless one of these holds (`stages.py:1072-1090`):

- it is a new instance;
- the `boot` event is enabled for the network scope;
- the `boot-legacy` event is enabled for the network scope.

It only applies the renames from the config (`_apply_netcfg_names`).

**R4. No datasource we can present enables `boot` by default.** The base class
supports `boot-new-instance`, `boot`, `boot-legacy` and `hotplug`, but its
default is `boot-new-instance` only (`cloudinit/sources/__init__.py:265-279`).

- NoCloud and VMware keep the base default.
- ConfigDrive and OpenStack add support but not the default
  (`DataSourceConfigDrive.py:37-44`, `DataSourceOpenStack.py:51-58`).
- Only Ec2, Azure, GCE, Scaleway, SmartOS, RbxCloud and AliYun change
  `default_update_events`.
- The upstream docs state "All datasources support the BOOT_NEW_INSTANCE event
  by default" (`doc/rtd/explanation/events.rst`, "Datasource event support").

**R5. A seed cannot switch `boot` on for its first boot.** `update_event_enabled`
merges the datasource defaults with the `updates:` key of the *merged system
config* (`stages.py:66-111`). Its docstring says: "on first boot, userdata may
NOT be available yet ... an event that should be enabled in userdata may be
denied". The merged config comes from these sources:

- `/etc/cloud/cloud.cfg{,.d}`;
- the kernel cmdline;
- the datasource's `get_config_obj()`. This is `{}` for NoCloud
  (`sources/__init__.py:754-755`), and for VMware it is filled only on the
  guest-customization (IMC) path (`DataSourceVMware.py:99`, `:435`);
- the *cached* `cloud-config.txt` of the current instance directory
  (`cloudinit/helpers.py:204-229`).

User-data and vendor-data from the new seed are processed **after** the
network step. For the same instance-id, the instance directory is the
source's, so the cached cloud-config is the **source's** previous user-data.
Conclusion: `updates: {network: {when: [boot]}}` delivered in a new seed takes
effect on the *second* boot at the earliest. It takes effect on the first boot
only if the guest already had it on disk.

**R6. Consequence for the mechanism in the brief.** A NoCloud seed (or
guestinfo, or a PVE drive) with the **same** instance-id **and** a
`network-config` matching `name: "e*"`/`dhcp4: true` keeps the identity but
**does not** bring the network up on the first target boot. cloud-init ignores
that network-config (R3, R4, R5). That mechanism cannot be the network fix on
its own.

**R7. A changed instance-id re-renders the network but is a new instance.**
This is today's libvirt behaviour. cloud-init's per-instance set (R1) is
larger than "identity". It rewrites apt sources (`apt_configure`), locale,
password locks (`users_groups`, `set_passwords`) and more, depending on the
image's config and cloud-init version. That is acceptable for option 2
("fresh"). It is not a way to keep the identity.

**R8. What cloud-init never touches.** cloud-init does not regenerate
`/etc/machine-id` (already recorded in `cloneidentity.go:35-38`). systemd
regenerates it only when it is empty. A disk copy therefore keeps it on every
path. With systemd-networkd, the DHCP client identifier (DUID) is derived from
the machine-id, and `set-name` keeps the interface name, so the same DHCP
server may hand out the same address even with a different MAC. That depends
on the server; it is not a guarantee.

**R9. Things a migration cannot fix without access to the guest.**

- Configurations bound to an interface *name*. Examples: a subiquity
  install's `00-installer-config.yaml` (`ens33:`), ifupdown, and a
  NetworkManager keyfile with `interface-name=`. Predictable names follow the
  PCI layout (`ens192`/`ens160` on vSphere, `enp1s0` or `ens3` on libvirt,
  `ens18` on PVE).
- Windows. It binds IP configuration to the adapter instance, not to the MAC.
  The NIC model changes between hypervisors, so a static configuration is lost
  and the new adapter uses DHCP, provided a driver exists: virtio-net needs
  virtio-win, vmxnet3 needs VMware Tools, and e1000/e1000e are inbox.
- Guests that run cloudbase-init. Its same-instance behaviour is not verified.

## Options considered

### Option A: an identity-preserving seed with a DHCP-any-NIC network-config

Present the source instance-id (and hostname) and a `network-config` matching
any `e*` NIC with DHCP, in each target's native datasource.

- **Rejected as the network fix.** cloud-init ignores the network-config on a
  same-instance boot (R3 to R6). The identity part is still useful, and it is
  part of Option B.

### Option B: carry the NIC MAC addresses, and present the same instance-id or no datasource (**recommended**)

- Each target NIC gets the MAC of the corresponding source NIC. The guest's
  existing network configuration then matches unchanged. This covers
  cloud-init's netplan `match: macaddress`, RHEL `HWADDR=`, NetworkManager
  `mac-address=`, udev persistent-net rules, and DHCP reservations keyed by
  MAC. It works for every guest OS and needs no guest cooperation.
- On the target, VirtRigaud presents either a datasource with the **same**
  instance-id (when it is known), or **no** datasource. It never presents a
  datasource with a different instance-id.
- This is how a hypervisor's own cross-host or cross-vCenter move behaves: a
  moved VM keeps its MAC.
- Costs and limits:
  - **Duplicate MACs.** The source must not run on the same L2 segment at the
    same time ([D6](#d6-safety-duplicate-identity-and-mac-rules)).
  - **vSphere rejects some MACs.** A manual MAC in VMware's OUI must be in
    `00:50:56:00:00:00`–`00:50:56:3F:FF:FF`; any other unique non-VMware OUI
    is accepted (vSphere 8 networking docs, "Static MAC Addresses"). A MAC
    that vCenter generated (`00:50:56:80`–`BF`) can therefore not be reused on
    a vSphere target. That only matters for a guest returning to vSphere.
  - **Name-bound configurations (R9) are not fixed.**

### Option C: a new instance-id, with user-data that suppresses the identity modules

Set `ssh_deletekeys: false`, `ssh_genkeytypes: []` and
`preserve_hostname: true`, and let the network re-render as a new instance.

- **Rejected.** The other per-instance modules (R7) still run. They differ by
  image, `cloud.cfg` and cloud-init version, and can rewrite apt sources,
  locale or password locks on a production guest. We cannot enumerate them
  safely.

### Option D: edit the guest disk offline during conversion

Use libguestfs/`virt-customize` to rewrite netplan or drop a
`cloud.cfg.d/99-virtrigaud-migrated.cfg` with `updates.network.when: [boot]`.

- **Rejected for now.**
  - It needs libguestfs on every target host, and on the vSphere path in the
    relay pod.
  - It fails on LUKS or unusual filesystems.
  - It parses untrusted guest filesystems in a privileged context.
  - It is the only option that changes the guest's files.
- Could be revisited as an opt-in.

### Option E: two boots

A same-instance-id seed whose user-data sets `updates.network.when: [boot]`
takes effect on the second boot (R5), so the controller would reboot the
target once.

- **Rejected.** The first boot has no network, so the cloud-init network
  stage waits until it times out. The controller would need to know when
  cloud-init has finished. Every later boot then re-renders the network from
  the seed, overwriting what the guest's administrator changed in
  `50-cloud-init.yaml`.

## Decision

### D1. Goals and non-goals

Goals, under `guestIdentity: Preserve` (the default), on vSphere, Proxmox and
libvirt (single-host and clustered) targets:

- The same `/var/lib/cloud/data/instance-id`. `previous-instance-id` equals
  it, and the instance directory list does not grow.
- The same hostname.
- The same SSH host keys.
- The same `/etc/machine-id`.
- Working network on the carried NICs for guests whose network configuration
  matches by MAC, which includes every cloud-init-configured guest.

Non-goals:

- Name-bound configurations, Windows static IPs and cloudbase-init (R9). They
  are documented, not fixed.
- Static IPs on a different subnet. A preserved configuration is the source's
  configuration.
- Copying user-data ([D7](#d7-security)).
- Option 2 behaviour on vSphere and Proxmox ([D9](#d9-slices-smallest-first-each-one-a-pr),
  Slice 6).

### D2. API (CRD): additive, v1beta1 stays compatible

`VMMigration.spec.options` gets the knob (recommended now; see Open
question 2):

```go
// GuestIdentity selects what the migrated guest keeps.
// Preserve (the default): the guest keeps its cloud-init instance-id,
// hostname, SSH host keys and machine-id; each target NIC carries the MAC of
// the source NIC at the same index; the target presents the same instance-id
// when the source's is known, otherwise no cloud-init datasource.
// Fresh: the guest boots as a new cloud-init instance on the target (new MACs,
// a new instance-id; per-instance modules run again). Today's libvirt
// behaviour; vSphere and Proxmox targets refuse it until they implement it.
// +kubebuilder:validation:Enum=Preserve;Fresh
// +optional
GuestIdentity string `json:"guestIdentity,omitempty"`
```

`VMMigration.status.guestIdentity` is new. It records what was captured once,
at `Validating`, so retries and a later deletion of the source cannot change
it:

```go
type MigrationGuestIdentity struct {
    Mode             string               `json:"mode"`                       // Preserve | Fresh, as applied
    InstanceID       string               `json:"instanceID,omitempty"`       // "" = unknown
    InstanceIDOrigin string               `json:"instanceIDOrigin,omitempty"` // Datasource | Unknown (later: Derived, Guest)
    Datasource       string               `json:"datasource,omitempty"`       // NoCloud | VMwareGuestInfo | ConfigDrive | None | Unknown
    Hostname         string               `json:"hostname,omitempty"`         // the source datasource's local-hostname
    NICs             []MigrationGuestNIC  `json:"nics,omitempty"`             // {index, macAddress}
    CapturedAt       metav1.Time          `json:"capturedAt"`
}
```

A new `GuestIdentity` condition on `VMMigration` has these reasons:

- `Preserved`: instance-id and every carried MAC.
- `PreservedWithoutDatasource`: MACs carried, instance-id unknown, no
  datasource presented.
- `MACNotCarried`: with which NIC and why: the target provider does not
  support it, there is no target network at that index, or the vSphere MAC
  rule.
- `SourceUnsupported`: the source provider predates the capture RPC.
- `Fresh`.

Each reason also produces an Event.

`VirtualMachine.spec.importedDisk` gets the guest identity the target must
present, set by the migration next to `migrationRef`
(`virtualmachine_types.go:666-701`):

```go
// GuestIdentity is the cloud-init identity the target presents to an imported
// guest. Set by VMMigration; a user creating a VM from an imported disk may
// set it too (it is no more powerful than spec.metaData).
// +optional
GuestIdentity *ImportedGuestIdentity `json:"guestIdentity,omitempty"`

type ImportedGuestIdentity struct {
    // +kubebuilder:validation:Enum=Preserve;Fresh
    Mode string `json:"mode"`
    // Empty with Preserve: present NO datasource.
    // +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`
    // +optional
    InstanceID string `json:"instanceID,omitempty"`
    // RFC 1123 hostname (labels of up to 63 characters, 253 in total).
    // +optional
    Hostname string `json:"hostname,omitempty"`
}
```

The MACs travel in the existing `spec.networks[i].macAddress`
(`virtualmachine_types.go:596-599`). `newMigrationTargetVM`
(`vmmigration_clustered_target.go:308-351`) sets it for index `i` from
`status.guestIdentity.nics[i]`, for every `i` that has a `target.networks`
entry. Source NICs with no target network are reported (`MACNotCarried`,
reason `NoTargetNetwork`) and dropped. They are not invented, and the user's
network list is not extended (Open question 8).

The instance-id pattern excludes `/` and control characters. cloud-init uses
the value as a directory name. A captured value that fails the pattern is
recorded as unknown, never presented.

### D3. Proto: one new RPC, one new `CreateRequest` field, three capabilities

```proto
// DescribeGuestIdentity reports what the guest of a VM is identified by, for
// a migration that must preserve it (ADR-0014). Read-only; routed and
// owner-checked like Describe.
rpc DescribeGuestIdentity(DescribeGuestIdentityRequest) returns (DescribeGuestIdentityResponse);

message DescribeGuestIdentityRequest {
  string vm_id = 1;
  string target_host_id = 2;   // clustered routing, as DescribeRequest
  ObjectIdentity owner = 3;    // owner check, as DescribeRequest
}

message GuestNIC {
  int32 index = 1;             // the order in which the provider creates NICs for spec.networks
  string mac_address = 2;      // lower-case, colon-separated
  string model = 3;            // informational (virtio, vmxnet3, e1000e, ...)
}

enum GuestDatasource {
  GUEST_DATASOURCE_UNSPECIFIED = 0;
  GUEST_DATASOURCE_NONE = 1;              // the provider presents none
  GUEST_DATASOURCE_NOCLOUD = 2;
  GUEST_DATASOURCE_VMWARE_GUESTINFO = 3;
  GUEST_DATASOURCE_CONFIG_DRIVE = 4;
  GUEST_DATASOURCE_UNKNOWN = 5;           // something is presented that the provider cannot read
}

message DescribeGuestIdentityResponse {
  bool nics_reported = 1;      // false: the provider could not enumerate NICs
  repeated GuestNIC nics = 2;
  GuestDatasource datasource = 3;
  string instance_id = 4;      // "" = unknown; only a value READ from the datasource
  string local_hostname = 5;   // the datasource's local-hostname, "" if none
}

// CreateRequest
GuestIdentitySeed guest_identity = <next free>;

message GuestIdentitySeed {
  enum Mode { MODE_UNSPECIFIED = 0; PRESERVE = 1; FRESH = 2; }
  Mode mode = 1;
  string instance_id = 2;      // PRESERVE: present exactly this; "" = present NO datasource
  string local_hostname = 3;   // optional; omitted keeps the guest's current hostname
}

// GetCapabilitiesResponse
bool supports_guest_identity_describe = <next free>;
bool supports_guest_identity_seed = <next free>;
bool supports_nic_mac = <next free>;      // honours NetworkAttachment.MacAddress on Create
```

Why a new RPC instead of new `Describe` fields: `Describe` runs on every
reconcile, and on libvirt and Proxmox reading a seed ISO costs SSH round
trips. The capture is needed once per migration.

An unknown `GuestIdentitySeed` (an older provider) is ignored by proto3. The
manager therefore sends it only to a provider that declares
`supports_guest_identity_seed`, and otherwise records `MACNotCarried` or
`SourceUnsupported` as appropriate.

### D4. Controller flow

1. **`Validating`, before the clustered target is placed** (before
   `vmmigration_controller.go:624`) and before the optional source power-off
   (`:636`): if `status.guestIdentity` is empty, call
   `DescribeGuestIdentity` on the source provider. Validate:
   - each MAC is unicast and not all-zero;
   - the instance-id matches the pattern;
   - the hostname is RFC 1123.

   Then record the result once. Under `Fresh`, record only the mode.
2. **MAC admissibility.** Check each MAC against the target provider type:
   - Every target needs `supports_nic_mac`.
   - A vSphere target refuses the VMware OUI `00:50:56` outside `00`–`3F`.
     Other VMware OUIs (`00:0C:29`, `00:05:69`, `00:1C:14`) are treated the
     same until a lab check shows otherwise.

   An inadmissible MAC is `MACNotCarried` for that NIC. It is never rewritten.
3. **Target VM.** `newMigrationTargetVM` and the creating-phase builder
   (`vmmigration_controller.go:1952-1961`) set:
   - `spec.networks[i].macAddress`;
   - `spec.importedDisk.guestIdentity = {mode, instanceID, hostname}`.
4. **VirtualMachine controller.** It forwards `spec.networks[].macAddress`
   into the attachment (`virtualmachine_controller.go:2587-2593`, the Slice 0
   fix). It maps `spec.importedDisk.guestIdentity` to
   `CreateRequest.guest_identity`, only when the target Provider declares
   `supports_guest_identity_seed`.
5. **Compatibility marker.** A migration with no `status.guestIdentity`
   (created before the upgrade and past `Validating`) behaves exactly as
   today.

### D5. How each target renders the identity

All meta-data is built with the YAML encoder from a fixed template, as
`cloneidentity.RewriteMetaData` does (`cloneidentity.go:195-235`). Values are
never interpolated. The seed holds `instance-id`, `local-hostname` (only when
known) and an empty `#cloud-config` user-data. NoCloud requires a user-data
file (`DataSourceNoCloud.py:100-103`). There is no `network-config` and no
`vendor-data`.

On a same-instance boot, cloud-init ignores a network-config (R3), and with no
network-config a *future new instance* (a clone) gets the fallback (DHCP on
its own first NIC), which is what a clone needs.

| Target | instance-id known | instance-id unknown | MAC |
|--------|-------------------|---------------------|-----|
| **libvirt** (both topologies) | A seed made by `PrepareCloudInit` with that meta-data, in the R27 per-create seed directory named after the domain. Delete and reclaim work unchanged. It replaces the default seed `createVMWithCloudInit` builds today (`provider_virsh.go:398-447`) | **No seed.** Use the existing "continue without cloud-init" path (`:443-447`) on purpose, not as a fallback after a failure | `<mac address=.../>` (`provider_virsh.go:2292-2296`) |
| **vSphere** | `guestinfo.metadata` (base64) and `guestinfo.metadata.encoding` in the imported-disk branch, even with no user-data (today it is gated at `server.go:2922`). No `guestinfo.userdata`. `ds-identify` accepts metadata alone (`tools/ds-identify:1689-1692`) when the guest has `open-vm-tools`; without them the guest has no datasource, which is still identity-safe | No guestinfo keys | `VirtualEthernetCard.AddressType = "manual"`, `MacAddress` on the NIC built at `server.go:2847-2884`. vSphere create consumes only `networks[0]` (`:2409-2425`), so only NIC 0 is carried until multi-NIC create exists |
| **Proxmox** | Build the NoCloud ISO on the node with `genisoimage` (always installed, D3). `qm importdisk` it into the VM's own storage, as the import already does for the boot disk (`s3import.go:370-389`). Then `qm set --ide2 <volid>,media=cdrom` over the existing SSH session (`s3import.go:287`). PVE accepts an `images` volume as a CD-ROM (`API2/Qemu.pm:190-200`). The volume is VM-owned, so `qm destroy` removes it: no leak, unlike a `cicustom` snippet or an ISO under `iso/`. It must not be named `vm-<id>-cloudinit`, because PVE rewrites that one | Nothing attached | `netN: virtio=<MAC>,bridge=...`, or `macaddr=` (`Network.pm:49`, `:66`). This depends on the Slice 0 fix |

Why not PVE's own cloud-init drive with `cicustom`: it needs a storage with
`snippets` content, which is not enabled on PVE's default storage. Snippets
are not VM-owned, so they leak on destroy. And PVE has no API to upload a
snippet.

**Proxmox Clone follow-up.** R26 relies on PVE regenerating its own drive to
give a clone a new identity (`server.go:1049-1061`). A static VirtRigaud seed
would make a clone of a migrated Proxmox VM boot as the same instance. Slice 4
therefore also makes Proxmox `Clone` rebuild the VirtRigaud seed volume with
the clone's identity, as libvirt's `planCloneSeed` does.

### D6. Safety: duplicate identity and MAC rules

Option 1 moves an identity. Running the source and the target together was
already wrong: two VMs would have the same hostname and host keys. Carrying
the MAC makes it worse on a shared L2: the bridge's forwarding table flaps and
DHCP collides. The rules:

- Under `Preserve`, the target is powered on only once the source is
  confirmed `Off`. The existing `ensureSourcePoweredOff` check
  (`vmmigration_source_power.go:131`) is reused; Open question 4 asks whether
  to power it off or to wait. With `deleteAfterMigration: false` the source
  stays defined but off, and the docs say not to start both.
- libvirt (single-host and clustered) refuses a carried MAC that another
  domain on the same host (clustered: any host) already uses. This is
  best-effort, from the domain XMLs it already reads.
- vCenter detects conflicts within its own inventory at power-on. PVE does
  not check; this is documented.

### D7. Security

- **What travels.** No tenant secret travels. The only data carried are the
  instance-id, the hostname and the MACs, all non-secret, all
  pattern-validated, and all written through the YAML encoder or typed API
  fields.
- **User-data is not copied, from any source.**
  - PVE's generated user-data contains `cipassword` (`Cloudinit.pm:156-167`).
  - `guestinfo.userdata` and our own seeds may contain keys or passwords.
  - The target may be in another namespace (consumer grants).

  The price is that `PER_ALWAYS` user-data such as `bootcmd` stops running on
  the target; this is documented. A later opt-in could copy a VirtRigaud-owned
  seed's user-data when the source and target namespaces are equal (Open
  question 5).
- **Values read from guest-influenced places.** An adopted VM's ISO, or
  `spec.metaData` on vSphere, is tenant-controlled. Such a value is only ever
  presented back to the same tenant's guest as its own identity, which the
  tenant could already set through `spec.metaData`. Reads are bounded (as
  `readSeedISO`, `maxSeedFileBytes`). Values are validated by pattern. File
  contents are never logged.
- **What is stored.** `status.guestIdentity` holds no secret. Seed files are
  private as in R27. The Proxmox seed volume is VM-owned. `guestinfo.metadata`
  is readable by the guest, which is its purpose.

### D8. Upgrade, rollback and compatibility

- **CRD.** Additive only. Without the field, `spec.options.guestIdentity` is
  treated as `Preserve` for migrations that reach `Validating` after the
  upgrade. A migration already past `Validating` has no
  `status.guestIdentity` and keeps today's behaviour (D4 step 5).
- **Behaviour change on libvirt targets** (release note): a migrated guest
  now keeps its hostname, host keys and instance-id instead of becoming a new
  instance. `guestIdentity: Fresh` restores the old behaviour.
- **An older source provider** (no `supports_guest_identity_describe`): the
  condition reason is `SourceUnsupported`. No MACs are carried and no seed is
  presented. On a libvirt target that is a regression for this pair, because
  today's fresh seed gave network. The condition says so and names
  `guestIdentity: Fresh` as the remedy (Open question 3).
- **An older target provider**:
  - without `supports_nic_mac`, the reason is `MACNotCarried`
    (`TargetUnsupported`), which is today's outcome;
  - without `supports_guest_identity_seed`, no seed field is sent, and libvirt
    keeps building a fresh seed, which is today's behaviour, recorded in the
    condition.
- **Rollback.** An older manager ignores the new fields. VMs created under
  `Preserve` keep their seed or guestinfo, and a re-sent `Create` renders
  identical content (deterministic, as R26 and R27 do).

### D9. Slices (smallest first; each one a PR)

- **Slice 0: MAC and Proxmox network plumbing (a bug fix that stands alone;
  can merge before this ADR is accepted).**
  - Forward `spec.networks[].macAddress` (C3).
  - Give `contracts.NetworkAttachment` JSON tags, or make Proxmox parse the
    real keys, so that the Proxmox bridge, VLAN and MAC are applied.
  - Proxmox: `macaddr=`/`<model>=` on write, and parse `<model>=<MAC>` in
    Describe and List.
  - vSphere: a manual MAC on `networks[0]`.
  - Add `supports_nic_mac`.
  - Unit tests that marshal the real `contracts` struct.
- **Slice 1: capture.**
  - The `DescribeGuestIdentity` RPC in libvirt (domain XML MACs and the seed
    ISO meta-data, single-host and routed), vSphere (NIC MACs and
    `guestinfo.metadata`), Proxmox (`netN` MACs, and the drive meta-data over
    SSH) and mock.
  - `spec.options.guestIdentity`, `status.guestIdentity`, the condition.
  - Carry the MACs to the target VM.

  With Slice 0, this alone fixes the reported vSphere and Proxmox targets for
  MAC-matched guests.
- **Slice 2: libvirt target.** `CreateRequest.guest_identity`,
  `ImportedDiskRef.guestIdentity`, a same-instance-id seed or no seed,
  `supports_guest_identity_seed`, and the MAC-in-use check. This ends the
  identity change on libvirt.
- **Slice 3: vSphere target.** `guestinfo.metadata` with the same
  instance-id.
- **Slice 4: Proxmox target.** The VM-owned seed volume, plus Proxmox `Clone`
  rebuilding it with a new identity (D5).
- **Slice 5: the power-on guard** (D6) and the duplicate-MAC checks.
- **Slice 6 (option 2, later): `Fresh` on vSphere and Proxmox.**
  - vSphere: `guestinfo.metadata` with `virtrigaud-<target UID>`.
  - Proxmox: PVE's own cloud-init drive with `ipconfig0=ip=dhcp`; its
    SHA-1 instance-id differs from the source's.
  - No MAC carry.
- **Slice 7 (optional): more instance-id sources.** A guest-agent read of
  `/var/lib/cloud/data/instance-id` as a cross-check or as a source for a VM
  we did not create, and the vSphere BIOS-UUID derivation after a lab check of
  the byte order.

### D10. Testing and lab plan

**Unit tests.**
- Seed rendering:
  - the YAML encoder escapes the values;
  - pattern rejection;
  - an unknown instance-id gives no seed;
  - deterministic output.
- libvirt: the domain XML has the MAC and a seed or none.
- vSphere: the ConfigSpec has a manual MAC and `guestinfo.metadata` only.
- Proxmox:
  - the network string;
  - the importdisk and `qm set` command builders;
  - the seed volume name is never `vm-<id>-cloudinit`.
- Controller (envtest):
  - capture happens once and before clustered placement;
  - MAC to `spec.networks` mapping, including the `NoTargetNetwork` case;
  - each condition reason;
  - the legacy path for a migration with no `status.guestIdentity`.
- Every provider: a "real struct" JSON test for `networks_json`.

**Lab matrix.** Ubuntu 24.04 cloud image, cloud-init 24.x and 25.x, in these
directions:

- libvirt → vSphere, libvirt → Proxmox;
- vSphere → libvirt, vSphere → Proxmox;
- Proxmox → libvirt, Proxmox → vSphere;
- libvirt → clustered libvirt.

On the source before export and on the target after its first boot, compare:

- `hostname`;
- `cat /var/lib/cloud/data/instance-id /var/lib/cloud/data/previous-instance-id`
  (all equal);
- `ls /var/lib/cloud/instances` (no new entry);
- `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` (same fingerprint);
- `cat /etc/machine-id` (same);
- `ip -br link` (same MAC, same name through `set-name`);
- `ip -br addr` (an IPv4 address from DHCP);
- `cloud-init status --long` (the target datasource, or "disabled" when the
  instance-id was unknown);
- `grep "Neither a new instance" /var/log/cloud-init.log` (proof that the
  network was not re-rendered).

**Negative and edge cases.**
- A guest with `manual_cache_clean: true` (R2).
- A source with no datasource, so the instance-id is unknown and no seed is
  presented.
- A vSphere-generated MAC to a vSphere target (`MACNotCarried`).
- A target with fewer networks than the source has NICs.
- `guestIdentity: Fresh` on libvirt (today's behaviour).
- A disk grow after migration with a seed present: `growpart` and `resizefs`
  run as `PER_ALWAYS`, which is relevant to ADR-0013.

## Consequences

**Positive.**
- A migrated guest is the same machine, with the same identity, on every
  target. The network works for every guest whose configuration matches by
  MAC, including non-cloud-init guests.
- Fixes a silent-drop bug (`macAddress`), and a Proxmox bridge and VLAN loss
  that affects every Proxmox VM.
- The knob leaves room for option 2 without another CRD change.

**Negative.**
- Option 1 cannot fix configurations bound to an interface name, or Windows
  static IPs, without access to the guest.
- Carrying the MAC needs care against duplicates. vSphere-generated MACs
  cannot return to vSphere.
- The instance-id is often unknown for a VM we did not create. Such guests
  then run without cloud-init on the target, so `growpart` does not run there.
- libvirt changes behaviour by default (D8).

## Open questions

1. **Is the MAC part of the identity we preserve?** Recommended: yes, as a
   hypervisor's own cross-host move does. This is the only mechanism that
   gives a same-instance guest working network without touching its disk
   (R3–R6).
2. **Add the `guestIdentity` knob now?** Recommended: yes, default
   `Preserve`. `Fresh` is today's libvirt behaviour; vSphere and Proxmox
   refuse it at `Validating` until Slice 6. It is the escape hatch for the
   libvirt behaviour change and for guests whose MAC cannot be carried.
3. **What if no MAC can be carried under `Preserve`?** Recommended: proceed
   with `GuestIdentity=False/MACNotCarried` and an Event. That is no
   regression on vSphere or Proxmox, and the identity is never changed unless
   asked. The alternative is to refuse at `Validating` and point to `Fresh`.
   For libvirt targets with an older source provider, should the controller
   fall back to `Fresh` automatically? Recommended: no.
4. **Require the source to be off before the target powers on** (D6)? If so,
   should the controller power it off itself, or wait for an `Off` source?
5. **Never copy user-data** (D7)? An opt-in copy of a VirtRigaud-owned seed's
   user-data could be allowed within one namespace.
6. **With the instance-id unknown, present no datasource** (recommended), or
   keep today's fresh libvirt seed?
7. **Proxmox seed:** a VM-owned importdisk'd ISO volume (recommended), or a
   `cicustom` snippet (needs `snippets` storage and leaks on destroy)?
8. **More source NICs than target networks:** drop them and report
   (recommended), or refuse? Should an empty `target.networks` get one
   synthesized entry per source NIC?
9. **Slice 0 now:** the Proxmox `networks_json` key mismatch drops the bridge
   and VLAN of every Proxmox VM today (C3). Ship it as a separate fix ahead of
   this ADR?
10. **Windows and cloudbase-init:** out of scope beyond MAC carry and the docs
    guidance (inbox `e1000e` or pre-installed drivers)?

## Follow-ups

- Docs: a "What a migrated guest keeps" page (the per-target table, R9's
  limits, the duplicate-MAC rule, `Fresh`). It replaces ADR-0006 §5's "out of
  scope" note with a link.
- Option D (offline disk edit) as an opt-in, if name-bound guests matter in
  practice.

## References

- cloud-init 24.4 source: `cloudinit/stages.py`, `cloudinit/helpers.py`,
  `cloudinit/cmd/main.py`, `cloudinit/sources/__init__.py`,
  `DataSourceNoCloud.py`, `DataSourceVMware.py`, `DataSourceConfigDrive.py`,
  `DataSourceOpenStack.py`, `cloudinit/config/cc_*.py`, `tools/ds-identify`.
  Upstream: <https://github.com/canonical/cloud-init/tree/24.4>.
- cloud-init docs, "Events and updates":
  <https://docs.cloud-init.io/en/latest/explanation/events.html>.
- pve-qemu-server: `src/PVE/QemuServer/Cloudinit.pm`, `Network.pm`,
  `src/PVE/API2/Qemu.pm`, `debian/control`.
  Upstream: <https://git.proxmox.com/?p=qemu-server.git>.
- VMware vSphere 8 networking, "Static MAC Addresses" (VMware OUI static range
  `00:50:56:00:00:00`–`00:50:56:3F:FF:FF`; other unique OUIs allowed).
