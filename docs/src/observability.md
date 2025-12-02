# VirtRigaud Observability Guide

This document describes the comprehensive observability features of VirtRigaud, including structured logging, metrics, tracing, and monitoring.

## Overview

VirtRigaud provides production-grade observability through:

- **Structured JSON Logging** with correlation IDs and automatic secret redaction
- **Comprehensive Prometheus Metrics** for all components and operations
- **OpenTelemetry Tracing** with gRPC instrumentation
- **Health Endpoints** for liveness and readiness probes
- **Grafana Dashboards** for visualization
- **Prometheus Alerts** for proactive monitoring

## Logging

### Configuration

Configure logging via environment variables:

```bash
LOG_LEVEL=info              # debug, info, warn, error
LOG_FORMAT=json             # json or console
LOG_SAMPLING=true           # Enable log sampling
LOG_DEVELOPMENT=false       # Development mode
```

### Correlation IDs

All log entries include correlation fields:

```json
{
  "level": "info",
  "ts": "2025-01-27T10:30:45.123Z",
  "msg": "VM operation started",
  "correlationID": "req-12345",
  "vm": "default/web-server-1",
  "provider": "default/vsphere-prod",
  "providerType": "vsphere",
  "taskRef": "task-67890",
  "reconcile": "uuid-abcdef"
}
```

### Secret Redaction

Sensitive information is automatically redacted:

```json
{
  "msg": "Connecting to provider",
  "endpoint": "vcenter://user:[REDACTED]@vc.example.com/Datacenter",
  "userData": "[REDACTED]"
}
```

## Metrics Catalog

### Manager Metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `virtrigaud_manager_reconcile_total` | Counter | Total reconcile operations | `kind`, `outcome` |
| `virtrigaud_manager_reconcile_duration_seconds` | Histogram | Reconcile duration | `kind` |
| `virtrigaud_queue_depth` | Gauge | Work queue depth | `kind` |

### Provider Metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `virtrigaud_provider_rpc_requests_total` | Counter | RPC requests | `provider_type`, `method`, `code` |
| `virtrigaud_provider_rpc_latency_seconds` | Histogram | RPC latency | `provider_type`, `method` |
| `virtrigaud_provider_tasks_inflight` | Gauge | Inflight tasks | `provider_type`, `provider` |

### VM Operation Metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `virtrigaud_vm_operations_total` | Counter | VM operations | `operation`, `provider_type`, `provider`, `outcome` |
| `virtrigaud_ip_discovery_duration_seconds` | Histogram | IP discovery time | `provider_type` |

### Circuit Breaker Metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `virtrigaud_circuit_breaker_state` | Gauge | CB state (0=closed, 1=half-open, 2=open) | `provider_type`, `provider` |
| `virtrigaud_circuit_breaker_failures_total` | Counter | CB failures | `provider_type`, `provider` |

### Error Metrics

| Metric | Type | Description | Labels |
|--------|------|-------------|--------|
| `virtrigaud_errors_total` | Counter | Errors by reason | `reason`, `component` |

## Tracing

### Configuration

Enable OpenTelemetry tracing:

```bash
VIRTRIGAUD_TRACING_ENABLED=true
VIRTRIGAUD_TRACING_ENDPOINT=http://jaeger:14268/api/traces
VIRTRIGAUD_TRACING_SAMPLING_RATIO=0.1
VIRTRIGAUD_TRACING_INSECURE=true
```

### Span Structure

Key spans include:

- `vm.reconcile` - Full VM reconciliation
- `vm.create` - VM creation operation
- `provider.validate` - Provider validation
- `rpc.Create` - gRPC calls to providers

### Trace Attributes

Standard attributes:

```
vm.namespace = "default"
vm.name = "web-server-1"
provider.type = "vsphere"
operation = "Create"
task.ref = "task-12345"
```

## Health Endpoints

### HTTP Endpoints

All components expose health endpoints on port 8080:

- `GET /healthz` - Liveness probe (always returns 200)
- `GET /readyz` - Readiness probe (checks dependencies)
- `GET /health` - Detailed health status (JSON)

### gRPC Health

Providers implement `grpc.health.v1.Health` service for health checks.

## Grafana Dashboards

### Manager Dashboard

- Reconcile rates and duration
- Queue depth monitoring
- Error rate tracking
- Resource usage (CPU/memory)

### Provider Dashboard

- RPC latency and error rates
- Task monitoring
- Circuit breaker status
- Provider-specific metrics

### VM Lifecycle Dashboard

- Creation success rates
- IP discovery times
- Failure analysis
- Provider comparison

## Prometheus Alerts

### Critical Alerts

- `VirtRigaudProviderDown` - Provider unavailable
- `VirtRigaudManagerDown` - Manager unavailable

### Warning Alerts

- `VirtRigaudProviderErrorRateHigh` - High error rate (>50%)
- `VirtRigaudReconcileStuck` - Slow reconciles (>5min)
- `VirtRigaudQueueBackedUp` - Queue depth >100
- `VirtRigaudCircuitBreakerOpen` - CB protection active

## Configuration Reference

### Complete Environment Variables

```bash
# Logging
LOG_LEVEL=info
LOG_FORMAT=json
LOG_SAMPLING=true
LOG_DEVELOPMENT=false

# Tracing
VIRTRIGAUD_TRACING_ENABLED=false
VIRTRIGAUD_TRACING_ENDPOINT=""
VIRTRIGAUD_TRACING_SAMPLING_RATIO=0.1
VIRTRIGAUD_TRACING_INSECURE=true

# RPC Timeouts
RPC_TIMEOUT_DESCRIBE=30s
RPC_TIMEOUT_MUTATING=4m
RPC_TIMEOUT_VALIDATE=10s
RPC_TIMEOUT_TASK_STATUS=10s

# Retry Configuration
RETRY_MAX_ATTEMPTS=5
RETRY_BASE_DELAY=500ms
RETRY_MAX_DELAY=30s
RETRY_MULTIPLIER=2.0
RETRY_JITTER=true

# Circuit Breaker
CB_FAILURE_THRESHOLD=10
CB_RESET_SECONDS=60s
CB_HALF_OPEN_MAX_CALLS=3

# Rate Limiting
RATE_LIMIT_QPS=10
RATE_LIMIT_BURST=20

# Workers
WORKERS_PER_KIND=2
MAX_INFLIGHT_TASKS=100

# Feature Gates
FEATURE_GATES=""

# Performance
VIRTRIGAUD_PPROF_ENABLED=false
VIRTRIGAUD_PPROF_ADDR=:6060
```

## Deployment

### ServiceMonitor

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: virtrigaud-manager
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud
  endpoints:
  - port: metrics
    interval: 30s
```

### PrometheusRule

Deploy alerts:

```bash
kubectl apply -f deploy/observability/prometheus/alerts.yaml
```

### Grafana Dashboards

Import dashboards from `deploy/observability/grafana/`

## Troubleshooting

### High Error Rates

1. Check provider health: `kubectl get providers`
2. Review error metrics: `virtrigaud_errors_total`
3. Check circuit breaker state
4. Review provider logs

### Slow Operations

1. Check RPC latency metrics
2. Review reconcile duration
3. Check resource constraints
4. Monitor task queue depth

### Memory Issues

1. Monitor `process_resident_memory_bytes`
2. Check for goroutine leaks: `go_goroutines`
3. Review heap usage: `go_memstats_heap_inuse_bytes`
