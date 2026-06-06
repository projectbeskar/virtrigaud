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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt"
	"github.com/projectbeskar/virtrigaud/internal/version"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

func main() {
	// Handle --version flag before any other flag parsing
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("virtrigaud-provider-libvirt %s\n", version.String())
		os.Exit(0)
	}

	var port int
	var healthPort int
	flag.IntVar(&port, "port", 9443, "gRPC server port")
	flag.IntVar(&healthPort, "health-port", 8080, "Health check port")
	flag.Parse()

	// Create logger with configurable format
	var handler slog.Handler
	logFormat := os.Getenv("LOG_FORMAT")
	if logFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: getLogLevel(),
		})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: getLogLevel(),
		})
	}
	logger := slog.New(handler)

	// Resolve TLS material + auth contract from the canonical mount path
	// and env-vars per ADR-0003 PR-2. See cmd/provider-vsphere/main.go
	// for the contract.
	tlsResolution, tlsErr := server.ResolveTLSAndAuth()
	switch {
	case tlsErr == nil:
		logger.Info("mTLS enabled",
			"cert_path", server.ProviderTLSCertFile,
			"ca_path", server.ProviderTLSCAFile,
			"require_client_cert", true,
			"allowed_sans", tlsResolution.Auth.AllowedSANs,
		)
	case errors.Is(tlsErr, server.ErrInsecureModeOptedIn):
		logger.Warn("STARTING IN PLAINTEXT MODE: VIRTRIGAUD_PROVIDER_INSECURE=true and no TLS material on disk. "+
			"manager↔provider gRPC traffic is NOT encrypted and NOT authenticated. "+
			"This is audit-flagged per ADR-0003.",
			"mount_path", server.ProviderTLSMountPath,
		)
	default:
		logger.Error("Failed to resolve TLS configuration", "error", tlsErr)
		os.Exit(1)
	}

	// Build SDK server configuration.
	//
	// Migration note (ADR-0003 PR-2): this main previously used a raw
	// grpc.NewServer() with no interceptors, a hand-rolled HTTP health
	// server, and direct signal handling. We now route through the SDK
	// server, which provides equivalent functionality (gRPC health
	// protocol, HTTP /healthz + /readyz, SIGINT/SIGTERM graceful
	// shutdown) plus the TLS + Auth interceptor chain the libvirt
	// provider was missing.
	config := server.DefaultConfig()
	config.Port = port
	config.HealthPort = healthPort
	config.Logger = logger
	config.TLS = tlsResolution.TLS
	config.Middleware = &middleware.Config{
		Logging: &middleware.LoggingConfig{
			Enabled: true,
			Logger:  logger,
		},
		Recovery: &middleware.RecoveryConfig{
			Enabled: true,
			Logger:  logger,
		},
		Auth: tlsResolution.Auth,
	}

	srv, err := server.New(config)
	if err != nil {
		logger.Error("Failed to create server", "error", err)
		os.Exit(1)
	}

	// Create the libvirt provider via the SDK pattern. The libvirt
	// internal package exposes a Server type that implements the
	// generated providerv1.ProviderServer interface, which is exactly
	// what RegisterProvider expects.
	providerImpl := libvirt.New()
	libvirtServer := libvirt.NewServer(providerImpl)
	srv.RegisterProvider(libvirtServer)

	logger.Info("Starting Libvirt provider server",
		"version", version.String(),
		"log_level", getLogLevel().String(),
		"log_format", logFormat,
		"port", port,
		"health_port", healthPort,
		"capabilities", []string{
			"core", "snapshots",
			"online-reconfigure", "qemu-guest-agent",
		},
		"supported_platforms", []string{"kvm", "qemu", "libvirt"},
	)

	if err := srv.Serve(context.Background()); err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

// getLogLevel returns the log level from LOG_LEVEL environment variable.
// Supported values: debug, warn, error, info (default)
func getLogLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
