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

package util

import (
	"math"
	"math/rand"
	"time"
)

// BackoffConfig configures exponential backoff
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

// DefaultBackoffConfig returns sensible defaults for backoff
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		InitialDelay: 1 * time.Second,
		MaxDelay:     5 * time.Minute,
		Multiplier:   2.0,
		Jitter:       true,
	}
}

// CalculateBackoff calculates the backoff delay for the given attempt
func CalculateBackoff(config BackoffConfig, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	// Calculate exponential delay
	delay := float64(config.InitialDelay) * math.Pow(config.Multiplier, float64(attempt))

	// Cap at max delay
	if delay > float64(config.MaxDelay) {
		delay = float64(config.MaxDelay)
	}

	// Add jitter if enabled
	if config.Jitter {
		// Add up to 10% jitter
		jitter := delay * 0.1 * rand.Float64()
		delay += jitter
	}

	return time.Duration(delay)
}

// IsRetryableAfter returns true if the operation should be retried after the given duration
func IsRetryableAfter(attempt int, maxAttempts int, config BackoffConfig) (bool, time.Duration) {
	if maxAttempts > 0 && attempt >= maxAttempts {
		return false, 0
	}

	return true, CalculateBackoff(config, attempt)
}
