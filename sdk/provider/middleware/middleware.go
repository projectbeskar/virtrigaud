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

// Package middleware provides gRPC interceptors for provider servers.
package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Config holds middleware configuration.
type Config struct {
	// Logging configuration
	Logging *LoggingConfig

	// Recovery configuration
	Recovery *RecoveryConfig

	// Authentication configuration
	Auth *AuthConfig

	// Rate limiting configuration
	RateLimit *RateLimitConfig

	// Timeout configuration
	Timeout *TimeoutConfig

	// Metrics configuration
	Metrics *MetricsConfig
}

// LoggingConfig configures request/response logging.
type LoggingConfig struct {
	// Enabled enables request/response logging
	Enabled bool

	// Logger instance (uses slog.Default() if nil)
	Logger *slog.Logger

	// LogPayloads enables logging of request/response payloads
	LogPayloads bool

	// SlowThreshold logs requests slower than this duration
	SlowThreshold time.Duration
}

// RecoveryConfig configures panic recovery.
type RecoveryConfig struct {
	// Enabled enables panic recovery
	Enabled bool

	// Logger for panic logs
	Logger *slog.Logger
}

// AuthConfig configures authentication.
type AuthConfig struct {
	// RequireTLS requires TLS client certificates
	RequireTLS bool

	// AllowedSANs lists allowed Subject Alternative Names for mTLS
	AllowedSANs []string

	// BearerTokenAuth enables bearer token authentication
	BearerTokenAuth bool

	// ValidateToken function for bearer token validation
	ValidateToken func(ctx context.Context, token string) error
}

// RateLimitConfig configures rate limiting.
type RateLimitConfig struct {
	// Enabled enables rate limiting
	Enabled bool

	// RequestsPerSecond limits requests per second per client
	RequestsPerSecond float64

	// BurstSize allows burst requests
	BurstSize int
}

// TimeoutConfig configures request timeouts.
type TimeoutConfig struct {
	// DefaultTimeout for all requests
	DefaultTimeout time.Duration

	// PerMethodTimeouts maps method names to specific timeouts
	PerMethodTimeouts map[string]time.Duration
}

// MetricsConfig configures metrics collection.
type MetricsConfig struct {
	// Enabled enables metrics collection
	Enabled bool

	// Namespace for metrics
	Namespace string
}

// Build creates interceptor chains from the configuration.
func Build(config *Config) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	var unaryInterceptors []grpc.UnaryServerInterceptor
	var streamInterceptors []grpc.StreamServerInterceptor

	if config == nil {
		return unaryInterceptors, streamInterceptors
	}

	// Recovery (should be first to catch panics from other interceptors)
	if config.Recovery != nil && config.Recovery.Enabled {
		unaryInterceptors = append(unaryInterceptors, recoveryUnaryInterceptor(config.Recovery))
		streamInterceptors = append(streamInterceptors, recoveryStreamInterceptor(config.Recovery))
	}

	// Authentication
	if config.Auth != nil && (config.Auth.RequireTLS || config.Auth.BearerTokenAuth) {
		unaryInterceptors = append(unaryInterceptors, authUnaryInterceptor(config.Auth))
		streamInterceptors = append(streamInterceptors, authStreamInterceptor(config.Auth))
	}

	// Rate limiting
	if config.RateLimit != nil && config.RateLimit.Enabled {
		unaryInterceptors = append(unaryInterceptors, rateLimitUnaryInterceptor(config.RateLimit))
		streamInterceptors = append(streamInterceptors, rateLimitStreamInterceptor(config.RateLimit))
	}

	// Timeout
	if config.Timeout != nil && config.Timeout.DefaultTimeout > 0 {
		unaryInterceptors = append(unaryInterceptors, timeoutUnaryInterceptor(config.Timeout))
		streamInterceptors = append(streamInterceptors, timeoutStreamInterceptor(config.Timeout))
	}

	// Metrics
	if config.Metrics != nil && config.Metrics.Enabled {
		unaryInterceptors = append(unaryInterceptors, metricsUnaryInterceptor(config.Metrics))
		streamInterceptors = append(streamInterceptors, metricsStreamInterceptor(config.Metrics))
	}

	// Logging (should be last to log the final result)
	if config.Logging != nil && config.Logging.Enabled {
		unaryInterceptors = append(unaryInterceptors, loggingUnaryInterceptor(config.Logging))
		streamInterceptors = append(streamInterceptors, loggingStreamInterceptor(config.Logging))
	}

	return unaryInterceptors, streamInterceptors
}

// recoveryUnaryInterceptor handles panics in unary RPCs.
func recoveryUnaryInterceptor(config *RecoveryConfig) grpc.UnaryServerInterceptor {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logger.Error("Panic in gRPC handler",
					"method", info.FullMethod,
					"panic", r,
					"stack", string(stack),
				)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()

		return handler(ctx, req)
	}
}

// recoveryStreamInterceptor handles panics in stream RPCs.
func recoveryStreamInterceptor(config *RecoveryConfig) grpc.StreamServerInterceptor {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				logger.Error("Panic in gRPC stream handler",
					"method", info.FullMethod,
					"panic", r,
					"stack", string(stack),
				)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()

		return handler(srv, ss)
	}
}

