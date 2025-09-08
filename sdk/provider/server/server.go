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

// Package server provides gRPC server bootstrapping utilities for provider implementations.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	healthcheck "github.com/projectbeskar/virtrigaud/internal/obs/health"
	"github.com/projectbeskar/virtrigaud/internal/version"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
)

// Config holds server configuration options.
type Config struct {
	// Port is the gRPC server port (default: 9443)
	Port int

	// HealthPort is the health check server port (default: 8080)
	HealthPort int

	// TLS configuration
	TLS *TLSConfig

	// Logger instance (uses slog.Default() if nil)
	Logger *slog.Logger

	// Middleware configuration
	Middleware *middleware.Config

	// KeepAlive settings
	KeepAlive *KeepAliveConfig

	// GracefulTimeout for shutdown (default: 30s)
	GracefulTimeout time.Duration

	// ServiceName for health checks (default: "provider")
	ServiceName string
}

// TLSConfig holds TLS configuration.
type TLSConfig struct {
	// CertFile path to TLS certificate
	CertFile string

	// KeyFile path to TLS private key
	KeyFile string

	// CAFile path to CA certificate for mTLS (optional)
	CAFile string

	// RequireClientCert enables mTLS client authentication
	RequireClientCert bool

	// AutoReload enables automatic certificate reloading
	AutoReload bool
}

// KeepAliveConfig holds keep-alive settings.
type KeepAliveConfig struct {
	// ServerParameters for server-side keep-alive
	ServerParameters *keepalive.ServerParameters

	// EnforcementPolicy for keep-alive enforcement
	EnforcementPolicy *keepalive.EnforcementPolicy
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Port:            9443,
		HealthPort:      8080,
		Logger:          slog.Default(),
		GracefulTimeout: 30 * time.Second,
		ServiceName:     "provider",
		KeepAlive: &KeepAliveConfig{
			ServerParameters: &keepalive.ServerParameters{
				MaxConnectionIdle:     15 * time.Second,
				MaxConnectionAge:      30 * time.Second,
				MaxConnectionAgeGrace: 5 * time.Second,
				Time:                  5 * time.Second,
				Timeout:               1 * time.Second,
			},
			EnforcementPolicy: &keepalive.EnforcementPolicy{
				MinTime:             5 * time.Second,
				PermitWithoutStream: false,
			},
		},
	}
}

// Server wraps a gRPC server with provider-specific functionality.
type Server struct {
	config        *Config
	grpcServer    *grpc.Server
	healthServer  *health.Server
	healthChecker *healthcheck.HealthChecker
	httpServer    *http.Server
	logger        *slog.Logger
	running       atomic.Bool
}

// New creates a new provider server with the given configuration.
func New(config *Config) (*Server, error) {
	if config == nil {
		config = DefaultConfig()
	}

	// Fill in defaults
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Port == 0 {
		config.Port = 9443
	}
	if config.HealthPort == 0 {
		config.HealthPort = 8080
	}
	if config.GracefulTimeout == 0 {
		config.GracefulTimeout = 30 * time.Second
	}
	if config.ServiceName == "" {
		config.ServiceName = "provider"
	}

	// Build gRPC server options
	var opts []grpc.ServerOption

	// Add keep-alive settings
	if config.KeepAlive != nil {
		if config.KeepAlive.ServerParameters != nil {
			opts = append(opts, grpc.KeepaliveParams(*config.KeepAlive.ServerParameters))
		}
		if config.KeepAlive.EnforcementPolicy != nil {
			opts = append(opts, grpc.KeepaliveEnforcementPolicy(*config.KeepAlive.EnforcementPolicy))
		}
	}

	// Add TLS credentials if configured
	if config.TLS != nil {
		creds, err := buildTLSCredentials(config.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		opts = append(opts, grpc.Creds(creds))
	}

	// Add middleware
	if config.Middleware != nil {
		unaryInterceptors, streamInterceptors := middleware.Build(config.Middleware)
		if len(unaryInterceptors) > 0 {
			opts = append(opts, grpc.ChainUnaryInterceptor(unaryInterceptors...))
		}
		if len(streamInterceptors) > 0 {
			opts = append(opts, grpc.ChainStreamInterceptor(streamInterceptors...))
		}
	}

	// Create gRPC server
	grpcServer := grpc.NewServer(opts...)

	// Create health server
	healthServer := health.NewServer()

	// Create health checker for HTTP endpoints
	healthChecker := healthcheck.NewHealthChecker()

	// Create HTTP server for health checks
	mux := http.NewServeMux()
	mux.Handle("/healthz", healthChecker.LivenessHandler())
	mux.Handle("/readyz", healthChecker.ReadinessHandler())
	mux.Handle("/health", healthChecker.HTTPHandler())

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", config.HealthPort),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return &Server{
		config:        config,
		grpcServer:    grpcServer,
		healthServer:  healthServer,
		healthChecker: healthChecker,
		httpServer:    httpServer,
		logger:        config.Logger,
	}, nil
}

