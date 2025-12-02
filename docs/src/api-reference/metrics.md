# Metrics Catalog

VirtRigaud exposes comprehensive metrics for monitoring and observability. All metrics are available at the `/metrics` endpoint on port 8080.

## Manager Metrics

### Reconciliation Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_manager_reconcile_total` | Counter | `kind`, `outcome` | Total number of reconcile loops |
| `virtrigaud_manager_reconcile_duration_seconds` | Histogram | `kind` | Time spent in reconcile loops |
| `virtrigaud_queue_depth` | Gauge | `kind` | Current queue depth for each resource kind |

### VM Operation Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_vm_operations_total` | Counter | `operation`, `provider_type`, `provider`, `outcome` | Total VM operations |
| `virtrigaud_vm_reconfigure_total` | Counter | `provider_type`, `outcome` | Total VM reconfiguration operations |
| `virtrigaud_vm_snapshot_total` | Counter | `action`, `provider_type`, `outcome` | Total VM snapshot operations |
| `virtrigaud_vm_clone_total` | Counter | `linked`, `provider_type`, `outcome` | Total VM clone operations |
| `virtrigaud_vm_image_prepare_total` | Counter | `provider_type`, `outcome` | Total VM image preparation operations |

### Build Information

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_build_info` | Gauge | `version`, `git_sha`, `go_version` | Build information |

## Provider Metrics

### gRPC Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_provider_rpc_requests_total` | Counter | `provider_type`, `method`, `code` | Total gRPC requests |
| `virtrigaud_provider_rpc_latency_seconds` | Histogram | `provider_type`, `method` | gRPC request latency |
| `virtrigaud_provider_tasks_inflight` | Gauge | `provider_type`, `provider` | Number of inflight tasks |

### Provider-Specific Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_ip_discovery_duration_seconds` | Histogram | `provider_type` | Time to discover VM IP addresses |

### Error Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_errors_total` | Counter | `reason`, `component` | Total errors by reason and component |

## Label Definitions

### Common Labels

- `provider_type`: The type of provider (`vsphere`, `libvirt`)
- `provider`: The name of the provider instance
- `outcome`: The result of an operation (`success`, `failure`, `error`)
- `kind`: The Kubernetes resource kind (`VirtualMachine`, `VMClass`, etc.)
- `component`: The component generating the metric (`manager`, `provider`)

### Operation-Specific Labels

- `operation`: Type of VM operation (`Create`, `Delete`, `Power`, `Describe`, `Reconfigure`)
- `method`: gRPC method name (`CreateVM`, `DeleteVM`, `PowerVM`, etc.)
- `code`: gRPC status code (`OK`, `INVALID_ARGUMENT`, `DEADLINE_EXCEEDED`, etc.)
- `action`: Snapshot action (`create`, `delete`, `revert`)
- `linked`: Whether a clone is linked (`true`, `false`)
- `reason`: Error reason (`ConnectionFailed`, `AuthenticationError`, etc.)

## Histogram Buckets

Duration histograms use the following buckets (in seconds):
```
0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300
```

## Example Queries

### Prometheus Queries

#### Error Rate
```promql
# Overall error rate
rate(virtrigaud_vm_operations_total{outcome="failure"}[5m]) /
rate(virtrigaud_vm_operations_total[5m])

# Provider-specific error rate
rate(virtrigaud_provider_rpc_requests_total{code!="OK"}[5m]) /
rate(virtrigaud_provider_rpc_requests_total[5m])
```

#### Latency
```promql
# 95th percentile VM creation time
histogram_quantile(0.95, 
  rate(virtrigaud_vm_operations_duration_seconds_bucket{operation="Create"}[5m])
)

# gRPC request latency by method
histogram_quantile(0.95,
  rate(virtrigaud_provider_rpc_latency_seconds_bucket[5m])
) by (method)
```

#### Throughput
```promql
# VM operations per second
rate(virtrigaud_vm_operations_total[5m])

# Operations by provider
rate(virtrigaud_vm_operations_total[5m]) by (provider_type, provider)
```

