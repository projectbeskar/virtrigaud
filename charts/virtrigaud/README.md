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

See [values.yaml](values.yaml) for complete configuration options.

### Network Policies

The chart can render namespaced `NetworkPolicy` objects that isolate the manager
and provider pods to only the flows they need. They are **opt-in and disabled by
default** (`security.networkPolicies.enabled: false`).

> **Why default-off?** Earlier chart versions declared `enabled: true` but no
> template consumed the values, so the effective behavior was always "no
> policies." Wiring real templates while keeping `true` would suddenly enforce
> isolation on `helm upgrade` and could black-hole webhook admission,
> manager↔provider gRPC, metrics scraping or DNS in clusters not designed for
> it. Default-off makes enabling a deliberate, behavior-neutral, testable choice.

**Requirements before enabling:**

- A **CNI that enforces NetworkPolicy** (Calico, Cilium, Antrea, Weave, …). On a
  CNI without enforcement these objects are inert.
- **Deployment-specific tuning** — the defaults are sane but not universal.

**What the policies allow** (when `enabled: true`):

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
  --set security.networkPolicies.enabled=true | grep -A2 'kind: NetworkPolicy'
```

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
    targetRevision: 0.2.2
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
      version: 0.2.2
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

