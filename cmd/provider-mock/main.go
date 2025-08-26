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
	"fmt"
	"log/slog"
	"os"

	"github.com/projectbeskar/virtrigaud/internal/providers/mock"
	"github.com/projectbeskar/virtrigaud/internal/version"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

func main() {
	// Handle --version flag before any other flag parsing
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("virtrigaud-provider-mock %s\n", version.String())
		os.Exit(0)
	}

	// Create logger
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: getLogLevel(),
	}))

	// Create server configuration
	config := server.DefaultConfig()
	config.Logger = logger
	config.Middleware = &middleware.Config{
		Logging: &middleware.LoggingConfig{
			Enabled: true,
			Logger:  logger,
		},
		Recovery: &middleware.RecoveryConfig{
			Enabled: true,
			Logger:  logger,
		},
	}

	// Create server
	srv, err := server.New(config)
	if err != nil {
		logger.Error("Failed to create server", "error", err)
		os.Exit(1)
	}

	// Create and register mock provider
	mockProvider := mock.NewProvider()
	srv.RegisterProvider(mockProvider)

	// Start server
	logger.Info("Starting mock provider server", "version", version.String(), "port", config.Port)
	if err := srv.Serve(context.Background()); err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

// getLogLevel returns the log level from environment variable.
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
