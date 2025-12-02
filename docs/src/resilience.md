# VirtRigaud Resilience Guide

This document describes the resilience patterns and error handling mechanisms in VirtRigaud.

## Overview

VirtRigaud implements comprehensive resilience patterns:

- **Error Taxonomy** - Structured error classification
- **Circuit Breakers** - Protection against cascading failures  
- **Exponential Backoff** - Intelligent retry strategies
- **Timeout Policies** - Prevent resource exhaustion
- **Rate Limiting** - Provider protection

## Error Taxonomy

### Error Types

VirtRigaud classifies all errors into specific categories:

| Type | Retryable | Description | Example |
|------|-----------|-------------|---------|
| `NotFound` | No | Resource doesn't exist | VM not found |
| `InvalidSpec` | No | Invalid configuration | Malformed VM spec |
| `Unauthorized` | No | Authentication failed | Invalid credentials |
| `NotSupported` | No | Unsupported operation | Feature not available |
| `Retryable` | Yes | Transient error | Network timeout |
| `Unavailable` | Yes | Service unavailable | Provider down |
| `RateLimit` | Yes | Rate limited | API quota exceeded |
| `Timeout` | Yes | Operation timeout | Long-running task |
| `QuotaExceeded` | No | Resource quota hit | Storage full |
| `Conflict` | No | Resource conflict | Duplicate name |

### Error Creation

```go
import "github.com/projectbeskar/virtrigaud/internal/providers/contracts"

// Create specific error types
err := contracts.NewNotFoundError("VM not found", originalErr)
err := contracts.NewRetryableError("Network timeout", originalErr)
err := contracts.NewUnavailableError("Provider unavailable", originalErr)

// Check if error is retryable
if providerErr, ok := err.(*contracts.ProviderError); ok {
    if providerErr.IsRetryable() {
        // Retry the operation
    }
}
```

## Circuit Breaker Pattern

### Configuration

```go
import "github.com/projectbeskar/virtrigaud/internal/resilience"

config := &resilience.Config{
    FailureThreshold: 10,              // Open after 10 failures
    ResetTimeout:     60 * time.Second, // Try again after 60s
    HalfOpenMaxCalls: 3,               // Allow 3 test calls
}

cb := resilience.NewCircuitBreaker("provider-vsphere", "vsphere", "prod", config)
```

### Usage

```go
err := cb.Call(ctx, func(ctx context.Context) error {
    // Call the potentially failing operation
    return provider.Create(ctx, request)
})

if err != nil {
    // Handle error (may be circuit breaker protection)
    log.Error(err, "Operation failed")
}
```

### States

1. **Closed** - Normal operation, failures are counted
2. **Open** - Fast-fail mode, requests are rejected immediately  
3. **Half-Open** - Testing mode, limited requests allowed

### Metrics

Circuit breaker state is exposed via metrics:

```
virtrigaud_circuit_breaker_state{provider_type="vsphere",provider="prod"} 0
virtrigaud_circuit_breaker_failures_total{provider_type="vsphere",provider="prod"} 5
```

## Retry Strategies

### Exponential Backoff

```go
import "github.com/projectbeskar/virtrigaud/internal/resilience"

config := &resilience.RetryConfig{
    MaxAttempts: 5,
    BaseDelay:   500 * time.Millisecond,
    MaxDelay:    30 * time.Second,
    Multiplier:  2.0,
    Jitter:      true,
}

err := resilience.Retry(ctx, config, func(ctx context.Context, attempt int) error {
    return provider.Describe(ctx, vmID)
})
```

### Backoff Calculation

For attempt `n`:
```
delay = BaseDelay × Multiplier^n
delay = min(delay, MaxDelay)
if Jitter:
    delay += random(0, delay * 0.1)
```

Example delays with `BaseDelay=500ms`, `Multiplier=2.0`:
- Attempt 0: 500ms
- Attempt 1: 1s
- Attempt 2: 2s  
- Attempt 3: 4s
- Attempt 4: 8s

