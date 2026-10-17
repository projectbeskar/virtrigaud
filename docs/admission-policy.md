# Reserved `virtrigaud.io` keys (admission policy)

The VirtRigaud manager writes some annotations and labels on the
`VirtualMachine`s it creates or adopts: where a migrated VM came from, which
`VMClone` produced a VM, that a VM was adopted from a hypervisor, and so on.
Without a guard, anyone who can create or edit a `VirtualMachine` can write
the same keys.

**For adoption and clones this policy is a security control, not a
convenience.** Some controllers still decide which `VirtualMachine` a
hypervisor VM is bound to from these keys:

- **Single-host adoption** creates the adopting `VirtualMachine` first,
  labelled `virtrigaud.io/adopted=true` and annotated
  `virtrigaud.io/adopted-id` with the hypervisor VM's id, and then binds
  that id (and stamps the VM with its new owner) only to a waiting
  `VirtualMachine` that carries both, under the matching name. A waiting
  adopter it can no longer bind is deleted, but only if it carries
  `virtrigaud.io/adopted-id`. One that an older manager created and couldn't
  bind is recognized by the label plus `spec.importedDisk` with source
  `manual`.
- **Clustered adoption** recognizes the `VirtualMachine` waiting for a domain
  by `virtrigaud.io/adopted-host` and `virtrigaud.io/adopted-id`.
- **`VMClone`** binds the cloned VM only to a target that carries its
  `virtrigaud.io/clone-uid` marker.

Each of these has other checks too (the referenced Provider, an empty
`status.id`, the name). But without the policy, a tenant who can create
`VirtualMachine`s in the Provider's or the clone target's namespace can forge
the marker. **Don't set `admissionPolicy.reservedKeys.enabled: false`** unless
another admission control (Kyverno, Gatekeeper) reserves the same keys.

A `VMMigration` doesn't rely on the policy. It binds its target by the UID its
own `Create` returned and by a claim only it can produce
([`cross-namespace-targets.md`](cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own)).
Its provenance annotations are informational, and a forged one can still
mislead people and tools that read it.

The chart installs a **ValidatingAdmissionPolicy** that lets only the
manager's ServiceAccount add or change these keys. It runs inside the API
server. It's not a webhook, so it doesn't need `webhooks.enabled` and keeps
working while the manager is down.

**On Kubernetes older than 1.30 there's no policy.** There, the adoption and
clone markers above stay forgeable by anyone who can create
`VirtualMachine`s in those namespaces. Restrict `create`/`update` on
`virtualmachines` there to trusted users, or reserve the keys with another
admission control.

## What it rejects

Requests from any identity other than the manager's ServiceAccount (and the
exemptions below) to:

- **create** a `VirtualMachine` that carries a reserved key, or
- **update** a `VirtualMachine` so that a reserved key is added or gets a new
  value.

It doesn't reject:

- an update that leaves the reserved keys as they are, for example
  `kubectl edit` of `spec.powerState`, or a `kubectl apply` of a manifest
  exported from the cluster;
- removing a reserved key;
- deleting a `VirtualMachine`;
- status updates (the API server ignores metadata changes there).

A rejected request fails with `Forbidden` and a message that names the keys,
for example:

```text
... ValidatingAdmissionPolicy 'virtrigaud-reserved-keys' with binding
'virtrigaud-reserved-keys' denied request: annotation(s) virtrigaud.io/migration
are reserved for the VirtRigaud manager and cannot be added or changed
```

## Key inventory

Every `virtrigaud.io` key the code base reads or writes on a
`VirtualMachine`:

| Key | Kind | Written by | Reserved |
|---|---|---|---|
| `virtrigaud.io/migration` | annotation | `VMMigration` (provenance) | yes |
| `virtrigaud.io/migration-claim` | annotation | `VMMigration` (claim token of the target VM it creates) | yes |
| `virtrigaud.io/migrated-from` | annotation | `VMMigration` | yes |
| `virtrigaud.io/imported-disk-id` | annotation | `VMMigration` | yes |
| `virtrigaud.io/disk-checksum` | annotation | `VMMigration` | yes |
| `virtrigaud.io/migration-completed` | annotation | `VMMigration`, when the migration completes | yes |
| `virtrigaud.io/migration-completed-at` | annotation | `VMMigration`, when the migration completes | yes |
| `virtrigaud.io/clone` | annotation | `VMClone` | yes |
| `virtrigaud.io/clone-uid` | annotation | `VMClone` | yes |
| `virtrigaud.io/cloned-from` | annotation | `VMClone` | yes |
| `virtrigaud.io/adopted-host` | annotation | clustered adoption | yes |
| `virtrigaud.io/adopted-id` | annotation | single-host and clustered adoption | yes |
| `virtrigaud.io/adopted` | label | adoption, `VMClone` | yes |
| `virtrigaud.io/orphan-on-delete` | annotation | users | no |
| `virtrigaud.io/force-delete` | annotation | users | no |
| `virtrigaud.io/create-outcome` | annotation | operators confirming a create outcome | no |
| `infra.virtrigaud.io/placement-uid` | annotation | the manager, and restores that carry it (an untrusted marker by design, [`clustered-restore.md`](clustered-restore.md)) | no |

