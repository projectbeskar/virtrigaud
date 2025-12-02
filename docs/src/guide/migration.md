# VM Migration

Migrate virtual machines between providers or clusters.

## Overview

VirtRigaud supports live and cold migration of VMs between:
- Different providers (e.g., vSphere to Proxmox)
- Different resource pools within the same provider
- Different clusters

## Migration Types

### Cold Migration

VM is shut down, migrated, then powered on:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: migrate-web-server
spec:
  vmRef:
    name: web-server
  targetProvider:
    name: target-provider
  type: cold
```

### Live Migration

VM remains running during migration (provider support required):

```yaml
spec:
  type: live
```

## Guides

- [Migration User Guide](../migration/user-guide.md) - Detailed migration walkthrough
- [VM Migration Guide](../vm-migration-guide.md) - Advanced migration scenarios
- [Migration API Reference](../migration/api-reference.md) - API documentation
