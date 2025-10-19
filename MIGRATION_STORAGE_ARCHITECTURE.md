# Migration Storage Architecture: Automatic Provider Restart

## Overview

VirtRigaud uses a **per-migration PVC approach** with **automatic provider restart** to mount storage dynamically. This architecture balances operational simplicity with storage isolation.

## How It Works

### Migration Lifecycle

1. **User Creates VMMigration CR**
   ```yaml
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: VMMigration
   metadata:
     name: vm-migration
   spec:
     storage:
       type: pvc
       pvc:
         storageClassName: nfs-client
         size: 100Gi
         accessMode: ReadWriteMany
   ```

2. **Controller Creates PVC**
   - Migration controller creates a dedicated PVC for this migration
   - PVC labeled with: `virtrigaud.io/migration=<migration-name>`
   - PVC owned by VMMigration CR (auto-cleanup on deletion)

3. **Controller Triggers Provider Reconciliation**
   - Annotates both source and target Provider CRs: `virtrigaud.io/reconcile-trigger=<timestamp>`
   - Adds PVC reference: `virtrigaud.io/migration-pvc=<pvc-name>`
   - Provider controller watches for annotation changes

4. **Provider Controller Updates Deployments**
   - Discovers all PVCs labeled `virtrigaud.io/component=migration-storage`
   - Updates deployment spec to mount all migration PVCs
   - Kubernetes performs rolling restart (RollingUpdate strategy)

5. **Provider Pods Restart**
   - **Graceful Shutdown**: 30-second termination grace period
   - **PreStop Hook**: 15-second sleep for in-flight request completion
   - **New Pod Starts**: Mounts all migration PVCs at `/mnt/migration-storage/<pvc-name>`
   - **Health Checks**: Liveness and readiness probes ensure pod is healthy

6. **Migration Controller Waits for Providers**
   - Polls provider status every 5 seconds
   - Waits up to 5 minutes for providers to be ready
   - Checks `ProviderAvailable` condition on Provider CR

7. **Migration Proceeds**
   - Export: Source provider writes disk to `/mnt/migration-storage/<pvc-name>/...`
   - Import: Target provider reads disk from same path
   - Progress tracking via migration status

8. **Cleanup**
   - Migration completes or fails
   - User deletes VMMigration CR
   - Kubernetes deletes PVC (owner reference)
   - Providers continue running with other migration PVCs mounted

## Implementation Details

### Migration Controller (`vmmigration_controller.go`)

**Key Functions:**

- `ensureMigrationPVC()`: Creates or verifies PVC exists
- `triggerProviderReconciliation()`: Annotates Provider CRs to trigger update
- `waitForProvidersReady()`: Polls providers until ready
- `waitForProviderReady()`: Waits for single provider with timeout/retry

**Flow:**
```
handleValidatingPhase()
  ├─> ensureMigrationPVC()
  │     ├─> Create PVC
  │     └─> triggerProviderReconciliation()
  │           ├─> Annotate source provider
  │           └─> Annotate target provider
  ├─> Store PVC name in status
  └─> waitForProvidersReady()
        ├─> Poll source provider (5 sec interval, 5 min timeout)
        └─> Poll target provider (5 sec interval, 5 min timeout)
```

### Provider Controller (`provider_controller.go`)

**Key Functions:**

- `discoverMigrationPVCs()`: Lists all migration PVCs in namespace
- `discoverMigrationVolumeMounts()`: Creates volume mounts for each PVC
- `buildProviderContainer()`: Adds PreStop lifecycle hook
- `buildPodVolumes()`: Includes all migration PVCs + credentials + tmp

**Provider Pod Spec:**
```yaml
spec:
  containers:
  - name: provider
    volumeMounts:
    - name: provider-credentials
      mountPath: /etc/virtrigaud/credentials
    - name: tmp
      mountPath: /tmp
    - name: migration-<pvc-name>
      mountPath: /mnt/migration-storage/<pvc-name>
    lifecycle:
      preStop:
        exec:
          command: ["/bin/sh", "-c", "sleep 15"]
  terminationGracePeriodSeconds: 30
  volumes:
  - name: migration-<pvc-name>
    persistentVolumeClaim:
      claimName: <pvc-name>
```

### Storage URL Format