// authUnaryInterceptor handles authentication for unary RPCs.
func authUnaryInterceptor(config *AuthConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if err := authenticateRequest(ctx, config); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// authStreamInterceptor handles authentication for stream RPCs.
func authStreamInterceptor(config *AuthConfig) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authenticateRequest(ss.Context(), config); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// authenticateRequest performs authentication checks.
func authenticateRequest(ctx context.Context, config *AuthConfig) error {
	// Check mTLS if required
	if config.RequireTLS {
		if err := validateTLSPeer(ctx, config.AllowedSANs); err != nil {
			return errors.NewPermissionDenied("TLS authentication failed").GRPCStatus().Err()
		}
	}

	// Check bearer token if required
	if config.BearerTokenAuth {
		if err := validateBearerToken(ctx, config.ValidateToken); err != nil {
			return errors.NewPermissionDenied("token authentication failed").GRPCStatus().Err()
		}
	}

	return nil
}

// validateTLSPeer validates the TLS peer certificate.
func validateTLSPeer(ctx context.Context, allowedSANs []string) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return fmt.Errorf("no peer information")
	}

	// TODO: Implement TLS certificate validation
	_ = p
	_ = allowedSANs

	return nil
}

// validateBearerToken validates a bearer token.
func validateBearerToken(ctx context.Context, validateFunc func(context.Context, string) error) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("no metadata")
	}

	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return fmt.Errorf("no authorization header")
	}

	token := tokens[0]
	if len(token) < 7 || token[:7] != "Bearer " {
		return fmt.Errorf("invalid authorization header format")
	}

	return validateFunc(ctx, token[7:])
}

// rateLimitUnaryInterceptor implements rate limiting for unary RPCs.
func rateLimitUnaryInterceptor(config *RateLimitConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// TODO: Implement rate limiting logic
		return handler(ctx, req)
	}
}

// rateLimitStreamInterceptor implements rate limiting for stream RPCs.
func rateLimitStreamInterceptor(config *RateLimitConfig) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// TODO: Implement rate limiting logic
		return handler(srv, ss)
	}
}

// timeoutUnaryInterceptor implements request timeouts for unary RPCs.
func timeoutUnaryInterceptor(config *TimeoutConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		timeout := config.DefaultTimeout
		if methodTimeout, ok := config.PerMethodTimeouts[info.FullMethod]; ok {
			timeout = methodTimeout
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		return handler(timeoutCtx, req)
	}
}

// timeoutStreamInterceptor implements request timeouts for stream RPCs.
func timeoutStreamInterceptor(config *TimeoutConfig) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		timeout := config.DefaultTimeout
		if methodTimeout, ok := config.PerMethodTimeouts[info.FullMethod]; ok {
			timeout = methodTimeout
		}

		timeoutCtx, cancel := context.WithTimeout(ss.Context(), timeout)
		defer cancel()

		return handler(srv, &timeoutServerStream{ServerStream: ss, ctx: timeoutCtx})
	}
}

// timeoutServerStream wraps a ServerStream with a timeout context.
type timeoutServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *timeoutServerStream) Context() context.Context {
	return s.ctx
}

// metricsUnaryInterceptor collects metrics for unary RPCs.
func metricsUnaryInterceptor(config *MetricsConfig) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)

		// TODO: Record metrics
		_ = duration
		_ = config

		return resp, err
	}
}

// metricsStreamInterceptor collects metrics for stream RPCs.
func metricsStreamInterceptor(config *MetricsConfig) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		duration := time.Since(start)

		// TODO: Record metrics
		_ = duration
		_ = config

		return err
	}
}

// loggingUnaryInterceptor logs unary RPC requests and responses.
func loggingUnaryInterceptor(config *LoggingConfig) grpc.UnaryServerInterceptor {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()

		logger.Debug("gRPC request started",
			"method", info.FullMethod,
			"payload", getPayloadLog(req, config.LogPayloads),
		)

		resp, err := handler(ctx, req)
		duration := time.Since(start)

		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelError
		} else if config.SlowThreshold > 0 && duration > config.SlowThreshold {
			level = slog.LevelWarn
		}

		logger.Log(ctx, level, "gRPC request completed",
			"method", info.FullMethod,
			"duration", duration,
			"error", err,
			"response", getPayloadLog(resp, config.LogPayloads),
		)

		return resp, err
	}
}

// loggingStreamInterceptor logs stream RPC requests.
func loggingStreamInterceptor(config *LoggingConfig) grpc.StreamServerInterceptor {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()

		logger.Debug("gRPC stream started", "method", info.FullMethod)

		err := handler(srv, ss)
		duration := time.Since(start)

		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelError
		}

		logger.Log(ss.Context(), level, "gRPC stream completed",
			"method", info.FullMethod,
			"duration", duration,
			"error", err,
		)

		return err
	}
}

// getPayloadLog returns a loggable representation of the payload.
func getPayloadLog(payload interface{}, logPayloads bool) interface{} {
	if !logPayloads {
		return "<redacted>"
	}
	return payload
}
