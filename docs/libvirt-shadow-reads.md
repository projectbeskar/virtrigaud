# Libvirt shadow-compare reads (ADR-0008 PR 4b)

> **Status:** opt-in, behavior-preserving. With the flag unset, the libvirt
> provider is exactly the virsh provider it was before — go-libvirt stays dormant
> (no dial, no goroutines).

The libvirt provider is being moved off `virsh`-subprocess reads onto a pure-Go
[`go-libvirt`](https://github.com/digitalocean/go-libvirt) client
([ADR-0008](adr/0008-libvirt-pure-go-driver-and-ssh-transport.md)). Before any read
flips to the new driver, we run **both** drivers in production and measure whether
they ever disagree. This is the **shadow-compare** phase (PR 4b). The flip itself is
PR 5, and it is gated on the divergence metric this phase produces reading **zero**
over a soak window (ADR-0008 **D5**).

This document is for operators running the soak.

## What shadow mode does

For a read family in **shadow** mode, on every (sampled) call the provider:

1. Runs the authoritative **virsh** path and **returns its answer to the caller —
   always**. Reads do **not** flip to go-libvirt in this phase.
2. Also runs the **go-libvirt** path, observed only.
3. Canonicalizes both answers (excluding whitespace, field order, formatting, and
   unit differences) and compares them field by field.
4. Meters any *semantic* divergence.

The go-libvirt path is fully isolated: if it errors or panics it is recovered,
logged, and metered — it can never change what the caller receives or slow it down
(it runs on a detached, time-bounded goroutine after virsh has already answered).

The first and only family in PR 4b is **`describe`** — the side-effect-free
"Describe first" family. It compares `exists`, `power_state` (coarsened), `uuid`,
`name`, `vcpu`, and `memory_mib`. Live IPs, console URL, and guest-agent enrichment
are out of scope for this family's native path and are not compared.

## Enabling shadow on a provider

The driver flag is an **environment variable on the provider Deployment**, not a
CRD field — a transient transition flag must not pollute the stable `v1beta1` API
(ADR-0008 **D4**). It reaches the provider pod through the existing generic
`spec.runtime.env` passthrough, which the provider controller appends verbatim to
the Deployment:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-lab
spec:
  type: libvirt
  runtime:
    mode: Remote
    env:
      - name: VIRTRIGAUD_LIBVIRT_NATIVE
        value: "shadow:describe"
```

Or, on an already-running provider Deployment, without touching the CR:

```bash
kubectl set env deployment/<provider-deployment> VIRTRIGAUD_LIBVIRT_NATIVE=shadow:describe
```

### Flag grammar

```
VIRTRIGAUD_LIBVIRT_NATIVE = entry ("," entry)*
entry                     = [ mode ":" ] family
mode                      = off | shadow | native      # default: shadow
family                    = describe                   # the only family in PR 4b
```

- **`off`** (or unset) — pure virsh; go-libvirt dormant. **The default; zero
  behavior change.**
- **`shadow:describe`** — run both, return virsh, meter divergence. **Use this for
  the soak.**
- **`native:describe`** — reserved for PR 5 (return go-libvirt's answer). It is
  accepted by the grammar today so the config you write does not change at the
  flip, but in this build it is **downgraded to shadow with a loud warning** —
  reads do not flip in PR 4b.
- A bare `describe` (no mode) defaults to **shadow**.

Unknown families/modes and duplicates are warned about and ignored, never fatal.

### Optional knobs

| Env var | Default | Purpose |
|---|---|---|
| `VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE` | `1` (every call) | Shadow 1 in every N calls, to bound CPU on a hot read over a large VM population. |
| `VIRTRIGAUD_LIBVIRT_SHADOW_TIMEOUT` | `10s` | Upper bound on one shadow Describe's detached goroutine. |

Shadowing roughly **doubles the work of each shadowed read** (a virsh fork *and* a
go-libvirt call). For the documented lab/production population this is negligible;
raise `VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE` if you are shadowing a much larger fleet.

## Reading the metrics

The provider process serves Prometheus metrics at **`/metrics` on its health
port** (`--health-port`, default `8080`). For a quick look:

```bash
kubectl port-forward deployment/<provider-deployment> 8080:8080
curl -s localhost:8080/metrics | grep virtrigaud_libvirt_
```

| Metric | Labels | Meaning |
|---|---|---|
| `virtrigaud_libvirt_shadow_divergence_total` | `family`, `field` | **The D5 flip-gate metric.** Each increment is one semantic divergence for one field. **Must be 0** for the family across the whole soak window. |
| `virtrigaud_libvirt_shadow_compare_total` | `family`, `result` | Shadow-compare runs by result (`equal`/`divergent`/`error`/`panic`). The denominator, and proof the harness is actually running. |
| `virtrigaud_libvirt_active_driver` | `family`, `driver` | The effective driver per family (`virsh`/`shadow`/`native`), `1` for the active one. The D4 audit signal. |

> A metric nobody dashboards on is not evidence. Before the soak counts, stand up a
> dashboard/alert on `virtrigaud_libvirt_shadow_divergence_total` and confirm
> `virtrigaud_libvirt_shadow_compare_total{result="equal"}` is climbing (the harness
> is running).

## The D5 soak → PR 5 flip-gate

PR 5 (flip `describe` reads to native) may merge only when, for the `describe`
family (ADR-0008 **D5**):

- `virtrigaud_libvirt_shadow_divergence_total{family="describe"}` reads **0
  across the full window**;
- the window is **≥ 14 days in production**, spanning **≥ 2 libvirtd restarts** and
  **≥ 1 host reboot**, with the compare counter showing the harness ran throughout;
- the **per-VM-shape coverage checklist** is complete: UEFI/NVRAM, snapshot-bearing,
  multi-disk, and multi-NIC domains have all been shadowed with zero divergence.

Any non-zero divergence: read the `field` label, `virsh dumpxml` the affected
domain, and reconcile the native projection before restarting the window.

## Credit

The "Describe first" design, its failure-mode catalogue (B2/B3/M2), and its 45-VM
validation come from [@jing2uo](https://github.com/jing2uo)'s
[PR #291](https://github.com/projectbeskar/virtrigaud/pull/291); PR 4b reconciles
that work onto go-libvirt (ADR-0008 **D9**).
