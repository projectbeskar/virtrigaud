# ADR-0009: Prepared-image artifact identity, provenance stamps, and fail-closed reuse

## Status

**Proposed (2026-09-25).** Not implemented. **Release blocker** for the release that
ships cross-namespace `VMImage` sharing
([#343](https://github.com/projectbeskar/virtrigaud/pull/343),
`spec.consumerNamespaceSelector`). The slices that must land before that release are
listed under *Implementation slices*.

**Review:** staff-architect, 2026-09-25: *accept with changes*. The changes are applied
in this revision.

**Maintainer decisions (2026-09-25):**
- Q1, Q2 and Q7 are decided as recommended.
- Q3 is decided differently from the recommendation: legacy mode is kept for one
  release, then refused.
- Q4, Q5 and Q6 are still open.

The status stays Proposed until this ADR merges.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Hypervisors in scope**: vSphere, libvirt/KVM, Proxmox VE, and the mock provider.

**Related**:
- [ADR-0005](./0005-image-preparation-trigger-model.md): the lazy, VM-create-driven
  image-prepare trigger. This ADR **amends ADR-0005 decisions 2 and 6**:
  - Decision 2 (the capability gate) gains a second capability. An import-capable
    provider without identity support is **held** (D7).
  - Decision 6 (the request shape) replaces "`TargetName` is the `VMImage` name" with
    the image identity plus the source digest (D1, D2, D7).

  ADR-0009 also changes **what a provider names, stamps, and accepts as "already
  prepared"**. ADR-0005 carries a pointer to this amendment.
- [#154](https://github.com/projectbeskar/virtrigaud/issues/154) /
  [#214](https://github.com/projectbeskar/virtrigaud/issues/214): `ImagePrepare` and
  "Create consumes the prepared location".
- [#343](https://github.com/projectbeskar/virtrigaud/pull/343): cross-namespace
  `Provider`/`VMClass`/`VMImage` references need a grant. Sharing a `VMImage` or a
  `Provider` is now a supported, documented setup, and that makes the defect below
  reachable across tenants by design.
- [#344](https://github.com/projectbeskar/virtrigaud/pull/344): per-Provider prepare
  state, in flight on branch `fix/vmimage-prepare-status-by-provider-identity`. It re-keys
  `VMImage.status.providerStatus` by `<providerNamespace>/<providerName>`, records
  `providerUID` and a per-entry `taskRef`, and prepares only for VMs that have not
  been created yet. It fixes the **operator-side records**. Its own docs list the
  **hypervisor-side artifact** problem as a known limitation that needs a design
  record. This is that record.
- Precedents this ADR follows:
  [#339](https://github.com/projectbeskar/virtrigaud/pull/339) (libvirt domains named
  `<namespace>.<name>`, 200-byte bound, `_`+16-hex truncation,
  `internal/providers/libvirt/domain_naming.go`),
  [#333](https://github.com/projectbeskar/virtrigaud/pull/333) /
  [#335](https://github.com/projectbeskar/virtrigaud/pull/335) (owner stamps in the
  libvirt domain `<metadata>` and vSphere ExtraConfig `virtrigaud.owner.*`, bind only
  on a matching UID, reserved ExtraConfig keys stripped from OVFs),
  [#334](https://github.com/projectbeskar/virtrigaud/pull/334) (libvirt image-path
  confinement and reserved file names).

No `v1beta1` spec change. There are additive proto fields and additive status fields
(D7, D8).

---

## Context

### What `ImagePrepare` is asked to do, traced through the live code (at `8a3d32a`)

`EnsureImageOnProvider` (`internal/controller/virtualmachine_image_prepare.go:89`)
runs in the VirtualMachine reconcile. For an **import-style** source
(`imageSourceNeedsPrepare`, `:358`: a vSphere `ovaURL`, a libvirt `url`, or an
HTTP/registry source; reference-style sources such as an existing template name or a
libvirt pool path never reach a provider's import path) it sends:

```go
ip.PrepareImage(ctx, contracts.ImagePrepareRequest{
    ImageJSON:   string(imageJSON),   // json.Marshal(vmImage.Spec), selector stripped
    TargetName:  vmImage.Name,        // :248, the bare VMImage name
    StorageHint: "",
})
```

On the wire this is `ImagePrepareRequest{image_json, target_name, storage_hint}`
(`proto/provider/v1/provider.proto:312-316`). The request carries **no identity**: no
namespace, no UID, no source digest. `target_name` is the only name the provider gets.
Everything a provider decides about the hypervisor artifact rests on that bare name.

`overrideImageWithPreparedLocation`
(`internal/controller/virtualmachine_controller.go:1874`) then feeds the returned
`prepared_image_id` / `prepared_image_path` into `Create`. vSphere clones the template
by that **name**, libvirt copies the file at that **path**, and Proxmox gets a template
reference.

### Every provider treats "an artifact with that name exists" as "prepared", before any source or checksum check

**vSphere** (`internal/providers/vsphere/image.go`):
- `ImagePrepare` (`:222`) runs its idempotency gate (`:259-273`) before it looks at
  the source. `findPreparedTemplate` (`:312`) calls `lookupTemplate`, which searches
  the whole default datacenter for a bare name
  (`internal/providers/vsphere/template_source.go:170-209`). Any **template** with
  that name counts as the prepared image. Since #335 a non-template VM with that name
  fails closed, but a template does not.
- The checksum is verified only on the import path (`image.go:414`), so a VMImage that
  pins `checksum` is never protected against an existing template.
- The import lands in `resolveImageFolder` (`:580`). That function falls back to the
  datacenter VM folder on **any** finder error (`:583`), including a transient one, so
  retries can target different folders.
- `prepared_image_id` is the bare name (`imagePrepareDone`, `:298`). `Create` resolves
  it again by bare name across the whole datacenter (`server.go:2387`).

**libvirt** (`internal/providers/libvirt/image.go`):
- The artifact is `<poolPath>/<targetName>.qcow2` (`targetImagePath`, `:161`;
  `imagePrepare`, `:236`). If a file exists at that path, the image counts as prepared
  (`:242-260`). The only guards are the reserved-name check (`:212`, #334) and the
  in-use check (`:247`).
- `targetImageExists` (`:326-332`) treats **any** probe error as "does not exist". The
  prepare then runs `qemu-img convert` straight onto the final path (`:359`), which
  overwrites whatever is there. A failed convert then runs `rm -f` on that final path
  (`:366`). Both are fail-open in the dangerous direction: they overwrite, and they
  delete.
- `convert` writes the final path in place. A concurrent prepare's `stat` sees the
  partial file and treats it as prepared.
- The download temp file is `.virtrigaud-imageprepare-<targetName>.download` (`:297`).
  Two concurrent prepares of the same name share it.
- For a source that sets both `path` and `url`, the two sides disagree. The manager
  treats it as an import, because the URL wins (`imageSourceNeedsPrepare`,
  `virtualmachine_image_prepare.go:365`). The provider converts the **path** instead
  (`image.go:265`). Once artifacts carry stamps, that would make a stamped copy of any
  file in the allowed image directories.
- `finalizeClonedDisk` (`clone.go:303-312`) runs `chown libvirt-qemu:kvm` and
  `chmod 777` on the prepared image (`image.go:283`, `:317`). That leaves the image,
  which every VM is later copied from, writable by any user on the host.

**Proxmox** (`internal/providers/proxmox/image.go`, `pveapi/client.go`):
- The URL-import gate (`imagePrepareImport`, `:334-346`) calls `findTemplateByName`
  (`:313`). That function returns the **first** template with the name. PVE names are
  not unique.
- `PrepareImage` (`pveapi/client.go:1076`) only calls `download-url` with
  `filename=<targetName>.<format>` (`:1094`). **Nothing turns the download into a
  template VM.** `prepared_image_id` is the name (`image.go:369`), but `Create`
  requires a numeric VMID (`server.go:238-243`, `strconv.Atoi`).
- So the Proxmox URL-import prepare does not work end to end today. The name gate
  reports another party's same-named template as "prepared", and the `source.http`
  checksum is never passed to PVE.

### What goes wrong, now that sharing is a supported setup

| # | Scenario | Result today |
|---|---|---|
| A | `team-a` and `team-b` share a Provider. `team-a` prepares `VMImage ubuntu` from a malicious OVA first. `team-b`'s own `ubuntu` VMImage (even one that pins a checksum) then prepares. | `team-b`'s VMs are cloned from `team-a`'s template. **Cross-tenant template poisoning.** |
| B | The reverse order. | `team-a`'s VMs boot `team-b`'s image content. **Cross-tenant disclosure.** |
| C | Two unrelated `ubuntu` VMImages in two namespaces, with two Providers whose accounts reach the same folder, pool, or node. No sharing is involved. | The two images collide. Whichever prepares first wins. |
| D | Two concurrent libvirt prepares of the same name (two Providers, one host). | They race on one download file and on one convert target. |
| E | A VMImage is deleted and re-created under the same name with a different source. | The old artifact is reused silently. |
| F | A VMImage's `spec.source` changes. | Nothing re-prepares. VMs keep using the old artifact. #344 does not change this. |
| G | A same-named artifact already exists: legacy, manual, or placed out of band. | Adopted without question. libvirt may overwrite it on a probe error. |

#344 stops the operator from **trusting another Provider's
record**. It cannot stop a provider from **finding another tenant's artifact under the
same bare name**. #343 makes A and B reachable by design. C, D, E and G happen without
any sharing at all.

### Why neither a name change alone nor a stamp alone is enough

- **Namespacing the name alone** (the #339 approach) fixes C. It does not fix G: a
  pre-existing or planted object at the new name is still adopted. It does not fix E
  or F either: a re-created VMImage or a changed source keeps its name.
- **A stamp alone, with bare names kept,** fixes A and B by failing closed. But then C
  is a permanent denial of service: `team-a`'s `ubuntu` blocks `team-b`'s `ubuntu` for
  good.
- The fix therefore needs both: **distinct names**, so legitimate tenants never
  collide, and **verified provenance**, so a name collision (planted, legacy, or a hash
  collision) is refused and never reused. #333/#335 did the same for VMs: a namespaced
  name plus an owner stamp, with a bind only on a matching UID.

---

## Decision

### D1: An artifact's identity is `(VMImage UID, source digest)`, and its name is derived from that identity by the provider

The prepared artifact for a VMImage is identified by the VMImage's **UID** and the
**source digest** (D2). The hypervisor-side name is derived from both. The rule lives
in the provider, following #339's "the provider owns this rule; the operator never
derives the name itself" (`domain_naming.go`, `importedDiskVolumeName`):

```
h16     = hex(sha256("v1/" + image.uid + "/" + source_digest))[:16]
prefix  = namespace + "." + name, cut to (budget - 1 - 16) bytes,
          then trailing '.' and '-' trimmed
name    = prefix + SEP + h16
```

Per provider, `budget` and `SEP` are:

| Provider | budget (bytes) | SEP | Source of the constraint |
|---|---|---|---|
| vSphere | 80 | `_` | vSphere entity names are at most 80 characters; `unsafeNameReason` (`vm_ownership.go`) forbids `/ \ % :` and the MOID shape `vm-<digits>` |
| libvirt | 200 (base name, before `.qcow2`) | `_` | #339's 200-byte bound, which leaves room for suffixes under the 255-byte Linux file-name limit |
| Proxmox | 80 | `-` | PVE validates `name` as a DNS name, which does not allow `_`. The name is cosmetic on PVE (D3/D5). |

Properties the rule guarantees:

1. **The UID is in the name** (through the hash). A VMImage that is deleted and
   re-created under the same name gets a **new** artifact. It never inherits the old
   one's content, and it never runs into a Conflict dead end against it (scenario E).
2. **The digest is in the name.** A changed `spec.source` produces a new artifact
   (scenario F) instead of a Conflict at the old name. Artifacts are immutable once
   published.
3. **A new-scheme name never equals a legacy or domain name** on vSphere or libvirt.
   It always contains `_`, and no Kubernetes name can contain `_`. The legacy bare
   `<vmimage>` name and #339's `<namespace>.<name>` domain form therefore cannot
   collide with it. This is #339's `_` argument, applied to images.
4. **Names cannot be predicted in advance.** An attacker who can pick a namespace or
   a VMImage name still cannot produce another image's name. The hash covers a UID
   that the API server assigns at creation, so a name cannot be planted before the
   VMImage exists. **This is not an access control:** UIDs and artifact names are
   visible to anyone who can read the owner's VMImage or the hypervisor. By-reference
   access stays under the credentials-reach rule until Slice 7.
5. **Every name has the same shape.** Unlike #339, there is no separate "untruncated"
   form. Every artifact name ends in `SEP` plus 16 hex digits, so there is no
   regular-versus-truncated injectivity case to reason about. The namespace (a DNS
   label, at most 63 bytes) always fits whole in the prefix: 80 − 17 = 63.
6. **libvirt reserved names cannot match.** The suffix `_<16 hex>.qcow2` never ends in
   a #334 reserved suffix (`-disk.qcow2`, `-migrated.qcow2`, `.download`, `.partial`,
   …). The prefix starts with a namespace, which begins with an alphanumeric character,
   so the file is never a dotfile.

**Two namespaces can never collide.** The namespace is always whole and always
followed by `.`, a character a namespace cannot contain. Within one namespace, names
differ only through `h16`. Safety depends on neither of these facts: it rests on the
stamp check (D4).

Example: VMImage `team-a/ubuntu-22.04` → vSphere template
`team-a.ubuntu-22.04_3c9e1f0a7b2d4e61`, libvirt file
`team-a.ubuntu-22.04_3c9e1f0a7b2d4e61.qcow2`, Proxmox template name
`team-a.ubuntu-22.04-3c9e1f0a7b2d4e61`.

**In identity mode, a libvirt source with both `path` and `url` is `InvalidSpec`.**
Today the manager treats that combination as an import from the URL, while the provider
converts the path (`image.go:265`). An identity-mode prepare would therefore stamp a
copy of any file in the allowed image directories as that image's artifact. An
identity-mode import takes exactly one input: a `url` (libvirt), an `ovaURL` (vSphere),
or an `http.url` (Proxmox). A libvirt `path` alone stays a reference-style source and
is never prepared (`imageSourceNeedsPrepare`).

When `image` is set, the new manager sends an **empty** `target_name` (D7).

### D2: The source digest is computed by the manager over `spec.source` only, and only its hash reaches the hypervisor

```
source_digest = "sha256:" + hex(sha256(canonicalJSON({"v": 1, "source": vmImage.Spec.Source})))
```

- **Canonical form:** Go `encoding/json` of the typed `v1beta1.ImageSource` inside the
  versioned envelope, with the excluded fields below cleared first. Struct field order
  is fixed and map keys are sorted, so the output is deterministic. Bumping `"v"`
  renames every artifact. That is a deliberate migration, never an accident. A
  golden-vector test pins it.
- **What it covers:** `spec.source`, meaning every field that defines content
  (URL/path, expected checksum and algorithm, format) and also the location fields
  (`storagePool`, `storage`, `node`). Including an extra field only ever costs a
  re-import. Leaving out a field that matters would reuse the wrong content.
- **What it excludes:**
  - The transport-only fields of `source.http`: `timeout`, `headers` and
    `authentication`. They change how bytes are fetched, not which bytes are expected.
    Rotating an inline token in a header must not orphan multi-GB artifacts.
  - `spec.metadata`, `spec.distribution` and `spec.consumerNamespaceSelector`. They do
    not change artifact content, and editing a selector must not re-import.
  - `spec.prepare`, for now. No provider reads it today.
- **Any `spec.prepare` field a provider starts honouring must join the digest in the
  same change.** For example: `storage.*`, `preferredFormat`, `validateChecksum`,
  `optimization`. Such fields change the artifact, and leaving them out would reuse an
  artifact built under the old options.
- **Why the manager computes it:** it has the typed spec, the rule is
  hypervisor-agnostic, and it lives in one place. The provider checks the syntax
  (`sha256:` plus 64 lowercase hex) and uses it verbatim. The manager is the trust root
  for identity anyway (mTLS, ADR-0003).
- **Only the digest is stored on the hypervisor.** Stamps (D3) carry the digest, never
  the URL (which can hold user-info or presigned tokens), never header values, and
  never secret references.
- **CRD caveat:** if a future release adds a *defaulted* field to `ImageSource`, the
  API server fills it in, the digest changes, and every image re-prepares once. Such a
  field must either be left out of the digest or carry no default.

### D3: Every prepared artifact carries a provenance stamp

**Stamp v1 contents:** `stampVersion=1`, `image.uid`, `image.namespace`, `image.name`,
`sourceDigest`, `preparedBy` (the Provider's `namespace/name/uid`, from the request, D7),
`preparedAt` (RFC 3339). Only `image.uid` and `sourceDigest` are authoritative (D4). The
rest is for audit and operators, like `ObjectIdentity`'s informational namespace and
name. The stamp holds no secrets and no URLs.

| Provider | Where the stamp lives | Written when | Tamper surface |
|---|---|---|---|
| **vSphere** | ExtraConfig keys `virtrigaud.image.{stampversion,uid,namespace,name,sourcedigest,preparedby,preparedat}`, inside the reserved `virtrigaud.` prefix (`ova_import.go:34`) | In the import spec's `ConfigSpec.ExtraConfig`, **after** `stripReservedExtraConfig` (`ova_import.go:55`) and **before** `ImportVApp`, so the entity carries the stamp from the moment it exists and an OVF can never supply one | Needs `VirtualMachine.Config.AdvancedConfig`, which #335 already requires. A template cannot be reconfigured without first being converted back to a VM (`MarkAsVirtualMachine`), so the stamp is effectively frozen once the template is marked |
| **libvirt** | A dot-sidecar next to the artifact: `.<artifact-base>.virtrigaud-image.json` (JSON, at most 4 KiB). It also records the artifact's `inode` and `size`, and both must match the artifact file (D4) | Published **before** the artifact (D6), so an artifact present implies a sidecar present | The same host principals that can write the pool directory. The artifact itself is read-only, mode `0444` (D6). A dotfile is refused as a base image by `reservedImageName` (`imagepath.go:341`), and staging files already rely on the same "dotfiles are not pool volumes" behaviour (`image.go:292-296`) |
| **Proxmox** (Slice 6) | The template VM's `description` (a `virtrigaud-image v1 … vmid=<n>` block) plus tags `virtrigaud-image` and `vr-img-<h16>`. The stamp's `vmid` **must equal the object's own VMID** | Set on the VM-create call that builds the template, before it is converted with `/template` | Weak on its own. Any holder of `VM.Config.Options` on the VM can write the tags and description. The tag lookup runs across the whole cluster. Clones copy tags. `/cluster/nextid` recycles VMIDs. Hence the Slice 6 rules in D5 |
| **mock** | In memory | On import | n/a |

**Clones must not carry an image stamp.** vSphere clones copy ExtraConfig, and PVE
clones copy `description` and tags. `Create` and `Clone` therefore **clear**
`virtrigaud.image.*` on vSphere (set to empty values; vSphere removes an empty key, the
same mechanism as `ownerExtraConfig` in `vm_ownership.go:161`). On Proxmox they replace
the description and tags. On vSphere this is hygiene, because a non-template is never an
artifact. On Proxmox it is **required**: without it, every clone looks like an
in-progress prepare to the tag-based lookup.

**Stamp parsing fails closed**, with the same rules as `ownerFromExtraConfig`: a
repeated key, a non-string value, an unknown `stampVersion`, a malformed digest, or
(libvirt) an oversized or non-JSON sidecar all make the stamp **untrusted**. That is the
same as having no stamp.

**libvirt stamp options that were evaluated:**

| Option | Verdict | Why |
|---|---|---|
| Dot-sidecar per artifact | **Chosen** | Works on every filesystem-backed pool, which `imagePrepare` already requires (`poolInfo.Path`, `image.go:231`). It can be published atomically and in order with `ln` (D6), is invisible to pool refresh and to #334 confinement, and needs one small read per probe |
| `user.*` xattr on the qcow2 | Rejected | Depends on the filesystem (NFSv3 has none; NFSv4.2 only on recent kernels), is lost on copy, needs `attr` tools on the host, and is writable by the same principals anyway |
| One pool-wide manifest | Rejected | A single mutable file shared by all tenants needs cross-host locking, and one corruption breaks every image |
| Metadata inside the qcow2 | Rejected | qcow2 has no user key-value area that `qemu-img` exposes |
| libvirt volume XML | Rejected | Directory pools do not persist per-volume metadata: the XML is re-derived from the file on every refresh |

### D4: Reuse only on a matching stamp; everything else fails closed

An existing object at the derived name (vSphere, libvirt), or carrying the `vr-img-<h16>`
tag (Proxmox), is **reused** only when all of the following hold:

1. **It is complete.**
   - vSphere: `config.template == true`.
   - libvirt: the artifact file and the sidecar both exist, and the artifact's inode
     and size equal those recorded in the sidecar.
   - Proxmox: `template == 1`, and the stamp's `vmid` equals the object's VMID.
2. **Its stamp parses** (D3).
3. **`stamp.image.uid == request.image.uid`.**
4. **`stamp.sourceDigest == request.source_digest`.**

`image.namespace`, `image.name` and `preparedBy` are never consulted. Because the name
already encodes (3) and (4), a mismatch at the derived name means one of three things:
something was planted, a stamp was edited, or there is a 2^-64 hash collision. All three
must be refused.

| Observed | Outcome |
|---|---|
| Nothing at the name | Import (D6) |
| Complete, and the stamp matches | **Reuse.** Return the location and `reused=true` |
| Incomplete, the stamp matches, and the object is live | In progress: a retryable `Unavailable`. The manager requeues |
| Incomplete, the stamp matches, and the object is **not** live | Abandoned by a crashed prepare of **this same image**. The provider may remove **only** that object (vSphere: the powered-off non-template in the import folder; libvirt: the matching sidecar that has no artifact, and own-pattern temp files), then import again |
| Anything else: no stamp, an untrusted stamp, another UID, another digest, or (vSphere) a non-template with no matching stamp | **Conflict** (`codes.AlreadyExists` → `contracts` `Conflict`, `internal/transport/grpc/client.go:1273`). Never overwrite, delete, re-stamp, or adopt |
| The probe itself fails (a vCenter error, an SSH/`stat` error, a PVE API error) | A retryable error. **Never** treated as "absent" (this fixes `targetImageExists`, `image.go:326`) |

**Liveness and the staleness bound.** There is one bound,
`max(2 × spec.prepare.timeout, 2h)`. It is the requester's value and there is no env
knob yet. It applies to temp files and incomplete artifacts alike.

- **libvirt** ages files by **mtime**. Active writes (`curl`, `qemu-img convert`) keep
  a temp file's mtime current, so a file older than the bound is not being written.
  The same rule covers a sidecar that has no artifact.
- **vSphere** ages the object by the stamp's `preparedAt`. Cleanup **also** requires
  that the entity has no running task in its `recentTask` and no active
  `HttpNfcLease`. Age alone never licenses a destroy.

The Conflict message is uniform and names only the requester's own artifact, following
#335's `vmConflictError`: *"a prepared-image artifact named X exists at this Provider's
image location but was not prepared for this VMImage; refusing to use or replace it"*.
The recorded owner goes to the provider log only.

**No cross-image deduplication by digest.** Two different VMImages with an identical
source still get two artifacts. Reasons:

- Without a pinned checksum, the digest is only a hash of the URL, so equal digests do
  not mean equal content.
- A shared cache would recreate cross-tenant coupling: one image's lifecycle would
  control another's.
- It would be an existence oracle: "someone already prepared this checksum".

See Alternative 5.

### D5: One artifact per `(VMImage identity, source digest, hypervisor location)`, shared by every Provider that resolves to that location

- **A shared VMImage is prepared once per location.** When `virtrigaud-system/ubuntu`
  is shared by `consumerNamespaceSelector`, VMs in every granted namespace consume the
  **same** artifact through any Provider that resolves to the same location. **This is
  the intended model.** The VMImage owner decides the content, and a namespace that is
  granted the image has accepted that content.
- **The location is whatever the requesting Provider resolves to.**
  - vSphere: the import folder in the default datacenter.
  - libvirt: the pool directory on the host.
  - Proxmox: the cluster, via the tag lookup, with the template's disk on the resolved
    storage.
  - Providers that resolve to the same location share one artifact, which avoids
    duplicate multi-GB imports. Providers at different locations get separate copies.
- **`preparedBy` is recorded but is not part of the reuse rule.** The trust boundary
  for an artifact is **write access to its location**. A principal who can write there
  can replace the content of any artifact, a per-Provider one included. So per-Provider
  artifacts would add storage and import time without adding protection. They would
  also force a full re-import every time a Provider is re-created (a new UID). Every
  Provider re-checks the stamp through its **own** credentials before reusing an
  artifact. There is no per-Provider opt-in (Alternative 4, Q2).
- **The multi-tenant default is one import location per tenant Provider.** Give each
  tenant's Provider its own vSphere import folder (`DefaultFolder`), its own libvirt
  pool, or its own Proxmox storage and PVE pool. Scope that Provider's hypervisor
  account to its location. Sharing a location is for Providers that are trusted with
  each other's artifacts. The docs (Slice 9) state this.
- **vSphere lookups are scoped to the import folder.** They no longer search the whole
  datacenter. The probe uses `vmsNamedInFolder` (`vm_ownership.go:290`, the #335
  Create-ownership lookup) on the resolved import folder. vCenter keeps VM names unique
  within a folder, which is exactly the uniqueness scope the probe needs, and it gives
  an atomic create-if-absent (D6).
  - `resolveImageFolder` falls back to the datacenter VM folder **only when
    `DefaultFolder` is empty**. A configured folder that does not resolve (missing,
    ambiguous, or a vCenter error) is a retryable error, so every retry resolves the
    same location. That is stricter than `resolveVMFolder` (`vm_ownership.go:254`):
    an artifact that lands in an unintended folder would be shared with whoever can
    read that folder.
  - A multi-VM OVF (one that yields a `VirtualAppImportSpec`) is `InvalidSpec`. An
    artifact is a single template VM.
  - `prepared_image_id` becomes the template's **absolute inventory path**.
    `lookupTemplate` resolves that path exactly (`FindByInventoryPath`) and
    `absoluteTemplatePathError` accepts it, so `Create` clones the verified template
    and not whatever same-named template exists elsewhere in the datacenter.
- **Proxmox (Slice 6):**
  - `prepared_image_id` is the VMID, the only template reference `Create` accepts
    (`server.go:240`).
  - Because a PVE stamp is weak (D3), lookups and imports are **limited to one PVE
    pool** (`/pools`), whose ACL grants VM rights only to the provider's API token.
    Tagged objects outside that pool are ignored.
  - Slice 6 also includes the **create-time stamp check** (Slice 7's check, brought
    forward for Proxmox). Before cloning, `Create` re-reads the template's stamp and
    requires UID, digest and `vmid` to match. That catches a template edited, re-tagged
    or recycled (a reused VMID) between prepare and create.
- **Gotcha:** two Providers that share a location but have different permissions (for
  example, B cannot read A's datastore) will reuse an artifact that B's `Create` then
  cannot clone. That fails honestly at create time. Operators should give Providers
  that share a location consistent permissions, or give them separate locations.
- **Reference-style sources are unchanged.** `templateName`, `contentLibrary`,
  `templateID` and a libvirt pool `path` still name existing hypervisor objects
  directly. That is the documented "sharing a Provider shares everything its
  credentials reach" rule (`docs/cross-namespace-references.md`). Slice 7 closes the
  remaining gap for **stamped** artifacts.

### D6: Staging is private to each prepare, and publishing is atomic and never overwrites

- **libvirt:**
  - Each prepare makes its own files in the pool directory with `mktemp`, using the
    host-side helper `makeHostTemp` (`internal/providers/libvirt/staging.go:94`, the
    #339 precedent): `.virtrigaud-imageprepare-XXXXXXXXXX.download` for the download
    and `.virtrigaud-imageprepare-XXXXXXXXXX.partial` for the `qemu-img convert` output.
    Both names are dotfiles with reserved suffixes. They live on the pool's filesystem
    (not `/tmp`, see `image.go:292-296`) and are independent of the artifact name.
  - **Finalize the `.partial` before publishing it:**
    1. `chmod 0444` it.
    2. `restorecon` it, for the SELinux label.
    3. `sync -- <partial>`, so a crash never publishes unflushed data.
    4. `stat` it to get the inode and size the sidecar records.

    There is **no `chown`**. With `fs.protected_hardlinks=1`, the provider's SSH user
    cannot hard-link a file it does not own and cannot write, so a `chown` to
    `libvirt-qemu` would break the `ln` below. The artifact only ever needs to be read,
    by `qemu-img convert` in `CopyImageToVolume`.
  - **`finalizeClonedDisk` is never applied to a prepared artifact.** Its
    `chown libvirt-qemu:kvm` and `chmod 777` (`clone.go:303-312`) are what leave
    today's prepared image writable by any host user. The copies each VM gets from the
    artifact are finalized as they are today.
  - **Publishing** is `ln -- <sidecar.tmp> <sidecar>`, then `ln -- <partial>
    <artifact>`, then `rm` of the temp names.
    - `link(2)` fails with `EEXIST` instead of replacing an existing file. It is also
      the classic NFS-safe exclusive-create primitive. On an NFS `EEXIST` that may come
      from a retransmitted `LINK`, check `st_nlink` on the temp file.
    - **Only the call that created the sidecar links the artifact.** A call whose
      sidecar `ln` returns `EEXIST` re-runs the D4 probe:
      - a matching sidecar with no artifact yet means in progress (retryable), or
        abandoned once it is no longer live;
      - a complete match means reuse, and the call deletes its own temp files;
      - anything else is a Conflict.
    - **If the artifact `ln` returns `EEXIST` after our sidecar was published,**
      something else occupies the artifact name. Unlink **our** sidecar first, so it
      never stamps someone else's file, then return a Conflict.
  - A filesystem without hard links fails the prepare with an explicit error.
  - The provider never writes, `rm`s or `convert`s onto the final name.
  - Leftover own-pattern temp files that are not live (D4) are swept.
- **vSphere:**
  - The OVA download already uses `os.CreateTemp` inside the pod (`image.go:610`).
    Keep it.
  - The atomic create is vCenter's per-folder name uniqueness: `ImportVApp` into a
    folder that already holds the name fails with `DuplicateName`. On `DuplicateName`,
    re-run the D4 probe.
  - **This is load-bearing, so Slice 3 cannot merge until it is verified** on vcsim
    and in the lab. The convergence fallback is specified now and implemented anyway:
    after the import, re-list the folder. If more than one object has the name, every
    call whose own object is **not** the one with the lowest MOID destroys **its own**
    object (the moref it created) and reuses the survivor after the D4 probe.
  - `cleanupPartialImport` (`image.go:720`) keeps destroying **only** the moref this
    call created.
- **Proxmox (Slice 6):**
  - Slice 6 must first verify two things on the lab PVE: that `content=import` needs
    PVE 8.2 or later, and how `download-url` behaves when the target file already
    exists.
  - The `download-url` filename is random for each prepare,
    `vr-prep-<16 random hex>.<format>`, and it is deleted once the disk has been
    imported into the template VM. This removes the `<targetName>.<format>` collision
    (`pveapi/client.go:1094`).
  - When the VMImage pins `checksum`, the prepare passes `checksum` and
    `checksum-algorithm`, and PVE verifies them on the server.
  - PVE has no atomic unique name. When two concurrent prepares both finish, both
    re-list and **converge on the lowest VMID**. The loser deletes only the template it
    just created (it has no clones yet).

The manager-side single-flight from #344 removes duplicate work within one
manager for one `(image, Provider)`. The provider-side rules above handle the rest:
several Providers, several provider pods, and crashes.

### D7: Wire contract (additive; no proto major bump)

```proto
message ImagePrepareRequest {
  string image_json = 1;
  string target_name = 2;          // EMPTY when `image` is set; every provider rejects an empty target_name
                                   // today, so an old provider refuses instead of importing under a bare name
  string storage_hint = 3;
  ObjectIdentity image = 4;        // the VMImage being prepared; uid is authoritative (ADR-0009 D1/D4)
  string source_digest = 5;        // "sha256:<64 hex>" over the canonical spec.source (D2)
  ObjectIdentity provider = 6;     // the Provider the prepare runs through; informational (stamp.preparedBy)
}

message ImagePrepareResponse {
  TaskRef task = 1;
  string prepared_image_id = 2;    // vSphere: absolute inventory path; Proxmox: VMID; libvirt: artifact base name
  string prepared_image_path = 3;  // libvirt: absolute pool path
  PreparedArtifact artifact = 4;   // the stamp the provider verified or wrote; unset from an older provider
}

message PreparedArtifact {
  string name = 1;                 // hypervisor-side artifact name (D1)
  ObjectIdentity image = 2;        // from the stamp
  string source_digest = 3;        // from the stamp
  bool reused = 4;                 // an existing, matching artifact was reused
}

// GetCapabilitiesResponse
bool supports_image_artifact_identity = 18;
```

`ImportDisk` is **not** changed. Its landing disk belongs to one VM and is already named
after the target VM (#339 `importedDiskVolumeName`); it is not a shared artifact.
`CreateRequest` is not changed by the release-blocking slices. Verifying the stamp at
create time is Slice 7.

**The manager's gate, in order.** This amends ADR-0005 decision 2.

1. The provider does not implement `ImagePreparer`, **or** the Provider does not
   advertise `SupportsImageImport`: **fall through unchanged** to the by-reference
   create, exactly as ADR-0005 decision 2 says. Clustered libvirt reports
   `supports_image_import=false` (`libvirt/server.go:661`) and keeps behaving as
   today.
2. The Provider supports import but does not advertise
   `supportsImageArtifactIdentity`: **hold**. No RPC is sent. The VMImage entry and
   the VM get `Ready=False` with reason `ProviderLacksArtifactIdentity` ("upgrade the
   provider image").
3. The Provider advertises both: call `ImagePrepare` with `image`, `source_digest`,
   `provider`, and an **empty** `target_name`.

**Version skew:**

| Manager | Provider | Behaviour |
|---|---|---|
| New | New | Identity-safe (D1-D6) |
| **New** | **Old** (the normal window: providers roll last, see `docs/upgrading.md`) | **Fails closed** at gate step 2. VMs that already exist are unaffected, because prepare runs only before a create (#344) |
| New | Old, with a **stale** capability in status (for example a provider image that was rolled back) | The old provider receives an empty `target_name`. All three providers reject that today: vSphere (`image.go:227-229`), libvirt (`image.go:204-206`), and the Proxmox URL import (`image.go:262-265`). So it **refuses instead of importing under a bare name**. Any response with no `artifact`, or with an `image.uid`/`source_digest` that differs from the request, is **not recorded** |
| Old | New | The request has no `image.uid` and a non-empty `target_name`. **In this release only**, the provider serves it in **deprecated legacy mode** (below) and emits the deprecation signal. **From release N+1** it refuses such a request with `FailedPrecondition`: "the manager is too old for identity-safe image preparation; upgrade the manager" (Q3, Slice 10) |
| Old | Old | Unchanged (the known limitation) |

A request that has neither `image.uid` nor a `target_name` stays `InvalidSpec`, as it
is today.

**Deprecated legacy mode (this release only, Q3).** A new provider serves a request
from an older manager with **today's bare-name naming and reuse-by-name**:

- The response has the shape that manager expects: `prepared_image_id` is the bare
  name, and there is no `artifact`.
- This is the known limitation shipping once more. It can only be reached from a
  manager older than this release.
- Legacy mode cannot touch new-scheme artifacts, because the names are disjoint
  (D1.3).
- Legacy mode still gets the provider-internal fixes that do not change the response:
  - libvirt: a probe error is retryable and never counts as "absent" (D4); staging is
    private (`mktemp`); nothing is converted onto the final name or removed from it;
    publishing uses `ln`; the artifact is `0444` with no chown and no
    `finalizeClonedDisk` (D6).
  - vSphere: the pre-ADR code path, unchanged.
  - Proxmox: there is no working bare-name path to keep. A legacy `source.http` request
    gets the same `InvalidSpec` as an identity request (D10).

**The deprecation signal** covers every legacy request, Proxmox included:

- a provider-side `WARN` log: "deprecated: image prepare without identity from an older
  manager; upgrade the manager; refused from the next release";
- the provider counter
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type}`, next to the
  existing `virtrigaud_provider_*` metrics (`internal/obs/metrics/metrics.go`).

There is no response flag. Only older managers take this path, and they cannot read a
new field. A new manager never triggers legacy mode, because it always sends an
identity. The upgrade docs tell operators to alert when the counter is non-zero.

**The end of an async task does not prove the artifact.** When the `taskRef` of an
asynchronous prepare (Proxmox) completes, the manager sends `ImagePrepare` again. That
second call is idempotent, and its `artifact` echo is what gets recorded. A task that
succeeded is never, on its own, proof that the artifact exists.

### D8: Status records which source an entry was prepared for (additive CRD status)

- `ProviderImageStatus.sourceDigest` records the digest the entry was prepared for. It
  is written from the `artifact` echo.
- `ProviderStatus` entries also require #344's `providerUID`. A VM is
  created from an entry only when **all** of these hold:
  - the entry is `available`;
  - its `providerUID` matches the Provider's current UID (#344);
  - its `sourceDigest` equals the digest of the **current** `spec.source`.
- **The digest match is enforced in both places that consume an entry, for every
  source kind:**
  - in `overrideImageWithPreparedLocation` (`virtualmachine_controller.go:1874`);
  - in #344's short-circuit for an entry that is already available.

  Without it, a VMImage switched from `ovaURL` to `templateName` would keep cloning
  the old prepared artifact. A reference-style source never prepares, and the old
  entry would still override the image at create time. When the digests do not match,
  the entry is ignored:
  - an import-style source prepares again;
  - a reference-style source is used as written.
- **With `onMissing: Import`**, any other entry makes the controller issue
  `ImagePrepare` again before the create. That covers legacy entries, entries written
  before this ADR, and entries whose `spec.source` changed (these get a new artifact).
  VMs that already exist are unaffected.
- **With `onMissing: Fail` or `Wait`**, a prepare is forbidden:
  - An available entry with **no** `sourceDigest` holds the create, with reason
    `SourceDigestMissing`. The message tells the image's owner to switch `onMissing`
    to `Import`, so that the controller re-validates the entry through the Provider.
  - An entry whose digest differs means the image is not prepared for the current
    source, so the normal `Fail`/`Wait` handling applies.
  - No message ever asks anyone to write status. `VMImage.status` has a single writer,
    the VirtualMachine controller (ADR-0005 decision 3), and `vmimages/status` is
    granted to no one else. #344 already fixed its `ProviderUIDMissing` message this
    way.
- `Provider.status.reportedCapabilities.supportsImageArtifactIdentity` is surfaced
  from `GetCapabilities` (#176 machinery).
- The manager's CRD readiness check (`internal/controller/vmcrdfeatures.go`) requires
  both new fields, just as #344 requires `providerUID`/`taskRef`. An older
  CRD would prune them, and then nothing would ever be trusted. The release already
  requires applying CRDs first.
- `spec.prepare.force` stays unimplemented. If it is ever implemented it must produce a
  **new** artifact, for example by including a force generation in the digest. It must
  never overwrite.

### D9: Legacy artifacts are left alone; existing images move to the new scheme on their next create

- **Never delete, rename, re-stamp, or adopt** a bare-named artifact. Its origin cannot
  be proven, and it may still be in use:
  - libvirt images attached in place before #334;
  - linked clones made outside VirtRigaud;
  - `templateName` references written by hand.
- **VMs created from legacy artifacts are unaffected.** Today every `Create` makes a
  full copy:
  - vSphere full clone: default `DiskMoveType`, `server.go:2470`;
  - libvirt copy: `CopyImageToVolume`, `provider_virsh.go:450`;
  - Proxmox: `full=1`, `server.go:252`.
  Running VMs never prepare (#344).
- **Existing VMImages:**
  - After the upgrade, the first create for a given `(image, location)` finds an entry
    with no `sourceDigest` (D8). That triggers **one** re-prepare under the new name,
    which is one re-import. The legacy artifact becomes an orphan.
  - Docs (Slice 9) explain how to find orphans:
    - vSphere templates without `virtrigaud.image.uid`;
    - libvirt `<pool>/<name>.qcow2` files with no sidecar that `pathInUseOnHost`
      reports unused;
    - on Proxmox there are none, because the URL import never produced templates.
  - The docs also give a checklist to follow before deleting a legacy artifact by hand.
- **A reference-style VMImage** that names a legacy prepared template by its bare name
  keeps working. That is an explicit reference covered by the sharing rule. The docs
  recommend replacing it with the import-style source.

### D10: For this release, the Proxmox URL import fails closed instead of pretending

The Proxmox URL import produces no consumable template today (see *Context*). The
release-blocking slice therefore:

- removes the bare-name gate;
- makes `source.http` imports return `InvalidSpec` ("Proxmox URL import does not yet
  produce a template; reference an existing template by `templateID`");
- **advertises `supports_image_artifact_identity`**. The capability means "never reuses
  an artifact by bare name", and after this slice that is true. If Proxmox did not
  advertise it, the manager would hold such VMImages with a false "upgrade the provider
  image" reason. Advertising it lets the honest `InvalidSpec` reach the VMImage
  (`InvalidSource`) instead. It also means Slice 5 depends on Slice 1.
- answers identity-less (legacy) `source.http` requests with the same `InvalidSpec`, and
  logs and counts them as legacy (D7). Proxmox has no working bare-name path to keep.
  From release N+1 it refuses them with `FailedPrecondition`, like the other providers.

The complete Proxmox design (D1/D3/D5/D6 above) is Slice 6. The reference-style paths
(`templateID`/`templateName`) are unchanged, because the controller never sends them to
a prepare (`imageSourceNeedsPrepare`).

### D11: Errors, conditions and metrics

- **New VMImage condition reasons:**
  - `ArtifactConflict` (D4). A long requeue of 5 minutes, because an operator has to
    act.
  - `ProviderLacksArtifactIdentity` (D7).
  - `SourceDigestMissing` (D8).
- **The VM** reports `Ready=False` / `WaitingForDependencies`, as in #344's
  holds.
- **New counters:**
  - manager: `virtrigaud_image_prepare_artifact_total{provider_type, outcome}`, with
    `outcome` one of `created`, `reused`, `in_progress`, `conflict`,
    `abandoned_cleanup`;
  - provider, this release only:
    `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type}`, the
    deprecation signal for legacy mode (D7). It is removed together with legacy mode in
    Slice 10.
- **Events:** `Warning ImageArtifactConflict` on the VMImage, emitted only when the
  state changes.
- **Messages** never name another namespace, VMImage, or stamp owner. Those details go
  only to the provider log, following #335.

---

## Per-provider mapping

| | vSphere | libvirt | Proxmox (Slice 6; guarded by D10 until then) |
|---|---|---|---|
| Artifact | Template VM | `<pool>/<name>.qcow2` | Template VM |
| Name | `<ns>.<name>`(cut)`_<h16>`, at most 80 | same, at most 200 plus `.qcow2` | `<ns>.<name>`(cut)`-<h16>`, at most 80 (cosmetic) |
| Identity key used for lookup | Exact name in the import folder | Exact path in the pool | Tag `vr-img-<h16>` inside the provider's PVE pool, then the stamp |
| Stamp | ExtraConfig `virtrigaud.image.*` in the import spec | `.<name>.virtrigaud-image.json`, which also records the artifact's inode and size | `description` block with `vmid=<n>`, plus tags |
| Complete when | `config.template` | File and sidecar both exist, and inode and size match | `template=1` and the stamp's `vmid` equals the object's VMID |
| Atomic create | Folder name uniqueness (`DuplicateName`), with the lowest-MOID fallback | `ln` / `link(2)` `EEXIST`; only the call that created the sidecar links the artifact | Converge on the lowest VMID |
| `prepared_image_id` / `_path` | Absolute inventory path / empty | Base name / absolute path | VMID / empty |
| Staging | `os.CreateTemp` in the pod | `mktemp` dotfiles in the pool | Random `download-url` filename |
| Clones must clear the stamp | Yes (hygiene) | n/a (a copy has no sidecar) | **Yes (required)** |
| Checksum verified by | The provider, on the downloaded OVA | The host, `*sum` on the download | PVE `download-url` `checksum` |

**API reference:**

| Hypervisor | Call | Example |
|---|---|---|
| vSphere | `OvfManager.CreateImportSpec` → append the stamp to `VirtualMachineImportSpec.ConfigSpec.ExtraConfig` → `ResourcePool.ImportVApp(spec, folder)` → `MarkAsTemplate` | `{Key: "virtrigaud.image.uid", Value: "5f0c…"}`, `{Key: "virtrigaud.image.sourcedigest", Value: "sha256:9b1e…"}` |
| libvirt | `mktemp -p <pool> .virtrigaud-imageprepare-XXXXXXXXXX.partial` → `qemu-img convert -f <fmt> -O qcow2 <dl> <partial>` → `chmod 0444 -- <partial>` → `restorecon -- <partial>` → `sync -- <partial>` → `stat -c '%i %s' -- <partial>` → `ln -- <sidecar.tmp> <sidecar>` → `ln -- <partial> <artifact>` (on `EEXIST`: `rm -f -- <sidecar>`, then Conflict) → `rm -f -- <temps>` | sidecar: `{"stampVersion":1,"image":{"uid":"5f0c…","namespace":"team-a","name":"ubuntu-22.04"},"sourceDigest":"sha256:9b1e…","preparedBy":{"namespace":"team-a","name":"libvirt","uid":"…"},"preparedAt":"2026-09-25T10:00:00Z","artifact":{"inode":1835021,"size":2361393152}}` |
| Proxmox | `POST /nodes/{n}/storage/{s}/download-url` (`content=import`, `filename=vr-prep-<rand>.qcow2`, `checksum`, `checksum-algorithm`) → `GET /cluster/nextid` → `POST /nodes/{n}/qemu` (`vmid`, `name`, `pool`, `description` (with `vmid=<n>`), `tags`, `scsi0=<s>:0,import-from=<s>:import/vr-prep-<rand>.qcow2`) → `POST /nodes/{n}/qemu/{vmid}/template` → `DELETE` the staging volume | `tags=virtrigaud-image;vr-img-3c9e1f0a7b2d4e61` |

---

## Alternatives considered

1. **A stamp only, with bare names kept.** This fixes poisoning and disclosure by
   failing closed. It turns every same-named VMImage in another namespace into a
   permanent denial of service (scenario C). Rejected.
2. **`<namespace>.<name>` only, with no stamp** (the #339 approach). This fixes
   accidental collisions. A planted or legacy object at the new name is still adopted.
   A re-created image or a changed source reuses stale content. Rejected.
3. **`<namespace>.<name>` as the name, with the UID and digest only in the stamp.**
   The names are more readable. But a re-created VMImage or a changed source then hits
   a Conflict at its own name, and the only ways out are an operator deleting the
   artifact or trusting a stamp from the same namespace (weak: namespaces can be
   deleted and re-created by someone else). Rejected, and recorded as Q1 in case the
   maintainer prefers readability.
4. **One artifact per Provider** (the Provider UID in the name and in the reuse rule).
   This gives the strongest *VirtRigaud-side* separation. It gives no protection
   against principals with write access to the location. It duplicates multi-GB
   imports for Providers that share a location, and a re-created Provider re-imports
   everything. Rejected, with no opt-in (Q2).
5. **A content-addressed cache shared between images** (dedup by checksum). Attractive
   for storage. But it is safe only for sources with a pinned checksum, it couples
   image lifecycles across tenants, and it leaks existence. Deferred. It could come
   back later as an explicit, cluster-admin-owned image catalogue.
6. **A cryptographic stamp** (an HMAC or signature from the manager). It does not stop
   a principal with write access, who can replace the content under a valid stamp. A
   provider pod serving legitimate requests would hold valid MACs anyway. It adds key
   management. Over UID plus digest for the paths VirtRigaud controls, it adds nothing.
   Rejected.
7. **Names derived by the manager** (the manager sends the final `target_name`). This
   would spread hypervisor charset and length rules into the manager. The #339
   precedent is that the provider owns naming. Rejected. The digest *is* computed by
   the manager, because it does not depend on the hypervisor.
8. **Overwrite on mismatch** (delete and re-import). This breaks "never overwrite", and
   could destroy another tenant's artifact or a legacy linked-clone base. Rejected.
9. **A content hash of the libvirt artifact in the sidecar.** It detects corruption, not
   tampering: whoever can rewrite the file can rewrite the sidecar. It would cost a
   multi-GB hash on every create. Deferred (Q6).

---

## Consequences

### Positive
- Scenarios A through G are closed for every prepare VirtRigaud performs. A name
  collision can no longer bind content across tenants. The worst case is a clear
  Conflict.
- Deleting and re-creating a VMImage, or changing its source, now does what the user
  expects: a fresh artifact, with no dead end and no stale reuse.
- The libvirt defects that overwrite or delete on a probe error, the partial-file-seen-as-
  prepared race, and the shared download temp file are all removed.
- The Proxmox capability becomes honest (D10). Today its URL import only appears to
  work.
- Artifacts become traceable: every artifact records which VMImage (by UID), which
  source (by digest), and which Provider produced it.

### Negative / trade-offs
- **Storage grows with no garbage collection.** Every re-created VMImage and every
  source change leaves the previous artifact in place, and so do legacy artifacts.
  ADR-0005 already put GC out of scope. It needs its own design (Slice 8), because
  Proxmox linked clones pin their base.
- **One re-import per `(image, location)` after the upgrade** (D9). For large OVAs this
  is visible latency on the first create after the upgrade.
- **Names are less readable.** The `_<h16>` suffix is always present, and on vSphere
  the image name may be cut. The stamp and the VMImage status hold the full identity.
- **Old providers fail closed for new creates from import-style images** until they
  are upgraded (D7). This is deliberate, and the upgrade docs cover it.
- **The bare-name path ships once more.** For one release, new providers serve older
  managers in deprecated legacy mode (D7), so the known limitation stays reachable
  through a manager that has not been upgraded. It is flagged by the provider-side log
  and metric, and removed in release N+1 (Slice 10).
- **A restore mints new UIDs.** A Velero or DR restore re-creates `VMImage`s with new
  UIDs, which means one re-import per image and location. The old artifacts become
  orphans (Q1).
- **The vSphere artifact scope narrows from the whole datacenter to one folder.**
  Providers with different default folders no longer share one template. That is
  correct, but it costs one import per folder.

---

## Migration and upgrade

1. **Order is unchanged:** CRDs, then manager, then providers (`docs/upgrading.md`).
   - The CRDs add `ProviderImageStatus.sourceDigest` and
     `reportedCapabilities.supportsImageArtifactIdentity`.
   - The manager's readiness check fails until both fields exist (D8).
2. **Window: manager upgraded, providers not yet.** Import-style prepares are held with
   `ProviderLacksArtifactIdentity`. Existing VMs and reference-style images are
   unaffected. Roll the providers to clear the hold.
3. **After the providers roll:** the first create for each `(image, location)`
   re-prepares under the new name (D9). There is nothing to migrate by hand.
4. **Operator follow-up (optional):** once no VM or hand-written reference uses them,
   remove orphaned legacy artifacts with the Slice 9 checklist.
5. **Rollback:**
   - Rolling the manager back to an older release makes it send identity-less requests.
     **In this release**, a new provider serves them in deprecated legacy mode, with
     bare names (D7). New creates keep working, but the known limitation is back while
     the manager is rolled back. The legacy log and
     `virtrigaud_provider_image_prepare_legacy_requests_total` flag it. A manager
     rollback already gives up #340, #341 and #343.
   - **From release N+1**, a new provider refuses identity-less requests with
     `FailedPrecondition`, so new creates from import-style images fail until the
     manager is rolled forward again.
   - Either way, new-scheme artifacts remain and are reused on roll-forward. Legacy mode
     cannot touch them (D1.3).
   - Rolling a provider back makes the new manager hold (no capability). If the Provider
     status still shows the capability, the old provider rejects the empty
     `target_name` (D7). Either way this fails closed.
6. **Clustered libvirt** (ADR-0007): `ImagePrepare` stays `Unimplemented`
   (`libvirt/server.go:661`). When it gains `target_host_id`, the location is the pool
   on that host and the naming is unchanged.

---

## Security implications summary

| Threat | Status after this ADR |
|---|---|
| One tenant's prepared content served to another tenant's image (A) | **Closed.** Names differ (D1), and reuse needs a matching UID and digest (D4) |
| One tenant's image content exposed through the other tenant's same-named image (B) | **Closed for prepares. By-reference access is unchanged.** Artifact names are not secrets (D1.4). A `templateName`/`path` reference to another image's artifact is still governed by the credentials-reach rule until Slice 7 |
| An object at the name placed ahead of time, or left over from before this ADR (G) | **Fail-closed Conflict.** Placing an object ahead of time needs write access to the hypervisor **and** knowledge of a UID that does not exist yet, so it is a denial of service at worst, never a binding |
| An OVF that ships a forged stamp | **Closed.** `virtrigaud.image.*` sits under the reserved prefix that `stripReservedExtraConfig` removes before import, and the real stamp is added afterwards (D3) |
| libvirt probe errors that overwrite or delete, and races on shared temp files (D) | **Closed** (D4, D6), in legacy mode too |
| The bare-name path, shipped once more as deprecated legacy mode (D7, Q3) | **Open for one release, and only through an older manager.** A manager that has been upgraded never sends an identity-less request. Every legacy request is logged and counted on the provider (`virtrigaud_provider_image_prepare_legacy_requests_total`). Legacy mode cannot reach new-scheme artifacts. Release N+1 refuses such requests with `FailedPrecondition` (Slice 10). Operators should alert on the counter and must not run an older manager against a Provider shared across tenants |
| Secrets in stamps | None. Stamps hold a digest only, never URLs, headers, or secret references (D2) |
| Information disclosure in errors | Uniform messages. Owner details go only to the provider log (D11) |
| A principal with direct write access to the location who forges a stamp or swaps content | **Not addressed. This is the trust boundary.** The mitigation is scoping hypervisor accounts (the existing guidance in `docs/cross-namespace-references.md`). Slice 7 adds a check at create time |
| Forged `VMImage.status` | The manager is the only writer (ADR-0005 D3). The image owner can only affect its own consumers, who already trust that owner's content. Do not grant tenants `vmimages/status` |
| Reference-style sources reaching any template the account can read | Unchanged by design ("sharing a Provider shares everything its credentials reach"). Slice 7 refuses references to *stamped* artifacts of other images |

No new RBAC for the manager. No new vCenter privilege beyond #335's
`VirtualMachine.Config.AdvancedConfig`. The exact PVE privileges for Slice 6 (probably
`VM.Allocate`, `VM.Config.Options`, `VM.Config.Disk`, `Datastore.AllocateSpace`,
`Datastore.AllocateTemplate`, `Sys.Modify` for `download-url`) are confirmed in that
slice.

---

## Open questions (maintainer decides)

Q1, Q2, Q3 and Q7 were decided by the maintainer on 2026-09-25. Q3 differs from the
review's recommendation. Q4, Q5 and Q6 are still open.

- **Q1: UID in the name.** *Decided (2026-09-25):* **yes**, through `h16`.
  - A re-created VMImage gets a fresh artifact instead of a Conflict dead end, and a
    name cannot be planted before the VMImage exists.
  - The cost is one re-import and one orphan per re-creation. A Velero or DR restore
    mints new UIDs, which means one re-import per image; the docs say so.
  - The alternative is Alternative 3.
- **Q2: sharing across Providers at one location.** *Decided (2026-09-25):* **share one
  artifact per image, source and location, with no per-Provider opt-in.**
  - Each Provider re-checks the stamp through its own credentials before reusing it.
  - Per-Provider names give no protection against anyone who can write the location.
  - The docs name **one import folder, pool or storage per tenant Provider** as the
    multi-tenant default (D5).
- **Q3: requests that carry no identity.** *Decided (2026-09-25):* **keep a legacy
  fallback for one release, then refuse.**
  - In this release, a new provider serves an identity-less request (from an older
    manager) with today's bare-name behaviour, as **deprecated legacy mode**. Each
    request raises a provider-side `WARN` log and increments
    `virtrigaud_provider_image_prepare_legacy_requests_total` (D7).
  - Release N+1 refuses such requests with `FailedPrecondition` (Slice 10).
  - The review had recommended refusing now. The maintainer chose a one-release grace
    period for managers that are not upgraded.
  - The other skew direction is unchanged and still fails closed: a new manager sends
    an empty `target_name`, which an old provider, or one with a stale capability,
    rejects.
- **Q4: the staleness bound.** *Recommended (pending maintainer decision):*
  `max(2 × spec.prepare.timeout, 2h)` everywhere, together with the liveness checks in
  D4 (libvirt mtime; vSphere no running task or NFC lease). There is no 1h floor and no
  env knob yet.
- **Q5: if the release date cannot absorb Slices 1-5.** *Recommended (pending maintainer
  decision):* **do not ship on the stopgap.** The stopgap would be #344's "known
  limitation" text plus a manager-side refusal of import-style prepares through any
  Provider whose `consumerNamespaceSelector` is set. That covers scenarios A and B only
  through a *shared Provider*. It does not cover C, D, E or G.
- **Q6: content hash for libvirt artifacts** (Alternative 9). *Recommended (pending
  maintainer decision):* **no.** Whoever can rewrite the file can rewrite the sidecar.
  The inode and size check (D3/D4) already catches a swapped file at no cost.
- **Q7: the scope of the digest** (D2). *Decided (2026-09-25):*
  - **include** the location fields (`storagePool`, `storage`, `node`);
  - **exclude** the transport-only `source.http` fields `timeout`, `headers` and
    `authentication`. Rotating an inline token must not orphan multi-GB artifacts.

---

## Implementation slices

| # | Slice | Blocks the release |
|---|---|---|
| 0 | This ADR (Proposed → Accepted) | yes |
| 1 | **Proto, contracts and mock.** D7 fields and `PreparedArtifact`; `contracts.ImagePrepareRequest`/`Response`; manager gRPC client mapping; capability plumbing into `Provider.status.reportedCapabilities`; `ProviderImageStatus.sourceDigest`; the mock implements D1-D4 in memory; SDK types if they are exposed. Run `proto-update` and `crd-update`. **Lands after #344**, because both edit `ProviderImageStatus` | **yes** |
| 2 | **Manager.** D2 digest; the D7 gate order (fall through, then hold, then call) with an empty `target_name`; echo check; D8 digest match in `overrideImageWithPreparedLocation` and in #344's short-circuit for every source kind; the `SourceDigestMissing` hold under `Fail`/`Wait`; readiness check; Conflict and lack-of-identity holds with reasons, events and metric (D11); the async confirm call. Builds on #344's keying, `providerUID` and create-only prepare | **yes** |
| 3 | **vSphere.** D1 naming; stamp in the import spec; folder-scoped probe; folder resolution falls back only when `DefaultFolder` is empty; multi-VM OVF is `InvalidSpec`; `DuplicateName` handling **plus** the lowest-MOID convergence fallback; absolute inventory path as `prepared_image_id`; D4 rule including the liveness checks (no running task or NFC lease); Create and Clone clear `virtrigaud.image.*`; deprecated legacy mode for identity-less requests: the pre-ADR path plus the `WARN` log and the legacy counter (D7). **The merge is gated on verifying on vcsim and in the lab that `ImportVApp` returns `DuplicateName`** | **yes** |
| 4 | **libvirt.** D1 naming; `path`+`url` is `InvalidSpec`; sidecar with inode and size; `mktemp` staging; artifact finalized `0444` (chmod, restorecon, sync; no chown; never `finalizeClonedDisk`); `ln` publishing where only the sidecar's creator links the artifact and an artifact `EEXIST` unlinks our sidecar; a probe that fails closed (replaces `targetImageExists`); convert never touches the final name; mtime-based sweep; deprecated legacy mode for identity-less requests (bare name, with the D4/D6 provider-internal fixes, `WARN` log and legacy counter); capability | **yes** |
| 5 | **Proxmox guard** (D10): remove the bare-name gate; `source.http` import fails with `InvalidSpec`; **advertise** the identity capability; identity-less `source.http` requests get the same `InvalidSpec` and are logged and counted as legacy. **Depends on Slice 1** (the capability field) | **yes** |
| 9 | **Docs:** `docs/image-preparation.md`, `docs/cross-namespace-references.md` (replace "known limitation"; one import location per tenant Provider as the multi-tenant default), `docs/upgrading.md` (skew window, legacy mode on manager rollback, the legacy counter to alert on, and refusal from release N+1, orphan checklist, a restore means a re-import), release notes, `examples/`; CHANGELOG with each slice | **yes** (ships with 2-5) |
| 6 | **Proxmox, full design:** first verify the PVE 8.2 `content=import` requirement and the `download-url` overwrite behaviour on the lab PVE. Then: a template VM from `download-url` plus `import-from`, inside a provider-only PVE pool; description (with `vmid=<n>`) and tag stamp; VMID as id; server-side checksum; converge on lowest VMID; clones clear the stamp; **the create-time stamp check for Proxmox** | no |
| 7 | **Verification at create time:** an additive `CreateRequest.image` identity plus the expected digest; the provider re-checks the stamp of the artifact it clones or copies; references to *stamped* artifacts of **other** images are refused | no |
| 8 | **Artifact GC:** a separate ADR, covering a VMImage finalizer, reference counting for linked clones, and orphan reporting | no |
| 10 | **Remove the legacy fallback in release N+1:** every provider refuses identity-less requests with `FailedPrecondition`; delete the bare-name code paths and the legacy counter; flip the legacy tests to expect the refusal; update the upgrade docs | no (blocks release N+1) |

**Tests, per slice:**

- **Name rule (1, 3, 4):**
  - golden vectors;
  - property tests: the name never contains a character outside the provider's
    charset, never exceeds the budget, always ends in `SEP`+16 hex, never equals any
    DNS-1123 name or `<ns>.<name>` form, never matches a #334 reserved suffix, and is
    not MOID-shaped.
- **Digest (2):**
  - a golden vector;
  - edits to the selector, `prepare`, `metadata`, `http.timeout`, `http.headers` and
    `http.authentication` do not change it;
  - edits to the checksum, URL, format or a location field do;
  - map ordering is stable.
- **Stamp parsing (3, 4, 6):** repeated keys, non-strings, unknown version, oversized
  sidecar, and a truncated digest are all untrusted.
- **vSphere (vcsim):**
  - foreign same-named template, unstamped template, stamp with another UID, and stamp
    with another digest all give a Conflict with no import;
  - matching template gives reuse with no import;
  - non-template with a matching stamp is in progress; it is cleaned up only once
    stale **and** with no running task or NFC lease;
  - `DuplicateName` leads to a re-probe;
  - two objects with the same name converge on the lowest MOID;
  - a configured `DefaultFolder` that does not resolve is retryable, never a fallback;
  - a multi-VM OVF is `InvalidSpec`;
  - an OVF carrying `virtrigaud.image.uid` has it stripped and the real stamp present;
  - two concurrent prepares produce one template;
  - a clone of a prepared template carries no `virtrigaud.image.*`;
  - `Create` resolves the absolute path when a same-named template exists in another
    folder;
  - an identity-less request (non-empty `target_name`, no `image`) takes the pre-ADR
    bare-name path, returns the bare name as `prepared_image_id`, and emits the
    deprecation signal (a `WARN` log, and the legacy counter increments).
- **libvirt (host-command fakes):**
  - a `stat` error is retryable and no convert or `rm` is issued on the final name;
  - an artifact with no sidecar is a Conflict;
  - `EEXIST` on the sidecar `ln` with a matching stamp: in progress, or reuse once the
    artifact is present; our temp files are removed;
  - `EEXIST` on the artifact `ln` after our sidecar was published: our sidecar is
    unlinked, then a Conflict is returned;
  - `EEXIST` with another stamp is a Conflict;
  - a sidecar whose inode or size does not match the artifact is a Conflict;
  - the published artifact has mode `0444`, is never chowned, and `finalizeClonedDisk`
    is never called on it;
  - `path`+`url` in identity mode is `InvalidSpec`;
  - concurrent prepares get separate `mktemp` names;
  - an identity-less request takes the legacy path:
    - it uses the bare name `<pool>/<target_name>.qcow2`;
    - it emits the deprecation signal (a `WARN` log, and the legacy counter
      increments);
    - it still keeps the D4/D6 fixes: a probe error is retryable, and there is no
      convert or `rm` on the final name;
    - it cannot see or reuse a new-scheme artifact.
- **Proxmox (pvefake):**
  - `source.http` gives `InvalidSpec`, and no `download-url` call is made, with or
    without an identity;
  - an identity-less request is counted as legacy;
  - the identity capability is advertised.
- **Conformance** (`test/conformance`): the identity cases for **every** provider,
  mock included:
  - two images with the same name in two namespaces get distinct artifacts;
  - a foreign or unstamped artifact at the derived name is a Conflict with no import;
  - a matching artifact is reused;
  - an empty `target_name` with no `image` is refused (`InvalidSpec`);
  - an identity-less request with a `target_name` takes the legacy path and
    increments `virtrigaud_provider_image_prepare_legacy_requests_total` in this
    release. Slice 10 flips this case to expect `FailedPrecondition`.
- **Controller:**
  - gate order: no `SupportsImageImport` falls through unchanged; import support with
    no identity capability holds with no RPC;
  - with a **stale capability**, the empty `target_name` sent to an old-style fake
    provider is refused and nothing is recorded;
  - echo missing or mismatched: not recorded;
  - Conflict: reason and backoff;
  - **the digest check in `overrideImageWithPreparedLocation`**: an available entry
    with a different or missing digest is never used to override, including after
    `ovaURL` → `templateName`;
  - #344's short-circuit also requires the digest match;
  - digest change: re-prepare before create, running VMs untouched;
  - `Fail`/`Wait` with an entry that has no digest: `SourceDigestMissing` hold, and the
    message never asks for a status edit;
  - two namespaces' `ubuntu` images: distinct artifact names;
  - a shared image through two Providers at one location: one artifact, reused.
- **envtest:** the new status fields round-trip; an older CRD fails readiness.
- **Upgrade e2e:** start from pre-ADR state (bare-name artifacts, entries with no
  digest) and upgrade CRDs, then manager, then providers. Assert:
  - **exactly one re-prepare per image and location**;
  - legacy artifacts are untouched;
  - running VMs are untouched;
  - the `Fail`/`Wait` hold appears until `onMissing` is switched to `Import`.

  Add a **manager-rollback leg**: an old manager against new providers is served in
  legacy mode, and the legacy counter increments. New-scheme artifacts are untouched
  and are reused on roll-forward.
- **Lab validation on all three hypervisors** by the maintainer, before Accepted
  becomes Implemented. vCenter `DuplicateName` gates the Slice 3 merge. The PVE
  checks belong to Slice 6.
