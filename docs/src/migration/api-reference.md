# VMMigration API Reference

## Resource Overview

The `VMMigration` custom resource defines a VM migration operation from a source provider to a target provider using intermediate storage.

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: string
  namespace: string
spec:
  # ... specification fields
status:
  # ... status fields reported by controller
```

## Spec Fields

### sourceName (required)

**Type**: `string`

The name of the source VirtualMachine resource to migrate.

```yaml
sourceName: my-source-vm
```

### sourceNamespace (required)

**Type**: `string`

The namespace of the source VirtualMachine.

```yaml
sourceNamespace: default
```

### targetProviderRef (required)

**Type**: `ObjectReference`

Reference to the target Provider resource.

```yaml
targetProviderRef:
  name: vsphere-prod
  namespace: default
```

### targetName (required)

**Type**: `string`

The name for the migrated VM on the target provider.

```yaml
targetName: migrated-vm
```

### targetNamespace (optional)

**Type**: `string`

**Default**: Same as sourceNamespace

The namespace for the target VM.

```yaml
targetNamespace: production
```

### targetStorageHint (optional)

**Type**: `string`

Provider-specific storage hint:
- **Libvirt**: Storage pool name (e.g., `default`, `local`)
- **Proxmox**: Storage name (e.g., `local-lvm`, `ceph-storage`)
- **vSphere**: Datastore name (e.g., `datastore1`, `vsan-datastore`)

```yaml
targetStorageHint: datastore1
```

### targetDiskFormat (optional)

**Type**: `string`

**Allowed Values**: `qcow2`, `vmdk`, `raw`

**Default**: Provider native format

Explicitly specify target disk format.

```yaml
targetDiskFormat: vmdk
```

### targetResourceHints (optional)

**Type**: `ResourceHints`

Override default resource allocation for target VM.

```yaml
targetResourceHints:
  cpu: 4                    # Number of vCPUs
  memory: 8192              # Memory in MB
  disk: 100                 # Disk size in GB
```

### targetNetworkHints (optional)

**Type**: `[]NetworkHint`

Network configuration for target VM.

```yaml
targetNetworkHints:
  - name: eth0
    network: "VM Network"    # vSphere network name
    # OR
    bridge: virbr0          # Libvirt/Proxmox bridge
```

### storage (required)

**Type**: `MigrationStorage`

Intermediate storage configuration for disk transfer.

#### S3 Storage

```yaml
storage:
  type: s3
  bucket: vm-migrations            # S3 bucket name
  region: us-east-1                # AWS region
  endpoint: s3.amazonaws.com       # S3 endpoint (optional for AWS)
  credentialsSecretRef:
    name: s3-credentials
    namespace: default
```

#### HTTP Storage

```yaml
storage:
  type: http
  endpoint: https://storage.example.com/exports
  credentialsSecretRef:
    name: http-credentials
    namespace: default
```

#### NFS Storage

```yaml
storage:
  type: nfs
  path: /mnt/vm-migrations        # NFS mount path
  endpoint: nfs.example.com       # NFS server (optional)
```

### cleanupPolicy (optional)

**Type**: `CleanupPolicy`

Controls cleanup behavior after migration.

```yaml
cleanupPolicy:
  deleteIntermediate: true         # Delete files from intermediate storage
  deleteSnapshot: true             # Delete source VM snapshot
  deleteSource: false              # Delete source VM (⚠️ DANGEROUS)
```

**Fields**:

- `deleteIntermediate` (bool): Delete intermediate storage files after successful migration. **Default**: `true`
- `deleteSnapshot` (bool): Delete source VM snapshot after successful migration. **Default**: `true`
- `deleteSource` (bool): Delete source VM after successful migration. **Default**: `false`

**⚠️ Warning**: Setting `deleteSource: true` will permanently delete the source VM. Only use after validating target VM.

### retryPolicy (optional)

**Type**: `RetryPolicy`

Automatic retry configuration for failed migrations.

```yaml
retryPolicy:
  maxAttempts: 3                   # Maximum retry attempts
  backoffDuration: 5m              # Initial backoff duration
  maxBackoffDuration: 30m          # Maximum backoff duration
