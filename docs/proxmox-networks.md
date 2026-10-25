# Proxmox: how a VM's networks reach PVE

A Proxmox VM gets the NICs and IP settings its `VirtualMachine.spec.networks`
names: the bridge, the VLAN tag and the NIC model of each network, and the
static IP, gateway and DNS servers of each entry. Before this change the
provider read none of them (rc4 finding R36, ADR-0014 Slice 0a): every Proxmox
VM got `virtio` on `vmbr0`, and a VM cloned from a template with user-data had
its template's NICs overwritten with that default.

## What the manager sends

For each entry of `spec.networks` the manager sends one network to the provider.
What it fills in, and from where:

| Provider receives | Comes from |
|---|---|
| `Name` | `spec.networks[].name`. A label only: the provider never maps a name to a bridge. |
| `Bridge` | `VMNetworkAttachment.spec.network.proxmox.bridge` (through `networkRef`) |
| `Model` | `VMNetworkAttachment.spec.network.proxmox.model` (the CRD defaults it to `virtio`) |
| `VLAN` | `VMNetworkAttachment.spec.network.proxmox.vlanTag` |
| `StaticIP`, `Prefix`, `Gateway`, `DNS` | `spec.networks[].ipAddress`, `prefix`, `gateway`, `dns` on the **VirtualMachine** |

The bridge, model and VLAN come from the attachment's `proxmox` block **only**
when the `Provider` is of type `proxmox`. The attachment's `vsphere` and
`libvirt` blocks are ignored for a Proxmox VM, so a vSphere VLAN or a libvirt
bridge in the same attachment never becomes a Proxmox NIC's tag or bridge. An
attachment with no `proxmox` block sends no bridge, model or VLAN to a Proxmox
VM: the provider's defaults apply (`virtio` on `vmbr0` for a new VM, the
template's NIC for a clone). The same rule holds the other way: a vSphere
Provider reads only the `vsphere` block, and a libvirt Provider only the
`libvirt` block.

Three things the manager does not send, so they have no effect on a Proxmox VM:

- `VMNetworkAttachment.spec.network.proxmox.firewall`, `rateLimit` and `mtu`.
- `VMNetworkAttachment.spec.ipAllocation`. A static IP is set on the
  `VirtualMachine`'s entry, not on the attachment.
- `spec.networks[].macAddress`. The manager drops it before the provider sees
  it (ADR-0014 Slice 0b, which comes with a policy for tenant-set MACs), so a
  VM's MAC is the one PVE draws. (The provider does write a MAC an attachment
  carries; it just never arrives yet.)

An entry with no `networkRef` sends only its name and static IP fields: it
names no bridge, model or VLAN.

