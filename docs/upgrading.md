# Upgrading from v0.3.11

This guide covers upgrading a VirtRigaud installation from **v0.3.11** (2026-06-21) to
the next release, built from `main` at or after commit `5adf071` (PR #341). That
window contains an unusually large number of security fixes, several of which are
**breaking** or change operator-visible behavior. Read the "Breaking changes" table
below before you upgrade, then follow "Required upgrade order".

Each change below links to the in-tree page that documents it in full — this guide
is the index and the sequencing, not a duplicate of that detail.

> If you are upgrading through several intermediate `main` snapshots (for example a
> CI/CD pipeline tracking `main`), read the caveats for **#339** (libvirt domain
> naming) and **#336** (chart NetworkPolicy key) — both describe a broken
> intermediate state that only matters if you deployed a `main` build from inside
> that window.

## Breaking changes

| Change | Who is affected | Action required |
|---|---|---|
| **Cross-namespace `Provider` / `VMClass` / `VMImage` references need a grant** ([`docs/cross-namespace-references.md`](cross-namespace-references.md)) | Anyone whose VirtualMachines (or VMClone/VMMigration targets) reference a Provider, VMClass or VMImage in **another** namespace — e.g. a shared Provider in `virtrigaud-system`. Such VMs **fail closed** after upgrade (`Ready=False`, reason `ConsumerNotAllowed`, no provider calls) until access is granted. | **After applying the new CRDs and before rolling the manager**, set `spec.consumerNamespaceSelector` on every shared Provider, VMClass and VMImage (`{}` = shared with all namespaces; or a label selector on the consumer namespaces). The field does not exist until the CRDs are upgraded: an older CRD rejects it (strict field validation) or prunes it. See step 2 of "Required upgrade order". Deleting a VM whose Provider is refused needs `virtrigaud.io/orphan-on-delete: "true"` or `force-delete`. See the upgrade notes in the linked doc for queries that list what needs a selector. |
| **`VirtualMachine.spec.providerRef` is immutable once bound** ([#341](https://github.com/projectbeskar/virtrigaud/pull/341), [`docs/vm-provider-binding.md`](vm-provider-binding.md)) | Anyone whose tooling edits `providerRef` after creation, and anyone using the old "re-point to a missing Provider, then delete" un-adopt trick | Detach with `virtrigaud.io/orphan-on-delete: "true"` instead. Upgrade CRDs with or before the manager — the manager refuses readiness on an old CRD. |
| **Cross-namespace VMClone/VMMigration targets need a grant** ([#340](https://github.com/projectbeskar/virtrigaud/pull/340), [`docs/cross-namespace-targets.md`](cross-namespace-targets.md)) | Anyone whose `spec.target.namespace` differs from the object's own namespace | Annotate the target namespace: `kubectl annotate namespace <target> infra.virtrigaud.io/allowed-source-namespaces=<source-ns>[,<source-ns2>...]`. Do this **before** upgrading if you have objects in flight — see the query in the linked doc. |
| **vSphere Create/Clone fail closed on VM ownership** ([#335](https://github.com/projectbeskar/virtrigaud/pull/335), [`docs/vm-ownership.md`](vm-ownership.md)) | All vSphere users | Grant the vCenter account **Virtual machine > Change Configuration > Advanced configuration** (`VirtualMachine.Config.AdvancedConfig`). Rename any VirtualMachine, VMImage prepare target, or bare template name matching `vm-<digits>`. Make sure every `VMImage.templateName` names a real vSphere template, not a regular VM. |
| **libvirt default cloud-init no longer provisions any credentials** ([#328](https://github.com/projectbeskar/virtrigaud/pull/328)) | Anyone relying on the default `ubuntu` user / SSH key / passwordless sudo on a libvirt VM created **without** `spec.userData` | Supply your own `spec.userData` with a user and key going forward. On VMs already created without `spec.userData`, the old hardcoded key and sudo grant are still present *inside the guest* — remove `/home/ubuntu/.ssh/authorized_keys`'s VirtRigaud entry and the `/etc/sudoers.d/90-cloud-init-users` drop-in by hand if you want them gone; rolling the provider image does not touch running VMs. |
| **libvirt: base images are confined and always copied, not attached in place** ([#334](https://github.com/projectbeskar/virtrigaud/pull/334), [`docs/image-preparation.md`](image-preparation.md#libvirt-image-paths-sourcelibvirtpath)) | libvirt users with images outside `/var/lib/libvirt/images`, in a non-default pool, or attached in place by an earlier release | Set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` (via `Provider.spec.runtime.env`) if images live elsewhere. Images an earlier release attached in place are now refused as a base — create a `VMImage` under a **new name** to get a fresh copy. **Before deleting any pre-upgrade libvirt VM**, run `virsh domblklist --details <domain>` on every domain on the host and check for a shared disk file — deleting one VM can delete a disk that other VMs still use. Expect more disk usage and slower creates from the copy. |

The following are **not** flagged `Breaking change` in the CHANGELOG (no API/CRD
schema break, and existing bound VMs are unaffected), but change what a *new*
`Create` does and are easy to trip over mid-upgrade:

| Change | What changes | Operator action |
|---|---|---|
| **New libvirt domains are named `<namespace>.<name>`** ([#339](https://github.com/projectbeskar/virtrigaud/pull/339), [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md)) | Only *new* domains get the namespaced host name; existing VMs keep their name and are addressed by `status.id` as always. | **Upgrade the manager before the libvirt provider** (see "Required upgrade order"). External tooling that assumes "domain name == VirtualMachine name" must read `status.id` instead. Remove leftover `/tmp/virtrigaud-cloudinit/<name>/` seed directories after upgrading (a pre-#331 failed create never cleaned these up on the host). |
| **libvirt Create fails closed on a same-named domain it doesn't own** ([#333](https://github.com/projectbeskar/virtrigaud/pull/333), [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md)) | A `VirtualMachine` create that collides with a domain that predates this change (no owner stamp) now fails with `Ready=False/ProviderConflict` instead of silently taking the domain over. | Adopt the domain (`virtrigaud.io/adopt-vms`) or rename the VirtualMachine to resolve a conflict. |
| **libvirt SSH commands are shell-quoted; guest-agent calls now actually work** ([#331](https://github.com/projectbeskar/virtrigaud/pull/331)) | Guest-agent-backed fields (Describe IPs, `SyncGuestTime`, best-effort in-guest filesystem grow after an online disk expand) were silently broken over SSH before this fix and now run for real, within new time/size bounds. | Confirm the SSH login shell for the provider's hypervisor account is POSIX-compatible (`sh`, `bash`, `dash`, `zsh`, `ash`) — the quoting relies on POSIX single-quote semantics. |
| **`VMImage` prepare state is keyed by Provider `<namespace>/<name>`** ([`docs/image-preparation.md`](image-preparation.md#prepare-state-is-per-provider)) | `status.providerStatus` and `status.availableOn` use `<namespace>/<name>` instead of the bare Provider name; each entry records the Provider's UID and its own `taskRef`; the image-wide `status.prepareTaskRef` is no longer written. On the first VM create that uses each image, a bare-name entry is moved to the image's own namespace when a Provider of that name exists there (and re-validated once through the idempotent prepare), otherwise dropped; a legacy `prepareTaskRef` is cleared. An image is now prepared only before a create or re-create, never for a VM that exists. | Upgrade the `VMImage` CRD **before** the manager: until it has `providerStatus[].providerUID`/`taskRef`, readiness fails and VM creates that need an image prepare are held. Update scripts that read `providerStatus`/`availableOn`. Under `onMissing: Fail`/`Wait`, an entry without `providerUID` holds creates (reason `ProviderUIDMissing`): switch `onMissing` to `Import` to re-validate it. Never write `vmimages/status` by hand; the VirtualMachine controller is its single writer (ADR-0005). Expect one `ImagePrepare` call per image and Provider on the first create after the upgrade (a no-op when the prepared image exists). |
| **Image prepares carry the `VMImage`'s identity; providers without artifact identity are held** ([ADR-0009](adr/0009-prepared-image-artifact-identity.md), [`docs/image-preparation.md`](image-preparation.md#prepared-image-artifact-identity)) | The manager sends every `ImagePrepare` with the `VMImage`'s UID, a digest of its `spec.source` and an empty target name, and records a prepare only when the provider's answer confirms that identity. A Provider that imports images but does not report `status.reportedCapabilities.supportsImageArtifactIdentity` gets no prepare: creates of import-style images (`ovaURL`, libvirt `url`, `http`) through it are held (`ProviderLacksArtifactIdentity`) until the provider is upgraded. `status.providerStatus[].sourceDigest` is recorded, and an entry is used only for the `spec.source` it was prepared for, so the first create for each image and location after the upgrade prepares the image once more, under its new artifact name. An artifact at that name that the provider cannot prove was prepared for the image holds creates (`ArtifactConflict`, `Warning` event `ImageArtifactConflict`, metric `virtrigaud_image_prepare_artifact_total{outcome="conflict"}`). | Roll the providers right after the manager (step 4): until they report the capability, new VMs from import-style images wait. Expect one re-import per image and image location; the bare-named artifacts of the earlier release are left on the hypervisor (remove them yourself once nothing references them). Under `onMissing: Fail`/`Wait`, an entry without a digest holds creates (`SourceDigestMissing`): switch `onMissing` to `Import`. Alert on `virtrigaud_image_prepare_artifact_total{outcome="conflict"}`. Reference-style images and VMs that exist are unaffected, but a VM re-created because it vanished from its hypervisor is a create and waits too. |
| **libvirt: prepared images are named, stamped and published by identity** (ADR-0009 Slice 4, [`docs/image-preparation.md`](image-preparation.md#libvirt-prepared-images-sourcelibvirturl)) | The libvirt provider reports `supportsImageArtifactIdentity` (single-host Providers; a clustered Provider still does not prepare images). A URL-sourced libvirt `VMImage` is prepared as `<pool dir>/<namespace>.<name>_<16 hex>.qcow2` with a stamp file `.<same>.virtrigaud-image.json`. The prepared file is **read-only (`0444`) and owned by the provider's SSH user** — no longer `chown libvirt-qemu:kvm` / `chmod 777`; each VM's own copy is unchanged. The pool directory must be an allowed image directory (`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`) on a filesystem with hard links, or the prepare fails with `InvalidSource`. Downloads are one transfer (no URL globbing), limited in time (`spec.prepare.timeout`) and size (`VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB`, default 256). A source that fails for good (HTTP 4xx, checksum mismatch, unreadable image) now shows `InvalidSource` instead of being retried every few seconds. | Roll the libvirt providers with the other providers (step 4). Providers that share a pool directory must use the same SSH user (another user's artifact is a Conflict). If `restorecon` must run (SELinux hosts), the SSH user needs passwordless `sudo` for it. Raise `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB` if an image is larger than 256 GiB. Alert on `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="libvirt"}`: non-zero means a manager older than this release is still preparing images by bare name. |
| **Proxmox: URL image import (`source.http`) is refused** (ADR-0009 D10, [`docs/image-preparation.md`](image-preparation.md#proxmox-image-sources)) | It never worked: the import only downloaded the file and never created a template VM that `Create` could clone, and it treated any same-named template as "already prepared". `ImagePrepare` now returns `InvalidSpec` for it, with or without an identity and before any PVE call, so the `VMImage` shows `Ready=False` / `InvalidSource` and new VMs from it are not created. VMs that already exist are unaffected. `source.proxmox.templateID`/`templateName` are unchanged. The Proxmox provider now advertises `supportsImageArtifactIdentity` and serves `/metrics` on its health port. | Prepare a template on Proxmox VE (`qm importdisk` + `qm template`) and point the `VMImage` at it with `source.proxmox.templateID`. Real URL import comes with ADR-0009 Slice 6. After the upgrade, `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"}` must stay at 0: a non-zero value means a manager older than this release is still sending requests (they are refused and counted). |
| **Chart: NetworkPolicy switch moved to `networkPolicy.*`** ([#336](https://github.com/projectbeskar/virtrigaud/pull/336)) | The legacy `security.networkPolicies` key is now ignored. Only relevant if you deployed a `main` build from the #327 window and set `security.networkPolicies.enabled=true`. | Set `networkPolicy.enabled=true` instead. For any Helm upgrade that carries custom values across chart versions, prefer `helm upgrade --reset-then-reuse-values` over `--reuse-values` (see "Required upgrade order"). |

**Experimental clustered providers** (`Provider.spec.topology: cluster`, the `Host`/
`HostPool` CRDs) were added entirely after v0.3.11, so nothing on v0.3.11 is affected.
If you are already running a clustered provider from `main`: `Provider.spec.topology`
is now immutable (recreate the Provider to change it), `Host`/`HostPool`
`providerRef`/`credentialSecretRef` must be in the same namespace as the Provider
(drop any explicit `namespace`), and `Host.spec.endpoint` is now pattern-validated.
See [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md) and
[ADR-0007](adr/0007-clustered-orchestrator-provider.md).

## Required upgrade order

1. **CRDs first.**
   - Helm (default): the chart's pre-upgrade hook applies the CRDs baked into the
     chart with `kubectl apply --server-side --force-conflicts` before the manager
     rolls. This is automatic when `crdUpgrade.enabled: true` (the default) — just
     make sure the chart you're upgrading with was packaged from a current checkout
     (`make gen-helm-crds` if you package it yourself).
   - Manual / GitOps / `crdUpgrade.enabled: false`: apply the CRDs yourself before
     the manager rolls out:
     ```sh
     kubectl apply --server-side --force-conflicts -f config/crd/bases
     ```
   - **Why this matters more than usual this release:** the new manager reads the
     installed `VirtualMachine`, `Provider`, `VMClass` and `VMImage` CRDs at startup
     and on every readiness probe. If the `VirtualMachine` CRD is missing
     `status.boundProvider` or the `spec.providerRef` immutability rule (#341), if
     the other three lack `spec.consumerNamespaceSelector`, or if the `VMImage` CRD
     lacks `status.providerStatus[].providerUID`, `taskRef` or `sourceDigest` or the
     `Provider` CRD lacks `status.reportedCapabilities.supportsImageArtifactIdentity`
     (ADR-0009), the manager **fails
     readiness** (it does not crash, and it does not stop managing existing VMs —
     but `helm upgrade --wait` will time out, and a rolling update will not proceed
     past the old pod).
2. **Then grant cross-namespace consumers**, before the new manager runs. If any
   VirtualMachine, VMClone or VMMigration references a `Provider`, `VMClass` or
   `VMImage` in another namespace, set `spec.consumerNamespaceSelector` on those
   objects now. The field exists only once step 1 has applied the new CRDs (an
   older CRD rejects or prunes it), and the old manager ignores it, so setting it
   here changes nothing until the new manager starts — which then finds the
   grants in place. Skipping this step makes the affected VMs stop being managed
   (`ConsumerNotAllowed`, nothing is deleted) until you set it.
   - **Helm:** the chart's pre-upgrade hook applies the CRDs in the same
     `helm upgrade` that rolls the manager, so there is no pause between steps 1
     and 3. Apply the new chart's CRDs yourself first (`kubectl apply
     --server-side --force-conflicts -f <new chart>/crds/`, or `config/crd/bases`
     from the release checkout), set the selectors, then run `helm upgrade` (the
     hook's re-apply is a no-op). If you run `helm upgrade` directly instead,
     set the selectors right after it: the affected VMs are refused only until
     then, and the manager re-drives them within seconds of each grant.
   - See the queries in
     [`docs/cross-namespace-references.md`](cross-namespace-references.md#upgrade-notes).
3. **Then the manager.** Wait for the manager Deployment to report Ready before
   touching provider images — see "Post-upgrade verification" below for the exact
   check.
   - The manager's RBAC changed: `get` on the `virtualmachines`, `providers`,
     `vmclasses` and `vmimages` `.infra.virtrigaud.io` CRDs (#341, consumer grant),
     `get;list;watch` on `namespaces` (#340). The Helm chart applies these with the
     manager; a manual/kustomize install must reapply `config/rbac/role.yaml`.
4. **Then the providers**, in any order across hypervisor types, and promptly: until a
   provider reports `supportsImageArtifactIdentity` (ADR-0009), the new manager sends it
   no image prepare, so creates of import-style images through it wait
   (`ProviderLacksArtifactIdentity`; VMs that exist and reference-style images are
   unaffected). Also:
   - **libvirt: do not roll the provider ahead of the manager.** A libvirt provider
     built after #339 expects `CreateRequest.owner` and `ImportDiskRequest.target_vm`
     from the manager. A manager older than #333 talking to a new provider still
     works (bare names throughout); a manager that already sends `owner` (#333+) but
     not yet `target_vm` (#339) makes the new provider name new domains
     `<namespace>.<name>` while migration landing disks still use the legacy name —
     every libvirt-target `VMMigration` fails in that window. Don't start a
     libvirt-target migration while the provider is ahead of the manager; if one is
     between its import and its create when you upgrade, re-run it afterwards.
   - **vSphere: grant the new vCenter privilege before rolling the provider.**
     Without `VirtualMachine.Config.AdvancedConfig`, every `Create` and `Clone`
     starts failing the moment the new provider image is live.
5. **Helm value carry-over:** if you keep custom `values.yaml` overrides across the
   upgrade, use `helm upgrade --reset-then-reuse-values` (Helm 3.14+), not
   `--reuse-values`. `--reuse-values` reuses the **old** chart's defaults, which is
   exactly the bug #336 fixed for the NetworkPolicy key — settings added in the new
   chart are silently missing and old defaults linger.

## New required privileges and config

- **vSphere:** `VirtualMachine.Config.AdvancedConfig` on the target VM folder and
  resource pool for the provider's vCenter account (#335).
- **Manager RBAC:** `get` on the `virtualmachines.infra.virtrigaud.io` CRD (#341) and
  on the `providers`, `vmclasses` and `vmimages` `.infra.virtrigaud.io` CRDs (the
  consumer-grant check);
  `get;list;watch` on `namespaces`, cluster-scoped (#340); `serviceaccounts`
  create/get/list/watch/update/patch/delete, already shipped since v0.3.11-era #297
  for per-provider ServiceAccounts — listed here because a manual/kustomize install
  that has drifted from `config/rbac/role.yaml` should reconcile it now. All of this
  is applied automatically by the Helm chart alongside the manager.
- **libvirt image directories:** `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` (provider pod env
  via `Provider.spec.runtime.env`), a comma- or colon-separated list of allowed host
  directories for `source.libvirt.path` and prepared images. Default:
  `/var/lib/libvirt/images`. Required if your images live elsewhere, in a
  subdirectory, under a session-mode path, or in a non-default storage pool (#334).
- **libvirt SSH login shell:** must be POSIX-compatible (`sh`, `bash`, `dash`, `zsh`,
  `ash`) for the SSH-quoting fix to work correctly (#331).
- **libvirt image download limit:** `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB` (provider
  pod env via `Provider.spec.runtime.env`), the largest image `ImagePrepare` downloads, in
  GiB. Default `256`; an invalid value falls back to the default (logged). A larger source
  is refused as `InvalidSource` (ADR-0009 Slice 4).

## Behavior changes that are visible but not breaking

- **TLS 1.2 floor on the webhook and metrics servers** (#327). Transparent for any
  client that already negotiates TLS 1.2+ (the Kubernetes API server and Prometheus
  both do); only matters if something talks to those endpoints with a hard TLS 1.0/1.1
  requirement.
- **NetworkPolicy templates exist now but are opt-in and default-off**
  (`networkPolicy.enabled: false`) (#327, #336). No behavior change unless you
  explicitly enable them, in which case read the chart README's "Network Policies"
  section first — it needs a NetworkPolicy-enforcing CNI and deployment-specific
  tuning (monitoring namespace, DNS, provider namespace, hypervisor egress CIDRs).
- **`VMClass.spec.memory` / `spec.diskDefaults.size` now have CRD upper bounds**
  (below 100Ti and 1Pi respectively) (#338). No plausible existing VMClass is
  affected; this closes an int32 wraparound bug, not a realistic value someone set.
- **Guest-agent-derived VM status fields may start appearing/changing** for libvirt
  VMs (#331) — they were silently no-ops before this release due to the same
  shell-quoting bug.
- **A vSphere `Describe` that hits a transient vCenter error no longer triggers a
  re-create** (#335) — it now retries instead of clearing `status.id`. This is a
  fix, but it means a VM that used to "self-heal" through a spurious re-create during
  vCenter blips now just waits and retries.

## Post-upgrade verification checklist

- [ ] `kubectl get pods -n virtrigaud-system` — manager and provider pods Ready.
- [ ] Manager CRD-features check verified:
  ```sh
  kubectl get --raw /metrics -n virtrigaud-system 2>/dev/null | grep virtrigaud_manager_vm_crd_security_features
  # or, without scraping metrics:
  kubectl get crd virtualmachines.infra.virtrigaud.io \
    -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.status.properties.boundProvider.type}'
  # should print: object
  kubectl get crd providers.infra.virtrigaud.io \
    -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.spec.properties.consumerNamespaceSelector.type}'
  # should print: object (likewise for vmclasses and vmimages)
  ```
- [ ] No VM is unexpectedly refused for a cross-namespace reference:
  ```sh
  kubectl get virtualmachines -A -o json | jq -r '.items[]
    | select(any(.status.conditions[]?; .type=="Ready" and .reason=="ConsumerNotAllowed"))
    | "\(.metadata.namespace)/\(.metadata.name)"'
  ```
  Each line needs `spec.consumerNamespaceSelector` on the object its `Ready` message
  names — see [`docs/cross-namespace-references.md`](cross-namespace-references.md).
- [ ] Audit bound VMs' `providerRef` before the CRD's admission rule locks them in —
      use the query in [`docs/vm-provider-binding.md`](vm-provider-binding.md#upgrade-notes).
- [ ] Audit cross-namespace clone/migration targets and confirm each target namespace
      carries the grant — the query is in
      [`docs/cross-namespace-targets.md`](cross-namespace-targets.md#upgrade-notes).
- [ ] vSphere: create (or reconcile) a test VM and confirm it succeeds — this exercises
      the new `AdvancedConfig` privilege end to end.
- [ ] libvirt: if images live outside `/var/lib/libvirt/images`, confirm
      `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` is set on the affected Providers and that a
      `VMImage`/`VirtualMachine` create against them still succeeds.
- [ ] libvirt: check for and clean up orphaned `/tmp/virtrigaud-cloudinit/` seed
      directories on each host.
- [ ] libvirt: before deleting any **pre-upgrade** VM, run
      `virsh domblklist --details <domain>` and compare disk sources across domains on
      that host to rule out a shared legacy disk.
- [ ] Proxmox: list `VMImage`s that import from a URL; any that VMs on a Proxmox
      Provider use must be switched to `source.proxmox.templateID` (URL import is
      refused in this release):
      ```sh
      kubectl get vmimages -A -o json | jq -r '.items[]
        | select(.spec.source.http != null and .spec.source.proxmox == null)
        | "\(.metadata.namespace)/\(.metadata.name)"'
      ```
- [ ] If you enabled `networkPolicy.enabled=true`, render and check the objects before
      relying on them: `helm template virtrigaud charts/virtrigaud --set
      networkPolicy.enabled=true | grep -A2 'kind: NetworkPolicy'`.

## Rollback caveats

- **CRD-level changes are one-way in practice.** The `spec.providerRef` immutability
  rule (#341) is a CRD-level CEL admission rule, not manager code — rolling the
  *manager* back to a pre-#341 image does **not** restore the ability to re-point a
  bound VM's `providerRef`, because the apiserver still enforces the rule from the
  installed CRD. Only reapplying an older CRD schema removes it, which also drops
  `status.boundProvider` and the other additive status fields already recorded (not
  recommended — you'd lose the binding record along with the protection).
- **An old manager against the new CRDs silently drops the new protections.** A
  manager older than #341 doesn't know about `status.boundProvider` and won't
  enforce the bound-provider check on VM/VMClone/VMMigration/VMSnapshot calls, even
  though the CRD's admission rule still blocks `spec.providerRef` edits. If you must
  roll the manager back temporarily, treat that window as running without the
  defense-in-depth check, not just "no schema difference."
- **New-format libvirt domains outlive a provider rollback.** A domain created under
  `<namespace>.<name>` (#339) keeps that host name even if you roll the libvirt
  provider image back to an older build — every operation after `Create` addresses it
  by `status.id`, which an older provider still resolves correctly.
- **An old manager ignores `spec.consumerNamespaceSelector`.** Rolling the manager
  back re-opens unrestricted cross-namespace `Provider`, `VMClass` and `VMImage`
  references for the duration. The selectors stay on the objects and take effect
  again when the new manager returns.
- **Rolling a provider back holds image prepares.** A provider image older than
  ADR-0009 does not report `supportsImageArtifactIdentity`, so the manager stops
  sending it image prepares (`ProviderLacksArtifactIdentity`); if the Provider's
  status still shows the capability, the old provider refuses the empty target name
  and nothing is recorded as prepared. Either way it fails closed. Images already
  prepared under the new scheme stay usable. In the stale-capability case the refusal
  is an invalid-argument answer, so the `VMImage` shows `InvalidSource` ("image source
  rejected by provider …: … target name …") — misleadingly blaming the image's owner,
  who may be in another namespace for a shared image. The image is fine: roll the
  provider forward again. Once the Provider controller re-reads the old provider's
  capabilities, the hold becomes `ProviderLacksArtifactIdentity`.
- **Cross-namespace grants and orphan-on-delete have no equivalent on old code.**
  Anything you did with `infra.virtrigaud.io/allowed-source-namespaces` (#340) or
  `virtrigaud.io/orphan-on-delete` (#341) after upgrading is inert if you roll the
  manager back — the old manager doesn't read either.
- **An old manager against a new libvirt provider.** Its prepares carry no identity:
  for this release they are served in deprecated legacy mode — the bare-name file
  `<pool dir>/<VMImage name>.qcow2`, reused by name, so same-named `VMImage`s of different
  namespaces share one file again — logged at `WARN` and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="libvirt"}`.
  Artifacts prepared by identity are left alone and reused when the manager rolls
  forward.
- **An old manager against a new Proxmox provider.** Its Proxmox URL imports carry no
  identity: they are refused like any other, logged at `WARN` and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"}`.
  Reference-style Proxmox sources keep working. Rolling the Proxmox provider back does
  not bring back a working URL import: the old one never produced a usable template.
- If you do need a full rollback during a maintenance window, go in reverse order
  (providers → manager → CRDs) and expect the caveats above; a partial rollback
  (providers only, or manager only) is safer and sufficient for most of the issues
  this release fixes.

## Also since v0.3.11

Lower-impact items with no required operator action, listed for completeness — see
`CHANGELOG.md` for full detail:

- Dependency/CVE fixes: grpc, x/text, Go toolchain bumps (#298, #303, #299).
- Provider pods run under a dedicated, token-less, binding-less ServiceAccount
  instead of the namespace `default` SA (#297) — automatic on rollout.
- libvirt: capped concurrent virsh forks, `ListVMs` reads domain XML once instead of
  per-domain (#289, #285) — performance only.
- libvirt: hardware-accelerated `<domain type='kvm'>` is used for **new** VMs when
  the host exposes a readable `/dev/kvm`, instead of always emulating with `qemu`
  (#283, contributed by @jing2uo) — existing domains are unaffected.
- ADR-0007 (clustered orchestrator provider) and ADR-0008 (pure-Go libvirt driver +
  in-process SSH transport) landed their early slices behind the experimental
  `topology: cluster` gate and a shadow-compare soak that never serves production
  traffic — no behavior change for single-host providers.
