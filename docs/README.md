# docs/

VirtRigaud's primary documentation lives on the website:
**[https://projectbeskar.github.io/virtrigaud](https://projectbeskar.github.io/virtrigaud)**

This directory holds in-tree references that are most useful alongside the source code:

| Path | Contents |
|------|----------|
| [`docs/adr/`](adr/) | Architecture Decision Records — design decisions that are binding on the codebase |
| [`docs/image-preparation.md`](image-preparation.md) | Image-preparation lifecycle: how `VMImage` prepare-on-create works and the `VMImage.status` fields it surfaces |
| [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md) | Clustered-provider inventory model: the `Host` and `HostPool` CRDs (ADR-0007 P1, additive; single-host providers unchanged) |
| [`docs/vm-ownership.md`](vm-ownership.md) | VM ownership across providers: how `Create` treats an existing same-named VM, the `ProviderConflict` condition, and the vSphere details (ExtraConfig owner stamp, folder-scoped lookup, clone sources limited to real templates, rejected MOID-shaped names, required vCenter privileges, upgrade notes) |
| [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md) | libvirt domain names and ownership: namespaced domain names (`<namespace>.<name>`) and the names derived from them, per-create staging on the host, the `CreateRequest.owner` stamp, fail-closed binding of pre-existing domains, adoption of stamped domains, the `ProviderConflict` condition, and upgrade notes |

For user guides, operator documentation, provider capabilities, and the API reference, see the website.
