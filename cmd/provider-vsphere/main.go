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
	"log/slog"
	"os"

	"github.com/projectbeskar/virtrigaud/internal/providers/vsphere"
	"github.com/projectbeskar/virtrigaud/internal/version"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

func main() {
	// Handle --version flag before any other flag parsing
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		println("vsphere-provider", version.String())
		os.Exit(0)
	}

	// Parse command-line flags
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
	// and env-vars per ADR-0003 PR-2. The helper returns:
	//   - (resolution, nil)                       → mTLS-mandatory
	//   - (resolution, ErrInsecureModeOptedIn)    → plaintext (loud WARN)
	//   - (nil, hard error)                       → refuse to start
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

	// Create server configuration
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

	// Create server
	srv, err := server.New(config)
	if err != nil {
		logger.Error("Failed to create server", "error", err)
		os.Exit(1)
	}

	// Create and register provider
	providerImpl := vsphere.New()
	srv.RegisterProvider(providerImpl)

	// Log startup information with capabilities
	logger.Info("Starting vSphere provider server",
		"version", version.String(),
		"log_level", getLogLevel().String(),
		"log_format", logFormat,
		"capabilities", []string{
			"core", "snapshots", "linked-clones",
			"online-reconfigure", "templates", "folders",
		},
		"supported_platforms", []string{"vsphere", "vcenter"},
	)

	// Start server
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
