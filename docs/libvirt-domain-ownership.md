# libvirt domain ownership

> The vSphere provider follows the same ownership rule. For the model that both
> providers share and for the vSphere details, see
> [`vm-ownership.md`](vm-ownership.md).

A libvirt domain name is global to its host: every namespace (tenant) whose
`VirtualMachine`s land on a host shares one set of domain names, and every
host-side file of a VM is named after its domain. The libvirt provider used to
name each domain after the bare `VirtualMachine` name, so two `VirtualMachine`s
called `web` in different namespaces mapped to the same domain, `web`.

Before the ownership rule below, `Create` treated "a domain with that name
already exists" as success and returned the existing domain as the VM's ID. The
second tenant's `VirtualMachine` was then bound to the first tenant's domain, and
could power it off, reconfigure it, snapshot it, delete it, or run in-guest
commands on it through the guest agent. Even with the ownership rule, a shared
bare name let one tenant block another tenant's `web` on every host, and two
concurrent creates of `web` raced on the same staging files and disk.

## Domain names

**A new domain is named `<namespace>.<name>`** — `VirtualMachine` `web` in
namespace `team-a` becomes domain `team-a.web`. The name is unambiguous because
a Kubernetes namespace can't contain a `.`: the first `.` always ends the
namespace. Two namespaces' `web` VMs are two different domains, with two
different disks and seeds, and never block each other.

