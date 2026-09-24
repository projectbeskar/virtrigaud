# libvirt domain ownership

> The vSphere provider follows the same ownership rule. For the model that both
> providers share and for the vSphere details, see
> [`vm-ownership.md`](vm-ownership.md).

The libvirt provider names each domain after the bare `VirtualMachine` name. The
namespace is not part of the name. If several namespaces (tenants) share a
libvirt host, two `VirtualMachine`s called `web` in different namespaces map to
the same domain name, `web`.

Before this change, `Create` treated "a domain with that name already exists" as
success and returned the existing domain as the VM's ID. The second tenant's
`VirtualMachine` was then bound to the first tenant's domain, and could power
it off, reconfigure it, snapshot it, delete it, or run in-guest commands on it
through the guest agent.

## Rule

**The provider never binds a `VirtualMachine` to a domain unless it can prove
that `VirtualMachine` created the domain.**

1. **The owner goes on the wire.** Every `Create` carries
   `CreateRequest.owner` (`ObjectIdentity{uid, namespace, name}`, proto field
   11). The manager fills it from `vm.UID`, `vm.Namespace` and `vm.Name`. Only
   the UID authorizes a bind. The namespace and name are there for audit and
   diagnostics.
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
   requested name already exists, the provider handles the create as follows:

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
   hit a different domain. The libvirt provider rejects these names at `Create`
   and `Clone` time with `InvalidSpec` (`codes.InvalidArgument`) and runs no
   virsh command. `VirtualMachine` CRD validation doesn't change, because other
   providers share it.
5. **Domain UUIDs are unpredictable.** New domains get an RFC 4122 v4 UUID from
   `crypto/rand`. libvirt refuses to define a domain whose name already exists
   under a different UUID. As a result, if two creates for the same name race,
   the second fails at `virsh define` and can't redefine the first domain.
6. **Clones start with no owner.** A clone never binds to or redefines an
   existing domain with the target name. The provider also removes the source
   VM's owner stamp from the cloned XML, so the clone doesn't claim the source's
   owner.

The ownership rule applies to both single-host providers and clustered providers
(ADR-0007 `topology: cluster`). Both paths share the same create core.

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

- VMs that are already bound (with `status.id` set) are **unaffected**. After a
  VM is bound, every operation uses `status.id` and never calls `Create` again.
- Domains that existed before this change carry no owner stamp. A create that
  collides with one of them now fails with `ProviderConflict`. Pre-existing
  domains are never bound automatically.
- One edge case: the provider created a domain before the upgrade, and the
  manager lost the `status.id` write for it. After the upgrade, that VM's
  retried create is refused. Adopt the domain to recover.
- The manager and the provider can be upgraded in either order. An older
  provider ignores `owner` and keeps the old behavior. A newer provider behind
  an older manager never binds an existing domain.