#### Queue Depth
```promql
# Current queue depth
virtrigaud_queue_depth

# Average queue depth over time
avg_over_time(virtrigaud_queue_depth[5m])
```

#### Inflight Tasks
```promql
# Current inflight tasks
virtrigaud_provider_tasks_inflight

# Inflight tasks by provider
virtrigaud_provider_tasks_inflight by (provider_type, provider)
```

### Grafana Dashboard Queries

#### VM Creation Success Rate Panel
```promql
sum(rate(virtrigaud_vm_operations_total{operation="Create",outcome="success"}[5m])) /
sum(rate(virtrigaud_vm_operations_total{operation="Create"}[5m])) * 100
```

#### Provider Health Panel
```promql
up{job="virtrigaud-provider"}
```

#### Error Rate by Provider Panel
```promql
sum(rate(virtrigaud_errors_total[5m])) by (component, provider_type)
```

## ServiceMonitor Configuration

Example ServiceMonitor for Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: virtrigaud-manager
  namespace: virtrigaud-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud
      app.kubernetes.io/component: manager
  endpoints:
  - port: metrics
    interval: 30s
    path: /metrics
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: virtrigaud-providers
  namespace: virtrigaud-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud
      app.kubernetes.io/component: provider
  endpoints:
  - port: metrics
    interval: 30s
    path: /metrics
```

## Alert Rules

Example PrometheusRule for common alerts:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: virtrigaud-alerts
  namespace: virtrigaud-system
spec:
  groups:
  - name: virtrigaud.rules
    rules:
    - alert: VirtRigaudProviderDown
      expr: up{job="virtrigaud-provider"} == 0
      for: 5m
      labels:
        severity: critical
      annotations:
        summary: "VirtRigaud provider is down"
        description: "Provider {{ $labels.instance }} has been down for more than 5 minutes"

    - alert: VirtRigaudHighErrorRate
      expr: |
        rate(virtrigaud_vm_operations_total{outcome="failure"}[5m]) /
        rate(virtrigaud_vm_operations_total[5m]) > 0.1
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: "High error rate in VM operations"
        description: "Error rate is {{ $value | humanizePercentage }} for {{ $labels.provider }}"

    - alert: VirtRigaudSlowVMCreation
      expr: |
        histogram_quantile(0.95,
          rate(virtrigaud_vm_operations_duration_seconds_bucket{operation="Create"}[5m])
        ) > 600
      for: 15m
      labels:
        severity: warning
      annotations:
        summary: "Slow VM creation times"
        description: "95th percentile VM creation time is {{ $value }}s"

    - alert: VirtRigaudQueueBacklog
      expr: virtrigaud_queue_depth > 100
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: "Queue backlog detected"
        description: "Queue depth for {{ $labels.kind }} is {{ $value }}"
```

## Custom Metrics

Providers can expose additional custom metrics specific to their implementation:

### vSphere Provider Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_vsphere_sessions_total` | Counter | `datacenter` | Total vSphere sessions created |
| `virtrigaud_vsphere_api_calls_total` | Counter | `method`, `datacenter` | Total vSphere API calls |

### Libvirt Provider Metrics

| Metric Name | Type | Labels | Description |
|-------------|------|--------|-------------|
| `virtrigaud_libvirt_connections_total` | Counter | `host` | Total Libvirt connections |
| `virtrigaud_libvirt_domains_total` | Gauge | `host`, `state` | Current number of domains by state |

## Metric Collection Best Practices

1. **Scrape Interval**: Use 30s interval for most metrics
2. **Retention**: Keep metrics for at least 30 days for trending
3. **High Cardinality**: Be careful with VM names and IDs in labels
4. **Aggregation**: Use recording rules for frequently queried metrics
5. **Alerting**: Set up alerts for SLI/SLO violations

## Related Documentation

- [Monitoring Guide](../admin-guides/monitoring.md)
- [Grafana Dashboards](../admin-guides/dashboards.md)
- [Alert Runbooks](../admin-guides/alerts.md)