### Predefined Configurations

```go
// For frequent, low-latency operations
aggressive := resilience.AggressiveRetryConfig()
// MaxAttempts: 10, BaseDelay: 100ms, Multiplier: 1.5

// For expensive operations
conservative := resilience.ConservativeRetryConfig()
// MaxAttempts: 3, BaseDelay: 1s, Multiplier: 3.0

// Disable retries
none := resilience.NoRetryConfig()
// MaxAttempts: 1
```

## Combined Resilience Policies

### Policy Builder

```go
policy := resilience.NewPolicyBuilder("vm-operations").
    WithRetry(resilience.DefaultRetryConfig()).
    WithCircuitBreaker(circuitBreaker).
    Build()

err := policy.Execute(ctx, func(ctx context.Context) error {
    return provider.Create(ctx, request)
})
```

### Integration Example

```go
// In VirtualMachine controller
func (r *VirtualMachineReconciler) createVM(ctx context.Context, vm *v1beta1.VirtualMachine) error {
    // Get circuit breaker for this provider
    cb := r.CircuitBreakerRegistry.GetOrCreate(
        "vm-operations", 
        provider.Spec.Type, 
        provider.Name,
    )
    
    // Create resilience policy
    policy := resilience.NewPolicyBuilder("create-vm").
        WithRetry(&resilience.RetryConfig{
            MaxAttempts: 3,
            BaseDelay:   1 * time.Second,
            MaxDelay:    30 * time.Second,
            Multiplier:  2.0,
            Jitter:      true,
        }).
        WithCircuitBreaker(cb).
        Build()
    
    // Execute with resilience
    return policy.Execute(ctx, func(ctx context.Context) error {
        resp, err := provider.Create(ctx, createReq)
        if err != nil {
            return err
        }
        
        vm.Status.ID = resp.ID
        vm.Status.TaskRef = resp.TaskRef
        return nil
    })
}
```

## Timeout Policies

### RPC Timeouts

Different operations have different timeout requirements:

```go
// Operation-specific timeouts
config := &config.RPCConfig{
    TimeoutDescribe:   30 * time.Second,  // Quick status check
    TimeoutMutating:   4 * time.Minute,   // Create/Delete/Power
    TimeoutValidate:   10 * time.Second,  // Provider validation
    TimeoutTaskStatus: 10 * time.Second,  // Task polling
}

// Usage in gRPC client
timeout := config.GetRPCTimeout("Create")
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()

resp, err := client.Create(ctx, request)
```

### Context Propagation

Always respect context deadlines:

```go
func (p *Provider) Create(ctx context.Context, req CreateRequest) error {
    // Check if context is already cancelled
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    
    // Perform operation with context
    return p.performCreate(ctx, req)
}
```

## Rate Limiting

### Provider Protection

```go
import "golang.org/x/time/rate"

// Configure rate limiter
limiter := rate.NewLimiter(
    rate.Limit(config.RateLimit.QPS),    // 10 requests per second
    config.RateLimit.Burst,              // Allow bursts of 20
)

// Check rate limit before operation
if !limiter.Allow() {
    return contracts.NewRateLimitError("Rate limit exceeded", nil)
}

// Proceed with operation
return provider.Create(ctx, request)
```

### Per-Provider Limits

Each provider instance has its own rate limiter:

```go
type ProviderManager struct {
    limiters map[string]*rate.Limiter
}

func (pm *ProviderManager) getLimiter(providerType, provider string) *rate.Limiter {
    key := fmt.Sprintf("%s:%s", providerType, provider)
    if limiter, exists := pm.limiters[key]; exists {
        return limiter
    }
    
    // Create new limiter
    limiter := rate.NewLimiter(rate.Limit(10), 20)
    pm.limiters[key] = limiter
    return limiter
}
```

## Condition Mapping

### VM Conditions

