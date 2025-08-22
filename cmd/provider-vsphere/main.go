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
	"flag"
	"fmt"
	"log"
	"net"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/vsphere"
	providerv1 "github.com/projectbeskar/virtrigaud/internal/rpc/provider/v1"
)

func main() {
	var port int
	var healthPort int
	flag.IntVar(&port, "port", 9443, "gRPC server port")
	flag.IntVar(&healthPort, "health-port", 8080, "Health check port")
	flag.Parse()

	// Create context that listens for the interrupt signal from the OS
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Create gRPC server
	server := grpc.NewServer()

	// Create vSphere provider
	provider := vsphere.NewServer(nil)

	// Register the provider service
	providerv1.RegisterProviderServer(server, provider)

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Start gRPC server
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	log.Printf("vSphere provider server listening on port %d", port)

	// Start server in a goroutine
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	<-ctx.Done()

	log.Println("Shutting down gRPC server...")
	server.GracefulStop()
}