// RegisterService registers a provider service implementation.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl interface{}) {
	s.grpcServer.RegisterService(desc, impl)
}

// RegisterProvider is a convenience method to register a provider service.
func (s *Server) RegisterProvider(service interface{}) {
	// Register the provider service using the generated service descriptor
	s.grpcServer.RegisterService(&providerv1.Provider_ServiceDesc, service)
}

// Serve starts the gRPC server and blocks until shutdown.
func (s *Server) Serve(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return fmt.Errorf("server is already running")
	}
	defer s.running.Store(false)

	// Register health service
	grpc_health_v1.RegisterHealthServer(s.grpcServer, s.healthServer)
	s.healthServer.SetServingStatus(s.config.ServiceName, grpc_health_v1.HealthCheckResponse_SERVING)
	s.healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Create listener
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.config.Port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", s.config.Port, err)
	}

	s.logger.Info("Starting provider server",
		"version", version.String(),
		"port", s.config.Port,
		"health_port", s.config.HealthPort,
		"tls_enabled", s.config.TLS != nil,
	)

	// Create context for graceful shutdown
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start servers in goroutines
	errChan := make(chan error, 2)

	// Start gRPC server
	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			errChan <- fmt.Errorf("gRPC server error: %w", err)
		}
	}()

	// Start HTTP health server
	go func() {
		s.logger.Info("Starting HTTP health server", "health_port", s.config.HealthPort)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("HTTP health server error: %w", err)
		}
	}()

	// Wait for shutdown signal or context cancellation
	select {
	case <-serverCtx.Done():
		s.logger.Info("Server context cancelled, shutting down")
	case sig := <-sigChan:
		s.logger.Info("Received shutdown signal", "signal", sig)
	case err := <-errChan:
		s.logger.Error("Server error", "error", err)
		return err
	}

	// Graceful shutdown
	return s.shutdown()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	return s.shutdown()
}

func (s *Server) shutdown() error {
	s.logger.Info("Shutting down servers")

	// Mark health as not serving
	s.healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	s.healthServer.SetServingStatus(s.config.ServiceName, grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	// Shutdown HTTP server first
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.GracefulTimeout/2)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		s.logger.Warn("HTTP server shutdown error", "error", err)
	} else {
		s.logger.Info("HTTP health server stopped gracefully")
	}

	// Graceful stop gRPC server with timeout
	stopped := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		s.logger.Info("gRPC server stopped gracefully")
		return nil
	case <-time.After(s.config.GracefulTimeout / 2):
		s.logger.Warn("gRPC server graceful shutdown timeout, forcing stop")
		s.grpcServer.Stop()
		return nil
	}
}

// buildTLSCredentials creates TLS credentials from the given config.
func buildTLSCredentials(tlsConfig *TLSConfig) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load key pair: %w", err)
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ServerName:   "", // Will be set by gRPC
	}

	if tlsConfig.RequireClientCert {
		config.ClientAuth = tls.RequireAndVerifyClientCert
		// TODO: Load CA cert for client verification
	}

	return credentials.NewTLS(config), nil
}
