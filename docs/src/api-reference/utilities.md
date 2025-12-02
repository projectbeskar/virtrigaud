# Internal Utilities Reference

_Auto-generated from internal utility packages_

These are internal utilities used by VirtRigaud controllers and providers.

---

## k8s

**Path:** `internal/k8s/`

```go
package k8s // import "github.com/projectbeskar/virtrigaud/internal/k8s"


CONSTANTS

const (
	// ConditionReady indicates the resource is ready for use
	ConditionReady = "Ready"
	// ConditionProvisioning indicates the resource is being provisioned
	ConditionProvisioning = "Provisioning"
	// ConditionReconfiguring indicates the resource is being reconfigured
	ConditionReconfiguring = "Reconfiguring"
	// ConditionError indicates an error condition
	ConditionError = "Error"
	// ConditionHealthy indicates the resource is healthy
	ConditionHealthy = "Healthy"
)
    Common condition types

const (
	// ReasonReconcileSuccess indicates successful reconciliation
	ReasonReconcileSuccess = "ReconcileSuccess"
	// ReasonReconcileError indicates reconciliation error
	ReasonReconcileError = "ReconcileError"
	// ReasonProviderError indicates provider-specific error
	ReasonProviderError = "ProviderError"
	// ReasonValidationError indicates validation error
	ReasonValidationError = "ValidationError"
	// ReasonCreating indicates resource is being created
	ReasonCreating = "Creating"
	// ReasonDeleting indicates resource is being deleted
	ReasonDeleting = "Deleting"
	// ReasonUpdating indicates resource is being updated
	ReasonUpdating = "Updating"
	// ReasonWaitingForDependencies indicates waiting for dependencies
	ReasonWaitingForDependencies = "WaitingForDependencies"
	// ReasonTaskInProgress indicates async task in progress
	ReasonTaskInProgress = "TaskInProgress"
)
    Common condition reasons


FUNCTIONS

func AddFinalizer(ctx context.Context, c client.Client, obj client.Object, finalizer string) error
    AddFinalizer adds a finalizer to the object if it doesn't already exist

func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition
    GetCondition returns the condition with the given type

func HasFinalizer(obj client.Object, finalizer string) bool
    HasFinalizer returns true if the object has the specified finalizer

func IsBeingDeleted(obj metav1.Object) bool
    IsBeingDeleted returns true if the object is being deleted

func IsConditionFalse(conditions []metav1.Condition, conditionType string) bool
    IsConditionFalse returns true if the condition is present and false

func IsConditionTrue(conditions []metav1.Condition, conditionType string) bool
    IsConditionTrue returns true if the condition is present and true

func IsConditionUnknown(conditions []metav1.Condition, conditionType string) bool
    IsConditionUnknown returns true if the condition is present and unknown

func RemoveFinalizer(ctx context.Context, c client.Client, obj client.Object, finalizer string) error
    RemoveFinalizer removes a finalizer from the object if it exists

func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string)
    SetCondition sets a condition on the given list of conditions

func SetErrorCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string)
    SetErrorCondition sets the Error condition

func SetHealthyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string)
    SetHealthyCondition sets the Healthy condition

func SetProvisioningCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string)
    SetProvisioningCondition sets the Provisioning condition

func SetReadyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string)
    SetReadyCondition sets the Ready condition

func SetReconfiguringCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string)
    SetReconfiguringCondition sets the Reconfiguring condition
```

---

## resilience

**Path:** `internal/resilience/`

