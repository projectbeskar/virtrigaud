# Blocking Decisions — ADR-0007 & ADR-0008

> **Purpose.** ADR-0007 (clustered provider) and ADR-0008 (pure-Go libvirt driver) were
> `Proposed` drafts when this worksheet was written. Both have since been accepted
> (2026-09-22). Five of their open questions **blocked the earliest implementation work**
> because they fix CRD/proto shape or rollout mechanics. This worksheet isolates those five so
> they can be decided first. Every other "Open implementation question" in the two ADRs can wait
> until its phase arrives.
>
> **How to use.** For each decision below: the options, trade-offs, and the draft's recommendation
> are laid out, followed by **what the choice commits you to**. Fill in **Decision** and
> **Rationale**. Once all five are marked, these fold back into ADR-0007/0008 (moving from "open"
> into the Decision sections), and the `crd-update` / `proto-update` work can begin.
>
> **Status:** ✅ resolved 2026-09-21 — all five decisions accepted (William Rizzo,
> @wrkode). These now fold into ADR-0007/0008; the `crd-update` / `proto-update`
> work is unblocked.

---

## Summary (mark here, detail below)

| # | Decision | Gates | Draft recommendation | Your call |
|---|----------|-------|----------------------|-----------|
| **D1** | Provider discriminator | ADR-0007 **P1** | `spec.topology` enum field | ✅ `topology` string enum, default `single` (2026-09-21) |
| **D2** | Host inventory → provider pod | registry constructor (ADR-0008 PR 2) | projected **Secret**, watched + hot-reloaded | ✅ single projected versioned Secret (2026-09-21) |
| **D3** | Intra-cluster migration trigger | ADR-0007 **P2** | discriminator on `VMMigration` | ✅ **new `VMHostMigration` kind** — overrides draft (2026-09-21) |
| **D4** | Driver flag surface + lifetime | ADR-0008 strangler rollout | *(open — leans env var, transient)* | ✅ per-family env var + status/metric audit + sunset (2026-09-21) |
| **D5** | Shadow-compare divergence budget | ADR-0008 **PR 4 → 5** | *(define numerically — see suggestion)* | ✅ 0-divergence, ≥14d prod, ≥2 restarts + ≥1 reboot, checklist (2026-09-21) |

---

## D1 — Provider discriminator *(ADR-0007 Q1)*

**Question.** How does a `Provider` declare it is a clustered, multi-host provider rather than a single-host one?

**Why it blocks P1.** P1 introduces the `Host`/`HostPool` CRDs, the scheduler, and `target_host_id`. The provider and controller need a switch to gate cluster behavior, and the CRD shape depends on how it's expressed.

**Options.**
- **A — `spec.topology: single | cluster` enum on the existing `Provider`.**
  - ✅ Flat type taxonomy (one libvirt provider type, two modes); purely additive; single-host path untouched; the *same* discriminator serves cloud-hypervisor later.
  - ❌ One `Provider` kind now branches into two behavior modes → more conditional logic in the provider + controller.
- **B — a distinct `libvirt-cluster` provider type** (new `ProviderType` enum value).
  - ✅ Explicit and self-documenting; clean separation of single vs cluster code paths.
  - ❌ Type proliferation (`libvirt-cluster`, `cloudhypervisor-cluster`, …); duplicated config schema across variants.

**Draft recommendation:** **A** (topology field).

**What A commits you to:** an additive `Topology` enum on `provider_types.go` (default `single`, so existing CRs are unchanged); provider + controller read it to gate cluster behavior; `examples/` + docs gain a `topology: cluster` sample. No breaking change.

> **Decision:** ✅ **Accepted 2026-09-21 — Option A.** `spec.topology: single | cluster`, an **open string enum** (default `single`), **not a bool**.
>
> **Rationale / notes:** Hypervisor type and topology are orthogonal axes; a string enum avoids type explosion and lets a future third mode be additive. Default `single` = no breaking change to existing providers. Folded into ADR-0007 D9 + Open question 1.

---

## D2 — Host inventory → provider pod *(ADR-0007 Q2)*