**Old Format (didn't include PVC name):**
```
pvc://vmmigrations/default/my-migration/export.qcow2
```

**New Format (includes PVC name):**
```
pvc://<pvc-name>/vmmigrations/default/my-migration/export.qcow2
```

Provider storage layer parses this and resolves to:
```
/mnt/migration-storage/<pvc-name>/vmmigrations/default/my-migration/export.qcow2
```

## Operational Characteristics

### Disruption Profile

**During Migration Start:**
- **Duration**: 5-15 seconds per provider
- **Cause**: Pod restart to mount new PVC
- **Impact**: 
  - VM describe/list operations: May fail with connection errors
  - VM create/delete: May fail with connection errors
  - Existing VMs: Continue running normally
  - Concurrent migrations: Multiple restarts if providers differ

**Mitigation:**
- Clients should implement retry logic (standard Kubernetes pattern)
- Schedule migrations during maintenance windows
- Use dedicated providers per environment for isolation

### Performance

**Restart Time Breakdown:**
- PreStop hook: 15 seconds
- Pod termination: 1-2 seconds
- New pod startup: 5-10 seconds (includes image pull if not cached)
- Health checks: 5 seconds (readiness probe)
- **Total**: 26-32 seconds worst case, 10-15 seconds typical

**Concurrent Migrations:**
- If using same providers: Single restart mounts all PVCs
- If using different providers: Independent restarts
- Provider controller is idempotent: multiple triggers = one update

### Storage Characteristics

**Isolation:**
- ✅ Each migration has dedicated PVC
- ✅ No cross-migration data leakage
- ✅ Independent lifecycle (delete migration = delete PVC)
- ✅ Per-migration size limits

**Resource Usage:**
- PVCs: N (one per active migration)
- Volume mounts: N per provider pod
- Storage: Sum of all migration PVC sizes

**Cleanup:**
- Automatic: PVC deleted when VMMigration CR is deleted (owner reference)
- Manual: Users can delete PVC independently if needed
- Orphaned PVCs: If migration is force-deleted, PVC may remain (labeled for identification)

## Alternative Architectures Considered

### Option 1: Shared Migration PVC (Not Chosen)
**Pro:** No restarts needed
**Con:** Requires pre-creation, shared IOPS, manual size management

### Option 2: Manager as Storage Proxy (Not Chosen)
**Pro:** No provider restarts
**Con:** Manager bottleneck, extra network hop, complex implementation

### Option 3: Privileged Sidecar (Not Chosen)
**Pro:** Dynamic mounting without main container restart
**Con:** Requires privileged pods (security risk), complex

### Option 4: Automatic Provider Restart (CHOSEN)
**Pro:** 
- Simple implementation
- Per-migration isolation
- Automatic cleanup
- Standard Kubernetes patterns

**Con:**
- Brief disruption (5-15s)
- Not suitable for high-frequency migrations

## Best Practices

### For Operators

1. **Storage Class Setup**
   - Use high-performance NFS or distributed storage
   - Ensure `ReadWriteMany` support
   - Configure appropriate IOPS/throughput limits

2. **Migration Planning**
   - Schedule during maintenance windows
   - Batch migrations to minimize total restarts
   - Monitor provider health during migrations

3. **Monitoring**
   - Watch provider pod restarts: `kubectl get pods -w`
   - Track migration phases: `kubectl get vmmigrations`
   - Alert on provider unavailability >1 minute

### For Developers

1. **Client Applications**
   - Implement exponential backoff retry
   - Handle gRPC `Unavailable` errors gracefully
   - Don't assume constant provider availability

2. **Provider Implementation**
   - Implement graceful shutdown in gRPC server
   - Close connections cleanly on SIGTERM
   - Use readiness probes accurately

## Troubleshooting

### Migration Stuck in "Validating"

**Check PVC:**
```bash
kubectl get pvc -n default
kubectl describe pvc <migration-name>-storage
```

**Check Provider Pods:**
```bash
kubectl get pods -n default -l app=virtrigaud-provider
kubectl describe pod <provider-pod>
```

**Check Provider Ready:**
```bash
kubectl get provider <provider-name> -o jsonpath='{.status.conditions}'
```

### Provider Fails to Mount PVC

**Check Volume Mounts:**
```bash
kubectl get deployment <provider-deployment> -o yaml | grep -A 5 volumeMounts
```

**Check PVC Status:**
```bash
kubectl get pvc <pvc-name> -o yaml
```

**Check Node Storage:**
```bash
kubectl debug node/<node-name> -it --image=busybox -- df -h
```

## Future Enhancements

1. **Shared PVC Option**: Add configuration flag for shared vs per-migration PVC
2. **Zero-Downtime Updates**: Implement blue-green deployment for providers
3. **PVC Pre-warming**: Pre-create PVCs in anticipation of migrations
4. **Sidecar Option**: Add alternative sidecar-based mounting for sensitive environments
5. **PVC Pools**: Implement PVC pooling for faster migration starts

## Conclusion

The automatic provider restart architecture provides a pragmatic balance between operational simplicity and production requirements. While it introduces brief (5-15 second) disruptions, it leverages standard Kubernetes patterns, provides strong isolation, and requires minimal additional infrastructure.

For most use cases, the disruption is acceptable given the infrequency of migration operations. For environments requiring zero disruption, the shared PVC or storage proxy architectures can be implemented as alternatives.
