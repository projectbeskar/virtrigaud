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
	"crypto/x509"
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
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

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

	// AutoReload enables hot-reload of the leaf certificate/key on file
	// change, without a pod restart, via
	// sigs.k8s.io/controller-runtime/pkg/certwatcher.
	//
	// When true (the recommended default for the TLS path — see
	// ResolveTLSAndAuth, which leaves it zero-valued so callers can set
	// it), the server wires tls.Config.GetCertificate to a CertWatcher
	// whose Start loop runs for the lifetime of Serve's context. The
	// rotated cert is picked up on the next TLS handshake.
	//
	// LIMITATION (v0.3.7): only the LEAF cert/key rotate hot. The
	// ClientCAs pool used to verify the manager's client certificate is
	// loaded once at startup into an immutable *x509.CertPool —
	// certwatcher has no primitive for reloading a CA bundle. Rotating
	// the CA still requires a provider pod restart. This is documented
	// honestly rather than faked; see ADR-0003 PR-3.
	//
	// When false, the server preserves the static-load behaviour shipped
	// in ADR-0003 PR-2: the cert/key are read once via
	// tls.LoadX509KeyPair and never refreshed.
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
				MaxConnectionIdle:     15 * time.Minute, // Increased from 15s to support long operations
				MaxConnectionAge:      2 * time.Hour,    // Increased from 30s to support disk exports
				MaxConnectionAgeGrace: 5 * time.Minute,  // Increased from 5s for graceful shutdown
				Time:                  30 * time.Second, // Increased from 5s for keep-alive pings
				Timeout:               10 * time.Second, // Increased from 1s for keep-alive timeout
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

	// certWatcher is non-nil only when TLS.AutoReload is true. Its Start
	// loop is launched in Serve and cancelled when Serve's context is
	// done, giving the goroutine a clean cancellation story (no bare
	// go func without ctx — per project rules).
	certWatcher *certwatcher.CertWatcher
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

	// Add TLS credentials if configured.
	//
	// When AutoReload is set we build a CertWatcher and wire its
	// GetCertificate into the *tls.Config so the server picks up a
	// rotated leaf cert on the next handshake. The watcher is retained on
	// the Server so Serve can run its Start loop under the serve context.
	var certWatcher *certwatcher.CertWatcher
	if config.TLS != nil {
		creds, watcher, err := buildTLSCredentials(config.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		certWatcher = watcher
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
		certWatcher:   certWatcher,
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

// GetServiceInfo returns the gRPC services registered on the underlying
// server, keyed by fully-qualified service name. It is a thin pass-through
// to grpc.Server.GetServiceInfo, primarily used by callers and tests to
// confirm a provider service was registered (e.g. after the libvirt
// SDK-migration in ADR-0003 PR-2).
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	return s.grpcServer.GetServiceInfo()
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

	// Start the certificate watcher (AutoReload path). Its Start loop is
	// bound to serverCtx, so it stops when the server shuts down — a
	// proper cancellation story rather than a detached goroutine. A
	// watcher Start failure is non-fatal to serving (the already-loaded
	// cert keeps working); we log it and continue.
	if s.certWatcher != nil {
		s.logger.Info("Starting TLS certificate watcher (AutoReload)",
			"cert_file", s.config.TLS.CertFile,
			"key_file", s.config.TLS.KeyFile,
		)
		go func() {
			if err := s.certWatcher.Start(serverCtx); err != nil {
				s.logger.Error("certificate watcher stopped with error", "error", err)
			}
		}()
	}

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
//
// When RequireClientCert is true the CA bundle at CAFile is loaded into
// ClientCAs and ClientAuth is set to RequireAndVerifyClientCert — the TLS
// stack then refuses any handshake that does not present a client cert
// chained to that CA. The SDK middleware's validateTLSPeer then checks
// the verified cert's SANs against AllowedSANs.
//
// When AutoReload is set the returned *certwatcher.CertWatcher is non-nil
// and the caller must run its Start loop; otherwise it is nil and the
// leaf cert is loaded statically.
func buildTLSCredentials(tlsConfig *TLSConfig) (credentials.TransportCredentials, *certwatcher.CertWatcher, error) {
	config, watcher, err := buildServerTLSConfig(tlsConfig)
	if err != nil {
		return nil, nil, err
	}
	return credentials.NewTLS(config), watcher, nil
}

// buildServerTLSConfig assembles the server-side *tls.Config from the given
// TLSConfig. It is split out from buildTLSCredentials so the resulting
// *tls.Config (MinVersion, ClientAuth, ClientCAs, GetCertificate) can be
// asserted directly in unit tests — credentials.NewTLS returns an opaque
// value that hides those fields.
//
// The config always pins MinVersion to TLS 1.3 (ADR-0003 floor, matches the
// manager-side dialer). When RequireClientCert is true the CA bundle at
// CAFile is loaded into ClientCAs and ClientAuth is set to
// RequireAndVerifyClientCert, so the TLS stack rejects any handshake without
// a client cert chained to that CA before the auth interceptor even runs.
//
// Leaf-cert source depends on AutoReload:
//
//   - AutoReload=true  → a certwatcher.CertWatcher is created over
//     (CertFile, KeyFile) and config.GetCertificate is wired to it. The
//     returned watcher is non-nil; the caller MUST run its Start loop. The
//     watcher reloads the leaf on file change; ClientCAs is still loaded
//     once and does NOT hot-reload (see TLSConfig.AutoReload).
//   - AutoReload=false → the leaf is loaded once via tls.LoadX509KeyPair
//     into config.Certificates (the v0.3.7 PR-2 static behaviour). The
//     returned watcher is nil.
func buildServerTLSConfig(tlsConfig *TLSConfig) (*tls.Config, *certwatcher.CertWatcher, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, // ADR-0003 floor; matches manager-side.
	}

	var watcher *certwatcher.CertWatcher
	if tlsConfig.AutoReload {
		w, err := certwatcher.New(tlsConfig.CertFile, tlsConfig.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"failed to create certificate watcher (cert=%s key=%s): %w",
				tlsConfig.CertFile, tlsConfig.KeyFile, err)
		}
		watcher = w
		config.GetCertificate = watcher.GetCertificate
	} else {
		// G304 is intentionally suppressed: CertFile/KeyFile/CAFile are
		// operator-supplied configuration values, not user-controlled
		// input. In production they resolve to the canonical
		// /etc/virtrigaud/tls mount.
		cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load key pair (cert=%s key=%s): %w",
				tlsConfig.CertFile, tlsConfig.KeyFile, err)
		}
		config.Certificates = []tls.Certificate{cert}
	}

	if tlsConfig.RequireClientCert {
		if tlsConfig.CAFile == "" {
			return nil, nil, fmt.Errorf("RequireClientCert=true requires CAFile to be set")
		}
		// #nosec G304 -- CAFile is operator-supplied configuration, not user input.
		caPEM, err := os.ReadFile(tlsConfig.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read client CA bundle (%s): %w",
				tlsConfig.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, fmt.Errorf("failed to parse any certificates from CA bundle %s",
				tlsConfig.CAFile)
		}
		// NOTE: ClientCAs is loaded once here. certwatcher only reloads
		// the leaf cert/key; there is no hot-reload primitive for the CA
		// pool in v0.3.7. Rotating the CA bundle still requires a
		// provider pod restart. Documented on TLSConfig.AutoReload.
		config.ClientCAs = pool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return config, watcher, nil
}
