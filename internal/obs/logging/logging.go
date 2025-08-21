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

package logging

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ContextKey represents the type for context keys
type ContextKey string

const (
	// CorrelationIDKey is the context key for correlation IDs
	CorrelationIDKey ContextKey = "correlationID"
	// TraceIDKey is the context key for trace IDs
	TraceIDKey ContextKey = "traceID"
	// VMKey is the context key for VM namespace/name
	VMKey ContextKey = "vm"
	// ProviderKey is the context key for provider namespace/name
	ProviderKey ContextKey = "provider"
	// ProviderTypeKey is the context key for provider type
	ProviderTypeKey ContextKey = "providerType"
	// TaskRefKey is the context key for external task references
	TaskRefKey ContextKey = "taskRef"
	// ReconcileKey is the context key for reconcile loop ID
	ReconcileKey ContextKey = "reconcile"
)

// Config holds logging configuration
type Config struct {
	Level        string
	Format       string // json or console
	Sampling     bool
	Development  bool
	SamplingRate int
}

// DefaultConfig returns default logging configuration
func DefaultConfig() *Config {
	return &Config{
		Level:        getEnvWithDefault("LOG_LEVEL", "info"),
		Format:       getEnvWithDefault("LOG_FORMAT", "json"),
		Sampling:     getEnvBoolWithDefault("LOG_SAMPLING", true),
		Development:  getEnvBoolWithDefault("LOG_DEVELOPMENT", false),
		SamplingRate: getEnvIntWithDefault("LOG_SAMPLING_RATE", 100), // Sample 1 in 100 for info
	}
}

// Setup initializes the global logger with structured JSON output and correlation support
func Setup(config *Config) error {
	// Build zap config
	zapConfig := zap.NewProductionConfig()

	if config.Development {
		zapConfig = zap.NewDevelopmentConfig()
	}

	// Set output format
	if config.Format == "console" {
		zapConfig.Encoding = "console"
		zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		zapConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		zapConfig.Encoding = "json"
		zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		zapConfig.EncoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
	}

	// Set log level
	level := zap.InfoLevel
	switch strings.ToLower(config.Level) {
	case "debug":
		level = zap.DebugLevel
	case "info":
		level = zap.InfoLevel
	case "warn", "warning":
		level = zap.WarnLevel
	case "error":
		level = zap.ErrorLevel
	case "panic":
		level = zap.PanicLevel
	case "fatal":
		level = zap.FatalLevel
	}
	zapConfig.Level = zap.NewAtomicLevelAt(level)

	// Configure sampling
	if config.Sampling {
		zapConfig.Sampling = &zap.SamplingConfig{
			Initial:    100,
			Thereafter: config.SamplingRate,
		}
	}

	// Add caller information
	zapConfig.DisableCaller = false
	zapConfig.DisableStacktrace = false

	// Build logger
	zapLogger, err := zapConfig.Build(
		zap.AddCallerSkip(1), // Skip the wrapper function
	)
	if err != nil {
		return fmt.Errorf("failed to build logger: %w", err)
	}

	// Set as global logger
	logger := zapr.NewLogger(zapLogger)
	ctrl.SetLogger(logger)
	klog.SetLogger(logger)

	return nil
}

// FromContext returns a logger with correlation fields from context
func FromContext(ctx context.Context) logr.Logger {
	logger := ctrl.LoggerFrom(ctx)
	return enrichLogger(ctx, logger)
}

// WithVM adds VM correlation to context
func WithVM(ctx context.Context, namespace, name string) context.Context {
	return context.WithValue(ctx, VMKey, fmt.Sprintf("%s/%s", namespace, name))
}

// WithProvider adds provider correlation to context
func WithProvider(ctx context.Context, namespace, name string) context.Context {
	return context.WithValue(ctx, ProviderKey, fmt.Sprintf("%s/%s", namespace, name))
}

// WithProviderType adds provider type to context
func WithProviderType(ctx context.Context, providerType string) context.Context {
	return context.WithValue(ctx, ProviderTypeKey, providerType)
}

// WithTaskRef adds task reference to context
func WithTaskRef(ctx context.Context, taskRef string) context.Context {
	return context.WithValue(ctx, TaskRefKey, taskRef)
}

