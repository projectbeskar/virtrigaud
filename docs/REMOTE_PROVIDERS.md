# Provider Architecture

This document describes the provider architecture in VirtRigaud.

## Overview

VirtRigaud uses a **Remote Provider** architecture where providers run as independent pods, communicating with the manager controller via gRPC. This design provides scalability, security, and reliability benefits.

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
│   │             │        gRPC Connection
│   └─────────────┤        Port 9090
└─────────────────┘
```

## Provider Components

### 1. Provider Runtime Deployments

Each Provider resource automatically creates:

- **Deployment**: Runs provider-specific containers
- **Service**: ClusterIP service for gRPC communication  
- **ConfigMaps**: Provider configuration
- **Secret mounts**: Credentials for hypervisor access

### 2. Provider Images

Specialized images for each provider type:

- **virtrigaud/provider-vsphere**: vSphere provider with govmomi
- **virtrigaud/provider-libvirt**: LibVirt provider with CGO bindings
- **virtrigaud/provider-proxmox**: Proxmox VE provider
- **virtrigaud/provider-mock**: Mock provider for testing

### 3. gRPC Communication

- **Protocol**: gRPC with protocol buffers
- **Security**: Secure communication over TLS (optional)
- **Health**: Built-in health checks and graceful shutdown
- **Metrics**: Prometheus metrics on port 8080

## Provider Configuration

### Basic Provider Setup

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vsphere-credentials
  namespace: default
type: Opaque
stringData:
  username: "admin@vsphere.local"
  password: "your-password"

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-datacenter
  namespace: default
spec:
  type: vsphere
  endpoint: "https://vcenter.example.com:443"
  credentialSecretRef:
    name: vsphere-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-vsphere:latest"
    service:
      port: 9090
```

### Advanced Configuration

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-cluster
  namespace: production
spec:
  type: libvirt
  endpoint: "qemu+ssh://admin@kvm.example.com/system"
  credentialSecretRef:
    name: libvirt-credentials
  defaults:
    cluster: production
  rateLimit:
    qps: 20
    burst: 50
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:v1.0.0"
    replicas: 3
    
    service:
      port: 9090
      
    resources:
      requests:
        cpu: "200m"
        memory: "256Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
        
    # High availability setup
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/instance: libvirt-cluster
          topologyKey: kubernetes.io/hostname
          
    # Node placement
    nodeSelector:
      workload-type: compute
      
    tolerations:
    - key: "compute-dedicated"
      operator: "Equal"
      value: "true"
      effect: "NoSchedule"
      
    # Environment variables
    env:
    - name: LIBVIRT_DEBUG
      value: "1"
    - name: PROVIDER_TIMEOUT
      value: "300s"
```

## Security Model

### Pod Security

- **Non-root execution**: All containers run as non-root users
- **Read-only filesystem**: Immutable container filesystem
- **Minimal capabilities**: Reduced Linux capabilities
- **Security contexts**: Enforced via deployment templates

### Credential Isolation

- **Separated secrets**: Each provider has dedicated credential secrets
- **Scoped access**: Providers only access their own hypervisor credentials
- **RBAC isolation**: Fine-grained RBAC per provider namespace

### Network Security

- **Service mesh ready**: Compatible with Istio/Linkerd
- **Network policies**: Optional traffic restrictions
- **TLS support**: Secure gRPC communication (configurable)

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
  rpc ListCapabilities(CapabilitiesRequest) returns (CapabilitiesResponse);
}
```

### Error Handling

- **Retry logic**: Exponential backoff for transient failures
- **Circuit breakers**: Prevent cascade failures
- **Timeout controls**: Configurable per-operation timeouts
- **Status reporting**: Conditions reflected in Kubernetes status

## Observability

### Metrics

Provider pods expose Prometheus metrics on port 8080:

```
# Request metrics
provider_grpc_requests_total{method="Create",status="success"} 42
provider_grpc_request_duration_seconds{method="Create",quantile="0.95"} 2.5

# VM metrics  
provider_vms_total{state="running"} 15
provider_vms_total{state="stopped"} 3

# Health metrics
provider_health_status{provider="vsphere-datacenter"} 1
provider_hypervisor_connection_status{endpoint="vcenter.example.com"} 1
```

### Logging

- **Structured logs**: JSON format with correlation IDs
- **Log levels**: Configurable verbosity (debug, info, warn, error)
- **Request tracing**: Context propagation across gRPC calls

### Health Checks

- **Kubernetes probes**: Liveness and readiness probes
- **gRPC health protocol**: Standard health check implementation
- **Hypervisor connectivity**: Validates connection to external systems

## Deployment Patterns

### Single Provider Setup

