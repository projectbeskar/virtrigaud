# Remote Provider Architecture

This document describes the remote provider runtime architecture implemented in virtrigaud.

## Overview

virtrigaud now supports two execution modes for providers:

1. **InProcess**: Traditional mode where providers run within the manager process
2. **Remote**: New mode where providers run as separate deployments with gRPC communication

## Architecture

```
┌─────────────────┐    ┌───────────────────┐    ┌─────────────────┐
│   VirtualMachine │    │     Provider      │    │ Provider Runtime│
│      CRD        │    │       CRD         │    │   Deployment    │
└─────────────────┘    └───────────────────┘    └─────────────────┘
         │                        │                        │
         │                        │                        │
         v                        v                        │
┌─────────────────┐    ┌───────────────────┐              │
│    Manager      │    │ Provider          │              │
│   Controller    │    │ Controller        │              │
│                 │    │                   │              │
│   ┌─────────────┤    │ - Creates Deploy  │              │
│   │ VM Reconcile│    │ - Creates Service │              │
│   │             │    │ - Updates Status  │              │
│   └─────────────┤    │                   │              │
│                 │    └───────────────────┘              │
│   ┌─────────────┤                                       │
│   │ gRPC Client │◄──────────────────────────────────────┘
│   │             │        TLS Connection
│   └─────────────┤        Port 9443
└─────────────────┘
```

## Remote Provider Components

### 1. Provider Runtime Deployments

Each Provider CR with `spec.runtime.mode: Remote` creates:

- **Deployment**: Runs provider-specific containers
- **Service**: ClusterIP service for gRPC communication  
- **Secret mounts**: Credentials and TLS certificates
- **NetworkPolicy**: (Optional) Traffic restrictions

### 2. Provider Images

Two specialized images are built:

- **provider-libvirt**: CGO-enabled for libvirt bindings
- **provider-vsphere**: Pure Go for vSphere (govmomi)

### 3. gRPC Communication

- **Protocol**: gRPC with protobuf definitions
- **Security**: mTLS with certificate-based authentication
- **Health**: Built-in health checks and graceful shutdown
- **Metrics**: Prometheus metrics on port 8080

## Provider CRD Enhancements

### Runtime Specification

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: Provider
spec:
  runtime:
    mode: Remote                    # InProcess | Remote
    image: provider-libvirt:v0.2.0  # Container image
    replicas: 2                     # Pod replicas
    
    service:
      port: 9443                    # gRPC port
    
    resources:                      # Resource requirements
      requests:
        cpu: "100m"
        memory: "128Mi"
      limits:
        cpu: "2"
        memory: "1Gi"
    
    tls:                           # TLS configuration
      enabled: true
      secretRef:
        name: provider-tls
    
    # Standard Kubernetes scheduling
    nodeSelector: {}
    tolerations: []
    affinity: {}
    securityContext: {}
    env: []
```

### Runtime Status

```yaml
status:
  runtime:
    mode: Remote
    endpoint: "virtrigaud-provider-default-libvirt-prod:9443"
    serviceRef:
      name: virtrigaud-provider-default-libvirt-prod
    phase: Ready
    message: "Provider runtime is healthy"
```

## Security Model

### Pod Security

- **runAsNonRoot**: All containers run as non-root user
- **readOnlyRootFilesystem**: Immutable container filesystem
- **No capabilities**: All Linux capabilities dropped
- **SecurityContext**: Enforced via CRDs and controllers

### Network Security

- **mTLS**: Mutual TLS authentication between manager and providers
- **Certificate Management**: Kubernetes secrets for cert storage
- **NetworkPolicy**: Optional traffic restrictions to hypervisor endpoints

### Credential Isolation

- **Separation**: Provider pods get mounted secrets, manager doesn't
- **Scoped Access**: Each provider only accesses its own credentials
- **Rotation**: Standard Kubernetes secret rotation mechanisms

## Communication Protocol

### gRPC Service Definition

```protobuf
service Provider {
  rpc Validate(ValidateRequest) returns (ValidateResponse);
  rpc Create(CreateRequest) returns (CreateResponse);
  rpc Delete(DeleteRequest) returns (TaskResponse);
  rpc Power(PowerRequest) returns (TaskResponse);
  rpc Reconfigure(ReconfigureRequest) returns (TaskResponse);
  rpc Describe(DescribeRequest) returns (DescribeResponse);
  rpc TaskStatus(TaskStatusRequest) returns (TaskStatusResponse);
}
```

### Error Handling

- **Circuit Breakers**: Prevent cascade failures
- **Exponential Backoff**: Retry with increasing delays
- **Timeout Controls**: Per-operation timeout configuration
- **Condition Updates**: Status reflected in Kubernetes conditions

## Observability

### Metrics

Each provider runtime exposes Prometheus metrics:

- **Request Counters**: gRPC method call counts
- **Latency Histograms**: Operation duration tracking
- **Error Rates**: Failed operation percentages
- **Health Status**: Provider connectivity and health

### Logging

- **Structured Logs**: JSON-formatted with correlation IDs
- **Log Levels**: Configurable verbosity (debug, info, warn, error)
- **Context Propagation**: Request tracing across components

### Health Checks

- **gRPC Health Protocol**: Standard health check implementation
- **Kubernetes Probes**: Liveness and readiness probe support
- **Dependency Checks**: Hypervisor connectivity validation

## Deployment Strategies

### High Availability

```yaml
runtime:
  replicas: 3
  affinity:
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app: virtrigaud-provider
        topologyKey: kubernetes.io/hostname
