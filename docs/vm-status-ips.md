# VirtualMachine `status.ips`

`kubectl get vm` shows `status.ips` in its `IPs` column. It lists the VM's addresses
except loopback, unspecified, link-local and multicast ones. Every provider's answer goes
through the same filter in the manager before it is written, so the list means the same on
vSphere, libvirt, Proxmox and the mock provider.

## What is listed

The manager keeps an address if it is an IPv4 or IPv6 address that is none of:

| Dropped | Range |
|---|---|
| Loopback | `127.0.0.0/8`, `::1` |
| Unspecified | `0.0.0.0`, `::` |
| IPv4 link-local | `169.254.0.0/16` |
| IPv6 link-local | `fe80::/10` |
| Multicast | `224.0.0.0/4`, `ff00::/8` |

Private addresses (RFC 1918), IPv6 unique-local addresses (`fc00::/7`, for example
`fd00::/8`) and global addresses are kept. An entry that is not an IP address (an
empty string, a host name, an address with a prefix length such as `192.0.2.5/24`)
is dropped. Each kept address is written in its canonical form (an IPv4-mapped IPv6
address as the IPv4 address it carries, IPv6 in lower case and compressed), a
duplicate is dropped, and the order is the order the provider reported them in, so
the first address is the one the provider lists first.

For example, a libvirt guest whose agent reports

```text
127.0.0.1, ::1, 192.0.2.31, fe80::5054:ff:fe88:d0c1
```

shows

```yaml
status:
  ips:
    - 192.0.2.31
```

`status.ips` is empty (absent) while the provider has no reachable address to
report. An address that is not listed also does not count as "has an IP" for the
manager's poll interval or for the `virtrigaud_ip_discovery_duration_seconds`
metric.

## Behaviour change for anything that parses `status.ips`

Up to v0.4.0-rc7, a libvirt VM whose QEMU guest agent runs listed every address of
every guest interface, loopback and link-local addresses included. Tooling that took
`status.ips[0]`, or that matched an entry such as `127.0.0.1` or `fe80::...`, no
longer finds it. The next reconcile of each VM rewrites its `status.ips`; no
`VirtualMachine` spec change is needed. There is no CRD schema change (only the
description of the field).

Where the manager also writes the field (VM adoption), the same filter applies.

## Where each provider gets the addresses

| Provider | Source | When it shows an address |
|---|---|---|
| vSphere | VMware Tools (`guest.ipAddress` and `guest.net`) | Tools run in the guest |
| Proxmox | QEMU guest agent (`network-get-interfaces`) | The VM is running and the agent answers |
| libvirt | The QEMU guest agent, with `virsh domifaddr` as the fallback (below) | The guest agent answers, or a fallback source has a row |

### libvirt

The provider asks for addresses in two steps (`getDomainIPAddresses` and
`parseDomifaddr` in `internal/providers/libvirt/virsh.go`, `describeOn` in
`internal/providers/libvirt/provider_virsh.go`).

1. `virsh domifaddr <domain> --source agent`, then `--source lease` (libvirt's DHCP
   leases) and `--source arp` (the host's ARP table entries for the domain's MAC
   addresses), in that order. The first source that yields a reachable address
   ends the search, and a source that fails (no agent in the guest) is skipped.
   Every row of the table counts, whatever its interface name: the agent names
   rows after the guest's interfaces (`eth0`, `lo`), while `lease` and `arp` name
   them after the NIC's host-side device (`vnet7`); a NIC with several addresses
   prints the others on rows whose name is `-`. The prefix length (`/24`, and
   `/0` from `arp`) is removed, and the rules above decide by address what is
   kept, so `lo` and link-local addresses never show.
2. While the domain is running, the provider also queries the guest agent
   (`guest-network-get-interfaces`, at most 16 interfaces and 16 addresses per
   interface, bounded by a few seconds). If that yields a reachable address, it
   **replaces** the list from step 1.

Which network the guest is on decides what is visible without the guest agent:

- **libvirt's NAT `default` network (or any libvirt-managed network):** libvirt runs
  the DHCP server, so the DHCP lease is shown without the agent. A guest with
  several NICs on such networks shows the lease of each.
- **A host bridge (`VMNetworkAttachment` `spec.network.libvirt.bridge`):** the guest
  gets a `<interface type='bridge'>` and the LAN's DHCP server answers, not
  libvirt, so there is **no lease**. The address is shown if the QEMU guest agent
  runs in the guest. Without it, `status.ips` is empty unless the host happens to
  have an ARP entry for the guest's MAC address (the `arp` source, a last resort
  that depends on recent traffic between the host and the guest).
- **No network attachment at all:** the VM gets a user-mode (`type='user'`)
  interface; libvirt holds no lease for it either, so it needs the agent too.

So the QEMU guest agent is the source that works on every network type. The
default cloud-init the provider injects installs and starts `qemu-guest-agent`
(see the README), and the provider adds the agent channel to every domain it
defines; an image or a `spec.userData` that does not install and start the agent
has none.

Up to v0.4.0-rc7 the rows of the `lease` and `arp` sources were all dropped
(the provider kept only rows whose interface name started with `e`, and these
sources name the rows `vnetN`), so a guest on the NAT `default` network without the
guest agent showed no address even though libvirt had its lease.
