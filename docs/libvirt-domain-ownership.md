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
| Cloud-init seed | `<seed dir>/virtrigaud-cloudinit-<namespace>.<name>.<random>/cloud-init.iso` (see [Cloud-init seed directory](#cloud-init-seed-directory)) |
| Staged domain XML | `/tmp/<namespace>.<name>-domain.xml.<random>` (removed after `virsh define`) |

- **Length.** `<namespace>.<name>` is at most 200 bytes. A longer one (a
  namespace can have 63 bytes and a name 253) keeps its first bytes, including
  the whole namespace, and ends with `_` and the first 16 hex digits of
  `sha256("<namespace>/<name>")`. The shortened name is stable across retries
  and distinct for two long names that share a prefix. `_` can't appear in a
  Kubernetes name, so no `VirtualMachine` can be named to land on another VM's
  shortened name. The bound keeps every file name above within the 255-byte
  file-name limit.
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
  keeps working unchanged and is never renamed. A VM whose create defined the
  bare-named domain before the upgrade but whose `status.id` was never recorded
  binds that domain on its retry — only if the domain's owner stamp records the
  VM's UID — instead of creating `<namespace>.<name>` next to it. A bare-named
  domain that is not the VM's (another namespace's, or unstamped) never blocks
  the namespaced create.
- **Older managers.** A request without an owner or target VM (a manager older
  than the provider, or a direct gRPC caller) keeps the legacy bare name, and all
  the rules below still apply to it. A legacy name must be a DNS-1123 subdomain
  **without** a `.`, so such a request can never create (or squat) a name of the
  namespaced form. A `VirtualMachine` whose name contains a `.` therefore needs
  a manager that sends the owner (#333 or later).

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
   DNS-1123 label or subdomain, a legacy name that contains a `.`, and a request
   whose owner does not name the VM.
   `VirtualMachine` CRD validation doesn't change, because other providers share
   it.
5. **Domain UUIDs are unpredictable.** New domains get an RFC 4122 v4 UUID from
   `crypto/rand`. libvirt refuses to define a domain whose name already exists
   under a different UUID. As a result, if two creates for the same name race,
   the second fails at `virsh define` and can't redefine the first domain.
6. **A clone never inherits the source's owner.** A clone is named
   `<target namespace>.<target name>`. The provider removes the source VM's
   owner stamp from the cloned XML, so the clone doesn't claim the source's
   owner. The target VirtualMachine exists before the clone, on a single-host
   provider (rc2 R4) as on a clustered one (ADR-0007 Addendum A, slice 3), and
   the clone is **stamped with the target's identity** (uid included) by the
   define that creates it. An existing domain of the clone's name is reported
   as the clone only when that stamp is the target's (a re-sent `Clone` whose
   answer or record was lost), whatever the source's state is by then; any
   other domain of that name (unstamped, another VM's, an unreadable stamp) is
   refused with `AlreadyExists` before anything is read for the copy, and is
   never bound or redefined. The single-host provider holds the clone's
   domain-name lock from that check to the define, so a re-send that arrives
   while an earlier attempt still copies waits for it. A request without a uid
   (a manager older than rc2) clones unstamped, and any domain of the clone's
   name refuses it. The clustered clone also reads the source only after
   checking the source VM's own stamp.
7. **No disk is written over another domain's disk.** Before `qemu-img` writes
   a VM's disk (`Create` from an image, `Clone`) or a migration's landing disk
   (`ImportDisk`), the provider checks the target path. If a file is there and
   any domain on the host uses it (as a disk or backing file), the operation is
   refused with `Conflict` and the file is untouched. A file no domain uses can
   only be left over from an earlier failed attempt for this very domain name
   (same namespace and name), so it is overwritten.
8. **Only a granted namespace can have names derived from it.** A `VMClone`
   or `VMMigration` names its clone or landing disk for its target namespace.
   The manager sends that namespace only when it is the object's own namespace
   or the target namespace lists the object's namespace in its
   `infra.virtrigaud.io/allowed-source-namespaces` annotation. Otherwise a
   tenant in `team-a` could make a migration land
   `team-b.<name>-migrated.qcow2`. Without the grant, no `Clone` or
   `ImportDisk` is sent. See
   [`cross-namespace-targets.md`](cross-namespace-targets.md).

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
  directory `<seed dir>/virtrigaud-cloudinit-<domain>.<random>/` in the
  [cloud-init seed directory](#cloud-init-seed-directory). Both are made by
  `mktemp` on the host: created exclusively, with an unpredictable suffix, mode
  `0600`/`0700`. Nothing else on the host can pre-create, predict, replace or
  rename them, and two concurrent creates never share one.
- The seed is private to the SSH user and then to the VM's qemu process; see
  [Cloud-init seed directory](#cloud-init-seed-directory).
- A URL image is downloaded to a per-download `mktemp` file in the same staging
  directory (`<domain>-disk-temp.img.<random>`), which is removed after the
  convert, so nothing can swap it between the header check and the convert.
- The staged domain XML is removed after `virsh define`, whether it succeeds or
  not. The seed directory is removed if the create fails and the domain is known
  not to exist. When `virsh define` reports an error, the provider checks
  `virsh domuuid` against the UUID it generated: if the domain exists with that
  UUID (the define succeeded and only its reply was lost), the create succeeds;
  if the check itself fails, the seed is kept, because the domain may reference
  it. A created domain keeps its seed (its CD-ROM references the ISO), and
  `Delete` removes it. Domains created by earlier releases keep their seed where
  it is (`/tmp/virtrigaud-cloudinit-<domain>.<random>/`, or
  `/tmp/virtrigaud-cloudinit/<name>/` before that), which `Delete` still finds
  from the domain XML.
- **Clones and the seed.** A clone (single-host and clustered, R26) gets a seed
  of its own: the source's seed files are read (through the SSH user's private
  copy once the source has started) and a new seed is built from them with the
  clone's identity, in a fresh seed directory in the seed directory
  (`<seed dir>/virtrigaud-cloudinit-<clone domain>.<random>/cloud-init.iso`,
  private exactly like a create's), and the clone's CD-ROM is re-pointed at it,
  so deleting either VM never removes the other's seed. The new seed is removed
  if the clone's define fails and the domain is known not to exist. Only a
  single-host clone made before R26 still references its source's seed. The
  clone boots with the **source's user-data** unless its `VMClone` gives its
  own, provisioning secrets included; a cross-namespace clone hands
  them to the target namespace (see
  [`cross-namespace-targets.md`](cross-namespace-targets.md#a-clone-carries-the-sources-cloud-init-seed)).

### Cloud-init seed directory

A seed ISO carries the VM's user-data, which may hold passwords, keys and
tokens. Since R27 the provider builds seeds in a dedicated directory that only
the provider's SSH user can list, and makes every seed file private:

- **Where.** `VIRTRIGAUD_LIBVIRT_SEED_DIR` on the provider pod (set it through
  the Provider's `spec.runtime.env`), default
  **`/var/lib/libvirt/virtrigaud-seeds`**. One absolute directory; it applies
  to every host of a clustered Provider. A malformed value, or one in `/tmp`,
  `/var/tmp`, `/run`, `/dev`, `/proc` or `/sys` (emptied at boot: a VM whose
  CD-ROM file is gone no longer starts), fails provider start-up.
- **Ownership and mode (fail closed).** Before it writes anything for a create,
  or for a clone (single-host or clustered) of a VM that has a seed, the provider checks the directory on the target host: it
  must exist (the provider creates it with mode `0711` when it is missing and
  the SSH user may, for example when the SSH user is root), be owned by the
  SSH user, and have **no read or write bit for its group or others**. Every
  directory above it — along the configured path and along its canonical path,
  so a symbolic link on the way is covered too — must be owned by root or the
  SSH user and **not writable by its group or others unless it is sticky**
  (another account could otherwise rename the seed directory, or a directory on
  its path, away and plant its own between the check and the write). A
  directory that fails the check is refused: the create (or the clone of a VM that has a seed) fails with
  a retryable error, `refusing to stage cloud-init data`, and resumes once the
  host is fixed. The provider log names the directory and the problem. A
  single-host provider also checks it at startup and logs a `WARN`.
- **Mode `0711`, not `0700`,** unless qemu runs as the SSH user or root: the
  qemu user must traverse the directory to open the ISO by its exact path. A
  single-host provider logs a `WARN` at startup when others cannot traverse
  it (no `o+x`); it does not refuse it.
- **Host-local, one per SSH user.** Keep the seed directory on a local
  filesystem, never on NFS or other shared storage: libvirt hands the ISO to the
  qemu user with `chown`, which a root-squashed export refuses, and the
  cleanup's check that no domain references a seed only sees the domains of
  its own host. On a host shared by several Providers with **different SSH
  users**, give each its own directory — the default is owned by one SSH user,
  and every other Provider's creates fail closed on it. Set
  `VIRTRIGAUD_LIBVIRT_SEED_DIR` per Provider in its `spec.runtime.env`, for
  example `/var/lib/libvirt/virtrigaud-seeds-<ssh user>`, and create each one
  owned by its SSH user. Providers that use the same SSH user may share one.
- **Every host that creates VMs needs it,** including the target host of a
  `VMMigration` into libvirt (the migrated VM's create builds its seed there) and
  every host of a clustered Provider.
- **Inside each seed directory** (`mktemp -d`, `0700` while it is built): the
  ISO is created under umask `0077` and then set to `0600` explicitly;
  `user-data` and `meta-data` are removed as soon as the ISO is built; a second
  copy of the ISO, `cloud-init.private.iso` (`0600`), stays with the SSH user
  (see clones below); only then is the directory opened to `0711`. No domain
  ever references the private copy, and `Delete` removes it with the directory.
  A failure at any of these steps, or a create cancelled while the ISO is
  built, removes the seed's directory again. After a successful create the
  provider also removes older seed directories of the same domain name that no
  domain references (left by an earlier attempt), once they are more than five
  minutes old.
- **How qemu reads a `0600` ISO.** At domain start, libvirt's DAC driver gives
  every disk source to the qemu user (`dynamic_ownership = 1` in
  `/etc/libvirt/qemu.conf`, libvirt's default and Ubuntu's): the ISO becomes
  `libvirt-qemu:kvm 0600`. libvirt does not hand a read-only disk back at
  shutdown, so it stays the qemu user's. With `dynamic_ownership = 0` a VM
  whose qemu runs as another user cannot open its seed: keep the default.
- **Clones (reading an existing seed).** Once its domain has started, a
  seed's `cloud-init.iso` belongs to the qemu user, so whenever the provider
  reads an existing seed as its SSH user — a clone copying it, or reading the
  files inside it — it reads the SSH user's `cloud-init.private.iso` next to
  it instead — in a per-create seed directory, and in an earlier release's
  per-name `/tmp/virtrigaud-cloudinit/<name>/` too. A seed of an earlier release
  has no private copy until you add one ([upgrading](upgrading.md#leftovers-from-earlier-releases));
  until then its ISO, which kept the SSH user's umask, is read itself. The
  provider never reads a seed as root, and no other local user can read either
  file.
- **AppArmor (Ubuntu).** No profile change is needed. `virt-aa-helper` adds
  every disk source of a domain, CD-ROMs included, to that domain's own
  profile, and its own profile may read any `*.iso` file. It refuses only
  system paths (`/etc`, `/usr`, `/boot`, `/dev`, ...). The default lives under
  `/var/lib/libvirt`, next to the rest of the host's VM state, on a persistent
  filesystem.
- **It holds secret material.** Every seed carries its VM's user-data in
  plaintext (inside the ISO) for as long as the VM exists. Exclude the seed
  directory from host backups, or make sure those backups are encrypted and
  access-controlled like the secrets themselves, and prefer short-lived
  bootstrap credentials in user-data (a one-time token that fetches the real
  secrets) over long-lived ones.
- **SELinux: not tested.** The seed directory was run on Ubuntu 24.04 with AppArmor
  (libvirt 10.0). On an SELinux host libvirt relabels a domain's disk sources at
  start, and a directory made below `/var/lib/libvirt` takes that directory's
  label, but VirtRigaud has not been run there; if a seeded VM does not start,
  look for AVC denials on the seed directory.

Prepare each libvirt host once, as root (replace `virtrigaud` with the
provider's SSH user):

```bash
install -d -m 0711 -o virtrigaud -g virtrigaud /var/lib/libvirt/virtrigaud-seeds
```

`Delete` finds a domain's seed in the seed directory and in `/tmp` (where
earlier releases built them). If you change `VIRTRIGAUD_LIBVIRT_SEED_DIR`
later, existing VMs keep their seed in the old directory and `Delete` no longer
removes it there: remove those directories by hand once their VMs are gone.

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
points at the same host.

A domain stamped only with UIDs of no `VirtualMachine` in this cluster is **not
adopted** unless the Provider also carries
`virtrigaud.io/adopt-stamped-vms: "true"` (single-host only). An owner stamp
only means something in the cluster that wrote it: the UID may belong to a
deleted `VirtualMachine` (a stale stamp), or to a live `VirtualMachine` of
another VirtRigaud installation, or of this cluster before a restore, that
manages the same host. Each such domain is logged and counted in the
Provider's adoption status message. With the opt-in it is adopted; the adopted
`VirtualMachine` is a new object with a new UID.

`ListVMs` also reports, under `provider_raw["owner_stamp_state"]`, a domain
whose owner stamp cannot be relied on: `unreadable` (a stamp that cannot be
parsed, repeats an attribute, or records no UID) or `multiple` (more than one
owner, which only a hand-edited definition has). Adoption never adopts such a
domain, on a single-host or a clustered provider. The single-host provider
reports it from the definition it lists; before this release it reported only
the readable UIDs, and adoption could take a domain whose stamp it could not
read.

The adopted domain is **handed to its `VirtualMachine`** before the
`VirtualMachine` is bound: `TransferOwner` writes the owner stamp (UID,
namespace and name) with `virsh metadata` — a check-and-set on the domain's
name, its UUID (the one `ListVMs` reported) and its current stamp, serialized
per host in the provider process — and reads it back from both definitions of
a running domain. On a **single-host** provider (which reports
`supportsOwnerTransfer`) this is what lets another Provider object's adoption
see the domain as owned, and replaces the stamp of a `VirtualMachine` that was
detached with `orphan-on-delete` when the domain is adopted again with
`adopt-stamped-vms`. A single-host provider still doesn't check owners on
per-VM calls. Two provider processes fronting the same host aren't serialized
against each other, so run one Provider per host endpoint. See
[Adoption stamps the adopted VM](vm-ownership.md#adoption-stamps-the-adopted-vm)
for which domains are stamped, the order, and an older provider. On a
**clustered** provider every routed call is owner-checked, so the same transfer
is what lets the adopting `VirtualMachine` reach its domain at all.
Until ADR-0007 A6.4 a clustered provider adopts **only unstamped** domains: a
domain stamped by a `VirtualMachine` that no longer exists (left by
`orphan-on-delete`, or by a restore that gave the `VirtualMachine` a new UID)
is a previous incarnation, skipped and named in the Provider's adoption status
with the hint to re-attach it per the A6 runbook or remove it. A domain stamped
for a live `VirtualMachine` is never touched. On a clustered provider domains
are identified by (host, domain) — the same name on two hosts is two
adoptions. See
[`clustered-provider-inventory.md`](clustered-provider-inventory.md#listing-and-adoption-slice-4).

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
- A create whose domain was defined before the upgrade and whose `status.id`
  write was lost binds that bare-named domain on the retry when it carries the
  VM's owner stamp (a provider with #333). A domain created **before** #333 has
  no stamp, so it can't be proven to be the VM's: the retry creates
  `<namespace>.<name>` and the unstamped bare-named domain is left behind. Adopt
  it or remove it.
- **Upgrade the manager first, then the libvirt provider.** A new manager with
  an older provider is fully compatible: the older provider ignores
  `target_vm` and keeps bare names for `Create`, `Clone` and the migration
  landing disk alike. The reverse is not: a manager from before this change
  that already sends `CreateRequest.owner` (#333 or later) makes a new provider
  name the domain `<namespace>.<name>`, but it sends no `target_vm` on
  `ImportDisk`, so the migration disk lands under the legacy name and the
  target VM's `Create` refuses it (`ValidationError`). Every libvirt-target
  migration fails while the provider is ahead of the manager, and clones get
  bare names in that window. (A manager older than #333 sends no owner at all,
  so a new provider keeps bare names everywhere for it.)
- Don't upgrade the provider while a libvirt-target `VMMigration` is between its
  import and its create: its landing disk is under the old name, and the create
  refuses it. Re-run the migration after the upgrade.
- **Remove leftover seed directories.** Before this change, a failed create over
  SSH never removed its seed directory `/tmp/virtrigaud-cloudinit/<name>/`
  (the cleanup ran in the provider pod, not on the host), so user-data
  (possibly with secrets) can be left in plaintext on the host. Remove every
  directory under `/tmp/virtrigaud-cloudinit/` that no domain references
  (compare with `virsh domblklist --details <domain>` for each domain). A
  directory a domain still references holds that VM's seed ISO; don't remove
  it, but you can delete the `user-data` and `meta-data` files next to the ISO,
  since the ISO carries their content.
