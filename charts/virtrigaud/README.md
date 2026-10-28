# VirtRigaud Helm Chart

This Helm chart installs VirtRigaud, a Kubernetes operator for managing virtual machines across multiple hypervisors.

## Features

- **Automatic CRD Upgrades**: CRDs are automatically upgraded during `helm upgrade` (not just during initial install)
- **Multi-provider Support**: Deploy providers for vSphere, Libvirt/KVM, and Proxmox VE
- **Production Ready**: Secure defaults, RBAC, and resource limits
- **Flexible Configuration**: Extensive customization via values.yaml

## Prerequisites

- Kubernetes 1.25+
- Helm 3.8+

## Installation

### Quick Start

```bash
# Add the Helm repository
helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
helm repo update

# Install VirtRigaud
helm install virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --create-namespace
```

### Custom Installation

```bash
# Install with custom values
helm install virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --create-namespace \
  --set manager.replicaCount=2
```

> Providers are normally deployed by the manager from `Provider` CRs — not via
> Helm flags. The chart-templated `providers.*` Deployments are disabled by
> default (#173); see [Provider Configuration](#provider-configuration) before
> opting in.

### Installation from Local Chart

> **Important:** the chart's `crds/` directory is generated on demand and is **not**
> checked into git (a fresh checkout contains only `.gitkeep`). Before installing from a
> local checkout you **must** generate the CRDs, or the chart ships no CRDs:
>
> ```bash
> make gen-helm-crds
> ```

```bash
# From repository root (run 'make gen-helm-crds' first — see note above)
helm install virtrigaud charts/virtrigaud \
  -n virtrigaud-system \
  --create-namespace
```

## Upgrading

> **Upgrading from v0.3.11?** Read
> [`docs/upgrading.md`](../../docs/upgrading.md) first. The move to v0.4.0
> includes several breaking security fixes (vSphere/libvirt VM-ownership
> checks, a new required vCenter privilege, libvirt default cloud-init no
> longer provisioning credentials, cross-namespace clone/migration targets
> needing a grant, and `VirtualMachine.spec.providerRef` becoming immutable
> once bound) with a required CRD → manager → provider upgrade order.
>
> Upgrading from v0.4.0 to v0.4.1 has [its own section](../../docs/upgrading.md#upgrading-from-v040-to-v041) in that guide.

> **Carrying custom values across chart versions:** prefer
> `--reset-then-reuse-values` (Helm 3.14+) over `--reuse-values`.
> `--reuse-values` replaces the new chart's defaults with the **old** chart's
> defaults, so settings added in a newer chart are missing and old defaults
> linger. `--reset-then-reuse-values` applies the new chart's defaults and then
> your own values.

### Automatic CRD Upgrades (Default)

By default, VirtRigaud automatically upgrades CRDs during `helm upgrade`:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system
```

**How it works:**
- A Kubernetes Job runs before upgrade (Helm pre-upgrade hook)
- The Job applies all CRDs using `kubectl apply --server-side --force-conflicts`
- The CRDs it applies are the ones baked into the chart **at package time**
- Job automatically cleans up after successful upgrade

> **Caveat:** because the hook does a server-side apply of the packaged CRDs, upgrading
> with a **stale** chart (one packaged before a newer CRD schema) will **overwrite** the
> live CRDs and can **prune** fields that the stale schema does not know about. Always
> upgrade with a chart built from a matching release. If you package the chart yourself,
> run `make gen-helm-crds` (or `make helm-package`) first so the baked-in CRDs are current.

**Benefits:**
- ✅ No manual CRD management needed
- ✅ Works seamlessly with GitOps tools (ArgoCD, Flux)
- ✅ Prevents CRD version drift
- ✅ Safe server-side apply with conflict resolution

### Disable Automatic CRD Upgrades

If you prefer to manage CRDs manually:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false
```

Then manually apply CRDs before upgrade. From a source checkout, apply the committed
source-of-truth CRDs (these are always present and current):

```bash
kubectl apply -f config/crd/bases/
```

> The chart's own `charts/virtrigaud/crds/` directory is generated on demand and is empty
> in a fresh checkout — run `make gen-helm-crds` first if you want to apply from there
> instead.

### Skip CRDs Entirely

If CRDs are managed externally (e.g., separate Helm chart):

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --skip-crds \
  --set crdUpgrade.enabled=false
```

## Configuration

### CRD Upgrade Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `crdUpgrade.enabled` | Enable automatic CRD upgrade during helm upgrade | `true` |
| `crdUpgrade.image.repository` | kubectl image repository | `ghcr.io/projectbeskar/virtrigaud/kubectl` |
| `crdUpgrade.image.tag` | kubectl image tag (auto-updated during release) | `v0.2.0` |
| `crdUpgrade.backoffLimit` | Job retry limit | `3` |
| `crdUpgrade.ttlSecondsAfterFinished` | Job cleanup time | `300` |
| `crdUpgrade.waitSeconds` | Wait time after applying CRDs | `5` |

### Manager Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `manager.replicaCount` | Number of manager replicas | `1` |
| `manager.image.repository` | Manager image repository | `projectbeskar/virtrigaud/manager` |
| `manager.image.tag` | Manager image tag | `v0.2.0` |
| `manager.resources.limits.cpu` | CPU limit | `500m` |
| `manager.resources.limits.memory` | Memory limit | `512Mi` |
| `manager.tmpSizeLimit` | `sizeLimit` of the manager pod's `/tmp` emptyDir (empty = none) | `128Mi` |
| `manager.providerTmpSizeLimitMax` | Ceiling on every provider pod's `/tmp` `sizeLimit` (`--provider-tmp-size-limit-max`; a quantity, empty = no ceiling). A `Provider` annotation above it, or `none`, is clamped to it with a `TmpSizeLimitClamped` Warning event; the defaults are capped at it too | `""` |
| `manager.providerTmpSizeLimit` | `sizeLimit` of the `/tmp` emptyDir of every provider pod run for a `Provider` CR (`--provider-tmp-size-limit`; a quantity or `none`). Empty keeps the per-type defaults: vSphere `512Gi`, others `1Gi` | `""` |
| `manager.maxDiskSize` | Administrator's ceiling on every disk VirtRigaud creates or grows (`--max-disk-size`; a whole number of bytes from `1Gi` to `64Pi`, use binary units such as `500Gi`; empty = no ceiling beyond the CRD's 64Ti per disk). See [Disk-size limit](#disk-size-limit) | `""` |
| `manager.clientQPS` | Client-side rate limit of the manager's Kubernetes API requests, in requests per second, for each REST client it builds (`--client-qps`; a positive number; empty = the manager's default, 20). Not an API-server limit. See [Kubernetes API client rate limits](#kubernetes-api-client-rate-limits) | `20` |
| `manager.clientBurst` | Requests each of those REST clients may send at once before `manager.clientQPS` applies (`--client-burst`; a positive whole number; empty = the manager's default, 40) | `40` |

#### `/tmp` size limits

Every pod's root filesystem is read-only, so each gets a writable `/tmp`
emptyDir, and each emptyDir has a `sizeLimit`: a pod that writes more than that
to `/tmp` is **evicted** by the kubelet.

> **The limit bounds the pod's `/tmp`; it does not reserve node storage.** The
> kubelet enforces it only after the data is written. A node with less free
> ephemeral storage than the limit (the vSphere default is `512Gi`) can still run
> out first, and the kubelet then evicts pods by its own disk-pressure rules.
> Size each limit to the nodes the pod can run on, or give the provider's `/tmp`
> real capacity: schedule it on nodes with that much free ephemeral storage, or
> back `/tmp` with a volume sized for its imports.

- **vSphere providers stage whole disks in `/tmp`:**
  - an OVA image download, capped by `VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB`
    (default 256 GiB);
  - for a migration import, or a converted or multi-extent export, about
    **twice the disk**.

  The default `512Gi` covers both up to that cap; it is a ceiling on what the
  pod may write, not a guarantee the node has that much free. For larger disks,
  raise the limit; either way, make sure the node has the free ephemeral storage
  the imports need, or lower the limit to what it has.
- **libvirt and Proxmox providers** stream disk data to and from the hypervisor
  host over SSH. Their `/tmp` only holds SSH keys, known_hosts and control
  sockets, so `1Gi` is ample.
- **One `Provider`:** annotate it
  `infra.virtrigaud.io/tmp-size-limit: "<quantity>"` (or `"none"`). The webhook
  refuses an invalid value, and changing it rolls that Provider's pods. With
  `manager.providerTmpSizeLimitMax` set, the annotation cannot go above that
  ceiling: a larger value, or `"none"`, is clamped to it, and the Provider gets a
  `TmpSizeLimitClamped` Warning event.
- **All CR-managed providers:** set `manager.providerTmpSizeLimit`.
- **Chart-templated providers:** set `providers.<name>.tmpSizeLimit` (a
  quantity, `"none"`, or empty for no limit; anything else fails the render).

#### Disk-size limit

By default the only bound on a VM disk is the CRD's: 64Ti per disk. Tenants who
own their namespace can ask for that much (and grow existing disks, and add up
to 20 more) and fill a shared datastore or pool. `manager.maxDiskSize` (the
manager's `--max-disk-size`) sets a ceiling **tenants cannot raise**:

- **New disks:** a VM whose VMClass `spec.diskDefaults.size` or any
  `spec.disks[].sizeGiB` is above the limit is not created. On a libvirt
  Provider a class without `spec.diskDefaults.size` counts as the libvirt
  default primary disk, 20Gi. The VM waits with `Ready=False`, reason
  `DiskSizeExceedsLimit`, and a Warning event, and is created once its sizes
  fit. A vanished VM is not re-created above it either.
- **Growth:** a Reconfigure never grows a disk beyond the limit. The VM's other
  changes (CPU, memory) are applied, the over-limit disk is left as it is, and
  the VM reports `DiskSizeWithinLimit=False`, reason `DiskSizeExceedsLimit`,
  whatever the Reconfigure's outcome (done, an async task, pending a restart,
  failed). The next Reconfigure that withholds nothing removes the condition.
  On a Proxmox Provider, which applies every `spec.disks` size to its first
  disk, no `spec.disks` size is sent at all once one is withheld.
- **Clones and migrations** copy the source VM's real disks, whatever its spec
  says now, so they are checked by the sizes the source's provider reports
  (`GetDiskInfo`), not by its VMClass or `spec.disks`:
  - a `VMClone` is checked for every disk of the source and the target class
    override, before the clone is issued; it stays `Pending`;
  - a `VMMigration` is checked for the migrated disk and the target class in
    `Validating`, again right before the export (the size is recorded in
    `status.diskInfo.sourceSize`), and that record right before the import;
    it stays in its phase;
  - **the target provider enforces the limit itself at the import.** The
    staged object can be replaced between the export and the import (a
    tenant-chosen NFS export, a staging PVC in the tenant's namespace), and the
    earlier checks trust the *source* provider's report. So the manager sends
    the target's limit with `ImportDisk` (`max_virtual_size_bytes`), and the
    target provider reads the virtual size of the disk it is about to land — a
    private copy or the converted output, never the staging location itself —
    and refuses a larger one (`DISK_SIZE_EXCEEDS_LIMIT`), or one whose size it
    cannot read (`DISK_SIZE_UNVERIFIABLE`). Nothing is landed; the migration
    is held in `Importing` (`Importing=False`) and not sent again for the same
    limit. vSphere checks the pod-local vmdk before the upload or the NFC
    lease (pvc, s3, nfs); libvirt checks the private staged copy (s3), the
    converted disk before it is published (nfs), and a private copy of the
    staged file before it becomes the volume (pvc: the copy on the host over
    SSH, or in the provider's own container over a local connection); Proxmox
    checks the node-local stage `qm importdisk` lands (s3, nfs). A provider
    image from before this release ignores the field and does not report
    `supports_import_size_limit`, so while a limit applies a migration into it
    is held (`DiskSizeLimitUnverifiable`, "upgrade the provider image").

  A size the provider does not report (a provider image from before this
  release, a disk that is not file-backed on libvirt, a read error) refuses the
  clone or migration while a limit applies (`DiskSizeLimitUnverifiable`). So
  does a migration that reaches the import with no recorded size (it was
  exported before the limit was set): delete it and migrate again.
- **Not covered: images larger than the class.** The limit bounds the sizes
  VirtRigaud asks for. A VM's primary disk comes out of its image:
  - vSphere (template) and libvirt (base image) grow it to the class
    `spec.diskDefaults.size`, never shrink it;
  - Proxmox does not apply `spec.diskDefaults.size` at all: the VM gets the
    template's disk as it is (the class size is still checked against the
    limit).

  An image whose disk is already larger than the limit therefore gives a larger
  disk, and that is not checked: no image's size is known before the Create
  (VMImage `status.providerStatus[].size` exists but nothing fills it yet).
  Control which images tenants can use (VMImages are namespaced; shared ones
  need `spec.consumerNamespaceSelector`), or bound the hypervisor's own
  storage.
- **Existing VMs** whose disks are already larger keep running, are never
  shrunk, and stay `Ready`.
- **Per Provider:** a Provider's owner can lower the limit for that Provider with
  the annotation `infra.virtrigaud.io/max-disk-size: "<quantity>"` — on a
  clustered Provider shared with other namespaces, the Provider's owner is the
  administrator of its consumers. The annotation can never raise the manager's
  ceiling. The Provider webhook refuses an invalid value; if one is stored
  anyway, nothing is created or grown through that Provider
  (`DiskSizeLimitUnverifiable`) until it is fixed.
- **Admission:** with `webhooks.enabled=true` and `manager.maxDiskSize` set, the
  chart also renders the `vvirtualmachine-disksize.kb.io` and
  `vvmclass-disksize.kb.io` validating webhooks, which refuse a larger
  `spec.disks[].sizeGiB` or `spec.diskDefaults.size` (or growing one beyond the
  limit) at `kubectl apply` time. They check the manager's ceiling only, not a
  Provider's annotation. Without `manager.maxDiskSize` they are not rendered, so
  VM and VMClass writes gain no admission dependency. The controllers enforce
  the limit whether or not webhooks are on.
- **Kustomize installs:** `config/webhook/manifests.yaml` is generated from the
  Go markers and always lists the VM and VMClass disk-size webhooks, with
  `failurePolicy: Fail`. It is not part of `config/default` (its `[WEBHOOK]`
  sections are commented out). If you enable it, VM and VMClass writes depend
  on the webhook being reachable even without a ceiling (the manager serves
  them whenever webhooks are on, and admits everything without one): remove
  those two entries, or set their `failurePolicy` to `Ignore`, if you do not
  use `--max-disk-size`.
- **Upgrade the provider images with the manager.** The clone and migration
  checks read the source's disk sizes from `GetDiskInfo`'s new `disks` list.
  A provider image from before this release does not fill it, so while a
  limit applies, clones through it are refused (`DiskSizeLimitUnverifiable`);
  migrations read the primary disk's size, which older images do report.

The limit is per disk, not a total per VM or per namespace (a VM can have up to
20 additional disks): bound totals with the hypervisor's own storage quotas, or
an object-count `ResourceQuota` on `virtualmachines.infra.virtrigaud.io`. Sizes are compared exactly: `500G` is
about `465.7Gi`, so prefer binary units.

#### Kubernetes API client rate limits

The manager limits its own requests to the Kubernetes API with a client-side
token bucket: `manager.clientQPS` (the manager's `--client-qps`) is the
sustained rate in requests per second, and `manager.clientBurst`
(`--client-burst`) the number of requests that may go out at once before that
rate applies. The defaults, 20 and 40, are the values the manager has always
used.

- **Per REST client, not per manager.** controller-runtime builds one REST
  client per API resource type, and each has its own bucket with these
  limits: `VirtualMachine` writes do not share a budget with `Host` status
  updates.
- **Client-side only.** These are not API-server limits. The API server's own
  protection (API Priority and Fairness) applies separately, whatever these
  values are.
- **Startup configuration.** A change takes effect when the manager restarts.
  A zero, negative or non-numeric value fails the chart render, and the
  manager refuses to start with it.

**When raising it helps.** During a burst of VM creations each new VM needs
several `VirtualMachine` writes before it is `Ready` (on a clustered Provider,
five were measured: the finalizer, the placement annotation, and three status
updates). When those
writes queue behind the client's rate limit, creation is paced by it rather
than by the scheduler or the providers.

**What it costs.** A higher limit lets the manager do more work at once: more
manager CPU (watch for CPU throttling at its limit; raise
`manager.resources.limits.cpu` with it), more load on the API server and etcd,
and more optimistic-concurrency conflicts, which the manager retries. Once the
client limit no longer binds, the next limit shows: in the measurement below,
the per-Provider placement step, slowed by CPU throttling. Adjust these values
to your workload (how many VMs are created at once) and to what your control
plane can absorb, and watch the API server's request latency and the manager's
CPU after changing them.

**Measured effect.** A disposable kind cluster with a fake provider
(Spike 1B): one clustered Provider, 1,000 Hosts, 10,000 existing VMs, manager
limits 500m CPU / 512Mi, a burst of 100 VM creations, one run per setting:

| | `clientQPS: 20`, `clientBurst: 40` | `clientQPS: 100`, `clientBurst: 40` |
|---|---|---|
| Create to `Ready`, p50 / p99 | 20.52 s / 23.00 s | 7.86 s / 8.18 s |
| Creation throughput | 4.3 VMs/s | 12.2 VMs/s |
| Client rate-limit wait, mean | 190 ms | 3.4 ms |
| Manager CPU during the burst (throttled periods) | 0.19 cores (15%) | 0.34 cores (36%) |
| Placement lock wait, p50 | ~0 | 177 ms |
| API server 429s, flow-control rejections, 5xx | none | none |
| Conflicts (HTTP 409), retried | none | 6 |

Five times the QPS gave 2.8 times the throughput, not five. These numbers come
from one benchmark environment; they are not a recommendation for production
clusters.

Example (validated only in that disposable benchmark environment, not across
production Kubernetes distributions):

```yaml
manager:
  clientQPS: 100
  clientBurst: 40
```

With kustomize (`config/manager`), add `--client-qps` and `--client-burst` to
the manager container's args; without them the defaults apply.

### Provider Configuration

In normal operation you do **not** enable providers here — the manager's
ProviderController deploys provider pods automatically from `Provider` CRs
(`spec.runtime`), which also wires each provider's TLS posture. The chart can
optionally template standalone provider Deployments, but they are **disabled by
default** (#173).

> **If you opt in** (`providers.<name>.enabled: true`) you must also configure the
> `providerTLS` block — set `providerTLS.secretName` (mTLS) or
> `providerTLS.insecure=true` (audit-flagged plaintext). Otherwise the provider
> fail-closes on startup (ADR-0003, v0.3.7 secure-by-default) and crash-loops.

```yaml
providers:
  vsphere:
    enabled: false  # opt-in; also requires providerTLS (see note above)
    replicaCount: 1

  libvirt:
    enabled: false  # opt-in; also requires providerTLS (see note above)
    replicaCount: 1

  proxmox:
    enabled: false
```

Each templated provider has a `providers.<name>.tmpSizeLimit` (vSphere `512Gi`,
libvirt and Proxmox `1Gi`; see [`/tmp` size limits](#tmp-size-limits)).

See [values.yaml](values.yaml) for complete configuration options.

### Network Policies

The chart can render namespaced `NetworkPolicy` objects that isolate the manager
and provider pods to only the flows they need. They are **opt-in and disabled by
default** (`networkPolicy.enabled: false`).

> **Why default-off?** Earlier chart versions declared `enabled: true` but no
> template consumed the values, so the effective behavior was always "no
> policies." Wiring real templates while keeping `true` would suddenly enforce
> isolation on `helm upgrade` and could black-hole webhook admission,
> manager↔provider gRPC, metrics scraping or DNS in clusters not designed for
> it. Default-off makes enabling a deliberate, behavior-neutral, testable choice.

> **Why a new top-level key?** The settings live under `networkPolicy`, not the
> legacy `security.networkPolicies` block, which is now ignored. v0.3.11 shipped
> `security.networkPolicies.enabled: true`, and `helm upgrade --reuse-values`
> reuses the **old** chart's defaults, so gating on the legacy key would switch
> policies on for every upgraded install and render them without the newer
> settings. If you enable `networkPolicy` on a release upgraded with
> `--reuse-values`, the chart fails with a message asking for
> `--reset-then-reuse-values`, which takes the new chart's defaults and keeps
> your own values.

**Requirements before enabling:**

- A **CNI that enforces NetworkPolicy** (Calico, Cilium, Antrea, Weave, …). On a
  CNI without enforcement these objects are inert.
- **Deployment-specific tuning** — the defaults are sane but not universal.

**What the policies allow** (when `networkPolicy.enabled: true`):

| Pod | Ingress | Egress |
|-----|---------|--------|
| **manager** | health probes (`:8081`, open source — kubelet); metrics (`:8080`, from `monitoringNamespaceSelector`); webhook (`:9443`, open source — API server, only when `webhooks.enabled`) | DNS (kube-dns); Kubernetes API (`apiServerPorts`); gRPC to providers (`providerGRPCPort`) |
| **provider** | health/metrics (`:8080`, open source — kubelet); gRPC from manager (`providerGRPCPort`) | DNS (kube-dns); hypervisor + migration staging (`hypervisorEgressPorts`) |

Ingress rules for the kubelet (health probes) and the API server (webhook
admission) use an **open source** (`from` omitted, port-scoped only): those
callers originate from host/node IPs that a pod/namespace selector cannot match.
Narrowing them would break readiness or, under `webhooks.validating.failurePolicy:
Fail`, brick `Provider` admission.

**Tuning checklist** (all values-driven — no template edits needed):

- **Metrics scrape:** label your monitoring namespace to match
  `monitoringNamespaceSelector` (default `metrics: enabled`, mirroring the
  kubebuilder scaffold). If Prometheus runs in the **same** namespace as the
  manager, label that namespace too — a `namespaceSelector` does not implicitly
  match the policy's own namespace.
- **DNS:** defaults target CoreDNS (`k8s-app: kube-dns` in `kube-system`). On
  clusters with **NodeLocal DNSCache**, pods resolve via a node-local
  link-local IP a selector can't match — set `dnsEgressCIDRs` (e.g.
  `169.254.20.10/32`) or DNS is black-holed.
- **Providers in another namespace:** by default the manager→provider egress
  peer is scoped to the release namespace. If provider pods run elsewhere, set
  `providerNamespaceSelector` (include the release namespace too) **and** render
  an equivalent provider policy into each provider namespace — `NetworkPolicy`
  is namespaced.
- **Chart-templated providers** (`providers.*.enabled: true`): those pods use
  `app.kubernetes.io/component: provider-<hv>` and a gRPC probe on `9090` —
  override `providerPodSelector` and `providerGRPCPort`, and widen the provider
  health allow to the probe port.
- **Lock down egress (recommended for regulated/banking):** pin
  `apiServerCIDRs` to the control-plane network and `hypervisorEgressCIDRs` to
  your hypervisor/storage networks. The latter is SSRF defense-in-depth behind
  the application-layer allowlist (migration storage endpoints are user-supplied).
- **Do not disable** `egress.dns` or `egress.kubernetesAPI` while other egress
  rules stay on — that turns those into denied directions and **wedges the
  manager** (no leader election, no reconcile). Likewise keep `ingress.webhookIngress`
  on when `failurePolicy: Fail` webhooks are enabled.
- **Provider metrics exposure:** provider `/metrics` shares the health port
  (`:8080`), whose ingress is open-source, so provider telemetry is reachable
  cluster-wide (telemetry only, no secrets) — a posture note for tight
  environments.

Enable and verify with a render before applying:

```bash
helm template virtrigaud charts/virtrigaud \
  --set networkPolicy.enabled=true | grep -A2 'kind: NetworkPolicy'
```

### Reserved-keys admission policy

On Kubernetes 1.30 and later the chart installs a `ValidatingAdmissionPolicy`
(and its binding) that lets only the manager's ServiceAccount add or change the
`virtrigaud.io` annotations and labels the manager alone writes on a
`VirtualMachine`: migration, clone and adoption provenance, the migration
claim, and `migration-completed`. It runs in the API server, not as a webhook,
so it works with `webhooks.enabled: false`. Updates that leave those keys
unchanged, and the keys users set (`virtrigaud.io/orphan-on-delete`,
`virtrigaud.io/force-delete`, ...), are not affected. On an older cluster
nothing is rendered. The key inventory is in
[`docs/admission-policy.md`](../../docs/admission-policy.md).

| Value | Description | Default |
|---|---|---|
| `admissionPolicy.reservedKeys.enabled` | Install the policy (Kubernetes 1.30+) | `true` |
| `admissionPolicy.reservedKeys.exemptUsernames` | Other identities that may set the reserved keys, e.g. a restore tool (`system:serviceaccount:velero:velero`) or another installation's manager | `[]` |
| `admissionPolicy.reservedKeys.exemptGroups` | Groups whose members may set them (e.g. a break-glass group; `system:masters` is not exempt by default) | `[]` |

Installing the policy needs rights to create `validatingadmissionpolicies` and
`validatingadmissionpolicybindings`. `helm template` without `--kube-version`
assumes Helm's built-in Kubernetes version; pass `--kube-version` when you
render for a cluster older than 1.30.

### Transport TLS floor

The manager pins an explicit **minimum of TLS 1.2** on its webhook and metrics
servers (rather than relying on Go's implicit default), the broadly-compatible
regulated floor — the Kubernetes API server and Prometheus both negotiate ≥ 1.2.
The floor is active on the always-TLS **webhook** server; on the **metrics**
server it applies when metrics are served over HTTPS (`--metrics-secure=true`;
the default serves plaintext metrics). No cipher-suite list is pinned — Go's
TLS 1.2+ defaults are AEAD-preferring and runtime-maintained, and over-specifying
ciphers is a staleness hazard.

## GitOps Integration

### ArgoCD

VirtRigaud works seamlessly with ArgoCD. The automatic CRD upgrade feature uses Helm hooks, which ArgoCD executes properly:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: virtrigaud
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://projectbeskar.github.io/virtrigaud
    chart: virtrigaud
    targetRevision: 0.4.1
    helm:
      values: |
        crdUpgrade:
          enabled: true  # CRDs will upgrade automatically
  destination:
    server: https://kubernetes.default.svc
    namespace: virtrigaud-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```

### Flux

Works with Flux HelmRelease:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: virtrigaud
  namespace: virtrigaud-system
spec:
  chart:
    spec:
      chart: virtrigaud
      sourceRef:
        kind: HelmRepository
        name: virtrigaud
      version: 0.4.1
  values:
    crdUpgrade:
      enabled: true  # CRDs will upgrade automatically
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
```

## Troubleshooting

### CRD Upgrade Job Fails

Check the job logs:

```bash
kubectl logs -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade
```

Common issues:
- **RBAC permissions**: Ensure ServiceAccount has CRD permissions
- **Image pull failures**: Check image repository and pull secrets
- **CRD conflicts**: Review conflict messages in logs

### Manual CRD Management

If automatic upgrades fail, manually apply CRDs:

```bash
# Apply all CRDs
kubectl apply -f charts/virtrigaud/crds/

# Or apply with server-side apply (recommended)
kubectl apply --server-side=true -f charts/virtrigaud/crds/
```

### Disable Automatic Upgrades Temporarily

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false \
  --reuse-values
```

## Uninstallation

Delete your `VirtualMachine`s and `Provider`s **before** uninstalling the chart.
A `Provider` carries an in-use finalizer (ADR-0010) that only the manager
releases; one left behind stays `Terminating` until its finalizer is removed by
hand.

```bash
# Uninstall the chart
helm uninstall virtrigaud -n virtrigaud-system

# Delete CRDs (WARNING: This deletes all VirtualMachine resources!)
kubectl delete crd -l app.kubernetes.io/name=virtrigaud
```

## Values File

See [values.yaml](values.yaml) for the complete list of configuration options.

## More Information

- [VirtRigaud Documentation](https://github.com/projectbeskar/virtrigaud/tree/main/docs)
- [Quick Start Guide](https://github.com/projectbeskar/virtrigaud/blob/main/docs/getting-started/quickstart.md)
- [Provider Documentation](https://github.com/projectbeskar/virtrigaud/blob/main/docs/PROVIDERS.md)
- [CRD Reference](https://github.com/projectbeskar/virtrigaud/blob/main/docs/CRDs.md)