// WithReconcile adds reconcile ID to context
func WithReconcile(ctx context.Context, reconcileID string) context.Context {
	return context.WithValue(ctx, ReconcileKey, reconcileID)
}

// WithCorrelationID adds correlation ID to context
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, CorrelationIDKey, correlationID)
}

// WithTraceID adds trace ID to context
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, TraceIDKey, traceID)
}

// enrichLogger adds correlation fields from context to logger
func enrichLogger(ctx context.Context, logger logr.Logger) logr.Logger {
	fields := make([]interface{}, 0, 14) // Pre-allocate for typical usage

	if val := ctx.Value(CorrelationIDKey); val != nil {
		fields = append(fields, "correlationID", val)
	}
	if val := ctx.Value(TraceIDKey); val != nil {
		fields = append(fields, "traceID", val)
	}
	if val := ctx.Value(VMKey); val != nil {
		fields = append(fields, "vm", val)
	}
	if val := ctx.Value(ProviderKey); val != nil {
		fields = append(fields, "provider", val)
	}
	if val := ctx.Value(ProviderTypeKey); val != nil {
		fields = append(fields, "providerType", val)
	}
	if val := ctx.Value(TaskRefKey); val != nil {
		fields = append(fields, "taskRef", val)
	}
	if val := ctx.Value(ReconcileKey); val != nil {
		fields = append(fields, "reconcile", val)
	}

	if len(fields) > 0 {
		return logger.WithValues(fields...)
	}
	return logger
}

// Redactor provides secure logging by redacting sensitive information
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor creates a redactor with common sensitive patterns
func NewRedactor() *Redactor {
	patterns := []*regexp.Regexp{
		// Passwords in URLs
		regexp.MustCompile(`://[^:]*:([^@]*?)@`),
		// API keys and tokens
		regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|pwd)\s*[:=]\s*["']?([^"'\s]+)["']?`),
		// Cloud-init user data (may contain sensitive info)
		regexp.MustCompile(`(?i)(user[_-]?data|userdata)\s*[:=]\s*["']?([^"'\n]{20,})["']?`),
		// SSH keys
		regexp.MustCompile(`ssh-[a-z0-9]+ [A-Za-z0-9+/=]+ `),
		// Generic secrets (base64-like strings > 20 chars)
		regexp.MustCompile(`[A-Za-z0-9+/]{20,}={0,2}`),
	}

	return &Redactor{patterns: patterns}
}

// Redact removes sensitive information from strings
func (r *Redactor) Redact(input string) string {
	result := input
	for _, pattern := range r.patterns {
		if pattern.NumSubexp() > 0 {
			// Replace capture groups with [REDACTED]
			result = pattern.ReplaceAllStringFunc(result, func(match string) string {
				submatches := pattern.FindStringSubmatch(match)
				if len(submatches) > 1 {
					// Replace the sensitive part (first capture group) with [REDACTED]
					return strings.Replace(match, submatches[1], "[REDACTED]", 1)
				}
				return match
			})
		} else {
			// Replace entire match with [REDACTED]
			result = pattern.ReplaceAllString(result, "[REDACTED]")
		}
	}
	return result
}

// RedactMap redacts values in a map
func (r *Redactor) RedactMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}

	result := make(map[string]string, len(input))
	for k, v := range input {
		if isSensitiveKey(k) {
			result[k] = "[REDACTED]"
		} else {
			result[k] = r.Redact(v)
		}
	}
	return result
}

// Global redactor instance
var globalRedactor = NewRedactor()

// RedactString is a convenience function for global redaction
func RedactString(input string) string {
	return globalRedactor.Redact(input)
}

// RedactMap is a convenience function for global map redaction
func RedactMap(input map[string]string) map[string]string {
	return globalRedactor.RedactMap(input)
}

// isSensitiveKey checks if a key name indicates sensitive data
func isSensitiveKey(key string) bool {
	sensitiveKeys := []string{
		"password", "passwd", "pwd", "secret", "token", "key", "auth",
		"credential", "cred", "api_key", "apikey", "access_key", "private_key",
		"tls.key", "client.key", "ssh_private_key", "userdata", "user_data",
	}

	keyLower := strings.ToLower(key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(keyLower, sensitive) {
			return true
		}
	}
	return false
}

// Helper functions for environment variable parsing
func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBoolWithDefault(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvIntWithDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}
