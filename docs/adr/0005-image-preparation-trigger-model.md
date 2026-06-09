# ADR-0005: Image-preparation trigger model

## Status

**Accepted (2026-06-08)** — design settled by staff-architect before implementation;
this document records the decision that PR-5 of the image-prepare epic
([#154](https://github.com/projectbeskar/virtrigaud/issues/154)) implements. There
are no open questions: the trade-offs below were resolved up front.

**Implementation**: code-complete on `feat/imageprepare-controller-154`. PR-5 wires the
`ImagePrepare` provider RPC (PR-1…PR-4, already merged) into the controllers so that
applying a `VMImage` + a `VirtualMachine` that references it drives an image prepare
end-to-end through CRs. No proto change, no CRD **spec** change.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Related issues**:
- [#154](https://github.com/projectbeskar/virtrigaud/issues/154) — image preparation (the epic). PR-1…PR-4 added the proto RPC + per-provider implementations + the `contracts.ImagePreparer` capability + the manager-side gRPC `Client.PrepareImage`. PR-5 (this ADR) is the controller wiring.
- [#176](https://github.com/projectbeskar/virtrigaud/issues/176) — capability negotiation. Supplies `Provider.status.reportedCapabilities.supportsImageImport`, which the trigger consults as the advertised capability flag.
- [#189](https://github.com/projectbeskar/virtrigaud/issues/189) — the clone/bind status race. A two-actor status write race on `virtualmachines/status` produced an orphaned clone. This ADR avoids the same class of bug on `vmimages/status` by keeping a single writer.
- [#152](https://github.com/projectbeskar/virtrigaud/issues/152) — least-privilege RBAC. The new `vmimages/status` grant is scoped to get;update;patch on the VirtualMachine reconciler (the writer) only.

**Companion ADRs**: [ADR-0001](./0001-transport-grpc-and-capi-integration.md)
establishes gRPC as the manager↔provider transport that `Client.PrepareImage` rides.
The `ImagePreparer` optional-capability pattern mirrors the `Cloner` pattern introduced
for VMClone (#179) and the `CapabilityReporter` pattern from #176: a narrow interface
type-asserted from `contracts.Provider`, never widening the core `Provider` interface.

---

## Context

The image-prepare RPC (`ImagePrepare`) and its per-provider implementations landed in
PR-1…PR-4 of #154, but nothing **calls** it from the control plane: applying a `VMImage`
did nothing (the `VMImage` controller was a no-op stub), and a `VirtualMachine`
referencing it created the VM by reference without ever importing/preparing the image
into the provider. PR-5 closes that loop.

Two facts about the existing code shape the decision:

1. **The (image, provider) pair lives in the VirtualMachine reconcile, not the VMImage
   reconcile.** A `VMImage` is provider-agnostic — the same image can be referenced by
   VMs on different providers. Only when a `VirtualMachine` is reconciled do we know
   *which* provider needs the image prepared. The `VMImage` controller has no provider
   resolver wired and no (image, provider) pair to act on.

2. **`VMImageStatus` already carries every field the flow needs.** `Ready`, `Phase`,
   `Message`, `AvailableOn []string`, `ProviderStatus map[string]ProviderImageStatus`,
   `PrepareTaskRef`, `LastPrepareTime`, `ObservedGeneration`, `Conditions`. No CRD spec
   change is required; only status writes. `VMImageSpec.Prepare.OnMissing`
   (`Import`/`Fail`/`Wait`, default `Import`) gates behaviour.

3. **#189 proved that two controllers writing the same status subresource race.** The
   clone controller and the VM controller both wrote `virtualmachines/status` and
   produced an orphaned clone. The fix was a re-GET-under-`RetryOnConflict` seed and a
   single confirmed writer. The image-prepare flow has the same hazard amplified:
   multiple VMs on multiple providers can reference one `VMImage` concurrently.

---

## Decision

### 1. Lazy, VM-create-driven prepare (not eager)

The **VirtualMachine controller** is the trigger. `EnsureImageOnProvider` runs inside
`reconcileVM`, after provider `Validate` and before the create path. It is the only
place that holds the (image, provider) pair. The `VMImage` controller is **not** upgraded
into a competing driver — it stays a no-op backstop (see decision 4).

Rejected alternative — **eager prepare** (a `VMImage`-controller-driven prepare to a set
of providers named in a new `spec.prepareOn` field): it would require a CRD spec change
(`prepareOn`), wiring a provider resolver into the `VMImage` controller, and answering
"prepare to which providers, when?" with no VM to anchor the decision. Deferred (see
"Out of scope").

### 2. `ImagePreparer` is an optional capability; a double gate prevents regressions

`EnsureImageOnProvider` prepares **only** when BOTH hold:

- the provider instance type-asserts to `contracts.ImagePreparer` (the manager gRPC
  `Client` implements it; in-process fakes/older clients may not), AND
- the Provider CR advertises `Status.ReportedCapabilities.SupportsImageImport` (surfaced
  from the `GetCapabilities` RPC by #176).

If either is absent, `EnsureImageOnProvider` returns `(false, nil)` and the VM falls
through to **today's unchanged by-reference create path**. A non-preparing provider
behaves exactly as before — no silent no-op of a feature, and no attempt to call a
possibly-`Unimplemented` RPC. Reading the advertised flag from the **Provider CR status**
(not a live RPC) keeps the trigger cheap and avoids a second round-trip on every
reconcile; a nil `ReportedCapabilities` reads as `false` (fail-safe).

### 3. Single-writer status, always conflict-safe

The VirtualMachine controller is the **single writer** of the prepare-related `VMImage`
status fields (`ProviderStatus[provider.Name]`, `PrepareTaskRef`, `Phase`, `Ready`,
`AvailableOn`, `LastPrepareTime`, `Message`, the prepare `Conditions`). Every write goes
through one helper (`writeImageStatus`) that re-GETs the latest `VMImage` and calls
`Status().Update` inside `retry.RetryOnConflict`. Because each VM owns only its own
provider's `ProviderStatus` entry and merges rather than blind-overwrites, two VMs
preparing the same image on **different** providers cannot clobber each other. This is the
direct lesson of #189 applied to `vmimages/status`.

### 4. `VMImage` controller stays a no-op backstop

The lighter of the two options the design allowed. Upgrading the `VMImage` controller to
poll prepare tasks would require wiring a provider resolver into it (it has none) and
would reintroduce a second writer of the same status fields — the exact #189 hazard. So
the `VMImage` controller remains a no-op; the **triggering VM polls its own prepare to
completion** using the same provider instance. The `VMImage` controller's RBAC stays
read-only (`get;list;watch`); the `vmimages/status` write grant is added to the
VirtualMachine reconciler (the writer) only, per #152.

### 5. `Ready` is the OR across providers; `ProviderStatus`/`AvailableOn` are the
per-provider truth

`VMImageStatus.Ready` is set true once **any** provider reports the image `Available`
(`ProviderStatus[p].Available`), with `AvailableOn` listing those providers and
`ProviderStatus[p]` carrying the per-provider id/path/message. This matches the existing
print columns (`Ready`, `Providers=availableOn[*]`) and lets a single `VMImage` be "ready
on libvirt-a, not yet on libvirt-b" without an ambiguous top-level boolean. `OnMissing`
holds (`Fail`/`Wait`) record a `Ready=False` condition with a reason rather than
preparing.

### 6. Request shape

`ImageJSON` is `json.Marshal(vmImage.Spec)` — `{"source":{...},"prepare":{...}}`, exactly
what the provider-side parsers consume. `TargetName` is the `VMImage` name. `StorageHint`
is empty so the provider picks its default storage (datastore / pool / Proxmox storage).
Synchronous providers (empty `TaskRef`, e.g. libvirt/vSphere import-on-call) are stamped
complete immediately; asynchronous providers (non-empty `TaskRef`) persist the ref, set
`Phase=Importing`, and the VM requeues to poll `IsTaskComplete`.

---

## Out of scope (PR-6 follow-up — additive, no breaking change)

- **Make `Create` consume the prepared template.** PR-5 proves prepare *runs* through a
  CR and that status reflects it (`Importing`→`Ready`); it does **not** yet feed the
  prepared template's path/id back into `Create`. The libvirt `Create` path already
  resolves `url`/`path`/template sources itself
  (`internal/providers/libvirt/provider_virsh.go`), so the VM still creates regardless.
  Returning the prepared image path/id from the provider and having `Create` use it is a
  later, **additive-proto** PR (PR-6), not a PR-5 concern.
- **Eager prepare + a `VMImageSpec.prepareOn` field.** Would be a CRD spec change; deferred
  unless a concrete use case (pre-warming images independent of any VM) appears.
- **VMImage GC / un-prepare on provider.** Deleting a `VMImage` does not remove the
  prepared template from the provider today; out of scope for PR-5.

---

## Consequences

### Positive
- **Closes the #154 loop with no proto or CRD-spec change.** Pure controller wiring on
  existing status fields; v1beta1 stays put.
- **No regression for non-preparing providers.** The double gate means any provider that
  does not advertise/implement image import behaves exactly as before.
- **#189-class race avoided by construction.** One writer, always
  re-GET-under-`RetryOnConflict`, per-provider entries merged not overwritten — covered by
  a concurrent unit test.
- **Least-privilege preserved.** Only `vmimages/status` (get;update;patch) is added, and
  only to the reconciler that writes it.

### Negative / trade-offs
- **An image is prepared lazily, on first VM create, not ahead of time.** The first VM
  referencing an image on a given provider pays the prepare latency (mitigated by the
  async TaskRef poll + requeue, and by idempotency for every subsequent VM). Acceptable
  for PR-5; eager pre-warming is the deferred `prepareOn` follow-up.
- **The VM does not yet *use* the prepared template** (PR-6). For libvirt/vSphere this is
  invisible because `Create` resolves the source itself; it is called out in the CHANGELOG
  and here so the follow-up is not forgotten.
- **The `VMImage` controller remaining a no-op may look unfinished.** It is deliberate
  (decision 4) and documented in the controller's type doc — the VM controller is the sole
  driver to avoid a second writer.

## Security implications summary

| Item | Status |
|---|---|
| New RBAC | `vmimages/status` get;update;patch on the VirtualMachine reconciler only (#152). VMImage object stays read-only across the manager. |
| Secrets in status | None. Prepare status carries provider ids/paths/messages, never credentials. |
| Capability gate | Prepare runs only when the provider advertises `SupportsImageImport` AND implements `ImagePreparer`; fail-safe to the unchanged create path otherwise. |
| CRD impact | None. No v1beta1 schema change. |