**Question.** The operator owns the `Host` list (as CRs), but the *stateless* provider must hold N live connections and answer `ListHosts`. How does the host list get **into** the pod?

**Why it blocks.** The `hostconn.Registry` constructor (ADR-0008 PR 2) cannot be written until its input source is fixed — this is the "only the constructor changes between 1 host and N" seam.

**Hard constraint.** The provider must **not** read the Kubernetes API — we just removed that access in #297 (dedicated no-RBAC ServiceAccount, no automounted token). So "provider watches `Host` CRs directly" is **ruled out**; re-adding API access would undo #297.

**Options.**
- **A — Provider controller projects the pool's `Host` CRs into a mounted Secret/ConfigMap; the provider watches the file and hot-reloads.**
  - ✅ Mirrors how credentials already reach providers (mounted, no API access); keeps the provider stateless; preserves the #297 hardening.
  - ❌ Dynamic add/remove-host reload semantics are unsettled (must not evict a connection mid-operation); ConfigMap 1 MiB limit at very large N (likely fine for realistic pools).
- **B — provider reads `Host` CRs from the k8s API.** ❌ *Ruled out* — breaks the no-API-access invariant and re-introduces the RBAC surface #297 removed.

**Sub-decisions (settle alongside A):**
- **Secret vs ConfigMap** for the projection — host addresses aren't secret, but adjacent SSH/known_hosts material is, so a **Secret** keeps it uniform with today's credential flow.
- **Reload contract** — host *added* → open its connection lazily on first use; host *removed* → drain/evict without breaking in-flight operations. This contract must be written down.

**Draft recommendation:** **A**, projected + watched; lean **Secret**; reload = lazy-open on add, graceful-drain on remove.

**What A commits you to:** the Provider controller gains logic to project `Host` CRs → a mounted, watched resource; the provider's `Registry` reads/watches it; a defined hot-reload contract.

> **Decision (mechanism):** ✅ **Accepted 2026-09-21 — Option A.** The Provider controller projects the pool's `Host` CRs into a **single mounted, versioned-schema** resource the provider **watches and hot-reloads**; reload contract = **lazy-open on host-add, graceful-drain on host-remove** (never sever an in-flight op). Provider reads the file, never the K8s API — preserves the #297 no-RBAC/no-API invariant.
>
> **Decision (Secret vs ConfigMap):** ✅ **Secret.** Keeps `known_hosts`/connection material out of namespace-readable config and stays uniform with today's credential flow.
>
> **Rationale / notes:** Folded into ADR-0007 D3 + Open question 2.

---

## D3 — Intra-cluster migration trigger *(ADR-0007 Q3)*

**Question.** What CRD triggers a **live** host→host migration within a cluster? ADR-0006's `VMMigration` is export-centric (cold; S3/NFS staging) and does not fit a live move.

**Why it blocks P2.** P2 implements live migration; its state machine and API shape depend on this.

**Options.**
- **A — new `VMHostMigration` kind.**
  - ✅ Purpose-built, clean live-migration state machine; doesn't overload `VMMigration`'s export phases; clear separation.
  - ❌ A **third** migration CRD (`VMMigration` cross-hypervisor + `VMHostMigration` intra-cluster) → more API surface and docs.
- **B — `spec.class: hostMigration | crossHypervisor` discriminator on `VMMigration`.**
  - ✅ One migration CRD; users learn one kind.
  - ❌ One CRD + controller housing two very different state machines (cold export/stage/import vs live RAM transfer) → risk of an overloaded type and a branchy controller.

**Draft recommendation:** **B** (discriminator), to avoid a third migration CRD.

**What B commits you to:** extending `vmmigration_types.go` with a `class` discriminator (defaulted so existing `VMMigration`s keep the cross-hypervisor behavior) and branching the controller's reconcile into two workflows. *(Choosing A instead commits you to a new `vmhostmigration_types.go` + its own controller.)*