```

**Fields**:

- `maxAttempts` (int): Maximum number of retry attempts. **Default**: `3`
- `backoffDuration` (duration): Initial backoff duration between retries. **Default**: `5m`
- `maxBackoffDuration` (duration): Maximum backoff duration (for exponential backoff). **Default**: `30m`

**Exponential Backoff**: 5m, 10m, 20m, 30m (capped)

## Status Fields

### phase

**Type**: `string`

Current migration phase.

**Possible Values**:
- `Pending`: Migration created, awaiting processing
- `Validating`: Validating source and target
- `Snapshotting`: Creating source VM snapshot
- `Exporting`: Exporting disk from source
- `Transferring`: Uploading to storage
- `Converting`: Converting disk format
- `Importing`: Downloading from storage
- `Creating`: Creating target VM
- `ValidatingTarget`: Validating target VM
- `Ready`: Migration completed successfully
- `Failed`: Migration failed

```yaml
status:
  phase: Transferring
```

### progress

**Type**: `int`

Migration progress percentage (0-100).

```yaml
status:
  progress: 45
```

### conditions

**Type**: `[]Condition`

Detailed status conditions for each phase.

```yaml
status:
  conditions:
  - type: Validated
    status: "True"
    reason: SourceAndTargetValid
    message: Source VM and target provider validated
    lastTransitionTime: "2025-10-16T10:00:05Z"
  - type: Snapshotted
    status: "True"
    reason: SnapshotCreated
    message: Snapshot snap-12345 created
    lastTransitionTime: "2025-10-16T10:00:30Z"
  - type: Transferring
    status: "True"
    reason: UploadInProgress
    message: "Uploading disk (45% complete, 4.5GB/10GB)"
    lastTransitionTime: "2025-10-16T10:05:05Z"
```

**Condition Types**:
- `Validated`: Source and target validation complete
- `Snapshotted`: Source VM snapshot created
- `Exported`: Disk exported from source
- `Transferred`: Disk uploaded to storage
- `Converted`: Disk format converted
- `Imported`: Disk downloaded from storage
- `Created`: Target VM created
- `Ready`: Migration completed
- `Failed`: Migration failed

### startTime

**Type**: `metav1.Time`

Timestamp when migration started.

```yaml
status:
  startTime: "2025-10-16T10:00:00Z"
```

### completionTime

**Type**: `metav1.Time`

Timestamp when migration completed (success or failure).

```yaml
status:
  completionTime: "2025-10-16T10:30:00Z"
```

### snapshotId

**Type**: `string`

ID of the source VM snapshot created for migration.

```yaml
status:
  snapshotId: snap-abc123
```

### exportTaskRef

**Type**: `string`

Reference to the export task on source provider.

```yaml
status:
  exportTaskRef: task-export-12345
```

### importTaskRef

**Type**: `string`

Reference to the import task on target provider.

```yaml
status:
  importTaskRef: task-import-67890
```

### intermediateStorageURL

**Type**: `string`

URL where disk is stored in intermediate storage.

```yaml
status:
  intermediateStorageURL: s3://vm-migrations/export-abc123.vmdk
```

### checksum

**Type**: `string`

SHA256 checksum of the migrated disk.

```yaml
status:
  checksum: sha256:abcdef1234567890...
```

### bytesTransferred

**Type**: `int64`

Total bytes transferred during migration.

```yaml
status:
  bytesTransferred: 10737418240
```

### retryAttempts

**Type**: `int`

Number of retry attempts made.

```yaml
status:
  retryAttempts: 1
```

### lastRetryTime

**Type**: `metav1.Time`

Timestamp of last retry attempt.

```yaml
status:
  lastRetryTime: "2025-10-16T10:15:00Z"
```

## Complete Example

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: production-db-migration
  namespace: databases
  labels:
    app: mysql
    env: production
spec:
  # Source Configuration
  sourceName: mysql-primary
  sourceNamespace: databases
  
  # Target Configuration
  targetProviderRef:
    name: vsphere-prod
    namespace: default
  targetName: mysql-primary-vsphere
  targetNamespace: databases
  targetStorageHint: ssd-datastore
  targetDiskFormat: vmdk
  
  targetResourceHints:
    cpu: 8
    memory: 16384
    disk: 500
  
  targetNetworkHints:
    - name: eth0
      network: "Production Network"
    - name: eth1
      network: "Storage Network"
  
  # Storage Configuration
  storage:
    type: s3
    bucket: vm-migrations-prod
    region: us-west-2
    endpoint: s3.us-west-2.amazonaws.com
    credentialsSecretRef:
      name: aws-s3-prod-creds
      namespace: databases
  
  # Cleanup Configuration
  cleanupPolicy:
    deleteIntermediate: true
    deleteSnapshot: true
    deleteSource: false          # Keep source for rollback
  
  # Retry Configuration
  retryPolicy:
    maxAttempts: 5
    backoffDuration: 10m
    maxBackoffDuration: 1h

status:
  phase: Transferring
  progress: 67
  startTime: "2025-10-16T08:00:00Z"
  snapshotId: snap-mysql-20251016
  exportTaskRef: task-export-abc123
  intermediateStorageURL: s3://vm-migrations-prod/mysql-primary-20251016.vmdk
  checksum: sha256:a1b2c3d4...
  bytesTransferred: 335544320000
  retryAttempts: 0
  conditions:
  - type: Validated
    status: "True"
    reason: ValidationSuccessful
    message: Source and target validated successfully
    lastTransitionTime: "2025-10-16T08:00:05Z"
  - type: Snapshotted
    status: "True"
    reason: SnapshotCreated
    message: Snapshot snap-mysql-20251016 created successfully
    lastTransitionTime: "2025-10-16T08:01:30Z"
  - type: Exported
    status: "True"
    reason: ExportSuccessful
    message: Disk exported from source (500GB)
    lastTransitionTime: "2025-10-16T08:15:00Z"
  - type: Transferring
    status: "True"
    reason: UploadInProgress
    message: "Uploading to S3 (67% complete, 335GB/500GB, ETA: 15m)"
    lastTransitionTime: "2025-10-16T08:45:00Z"
```

