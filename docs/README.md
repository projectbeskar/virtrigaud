# docs/

VirtRigaud's primary documentation lives on the website:
**[https://projectbeskar.github.io/virtrigaud](https://projectbeskar.github.io/virtrigaud)**

This directory holds in-tree references that are most useful alongside the source code:

| Path | Contents |
|------|----------|
| [`docs/upgrading.md`](upgrading.md) | Upgrade guide from v0.3.11: breaking changes, required upgrade order (CRDs → manager → providers), new required privileges/config, non-breaking-but-visible behavior changes, a post-upgrade verification checklist, and rollback caveats |
| [`docs/release-notes/next.md`](release-notes/next.md) | Draft release notes for the release that follows v0.3.11, meant to be pasted into the GitHub release body; superseded once that release is published |
| [`docs/adr/`](adr/) | Architecture Decision Records — design decisions that are binding on the codebase |
| [`docs/image-preparation.md`](image-preparation.md) | Image-preparation lifecycle: how `VMImage` prepare-on-create works and the `VMImage.status` fields it surfaces |
| [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md) | Clustered-provider inventory model: the `Host` and `HostPool` CRDs (ADR-0007 P1, additive; single-host providers unchanged) |
| [`docs/vm-ownership.md`](vm-ownership.md) | VM ownership across providers: how `Create` treats an existing same-named VM, the `ProviderConflict` condition, and the vSphere details (ExtraConfig owner stamp, folder-scoped lookup, clone sources limited to real templates, rejected MOID-shaped names, required vCenter privileges, upgrade notes) |
| [`docs/vm-provider-binding.md`](vm-provider-binding.md) | Why `VirtualMachine` `spec.providerRef` is immutable once the VM is bound (the CRD CEL rule), the operator-side `status.boundProvider` check and the `ProviderRefMismatch` condition, a re-created `Provider` (`BoundProviderRecreated`), clone/migration target rules, the manager's installed-CRD check (readiness, `virtrigaud_manager_vm_crd_security_features`), detaching a VM without deleting it (`virtrigaud.io/orphan-on-delete`), and upgrade notes (breaking change, CRD first, trust on first reconcile) |
| [`docs/cross-namespace-targets.md`](cross-namespace-targets.md) | Cross-namespace `VMClone` / `VMMigration` targets: the `infra.virtrigaud.io/allowed-source-namespaces` grant on the target namespace (who can set it, self-service platforms, name reuse), the `TargetNamespaceNotAllowed` refusal and how to recover from it, what revoking a grant stops, why a granted migration's disk is copied rather than attached in place, why `VMMigration` `spec.source.providerRef` must name the source VM's own Provider, and upgrade notes |
| [`docs/cross-namespace-references.md`](cross-namespace-references.md) | Cross-namespace `Provider` / `VMClass` / `VMImage` references: `spec.consumerNamespaceSelector` (unset = own namespace only, `{}` = every namespace, label selectors, `kubernetes.io/metadata.name`), who can grant (object owners, namespace labellers, self-service platforms), the `ConsumerNotAllowed` refusal and what each controller checks, how a grant takes effect (watches, 5-minute recheck), revocation, deleting a VM whose `Provider` is refused (orphan-on-delete / force-delete), and upgrade notes (breaking: set selectors before upgrading) |
| [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md) | libvirt domain names and ownership: namespaced domain names (`<namespace>.<name>`) and the names derived from them, per-create staging on the host, the `CreateRequest.owner` stamp, fail-closed binding of pre-existing domains, adoption of stamped domains, the `ProviderConflict` condition, and upgrade notes |

For user guides, operator documentation, provider capabilities, and the API reference, see the website.