> **Decision:** ✅ **Accepted 2026-09-21 — Option A (overrides the draft's Option B).** A **new `VMHostMigration` kind**, not a `spec.class` discriminator on `VMMigration`.
>
> **Rationale / notes:** Cold cross-hypervisor (ADR-0006) and live intra-cluster are different domains sharing only a noun; a v1beta1 union-type CRD ages badly and is automatic-HA's awkward base. Separate kinds give independent concurrency/RBAC/rollback for host-drain evacuations. Cost mitigated by **sharing internal migration primitives + condition vocabulary, not the CRD schema.** Folded into ADR-0007 D5, the CRD/API-changes section, and Open question 3. This was the one close call (see the divergence note below).

---

## D4 — Driver flag surface + lifetime *(ADR-0008 Q1)*

**Question.** The strangler-fig refactor flips libvirt operations from `virsh` → go-libvirt **per family** behind a flag. Where does that flag live, and when is it removed?

**Why it matters.** It shapes the rollout mechanics and operator UX for the whole multi-quarter refactor (and the per-family flag is what lets a regression retire one family without reverting all).

**Options.**
- **A — env var on the provider Deployment** (e.g. `VIRTRIGAUD_LIBVIRT_NATIVE=reads,lifecycle`).
  - ✅ Zero CRD change; low friction; easy to flip per-deployment; nothing to deprecate later.
  - ❌ Not declarative/auditable; invisible in the `Provider` CR; harder to reason about fleet-wide state via GitOps.
- **B — `Provider.spec` field.**
  - ✅ Declarative, auditable, visible in the CR + GitOps.
  - ❌ A CRD change, and a **transition** flag on the *stable* v1beta1 API that must later be deprecated/removed cleanly.

**Lifetime sub-question.** When is the flag removed? A transition flag that outlives the transition becomes permanent supported config → set a sunset (e.g., "removed the release after all families default to native").

**Draft recommendation:** *open.* Because the flag is **transient**, an **env var (A)** may fit better — it avoids having to deprecate a `v1beta1` field once the refactor completes. Choose B only if fleet-wide auditability of the rollout outweighs that.

**What the choice commits you to:** the flag's location **and** a documented removal milestone in the ADR's phasing.

> **Decision (surface — env vs spec):** ✅ **Accepted 2026-09-21 — env var.** Per-family `VIRTRIGAUD_LIBVIRT_NATIVE=reads,lifecycle,…` on the provider Deployment, **not** a `Provider.spec` field — a transient flag must not pollute stable v1beta1. Auditability via an operator-emitted `Provider.status` condition + metric (read-only, non-API-committing).
>
> **Decision (sunset milestone):** ✅ Flag **removed and native made unconditional in the first minor release after the last operation family clears the D5 flip-gate in production.**
>
> **Rationale / notes:** Folded into ADR-0008 D6 + Open question 1.

---

## D5 — Shadow-compare divergence budget *(ADR-0008 Q2)*

**Question.** PR 4 runs go-libvirt reads in **shadow** (run both drivers, return `virsh`'s answer, meter divergence). Before flipping reads to native (PR 5), what divergence threshold counts as "safe to flip"?

**Why it blocks PR 4 → 5.** Without a pre-agreed gate, "good enough" gets defined by whoever wants to ship. This is *pick the numbers*, not either/or.

**Parameters to set.**
- **Divergence rate** — e.g. **0 semantic** divergences; formatting-only diffs (whitespace, field order) explicitly excluded.
- **Soak duration** — e.g. ≥ *N* days / ≥ *M* reconcile cycles across the real VM population (lab + the production provider).
- **Resilience events observed clean** — e.g. ≥ *K* libvirtd restarts and ≥ 1 host reboot with zero divergence.

**Trade-off.** Strict (0 divergence over a long soak spanning restarts) = high confidence, slower to flip. Lax = faster flip, risk of undetected drift reaching production reads.

**Draft recommendation (suggested numbers to accept or edit):** **0 semantic divergences over ≥ 7 days** across all lab + production VMs, spanning **≥ 2 libvirtd restarts** and **≥ 1 host reboot**, formatting-only diffs excluded. PR 5 (flip) may not merge until this is met and recorded.

**What the choice commits you to:** a concrete, written flip-gate that PR 5 must satisfy, plus the divergence metric/report to measure it (see ADR-0008 Q3).

> **Decision (rate / soak / events):** ✅ **Accepted 2026-09-21.** **0 semantic divergences** (canonicalizing comparator excludes whitespace/field-order) **≥ 14 days in production**, spanning **≥ 2 libvirtd restarts and ≥ 1 host reboot**, **plus** a per-VM-shape coverage checklist (UEFI/NVRAM, snapshot-bearing, multi-disk, multi-NIC). Measured by `virtrigaud_libvirt_shadow_divergence_total{family,field}`; **PR 5 may not merge until the metric reads 0 across the window and the checklist is complete.**
>
> **Rationale / notes:** Also answers Open question 3 (the metric is the reporting/evidence surface). Folded into ADR-0008 D6 + Open questions 2 & 3.

---

## After markup

With these five now decided (2026-09-21), the immediate mechanical follow-ups unblock:
- **`crd-update`** — `provider_types.go` (`Topology` per D1), the `VMHostMigration` kind (per D3), `host_types.go` + `hostpool_types.go`.
- **`proto-update`** — `ListHosts` / `GetHostInfo` / `MigrateVM` + `target_host_id`, stubbed `Unimplemented` in all providers.
- The `hostconn.Registry` constructor design (per D2) and the strangler flag plumbing (per D4).

The remaining ADR open questions (0007 Q4–Q7, 0008 Q4–Q9 — 0008 Q3 is now answered by D5's metric) are **not** blocking and can be decided as each phase approaches. **0007 Q8** (p2p migration + PKI) is deliberately deferred to the future fencing/HA ADR — do not decide it here.

---

## Claude's recommendations — security · scalability · future-proof (2026-09-21)

Confirms the draft on **D1, D2**; resolves **D4**'s open lean; strengthens **D5**; **diverges from the draft on D3** (closest call).

| # | Recommended | Deciding lens | Why (short) |
|---|-------------|---------------|-------------|
| **D1** | **A — `topology` enum**, default `single`, **open string not bool** | future-proof / scale | Orthogonal hypervisor × topology axes; avoids type explosion; cloud-hypervisor reuses it. Default `single` = no breaking change. |
| **D2** | **A — single projected *Secret*, watched, versioned schema, drain-on-remove** | **security** | Preserves the #297 no-API-access invariant; Secret keeps `known_hosts`/connection material out of namespace-readable ConfigMaps. Reload: lazy-open on add, graceful-drain on remove. |
| **D3** | **A — new `VMHostMigration` kind** *(diverges from draft B)* | future-proof / scale | Live-intra-cluster and cold-cross-hypervisor are different domains sharing only a noun; a v1beta1 union-type CRD ages badly and is HA's awkward base. Share internal libs + condition vocab, **not** the CRD schema. |
| **D4** | **A — env var, per-family, documented sunset; audit via status condition + metric** | future-proof | A transient flag must not pollute the stable v1beta1 API. Auditability comes from operator-emitted status/metric, not a spec field. Sunset: native unconditional the release after the last family clears D5. |
| **D5** | **Strict, coverage-based, machine-checked** | scale / integrity | 0 semantic divergences over **≥14d prod**, **≥2 libvirtd restarts + ≥1 host reboot**, a per-VM-shape coverage checklist (UEFI/NVRAM, snapshot, multi-disk, multi-NIC), gated by a `shadow_divergence_total{family,field}` metric. |

**The one divergence to weigh (D3):** the draft chose a `VMMigration` discriminator to avoid a third CRD; I recommend a separate `VMHostMigration` kind because the two are genuinely different domains and v1beta1 is expensive to fix later. If minimizing v1 API surface matters more to you than domain-clean schemas, the discriminator is the defensible alternative — this is the only one of the five that's close.

*Accepted as recommended on 2026-09-21 (D3 taken as the divergence — the `VMHostMigration` kind over the draft's discriminator). The `Decision` fields above are now filled; these fold into ADR-0007/0008.*