| Host-side object | Name |
|---|---|
| Domain | `<namespace>.<name>` (the VM's `status.id`) |
| Primary disk | `<pool>/<namespace>.<name>-disk.qcow2` |
| Migration landing disk | `<pool>/<namespace>.<name>-migrated.qcow2` |
| Clone | `<target namespace>.<target name>`, disk `<…>-disk.qcow2` |
| Cloud-init seed | `/tmp/virtrigaud-cloudinit-<namespace>.<name>.<random>/cloud-init.iso` |
| Staged domain XML | `/tmp/<namespace>.<name>-domain.xml.<random>` (removed after `virsh define`) |

- **Length.** `<namespace>.<name>` is at most 200 bytes. A longer one (a
  namespace can have 63 bytes and a name 253) keeps its first bytes, including
  the whole namespace, and ends with `-` and the first 8 hex digits of
  `sha256("<namespace>/<name>")`. The shortened name is stable across retries
  and distinct for two long names that share a prefix. The bound keeps every
  file name above within the 255-byte file-name limit.
- **One rule.** The provider derives every one of these names with a single
  function (`domainNameFor`). `Create` (single-host and clustered) names the
  domain from `CreateRequest.owner`. `Clone` names it from
  `CloneRequest.target_vm`, and a migration import names its landing disk from
  `ImportDiskRequest.target_vm` (both additive proto fields that carry the target
  `VirtualMachine`'s namespace and name). The manager never derives a name
  itself. It records the domain name `Create`/`Clone` returns as `status.id` and
  hands the landing path `ImportDisk` returns to the target VM unchanged. So the
  file the import lands is exactly the file the VM's `Create` attaches in place.
- **Existing VMs keep their names.** Every operation after `Create` addresses
  the domain by `status.id`, so a VM created before this change (domain `web`)
  keeps working unchanged and is never renamed.
- **Older managers.** A request without an owner or target VM (a manager older
  than the provider) keeps the legacy bare name, and all the rules below still
  apply to it.

External tooling that assumed "domain name == `VirtualMachine` name" (for
example `virsh` scripts, monitoring labels, backup jobs) must read the VM's
`status.id` instead.

## Rule

**The provider never binds a `VirtualMachine` to a domain unless it can prove
that `VirtualMachine` created the domain.**

1. **The owner goes on the wire.** Every `Create` carries
   `CreateRequest.owner` (`ObjectIdentity{uid, namespace, name}`, proto field
   11). The manager fills it from `vm.UID`, `vm.Namespace` and `vm.Name`. Only
   the UID authorizes a bind. The namespace and name name the domain
   (`<namespace>.<name>`, see [Domain names](#domain-names)) and are recorded for
   audit and diagnostics; they never authorize anything.
2. **The owner is stamped on create.** A new domain gets a metadata element
   inside `<metadata>`:

   ```xml
   <metadata>
     <virtrigaud:owner xmlns:virtrigaud="https://virtrigaud.io/xmlns/libvirt/owner/v1"
                       uid="…" namespace="…" name="…"/>
   </metadata>
   ```

   Every value is XML-escaped. The provider identifies the element by its
   namespace URI, never by its prefix. To inspect it, run
   `virsh metadata <domain> --uri https://virtrigaud.io/xmlns/libvirt/owner/v1`.
3. **Creates fail closed on an existing domain.** When a domain with the
   requested (namespaced) name already exists, the provider handles the create
   as follows. With namespaced names this is normally the VM's own domain (a
   retry); another domain can only have that exact name if it is a previous
   incarnation of the same `VirtualMachine` (deleted without its domain and
   re-created), a legacy VM literally named `<namespace>.<name>`, or a
   hand-made domain.

   | Existing domain | Result |
   |---|---|
   | Carries the requester's UID | Idempotent success. This covers a create that is retried because the manager lost its `status.id` write. |
   | No owner stamp: not created by VirtRigaud, or created before this change | **Refused** |
   | Carries a different UID | **Refused** |
   | Owner stamp can't be read | **Refused** |
   | Request carries no owner (a manager older than the provider) | **Refused** |

   A refusal is a non-retryable `Conflict`, sent over gRPC as
   `codes.AlreadyExists`. If no domain with that name exists, the provider
   creates it normally, even when the request has no owner.
4. **Ambiguous names are rejected.** virsh resolves a domain argument first as a
   numeric ID, then as a UUID, and only then as a name. A domain named `12` or
   `1b4e28ba-2fa1-11d2-883f-0016d3cca427` would therefore let later operations
   hit a different domain. A namespaced name always contains a `.`, so it can
   never look like an ID or a UUID (`team-a.12` is fine). Only a legacy bare
   name (a request from an older manager) can be ambiguous; the provider rejects
   it at `Create` and `Clone` time with `InvalidSpec` (`codes.InvalidArgument`)
   and runs no virsh command. It also rejects a namespace or name that is not a
   DNS-1123 label or subdomain, and a request whose owner does not name the VM.
   `VirtualMachine` CRD validation doesn't change, because other providers share
   it.
5. **Domain UUIDs are unpredictable.** New domains get an RFC 4122 v4 UUID from
   `crypto/rand`. libvirt refuses to define a domain whose name already exists
   under a different UUID. As a result, if two creates for the same name race,
   the second fails at `virsh define` and can't redefine the first domain.
6. **Clones start with no owner.** A clone is named
   `<target namespace>.<target name>` and never binds to or redefines an
   existing domain with that name. The provider also removes the source VM's
   owner stamp from the cloned XML, so the clone doesn't claim the source's
   owner.
7. **No disk is written over another domain's disk.** Before `qemu-img` writes
   a VM's disk (`Create` from an image, `Clone`) or a migration's landing disk
   (`ImportDisk`), the provider checks the target path. If a file is there and
   any domain on the host uses it (as a disk or backing file), the operation is
   refused with `Conflict` and the file is untouched. A file no domain uses can
   only be left over from an earlier failed attempt for this very domain name
   (same namespace and name), so it is overwritten.

The ownership and naming rules apply to both single-host providers and clustered
providers (ADR-0007 `topology: cluster`). Both paths share the same create core.
On a clustered provider, the finalizer's cleanup of a create that is still in
flight (no `status.id` yet) addresses the VM by its bare name; the provider also
looks up the namespaced domain it would have created, under the same owner
check, so that domain doesn't leak.

## Staging on the host

`Create` stages files on the hypervisor host before `virsh define`: the domain
XML, and the cloud-init seed (user-data, which may carry secrets, meta-data and
the seed ISO). Each create gets its own:

- The domain XML goes to `/tmp/<domain>-domain.xml.<random>` and the seed to a
  directory `/tmp/virtrigaud-cloudinit-<domain>.<random>/`. Both are made by
  `mktemp` on the host: created exclusively, with an unpredictable suffix, mode
  `0600`/`0700`, directly in the sticky `/tmp`. Nothing else on the host can
  pre-create, predict, replace or rename them, and two concurrent creates never
  share one.
- `user-data` and `meta-data` stay private to the SSH user. Once the ISO is
  built, the seed directory is set to `0711`, so the qemu process can open the
  ISO by its exact path but nobody can list the directory.
- The staged domain XML is removed after `virsh define`, whether it succeeds or
  not. The seed directory is removed if the create fails. A created domain keeps
  its seed (its CD-ROM references the ISO), and `Delete` removes it. Domains
  created before this change keep their seed under
  `/tmp/virtrigaud-cloudinit/<name>/`, which `Delete` still finds from the domain
  XML.

## Adoption

The adoption flow (`virtrigaud.io/adopt-vms` on the Provider) creates a
`VirtualMachine` for each domain no `VirtualMachine` manages. A namespaced domain
name such as `team-a.web` is a valid Kubernetes object name, so a domain of any
name can still be adopted.

`ListVMs` reports each domain's owner stamp (`provider_raw["owner_uid"]`,
read from the definition it already fetches). Adoption **never adopts a domain
stamped with the UID of a `VirtualMachine` that still exists**, in any namespace
and through any Provider object. This covers a domain whose create is still in
flight (its `status.id` is not written yet, and the operator can't derive a
namespaced name) and a domain managed through another Provider object that
points at the same host. A domain stamped only by a `VirtualMachine` that was
deleted (its domain left behind) is adoptable; the adopted `VirtualMachine` is a
new object with a new UID.

## What operators see

A refused create leaves `status.id` empty. The `VirtualMachine` is therefore
bound to nothing, and deleting it never calls the provider's `Delete`, so the
other domain is untouched. The VM shows:

- `Ready=False` and `Provisioning=False` with reason **`ProviderConflict`**, for
  an ownership refusal, or **`ValidationError`**, for an ambiguous name.
  `observedGeneration` is set.
- A condition message that names only the requested domain. It never discloses
  which namespace or VM owns the domain, because that belongs to another tenant.
  The provider log records those details for operators.
- A re-check every **2 minutes** (every 30 seconds for an invalid-spec rejection such as an ambiguous name), instead of the 5-second transient-error retry.
- The manager metric `virtrigaud_errors_total{reason="provider-create-rejected"}`.
  A rising count means tenants are colliding on names, or someone is probing
  for names.

To resolve a refused create, do one of the following:

- **Adopt the domain** if it should be managed. Use the adoption flow: set the
  `virtrigaud.io/adopt-vms` annotation on the Provider (see
  `examples/vm-adoption-example.yaml`). Adopted VMs get `status.id` directly and
  never go through `Create`.
- **Remove the domain** if it is stale. The next re-check creates the VM.
- **Rename the `VirtualMachine`.** Names are immutable, so you must recreate it
  under a different name.

## Upgrade notes

- **New libvirt VMs are named `<namespace>.<name>` on the host.** VMs that
  already exist keep their domain names: every operation after `Create` uses
  `status.id`, and nothing is renamed. External tooling that assumed "domain
  name == `VirtualMachine` name" must read `status.id` instead.
- VMs that are already bound (with `status.id` set) are **unaffected**. After a
  VM is bound, every operation uses `status.id` and never calls `Create` again.
- Domains that existed before this change carry no owner stamp. A create that
  collides with one of them now fails with `ProviderConflict`. Pre-existing
  domains are never bound automatically.
- One edge case: the provider created a domain before the upgrade, and the
  manager lost the `status.id` write for it. After the upgrade, that VM's
  retried create is refused. Adopt the domain to recover.
- The manager and the provider can be upgraded in either order. An older
  provider ignores `owner` and `target_vm` and keeps the old (bare) names. A
  newer provider behind an older manager keeps the bare names too, and never
  binds an existing domain. A migration whose import ran on one version and
  whose create runs on another may find the landing disk under the other
  naming; the create then refuses the disk (`ValidationError`). Re-run the
  migration after both components are upgraded.
