# Installing CRDs

Custom Resource Definitions (CRDs) must be installed before deploying the VirtRigaud controller.

## Using Helm

Helm 3.8+ automatically installs CRDs:

```bash
helm install virtrigaud virtrigaud/virtrigaud
```

For upgrading CRDs with Helm, see [Helm CRD Upgrades](../helm-crd-upgrades.md).

## Using kubectl

Apply CRD manifests directly:

```bash
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/latest/download/crds.yaml
```

## Verify Installation

Check that CRDs are registered:

```bash
kubectl get crds | grep virtrigaud.io
```

Expected output:
```
providers.infra.virtrigaud.io
virtualmachines.infra.virtrigaud.io
vmclasses.infra.virtrigaud.io
vmimages.infra.virtrigaud.io
vmmigrations.infra.virtrigaud.io
vmplacementpolicies.infra.virtrigaud.io
vmsets.infra.virtrigaud.io
```

## What's Next?

- [Deploy the Controller](controller.md)
- [Custom Resource Definitions Reference](../crds.md)