```

### Resource Management

```yaml
runtime:
  resources:
    requests:
      cpu: "100m"      # Minimum guaranteed
      memory: "128Mi"
    limits:
      cpu: "2"         # Maximum allowed
      memory: "1Gi"
```

### Node Placement

```yaml
runtime:
  nodeSelector:
    virtrigaud.io/provider: "libvirt"
  tolerations:
  - key: "virtrigaud.io/provider"
    operator: "Equal"
    value: "libvirt"
    effect: "NoSchedule"
```

## Migration Path

### Backward Compatibility

Existing Provider CRs without `runtime` specification continue to work in InProcess mode:

```yaml
# This continues to work unchanged
apiVersion: infra.virtrigaud.io/v1alpha1
kind: Provider
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  # runtime: omitted = InProcess mode
```

### Gradual Migration

1. **Deploy**: Add new remote providers alongside existing ones
2. **Test**: Validate functionality with non-production workloads
3. **Switch**: Update VirtualMachine providerRef to use remote providers
4. **Cleanup**: Remove old in-process provider configurations

## Benefits

### Scalability

- **Horizontal Scaling**: Multiple provider replicas per hypervisor
- **Resource Isolation**: Provider pods can be sized independently
- **Load Distribution**: gRPC load balancing across provider instances

### Security

- **Credential Isolation**: Hypervisor credentials only in provider pods
- **Network Segmentation**: Provider pods can run in isolated namespaces
- **Least Privilege**: Manager runs without hypervisor access

### Reliability

- **Fault Isolation**: Provider failures don't crash the manager
- **Independent Updates**: Provider images can be updated separately
- **Circuit Breaking**: Automatic failure detection and recovery

### Maintenance

- **Rolling Updates**: Provider deployments support rolling updates
- **Health Monitoring**: Built-in health checks and metrics
- **Debugging**: Isolated provider logs and metrics

## Example Configurations

See `examples/remote/` for complete working examples:

- `libvirt-remote-provider.yaml`: Libvirt with CGO in remote container
- `vsphere-remote-provider.yaml`: vSphere with HA and enterprise features

## Troubleshooting

### Common Issues

1. **TLS Certificate Problems**
   - Verify certificate validity and CA trust
   - Check certificate subject matches service DNS name
   - Ensure secrets are properly mounted

2. **Network Connectivity**
   - Verify service endpoints and ports
   - Check NetworkPolicy rules if enabled
   - Test gRPC connectivity with grpcurl

3. **Resource Constraints**
   - Monitor provider pod resource usage
   - Adjust resource requests/limits as needed
   - Check node capacity and scheduling

### Debugging Commands

```bash
# Check provider status
kubectl describe provider libvirt-prod

# Check provider runtime deployment
kubectl get deployment virtrigaud-provider-default-libvirt-prod

# Check provider logs
kubectl logs -l app.kubernetes.io/instance=libvirt-prod

# Test gRPC connectivity
grpcurl -insecure virtrigaud-provider-default-libvirt-prod:9443 \
  grpc.health.v1.Health/Check
```

## Future Enhancements

### Planned Features

- **Auto-scaling**: HPA based on VM creation rate
- **Multi-cluster**: Cross-cluster provider communication
- **Policy Engine**: Advanced placement and security policies
- **Provider Marketplace**: Community provider registry