VirtRigaud sets standard conditions based on operations:

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `Ready` | True | `VMReady` | VM is ready for use |
| `Ready` | False | `ProviderError` | Provider operation failed |
| `Ready` | False | `ValidationError` | Spec validation failed |
| `Provisioning` | True | `Creating` | VM creation in progress |
| `Provisioning` | False | `CreateFailed` | VM creation failed |

### Provider Conditions

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `ProviderRuntimeReady` | True | `DeploymentReady` | Remote runtime ready |
| `ProviderRuntimeReady` | False | `DeploymentError` | Deployment failed |
| `ProviderAvailable` | True | `HealthCheckPassed` | Provider healthy |
| `ProviderAvailable` | False | `HealthCheckFailed` | Provider unhealthy |

### Error to Condition Mapping

```go
func mapErrorToCondition(err error) metav1.Condition {
    if providerErr, ok := err.(*contracts.ProviderError); ok {
        switch providerErr.Type {
        case contracts.ErrorTypeNotFound:
            return metav1.Condition{
                Type:    "Ready",
                Status:  metav1.ConditionFalse,
                Reason:  "ResourceNotFound",
                Message: providerErr.Message,
            }
        case contracts.ErrorTypeUnauthorized:
            return metav1.Condition{
                Type:    "Ready", 
                Status:  metav1.ConditionFalse,
                Reason:  "AuthenticationFailed",
                Message: providerErr.Message,
            }
        case contracts.ErrorTypeUnavailable:
            return metav1.Condition{
                Type:    "Ready",
                Status:  metav1.ConditionFalse,
                Reason:  "ProviderUnavailable", 
                Message: providerErr.Message,
            }
        }
    }
    
    // Default error condition
    return metav1.Condition{
        Type:    "Ready",
        Status:  metav1.ConditionFalse,
        Reason:  "InternalError",
        Message: err.Error(),
    }
}
```

## Best Practices

### Error Handling

1. **Always classify errors** - Use appropriate error types
2. **Preserve context** - Wrap errors with additional context
3. **Avoid retrying non-retryable errors** - Check error type first
4. **Set meaningful conditions** - Help users understand state

### Circuit Breakers

1. **Per-provider instances** - Isolate failures
2. **Appropriate thresholds** - Balance protection vs availability
3. **Monitor state changes** - Alert on circuit breaker trips
4. **Manual override** - Provide way to reset if needed

### Timeouts

1. **Operation-appropriate** - Different timeouts for different ops
2. **Propagate context** - Always pass context through
3. **Handle cancellation** - Check context.Done() regularly
4. **Resource cleanup** - Ensure resources are freed on timeout

### Rate Limiting

1. **Provider protection** - Prevent overwhelming providers
2. **Burst handling** - Allow reasonable bursts
3. **Back-pressure** - Surface rate limits to users
4. **Fair sharing** - Consider tenant isolation

## Configuration Examples

### Development Environment

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: virtrigaud-config
data:
  # Relaxed timeouts for development
  RPC_TIMEOUT_MUTATING: "10m"
  
  # Aggressive retries for flaky dev environments  
  RETRY_MAX_ATTEMPTS: "10"
  RETRY_BASE_DELAY: "100ms"
  
  # Lower circuit breaker threshold
  CB_FAILURE_THRESHOLD: "5"
  CB_RESET_SECONDS: "30s"
```

### Production Environment

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: virtrigaud-config
data:
  # Strict timeouts
  RPC_TIMEOUT_MUTATING: "4m"
  RPC_TIMEOUT_DESCRIBE: "30s"
  
  # Conservative retries
  RETRY_MAX_ATTEMPTS: "3"
  RETRY_BASE_DELAY: "1s"
  RETRY_MAX_DELAY: "60s"
  
  # Higher circuit breaker threshold
  CB_FAILURE_THRESHOLD: "15" 
  CB_RESET_SECONDS: "120s"
  
  # Rate limiting
  RATE_LIMIT_QPS: "20"
  RATE_LIMIT_BURST: "50"
```