## Storage Credentials Secrets

### S3 Credentials

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3-credentials
  namespace: default
type: Opaque
stringData:
  accessKey: AKIAIOSFODNN7EXAMPLE
  secretKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
  region: us-east-1                      # Optional
  endpoint: https://s3.amazonaws.com     # Optional (for non-AWS S3)
```

### HTTP Credentials

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: http-credentials
  namespace: default
type: Opaque
stringData:
  token: Bearer_your-api-token-here
  # OR
  username: admin
  password: secret123
```

### NFS (No Credentials Required)

NFS typically doesn't require credentials in the secret, but you may need:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: nfs-config
  namespace: default
type: Opaque
stringData:
  mountOptions: vers=4,rw,sync
```

## Validation Rules

The VMMigration controller validates:

1. **Source VM exists** and is accessible
2. **Target provider exists** and is ready
3. **Storage configuration is valid**:
   - S3: bucket, region specified
   - HTTP: endpoint specified
   - NFS: path specified
4. **Credentials secret exists** (if required)
5. **Target storage exists** (datastore/pool/storage)
6. **No conflicting migration** for same source VM

## Finalizers

The VMMigration resource uses finalizers for graceful cleanup:

```yaml
metadata:
  finalizers:
  - vmmigration.infra.virtrigaud.io/cleanup
```

This ensures:
- Intermediate storage is cleaned up before deletion
- Source snapshots are removed (if configured)
- Migration state is properly tracked

## Labels and Annotations

### Recommended Labels

```yaml
metadata:
  labels:
    app: mysql                          # Application name
    env: production                     # Environment
    migration.virtrigaud.io/type: upgrade  # Migration type
    migration.virtrigaud.io/source-provider: libvirt
    migration.virtrigaud.io/target-provider: vsphere
```

### Recommended Annotations

```yaml
metadata:
  annotations:
    migration.virtrigaud.io/requestor: user@example.com
    migration.virtrigaud.io/reason: "Infrastructure upgrade"
    migration.virtrigaud.io/ticket: "TICKET-12345"
```

## Events

The VMMigration controller emits Kubernetes events for major milestones:

```
Normal   ValidationStarted     Source VM validation started
Normal   ValidationSuccessful  Source VM and target provider validated
Normal   SnapshotCreated       Snapshot snap-12345 created
Normal   ExportStarted         Disk export started
Normal   ExportCompleted       Disk export completed (10GB)
Normal   TransferStarted       Upload to storage started
Normal   TransferProgress      Upload progress: 50%
Normal   TransferCompleted     Upload completed (10GB)
Normal   ImportStarted         Download from storage started
Normal   ImportCompleted       Download completed (10GB)
Normal   VMCreated             Target VM created successfully
Normal   MigrationCompleted    Migration completed successfully
Warning  RetryScheduled        Migration failed, retry scheduled in 5m
Warning  MaxRetriesExceeded    Maximum retry attempts (3) exceeded
Error    MigrationFailed       Migration failed: storage authentication error
```

## RBAC Requirements

To use VMMigrations, service accounts need:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
rules:
- apiGroups: ["infra.virtrigaud.io"]
  resources: ["vmmigrations"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["infra.virtrigaud.io"]
  resources: ["vmmigrations/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
```

---

**Next**: Check out [User Guide](./user-guide.md) for practical migration examples.

