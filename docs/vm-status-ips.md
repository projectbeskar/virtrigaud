# VirtualMachine `status.ips`

`kubectl get vm` shows `status.ips` in its `IPs` column. It lists the addresses a
user can reach the VM on, and nothing else. Every provider's answer goes through
the same filter in the manager before it is written, so the list means the same on
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

The provider asks for addresses in two steps (`getDomainIPAddresses` in
`internal/providers/libvirt/virsh.go` and `describeOn` in
`internal/providers/libvirt/provider_virsh.go`).

1. `virsh domifaddr <domain> --source agent`, then `--source lease` (libvirt's DHCP
   leases) and `--source arp`, in that order. The first source that yields a row
   ends the search. Only rows whose interface name starts with `e` (`eth0`,
   `ens3`, `enp1s0`) are kept; `lo`, `127.0.0.1`, `::1` and `fe80:` rows are skipped.
2. While the domain is running, the provider also queries the guest agent
   (`guest-network-get-interfaces`, at most 16 interfaces and 16 addresses per
   interface, bounded by a few seconds). If that yields an address, it **replaces**
   the list from step 1. Its addresses are filtered with the rules above, which is
   what removes loopback and link-local addresses the agent reports for `lo` and
   for every interface.

So the QEMU guest agent is the source that works on every network type. The
default cloud-init the provider injects starts `qemu-guest-agent`
(see the README); an image or a `spec.userData` that does not install and start it
has none.

**A guest on a bridge shows no address unless the guest agent runs in the guest.**
A VM attached through a `VMNetworkAttachment` with `spec.network.libvirt.bridge`
gets a `<interface type='bridge'>`. libvirt does not run the DHCP server on a
host bridge (the LAN's does), so it holds no lease for the guest and `--source
lease` has nothing to report. A VM created with no network attachment at all gets a user-mode
(`type='user'`) interface, which has no libvirt lease either. In both cases a guest
without a running agent has an empty `status.ips`, although it may well have an
address; install and start `qemu-guest-agent` in the guest (the cloud-init
package `qemu-guest-agent`, the service `qemu-guest-agent`).

The guest agent also needs the VM's agent channel, which the provider adds to every
domain it defines.
