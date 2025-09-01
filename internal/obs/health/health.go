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

package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// Status represents the health status of a component
type Status string

const (
	// StatusHealthy indicates the component is healthy
	StatusHealthy Status = "healthy"
	// StatusUnhealthy indicates the component is unhealthy
	StatusUnhealthy Status = "unhealthy"
	// StatusUnknown indicates the component status is unknown
	StatusUnknown Status = "unknown"
)

// Check represents a health check function
type Check func(ctx context.Context) error

// CheckResult represents the result of a health check
type CheckResult struct {
	Name      string        `json:"name"`
	Status    Status        `json:"status"`
	Message   string        `json:"message,omitempty"`
	Duration  time.Duration `json:"duration"`
	Timestamp time.Time     `json:"timestamp"`
}

// HealthChecker manages health checks for a service
type HealthChecker struct {
	mu     sync.RWMutex
	checks map[string]Check
	cache  map[string]*CheckResult
	ttl    time.Duration
}

// NewHealthChecker creates a new health checker
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{
		checks: make(map[string]Check),
		cache:  make(map[string]*CheckResult),
		ttl:    30 * time.Second, // Cache results for 30 seconds
	}
}

// RegisterCheck registers a health check
func (hc *HealthChecker) RegisterCheck(name string, check Check) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.checks[name] = check
}

// UnregisterCheck removes a health check
func (hc *HealthChecker) UnregisterCheck(name string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	delete(hc.checks, name)
	delete(hc.cache, name)
}

// RunCheck executes a specific health check
func (hc *HealthChecker) RunCheck(ctx context.Context, name string) *CheckResult {
	hc.mu.RLock()
	check, exists := hc.checks[name]
	if !exists {
		hc.mu.RUnlock()
		return &CheckResult{
			Name:      name,
			Status:    StatusUnknown,
			Message:   "check not found",
			Timestamp: time.Now(),
		}
	}

	// Check cache first
	if cached, ok := hc.cache[name]; ok && time.Since(cached.Timestamp) < hc.ttl {
		hc.mu.RUnlock()
		return cached
	}
	hc.mu.RUnlock()

	// Run the check
	start := time.Now()
	err := check(ctx)
	duration := time.Since(start)

	result := &CheckResult{
		Name:      name,
		Duration:  duration,
		Timestamp: time.Now(),
	}

	if err != nil {
		result.Status = StatusUnhealthy
		result.Message = err.Error()
	} else {
		result.Status = StatusHealthy
	}

	// Cache the result
	hc.mu.Lock()
	hc.cache[name] = result
	hc.mu.Unlock()

	return result
}

// RunAllChecks executes all registered health checks
func (hc *HealthChecker) RunAllChecks(ctx context.Context) map[string]*CheckResult {
	hc.mu.RLock()
	checkNames := make([]string, 0, len(hc.checks))
	for name := range hc.checks {
		checkNames = append(checkNames, name)
	}
	hc.mu.RUnlock()

	results := make(map[string]*CheckResult, len(checkNames))
	for _, name := range checkNames {
		results[name] = hc.RunCheck(ctx, name)
	}

	return results
}

// IsHealthy returns true if all checks are healthy
func (hc *HealthChecker) IsHealthy(ctx context.Context) bool {
	results := hc.RunAllChecks(ctx)
	for _, result := range results {
		if result.Status != StatusHealthy {
			return false
		}
	}
	return true
}

// OverallStatus represents the overall health status
type OverallStatus struct {
	Status  Status                  `json:"status"`
	Checks  map[string]*CheckResult `json:"checks"`
	Summary map[Status]int          `json:"summary"`
}

// GetOverallStatus returns the overall health status
func (hc *HealthChecker) GetOverallStatus(ctx context.Context) *OverallStatus {
	results := hc.RunAllChecks(ctx)

	summary := map[Status]int{
		StatusHealthy:   0,
		StatusUnhealthy: 0,
		StatusUnknown:   0,
	}

	for _, result := range results {
		summary[result.Status]++
	}

	overall := StatusHealthy
	if summary[StatusUnhealthy] > 0 {
		overall = StatusUnhealthy
	} else if summary[StatusUnknown] > 0 {
		overall = StatusUnknown
	}

	return &OverallStatus{
		Status:  overall,
		Checks:  results,
		Summary: summary,
	}
}

// HTTPHandler returns an HTTP handler for health checks
func (hc *HealthChecker) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		status := hc.GetOverallStatus(ctx)

		w.Header().Set("Content-Type", "application/json")

		if status.Status == StatusHealthy {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		if err := json.NewEncoder(w).Encode(status); err != nil {
			http.Error(w, "failed to encode health status", http.StatusInternalServerError)
		}
	}
}

// LivenessHandler returns a simple liveness probe handler
func (hc *HealthChecker) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// ReadinessHandler returns a readiness probe handler
func (hc *HealthChecker) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if hc.IsHealthy(ctx) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		}
	}
}

// GRPCHealthServer wraps a gRPC health server with our health checker
type GRPCHealthServer struct {
	*health.Server
	checker *HealthChecker
}

// NewGRPCHealthServer creates a new gRPC health server
func NewGRPCHealthServer(checker *HealthChecker) *GRPCHealthServer {
	return &GRPCHealthServer{
		Server:  health.NewServer(),
		checker: checker,
	}
}

// UpdateStatus updates the gRPC health status based on health checks
func (s *GRPCHealthServer) UpdateStatus(ctx context.Context, serviceName string) {
	if s.checker.IsHealthy(ctx) {
		s.SetServingStatus(serviceName, grpc_health_v1.HealthCheckResponse_SERVING)
	} else {
		s.SetServingStatus(serviceName, grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	}
}

// StartHTTPServer starts an HTTP server for health endpoints
func StartHTTPServer(addr string, checker *HealthChecker) error {
	mux := http.NewServeMux()

	// Health endpoints
	mux.Handle("/healthz", checker.LivenessHandler())
	mux.Handle("/readyz", checker.ReadinessHandler())
	mux.Handle("/health", checker.HTTPHandler())

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return server.ListenAndServe()
}

// Common health checks

// TCPCheck creates a health check that verifies TCP connectivity
func TCPCheck(addr string) Check {
	return func(ctx context.Context) error {
		conn, err := (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", addr, err)
		}
		conn.Close() //nolint:errcheck // Connection close in error path not critical
		return nil
	}
}

// HTTPCheck creates a health check that verifies HTTP connectivity
func HTTPCheck(url string) Check {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to make request to %s: %w", url, err)
		}
		defer resp.Body.Close() //nolint:errcheck // Response body close in defer is not critical

		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
		}

		return nil
	}
}

// FunctionCheck creates a health check from a simple function
func FunctionCheck(fn func() error) Check {
	return func(ctx context.Context) error {
		return fn()
	}
}