```yaml
# Simple development setup
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: dev-vsphere
spec:
  type: vsphere
  endpoint: "https://vcenter-dev.example.com:443"
  credentialSecretRef:
    name: dev-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-vsphere:latest"
```

### High Availability Setup

```yaml
# Production HA setup
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: prod-vsphere
spec:
  type: vsphere
  endpoint: "https://vcenter-prod.example.com:443"
  credentialSecretRef:
    name: prod-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-vsphere:v1.0.0"
    replicas: 3
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              app.kubernetes.io/instance: prod-vsphere
          topologyKey: kubernetes.io/hostname
```

### Multi-Environment Setup

```yaml
# Development environment
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: dev-libvirt
  namespace: development
spec:
  type: libvirt
  endpoint: "qemu+ssh://dev@libvirt-dev.example.com/system"
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:latest"
    resources:
      requests:
        cpu: "100m"
        memory: "128Mi"

---
# Production environment  
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: prod-libvirt
  namespace: production
spec:
  type: libvirt
  endpoint: "qemu+ssh://prod@libvirt-prod.example.com/system"
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:v1.0.0"
    replicas: 2
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "2"
        memory: "2Gi"
```

## Benefits

### Scalability

- **Horizontal scaling**: Multiple provider replicas per hypervisor
- **Resource isolation**: Independent resource allocation per provider
- **Load distribution**: gRPC load balancing across provider instances

### Security

- **Credential isolation**: Hypervisor credentials isolated to provider pods
- **Network segmentation**: Providers can run in separate namespaces
- **Least privilege**: Manager runs without direct hypervisor access

### Reliability

- **Fault isolation**: Provider failures don't affect the manager
- **Independent updates**: Provider images updated separately
- **Circuit breaking**: Automatic failure detection and recovery

### Operational Excellence

- **Rolling updates**: Zero-downtime provider updates
- **Health monitoring**: Built-in health checks and metrics
- **Debugging**: Isolated provider logs and observability

## Troubleshooting

### Common Issues

1. **Image Pull Failures**
   ```bash
   # Check image availability
   docker pull virtrigaud/provider-vsphere:latest
   
   # Verify imagePullSecrets if using private registry
   kubectl get secret regcred -o yaml
   ```

2. **Network Connectivity**
   ```bash
   # Test provider service
   kubectl get svc virtrigaud-provider-*
   
   # Check provider pod logs
   kubectl logs -l app.kubernetes.io/name=virtrigaud-provider
   ```

3. **Credential Issues**
   ```bash
   # Verify secret exists and is mounted
   kubectl get secret vsphere-credentials
   kubectl describe pod virtrigaud-provider-*
   ```

### Debugging Commands

```bash
# Check provider status
kubectl describe provider vsphere-datacenter

# Check provider deployment
kubectl get deployment -l app.kubernetes.io/instance=vsphere-datacenter

# Check provider pods
kubectl get pods -l app.kubernetes.io/instance=vsphere-datacenter

# View provider logs
kubectl logs -l app.kubernetes.io/instance=vsphere-datacenter -f

# Check provider metrics
kubectl port-forward svc/virtrigaud-provider-vsphere-datacenter 8080:8080
curl http://localhost:8080/metrics
```

### Performance Tuning

```yaml
# Optimize for high-volume workloads
spec:
  rateLimit:
    qps: 100        # Increase API rate limit
    burst: 200      # Allow burst capacity
  runtime:
    replicas: 5     # Scale out for throughput
    resources:
      requests:
        cpu: "1"    # Guarantee CPU resources
        memory: "1Gi"
      limits:
        cpu: "4"    # Allow burst CPU
        memory: "4Gi"
```

## Best Practices

### Resource Management

- **Right-sizing**: Start with small requests, monitor and adjust
- **Limits**: Always set memory limits to prevent OOM kills
- **QoS**: Use Guaranteed QoS for production workloads

### Security

- **Secrets rotation**: Implement regular credential rotation
- **Network policies**: Restrict provider-to-hypervisor traffic
- **RBAC**: Use dedicated service accounts per provider

### Monitoring

- **Alerting**: Set up alerts on provider health metrics
- **Dashboards**: Create Grafana dashboards for provider metrics
- **Log aggregation**: Centralize logs for debugging and auditing

## Migration and Upgrades

### Provider Image Updates

```bash
# Update provider image
kubectl patch provider vsphere-datacenter -p '
{
  "spec": {
    "runtime": {
      "image": "virtrigaud/provider-vsphere:v1.1.0"
    }
  }
}'

# Monitor rollout
kubectl rollout status deployment virtrigaud-provider-vsphere-datacenter
```

### Configuration Changes

```bash
# Update provider configuration
kubectl edit provider vsphere-datacenter

# Verify changes applied
kubectl describe provider vsphere-datacenter
```