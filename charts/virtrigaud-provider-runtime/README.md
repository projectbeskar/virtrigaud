# virtrigaud-provider-runtime (deprecated)

> **Deprecated in v0.4.1. This chart is removed in the next minor release.**
> It was never published to the VirtRigaud Helm repository and is not released
> with the `virtrigaud` chart. Do not use it for new installations.

## Why it is deprecated

The chart runs a provider `Deployment` (plus `Service`, `ServiceAccount`, RBAC,
`NetworkPolicy`, HPA and PDB) on its own, outside any `Provider` resource. The
VirtRigaud manager never talks to such a pod: for each `Provider` resource the
manager runs the provider itself and connects only to the `Service` it created
for it (`status.runtime.endpoint`). A provider deployed with this chart is
therefore unused by the manager, and it bypasses what the manager sets up for a
provider it runs:

- the gRPC TLS or mTLS settings of `spec.runtime.service.tls`;
- the credential `Secret` of `spec.credentialSecretRef`, mounted read-only at
  `/etc/virtrigaud/credentials`;
- a dedicated ServiceAccount for the provider pod;
- the hardened default security context (non-root, read-only root file system,
  no privilege escalation, all capabilities dropped).

## Migrating to a `Provider` resource

1. Create a `Provider` resource for each provider this chart deploys. The
   manager then runs the provider. Examples:
   [`examples/provider-libvirt.yaml`](../../examples/provider-libvirt.yaml),
   [`examples/provider-vsphere.yaml`](../../examples/provider-vsphere.yaml),
   [`examples/proxmox-complete-example.yaml`](../../examples/proxmox-complete-example.yaml)
   and [`examples/provider-libvirt-clustered.yaml`](../../examples/provider-libvirt-clustered.yaml).
2. Carry the chart values over to the `Provider`:

   | Chart value | `Provider` field |
   |---|---|
   | `image.repository`, `image.tag` | `spec.runtime.image` |
   | `image.pullPolicy`, `imagePullSecrets` | `spec.runtime.imagePullPolicy`, `spec.runtime.imagePullSecrets` |
   | `replicaCount` | `spec.runtime.replicas` |
   | `env` (provider settings such as `PVE_DEFAULT_STORAGE` or `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`) | `spec.runtime.env` |
   | `env` entries that carry the hypervisor address or credentials (for example `PVE_ENDPOINT`, `PVE_TOKEN_ID`, `PVE_TOKEN_SECRET`) | `spec.endpoint` and the `Secret` named by `spec.credentialSecretRef` |
   | `credentials.secretName` | `spec.credentialSecretRef` (see the example for the provider's `Secret` keys) |
   | `tls.enabled`, `tls.secretName` | `spec.runtime.service.tls.enabled`, `spec.runtime.service.tls.secretRef` (a `Provider` with no `tls` block is not deployed) |
   | `resources`, `nodeSelector`, `tolerations`, `affinity` | `spec.runtime.resources`, `spec.runtime.nodeSelector`, `spec.runtime.tolerations`, `spec.runtime.affinity` |
   | `securityContext` | Leave `spec.runtime.securityContext` unset to keep the hardened default. If you set it, it replaces the default entirely (it is not merged): include `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]` |
   | `livenessProbe`, `readinessProbe` | `spec.runtime.livenessProbe`, `spec.runtime.readinessProbe` |

   The chart's HPA, PDB and extra RBAC rules have no `Provider` field. For
   network policy, use the opt-in `networkPolicy` values of the `virtrigaud`
   chart, which select the provider pods the manager runs (in the release
   namespace only: NetworkPolicy is namespaced).
3. Wait until the `Provider` reports `Ready`
   (`kubectl get providers.infra.virtrigaud.io -A`).
4. Remove the chart's release: `helm uninstall <release> --namespace <namespace>`.

The `examples/values-*.yaml` files in this directory are kept, unchanged in
substance, until the chart is removed.
