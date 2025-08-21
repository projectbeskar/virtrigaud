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
	"fmt"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// State represents the circuit breaker state
type State int

const (
	// StateClosed means the circuit breaker is closed (normal operation)
	StateClosed State = iota
	// StateHalfOpen means the circuit breaker is half-open (testing)
	StateHalfOpen
	// StateOpen means the circuit breaker is open (failing fast)
	StateOpen
)

// String returns string representation of the state
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	case StateOpen:
		return "open"
	default:
		return "unknown"
	}
}

// Config holds circuit breaker configuration
type Config struct {
	FailureThreshold int           // Number of failures to open the circuit
	ResetTimeout     time.Duration // Time to wait before transitioning to half-open
	HalfOpenMaxCalls int           // Maximum calls allowed in half-open state
}

// DefaultConfig returns default circuit breaker configuration
func DefaultConfig() *Config {
	return &Config{
		FailureThreshold: 10,
		ResetTimeout:     60 * time.Second,
		HalfOpenMaxCalls: 3,
	}
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	mu              sync.RWMutex
	config          *Config
	state           State
	failures        int
	lastFailureTime time.Time
	halfOpenCalls   int
	metrics         *metrics.CircuitBreakerMetrics
	name            string
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(name, providerType, provider string, config *Config) *CircuitBreaker {
	if config == nil {
		config = DefaultConfig()
	}

	cb := &CircuitBreaker{
		config:  config,
		state:   StateClosed,
		metrics: metrics.NewCircuitBreakerMetrics(providerType, provider),
		name:    name,
	}

	// Initialize metrics
	cb.metrics.SetState(metrics.CircuitBreakerClosed)

	return cb
}

// Call executes the given function with circuit breaker protection
func (cb *CircuitBreaker) Call(ctx context.Context, fn func(ctx context.Context) error) error {
	// Check if we should allow the call
	if !cb.allowCall() {
		return contracts.NewUnavailableError(
			fmt.Sprintf("circuit breaker %s is open", cb.name),
			nil,
		)
	}

	// Execute the function
	err := fn(ctx)

	// Record the result
	cb.recordResult(err)

	return err
}

// allowCall determines if a call should be allowed
func (cb *CircuitBreaker) allowCall() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		// Check if we should transition to half-open
		if time.Since(cb.lastFailureTime) > cb.config.ResetTimeout {
			cb.transitionToHalfOpen()
			return true
		}
		return false
	case StateHalfOpen:
		// Allow limited calls in half-open state
		if cb.halfOpenCalls < cb.config.HalfOpenMaxCalls {
			cb.halfOpenCalls++
			return true
		}
		return false
	default:
		return false
	}
}

// recordResult records the result of a call
func (cb *CircuitBreaker) recordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.recordFailure()
	} else {
		cb.recordSuccess()
	}
}

// recordFailure records a failure
func (cb *CircuitBreaker) recordFailure() {
	cb.failures++
	cb.lastFailureTime = time.Now()
	cb.metrics.RecordFailure()

	switch cb.state {
	case StateClosed:
		if cb.failures >= cb.config.FailureThreshold {
			cb.transitionToOpen()
		}
	case StateHalfOpen:
		// Any failure in half-open state should open the circuit
		cb.transitionToOpen()
	}
}

// recordSuccess records a success
func (cb *CircuitBreaker) recordSuccess() {
	switch cb.state {
	case StateHalfOpen:
		// If all half-open calls succeed, close the circuit
		if cb.halfOpenCalls >= cb.config.HalfOpenMaxCalls {
			cb.transitionToClosed()
		}
	case StateClosed:
		// Reset failures on success
		cb.failures = 0
	}
}

// transitionToClosed transitions to closed state
func (cb *CircuitBreaker) transitionToClosed() {
	cb.state = StateClosed
	cb.failures = 0
	cb.halfOpenCalls = 0
	cb.metrics.SetState(metrics.CircuitBreakerClosed)
}

// transitionToOpen transitions to open state
func (cb *CircuitBreaker) transitionToOpen() {
	cb.state = StateOpen
	cb.halfOpenCalls = 0
	cb.metrics.SetState(metrics.CircuitBreakerOpen)
}

// transitionToHalfOpen transitions to half-open state
func (cb *CircuitBreaker) transitionToHalfOpen() {
	cb.state = StateHalfOpen
	cb.halfOpenCalls = 0
	cb.metrics.SetState(metrics.CircuitBreakerHalfOpen)
}

// GetState returns the current state
func (cb *CircuitBreaker) GetState() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// GetFailures returns the current failure count
func (cb *CircuitBreaker) GetFailures() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.failures
}

// Reset resets the circuit breaker to closed state
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.transitionToClosed()
}

// Registry manages multiple circuit breakers
type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*CircuitBreaker
	config   *Config
}

// NewRegistry creates a new circuit breaker registry
func NewRegistry(config *Config) *Registry {
	if config == nil {
		config = DefaultConfig()
	}

	return &Registry{
		breakers: make(map[string]*CircuitBreaker),
		config:   config,
	}
}

// GetOrCreate gets an existing circuit breaker or creates a new one
func (r *Registry) GetOrCreate(name, providerType, provider string) *CircuitBreaker {
	key := fmt.Sprintf("%s:%s:%s", providerType, provider, name)

	r.mu.RLock()
	if breaker, exists := r.breakers[key]; exists {
		r.mu.RUnlock()
		return breaker
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if breaker, exists := r.breakers[key]; exists {
		return breaker
	}

	// Create new circuit breaker
	breaker := NewCircuitBreaker(name, providerType, provider, r.config)
	r.breakers[key] = breaker
	return breaker
}

// Get gets an existing circuit breaker
func (r *Registry) Get(name, providerType, provider string) (*CircuitBreaker, bool) {
	key := fmt.Sprintf("%s:%s:%s", providerType, provider, name)

	r.mu.RLock()
	defer r.mu.RUnlock()

	breaker, exists := r.breakers[key]
	return breaker, exists
}

// Remove removes a circuit breaker
func (r *Registry) Remove(name, providerType, provider string) {
	key := fmt.Sprintf("%s:%s:%s", providerType, provider, name)

	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.breakers, key)
}

// List returns all circuit breakers
func (r *Registry) List() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		result[k] = v
	}
	return result
}

// Reset resets all circuit breakers
func (r *Registry) Reset() {
	r.mu.RLock()
	breakers := make([]*CircuitBreaker, 0, len(r.breakers))
	for _, breaker := range r.breakers {
		breakers = append(breakers, breaker)
	}
	r.mu.RUnlock()

	for _, breaker := range breakers {
		breaker.Reset()
	}
}