The `VMNetworkAttachment` CRD currently limits a Proxmox `bridge` to `vmbr0`,
`vmbr1`, ... (`^vmbr[0-9]+$`). The provider itself accepts any Linux bridge name
(see [What is refused](#what-is-refused)), so an SDN VNet is not reachable
through the CRD yet. Relaxing the CRD pattern to the provider's is a tracked
follow-up (ADR-0014).

## A new VM

A VM that is not cloned from a template (created without a disk, or from a
migration's imported disk) gets one NIC per entry, `net0`, `net1`, and so on,
in the order of `spec.networks`:

```
net0: <model>,bridge=<bridge>[,tag=<vlan>]
```

A model or bridge the entry leaves empty is `virtio` or `vmbr0`. A request with
no networks at all gets one `virtio` NIC on `vmbr0`.

## A VM cloned from a template

The clone's NICs are the template's, with a new MAC each (PVE draws the MACs
when it clones). The provider changes only a NIC an entry **names**, meaning
it carries a bridge, a model, a VLAN tag or a MAC. Because the CRD defaults
`model` to `virtio`, every entry whose `networkRef` is an attachment with a
`proxmox` block names its NIC.

Entries are matched to the template's NICs **by index**: entry 0 to `net0`,
entry 1 to `net1`, and so on. The `netN` keys of the template need not be
contiguous.

- **An entry that names nothing about its NIC** (no `networkRef`, so only a
  name and perhaps a static IP) leaves that NIC as the template made it,
  bridge, VLAN, options and the MAC PVE gave the clone.
- **An entry that names a NIC** is merged into the clone's NIC at the same
  index. The model is the entry's. The clone's MAC and its other options
  (`firewall`, `rate`, ...) are kept; the provider does not draw a second MAC
  and does not write `mac=` (PVE has no such key). The options PVE accepts on
  a `virtio` NIC only (`mtu`, `queues`, `host-tunnel`) are kept while the model
  is `virtio` and dropped when the entry changes it to another model. A bridge
  the entry names replaces the template's, and with it the VLAN: the entry's VLAN
  tag is set, or, with none, the template's tag is removed. An entry with a VLAN
  tag and no bridge changes only the tag. A template NIC with no bridge at all
  (user-mode networking) that an entry names only by model gets `bridge=vmbr0`.
- **An entry with no NIC at its index in the template** adds a NIC (`virtio` on
  the named bridge, or `vmbr0`). With a template that has `net0` and `net2` and a
  spec of two entries, entry 0 changes `net0`, entry 1 adds `net1`, and `net2` is
  kept: the VM has three NICs.
- **A template NIC no entry maps to** (an index beyond the spec's entries, or
  one the spec does not list) is untouched, so a clone has at least the
  template's NICs. A spec cannot remove one.

The model deserves care: an attachment that names only a bridge still carries the
CRD's default model `virtio`, so a template NIC of another model (`e1000`) becomes
`virtio`. Set `model` on the attachment to the template's model to keep it.

This runs for every clone, with or without user-data (the old overwrite
happened only for clones with user-data or SSH keys).

## IP settings

**A VM with user-data** (`spec.userData`) gets a NoCloud seed CD-ROM that
carries its user-data, and the IP settings travel in the seed's
`network-config`, not in PVE's cloud-init keys: the provider sets no
`ipconfigN` and no `nameserver` on such a VM. Each entry becomes an entry of
the network-config (version 2) for NIC `N`, matched by the MAC address PVE
gave that NIC and named `eth<N>`, as PVE's own cloud-init network data names
it: `dhcp4: true` without a static IP, else the address with its prefix and a
default route through the gateway. The DNS servers of all entries are given to
every NIC. See
[`docs/proxmox-cloud-init-storage.md`](proxmox-cloud-init-storage.md#network-config).

**A VM without user-data** gets each entry as its `ipconfigN`, PVE's cloud-init
network setting of NIC `N`, as before:

- With a static IP: `ip=<address>/<prefix>[,gw=<gateway>]`. It takes effect only
  when the VM has a PVE cloud-init drive (one its template has).
- Without one: `ip=dhcp` on every NIC of a new VM; a clone gets none (it keeps
  the template's). The prefix and gateway of such an entry are ignored.

The DNS servers of all entries, static or DHCP, become the VM's single
`nameserver` setting (PVE has no per-NIC DNS), each server once, in order. The
provider does not set `searchdomain`.

**IPv6.** The provider writes `ip6=` and `gw6=` for an IPv6 address, but the
`VirtualMachine` CRD does not let one reach it yet: `ipAddress` and `gateway` are
IPv4-only patterns and `prefix` is at most 32. IPv6 is for a caller that talks to
the provider directly.

## What is refused

A network the provider cannot apply is refused as `InvalidSpec` before any VM is
made. The message names the entry's index and the rule, never the value. The
manager treats it as a terminal error, so a new `Create` and a re-sent one
fail instead of creating a VM on the wrong network:

- the bridge is not a Linux bridge name (letters, digits, `.`, `_`, `-`, at
  most 15 characters, starting with a letter or digit; the CRD itself
  currently allows `vmbrN` only);
- the model is not one PVE knows (`virtio`, `e1000`, `e1000e`,
  `e1000-82540em`, `e1000-82544gc`, `e1000-82545em`, `rtl8139`, `vmxnet3`,
  `i82551`, `i82557b`, `i82559er`, `ne2k_pci`, `ne2k_isa`, `pcnet`);
- the VLAN tag is not 1 to 4094;
- a MAC is not a unicast, non-zero 48-bit address;
- a static IP is not a unicast address (not `0.0.0.0`, loopback or multicast),
  has **no prefix length** (1 to 32, or 1 to 128 for IPv6), or is written as
  `address/prefix` with a `prefix` that disagrees. The `VirtualMachine` CRD
  allows an `ipAddress` without `prefix`, and vSphere accepts it; Proxmox
  refuses it, because PVE's `ip=` needs a prefix;
- the gateway is not a unicast address of the same family as the static IP;
- a `dns` entry is not an IP address. A hostname, or a space-separated list, is
  refused; the list is comma-separated. This applies to a DHCP entry's `dns`
  too;
- there are more than 32 entries (a PVE VM has `net0` to `net31`);
- `networks_json` is not a JSON list of network attachments.

## Permissions of the PVE token

Applying a VM's networks needs privileges the provider did not need while it
ignored them. A token with the `Administrator` role, or the root user, is
unaffected. A narrowly scoped token needs, on the VM and its bridges:

- `VM.Config.Network` to set a `netN` (a new VM's NICs, and a clone's named NICs);
- `VM.Config.Cloudinit` to set `ipconfigN` and `nameserver` (static IP and DNS
  on a VM without user-data; a VM with user-data gets them through its seed,
  which the provider attaches over SSH);
- `SDN.Use` on `/sdn/zones/<zone>/<bridge>` (and
  `/sdn/zones/<zone>/<bridge>/<tag>` for a VLAN tag) for every bridge a VM is put
  on, for any token that is not root. PVE checks it for each `netN` it writes
  (`check_bridge_access`). A plain Linux bridge such as `vmbr0` sits in the
  `localnetwork` zone.

A token that worked before may now get a 403 when the provider configures a
clone. A clone that fails to configure is rolled back (the provider removes
it), and the `VirtualMachine` shows the provider's error.

Treat these `SDN.Use` ACLs as the control that decides which bridges and VLANs
the VirtualMachine authors in a namespace can reach: the provider applies
whatever bridge and tag the manager sends, as the token allows. A
Provider-level policy (`allowedBridges` and `allowedVLANs`) is tracked with
ADR-0014 Slice 0b.

## What is reported

`ListVMs` reports each NIC's bridge (as the network name) and MAC, read from the
`netN` values in the VM's config in NIC order. `Describe` still reports no NICs
(its response has no field for them), so a NIC's MAC is visible through
`ListVMs` only.

## Upgrading

VMs that relied on the accidental `vmbr0` default change on their next create or
clone. A VM already created is not modified. See
[Upgrading](upgrading.md#behavior-changes-that-are-visible-but-not-breaking).