Keys on other kinds aren't covered. The `Provider` annotations
(`virtrigaud.io/adopt-vms`, `virtrigaud.io/adopt-filter`,
`virtrigaud.io/adopt-stamped-vms`, `infra.virtrigaud.io/max-disk-size`,
`infra.virtrigaud.io/tmp-size-limit`,
`infra.virtrigaud.io/allow-consumer-orphan-on-delete`) and the `Namespace`
grant (`infra.virtrigaud.io/allowed-source-namespaces`) are set by users and
administrators. The manager also writes `virtrigaud.io/reconcile-trigger` and
`virtrigaud.io/migration-pvc` on `Provider`s, but nothing reads them as proof
of anything.

The manager creates the target VM of a `VMClone` or `VMMigration`, so the
policy wouldn't stop a reserved key copied through it. A `VMMigration`
therefore drops every `virtrigaud.io` key from its `spec.target.annotations`,
and the reserved labels (`virtrigaud.io/adopted`) from its
`spec.target.labels`, with a Normal `TargetLabelsDropped` event that names
them. Other labels, in any domain, are copied. A `VMClone` drops the
`virtrigaud.io` keys from its `spec.target.annotations` and always sets
`virtrigaud.io/adopted` itself.

`internal/controller/admission_policy_test.go` keeps this classification and
the policy in step. A new manager-owned key fails that test until it's added
to the policy.

## Configuration

```yaml
admissionPolicy:
  reservedKeys:
    enabled: true          # default
    exemptUsernames: []    # e.g. ["system:serviceaccount:velero:velero"]
    exemptGroups: []       # e.g. ["platform-break-glass"]
```

- **Kubernetes 1.30 or later is required** (`admissionregistration.k8s.io/v1`).
  On an older cluster the chart renders nothing, and the manager's own checks
  still apply. `helm template` without `--kube-version` assumes Helm's built-in
  Kubernetes version, so pass `--kube-version` when you render for an older
  cluster.
- **Installing it needs cluster-scoped rights**: the identity that runs
  `helm install`/`upgrade` must be able to create
  `validatingadmissionpolicies` and `validatingadmissionpolicybindings`.
- **Backup and restore.** A restore that re-creates `VirtualMachine`s with
  their annotations, such as Velero, is rejected for every VM that carries a
  reserved key. Add the restore tool's identity to `exemptUsernames` (or a
  group to `exemptGroups`) before you restore.
- **More than one VirtRigaud installation in a cluster.** Each installation's
  policy exempts only its own manager, so it rejects the other managers'
  writes. Add every other installation's manager ServiceAccount
  (`system:serviceaccount:<namespace>:<name>`) to `exemptUsernames`, or turn
  the policy off on all but one installation and list every manager there.
- **A leader-election overlap with the policy off.** When a migration's
  `Create` of its target VM answers `AlreadyExists` for a VM that carries a
  `virtrigaud.io/migration-claim` it can't match, the migration re-checks
  instead of failing, because with the policy on only a manager (an
  overlapping leader) can have written that annotation. With the policy off,
  a tenant can plant the annotation as well. That only delays the
  `TargetVMNotOwned` failure by one re-check.
- **The kustomize copy follows `config/default`.** `config/default` fills the
  exempted manager's namespace and name from its ServiceAccount with
  `replacements`, after its own `namespace` and `namePrefix`. An overlay on
  top of `config/default` that changes either must repeat that
  `replacements` block. Otherwise the policy keeps exempting the old name and
  denies the manager.
- **Cluster administrators aren't exempt by default.** A policy applies to
  `system:masters` too. To keep a break-glass path, list a group in
  `exemptGroups`.

The kustomize deployment (`config/default`) installs the same policy from
`config/admission-policy/`, exempting the manager ServiceAccount
(`virtrigaud-system/virtrigaud-controller-manager` by default).
On a cluster older than 1.30, remove `../admission-policy` from
`config/default/kustomization.yaml`.

## Checking it

```sh
kubectl get validatingadmissionpolicy,validatingadmissionpolicybinding | grep reserved-keys

# As a tenant: must be refused.
kubectl -n team-a annotate virtualmachine web virtrigaud.io/migration=team-a/forged
```