```go
package resilience // import "github.com/projectbeskar/virtrigaud/internal/resilience"


FUNCTIONS

func IsRetryable(err error) bool
    IsRetryable determines if an error is retryable

func Retry(ctx context.Context, config *RetryConfig, fn RetryFunc) error
    Retry executes a function with exponential backoff retry logic


TYPES

type CircuitBreaker struct {
	// Has unexported fields.
}
    CircuitBreaker implements the circuit breaker pattern

func NewCircuitBreaker(name, providerType, provider string, config *Config) *CircuitBreaker
    NewCircuitBreaker creates a new circuit breaker

func (cb *CircuitBreaker) Call(ctx context.Context, fn func(ctx context.Context) error) error
    Call executes the given function with circuit breaker protection

func (cb *CircuitBreaker) GetFailures() int
    GetFailures returns the current failure count

func (cb *CircuitBreaker) GetState() State
    GetState returns the current state

func (cb *CircuitBreaker) Reset()
    Reset resets the circuit breaker to closed state

type Config struct {
	FailureThreshold int           // Number of failures to open the circuit
	ResetTimeout     time.Duration // Time to wait before transitioning to half-open
	HalfOpenMaxCalls int           // Maximum calls allowed in half-open state
}
    Config holds circuit breaker configuration

func DefaultConfig() *Config
    DefaultConfig returns default circuit breaker configuration

type Policy struct {
	// Has unexported fields.
}
    Policy combines retry and circuit breaker policies

func NewPolicy(name string, retryConfig *RetryConfig, circuitBreaker *CircuitBreaker) *Policy
    NewPolicy creates a new resilience policy

func (p *Policy) Execute(ctx context.Context, fn func(ctx context.Context) error) error
    Execute executes a function with the full resilience policy

func (p *Policy) GetCircuitBreaker() *CircuitBreaker
    GetCircuitBreaker returns the circuit breaker

func (p *Policy) GetRetryConfig() *RetryConfig
    GetRetryConfig returns the retry configuration

type PolicyBuilder struct {
	// Has unexported fields.
}
    PolicyBuilder helps build resilience policies

func NewPolicyBuilder(name string) *PolicyBuilder
    NewPolicyBuilder creates a new policy builder

func (pb *PolicyBuilder) Build() *Policy
    Build builds the policy

func (pb *PolicyBuilder) WithCircuitBreaker(circuitBreaker *CircuitBreaker) *PolicyBuilder
    WithCircuitBreaker sets the circuit breaker

func (pb *PolicyBuilder) WithRetry(config *RetryConfig) *PolicyBuilder
    WithRetry sets the retry configuration

type Registry struct {
	// Has unexported fields.
}
    Registry manages multiple circuit breakers

func NewRegistry(config *Config) *Registry
    NewRegistry creates a new circuit breaker registry

func (r *Registry) Get(name, providerType, provider string) (*CircuitBreaker, bool)
    Get gets an existing circuit breaker

func (r *Registry) GetOrCreate(name, providerType, provider string) *CircuitBreaker
    GetOrCreate gets an existing circuit breaker or creates a new one

func (r *Registry) List() map[string]*CircuitBreaker
    List returns all circuit breakers

func (r *Registry) Remove(name, providerType, provider string)
    Remove removes a circuit breaker

func (r *Registry) Reset()
    Reset resets all circuit breakers

type RetryConfig struct {
	MaxAttempts int           // Maximum number of retry attempts
	BaseDelay   time.Duration // Base delay between retries
	MaxDelay    time.Duration // Maximum delay between retries
	Multiplier  float64       // Backoff multiplier
	Jitter      bool          // Whether to add jitter to delays
}
    RetryConfig holds retry configuration

func AggressiveRetryConfig() *RetryConfig
    AggressiveRetryConfig returns a configuration for aggressive retrying

func ConservativeRetryConfig() *RetryConfig
    ConservativeRetryConfig returns a configuration for conservative retrying

func DefaultRetryConfig() *RetryConfig
    DefaultRetryConfig returns default retry configuration

func NoRetryConfig() *RetryConfig
    NoRetryConfig returns a configuration with no retries

type RetryFunc func(ctx context.Context, attempt int) error
    RetryFunc represents a function that can be retried

type RetryableCall struct {
	// Has unexported fields.
}
    RetryableCall wraps a function call with retry logic

func NewRetryableCall(name string, config *RetryConfig) *RetryableCall
    NewRetryableCall creates a new retryable call

func (rc *RetryableCall) Execute(ctx context.Context, fn RetryFunc) error
    Execute executes a function with retry logic

func (rc *RetryableCall) ExecuteWithCircuitBreaker(
	ctx context.Context,
	circuitBreaker *CircuitBreaker,
	fn func(ctx context.Context) error,
) error
    ExecuteWithCircuitBreaker executes a function with both retry and circuit
    breaker protection

type State int
    State represents the circuit breaker state

const (
	// StateClosed means the circuit breaker is closed (normal operation)
	StateClosed State = iota
```

---

## util

**Path:** `internal/util/`

```go
package util // import "github.com/projectbeskar/virtrigaud/internal/util"


FUNCTIONS

func BoolPtr(b bool) *bool
    BoolPtr returns a pointer to the given bool

func BoolValue(b *bool) bool
    BoolValue returns the bool value or false if nil

func CalculateBackoff(config BackoffConfig, attempt int) time.Duration
    CalculateBackoff calculates the backoff delay for the given attempt

func Int32Ptr(i int32) *int32
    Int32Ptr returns a pointer to the given int32

func Int32Value(i *int32) int32
    Int32Value returns the int32 value or zero if nil

func Int64Ptr(i int64) *int64
    Int64Ptr returns a pointer to the given int64

func Int64Value(i *int64) int64
    Int64Value returns the int64 value or zero if nil

func IsRetryableAfter(attempt, maxAttempts int, config BackoffConfig) (bool, time.Duration)
    IsRetryableAfter returns true if the operation should be retried after the
    given duration

func StringPtr(s string) *string
    StringPtr returns a pointer to the given string

func StringValue(s *string) string
    StringValue returns the string value or empty string if nil


TYPES

type BackoffConfig struct {
	// InitialDelay is the initial delay duration
	InitialDelay time.Duration
	// MaxDelay is the maximum delay duration
	MaxDelay time.Duration
	// Multiplier is the backoff multiplier
	Multiplier float64
	// Jitter adds randomness to prevent thundering herd
	Jitter bool
}
    BackoffConfig configures exponential backoff

func DefaultBackoffConfig() BackoffConfig
    DefaultBackoffConfig returns sensible defaults for backoff
```

---


_Generated on: 2025-12-02 01:05:57 UTC_
