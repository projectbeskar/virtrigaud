/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resilience

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// RetryConfig holds retry configuration
type RetryConfig struct {
	MaxAttempts int           // Maximum number of retry attempts
	BaseDelay   time.Duration // Base delay between retries
	MaxDelay    time.Duration // Maximum delay between retries
	Multiplier  float64       // Backoff multiplier
	Jitter      bool          // Whether to add jitter to delays
}

// DefaultRetryConfig returns default retry configuration
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Multiplier:  2.0,
		Jitter:      true,
	}
}

// IsRetryable determines if an error is retryable
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Check if it's a ProviderError
	if providerErr, ok := err.(*contracts.ProviderError); ok {
		return providerErr.IsRetryable()
	}

	// For non-ProviderError types, we're conservative and don't retry
	return false
}

// RetryFunc represents a function that can be retried
type RetryFunc func(ctx context.Context, attempt int) error

// Retry executes a function with exponential backoff retry logic
func Retry(ctx context.Context, config *RetryConfig, fn RetryFunc) error {
	if config == nil {
		config = DefaultRetryConfig()
	}

	var lastErr error

	for attempt := 0; attempt < config.MaxAttempts; attempt++ {
		// Execute the function
		err := fn(ctx, attempt)
		if err == nil {
			return nil // Success
		}

		lastErr = err

		// Check if error is retryable
		if !IsRetryable(err) {
			return err
		}

		// Don't delay after the last attempt
		if attempt == config.MaxAttempts-1 {
			break
		}

		// Calculate delay for next attempt
		delay := calculateDelay(config, attempt)

		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	return lastErr
}

// calculateDelay calculates the delay for the given attempt
func calculateDelay(config *RetryConfig, attempt int) time.Duration {
	// Calculate exponential backoff
	delay := float64(config.BaseDelay) * math.Pow(config.Multiplier, float64(attempt))

	// Cap at maximum delay
	if time.Duration(delay) > config.MaxDelay {
		delay = float64(config.MaxDelay)
	}

	// Add jitter if enabled
	if config.Jitter {
		// Add random jitter up to 10% of the delay
		jitter := delay * 0.1 * rand.Float64()
		delay += jitter
	}

	return time.Duration(delay)
}

// RetryableCall wraps a function call with retry logic
type RetryableCall struct {
	config *RetryConfig
	name   string
}

// NewRetryableCall creates a new retryable call
func NewRetryableCall(name string, config *RetryConfig) *RetryableCall {
	if config == nil {
		config = DefaultRetryConfig()
	}

	return &RetryableCall{
		config: config,
		name:   name,
	}
}

// Execute executes a function with retry logic
func (rc *RetryableCall) Execute(ctx context.Context, fn RetryFunc) error {
	return Retry(ctx, rc.config, fn)
}

// ExecuteWithCircuitBreaker executes a function with both retry and circuit breaker protection
func (rc *RetryableCall) ExecuteWithCircuitBreaker(
	ctx context.Context,
	circuitBreaker *CircuitBreaker,
	fn func(ctx context.Context) error,
) error {
	return rc.Execute(ctx, func(ctx context.Context, attempt int) error {
		return circuitBreaker.Call(ctx, fn)
	})
}

// Policy combines retry and circuit breaker policies
type Policy struct {
	retryConfig    *RetryConfig
	circuitBreaker *CircuitBreaker
	name           string
}

// NewPolicy creates a new resilience policy
func NewPolicy(name string, retryConfig *RetryConfig, circuitBreaker *CircuitBreaker) *Policy {
	if retryConfig == nil {
		retryConfig = DefaultRetryConfig()
	}

	return &Policy{
		retryConfig:    retryConfig,
		circuitBreaker: circuitBreaker,
		name:           name,
	}
}

// Execute executes a function with the full resilience policy
func (p *Policy) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	retryableCall := NewRetryableCall(p.name, p.retryConfig)

	if p.circuitBreaker != nil {
		return retryableCall.ExecuteWithCircuitBreaker(ctx, p.circuitBreaker, fn)
	}

	return retryableCall.Execute(ctx, func(ctx context.Context, attempt int) error {
		return fn(ctx)
	})
}

// GetRetryConfig returns the retry configuration
func (p *Policy) GetRetryConfig() *RetryConfig {
	return p.retryConfig
}

// GetCircuitBreaker returns the circuit breaker
func (p *Policy) GetCircuitBreaker() *CircuitBreaker {
	return p.circuitBreaker
}

// PolicyBuilder helps build resilience policies
type PolicyBuilder struct {
	name           string
	retryConfig    *RetryConfig
	circuitBreaker *CircuitBreaker
}

// NewPolicyBuilder creates a new policy builder
func NewPolicyBuilder(name string) *PolicyBuilder {
	return &PolicyBuilder{
		name: name,
	}
}

// WithRetry sets the retry configuration
func (pb *PolicyBuilder) WithRetry(config *RetryConfig) *PolicyBuilder {
	pb.retryConfig = config
	return pb
}

// WithCircuitBreaker sets the circuit breaker
func (pb *PolicyBuilder) WithCircuitBreaker(circuitBreaker *CircuitBreaker) *PolicyBuilder {
	pb.circuitBreaker = circuitBreaker
	return pb
}

// Build builds the policy
func (pb *PolicyBuilder) Build() *Policy {
	return NewPolicy(pb.name, pb.retryConfig, pb.circuitBreaker)
}

// Common retry configurations

// AggressiveRetryConfig returns a configuration for aggressive retrying
func AggressiveRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts: 10,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Multiplier:  1.5,
		Jitter:      true,
	}
}

// ConservativeRetryConfig returns a configuration for conservative retrying
func ConservativeRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		MaxDelay:    60 * time.Second,
		Multiplier:  3.0,
		Jitter:      true,
	}
}

// NoRetryConfig returns a configuration with no retries
func NoRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxAttempts: 1,
		BaseDelay:   0,
		MaxDelay:    0,
		Multiplier:  1.0,
		Jitter:      false,
	}
}
